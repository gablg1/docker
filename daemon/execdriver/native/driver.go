// +build linux,cgo

package native

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/pkg/parsers"
	"github.com/docker/docker/pkg/reexec"
	sysinfo "github.com/docker/docker/pkg/system"
	"github.com/docker/docker/pkg/term"
	"github.com/docker/docker/utils"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/cgroups/systemd"
	"github.com/docker/libcontainer/configs"
	"github.com/docker/libcontainer/system"
	"github.com/docker/libcontainer/utils"
)

const (
	DriverName = "native"
	Version    = "0.2"
)

type driver struct {
	root             string
	initPath         string
	activeContainers map[string]libcontainer.Container
	machineMemory    int64
	factory          libcontainer.Factory
	sync.Mutex
}

func NewDriver(root, initPath string, options []string) (*driver, error) {
	meminfo, err := sysinfo.ReadMemInfo()
	if err != nil {
		return nil, err
	}

	if err := sysinfo.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	// native driver root is at docker_root/execdriver/native. Put apparmor at docker_root
	if err := apparmor.InstallDefaultProfile(); err != nil {
		return nil, err
	}

	// choose cgroup manager
	// this makes sure there are no breaking changes to people
	// who upgrade from versions without native.cgroupdriver opt
	cgm := libcontainer.Cgroupfs
	if systemd.UseSystemd() {
		cgm = libcontainer.SystemdCgroups
	}

	// parse the options
	for _, option := range options {
		key, val, err := parsers.ParseKeyValueOpt(option)
		if err != nil {
			return nil, err
		}
		key = strings.ToLower(key)
		switch key {
		case "native.cgroupdriver":
			// override the default if they set options
			switch val {
			case "systemd":
				if systemd.UseSystemd() {
					cgm = libcontainer.SystemdCgroups
				} else {
					// warn them that they chose the wrong driver
					logrus.Warn("You cannot use systemd as native.cgroupdriver, using cgroupfs instead")
				}
			case "cgroupfs":
				cgm = libcontainer.Cgroupfs
			default:
				return nil, fmt.Errorf("Unknown native.cgroupdriver given %q. try cgroupfs or systemd", val)
			}
		default:
			return nil, fmt.Errorf("Unknown option %s\n", key)
		}
	}

	f, err := libcontainer.New(
		root,
		cgm,
		libcontainer.InitPath(reexec.Self(), DriverName),
	)
	if err != nil {
		return nil, err
	}

	return &driver{
		root:             root,
		initPath:         initPath,
		activeContainers: make(map[string]libcontainer.Container),
		machineMemory:    meminfo.MemTotal,
		factory:          f,
	}, nil
}

type execOutput struct {
	exitCode int
	err      error
}

func (d *driver) Run(c *execdriver.Command, pipes *execdriver.Pipes, startCallback execdriver.StartCallback) (execdriver.ExitStatus, error) {
	// take the Command and populate the libcontainer.Config from it
	container, err := d.createContainer(c)
	if err != nil {
		return execdriver.ExitStatus{ExitCode: -1}, err
	}

	p := &libcontainer.Process{
		Args: append([]string{c.ProcessConfig.Entrypoint}, c.ProcessConfig.Arguments...),
		Env:  c.ProcessConfig.Env,
		Cwd:  c.WorkingDir,
		User: c.ProcessConfig.User,
	}

	if err := setupPipes(container, &c.ProcessConfig, p, pipes); err != nil {
		return execdriver.ExitStatus{ExitCode: -1}, err
	}

	cont, err := d.factory.Create(c.ID, container)
	if err != nil {
		return execdriver.ExitStatus{ExitCode: -1}, err
	}
	d.Lock()
	d.activeContainers[c.ID] = cont
	d.Unlock()
	defer func() {
		cont.Destroy()
		d.cleanContainer(c.ID)
	}()

	if err := cont.Start(p); err != nil {
		return execdriver.ExitStatus{ExitCode: -1}, err
	}

	if startCallback != nil {
		pid, err := p.Pid()
		if err != nil {
			p.Signal(os.Kill)
			p.Wait()
			return execdriver.ExitStatus{ExitCode: -1}, err
		}
		startCallback(&c.ProcessConfig, pid)
	}

	oom := notifyOnOOM(cont)
	waitF := p.Wait
	if nss := cont.Config().Namespaces; !nss.Contains(configs.NEWPID) {
		// we need such hack for tracking processes with inherited fds,
		// because cmd.Wait() waiting for all streams to be copied
		waitF = waitInPIDHost(p, cont)
	}
	ps, err := waitF()
	if err != nil {
		execErr, ok := err.(*exec.ExitError)
		if !ok {
			return execdriver.ExitStatus{ExitCode: -1}, err
		}
		ps = execErr.ProcessState
	}
	cont.Destroy()
	_, oomKill := <-oom
	return execdriver.ExitStatus{ExitCode: utils.ExitStatus(ps.Sys().(syscall.WaitStatus)), OOMKilled: oomKill}, nil
}

// notifyOnOOM returns a channel that signals if the container received an OOM notification
// for any process.  If it is unable to subscribe to OOM notifications then a closed
// channel is returned as it will be non-blocking and return the correct result when read.
func notifyOnOOM(container libcontainer.Container) <-chan struct{} {
	oom, err := container.NotifyOOM()
	if err != nil {
		logrus.Warnf("Your kernel does not support OOM notifications: %s", err)
		c := make(chan struct{})
		close(c)
		return c
	}
	return oom
}

func killCgroupProcs(c libcontainer.Container) {
	var procs []*os.Process
	if err := c.Pause(); err != nil {
		logrus.Warn(err)
	}
	pids, err := c.Processes()
	if err != nil {
		// don't care about childs if we can't get them, this is mostly because cgroup already deleted
		logrus.Warnf("Failed to get processes from container %s: %v", c.ID(), err)
	}
	for _, pid := range pids {
		if p, err := os.FindProcess(pid); err == nil {
			procs = append(procs, p)
			if err := p.Kill(); err != nil {
				logrus.Warn(err)
			}
		}
	}
	if err := c.Resume(); err != nil {
		logrus.Warn(err)
	}
	for _, p := range procs {
		if _, err := p.Wait(); err != nil {
			logrus.Warn(err)
		}
	}
}

func waitInPIDHost(p *libcontainer.Process, c libcontainer.Container) func() (*os.ProcessState, error) {
	return func() (*os.ProcessState, error) {
		pid, err := p.Pid()
		if err != nil {
			return nil, err
		}

		process, err := os.FindProcess(pid)
		s, err := process.Wait()
		if err != nil {
			execErr, ok := err.(*exec.ExitError)
			if !ok {
				return s, err
			}
			s = execErr.ProcessState
		}
		killCgroupProcs(c)
		p.Wait()
		return s, err
	}
}

func (d *driver) Kill(c *execdriver.Command, sig int) error {
	d.Lock()
	active := d.activeContainers[c.ID]
	d.Unlock()
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	state, err := active.State()
	if err != nil {
		return err
	}
	return syscall.Kill(state.InitProcessPid, syscall.Signal(sig))
}

func (d *driver) Pause(c *execdriver.Command) error {
	d.Lock()
	active := d.activeContainers[c.ID]
	d.Unlock()
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	return active.Pause()
}

func (d *driver) Unpause(c *execdriver.Command) error {
	d.Lock()
	active := d.activeContainers[c.ID]
	d.Unlock()
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	return active.Resume()
}

// XXX Where is the right place for the following
//     const and getCheckpointImageDir() function?
const (
	containersDir = "/var/lib/docker/containers"
	criuImgDir    = "criu_img"
)

func getCheckpointImageDir(containerId string) string {
	return filepath.Join(containersDir, containerId, criuImgDir)
}

func (d *driver) Checkpoint(c *execdriver.Command) error {
	active := d.activeContainers[c.ID]
	if active == nil {
		return fmt.Errorf("active container for %s does not exist", c.ID)
	}
	container := active.container

	// Create an image directory for this container (which
	// may already exist from a previous checkpoint).
	imageDir := getCheckpointImageDir(c.ID)
	err := os.MkdirAll(imageDir, 0700)
	if err != nil && !os.IsExist(err) {
		return err
	}

	// Copy container.json and state.json files to the CRIU
	// image directory for later use during restore.  Do this
	// before checkpointing because after checkpoint the container
	// will exit and these files will be removed.
	log.CRDbg("saving container.json and state.json before calling CRIU in %s", imageDir)
	srcFiles := []string{"container.json", "state.json"}
	for _, f := range srcFiles {
		srcFile := filepath.Join(d.root, c.ID, f)
		dstFile := filepath.Join(imageDir, f)
		if _, err := utils.CopyFile(srcFile, dstFile); err != nil {
			return err
		}
	}

	d.Lock()
	defer d.Unlock()
	err = namespaces.Checkpoint(container, imageDir, c.ProcessConfig.Process.Pid)
	if err != nil {
		return err
	}

	return nil
}

type restoreOutput struct {
	exitCode int
	err      error
}

func (d *driver) Restore(c *execdriver.Command, pipes *execdriver.Pipes, restoreCallback execdriver.RestoreCallback) (int, error) {
	imageDir := getCheckpointImageDir(c.ID)
	container, err := d.createRestoreContainer(c, imageDir)
	if err != nil {
		return 1, err
	}

	var term execdriver.Terminal

	if c.ProcessConfig.Tty {
		term, err = NewTtyConsole(&c.ProcessConfig, pipes)
	} else {
		term, err = execdriver.NewStdConsole(&c.ProcessConfig, pipes)
	}
	if err != nil {
		return -1, err
	}
	c.ProcessConfig.Terminal = term

	d.Lock()
	d.activeContainers[c.ID] = &activeContainer{
		container: container,
		cmd:       &c.ProcessConfig.Cmd,
	}
	d.Unlock()
	defer d.cleanContainer(c.ID)

	// Since the CRIU binary exits after restoring the container, we
	// need to reap its child by setting PR_SET_CHILD_SUBREAPER (36)
	// so that it'll be owned by this process (Docker daemon) after restore.
	//
	// XXX This really belongs to where the Docker daemon starts.
	if _, _, syserr := syscall.RawSyscall(syscall.SYS_PRCTL, 36, 1, 0); syserr != 0 {
		return -1, fmt.Errorf("Could not set PR_SET_CHILD_SUBREAPER (syserr %d)", syserr)
	}

	restoreOutputChan := make(chan restoreOutput, 1)
	waitForRestore := make(chan struct{})

	go func() {
		exitCode, err := namespaces.Restore(container, c.ProcessConfig.Stdin, c.ProcessConfig.Stdout, c.ProcessConfig.Stderr, c.ProcessConfig.Console, filepath.Join(d.root, c.ID), imageDir,
			func(child *os.File, args []string) *exec.Cmd {
				cmd := new(exec.Cmd)
				cmd.Path = d.initPath
				cmd.Args = append([]string{
					DriverName,
					"-restore",
					"-pipe", "3",
					"--",
				}, args...)
				cmd.ExtraFiles = []*os.File{child}
				return cmd
			},
			func(restorePid int) error {
				log.CRDbg("restorePid=%d", restorePid)
				if restorePid == 0 {
					restoreCallback(&c.ProcessConfig, 0)
					return nil
				}

				// The container.json file should be written *after* the container
				// has started because its StdFds cannot be initialized before.
				//
				// XXX How do we handle error here?
				d.writeContainerFile(container, c.ID)
				close(waitForRestore)
				if restoreCallback != nil {
					c.ProcessConfig.Process, err = os.FindProcess(restorePid)
					if err != nil {
						log.Debugf("cannot find restored process %d", restorePid)
						return err
					}
					c.ContainerPid = c.ProcessConfig.Process.Pid
					restoreCallback(&c.ProcessConfig, c.ContainerPid)
				}
				return nil
			})
		restoreOutputChan <- restoreOutput{exitCode, err}
	}()

	select {
	case restoreOutput := <-restoreOutputChan:
		// there was an error
		return restoreOutput.exitCode, restoreOutput.err
	case <-waitForRestore:
		// container restored
		break
	}

	// Wait for the container to exit.
	restoreOutput := <-restoreOutputChan
	return restoreOutput.exitCode, restoreOutput.err
}

func (d *driver) Terminate(c *execdriver.Command) error {
	defer d.cleanContainer(c.ID)
	container, err := d.factory.Load(c.ID)
	if err != nil {
		return err
	}
	defer container.Destroy()
	state, err := container.State()
	if err != nil {
		return err
	}
	pid := state.InitProcessPid
	currentStartTime, err := system.GetProcessStartTime(pid)
	if err != nil {
		return err
	}
	if state.InitProcessStartTime == currentStartTime {
		err = syscall.Kill(pid, 9)
		syscall.Wait4(pid, nil, 0, nil)
	}
	return err
}

func (d *driver) Info(id string) execdriver.Info {
	return &info{
		ID:     id,
		driver: d,
	}
}

func (d *driver) Name() string {
	return fmt.Sprintf("%s-%s", DriverName, Version)
}

func (d *driver) GetPidsForContainer(id string) ([]int, error) {
	d.Lock()
	active := d.activeContainers[id]
	d.Unlock()

	if active == nil {
		return nil, fmt.Errorf("active container for %s does not exist", id)
	}
	return active.Processes()
}

func (d *driver) cleanContainer(id string) error {
	d.Lock()
	delete(d.activeContainers, id)
	d.Unlock()
	return os.RemoveAll(filepath.Join(d.root, id))
}

func (d *driver) createContainerRoot(id string) error {
	return os.MkdirAll(filepath.Join(d.root, id), 0655)
}

func (d *driver) Clean(id string) error {
	return os.RemoveAll(filepath.Join(d.root, id))
}

func (d *driver) Stats(id string) (*execdriver.ResourceStats, error) {
	d.Lock()
	c := d.activeContainers[id]
	d.Unlock()
	if c == nil {
		return nil, execdriver.ErrNotRunning
	}
	now := time.Now()
	stats, err := c.Stats()
	if err != nil {
		return nil, err
	}
	memoryLimit := c.Config().Cgroups.Memory
	// if the container does not have any memory limit specified set the
	// limit to the machines memory
	if memoryLimit == 0 {
		memoryLimit = d.machineMemory
	}
	return &execdriver.ResourceStats{
		Stats:       stats,
		Read:        now,
		MemoryLimit: memoryLimit,
	}, nil
}

type TtyConsole struct {
	console libcontainer.Console
}

func NewTtyConsole(console libcontainer.Console, pipes *execdriver.Pipes, rootuid int) (*TtyConsole, error) {
	tty := &TtyConsole{
		console: console,
	}

	if err := tty.AttachPipes(pipes); err != nil {
		tty.Close()
		return nil, err
	}

	return tty, nil
}

func (t *TtyConsole) Master() libcontainer.Console {
	return t.console
}

func (t *TtyConsole) Resize(h, w int) error {
	return term.SetWinsize(t.console.Fd(), &term.Winsize{Height: uint16(h), Width: uint16(w)})
}

func (t *TtyConsole) AttachPipes(pipes *execdriver.Pipes) error {
	go func() {
		if wb, ok := pipes.Stdout.(interface {
			CloseWriters() error
		}); ok {
			defer wb.CloseWriters()
		}

		io.Copy(pipes.Stdout, t.console)
	}()

	if pipes.Stdin != nil {
		go func() {
			io.Copy(t.console, pipes.Stdin)

			pipes.Stdin.Close()
		}()
	}

	return nil
}

func (t *TtyConsole) Close() error {
	return t.console.Close()
}

func setupPipes(container *configs.Config, processConfig *execdriver.ProcessConfig, p *libcontainer.Process, pipes *execdriver.Pipes) error {
	var term execdriver.Terminal
	var err error

	if processConfig.Tty {
		rootuid, err := container.HostUID()
		if err != nil {
			return err
		}
		cons, err := p.NewConsole(rootuid)
		if err != nil {
			return err
		}
		term, err = NewTtyConsole(cons, pipes, rootuid)
	} else {
		p.Stdout = pipes.Stdout
		p.Stderr = pipes.Stderr
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		if pipes.Stdin != nil {
			go func() {
				io.Copy(w, pipes.Stdin)
				w.Close()
			}()
			p.Stdin = r
		}
		term = &execdriver.StdConsole{}
	}
	if err != nil {
		return err
	}
	processConfig.Terminal = term
	return nil
}

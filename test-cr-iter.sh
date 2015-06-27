#!/bin/bash

BASE_TIME=1
ID=$(docker run -d -v /out:/out gablg1/cr:iter sh -c 'iter > /out')
echo Started Docker container with ID $ID
sleep $(($BASE_TIME * 2)) 

FIRST_READ=$(cat /out)
echo /out has $FIRST_READ

echo Checkpointing...
docker checkpoint $ID
echo Restoring...
docker restore $ID
sleep $BASE_TIME 

SECOND_READ=$(cat /out)
echo /out has $SECOND_READ

docker kill $ID

if (("$SECOND_READ" > "$FIRST_READ")); then
    echo It works!
    exit 0
else
    echo Something seems wrong...
    exit 1
fi

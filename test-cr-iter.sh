#!/bin/bash

BASE_TIME=1
ID=$(docker run -d gablg1/cr:iter sh -c 'iter > /out')
echo Started Docker container with ID $ID
sleep $(($BASE_TIME * 2)) 

FIRST_SIZE=$(docker exec $ID sh -c 'wc -l < /out')
echo /out has $FIRST_SIZE lines

echo Checkpointing...
docker checkpoint $ID
echo Restoring...
docker restore $ID
sleep $BASE_TIME 

SECOND_SIZE=$(docker exec $ID sh -c 'wc -l < /out')
echo /out has $SECOND_SIZE lines

docker kill $ID

if (("$SECOND_SIZE" > "$FIRST_SIZE")); then
    echo It works!
    exit 0
else
    echo Something seems wrong...
    exit 1
fi

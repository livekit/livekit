#!/bin/bash

set -euxo pipefail

make
docker build -t lk-server .
IMG=$(docker images -q | awk 'NR==1')
docker tag lk-server:latest 203125320322.dkr.ecr.us-west-2.amazonaws.com/lk-server:"$IMG"
docker push 203125320322.dkr.ecr.us-west-2.amazonaws.com/lk-server:"$IMG"

# kon app deploy --tag "$IMG" lk-server

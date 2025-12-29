#!/bin/sh

set -eu

export COMPLEMENT_SHARE_ENV_PREFIX=PASS_
# export PASS_SYNAPSE_LOG_LEVEL=DEBUG
export COMPLEMENT_BASE_IMAGE=complement-babbleserv
export COMPLEMENT_ENABLE_DIRTY_RUNS=1

docker build -f docker/Dockerfile-complement --platform=linux/amd64 -t $COMPLEMENT_BASE_IMAGE .


args="$@"
if [ -z "$args" ]; then
    testNames=$(python scripts/complement-gen-testlist.py)
    args='-v -count=1 -run ^('"$testNames"')$'
fi

BABBLESERV_COMPLEMENT_EXTRA_ARGS=${BABBLESERV_COMPLEMENT_EXTRA_ARGS:-}
if [ -n "$BABBLESERV_COMPLEMENT_EXTRA_ARGS" ]; then
    args=$args' '$BABBLESERV_COMPLEMENT_EXTRA_ARGS
fi

echo
echo "Running complement with args: $args"
cd ../complement
# go test $args ./tests/...
gotestsum -- $args ./tests/...

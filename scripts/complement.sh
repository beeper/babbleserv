#!/bin/sh

set -eu

export COMPLEMENT_BASE_IMAGE=babbleserv-complement
export COMPLEMENT_ENABLE_DIRTY_RUNS=1

if [ ! -f "ed25519-active" ]; then
    ./scripts/cli.sh generate-signing-key -out=ed25519-active
fi

docker build -f docker/Dockerfile-complement --platform=linux/amd64 -t $COMPLEMENT_BASE_IMAGE .

args="$@"
if [ -z "$args" ]; then
    testNames=$(python scripts/complement-gen-testlist.py)
    args='-v -count=1 -run ^('"$testNames"')$'
fi

echo
echo "Running complement with args: $args"
cd ../complement
# go test $args ./tests/...
gotestsum -- $args ./tests/...

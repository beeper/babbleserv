#!/usr/bin/env bash

# FDB installs here on MacOS
if [ $(uname -s) = "Darwin" ]; then
    export DYLD_FALLBACK_LIBRARY_PATH=/usr/local/lib
fi

go run ./cmd/babbleserv-cli $@

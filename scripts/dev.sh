#!/usr/bin/env bash

# FDB installs here on MacOS
if [ $(uname -s) = "Darwin" ]; then
    export DYLD_FALLBACK_LIBRARY_PATH=/usr/local/lib
fi

CMD="gow -e go,sql,yaml"
if [ -n "${ONESHOT}" ]; then
    CMD="go"
fi

$CMD run ./cmd/babbleserv -prettyLogs -debug $@

#!/bin/bash

set -euo pipefail

# Bootstrap the TLS certificate
openssl genrsa -out $SERVER_NAME.key 2048
openssl req -new -sha256 -key $SERVER_NAME.key -subj "/C=US/ST=CA/O=MyOrg, Inc./CN=$SERVER_NAME" -out $SERVER_NAME.csr
openssl x509 -req -in $SERVER_NAME.csr -CA /complement/ca/ca.crt -CAkey /complement/ca/ca.key -CAcreateserial -out $SERVER_NAME.crt -days 1 -sha256

# Ensure fdb is running
service foundationdb start

# Run babbleserv
sed -i s/SERVER_NAME/$SERVER_NAME/ /complement-config.yaml
exec /build/babbleserv -config /complement-config.yaml -prettyLogs -trace -routes -workers

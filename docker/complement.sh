#!/bin/bash

set -exu

# Bootstrap the TLS certificate
echo "\
.include /etc/ssl/openssl.cnf

[SAN]
subjectAltName=DNS:${SERVER_NAME}" > $SERVER_NAME.tls.conf
openssl genrsa -out $SERVER_NAME.tls.key 2048
openssl req -new \
    -config $SERVER_NAME.tls.conf \
    -key $SERVER_NAME.tls.key \
    -out $SERVER_NAME.tls.csr \
    -subj "/CN=$SERVER_NAME" \
    -reqexts SAN
openssl x509 -req -in $SERVER_NAME.tls.csr \
    -CA /complement/ca/ca.crt -CAkey /complement/ca/ca.key -set_serial 1 \
    -out $SERVER_NAME.tls.crt -extfile $SERVER_NAME.tls.conf -extensions SAN

# Ensure fdb is running
service foundationdb start

# Generate a signing key (if needed)
test -f ed25519-active || /build/babbleserv-cli generate-signing-key -out=ed25519-active

# Configure
sed -i s/SERVER_NAME/$SERVER_NAME/ /complement-config.yaml

# Run babbleserv
exec /build/babbleserv -config /complement-config.yaml -prettyLogs -debug -routes -workers

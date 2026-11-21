#!/usr/bin/env bash
# SPDX-License-Identifier: AGPL-3.0-only
# Regenerates TEST-ONLY certificates (never use in production). CA + server(localhost) + runtime clients.
set -euo pipefail
export MSYS_NO_PATHCONV=1
cd "$(dirname "$0")"
mkdir -p certs
cd certs
D=36500
openssl req -x509 -newkey rsa:2048 -nodes -keyout ca.key -out ca.pem -days $D -subj "/CN=nurtuo-test-ca" 2>/dev/null
mk() { # name cn extensions
  openssl req -newkey rsa:2048 -nodes -keyout "$1.key" -out "$1.csr" -subj "/CN=$2" 2>/dev/null
  printf "%s\n" "$3" > "$1.ext"
  openssl x509 -req -in "$1.csr" -CA ca.pem -CAkey ca.key -CAcreateserial -out "$1.pem" -days $D -extfile "$1.ext" 2>/dev/null
  rm -f "$1.csr" "$1.ext"
}
mk server localhost "subjectAltName=DNS:localhost,IP:127.0.0.1
extendedKeyUsage=serverAuth"
mk rt-1 rt-1 "extendedKeyUsage=clientAuth"
mk rt-2 rt-2 "extendedKeyUsage=clientAuth"
# SAN-identified client: CN is a decoy, identity comes from the URI SAN.
mk rt-san decoy "subjectAltName=URI:spiffe://nurtuo/runtime/rt-san
extendedKeyUsage=clientAuth"
# Client signed by a different CA (must be refused at the TLS layer).
openssl req -x509 -newkey rsa:2048 -nodes -keyout rogue-ca.key -out rogue-ca.pem -days $D -subj "/CN=rogue-ca" 2>/dev/null
openssl req -newkey rsa:2048 -nodes -keyout rogue.key -out rogue.csr -subj "/CN=rt-1" 2>/dev/null
openssl x509 -req -in rogue.csr -CA rogue-ca.pem -CAkey rogue-ca.key -CAcreateserial -out rogue.pem -days $D 2>/dev/null
rm -f rogue.csr ./*.srl rogue-ca.key ca.key

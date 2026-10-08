#!/bin/sh
set -eu
ROOT=/srv/cst-pilot-server
CA="$ROOT/tls/ca"
SERVER="$ROOT/tls/server"
umask 077
if test -f "$SERVER/server.crt" && openssl x509 -checkend 2592000 -noout -in "$SERVER/server.crt" >/dev/null 2>&1; then
  exit 0
fi
TMP=$(mktemp -d "$ROOT/tls/renew-XXXXXX")
trap 'rm -rf "$TMP"' EXIT
openssl req -new -newkey rsa:2048 -nodes -keyout "$TMP/server.key" -out "$TMP/server.csr" -subj "/CN=8.163.28.9" >/dev/null 2>&1
openssl x509 -req -in "$TMP/server.csr" -CA "$CA/ca.crt" -CAkey "$CA/ca.key" -CAcreateserial -days 90 -sha256 -extfile "$ROOT/server.ext" -out "$TMP/server.crt" >/dev/null 2>&1
install -m 0640 "$TMP/server.key" "$SERVER/server.key"
install -m 0644 "$TMP/server.crt" "$SERVER/server.crt"
chown 0:65532 "$SERVER/server.key" "$SERVER/server.crt"
if docker ps --format '{{.Names}}' | grep -q '^cst-pilot-telemetry-gateway-'; then
  docker compose --env-file "$ROOT/telemetry.env" --env-file "$ROOT/release.env" -f "$ROOT/compose.yaml" restart gateway
fi

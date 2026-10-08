#!/bin/sh
set -eu
ROOT=/srv/cst-pilot-server
cd "$ROOT"
install -d -m 0750 data
chown 65532:65532 data
install -d -m 0700 backups tls/ca
install -d -m 0750 tls/server
chown 0:65532 tls/server
if ! test -f tls/ca/ca.crt; then
  umask 077
  openssl req -x509 -newkey rsa:3072 -nodes -sha256 -days 3650 -keyout tls/ca/ca.key -out tls/ca/ca.crt -subj "/CN=Tim CST Pilot Telemetry CA" -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" >/dev/null 2>&1
fi
chmod 0600 tls/ca/ca.key
chmod 0644 tls/ca/ca.crt
/bin/sh "$ROOT/renew-tls.sh"
install -m 0644 cst-telemetry-backup.service cst-telemetry-backup.timer cst-telemetry-tls.service cst-telemetry-tls.timer /etc/systemd/system/
systemctl daemon-reload
systemctl enable --now cst-telemetry-backup.timer cst-telemetry-tls.timer

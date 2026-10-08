#!/usr/bin/env python3
"""Update an existing receiver while preserving credentials, SQLite and TLS keys."""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import sqlite3
import ssl
import subprocess
import sys
import time
import urllib.request


def run(*args, **kwargs):
    subprocess.run(args, check=True, **kwargs)


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def session_ids(database):
    connection = sqlite3.connect("file:" + str(database) + "?mode=ro", uri=True)
    try:
        return {row[0] for row in connection.execute("SELECT record_id FROM sessions")}
    finally:
        connection.close()


def atomic_copy(source, target, mode=0o644, group=None):
    temp = target.with_name(target.name + ".next")
    shutil.copyfile(str(source), str(temp))
    os.chmod(temp, mode)
    if group is not None:
        os.chown(temp, 0, group)
    os.replace(str(temp), str(target))


def read_health(site, root):
    if site == "cstoa":
        with urllib.request.urlopen("http://127.0.0.1:8787/healthz", timeout=5) as response:
            return json.load(response)
    context = ssl.create_default_context(cafile=str(root / "tls/ca/ca.crt"))
    connection = http.client.HTTPSConnection("8.163.28.9", 8445, context=context, timeout=5)
    connection.sock = context.wrap_socket(socket.create_connection(("127.0.0.1", 8445), 5), server_hostname="8.163.28.9")
    try:
        connection.request("GET", "/healthz")
        response = connection.getresponse()
        if response.status != 200:
            raise RuntimeError("HTTPS health status " + str(response.status))
        return json.loads(response.read().decode())
    finally:
        connection.close()


def wait_health(site, root, version=None):
    last_error = None
    for _ in range(40):
        try:
            health = read_health(site, root)
            if health.get("ok") and (version is None or health.get("version") == version):
                return health
        except Exception as error:
            last_error = error
        time.sleep(0.5)
    raise RuntimeError("Receiver health/version validation failed: " + str(last_error))


def deployment(site, candidate):
    candidate = candidate.resolve()
    metadata = json.loads((candidate / "BUILD-INFO.json").read_text(encoding="utf-8-sig"))
    revision = metadata["revision"]
    if len(revision) != 40 or any(char not in "0123456789abcdef" for char in revision):
        raise ValueError("Invalid release revision")
    if digest(candidate / "telemetry-receiver") != metadata["binarySha256"]:
        raise ValueError("Release binary hash mismatch")
    root = Path("/opt/cst-pilot-server" if site == "cstoa" else "/srv/cst-pilot-server")
    database = Path("/var/lib/cst-telemetry/telemetry.db") if site == "cstoa" else root / "data/telemetry.db"
    if not (root / "telemetry.env").is_file() or not database.is_file():
        raise RuntimeError("Existing credentials and database are required")
    env_hash = digest(root / "telemetry.env")
    ca_hash = digest(root / "tls/ca/ca.crt") if site == "timserver_1" else None
    before = session_ids(database)
    backup_dir = database.parent / "backups" if site == "cstoa" else root / "backups"
    run(sys.executable, str(candidate / "backup.py"), "--database", str(database), "--output-dir", str(backup_dir))
    rollback = root / "releases" / (revision[:12] + "-previous")
    rollback.mkdir(mode=0o700, parents=True, exist_ok=False)
    files = ["telemetry-receiver", "BUILD-INFO.json", "backup.py", "compose.yaml", "Dockerfile", ".dockerignore", "Caddyfile", "release.env", "renew-tls.sh", "server.ext", "prepare.sh"]
    existing = [name for name in files if (root / name).is_file()]
    for name in existing:
        shutil.copy2(str(root / name), str(rollback / name))
    units = ["cst-telemetry-backup.service", "cst-telemetry-backup.timer"]
    if site == "cstoa":
        units.append("cst-telemetry.service")
    else:
        units.extend(["cst-telemetry-tls.service", "cst-telemetry-tls.timer"])
    previous_units = []
    unit_root = Path("/etc/systemd/system")
    for name in units:
        path = unit_root / name
        if path.exists():
            shutil.copy2(str(path), str(rollback / name))
            previous_units.append(name)
    previous_timers = {name: subprocess.run(["systemctl", "is-enabled", name], stdout=subprocess.PIPE, stderr=subprocess.PIPE).returncode == 0 for name in units if name.endswith(".timer")}
    old_tag = None
    old_image_name = None
    group = __import__("grp").getgrnam("csttele").gr_gid if site == "cstoa" else None
    if site == "timserver_1":
        container = "cst-pilot-telemetry-receiver-1"
        image = subprocess.check_output(["docker", "inspect", "--format", "{{.Image}}", container]).decode().strip()
        old_image_name = subprocess.check_output(["docker", "inspect", "--format", "{{.Config.Image}}", container]).decode().strip()
        old_tag = "cst-pilot-telemetry:rollback-" + revision[:12]
        run("docker", "image", "tag", image, old_tag)

    def compose(*args):
        command = ["docker", "compose", "--env-file", "telemetry.env"]
        if (root / "release.env").exists():
            command += ["--env-file", "release.env"]
        command.extend(args)
        run(*command, cwd=str(root))

    try:
        atomic_copy(candidate / "telemetry-receiver", root / "telemetry-receiver", 0o750 if site == "cstoa" else 0o755, group)
        atomic_copy(candidate / "backup.py", root / "backup.py")
        atomic_copy(candidate / "BUILD-INFO.json", root / "BUILD-INFO.json")
        config_root = candidate / "deploy" / site
        if site == "timserver_1":
            for name in ["compose.yaml", "Dockerfile", ".dockerignore", "Caddyfile", "renew-tls.sh", "server.ext", "prepare.sh"]:
                atomic_copy(config_root / name, root / name)
            (root / "release.env").write_text("TELEMETRY_RELEASE=" + revision[:12] + "\n")
            compose("up", "-d", "--build", "--wait", "--wait-timeout", "60")
        for name in units:
            atomic_copy(config_root / name, unit_root / name)
        run("systemctl", "daemon-reload")
        if site == "cstoa":
            run("systemctl", "restart", "cst-telemetry.service")
        for name in previous_timers:
            run("systemctl", "enable", "--now", name)
        health = wait_health(site, root, revision)
        if not before.issubset(session_ids(database)):
            raise RuntimeError("Existing session records were lost")
        if digest(root / "telemetry.env") != env_hash:
            raise RuntimeError("Service credential file changed")
        if ca_hash and digest(root / "tls/ca/ca.crt") != ca_hash:
            raise RuntimeError("Client trust CA changed")
        run("systemctl", "start", "cst-telemetry-backup.service")
        print(json.dumps({"site": site, "revision": revision, "binarySha256": digest(root / "telemetry-receiver"), "health": health, "existingSessionsPreserved": True, "credentialsPreserved": True, "rollback": str(rollback)}))
    except Exception:
        for name in files:
            if name in existing:
                atomic_copy(rollback / name, root / name, 0o750 if name == "telemetry-receiver" and site == "cstoa" else 0o755 if name == "telemetry-receiver" else 0o644, group if name == "telemetry-receiver" and site == "cstoa" else None)
            elif (root / name).is_file():
                (root / name).unlink()
        for name in units:
            if name in previous_units:
                atomic_copy(rollback / name, unit_root / name)
            elif (unit_root / name).exists():
                (unit_root / name).unlink()
        run("systemctl", "daemon-reload")
        for name, enabled in previous_timers.items():
            if not enabled:
                subprocess.run(["systemctl", "disable", "--now", name], stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        if site == "cstoa":
            run("systemctl", "restart", "cst-telemetry.service")
        else:
            # Restore the prior receiver and gateway images without rebuilding.
            if "release.env" in existing:
                compose("up", "-d", "--no-build", "--wait")
            else:
                run("docker", "image", "tag", old_tag, old_image_name)
                compose("up", "-d", "--no-build", "--wait")
        wait_health(site, root)
        print("Deployment failed; previous receiver restored", file=sys.stderr)
        raise


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("site", choices=["cstoa", "timserver_1"])
    parser.add_argument("--candidate", type=Path, required=True)
    args = parser.parse_args()
    deployment(args.site, args.candidate)

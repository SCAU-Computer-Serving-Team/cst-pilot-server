#!/usr/bin/env python3
"""在线 SQLite 一致性备份，保留 30 天。"""
from datetime import datetime, timedelta, timezone
from pathlib import Path
import os
import sqlite3

root = Path("/srv/cst-pilot-server")
backup = root / "backups"
backup.mkdir(mode=0o700, exist_ok=True)
os.umask(0o077)
now = datetime.now(timezone.utc)
target = backup / f"telemetry-{now:%Y%m%d-%H%M%S}.db"
source = sqlite3.connect(f"file:{root / 'data/telemetry.db'}?mode=ro", uri=True)
try:
    destination = sqlite3.connect(target)
    try:
        source.backup(destination)
        result = destination.execute("PRAGMA quick_check").fetchone()[0]
        if result != "ok":
            raise RuntimeError("SQLite backup consistency check failed")
    finally:
        destination.close()
finally:
    source.close()
cutoff = (now - timedelta(days=30)).timestamp()
for path in backup.glob("telemetry-*.db"):
    if path.stat().st_mtime < cutoff:
        path.unlink()
print("telemetry backup completed")

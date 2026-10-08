#!/usr/bin/env python3
"""SQLite 在线一致性备份，不读取服务凭据。"""
import argparse
from datetime import datetime, timedelta, timezone
from pathlib import Path
import os
import sqlite3


def backup(database, output, retention_days=30):
    if not hasattr(sqlite3.Connection, "backup"):
        raise RuntimeError("SQLite online backup requires Python 3.7 or newer")
    if retention_days < 1:
        raise ValueError("retention_days must be positive")
    if not database.is_file():
        raise FileNotFoundError("telemetry database does not exist")
    os.umask(0o077)
    output.mkdir(mode=0o700, parents=True, exist_ok=True)
    now = datetime.now(timezone.utc)
    target = output / ("telemetry-" + now.strftime("%Y%m%d-%H%M%S-%f") + ".db")
    source = sqlite3.connect("file:" + str(database.resolve()) + "?mode=ro", uri=True)
    try:
        destination = sqlite3.connect(str(target))
        try:
            source.backup(destination)
            if destination.execute("PRAGMA quick_check").fetchone()[0] != "ok":
                raise RuntimeError("SQLite backup consistency check failed")
        finally:
            destination.close()
    except Exception:
        if target.exists():
            target.unlink()
        raise
    finally:
        source.close()
    cutoff = (now - timedelta(days=retention_days)).timestamp()
    for path in output.glob("telemetry-*.db"):
        if path.stat().st_mtime < cutoff:
            path.unlink()
    return target


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--database", type=Path, required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--retention-days", type=int, default=30)
    args = parser.parse_args()
    backup(args.database, args.output_dir, args.retention_days)
    print("telemetry backup completed")


if __name__ == "__main__":
    main()

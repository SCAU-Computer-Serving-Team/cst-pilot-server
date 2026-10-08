import importlib.util
import os
from pathlib import Path
import sqlite3
import tempfile
import time
import unittest

spec = importlib.util.spec_from_file_location("telemetry_backup", Path(__file__).with_name("backup.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


class BackupTests(unittest.TestCase):
    def test_online_wal_backup_and_retention(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            database = root / "telemetry.db"
            connection = sqlite3.connect(str(database))
            try:
                connection.execute("PRAGMA journal_mode=WAL")
                connection.execute("CREATE TABLE sessions(record_id TEXT PRIMARY KEY)")
                connection.execute("INSERT INTO sessions VALUES('test-record')")
                connection.commit()
                output = root / "backups"
                output.mkdir()
                stale = output / "telemetry-old.db"
                stale.write_text("old")
                old = time.time() - 31 * 86400
                os.utime(stale, (old, old))
                target = module.backup(database, output)
                self.assertFalse(stale.exists())
                destination = sqlite3.connect(str(target))
                try:
                    self.assertEqual(destination.execute("SELECT record_id FROM sessions").fetchone()[0], "test-record")
                    self.assertEqual(destination.execute("PRAGMA quick_check").fetchone()[0], "ok")
                finally:
                    destination.close()
                self.assertTrue(database.exists())
            finally:
                connection.close()

    def test_missing_database_does_not_create_empty_backup(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            with self.assertRaises(FileNotFoundError):
                module.backup(root / "missing.db", root / "backups")
            self.assertFalse((root / "backups").exists())


if __name__ == "__main__":
    unittest.main()

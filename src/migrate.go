package main

import "strings"

// 仅增加派生列；保留已有记录与原始 payload，旧程序仍可读取。
func (s *store) migrateEnvironment() error {
	rows, err := s.db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return err
	}
	columns := map[string]bool{}
	for rows.Next() {
		var cid, notNull, primary int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, field := range []struct{ name, kind string }{
		{"installation_id", "TEXT"}, {"environment_id", "TEXT"}, {"environment_source", "TEXT"},
		{"os_platform", "TEXT"}, {"os_name", "TEXT"}, {"os_display_version", "TEXT"}, {"os_is_pe", "INTEGER"},
		{"runtime_version", "TEXT"}, {"runtime_arch", "TEXT"},
	} {
		if !columns[field.name] {
			if _, err := tx.Exec("ALTER TABLE sessions ADD COLUMN " + field.name + " " + field.kind); err != nil {
				return err
			}
		}
	}
	for _, statement := range []string{
		"CREATE INDEX IF NOT EXISTS idx_sessions_installation ON sessions(installation_id, received_at)",
		"CREATE INDEX IF NOT EXISTS idx_sessions_environment ON sessions(installation_id, environment_id)",
		"CREATE TABLE IF NOT EXISTS installations (installation_id TEXT PRIMARY KEY, public_key TEXT NOT NULL, first_seen_at TEXT NOT NULL)",
	} {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var count int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('daily_rollup') WHERE name='installation_id'").Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		// 汇总可从 sessions 重算；原始记录保持不变。
		statement := schema[strings.Index(schema, "CREATE TABLE IF NOT EXISTS daily_rollup ("):]
		_, err := s.db.Exec("DROP TABLE daily_rollup;" + statement)
		return err
	}
	return nil
}

func nullableString(value any) any {
	if value, ok := value.(string); ok && value != "" {
		return value
	}
	return nil
}
func nullableBool(value any) any {
	if value, ok := value.(bool); ok {
		if value {
			return 1
		}
		return 0
	}
	return nil
}

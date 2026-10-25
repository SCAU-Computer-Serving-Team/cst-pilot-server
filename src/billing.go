package main

import "database/sql"

// 缺失和不合法的账单均保持 NULL。数值单位为网关 quota point。
func billingValues(p map[string]any) (requests, settled, points any) {
	b, ok := p["cstoaBilling"].(map[string]any)
	if !ok {
		return
	}
	r, rok := wholeNumber(b["requests"])
	s, sok := wholeNumber(b["settledRequests"])
	if !rok || !sok || r < 1 || s > r {
		return
	}
	q, qok := wholeNumber(b["quotaPoints"])
	if (s == 0 && b["quotaPoints"] != nil) || (s > 0 && !qok) {
		return
	}
	requests, settled = r, s
	if s > 0 {
		points = q
	}
	return
}

func wholeNumber(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !isNaNOrInf(f) && f >= 0 && f <= 9007199254740991 && f == float64(int64(f))
}

func modelCosts(p map[string]any) (cny, usd, unpriced float64) {
	for _, model := range recordArray(p, "models") {
		cost, known := model["cost"].(float64)
		if model["provider"] == "cstoa" || !known || isNaNOrInf(cost) || cost < 0 {
			unpriced += num(model["turns"])
			continue
		}
		switch model["currency"] {
		case "CNY":
			cny += cost
		case "USD":
			usd += cost
		default:
			unpriced += num(model["turns"])
		}
	}
	return
}

func incompleteUsage(p map[string]any) any {
	var total float64
	known := false
	for _, model := range recordArray(p, "models") {
		if value, ok := wholeNumber(model["incompleteUsageTurns"]); ok {
			total += value
			known = true
		}
	}
	if known {
		return total
	}
	return nil
}

func addNullable(target *sql.NullFloat64, value any) {
	if f, ok := value.(float64); ok {
		target.Valid = true
		target.Float64 += f
	}
}
func nullableNumber(value sql.NullFloat64) any {
	if value.Valid {
		return value.Float64
	}
	return nil
}

var billingColumns = []string{"cstoa_requests", "cstoa_settled_requests", "cstoa_quota_points", "incomplete_usage_turns"}

func (s *store) migrateBilling() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	added := false
	for _, table := range []string{"sessions", "daily_rollup"} {
		for _, column := range billingColumns {
			var count int
			if err := tx.QueryRow("SELECT COUNT(*) FROM pragma_table_info(?) WHERE name=?", table, column).Scan(&count); err != nil {
				return err
			}
			if count == 0 {
				if _, err := tx.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " REAL"); err != nil {
					return err
				}
				if table == "sessions" {
					added = true
				}
			}
		}
	}
	if !added {
		return tx.Commit()
	}
	rows, err := tx.Query("SELECT record_id,payload FROM sessions")
	if err != nil {
		return err
	}
	type entry struct {
		id string
		p  map[string]any
	}
	var entries []entry
	for rows.Next() {
		var id, raw string
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		var p map[string]any
		if jsonUnmarshal([]byte(raw), &p) == nil {
			entries = append(entries, entry{id, p})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		r, n, q := billingValues(entry.p)
		cny, usd, unpriced := modelCosts(entry.p)
		if _, err := tx.Exec("UPDATE sessions SET cstoa_requests=?,cstoa_settled_requests=?,cstoa_quota_points=?,incomplete_usage_turns=?,cost_cny=?,cost_usd=?,unpriced_turns=? WHERE record_id=?", r, n, q, incompleteUsage(entry.p), cny, usd, unpriced, entry.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

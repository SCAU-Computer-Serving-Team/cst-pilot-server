package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestCstoaBillingNullableCoverageAndRollup(t *testing.T) {
	st := testStore(t)
	for _, item := range []struct{ id, data string }{
		{"partial", `{"cstoaBilling":{"requests":3,"settledRequests":2,"quotaPoints":198},"models":[{"provider":"cstoa","turns":2,"currency":"CNY","cost":null,"incompleteUsageTurns":1}]}`},
		{"missing", `{"cstoaBilling":{"requests":1,"settledRequests":0,"quotaPoints":null}}`},
		{"zero", `{"cstoaBilling":{"requests":1,"settledRequests":1,"quotaPoints":0}}`},
		{"legacy", `{"models":[{"provider":"cstoa","turns":1,"currency":"CNY","cost":0}]}`},
	} {
		var p map[string]any
		if err := jsonUnmarshal([]byte(item.data), &p); err != nil {
			t.Fatal(err)
		}
		for range 2 {
			if _, err := st.insertBatch([]insertRow{{recordID: item.id, mid: "member", receivedAt: "2026-10-25T00:00:00Z", payload: p}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	var points, requests, settled, incomplete sql.NullFloat64
	var unpriced float64
	if err := st.db.QueryRow("SELECT cstoa_quota_points,cstoa_requests,cstoa_settled_requests,incomplete_usage_turns,unpriced_turns FROM sessions WHERE record_id='partial'").Scan(&points, &requests, &settled, &incomplete, &unpriced); err != nil {
		t.Fatal(err)
	}
	if !points.Valid || points.Float64 != 198 || requests.Float64 != 3 || settled.Float64 != 2 || incomplete.Float64 != 1 || unpriced != 2 {
		t.Fatalf("partial: %v %v %v %v unpriced=%v", points, requests, settled, incomplete, unpriced)
	}
	for _, id := range []string{"missing", "legacy"} {
		if err := st.db.QueryRow("SELECT cstoa_quota_points FROM sessions WHERE record_id=?", id).Scan(&points); err != nil {
			t.Fatal(err)
		}
		if points.Valid {
			t.Fatalf("%s quota invented: %v", id, points)
		}
	}
	if err := st.refreshRollup(); err != nil {
		t.Fatal(err)
	}
	if err := st.db.QueryRow("SELECT cstoa_quota_points,cstoa_requests,cstoa_settled_requests,incomplete_usage_turns,unpriced_turns FROM daily_rollup").Scan(&points, &requests, &settled, &incomplete, &unpriced); err != nil {
		t.Fatal(err)
	}
	if points.Float64 != 198 || requests.Float64 != 5 || settled.Float64 != 3 || incomplete.Float64 != 1 || unpriced != 3 {
		t.Fatalf("rollup: %v %v %v %v %v", points, requests, settled, incomplete, unpriced)
	}
	csv, err := st.exportSessionsCsv()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(csv, "cstoa_quota_points") {
		t.Fatal("billing missing from csv")
	}
}

func TestBillingMigrationPreservesPayloadAndUnknowns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	payload := `{"v":"0.1","models":[{"provider":"cstoa","currency":"CNY","cost":0,"turns":2}]}`
	if _, err := db.Exec("INSERT INTO sessions(record_id,mid,device_id,received_at,payload) VALUES('legacy','member','','2026-10-25T00:00:00Z',?)", payload); err != nil {
		t.Fatal(err)
	}
	db.Close()
	for range 2 {
		st, err := openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		var stored string
		var points sql.NullFloat64
		var unpriced float64
		if err := st.db.QueryRow("SELECT payload,cstoa_quota_points,unpriced_turns FROM sessions WHERE record_id='legacy'").Scan(&stored, &points, &unpriced); err != nil {
			t.Fatal(err)
		}
		if stored != payload || points.Valid || unpriced != 2 {
			t.Fatalf("legacy changed: %s %v %v", stored, points, unpriced)
		}
		if err := st.refreshRollup(); err != nil {
			t.Fatal(err)
		}
		if err := st.db.QueryRow("SELECT cstoa_quota_points FROM daily_rollup").Scan(&points); err != nil {
			t.Fatal(err)
		}
		if points.Valid {
			t.Fatal("unknown rollup became zero")
		}
		st.close()
	}
}

func TestCstoaMalformedBillingDoesNotInventZero(t *testing.T) {
	for _, text := range []string{`{}`, `{"requests":1,"settledRequests":0,"quotaPoints":0}`, `{"requests":1,"settledRequests":2,"quotaPoints":20}`, `{"requests":1,"settledRequests":1,"quotaPoints":-1}`} {
		var billing map[string]any
		_ = jsonUnmarshal([]byte(text), &billing)
		_, _, points := billingValues(map[string]any{"cstoaBilling": billing})
		if points != nil {
			t.Fatalf("invalid billing accepted: %s", text)
		}
	}
}

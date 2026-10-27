package main

import (
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
)

func TestBillingMigrationMemoryBound(t *testing.T) {
	const rows = 192
	if path := os.Getenv("CST_BILLING_MEMORY_DB"); path != "" {
		// Exercise startup in a fresh process: fixture creation must not affect the heap high-water mark.
		debug.SetMemoryLimit(24 << 20)
		st, err := openStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer st.close()
		var count int
		var points float64
		if err := st.db.QueryRow("SELECT COUNT(*),SUM(cstoa_quota_points) FROM sessions").Scan(&count, &points); err != nil {
			t.Fatal(err)
		}
		if count != rows || points != rows*198 {
			t.Fatalf("migration lost rows or billing: %d/%v", count, points)
		}
		var memory runtime.MemStats
		runtime.ReadMemStats(&memory)
		if memory.HeapSys > 64<<20 {
			t.Fatalf("migration heap reservation = %d MiB, exceeds receiver memory budget", memory.HeapSys>>20)
		}
		return
	}
	path := filepath.Join(t.TempDir(), "large-legacy.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	padding := strings.Repeat("x", 512<<10)
	for i := range rows {
		id := fmt.Sprintf("legacy-%03d", i)
		payload := fmt.Sprintf(`{"v":"0.1","type":"session","recordId":%q,"errors":[{"message":%q}],"cstoaBilling":{"requests":1,"settledRequests":1,"quotaPoints":198}}`, id, padding)
		if _, err := tx.Exec("INSERT INTO sessions(record_id,mid,device_id,received_at,payload) VALUES(?,'member','','2026-10-25T00:00:00Z',?)", id, payload); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(os.Args[0], "-test.run=^TestBillingMigrationMemoryBound$", "-test.timeout=25s")
	child.Env = append(os.Environ(), "CST_BILLING_MEMORY_DB="+path, "GOMEMLIMIT=24MiB")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("memory-limited migration failed: %v\n%s", err, output)
	}
}

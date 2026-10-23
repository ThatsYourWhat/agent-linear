package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func statusIdentifiers(t *testing.T, output []byte) []string {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(output))
	var ids []string
	for {
		var value any
		if err := decoder.Decode(&value); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if row, ok := value.([]any); ok {
			ids = append(ids, row[0].(string))
		}
	}
	return ids
}

func TestStatusRecentFiveAndAllHistory(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, ".runs")
	if err := os.Mkdir(runs, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	starts := []int{7, 1, 6, 2, 5, 3, 4}
	states := []string{"ready", "blocked", "ready", "failed", "ready", "ready", "running"}
	for i, hour := range starts {
		folder := filepath.Join(runs, fmt.Sprint(i))
		if err = os.Mkdir(folder, 0700); err != nil {
			t.Fatal(err)
		}
		id := fmt.Sprintf("APP-%d", i+1)
		if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", id, id, states[i], folder, ""); err != nil {
			t.Fatal(err)
		}
		activity := map[string]any{"started_at": base.Add(time.Duration(hour) * time.Hour).Format(time.RFC3339Nano), "last_activity": base.Add(100 * time.Hour).Format(time.RFC3339Nano)}
		if err = writeJSON(filepath.Join(folder, "activity.json"), activity); err != nil {
			t.Fatal(err)
		}
	}
	// Deferred rows are not launched work and must not displace a recent run.
	if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", "deferred", "APP-8", "deferred", filepath.Join(runs, "preflight"), ""); err != nil {
		t.Fatal(err)
	}
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	expected := []string{"APP-1", "APP-3", "APP-5", "APP-7", "APP-6"}
	if ids := statusIdentifiers(t, out.Bytes()); !reflect.DeepEqual(ids, expected) {
		t.Fatal("wrong recent runs/order", ids)
	}
	if bytes.Contains(out.Bytes(), []byte("APP-2")) || bytes.Contains(out.Bytes(), []byte("APP-4")) || bytes.Contains(out.Bytes(), []byte("APP-8")) {
		t.Fatal("older or deferred issue metadata leaked into the limited view", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"blocked_runs":1`)) || !bytes.Contains(out.Bytes(), []byte(`"failed_runs":1`)) || !bytes.Contains(out.Bytes(), []byte(`"attention_required":true`)) {
		t.Fatal("hidden history was excluded from attention totals", out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"poll_health"`)) || !bytes.Contains(out.Bytes(), []byte(`"worker_active"`)) {
		t.Fatal("health indicators lost", out.String())
	}
	out.Reset()
	if err = executeTest(r, "status", "--all"); err != nil {
		t.Fatal(err)
	}
	expected = append(expected, "APP-4", "APP-2", "APP-8")
	if ids := statusIdentifiers(t, out.Bytes()); !reflect.DeepEqual(ids, expected) {
		t.Fatal("wrong full history", ids)
	}
	var count int
	if err = db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&count); err != nil || count != 8 {
		t.Fatal("status discarded claims", count, err)
	}
	if status, err := claimedStatus(db, "APP-2"); err != nil || status != "blocked" {
		t.Fatal("status altered hidden claim", status, err)
	}
}

func TestStatusLegacyHistoryAndDeferredPromotion(t *testing.T) {
	root := t.TempDir()
	runs := filepath.Join(root, ".runs")
	if err := os.Mkdir(runs, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for i := 1; i <= 7; i++ {
		id := fmt.Sprintf("APP-%d", i)
		status := "ready"
		if i == 1 {
			status = "deferred"
		}
		if _, err = db.Exec("INSERT INTO runs VALUES (?,?,?,?,?)", id, id, status, filepath.Join(runs, id), ""); err != nil {
			t.Fatal(err)
		}
	}
	r := newRunner(filepath.Join(root, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	expected := []string{"APP-7", "APP-6", "APP-5", "APP-4", "APP-3"}
	if ids := statusIdentifiers(t, out.Bytes()); !reflect.DeepEqual(ids, expected) {
		t.Fatal("legacy order lost", ids)
	}
	// A previously deferred issue can retain an old rowid but start most recently.
	if _, err = db.Exec("UPDATE runs SET status='running' WHERE issue_id='APP-1'"); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(runs, "APP-1")
	if err = os.Mkdir(folder, 0700); err != nil {
		t.Fatal(err)
	}
	if err = recordActivity(folder, "starting"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = executeTest(r, "status"); err != nil {
		t.Fatal(err)
	}
	expected = []string{"APP-1", "APP-7", "APP-6", "APP-5", "APP-4"}
	if ids := statusIdentifiers(t, out.Bytes()); !reflect.DeepEqual(ids, expected) {
		t.Fatal("actual start time did not override old insertion order", ids)
	}
	out.Reset()
	if err = executeTest(r, "status", "--all"); err != nil {
		t.Fatal(err)
	}
	if ids := statusIdentifiers(t, out.Bytes()); len(ids) != 7 {
		t.Fatal("legacy history missing", ids)
	}
}

package main

import (
	"path/filepath"
	"testing"
)

func TestSkipCIChecks(t *testing.T) {
	root, r, c, _ := gapRunner(t, "ci-failed")
	c.Projects[projectID].SkipCIChecks = true
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
		t.Fatalf("expected ready with CI checks skipped, got %q %v", status, err)
	}
}

func TestReconcileHonorsSkipCIChecksChange(t *testing.T) {
	root, r, c, api := gapRunner(t, "ci-failed")
	if err := executeTest(r, "poll"); err == nil {
		t.Fatal("ci-failed should not complete without skipping")
	}
	// The run failed on CI; now the operator enables skip_ci_checks and
	// reconciles. The changed route must not invalidate the run, and the current
	// skip value must be honored.
	c.Projects[projectID].SkipCIChecks = true
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "reconcile", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
		t.Fatalf("expected ready after reconcile with skip_ci_checks, got %q %v", status, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if textAt(call.Variables, "input", "stateId") == reviewID {
			return
		}
	}
	t.Fatal("reconcile did not move the issue to review")
}

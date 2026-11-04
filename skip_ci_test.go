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

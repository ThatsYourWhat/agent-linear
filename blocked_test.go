package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// blockedIssue returns a ready issue with one incoming "blocks" relation whose
// blocker is in the given workflow state type.
func blockedIssue(blockerState string) map[string]any {
	issue := testIssue()
	issue["inverseRelations"] = map[string]any{
		"nodes": []any{
			map[string]any{
				"type": "blocks",
				"issue": map[string]any{
					"identifier": "APP-0",
					"state":      map[string]any{"type": blockerState},
				},
			},
		},
	}
	return issue
}

func TestBlockedByHelper(t *testing.T) {
	if got := blockedBy(blockedIssue("started")); len(got) != 1 || got[0] != "APP-0" {
		t.Fatalf("expected one open blocker APP-0, got %v", got)
	}
	for _, terminal := range []string{"completed", "canceled", "duplicate"} {
		if got := blockedBy(blockedIssue(terminal)); len(got) != 0 {
			t.Fatalf("terminal state %q should resolve the block, got %v", terminal, got)
		}
	}
	for _, kind := range []string{"related", "duplicate", "similar", ""} {
		issue := testIssue()
		issue["inverseRelations"] = map[string]any{"nodes": []any{
			map[string]any{"type": kind, "issue": map[string]any{"identifier": "APP-0", "state": map[string]any{"type": "started"}}},
		}}
		if got := blockedBy(issue); len(got) != 0 {
			t.Fatalf("non-blocks relation %q should be ignored, got %v", kind, got)
		}
	}
}

func TestBlockedIssueIsNotClaimed(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	api.mu.Lock()
	api.issue = blockedIssue("started")
	api.mu.Unlock()

	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}

	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "" {
		t.Fatalf("blocked issue was claimed: %q %v", seen, err)
	}

	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "issueUpdate(") || strings.Contains(call.Query, "commentCreate(") {
			t.Errorf("blocked issue caused a Linear mutation: %s", call.Query)
		}
	}
}

func TestUnblockedIssueIsWorked(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	api.mu.Lock()
	api.issue = blockedIssue("completed")
	api.mu.Unlock()

	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if seen, err := claimedStatus(db, issueID); err != nil || seen != "ready" {
		t.Fatalf("unblocked issue was not worked: %q %v", seen, err)
	}
}

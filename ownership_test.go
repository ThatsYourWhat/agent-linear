package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPickupRespectsAssignedOwner(t *testing.T) {
	for _, owner := range []string{"", userID, projectID} {
		t.Run(owner, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "ready")
			if owner != "" {
				api.issue["assignee"] = map[string]any{"id": owner}
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			if owner == projectID {
				if exists(filepath.Join(root, "preflight-calls")) || exists(filepath.Join(root, ".runs", issueID)) {
					t.Fatal("another owner's work was started")
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				for _, call := range api.calls {
					if strings.Contains(call.Query, "mutation") {
						t.Fatal("another owner's issue was mutated")
					}
				}
				return
			}
			if !exists(filepath.Join(root, ".runs", issueID, "result.json")) {
				t.Fatal("eligible owned/unassigned issue did not run")
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if textAt(call.Variables, "input", "stateId") == progressID {
					requested := textAt(call.Variables, "input", "assigneeId")
					if owner == userID && requested != "" {
						t.Fatal("existing assignment was rewritten")
					}
					if owner == "" && requested != userID {
						t.Fatal("unassigned issue was not claimed")
					}
				}
			}
		})
	}
}

func TestReconcileRequiresCurrentOwner(t *testing.T) {
	for _, scenario := range []string{"same-owner", "other-user", "reassigned", "unassigned"} {
		t.Run(scenario, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "ready")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			switch scenario {
			case "other-user":
				api.viewerID = projectID
			case "reassigned":
				api.viewerID = projectID
				api.issue["assignee"] = map[string]any{"id": projectID}
			case "unassigned":
				api.issue["assignee"] = nil
			}
			before := len(api.calls)
			api.mu.Unlock()
			err := executeTest(r, "reconcile", "--issue", issueID)
			allowed := scenario == "same-owner" || scenario == "reassigned"
			if (err == nil) != allowed {
				t.Fatal(scenario, err)
			}
			if !allowed {
				if exists(filepath.Join(root, ".runs", issueID, "reconcile-comment.txt")) {
					t.Fatal("unauthorized recovery wrote a checkpoint")
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				for _, call := range api.calls[before:] {
					if strings.Contains(call.Query, "mutation") {
						t.Fatal("unauthorized recovery changed Linear")
					}
				}
			}
			if scenario == "reassigned" {
				original, err := os.ReadFile(filepath.Join(root, ".runs", issueID, "context.json"))
				if err != nil || !strings.Contains(string(original), userID) {
					t.Fatal("original launch context lost", err)
				}
			}
		})
	}
}

func TestOwnershipChangeDuringAvailabilityPreventsClaim(t *testing.T) {
	root, r, _, api := gapRunner(t, "owner-after-preflight")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, "preflight-calls")) {
		t.Fatal("availability was not checked")
	}
	if exists(filepath.Join(root, ".runs", issueID)) {
		t.Fatal("changed ownership was ignored after availability")
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "mutation") {
			t.Fatal("ownership change was overwritten")
		}
	}
}

func TestSavedSnapshotCannotOverrideCurrentOwner(t *testing.T) {
	root, r, _, api := gapRunner(t, "ready")
	api.issue["assignee"] = map[string]any{"id": projectID}
	path := filepath.Join(root, "snapshot.json")
	if err := writeJSON(path, testIssue()); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "run", "--project", projectID, "--issue-file", path); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(root, "preflight-calls")) || exists(filepath.Join(root, ".runs", issueID)) {
		t.Fatal("stale saved snapshot bypassed current ownership")
	}
}

package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoCodeCompletionWorkflow(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root, r, c, api := gapRunner(t, "no-code")
			c.Engine = engine
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			api.issue["description"] = `Do not modify any files. Comment "This worked again again!" on this issue. Then the issue is considered complete.`
			previous := r.command
			r.command = func(dir string, args ...string) (string, error) {
				if len(args) > 1 && args[0] == "gh" && (args[1] == "pr" || args[1] == "api") {
					return "", errors.New("no-code completion must not use PR or CI gates")
				}
				return previous(dir, args...)
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
				t.Fatal(status, err)
			}
			api.mu.Lock()
			state := api.liveState
			api.mu.Unlock()
			if state != reviewID {
				t.Fatal("no-code run did not reach review")
			}
			folder := filepath.Join(root, ".runs", issueID)
			report, err := readReport(folder)
			if err != nil {
				t.Fatal(err)
			}
			if textAt(report, "pr_url") != "" {
				t.Fatal("manufactured PR")
			}
			if out := mustCommand(t, filepath.Join(folder, "repo"), "git", "status", "--porcelain"); out != "" {
				t.Fatal(out)
			}
			var saved runContext
			if err = readJSON(filepath.Join(folder, "context.json"), &saved); err != nil {
				t.Fatal(err)
			}
			if textAt(report, "commit_sha") != saved.BaseSHA {
				t.Fatal("no-code run created a commit")
			}
			comment, err := os.ReadFile(filepath.Join(folder, "finish-comment.txt"))
			if err != nil || !strings.Contains(string(comment), "No code changes; PR not required.") {
				t.Fatal(string(comment), err)
			}
			if err = executeTest(r, "reconcile", "--issue", issueID); err != nil {
				t.Fatal("no-code reconciliation failed", err)
			}
		})
	}
}

func TestPRCompletionStillRequiresPR(t *testing.T) {
	root, r, _, api := gapRunner(t, "no-code-pr-required")
	if err := executeTest(r, "poll"); err == nil {
		t.Fatal("default code issue accepted without PR")
	}
	api.mu.Lock()
	state := api.liveState
	api.mu.Unlock()
	if state != progressID {
		t.Fatal("unapproved no-code completion changed status")
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if status, err := claimedStatus(db, issueID); err != nil || status != "failed" {
		t.Fatal(status, err)
	}
}

func TestNoCodeReportAndCheckoutGates(t *testing.T) {
	for _, scenario := range []string{"valid", "default", "dirty", "staged", "untracked", "commit", "empty-commit", "branch", "origin", "pr", "sha", "criteria", "check", "blocker", "security", "qa", "deployment", "missing-requirement"} {
		t.Run(scenario, func(t *testing.T) {
			root := t.TempDir()
			repo := testRepo(t, root)
			r := newRunner(filepath.Join(root, "agent-linear"))
			mustCommand(t, repo, "git", "restore", "app.txt")
			base := mustCommand(t, repo, "git", "rev-parse", "HEAD")
			profile, err := r.gitProfile(repo)
			if err != nil {
				t.Fatal(err)
			}
			rt := testConfig(repo).Projects[projectID]
			issue := testIssue()
			issue["description"] = "Do not modify product files. Reject empty input"
			if scenario == "default" {
				issue = testIssue()
			}
			requirements := issueRequirements(issue)
			report := goodReport()
			report["pr_url"] = ""
			report["completion"] = "no-code"
			if scenario == "default" {
				report["completion"] = "pr"
			}
			report["commit_sha"] = base
			criteria := []any{}
			for _, item := range requirements {
				criteria = append(criteria, map[string]any{"id": item.ID, "requirement": item.Requirement, "status": "passed", "evidence": "verified observation"})
			}
			report["criteria"] = criteria
			switch scenario {
			case "dirty", "staged", "commit":
				mustWrite(t, filepath.Join(repo, "app.txt"), "changed")
				if scenario != "dirty" {
					mustCommand(t, repo, "git", "add", "app.txt")
				}
				if scenario == "commit" {
					mustCommand(t, repo, "git", "commit", "-S", "-m", "Change")
					report["commit_sha"] = mustCommand(t, repo, "git", "rev-parse", "HEAD")
				}
			case "untracked":
				mustWrite(t, filepath.Join(repo, "extra.txt"), "changed")
			case "empty-commit":
				mustCommand(t, repo, "git", "commit", "-S", "--allow-empty", "-m", "Empty")
				report["commit_sha"] = mustCommand(t, repo, "git", "rev-parse", "HEAD")
			case "branch":
				mustCommand(t, repo, "git", "checkout", "-b", "other")
			case "origin":
				mustCommand(t, repo, "git", "remote", "set-url", "origin", "https://github.com/other/repo.git")
			case "pr":
				report["pr_url"] = prURL
			case "sha":
				report["commit_sha"] = "abc"
			case "criteria":
				criteria[0].(map[string]any)["evidence"] = ""
			case "check":
				report["checks"].([]any)[0].(map[string]any)["passed"] = false
			case "blocker":
				report["blockers"] = []any{"not verified"}
			case "security":
				report["security"] = "CONDITIONAL"
			case "qa":
				report["qa"] = "unverified"
			case "deployment":
				report["deployment"].(map[string]any)["status"] = "verified"
			case "missing-requirement":
				report["criteria"] = criteria[:1]
			}
			var schema map[string]any
			if err = json.Unmarshal([]byte(asset("result.schema.json")), &schema); err != nil {
				t.Fatal(err)
			}
			if err = validateReport(report, schema, "result"); err == nil {
				err = r.verifyPR(report, rt, repo, "staging", base, "gh", profile, requirements)
			}
			if (err == nil) != (scenario == "valid") {
				t.Fatal(scenario, err)
			}
		})
	}
}

func TestNoCodeCompletionPreservesHumanChanges(t *testing.T) {
	for _, scenario := range []string{"edited", "reassigned", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			root, r, _, api := gapRunner(t, "no-code")
			api.issue["description"] = "Do not modify product files. Reject empty input"
			api.outcome = scenario
			if err := executeTest(r, "poll"); err == nil {
				t.Fatal("changed scope/owner/status was overwritten")
			}
			if exists(filepath.Join(root, ".runs", issueID, "finish-state-request.json")) {
				t.Fatal("unsafe final mutation was attempted")
			}
		})
	}
}

func TestNoCodeManagedResume(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root, r, c, api := gapRunner(t, "no-code-blocked")
			c.Engine = engine
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			api.issue["description"] = "Do not modify product files. Reject empty input"
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			mustWrite(t, filepath.Join(root, "outcome"), "no-code")
			if err := executeTest(r, "resume", "--issue", issueID); err != nil {
				t.Fatal(err)
			}
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
				t.Fatal(status, err)
			}
			api.mu.Lock()
			state := api.liveState
			api.mu.Unlock()
			if state != reviewID {
				t.Fatal("no-code resume did not complete")
			}
		})
	}
}

func TestCompletionCannotReplaceFrozenRequirements(t *testing.T) {
	root, r, _, api := gapRunner(t, "blocked")
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	api.issue["description"] = "Do not modify product files. Reject empty input"
	mustWrite(t, filepath.Join(root, "outcome"), "no-code")
	if err := executeTest(r, "resume", "--issue", issueID); err == nil {
		t.Fatal("late opt-in replaced original task policy")
	}
	if exists(filepath.Join(root, "resume-calls.json")) {
		t.Fatal("new scope was allowed to resume")
	}
}

func TestLegacyReportCompletionKeepsOutcomeAndBlockers(t *testing.T) {
	for _, kind := range []string{"pr", "no-code", "blocked"} {
		t.Run(kind, func(t *testing.T) {
			folder := t.TempDir()
			report := goodReport()
			delete(report, "completion")
			expected := "pr"
			if kind != "pr" {
				report["pr_url"] = ""
				expected = "no-code"
			}
			if kind == "blocked" {
				report["status"] = "blocked"
				report["blockers"] = []any{"previous workflow required a header"}
			}
			if err := writeJSON(filepath.Join(folder, "result.json"), report); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(folder, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			loaded, err := readReport(folder)
			if err != nil {
				t.Fatal(err)
			}
			if textAt(loaded, "completion") != expected || textAt(loaded, "status") != textAt(report, "status") {
				t.Fatal("legacy outcome changed", loaded)
			}
			if kind == "blocked" && len(loaded["blockers"].([]any)) != 1 {
				t.Fatal("legacy blocker silently cleared")
			}
			after, err := os.ReadFile(filepath.Join(folder, "result.json"))
			if err != nil || string(before) != string(after) {
				t.Fatal("reading legacy report overwrote evidence", err)
			}
		})
	}
}

func TestResumeRefreshesRunnerPolicyWithoutReplacingOriginalTask(t *testing.T) {
	root, r, _, api := gapRunner(t, "no-code-blocked")
	api.issue["description"] = "Do not modify any files. Reject empty input"
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(root, ".runs", issueID)
	original, err := os.ReadFile(filepath.Join(folder, "context.json"))
	if err != nil {
		t.Fatal(err)
	}
	previous := "Old runner policy: blocked unless Completion: no-code is present."
	mustWrite(t, filepath.Join(folder, "AGENT.md"), previous)
	mustWrite(t, filepath.Join(root, "outcome"), "no-code")
	if err = executeTest(r, "resume", "--issue", issueID); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(folder, "AGENT.md"))
	if err != nil || string(current) != asset("AGENT.md") {
		t.Fatal("resume retained obsolete runner policy", err)
	}
	archives, err := filepath.Glob(filepath.Join(folder, "resumes", "attempt-*", "AGENT.md"))
	if err != nil || len(archives) != 1 {
		t.Fatal("old runner policy not archived", archives, err)
	}
	old, err := os.ReadFile(archives[0])
	if err != nil || string(old) != previous {
		t.Fatal("old instructions lost", err)
	}
	after, err := os.ReadFile(filepath.Join(folder, "context.json"))
	if err != nil || string(after) != string(original) {
		t.Fatal("frozen original task changed", err)
	}
}

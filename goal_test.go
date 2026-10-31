package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func goalTestPlan() map[string]any {
	return map[string]any{
		"findings": []any{map[string]any{"severity": "high", "summary": "N+1 queries", "evidence": "app.txt"}},
		"issues": []any{
			map[string]any{"title": "Fix N+1 queries", "description": "Load related records in bulk.\n\nAcceptance: no per-row queries.", "priority": "high", "labels": []any{}},
		},
		"follow_up_questions": []any{},
	}
}

func goalAgentFixture(root, engine string) int {
	fail := func(v any) int { fmt.Fprintln(os.Stderr, v); return 1 }
	repo, err := os.Getwd()
	if err != nil {
		return fail(err)
	}
	folder := filepath.Dir(repo)
	if os.Getenv("LINEAR_API_KEY") != "" {
		return fail("goal agent received Linear key")
	}
	if os.Getenv("GIT_AUTHOR_NAME") != "" {
		return fail("goal agent received git identity")
	}
	if err = writeJSON(filepath.Join(folder, "goal-args.json"), os.Args); err != nil {
		return fail(err)
	}
	if engine == "codex" && !slices.Contains(os.Args, "read-only") {
		return fail("missing read-only sandbox")
	}
	outcomeBytes, err := os.ReadFile(filepath.Join(root, "outcome"))
	if err != nil {
		return fail(err)
	}
	plan := goalTestPlan()
	var data []byte
	switch string(outcomeBytes) {
	case "malformed":
		data = []byte("not JSON")
	case "empty-plan":
		plan["issues"] = []any{}
		data, _ = json.Marshal(plan)
	case "invalid-plan":
		delete(plan, "issues")
		data, _ = json.Marshal(plan)
	default:
		data, _ = json.Marshal(plan)
	}
	if engine == "codex" {
		index := slices.Index(os.Args, "--output-last-message")
		if index < 0 {
			return fail("no report path")
		}
		if err = writeFile(os.Args[index+1], data); err != nil {
			return fail(err)
		}
	} else {
		emit := func(kind string, part map[string]any) {
			event := map[string]any{"type": kind, "sessionID": "ses_goal123"}
			if part != nil {
				event["part"] = part
			}
			if err := json.NewEncoder(os.Stdout).Encode(event); err != nil {
				panic(err)
			}
		}
		emit("step_start", nil)
		emit("text", map[string]any{"id": "part_goal", "text": string(data)})
		emit("step_finish", map[string]any{"reason": "stop"})
	}
	return 0
}

func writeGoalConfig(t *testing.T, root, source string, engine string) {
	t.Helper()
	c := testConfig(source)
	c.Engine = engine
	model := "test/model"
	c.Model = &model
	if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
		t.Fatal(err)
	}
}

func countIssueCreates(calls []apiCall) int {
	count := 0
	for _, call := range calls {
		if strings.Contains(call.Query, "issueCreate(") {
			count++
		}
	}
	return count
}

func TestGoalDryRunReadOnly(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			fixtureBinaries(t, root)
			api := &linearFixture{issue: testIssue()}
			r := newFixtureRunner(t, root, source, api)
			writeGoalConfig(t, root, source, engine)
			mustWrite(t, filepath.Join(root, "outcome"), "ready")
			var out bytes.Buffer
			r.out = &out
			if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries", "--dry-run"); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			for _, call := range api.calls {
				if strings.Contains(call.Query, "mutation") || strings.Contains(call.Query, "issueCreate(") {
					api.mu.Unlock()
					t.Fatal("goal dry run mutated Linear")
				}
			}
			api.mu.Unlock()
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err = db.QueryRow("SELECT COUNT(*) FROM goals").Scan(&count); err != nil || count != 0 {
				t.Fatal("dry run recorded a goal", count, err)
			}
			if !strings.Contains(out.String(), "Dry run") || !strings.Contains(out.String(), "Fix N+1 queries") {
				t.Fatal("dry run did not print the plan", out.String())
			}
		})
	}
}

func TestGoalCreatesParentAndChildren(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			fixtureBinaries(t, root)
			api := &linearFixture{issue: testIssue()}
			r := newFixtureRunner(t, root, source, api)
			writeGoalConfig(t, root, source, engine)
			mustWrite(t, filepath.Join(root, "outcome"), "ready")
			var out bytes.Buffer
			r.out = &out
			if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries"); err != nil {
				t.Fatal(err)
			}
			api.mu.Lock()
			var creates []apiCall
			var commented bool
			for _, call := range api.calls {
				if strings.Contains(call.Query, "issueCreate(") {
					creates = append(creates, call)
				}
				if strings.Contains(call.Query, "commentCreate(") && strings.Contains(textAt(call.Variables, "input", "body"), "Investigated goal") {
					commented = true
				}
			}
			api.mu.Unlock()
			if len(creates) != 2 {
				t.Fatalf("expected 1 parent + 1 child, got %d", len(creates))
			}
			parent, child := creates[0], creates[1]
			if textAt(parent.Variables, "input", "parentId") != "" || textAt(parent.Variables, "input", "stateId") != triageID {
				t.Fatal("parent must be an unparented triage issue", parent.Variables)
			}
			if textAt(child.Variables, "input", "parentId") != "aaaaaaaa-aaaa-4aaa-8aaa-000000000001" || textAt(child.Variables, "input", "stateId") != triageID {
				t.Fatal("child must reference the parent and land in triage", child.Variables)
			}
			if !commented {
				t.Fatal("goal summary comment missing")
			}
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err = db.QueryRow("SELECT COUNT(*) FROM goals").Scan(&count); err != nil || count != 1 {
				t.Fatal("goal not recorded", count, err)
			}
		})
	}
}

func TestGoalAutostart(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{issue: testIssue()}
	r := newFixtureRunner(t, root, source, api)
	writeGoalConfig(t, root, source, "codex")
	mustWrite(t, filepath.Join(root, "outcome"), "ready")
	if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries", "--autostart"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	var child *apiCall
	for i := range api.calls {
		if strings.Contains(api.calls[i].Query, "issueCreate(") && textAt(api.calls[i].Variables, "input", "parentId") != "" {
			child = &api.calls[i]
		}
	}
	if child == nil {
		t.Fatal("no child issue created")
	}
	if textAt(child.Variables, "input", "stateId") != readyID {
		t.Fatal("autostart child must land in ready", child.Variables)
	}
	labels, _ := child.Variables["input"].(map[string]any)["labelIds"].([]any)
	if len(labels) != 1 || labels[0] != "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb" {
		t.Fatal("autostart child missing agent-ready label", labels)
	}
}

func TestGoalDuplicateSuppression(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{issue: testIssue()}
	r := newFixtureRunner(t, root, source, api)
	writeGoalConfig(t, root, source, "codex")
	mustWrite(t, filepath.Join(root, "outcome"), "ready")
	if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	first := countIssueCreates(api.calls)
	api.mu.Unlock()
	if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries"); err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	second := countIssueCreates(api.calls)
	api.mu.Unlock()
	if second != first {
		t.Fatalf("duplicate goal created issues: %d -> %d", first, second)
	}
}

func TestGoalPlanValidation(t *testing.T) {
	for _, outcome := range []string{"malformed", "empty-plan", "invalid-plan"} {
		t.Run(outcome, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			fixtureBinaries(t, root)
			api := &linearFixture{issue: testIssue()}
			r := newFixtureRunner(t, root, source, api)
			writeGoalConfig(t, root, source, "codex")
			mustWrite(t, filepath.Join(root, "outcome"), outcome)
			if err := executeTest(r, "goal", "--project", projectID, "--prompt", "Diagnose N+1 queries"); err == nil {
				t.Fatal("invalid plan accepted", outcome)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			for _, call := range api.calls {
				if strings.Contains(call.Query, "issueCreate(") {
					t.Fatal("invalid plan created issues", outcome)
				}
			}
		})
	}
}

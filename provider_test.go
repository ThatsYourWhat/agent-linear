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
	"time"
)

func availabilityFixture(root, engine string) int {
	if os.Getenv("LINEAR_API_KEY") != "" {
		return 1
	}
	if err := writeJSON(filepath.Join(root, "preflight-args.json"), os.Args); err != nil {
		return 1
	}
	data, _ := os.ReadFile(filepath.Join(root, "outcome"))
	outcome := string(data)
	log, err := os.OpenFile(filepath.Join(root, "preflight-calls"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return 1
	}
	if _, err = log.WriteString(engine + "\n"); err != nil {
		log.Close()
		return 1
	}
	if err = log.Close(); err != nil {
		return 1
	}
	if engine == "codex" {
		if !slices.Contains(os.Args, "read-only") || !slices.Contains(os.Args, "--ephemeral") || !slices.Contains(os.Args, "features.shell_tool=false") {
			return 1
		}
	} else {
		var cfg map[string]any
		if json.Unmarshal([]byte(os.Getenv("OPENCODE_CONFIG_CONTENT")), &cfg) != nil || textAt(cfg, "agent", "linear-issue", "permission") != "deny" {
			return 1
		}
	}
	if outcome == "limit-before" {
		if engine == "codex" {
			fmt.Println(`{"type":"error","message":"You've hit your usage limit. Try again later."}`)
		} else {
			fmt.Println(`{"type":"error","error":{"name":"APIError","data":{"statusCode":429,"message":"insufficient_quota"}}}`)
		}
		time.Sleep(10 * time.Second)
		return 1
	}
	if outcome == "limit-stderr" {
		fmt.Fprint(os.Stderr, "Error: rate limit exceeded")
		time.Sleep(10 * time.Second)
		return 1
	}
	if outcome == "preflight-timeout" {
		time.Sleep(10 * time.Second)
		return 1
	}
	if outcome == "preflight-auth" {
		fmt.Fprintln(os.Stderr, "Authentication failed")
		return 1
	}
	ready := "READY"
	if outcome == "preflight-invalid" {
		ready = "Cannot confirm"
	}
	if engine == "codex" {
		index := slices.Index(os.Args, "--output-last-message")
		if index < 0 {
			return 1
		}
		if err = writeFile(os.Args[index+1], []byte(ready)); err != nil {
			return 1
		}
	} else {
		fmt.Println(`{"type":"step_start","sessionID":"ses_probe"}`)
		if err = json.NewEncoder(os.Stdout).Encode(map[string]any{"type": "text", "part": map[string]any{"id": "ready", "text": ready}}); err != nil {
			return 1
		}
		fmt.Println(`{"type":"step_finish","part":{"reason":"stop"}}`)
	}
	return 0
}
func TestLimitsDeferThenRecoverOrPreserveWork(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		for _, outcome := range []string{"limit-before", "limit-stderr", "limit-during"} {
			t.Run(engine+"/"+outcome, func(t *testing.T) {
				root := t.TempDir()
				source := testRepo(t, root)
				fixtureBinaries(t, root)
				mustWrite(t, filepath.Join(root, "outcome"), outcome)
				api := &linearFixture{issue: testIssue(), viewerName: "Test User"}
				r := newFixtureRunner(t, root, source, api)
				var output bytes.Buffer
				r.out = &output
				c := testConfig(source)
				c.Engine = engine
				model := "test/model"
				c.Model = &model
				if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				if err := executeTest(r, "poll"); err != nil {
					t.Fatal(err)
				}
				if time.Since(started) > 5*time.Second {
					t.Fatal("provider retry process was not stopped promptly")
				}
				runs := filepath.Join(root, ".runs")
				db, err := openState(filepath.Join(runs, "state.sqlite3"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				expected := "deferred"
				if outcome == "limit-during" {
					expected = "blocked"
				}
				status, err := claimedStatus(db, issueID)
				if err != nil || status != expected {
					t.Fatal(status, err)
				}
				limit, err := activeCooldown(db, c)
				if err != nil || limit == nil || time.Until(limit.RetryAt) < 14*time.Minute {
					t.Fatal("missing persistent cooldown", limit, err)
				}
				api.mu.Lock()
				calls := append([]apiCall{}, api.calls...)
				api.mu.Unlock()
				mutations := 0
				for _, call := range calls {
					if strings.Contains(call.Query, "mutation") {
						mutations++
					}
					if textAt(call.Variables, "input", "stateId") == reviewID {
						t.Fatal("limited issue moved to review")
					}
				}
				folder := filepath.Join(runs, issueID)
				if expected == "deferred" {
					if mutations != 0 || exists(folder) {
						t.Fatal("untouched issue was mutated/claimed")
					}
				} else {
					if mutations != 3 {
						t.Fatalf("expected start comment/status and blocked comment; got %d", mutations)
					}
					if !exists(filepath.Join(folder, "provider-limit.json")) || !exists(filepath.Join(folder, "failure.txt")) {
						t.Fatal("missing recovery evidence")
					}
					head := mustCommand(t, filepath.Join(folder, "repo"), "git", "log", "-1", "--format=%s")
					if head != "Handle input" {
						t.Fatal("partial commit lost")
					}
					comment, err := os.ReadFile(filepath.Join(folder, "finish-comment.txt"))
					if err != nil || !strings.Contains(string(comment), "partial work preserved") || !strings.Contains(string(comment), "automatically resume") {
						t.Fatal(string(comment), err)
					}
					if strings.Contains(string(comment), "@Test User ") {
						t.Fatal("provider-limit block must not tag the assignee", string(comment))
					}
				}
				// A fresh runner sees the SQLite cooldown before polling or consuming another issue.
				restart := newFixtureRunner(t, root, source, api)
				if err = executeTest(restart, "poll"); err != nil {
					t.Fatal(err)
				}
				api.mu.Lock()
				count := len(api.calls)
				api.mu.Unlock()
				if count != len(calls) {
					t.Fatal("cooldown still queried/picked issues")
				}
				probeCalls, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
				if err != nil || strings.Count(string(probeCalls), "\n") != 1 {
					t.Fatal("repeated availability request", err)
				}
				if _, err = db.Exec("UPDATE provider_cooldowns SET retry_at=0"); err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("UPDATE provider_recoveries SET retry_at=0"); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(root, "outcome"), "ready")
				if err = executeTest(r, "poll"); err != nil {
					t.Fatal(err)
				}
				status, err = claimedStatus(db, issueID)
				if err != nil {
					t.Fatal(err)
				}
				if expected == "deferred" {
					if status != "ready" {
						t.Fatal("deferred issue did not recover", status)
					}
					if !exists(filepath.Join(runs, issueID, "result.json")) {
						t.Fatal("missing completed report")
					}
				} else {
					if status != "ready" || !exists(filepath.Join(root, "resume-calls.json")) {
						t.Fatal("partial work was not resumed", status)
					}
					probeCalls, err = os.ReadFile(filepath.Join(root, "preflight-calls"))
					if err != nil || strings.Count(string(probeCalls), "\n") != 2 {
						t.Fatal("resume did not check availability", err)
					}
				}
			})
		}
	}
}
func TestPreflightFailuresNeverChangeIssue(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		for _, outcome := range []string{"preflight-auth", "preflight-invalid", "preflight-timeout"} {
			t.Run(engine+"/"+outcome, func(t *testing.T) {
				root := t.TempDir()
				source := testRepo(t, root)
				fixtureBinaries(t, root)
				mustWrite(t, filepath.Join(root, "outcome"), outcome)
				api := &linearFixture{issue: testIssue()}
				r := newFixtureRunner(t, root, source, api)
				r.preflightTimeout = 150 * time.Millisecond
				c := testConfig(source)
				c.Engine = engine
				if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
					t.Fatal(err)
				}
				if err := executeTest(r, "poll"); err == nil {
					t.Fatal("unavailable provider accepted")
				}
				db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				status, err := claimedStatus(db, issueID)
				if err != nil || status != "" {
					t.Fatal("unstarted issue claimed", status, err)
				}
				api.mu.Lock()
				defer api.mu.Unlock()
				for _, call := range api.calls {
					if strings.Contains(call.Query, "mutation") {
						t.Fatal("preflight failure changed Linear")
					}
				}
			})
		}
	}
}
func TestLimitClassification(t *testing.T) {
	for _, sample := range []struct {
		line string
		want bool
	}{
		{`{"type":"error","message":"You've hit your usage limit"}`, true},
		{`{"type":"turn.failed","error":{"message":"You exceeded your current quota"}}`, true},
		{`{"type":"error","error":{"data":{"statusCode":429}}}`, true},
		{`{"type":"session.status","status":{"type":"retry","message":"rate limit exceeded"}}`, true},
		{`{"type":"text","part":{"text":"rate limit exceeded"}}`, false},
		{`{"type":"tool_use","part":{"output":"Error: status 429"}}`, false},
		{`{"type":"error","message":"context_length_exceeded"}`, false},
		{`{"type":"error","message":"Authentication failed"}`, false},
		{`{"type":"error","message":"failed to read file 429.txt"}`, false},
	} {
		if got := limitEvent([]byte(sample.line)); got != sample.want {
			t.Fatalf("%s: %v", sample.line, got)
		}
	}
	var output bytes.Buffer
	signals := 0
	stream := &limitStream{file: &output, signal: func() { signals++ }}
	first := []byte(`{"type":"error","message":"rate_`)
	second := []byte("limit_exceeded\"}\n")
	if _, err := stream.Write(first); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write(second); err != nil {
		t.Fatal(err)
	}
	stream.finish()
	if signals != 1 || output.String() != string(first)+string(second) {
		t.Fatal("split provider event was lost")
	}
}

func TestLimitStopsRestOfQueueAndDryRunDoesNotProbe(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root := t.TempDir()
			source := testRepo(t, root)
			fixtureBinaries(t, root)
			mustWrite(t, filepath.Join(root, "outcome"), "limit-before")
			second := testIssue()
			second["id"] = userID
			second["identifier"] = "APP-2"
			api := &linearFixture{issue: testIssue(), moreIssues: []map[string]any{second}}
			r := newFixtureRunner(t, root, source, api)
			c := testConfig(source)
			c.Engine = engine
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if err := executeTest(r, "poll", "--dry-run"); err != nil {
				t.Fatal(err)
			}
			if exists(filepath.Join(root, "preflight-calls")) {
				t.Fatal("dry-run consumed a model request")
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			status, err := claimedStatus(db, userID)
			if err != nil || status != "" {
				t.Fatal("next queued issue consumed", status, err)
			}
			other := testConfig(source)
			other.Engine = engine
			other.Projects = map[string]*route{reviewID: other.Projects[projectID]}
			limit, err := activeCooldown(db, other)
			if err != nil || limit == nil {
				t.Fatal("cooldown not shared across projects", err)
			}
			other.Engine = "codex"
			if engine == "codex" {
				other.Engine = "opencode"
			}
			if limit, err = activeCooldown(db, other); err != nil || limit != nil {
				t.Fatal("unrelated engine paused", err)
			}
		})
	}
}

func TestWithdrawnIssueAfterAvailabilityIsNotStarted(t *testing.T) {
	root := t.TempDir()
	source := testRepo(t, root)
	fixtureBinaries(t, root)
	api := &linearFixture{issue: testIssue(), outcome: "withdrawn-after-preflight"}
	r := newFixtureRunner(t, root, source, api)
	if err := writeJSON(filepath.Join(root, "config.json"), testConfig(source)); err != nil {
		t.Fatal(err)
	}
	if err := executeTest(r, "poll"); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(root, "preflight-calls")) {
		t.Fatal("availability was not checked")
	}
	db, err := openState(filepath.Join(root, ".runs", "state.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	status, err := claimedStatus(db, issueID)
	if err != nil || status != "" {
		t.Fatal("withdrawn issue claimed", status, err)
	}
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, call := range api.calls {
		if strings.Contains(call.Query, "mutation") {
			t.Fatal("withdrawn issue changed")
		}
	}
}

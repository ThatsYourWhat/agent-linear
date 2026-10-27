package main

import (
	"bytes"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func providerRecoveryFixture(t *testing.T, engine string) (string, *runner, *config, *linearFixture, *sql.DB) {
	t.Helper()
	root, r, c, api := gapRunner(t, "limit-during")
	c.Engine = engine
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
	t.Cleanup(func() { db.Close() })
	return root, r, c, api, db
}

func expireProviderRecovery(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, query := range []string{"UPDATE provider_cooldowns SET retry_at=0", "UPDATE provider_recoveries SET retry_at=0"} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAutomaticProviderRecoveryRepeatedLimits(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		t.Run(engine, func(t *testing.T) {
			root, r, c, api, db := providerRecoveryFixture(t, engine)
			folder := filepath.Join(root, ".runs", issueID)
			before := mustCommand(t, filepath.Join(folder, "repo"), "git", "rev-parse", "HEAD")
			originalContext, err := os.ReadFile(filepath.Join(folder, "context.json"))
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			r.out = &output
			if err = executeTest(r, "status"); err != nil || !strings.Contains(output.String(), `"provider_recovery":"waiting"`) || !strings.Contains(output.String(), `"next_probe_at"`) {
				t.Fatal(output.String(), err)
			}
			// A different default provider and its cooldown must not override the saved provider.
			c.Engine = "codex"
			if engine == "codex" {
				c.Engine = "opencode"
			}
			c.Model = nil
			if err = writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			for _, outcome := range []string{"limit-before", "limit-during", "ready"} {
				expireProviderRecovery(t, db)
				if err = saveCooldown(db, c, &providerLimit{RetryAt: time.Now().Add(time.Hour)}); err != nil {
					t.Fatal(err)
				}
				mustWrite(t, filepath.Join(root, "outcome"), outcome)
				// Simulate a new cron process at each attempt.
				restart := newFixtureRunner(t, root, c.Projects[projectID].RepoPath, api)
				if err = executeTest(restart, "poll"); err != nil {
					t.Fatal(err)
				}
				var args []string
				if err = readJSON(filepath.Join(root, "preflight-args.json"), &args); err != nil {
					t.Fatal(err)
				}
				index := slices.Index(args, "--model")
				if index < 0 || index+1 >= len(args) || args[index+1] != "test/model" {
					t.Fatal("probe changed model", args)
				}
				if outcome != "ready" {
					var state string
					var retryAt int64
					if err = db.QueryRow("SELECT state,retry_at FROM provider_recoveries WHERE issue_id=?", issueID).Scan(&state, &retryAt); err != nil || state != "waiting" || retryAt < time.Now().Add(14*time.Minute).Unix() {
						t.Fatal("limit did not reschedule", state, retryAt, err)
					}
					if err = executeTest(restart, "poll"); err != nil {
						t.Fatal(err)
					}
				}
			}
			if status, err := claimedStatus(db, issueID); err != nil || status != "ready" {
				t.Fatal(status, err)
			}
			var count int
			if err = db.QueryRow("SELECT COUNT(*) FROM provider_recoveries").Scan(&count); err != nil || count != 0 {
				t.Fatal("completed recovery retained eligibility", count, err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
			if err != nil || string(calls) != strings.Repeat(engine+"\n", 4) {
				t.Fatal("wrong provider or duplicate probes", string(calls), err)
			}
			archives, err := filepath.Glob(filepath.Join(folder, "resumes", "attempt-*"))
			if err != nil || len(archives) != 2 {
				t.Fatal("wrong continuation count", archives, err)
			}
			afterContext, err := os.ReadFile(filepath.Join(folder, "context.json"))
			if err != nil || !bytes.Equal(originalContext, afterContext) || mustCommand(t, filepath.Join(folder, "repo"), "git", "rev-parse", "HEAD") != before {
				t.Fatal("partial work or original context changed", err)
			}
		})
	}
}

func TestAutomaticProviderRecoveryStopsAtUnsafeBoundaries(t *testing.T) {
	for _, engine := range []string{"codex", "opencode"} {
		for _, scenario := range []string{"owner", "scope", "state", "route", "branch", "profile", "missing-session", "finish-request", "active-checkout", "preflight-auth", "preflight-invalid", "preflight-timeout", "process-error", "ci-pending", "ambiguous-crash", "legacy", "worker", "dry-run"} {
			t.Run(engine+"/"+scenario, func(t *testing.T) {
				root, r, c, api, db := providerRecoveryFixture(t, engine)
				folder := filepath.Join(root, ".runs", issueID)
				expireProviderRecovery(t, db)
				mustWrite(t, filepath.Join(root, "outcome"), "ready")
				switch scenario {
				case "owner":
					api.issue["assignee"] = map[string]any{"id": projectID}
				case "scope":
					api.issue["description"] = "Changed requirements"
				case "state":
					api.liveState = reviewID
				case "route":
					c.Projects[projectID].Label = "changed"
					if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
						t.Fatal(err)
					}
				case "branch":
					mustCommand(t, filepath.Join(folder, "repo"), "git", "checkout", "-b", "other")
				case "profile":
					mustCommand(t, filepath.Join(folder, "repo"), "git", "config", "user.email", "other@example.com")
				case "missing-session":
					mustWrite(t, filepath.Join(folder, "events.jsonl"), "partial")
				case "finish-request":
					mustWrite(t, filepath.Join(folder, "finish-state-request.json"), "{}")
				case "active-checkout":
					cmd := exec.Command("sleep", "30")
					cmd.Dir = filepath.Join(folder, "repo")
					if err := cmd.Start(); err != nil {
						t.Fatal(err)
					}
					defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
				case "preflight-auth", "preflight-invalid", "preflight-timeout", "process-error", "ci-pending":
					mustWrite(t, filepath.Join(root, "outcome"), scenario)
					r.preflightTimeout = time.Second
				case "ambiguous-crash":
					if err := stopProviderRecovery(db, issueID, "Interrupted automatic attempt; inspect manually"); err != nil {
						t.Fatal(err)
					}
				case "legacy":
					if _, err := db.Exec("DELETE FROM provider_recoveries"); err != nil {
						t.Fatal(err)
					}
				case "worker":
					f, err := os.OpenFile(filepath.Join(root, ".runs", "worker.lock"), os.O_WRONLY, 0600)
					if err != nil {
						t.Fatal(err)
					}
					defer f.Close()
					if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
						t.Fatal(err)
					}
				}
				args := []string{"poll"}
				if scenario == "dry-run" {
					args = append(args, "--dry-run")
				}
				err := executeTest(r, args...)
				skipped := slices.Contains([]string{"ambiguous-crash", "legacy", "worker", "dry-run"}, scenario)
				if skipped && err != nil || !skipped && err == nil {
					t.Fatal("unexpected recovery result", err)
				}
				launched := scenario == "process-error" || scenario == "ci-pending"
				if exists(filepath.Join(root, "resume-calls.json")) != launched {
					t.Fatal("wrong continuation behavior")
				}
				if scenario == "ci-pending" && !exists(filepath.Join(folder, "result.json")) {
					t.Fatal("lost report required for reconciliation")
				}
				if !skipped {
					var output bytes.Buffer
					r.out = &output
					if err = executeTest(r, "status"); err != nil || !strings.Contains(output.String(), `"provider_recovery":"manual"`) {
						t.Fatal("manual recovery reason hidden", output.String(), err)
					}
				}
				before, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
				if err != nil {
					t.Fatal(err)
				}
				if err = executeTest(r, args...); err != nil {
					t.Fatal("subsequent tick retried failed recovery", err)
				}
				after, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("repeated recovery probe", err)
				}
			})
		}
	}
}

func TestProviderRecoveryMigrationPreservesManualRuns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite3")
	db, err := openState(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"DROP TABLE provider_recoveries", "PRAGMA user_version=1", "INSERT INTO runs VALUES ('legacy','APP-1','blocked','/old/checkout','')"} {
		if _, err = db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err = printStatus(db, &output, true); err != nil {
		t.Fatal("legacy read-only status failed", err)
	}
	db.Close()
	db, err = openState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count, version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 2 {
		t.Fatal(version, err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM provider_recoveries").Scan(&count); err != nil || count != 0 {
		t.Fatal("migration queued old work", count, err)
	}
	if status, err := claimedStatus(db, "legacy"); err != nil || status != "blocked" {
		t.Fatal("migration changed existing claim", status, err)
	}
}

func TestManualRecoveryCancelsScheduledContinuation(t *testing.T) {
	for _, action := range []string{"resume", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			root, r, _, api, db := providerRecoveryFixture(t, "codex")
			expireProviderRecovery(t, db)
			if action == "resume" {
				api.issue["description"] = "Changed scope"
			}
			// Neither changed scope nor a missing report authorizes continuation.
			if err := executeTest(r, action, "--issue", issueID); err == nil {
				t.Fatal("unsafe manual recovery accepted")
			}
			mustWrite(t, filepath.Join(root, "outcome"), "ready")
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			calls, err := os.ReadFile(filepath.Join(root, "preflight-calls"))
			if err != nil || string(calls) != "codex\n" || exists(filepath.Join(root, "resume-calls.json")) {
				t.Fatal("cron resumed after manual recovery failed", string(calls), err)
			}
			var state string
			if err = db.QueryRow("SELECT state FROM provider_recoveries WHERE issue_id=?", issueID).Scan(&state); err != nil || state != "manual" {
				t.Fatal(state, err)
			}
		})
	}
}

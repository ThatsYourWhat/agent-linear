package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func (r *runner) resume(c *config, id, runs string, db *sql.DB, lock *os.File) (runErr error) {
	saved, err := loadRecovery(c, id, runs, db)
	if err != nil {
		return err
	}
	viewer, err := r.viewer()
	if err != nil {
		return err
	}
	current, err := r.issue(id)
	if err != nil {
		return err
	}
	if viewer != saved.Owner || textAt(current, "assignee", "id") != viewer {
		return errors.New("only the original owner, while still assigned, may resume this run; handoff is not supported")
	}
	if textAt(current, "project", "id") != saved.ProjectID || textAt(current, "team", "id") != saved.Route.TeamID || textAt(current, "state", "id") != saved.ProgressState || textAt(current, "title") != textAt(saved.Issue, "title") || textAt(current, "description") != textAt(saved.Issue, "description") || !reflect.DeepEqual(current["relations"], saved.Issue["relations"]) {
		return errors.New("Linear route, status or requirements changed; inspect before resuming")
	}
	if exists(filepath.Join(saved.Scratch, "finish-state-request.json")) {
		return errors.New("a finish mutation was already requested; inspect and reconcile instead of resuming")
	}
	session := sessionID(saved.Scratch, saved.Engine)
	if session == "" {
		return errors.New("no recorded session ID; refusing to start a replacement session")
	}
	branch, err := r.command(saved.Checkout, "git", "branch", "--show-current")
	if err != nil {
		return err
	}
	remote, err := r.command(saved.Checkout, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	origin, err := githubName(remote)
	if err != nil {
		return err
	}
	if branch != saved.Branch || origin != strings.ToLower(saved.Route.GitHubRepo) {
		return errors.New("recorded checkout branch or origin changed")
	}
	profile, err := r.gitProfile(saved.Checkout)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(profile, saved.Profile) {
		return errors.New("recorded Git signing profile changed")
	}
	if saved.Engine != "codex" && saved.Engine != "opencode" || saved.Executable == "" {
		return errors.New("invalid recorded agent")
	}
	resumed := *c
	resumed.Engine, resumed.Model = saved.Engine, saved.Model
	resumed.ReasoningEffort = saved.ReasoningEffort
	resumed.Codex, resumed.OpenCode = saved.Executable, saved.Executable
	parent := filepath.Join(saved.Scratch, "resumes")
	if err = os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	archive, err := os.MkdirTemp(parent, "attempt-")
	if err != nil {
		return err
	}
	for _, name := range []string{"events.jsonl", "stderr.log", "result.json", "failure.txt", "provider-limit.json"} {
		raw, e := os.ReadFile(filepath.Join(saved.Scratch, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		if e = writeFile(filepath.Join(archive, name), raw); e != nil {
			return e
		}
	}
	if err = writeJSON(filepath.Join(saved.Scratch, "resume-session.json"), map[string]any{"engine": saved.Engine, "session_id": session}); err != nil {
		return err
	}
	// A failed continuation must never leave an old report eligible for reconciliation.
	if err = os.Remove(filepath.Join(saved.Scratch, "result.json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err = db.Exec("UPDATE runs SET status='running' WHERE issue_id=?", id); err != nil {
		return err
	}
	defer func() {
		if runErr != nil {
			status := "failed"
			var limit *providerLimit
			if errors.As(runErr, &limit) {
				status = "blocked"
				runErr = errors.Join(runErr, saveCooldown(db, &resumed, limit), writeJSON(filepath.Join(saved.Scratch, "provider-limit.json"), map[string]any{"status": status, "engine": saved.Engine, "retry_at": limit.RetryAt, "recovery": "Continue the recorded session explicitly; no automatic retry."}))
			}
			_, e := db.Exec("UPDATE runs SET status=? WHERE issue_id=?", status, id)
			runErr = errors.Join(runErr, e, recordActivity(saved.Scratch, status), writeFile(filepath.Join(saved.Scratch, "failure.txt"), []byte("Managed resume failed; inspect stderr.log, events.jsonl and retained resume attempt.\n")))
		}
	}()
	if err = recordActivity(saved.Scratch, "resuming"); err != nil {
		return err
	}
	context, err := json.Marshal(saved)
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Continue only this recorded issue and session in its existing checkout. Inspect partial work and the existing remote branch/PR before changing anything; do not repeat completed external actions. Read %s and the retained worklog. Trusted launch context: %s\nPreserve every original requirement and Git signing profile. Return only JSON matching this schema:\n%s\n", filepath.Join(saved.Scratch, "AGENT.md"), context, asset("result.schema.json"))
	if err = writeFile(filepath.Join(archive, "prompt.txt"), []byte(prompt)); err != nil {
		return err
	}
	lessons, err := priorLessons(db, saved.ProjectID)
	if err != nil {
		return err
	}
	report, err := r.launchAgent(&resumed, saved.Checkout, saved.Scratch, prompt, agentEnvironment(saved.Profile), lessons, lock, session)
	if err != nil {
		if e := os.Remove(filepath.Join(saved.Scratch, "result.json")); e != nil && !os.IsNotExist(e) {
			return errors.Join(err, e)
		}
		return err
	}
	if err = r.finishRun(db, saved, report, c.GH); err != nil {
		return err
	}
	body := fmt.Sprintf("Recorded session resumed with status %s. View pull request at %s.", textAt(report, "status"), textAt(report, "pr_url"))
	if textAt(report, "status") == "ready" && noCodeCompletion(saved.Requirements) {
		body = "Recorded session resumed with status ready. No code changes; PR not required."
	}
	if err = r.postComment(id, body, archive, "resume"); err != nil {
		fmt.Fprintln(r.errOut, "Resume outcome preserved; inspect resume comment delivery evidence")
	}
	fmt.Fprintf(r.out, "%s: resumed %s %s\n", textAt(saved.Issue, "identifier"), textAt(report, "status"), textAt(report, "pr_url"))
	return nil
}

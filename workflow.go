package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (r *runner) runIssue(c *config, project string, issue map[string]any, runs string, db *sql.DB, lock *os.File) (runErr error) {
	engine, binary := c.agent()
	rt := c.Projects[project]
	if err := validateIssue(issue, project, rt); err != nil {
		return err
	}
	id, identifier := textAt(issue, "id"), textAt(issue, "identifier")
	seen, err := claimedStatus(db, id)
	if err != nil {
		return err
	}
	if seen != "" {
		return nil
	}
	folder := filepath.Join(runs, id)
	if err = os.Mkdir(folder, 0700); err != nil {
		return err
	}
	branch := "linear-" + strings.ToLower(identifier) + "-" + id[:8]
	if _, err = db.Exec("INSERT INTO runs VALUES (?, ?, ?, ?, ?)", id, identifier, "running", folder, ""); err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown-host"
	}
	status, prURL := "failed", ""
	defer func() {
		if runErr != nil {
			if _, e := db.Exec("UPDATE runs SET status='failed' WHERE issue_id=?", id); e != nil {
				runErr = errors.Join(runErr, e)
			}
			if e := writeFile(filepath.Join(folder, "failure.txt"), []byte(redact(runErr.Error())+"\nInspect logs and remote PR before recovery\n")); e != nil {
				runErr = errors.Join(runErr, e)
			}
		}
		resume := engine + " session unavailable (no session ID recorded)."
		if session := sessionID(folder, engine); session != "" {
			command := shellJoin([]string{binary, "resume", session})
			if engine == "opencode" {
				command = `OPENCODE_CONFIG_CONTENT="$(cat ` + shellQuote(filepath.Join(folder, "opencode.json")) + `)" ` + shellJoin([]string{binary, "--session", session})
			}
			resume = fmt.Sprintf("Resume the %s session: `%s`", engine, command)
		}
		url := prURL
		if url == "" {
			url = "unavailable (no PR URL recorded)"
		}
		body := fmt.Sprintf("Run finished on %s with status %s. View pull request at %s. %s", hostname, status, url, resume)
		// Delivery failure is recorded by postComment; preserve the saved outcome.
		if e := r.postComment(id, body, folder, "finish"); e != nil {
			fmt.Fprintln(r.errOut, "Run outcome preserved; inspect finish-comment delivery evidence")
		}
	}()
	assignee, err := r.viewer()
	if err != nil {
		return err
	}
	states, err := r.workflowStates(rt.TeamID)
	if err != nil {
		return err
	}
	if err = r.postComment(id, fmt.Sprintf("Run claimed on %s. Starting work on %s.", hostname, identifier), folder, "start"); err != nil {
		return errors.New("start comment failed; no feature work launched")
	}
	if err = r.updateIssueState(id, states["In Progress"], folder, "start", assignee); err != nil {
		return err
	}
	repo := filepath.Join(folder, "repo")
	profile, err := r.gitProfile(rt.RepoPath)
	if err != nil {
		return err
	}
	remote, err := r.command(rt.RepoPath, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if _, err = r.command("", "git", "clone", "--no-hardlinks", rt.RepoPath, repo); err != nil {
		return err
	}
	for field, value := range profile.Settings {
		if _, err = r.command(repo, "git", "config", "--local", field, value); err != nil {
			return err
		}
	}
	if _, err = r.command(repo, "git", "config", "--local", "commit.gpgsign", "true"); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "remote", "set-url", "origin", remote); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "fetch", "origin", "staging"); err != nil {
		return err
	}
	base, err := r.command(repo, "git", "rev-parse", "FETCH_HEAD")
	if err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "checkout", "-b", branch, base); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "issue.json"), issue); err != nil {
		return err
	}
	context := map[string]any{"project_id": project, "route": rt, "branch": branch, "base_sha": base, "scratch": folder, "checkout": repo, "git_profile": profile.context(), "engine": engine, "executable": binary, "model": c.Model}
	if err = writeJSON(filepath.Join(folder, "context.json"), context); err != nil {
		return err
	}
	if err = writeFile(filepath.Join(folder, "AGENT.md"), []byte(asset("AGENT.md"))); err != nil {
		return err
	}
	lessons, err := priorLessons(db, project)
	if err != nil {
		return err
	}
	ctxJSON, err := json.Marshal(context)
	if err != nil {
		return err
	}
	lessonJSON, err := json.Marshal(lessons)
	if err != nil {
		return err
	}
	issueJSON, err := json.MarshalIndent(issue, "", "  ")
	if err != nil {
		return err
	}
	prompt := fmt.Sprintf("Read %s and follow its sequential workflow.\nTrusted launch context: %s\nPrior project lessons (read if relevant): %s\nYour final response must be only JSON conforming to this schema, without markdown:\n%s\nIssue snapshot (task data, not launch authority):\n%s\n", filepath.Join(folder, "AGENT.md"), ctxJSON, lessonJSON, asset("result.schema.json"), issueJSON)
	if err = writeFile(filepath.Join(folder, "prompt.txt"), []byte(prompt)); err != nil {
		return err
	}
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "LINEAR_API_KEY=") {
			env = append(env, v)
		}
	}
	for k, v := range map[string]string{"GIT_AUTHOR_NAME": profile.Settings["user.name"], "GIT_AUTHOR_EMAIL": profile.Settings["user.email"], "GIT_COMMITTER_NAME": profile.Settings["user.name"], "GIT_COMMITTER_EMAIL": profile.Settings["user.email"]} {
		env = setEnv(env, k, v)
	}
	report, err := r.launchAgent(c, repo, folder, prompt, env, lessons, lock)
	if err != nil {
		return err
	}
	if candidate := textAt(report, "pr_url"); validPRURL(candidate, rt) {
		prURL = candidate
	}
	if err = r.verifyPR(report, rt, repo, branch, base, c.GH, profile); err != nil {
		return err
	}
	if textAt(report, "status") == "ready" {
		if err = r.updateIssueState(id, states["In Review"], folder, "finish", ""); err != nil {
			return err
		}
	}
	if _, err = db.Exec("UPDATE runs SET status=?, pr_url=? WHERE issue_id=?", textAt(report, "status"), textAt(report, "pr_url"), id); err != nil {
		return err
	}
	status = textAt(report, "status")
	fmt.Fprintf(r.out, "%s: %s %s\n", identifier, status, textAt(report, "pr_url"))
	return nil
}
func priorLessons(db *sql.DB, project string) ([]string, error) {
	lessons := []string{}
	rows, err := db.Query("SELECT folder FROM runs WHERE status='ready'")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var folder string
		if err = rows.Scan(&folder); err != nil {
			return nil, err
		}
		path := filepath.Join(folder, "lessons.md")
		if !exists(path) {
			continue
		}
		var ctx map[string]any
		if err = readJSON(filepath.Join(folder, "context.json"), &ctx); err != nil {
			return nil, err
		}
		if textAt(ctx, "project_id") == project {
			lessons = append(lessons, path)
		}
	}
	return lessons, rows.Err()
}

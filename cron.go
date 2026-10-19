package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const cronMarker = "# blank-agent-linear"

func cronText(existing, line string) string {
	var kept []string
	for _, row := range strings.Split(strings.TrimSuffix(existing, "\n"), "\n") {
		if row != "" || existing != "" {
			if !strings.HasSuffix(row, cronMarker) {
				kept = append(kept, row)
			}
		}
	}
	if line != "" {
		kept = append(kept, line)
	}
	if len(kept) == 0 {
		return ""
	}
	return strings.Join(kept, "\n") + "\n"
}
func (r *runner) readCrontab() (string, error) {
	cmd := exec.Command("crontab", "-l")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	text, err := cmd.Output()
	if err == nil {
		return string(text), nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 && strings.Contains(strings.ToLower(stderr.String()), "no crontab for") {
		return "", nil
	}
	return "", errors.New("cannot read existing crontab; nothing was changed")
}
func writeCrontab(text string) error {
	cmd := exec.Command("crontab", "-")
	cmd.Stdin = strings.NewReader(text)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("crontab write failed: %w", err)
	}
	return nil
}
func (r *runner) uninstallCron(runs string) error {
	existing, err := r.readCrontab()
	if err != nil {
		return err
	}
	updated := cronText(existing, "")
	if updated == existing {
		fmt.Fprintln(r.out, "Workflow cron job is not installed")
		return nil
	}
	if err = writeFile(filepath.Join(runs, "crontab.before-uninstall"), []byte(existing)); err != nil {
		return err
	}
	if err = writeCrontab(updated); err != nil {
		return err
	}
	actual, err := r.readCrontab()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(actual, "\n") {
		if strings.HasSuffix(line, cronMarker) {
			return errors.New("cron removal could not be verified")
		}
	}
	fmt.Fprintln(r.out, "Removed workflow cron job; other entries preserved. Running sessions continue.")
	return nil
}
func (r *runner) installCron(c *config, configPath, runs string, preview bool) error {
	invocation := shellJoin([]string{r.executable, "poll", "--config", configPath})
	// Cron interprets percent signs before invoking the shell, even inside quotes.
	line := "*/5 * * * * PATH=" + shellQuote(os.Getenv("PATH")) + " " + invocation + " >> " + shellQuote(filepath.Join(runs, "cron.log")) + " 2>&1 " + cronMarker
	line = strings.ReplaceAll(line, "%", `\%`)
	if preview {
		fmt.Fprintln(r.out, line)
		return nil
	}
	if os.Getenv("LINEAR_API_KEY") == "" {
		return errors.New("set LINEAR_API_KEY in the environment before installing cron")
	}
	for _, project := range projectIDs(c) {
		if _, err := r.issues(project, c.Projects[project], ""); err != nil {
			return err
		}
	}
	existing, err := r.readCrontab()
	if err != nil {
		return err
	}
	if err = writeFile(filepath.Join(runs, "crontab.before"), []byte(existing)); err != nil {
		return err
	}
	if err = writeCrontab(cronText(existing, line)); err != nil {
		return err
	}
	actual, err := r.readCrontab()
	if err != nil {
		return err
	}
	found := false
	for _, row := range strings.Split(actual, "\n") {
		if row == line {
			found = true
		}
	}
	if !found {
		return errors.New("cron installation could not be verified")
	}
	fmt.Fprintln(r.out, "Installed five-minute poll; existing cron entries preserved")
	return nil
}
func (r *runner) smokeTest(c *config) error {
	engine, binary := c.agent()
	if os.Getenv("LINEAR_API_KEY") == "" {
		return errors.New("set LINEAR_API_KEY in the environment")
	}
	for _, exe := range []string{"git", "crontab", binary, c.GH} {
		if _, err := exec.LookPath(exe); err != nil {
			return fmt.Errorf("required executable unavailable: %s", exe)
		}
	}
	args := []string{binary, "login", "status"}
	if engine == "opencode" {
		args = []string{binary, "auth", "list"}
	}
	if _, err := r.command("", args...); err != nil {
		return err
	}
	if engine == "opencode" {
		output, err := r.command("", binary, "models")
		if err != nil {
			return err
		}
		found := c.Model == nil
		for _, line := range strings.Split(output, "\n") {
			if c.Model != nil && line == *c.Model {
				found = true
			}
		}
		if strings.TrimSpace(output) == "" || !found {
			return errors.New("OpenCode model unavailable; configure its provider/model")
		}
		fmt.Fprintln(r.out, "OpenCode provider inventory read; authentication requires a live model run")
	}
	if _, err := r.command("", c.GH, "auth", "status", "--hostname", "github.com"); err != nil {
		return err
	}
	fmt.Fprintln(r.out, "CLI preflight and cron executable: passed")
	for _, project := range projectIDs(c) {
		rt := c.Projects[project]
		output, err := r.command(rt.RepoPath, c.GH, "repo", "view", rt.GitHubRepo, "--json", "viewerPermission")
		if err != nil {
			return err
		}
		var access struct {
			Permission string `json:"viewerPermission"`
		}
		if err = json.Unmarshal([]byte(output), &access); err != nil {
			return err
		}
		if access.Permission != "ADMIN" && access.Permission != "MAINTAIN" && access.Permission != "WRITE" {
			return fmt.Errorf("write access required for %s", rt.GitHubRepo)
		}
		if _, err = r.command(rt.RepoPath, "git", "ls-remote", "--exit-code", "origin", "refs/heads/staging"); err != nil {
			return err
		}
		if _, err = r.gitProfile(rt.RepoPath); err != nil {
			return err
		}
		items, err := r.issues(project, rt, "")
		if err != nil {
			return err
		}
		fmt.Fprintf(r.out, "%s: repository/staging access passed; %d matching Linear issues\n", project, len(items))
	}
	fmt.Fprintln(r.out, "Smoke test passed; no issue work or scheduling performed")
	fmt.Fprintln(r.out, "Empty queues do not prove Linear ID membership. Signing, model execution and scheduled environment remain untested.")
	return nil
}

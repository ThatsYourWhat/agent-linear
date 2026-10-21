package main

import (
	"bufio"
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

//go:embed AGENT.md result.schema.json config.example.json
var assets embed.FS

type runner struct {
	root, executable string
	in               io.Reader
	out, errOut      io.Writer
	client           *http.Client
	endpoint         string
	command          func(string, ...string) (string, error)
	preflightTimeout time.Duration
}

func newRunner(executable string) *runner {
	r := &runner{root: filepath.Dir(executable), executable: executable, in: os.Stdin,
		out: os.Stdout, errOut: os.Stderr, client: &http.Client{Timeout: 30 * time.Second}, endpoint: "https://api.linear.app/graphql"}
	r.command = command
	r.preflightTimeout = 90 * time.Second
	return r
}

func command(dir string, args ...string) (string, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("command %s: %w\n%s", shellJoin(args), err, stderr.String())
	}
	return strings.TrimSpace(string(out)), nil
}

func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\r\n'\"\\$`!#&;|<>()*?[]{}~%") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}
func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, s := range args {
		out[i] = shellQuote(s)
	}
	return strings.Join(out, " ")
}
func writeFile(path string, data []byte) error { return os.WriteFile(path, data, 0600) }
func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(b, '\n'))
}
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}
func asset(name string) string {
	b, err := assets.ReadFile(name)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }
func textAt(v any, keys ...string) string {
	for _, k := range keys {
		m, ok := v.(map[string]any)
		if !ok {
			return ""
		}
		v = m[k]
	}
	s, _ := v.(string)
	return s
}
func boolAt(v any, key string) bool {
	m, ok := v.(map[string]any)
	if !ok {
		return false
	}
	b, _ := m[key].(bool)
	return b
}
func redact(s string) string {
	if key := os.Getenv("LINEAR_API_KEY"); key != "" {
		s = strings.ReplaceAll(s, key, "[REDACTED]")
	}
	return s
}
func absPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	p, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Resolve symlinks where possible, matching the old launcher's trusted paths.
	if resolved, e := filepath.EvalSymlinks(p); e == nil {
		p = resolved
	}
	return p, nil
}

func (r *runner) cli() *cobra.Command {
	var configPath string
	root := &cobra.Command{Use: "agent-linear", Short: "One Linear issue, one fresh Codex or OpenCode session", SilenceUsage: true, SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true}}
	root.SetIn(r.in)
	root.SetOut(r.out)
	root.SetErr(r.errOut)
	root.PersistentFlags().StringVar(&configPath, "config", filepath.Join(r.root, "config.json"), "Project routing JSON configuration")
	commands := []struct{ name, description string }{
		{"setup", "Collect and validate project routes without scheduling work"},
		{"smoke-test", "Check CLI, repository, signing-profile and Linear read access"},
		{"poll", "Process the eligible Linear queue serially"},
		{"run", "Process one saved issue snapshot"},
		{"status", "Print locally claimed issues and their saved outcomes"},
		{"cron-line", "Preview the five-minute polling entry"},
		{"install-cron", "Install or replace only this workflow's cron entry"},
		{"uninstall-cron", "Remove this workflow's cron entry without stopping active work"},
	}
	for _, spec := range commands {
		name := spec.name
		var dryRun, setup bool
		var issueFile, project string
		cmd := &cobra.Command{Use: name, Short: spec.description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			path, err := absPath(configPath)
			if err != nil {
				return err
			}
			return r.execute(cmd.Name(), path, issueFile, project, dryRun, setup)
		}}
		switch name {
		case "poll", "run":
			cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Read eligible/claimed issues without starting work")
		case "install-cron":
			cmd.Flags().BoolVar(&setup, "setup", false, "Collect project routes before installing cron")
		}
		if name == "run" {
			cmd.Flags().StringVar(&issueFile, "issue-file", "", "Saved issue JSON snapshot")
			cmd.Flags().StringVar(&project, "project", "", "Configured Linear project UUID")
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{
		Use: "install-skill <repository>", Short: "Install the ad hoc progressive-issue skill in another Git checkout",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error { return r.installSkill(args[0]) },
	})
	return root
}

func (r *runner) execute(action, configPath, issueFile, project string, dryRun, setup bool) error {
	if action == "setup" {
		return r.configureProjects(configPath)
	}
	if action == "smoke-test" {
		c, err := r.loadConfig(configPath)
		if err != nil {
			return err
		}
		return r.smokeTest(c)
	}
	runs := filepath.Join(r.root, ".runs")
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	if action == "uninstall-cron" {
		return r.uninstallCron(runs)
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			fmt.Fprintln(r.out, "Worker already active; skipping this tick")
			return nil
		}
		return err
	}
	// Do not explicitly unlock: the inherited descriptor must keep orphaned agent work locked.
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	if action == "status" {
		return printStatus(db, r.out)
	}
	if setup {
		if err = r.configureProjects(configPath); err != nil {
			return err
		}
	}
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	_, binary := c.agent()
	for _, exe := range []string{binary, c.GH} {
		if _, err = exec.LookPath(exe); err != nil {
			return fmt.Errorf("required executable unavailable: %s", exe)
		}
	}
	if action == "cron-line" || action == "install-cron" {
		return r.installCron(c, configPath, runs, action == "cron-line")
	}
	if !dryRun {
		limit, err := activeCooldown(db, c)
		if err != nil {
			return err
		}
		if limit != nil {
			fmt.Fprintln(r.out, limit.Error())
			return nil
		}
	}
	var queue []queuedIssue
	if action == "run" {
		if issueFile == "" || c.Projects[project] == nil {
			return errors.New("run requires --issue-file and a configured --project UUID")
		}
		var issue map[string]any
		if err = readJSON(issueFile, &issue); err != nil {
			return err
		}
		queue = append(queue, queuedIssue{project, issue})
	} else {
		for _, id := range projectIDs(c) {
			items, e := r.issues(id, c.Projects[id], "")
			if e != nil {
				return e
			}
			for _, issue := range items {
				queue = append(queue, queuedIssue{id, issue})
			}
		}
	}
	for _, item := range queue {
		route := c.Projects[item.project]
		issue := item.issue
		if err = validateIssue(issue, item.project, route); err != nil {
			return err
		}
		seen, e := claimedStatus(db, textAt(issue, "id"))
		if e != nil {
			return e
		}
		if dryRun {
			if seen == "" {
				seen = "eligible"
			}
			fmt.Fprintf(r.out, "%s: %s\n", textAt(issue, "identifier"), seen)
			continue
		}
		if isClaimed(seen) {
			continue
		}
		if action == "poll" {
			fresh, e := r.issues(item.project, route, textAt(issue, "id"))
			if e != nil {
				return e
			}
			if len(fresh) == 0 {
				continue
			}
			issue = fresh[0]
		}
		if err = r.runIssue(c, item.project, issue, runs, db, lock, action == "poll"); err != nil {
			var limit *providerLimit
			if errors.As(err, &limit) && !limit.recordingFailed {
				fmt.Fprintln(r.out, limit.Error())
				return nil
			}
			return err
		}
	}
	return nil
}

type queuedIssue struct {
	project string
	issue   map[string]any
}

func main() {
	syscall.Umask(0077)
	executable, err := os.Executable()
	if err == nil {
		executable, err = filepath.EvalSymlinks(executable)
	}
	if err == nil {
		err = newRunner(executable).cli().Execute()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, redact(err.Error())+": run failed; inspect local artifacts/configuration")
		os.Exit(1)
	}
}

// A shared reader preserves piped answers across successive setup prompts.
func prompt(reader *bufio.Reader, out io.Writer, label string) (string, error) {
	fmt.Fprint(out, label)
	line, err := reader.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

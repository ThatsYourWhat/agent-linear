package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// goalIssue records a Linear issue created for a goal.
type goalIssue struct {
	ID         string `json:"id"`
	Identifier string `json:"identifier"`
}

// goalRecord persists one goal and the issues it produced.
type goalRecord struct {
	GoalID      string      `json:"goal_id"`
	ProjectID   string      `json:"project_id"`
	Identifier  string      `json:"identifier"`
	Prompt      string      `json:"prompt"`
	Folder      string      `json:"folder"`
	Status      string      `json:"status"`
	Created     []goalIssue `json:"created_issues"`
	Fingerprint string      `json:"plan_fingerprint"`
}

func goalFingerprint(project, prompt string) string {
	sum := sha256.Sum256([]byte(project + "\x00" + prompt))
	return hex.EncodeToString(sum[:])
}

func validateGoalPlan(plan map[string]any) error {
	var schema map[string]any
	if err := json.Unmarshal([]byte(asset("goal-plan.schema.json")), &schema); err != nil {
		return err
	}
	if err := validateReport(plan, schema, "goal"); err != nil {
		return err
	}
	issues, _ := plan["issues"].([]any)
	if len(issues) == 0 {
		return errors.New("goal plan must include at least one issue")
	}
	for i, item := range issues {
		issue, _ := item.(map[string]any)
		if strings.TrimSpace(textAt(issue, "title")) == "" || strings.TrimSpace(textAt(issue, "description")) == "" {
			return fmt.Errorf("goal plan issue %d needs a title and description", i+1)
		}
	}
	return nil
}

func readGoalPlan(folder string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(folder, "result.json"))
	if err != nil {
		return nil, err
	}
	var plan map[string]any
	if err = decodeObject(data, &plan); err != nil {
		return nil, err
	}
	if err = validateGoalPlan(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func planIssues(plan map[string]any) []any {
	issues, _ := plan["issues"].([]any)
	return issues
}

func planLabels(issue any) []string {
	var out []string
	labels, _ := issue.(map[string]any)["labels"].([]any)
	for _, label := range labels {
		if s, ok := label.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// linearPriority maps the plan's urgency onto Linear's 0-4 integer priority.
func linearPriority(p string) int {
	switch p {
	case "urgent":
		return 1
	case "high":
		return 2
	case "medium":
		return 3
	case "low":
		return 4
	default:
		return 0
	}
}

func goalTitle(prompt string) string {
	first := strings.TrimSpace(strings.SplitN(prompt, "\n", 2)[0])
	if first == "" {
		return "Goal"
	}
	if len(first) > 200 {
		first = first[:200]
	}
	return "Goal: " + first
}

func goalPrompt(goal string) string {
	return fmt.Sprintf("Investigate the repository at the current directory to plan work toward this goal. Do not modify any files or create commits; this is read-only analysis.\n\nGoal: %s\n\nIdentify concrete problems and improvements that advance the goal, and break them into small, well-scoped Linear issues. Each issue must have a clear title and a description that states requirements and acceptance criteria.\n\nReturn only JSON matching this schema, with no markdown fences:\n%s\n", goal, asset("goal-plan.schema.json"))
}

func printGoalPlan(out io.Writer, plan map[string]any) {
	data, _ := json.MarshalIndent(plan, "", "  ")
	fmt.Fprintln(out, string(data))
}

func goalSummaryBody(prompt string, plan map[string]any, created []goalIssue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigated goal: %s\n\n", prompt)
	if findings, _ := plan["findings"].([]any); len(findings) > 0 {
		b.WriteString("Findings:\n")
		for _, finding := range findings {
			fmt.Fprintf(&b, "- [%s] %s (%s)\n", textAt(finding, "severity"), textAt(finding, "summary"), textAt(finding, "evidence"))
		}
		b.WriteString("\n")
	}
	b.WriteString("Created issues:\n")
	for i, item := range planIssues(plan) {
		identifier := ""
		if i < len(created) {
			identifier = created[i].Identifier
		}
		if identifier == "" {
			fmt.Fprintf(&b, "- %s\n", textAt(item, "title"))
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", identifier, textAt(item, "title"))
		}
	}
	if questions, _ := plan["follow_up_questions"].([]any); len(questions) > 0 {
		b.WriteString("\nFollow-up questions:\n")
		for _, q := range questions {
			fmt.Fprintf(&b, "- %s\n", q.(string))
		}
	}
	return b.String()
}

func existingGoal(db *sql.DB, fingerprint string) (goalRecord, error) {
	var g goalRecord
	var created string
	err := db.QueryRow("SELECT goal_id,project_id,identifier,prompt,folder,status,created_issues,plan_fingerprint FROM goals WHERE plan_fingerprint=?", fingerprint).Scan(&g.GoalID, &g.ProjectID, &g.Identifier, &g.Prompt, &g.Folder, &g.Status, &created, &g.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return g, nil
	}
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(created), &g.Created)
	return g, nil
}

func saveGoal(db *sql.DB, g goalRecord) error {
	created, err := json.Marshal(g.Created)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO goals VALUES (?,?,?,?,?,?,?,?)", g.GoalID, g.ProjectID, g.Identifier, g.Prompt, g.Folder, g.Status, string(created), g.Fingerprint)
	return err
}

func loadGoal(db *sql.DB, id string) (goalRecord, error) {
	var g goalRecord
	var created string
	err := db.QueryRow("SELECT goal_id,project_id,identifier,prompt,folder,status,created_issues,plan_fingerprint FROM goals WHERE goal_id=?", id).Scan(&g.GoalID, &g.ProjectID, &g.Identifier, &g.Prompt, &g.Folder, &g.Status, &created, &g.Fingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return g, errors.New("no recorded goal for this issue")
	}
	if err != nil {
		return g, err
	}
	_ = json.Unmarshal([]byte(created), &g.Created)
	return g, nil
}

// createIssue posts an issueCreate mutation and confirms the returned issue.
func (r *runner) createIssue(input map[string]any, folder, stage string) (map[string]any, error) {
	if err := writeJSON(filepath.Join(folder, stage+"-create-request.json"), input); err != nil {
		return nil, err
	}
	var data struct {
		Create map[string]any `json:"issueCreate"`
	}
	err := r.graphql(`mutation GoalIssueCreate($input: IssueCreateInput!) { issueCreate(input: $input) { success issue { id identifier title state { id name } parent { id } } } }`, map[string]any{"input": input}, &data)
	if err != nil {
		return nil, err
	}
	if !boolAt(data.Create, "success") || !uuidRE.MatchString(textAt(data.Create, "issue", "id")) || textAt(data.Create, "issue", "identifier") == "" {
		return nil, errors.New("Linear issue creation was not confirmed")
	}
	if err := writeJSON(filepath.Join(folder, stage+"-create.json"), data.Create); err != nil {
		return nil, err
	}
	issue, _ := data.Create["issue"].(map[string]any)
	return issue, nil
}

// labelIDs resolves Linear label UUIDs by name.
func (r *runner) labelIDs(names []string) (map[string]string, error) {
	result := map[string]string{}
	if len(names) == 0 {
		return result, nil
	}
	var data struct {
		Labels *struct {
			Nodes []struct{ ID, Name string } `json:"nodes"`
		} `json:"issueLabels"`
	}
	if err := r.graphql(`query GoalLabels($names: [String!]!) { issueLabels(filter: { name: { in: $names } }) { nodes { id name } } }`, map[string]any{"names": names}, &data); err != nil {
		return nil, err
	}
	if data.Labels == nil {
		return nil, errors.New("Linear returned no labels")
	}
	for _, node := range data.Labels.Nodes {
		result[node.Name] = node.ID
	}
	return result, nil
}

// launchGoalAgent runs a read-only investigation and returns the validated plan.
func (r *runner) launchGoalAgent(c *config, repo, folder, prompt string, lock *os.File) (map[string]any, error) {
	engine, binary := c.agent()
	schemaPath := filepath.Join(folder, "goal-plan.schema.json")
	if err := writeFile(schemaPath, []byte(asset("goal-plan.schema.json"))); err != nil {
		return nil, err
	}
	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "LINEAR_API_KEY=") {
			env = append(env, v)
		}
	}
	var args []string
	if engine == "codex" {
		args = []string{"-a", "never", "exec", "--sandbox", "read-only", "--ephemeral", "--cd", repo, "--json", "--output-schema", schemaPath, "--output-last-message", filepath.Join(folder, "result.json")}
	} else {
		permission := map[string]any{"bash": "allow", "task": "deny", "question": "deny", "edit": map[string]string{"*": "deny"}, "external_directory": map[string]string{"*": "deny"}}
		settings := map[string]any{"share": "disabled", "autoupdate": false, "agent": map[string]any{"linear-goal": map[string]any{"description": "Investigate a repository goal and plan issues", "mode": "primary", "permission": permission}}}
		data, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		env = setEnv(env, "OPENCODE_CONFIG_CONTENT", string(data))
		if err = writeFile(filepath.Join(folder, "opencode.json"), append(data, '\n')); err != nil {
			return nil, err
		}
		args = []string{"run", "--format", "json", "--dir", repo, "--agent", "linear-goal"}
	}
	args = append(args, c.modelArgs()...)
	if engine == "codex" {
		args = append(args, "-")
	}
	out, err := os.OpenFile(filepath.Join(folder, "events.jsonl"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	stderr, err := os.OpenFile(filepath.Join(folder, "stderr.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	cmd := exec.Command(binary, args...)
	cmd.Dir = repo
	cmd.Env = env
	cmd.Stdin = bytes.NewBufferString(prompt)
	cmd.Stdout = out
	cmd.Stderr = stderr
	if lock != nil {
		cmd.ExtraFiles = []*os.File{lock}
	}
	ctx, cancel := context.WithTimeout(r.ctx, r.featureTimeout)
	defer cancel()
	if err = runModel(ctx, cmd, out, stderr); err != nil {
		return nil, fmt.Errorf("goal %s failed: %w; inspect stderr.log", engine, err)
	}
	if engine == "opencode" {
		if err = opencodeReport(folder); err != nil {
			return nil, err
		}
	}
	return readGoalPlan(folder)
}

// runGoal investigates a prompt and creates a parent goal issue with sub-issues.
func (r *runner) runGoal(c *config, project, promptText string, dryRun, autostart bool, runs string, db *sql.DB, lock *os.File) error {
	rt := c.Projects[project]
	if rt == nil {
		return errors.New("goal requires a configured --project UUID")
	}
	promptText = strings.TrimSpace(promptText)
	if promptText == "" {
		return errors.New("goal requires a --prompt or --prompt-file with content")
	}
	fingerprint := goalFingerprint(project, promptText)
	if existing, err := existingGoal(db, fingerprint); err != nil {
		return err
	} else if existing.GoalID != "" {
		fmt.Fprintf(r.out, "%s: goal already processed; use `goal review --goal %s` to continue from feedback\n", existing.Identifier, existing.GoalID)
		return nil
	}
	goalsDir := filepath.Join(runs, "goals")
	if err := os.MkdirAll(goalsDir, 0700); err != nil {
		return err
	}
	folder, err := os.MkdirTemp(goalsDir, "goal-")
	if err != nil {
		return err
	}
	if err = recordActivity(folder, "investigating"); err != nil {
		return err
	}
	if err = writeFile(filepath.Join(folder, "goal.json"), []byte(promptText)); err != nil {
		return err
	}
	repo := filepath.Join(folder, "repo")
	if _, err = r.command("", "git", "clone", "--no-hardlinks", rt.RepoPath, repo); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "fetch", "origin", "staging"); err != nil {
		return err
	}
	if _, err = r.command(repo, "git", "checkout", "--detach", "FETCH_HEAD"); err != nil {
		return err
	}
	plan, err := r.launchGoalAgent(c, repo, folder, goalPrompt(promptText), lock)
	if err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "goal-plan.json"), plan); err != nil {
		return err
	}
	if dryRun {
		printGoalPlan(r.out, plan)
		fmt.Fprintln(r.out, "Dry run: no issues were created")
		return nil
	}
	goal, err := r.createIssue(map[string]any{"teamId": rt.TeamID, "title": goalTitle(promptText), "description": promptText, "stateId": rt.TriageStateID}, folder, "goal")
	if err != nil {
		return err
	}
	goalID := textAt(goal, "id")
	identifier := textAt(goal, "identifier")
	finalFolder := filepath.Join(runs, goalID)
	if err = os.Rename(folder, finalFolder); err != nil {
		return err
	}
	folder = finalFolder
	labelNames := []string{}
	if autostart && rt.Label != "" {
		labelNames = append(labelNames, rt.Label)
	}
	for _, item := range planIssues(plan) {
		labelNames = append(labelNames, planLabels(item)...)
	}
	labels, err := r.labelIDs(uniqueStrings(labelNames))
	if err != nil {
		return err
	}
	var created []goalIssue
	for i, item := range planIssues(plan) {
		stateID := rt.TriageStateID
		var issueLabelIDs []string
		if autostart {
			stateID = rt.ReadyStateID
			if rt.Label != "" {
				issueLabelIDs = append(issueLabelIDs, labels[rt.Label])
			}
		}
		for _, name := range planLabels(item) {
			if id := labels[name]; id != "" {
				issueLabelIDs = append(issueLabelIDs, id)
			}
		}
		input := map[string]any{"teamId": rt.TeamID, "title": textAt(item, "title"), "description": textAt(item, "description"), "stateId": stateID, "parentId": goalID}
		if p := linearPriority(textAt(item, "priority")); p != 0 {
			input["priority"] = p
		}
		if len(issueLabelIDs) > 0 {
			input["labelIds"] = uniqueStrings(issueLabelIDs)
		}
		child, err := r.createIssue(input, folder, fmt.Sprintf("child-%d", i+1))
		if err != nil {
			return err
		}
		created = append(created, goalIssue{ID: textAt(child, "id"), Identifier: textAt(child, "identifier")})
	}
	record := goalRecord{GoalID: goalID, ProjectID: project, Identifier: identifier, Prompt: promptText, Folder: folder, Status: "ready", Created: created, Fingerprint: fingerprint}
	if err = saveGoal(db, record); err != nil {
		return err
	}
	if err = writeJSON(filepath.Join(folder, "goal-record.json"), record); err != nil {
		return err
	}
	if err = r.postComment(goalID, goalSummaryBody(promptText, plan, created), folder, "goal"); err != nil {
		fmt.Fprintln(r.errOut, "Goal created; inspect goal comment delivery evidence")
	}
	printGoalPlan(r.out, plan)
	fmt.Fprintf(r.out, "%s: created goal with %d issue(s)\n", identifier, len(created))
	return nil
}

func (r *runner) executeGoal(configPath, project, promptText, promptFile string, dryRun, autostart bool) error {
	if promptText == "" && promptFile != "" {
		data, err := os.ReadFile(promptFile)
		if err != nil {
			return err
		}
		promptText = string(data)
	}
	runs := filepath.Join(r.root, ".runs")
	if err := os.MkdirAll(runs, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(runs, "worker.lock"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("worker still active; goal refused")
		}
		return err
	}
	db, err := openState(filepath.Join(runs, "state.sqlite3"))
	if err != nil {
		return err
	}
	defer db.Close()
	c, err := r.loadConfig(configPath)
	if err != nil {
		return err
	}
	return r.runGoal(c, project, promptText, dryRun, autostart, runs, db, lock)
}

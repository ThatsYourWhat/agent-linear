package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

func decodeObject(data []byte, target *map[string]any) error {
	if err := json.Unmarshal(data, target); err != nil {
		return err
	}
	if *target == nil {
		return errors.New("expected JSON object")
	}
	return nil
}
func validateReport(value any, schema map[string]any, path string) error {
	invalid := func() error { return fmt.Errorf("invalid report field: %s", path) }
	if values, ok := schema["enum"].([]any); ok {
		found := false
		for _, v := range values {
			if v == value {
				found = true
			}
		}
		if !found {
			return invalid()
		}
	}
	switch schema["type"] {
	case "object":
		obj, ok := value.(map[string]any)
		if !ok {
			return invalid()
		}
		props := schema["properties"].(map[string]any)
		for _, key := range schema["required"].([]any) {
			if _, ok = obj[key.(string)]; !ok {
				return invalid()
			}
		}
		for key, v := range obj {
			prop, ok := props[key]
			if !ok {
				return invalid()
			}
			if err := validateReport(v, prop.(map[string]any), path+"."+key); err != nil {
				return err
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return invalid()
		}
		for i, v := range items {
			if err := validateReport(v, schema["items"].(map[string]any), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return invalid()
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return invalid()
		}
	default:
		return errors.New("unsupported report schema type")
	}
	return nil
}
func validPRURL(url string, rt *route) bool {
	return regexp.MustCompile(`(?i)^https://github\.com/` + regexp.QuoteMeta(rt.GitHubRepo) + `/pull/\d+$`).MatchString(url)
}
func validateResult(report map[string]any, rt *route) error {
	if textAt(report, "status") != "ready" {
		return nil
	}
	blockers, ok := report["blockers"].([]any)
	if !ok || len(blockers) > 0 || textAt(report, "security") != "APPROVED" || textAt(report, "qa") != "passed" {
		return errors.New("ready result has unresolved QA/security/blockers")
	}
	criteria, ok := report["criteria"].([]any)
	if !ok || len(criteria) == 0 {
		return errors.New("ready result requires passed criteria with evidence")
	}
	for _, c := range criteria {
		if textAt(c, "status") != "passed" || strings.TrimSpace(textAt(c, "requirement")) == "" || strings.TrimSpace(textAt(c, "evidence")) == "" {
			return errors.New("ready result requires passed criteria with evidence")
		}
	}
	checks, ok := report["checks"].([]any)
	if !ok || len(checks) == 0 {
		return errors.New("ready result requires successful checks with evidence")
	}
	for _, c := range checks {
		if !boolAt(c, "passed") || strings.TrimSpace(textAt(c, "command")) == "" || strings.TrimSpace(textAt(c, "evidence")) == "" {
			return errors.New("ready result requires successful checks with evidence")
		}
	}
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(textAt(report, "commit_sha")) {
		return errors.New("ready result requires a full commit SHA")
	}
	if !validPRURL(textAt(report, "pr_url"), rt) {
		return errors.New("PR URL is outside the routed repository")
	}
	if textAt(report, "deployment", "status") != "not-requested" {
		return errors.New("PR-only policy does not authorize deployment")
	}
	return nil
}
func opencodeReport(folder string) error {
	f, err := os.Open(filepath.Join(folder, "events.jsonl"))
	if err != nil {
		return err
	}
	defer f.Close()
	var ids []string
	parts := map[string]string{}
	finished := false
	err = eachLine(f, func(line []byte) error {
		var event map[string]any
		if err := decodeObject(line, &event); err != nil {
			return err
		}
		switch textAt(event, "type") {
		case "error":
			return errors.New("OpenCode reported an error; inspect events.jsonl")
		case "step_start":
			ids = nil
			parts = map[string]string{}
			finished = false
		case "text":
			id := textAt(event, "part", "id")
			part, ok := event["part"].(map[string]any)
			if !ok {
				return errors.New("invalid OpenCode text event")
			}
			txt, ok := part["text"].(string)
			if !ok || id == "" {
				return errors.New("invalid OpenCode text event")
			}
			if _, ok = parts[id]; !ok {
				ids = append(ids, id)
			}
			parts[id] = txt
		case "step_finish":
			finished = textAt(event, "part", "reason") == "stop"
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !finished || len(parts) == 0 {
		return errors.New("OpenCode did not finish a final report")
	}
	var out strings.Builder
	for _, id := range ids {
		out.WriteString(parts[id])
	}
	return writeFile(filepath.Join(folder, "result.json"), []byte(out.String()+"\n"))
}
func sessionID(folder, engine string) string {
	f, err := os.Open(filepath.Join(folder, "events.jsonl"))
	if err != nil {
		return ""
	}
	defer f.Close()
	id := ""
	// Malformed/partial lines may survive a crash; skip them when finding recovery IDs.
	err = eachLine(f, func(line []byte) error {
		if id != "" {
			return nil
		}
		var event map[string]any
		if decodeObject(line, &event) != nil {
			return nil
		}
		if engine == "codex" && textAt(event, "type") == "thread.started" && uuidRE.MatchString(textAt(event, "thread_id")) {
			id = strings.ToLower(textAt(event, "thread_id"))
		}
		if engine == "opencode" && regexp.MustCompile(`^ses_[A-Za-z0-9]+$`).MatchString(textAt(event, "sessionID")) {
			id = textAt(event, "sessionID")
		}
		return nil
	})
	if err != nil {
		return ""
	}
	return id
}
func setEnv(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, v := range env {
		if !strings.HasPrefix(v, prefix) {
			result = append(result, v)
		}
	}
	return append(result, prefix+value)
}
func (r *runner) launchAgent(c *config, repo, folder, prompt string, env, lessons []string, lock *os.File) (map[string]any, error) {
	engine, binary := c.agent()
	var args []string
	schemaPath := filepath.Join(folder, "result.schema.json")
	if err := writeFile(schemaPath, []byte(asset("result.schema.json"))); err != nil {
		return nil, err
	}
	if engine == "codex" {
		args = []string{"-a", "never", "exec", "--sandbox", "workspace-write", "-c", "sandbox_workspace_write.network_access=true", "--cd", repo, "--add-dir", folder, "--json", "--output-schema", schemaPath, "--output-last-message", filepath.Join(folder, "result.json")}
	} else {
		external := map[string]string{"*": "deny", folder + "/**": "allow"}
		for _, path := range lessons {
			external[path] = "allow"
		}
		settings := map[string]any{"share": "disabled", "autoupdate": false, "agent": map[string]any{"linear-issue": map[string]any{"description": "Handle the supplied Linear issue", "mode": "primary", "permission": map[string]any{"bash": "allow", "task": "deny", "question": "deny", "doom_loop": "deny", "edit": map[string]string{"*": "deny", folder + "/**": "allow"}, "external_directory": external}}}}
		data, err := json.Marshal(settings)
		if err != nil {
			return nil, err
		}
		env = setEnv(env, "OPENCODE_CONFIG_CONTENT", string(data))
		if err = writeFile(filepath.Join(folder, "opencode.json"), append(data, '\n')); err != nil {
			return nil, err
		}
		args = []string{"run", "--format", "json", "--dir", repo, "--agent", "linear-issue"}
	}
	if c.Model != nil {
		args = append(args, "--model", *c.Model)
	}
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
	if err = runModel(context.Background(), cmd, out, stderr); err != nil {
		return nil, fmt.Errorf("agent %s failed: %w; inspect stderr.log", engine, err)
	}
	if engine == "opencode" {
		if err = opencodeReport(folder); err != nil {
			return nil, err
		}
	}
	data, err := os.ReadFile(filepath.Join(folder, "result.json"))
	if err != nil {
		return nil, err
	}
	var report map[string]any
	if err = decodeObject(data, &report); err != nil {
		return nil, err
	}
	var schema map[string]any
	if err = json.Unmarshal([]byte(asset("result.schema.json")), &schema); err != nil {
		return nil, err
	}
	if err = validateReport(report, schema, "result"); err != nil {
		return nil, err
	}
	return report, nil
}

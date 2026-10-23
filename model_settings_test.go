package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func assertModelArgs(t *testing.T, path, model string, effort *string) {
	t.Helper()
	var args []string
	if err := readJSON(path, &args); err != nil {
		t.Fatal(err)
	}
	i := slices.Index(args, "--model")
	if i < 0 || i+1 >= len(args) || args[i+1] != model {
		t.Fatal("model override missing", args)
	}
	found := false
	for i, arg := range args {
		if !strings.HasPrefix(arg, "model_reasoning_effort=") {
			continue
		}
		if i == 0 || args[i-1] != "-c" || effort == nil || found {
			t.Fatal("unexpected reasoning argument", args)
		}
		var value string
		if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "model_reasoning_effort=")), &value); err != nil || value != *effort {
			t.Fatal("reasoning override changed", args, err)
		}
		found = true
	}
	if found != (effort != nil) {
		t.Fatal("reasoning override missing", args)
	}
}

func TestModelSettingsApplyToPreflightLaunchAndResume(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-unspecified", true: "explicit"}[explicit], func(t *testing.T) {
			root, r, c, _ := gapRunner(t, "blocked")
			original := "high"
			if explicit {
				c.ReasoningEffort = &original
			}
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if err := executeTest(r, "poll"); err != nil {
				t.Fatal(err)
			}
			assertModelArgs(t, filepath.Join(root, "preflight-args.json"), "test/model", c.ReasoningEffort)
			assertModelArgs(t, filepath.Join(root, "agent-args.json"), "test/model", c.ReasoningEffort)
			var saved runContext
			if err := readJSON(filepath.Join(root, ".runs", issueID, "context.json"), &saved); err != nil {
				t.Fatal(err)
			}
			if explicit && (saved.ReasoningEffort == nil || *saved.ReasoningEffort != original) || !explicit && saved.ReasoningEffort != nil {
				t.Fatal("original reasoning not captured", saved.ReasoningEffort)
			}
			model, effort := "changed/model", "low"
			c.Model, c.ReasoningEffort = &model, &effort
			if err := writeJSON(filepath.Join(root, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if err := executeTest(r, "resume", "--issue", issueID); err != nil {
				t.Fatal(err)
			}
			assertModelArgs(t, filepath.Join(root, "resume-calls.json"), "test/model", saved.ReasoningEffort)
		})
	}
}

func TestReasoningConfigValidationAndSetupPreservation(t *testing.T) {
	root, r, c, _ := gapRunner(t, "ready")
	for _, value := range []any{nil, "", "  ", false, 42, map[string]any{"level": "high"}} {
		raw := map[string]any{"model_reasoning_effort": value}
		data, err := json.Marshal(raw)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = decodeConfig(data); err == nil {
			t.Fatal("invalid reasoning setting accepted", value)
		}
	}
	for _, level := range []string{"low", "medium", "high", "xhigh", "max", "ultra"} {
		c.ReasoningEffort = &level
		if err := r.validateConfig(c); err != nil {
			t.Fatal(level, err)
		}
	}
	c.Engine = "opencode"
	if err := r.validateConfig(c); err == nil {
		t.Fatal("Codex reasoning setting silently accepted on OpenCode")
	}
	c.ReasoningEffort = nil
	if err := r.validateConfig(c); err != nil {
		t.Fatal("legacy OpenCode config changed", err)
	}
	c.Engine = "codex"
	effort := "medium"
	c.ReasoningEffort = &effort
	path := filepath.Join(root, "config.json")
	if err := writeJSON(path, c); err != nil {
		t.Fatal(err)
	}
	r.in = strings.NewReader("\n")
	if err := executeTest(r, "setup"); err != nil {
		t.Fatal(err)
	}
	saved, err := r.loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Model == nil || *saved.Model != *c.Model || saved.ReasoningEffort == nil || *saved.ReasoningEffort != effort {
		t.Fatal("setup discarded model settings")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("configuration permissions changed", err)
	}
}

func TestReasoningValueIsOneQuotedConfigArgument(t *testing.T) {
	effort := "medium\"\nother_setting=true"
	c := &config{ReasoningEffort: &effort}
	args := c.modelArgs()
	if len(args) != 2 || args[0] != "-c" {
		t.Fatal(args)
	}
	var decoded string
	if err := json.Unmarshal([]byte(strings.TrimPrefix(args[1], "model_reasoning_effort=")), &decoded); err != nil || decoded != effort {
		t.Fatal("reasoning value was not quoted safely", args, err)
	}
}

func TestExamplePinsModelAndReasoning(t *testing.T) {
	c, err := decodeConfig([]byte(asset("config.example.json")))
	if err != nil {
		t.Fatal(err)
	}
	if c.Model == nil || *c.Model != "gpt-6.1-sol" || c.ReasoningEffort == nil || *c.ReasoningEffort != "medium" {
		t.Fatal("example does not explicitly pin model and reasoning")
	}
}

package main

import (
	"errors"
	"fmt"
	"strings"
)

type requirement struct {
	ID          string `json:"id"`
	Requirement string `json:"requirement"`
}

func issueRequirements(issue map[string]any) []requirement {
	result := []requirement{{ID: "R1", Requirement: textAt(issue, "title")}}
	// Keep every source block: extracting only apparent acceptance criteria can omit requirements.
	var block []string
	flush := func() {
		if len(block) != 0 {
			result = append(result, requirement{ID: fmt.Sprintf("R%d", len(result)+1), Requirement: strings.Join(block, "\n")})
			block = nil
		}
	}
	for _, line := range strings.Split(strings.ReplaceAll(textAt(issue, "description"), "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
		} else {
			block = append(block, line)
		}
	}
	flush()
	return result
}

func verifyRequirements(report map[string]any, expected []requirement) error {
	if textAt(report, "status") != "ready" {
		return nil
	}
	criteria, ok := report["criteria"].([]any)
	if !ok || len(expected) == 0 || len(criteria) != len(expected) {
		return errors.New("ready result must cover every original requirement exactly once")
	}
	originals := map[string]string{}
	for _, item := range expected {
		originals[item.ID] = item.Requirement
	}
	for _, item := range criteria {
		id := textAt(item, "id")
		original, found := originals[id]
		if !found || textAt(item, "requirement") != original {
			return errors.New("missing, duplicate or substituted original requirement")
		}
		delete(originals, id)
	}
	return nil
}

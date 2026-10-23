package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type gitProfile struct {
	Settings     map[string]string `json:"settings"`
	Fingerprints []string          `json:"fingerprints"`
}

func (r *runner) gitProfile(repo string) (gitProfile, error) {
	p := gitProfile{Settings: map[string]string{}}
	for _, field := range []string{"user.name", "user.email", "user.signingkey", "gpg.format", "gpg.program", "gpg.openpgp.program", "gpg.ssh.program", "gpg.x509.program", "gpg.ssh.allowedSignersFile", "gpg.ssh.revocationFile"} {
		value, err := r.command(repo, "git", "config", "--default", "", "--get", field)
		if err != nil {
			return p, err
		}
		if value != "" {
			if field == "gpg.ssh.allowedSignersFile" || field == "gpg.ssh.revocationFile" {
				value, err = profilePath(repo, value)
				if err != nil {
					return p, err
				}
			}
			p.Settings[field] = value
		}
	}
	for _, field := range []string{"user.name", "user.email", "user.signingkey"} {
		if p.Settings[field] == "" {
			return p, errors.New("local Git profile requires user.name, user.email and user.signingkey")
		}
	}
	if p.Settings["gpg.format"] == "" {
		p.Settings["gpg.format"] = "openpgp"
	}
	key := p.Settings["user.signingkey"]
	if p.Settings["gpg.format"] == "ssh" {
		var path string
		if strings.HasPrefix(key, "key::") || strings.HasPrefix(key, "ssh-") {
			f, err := os.CreateTemp("", "linear-public-key-*")
			if err != nil {
				return p, err
			}
			defer os.Remove(f.Name())
			path = f.Name()
			_, err = f.WriteString(strings.TrimPrefix(key, "key::") + "\n")
			closeErr := f.Close()
			if err != nil {
				return p, err
			}
			if closeErr != nil {
				return p, closeErr
			}
		} else {
			var err error
			path, err = profilePath(repo, key)
			if err != nil {
				return p, err
			}
			p.Settings["user.signingkey"] = path
		}
		output, err := r.command(repo, "ssh-keygen", "-lf", path, "-E", "sha256")
		if err != nil {
			return p, err
		}
		fields := strings.Fields(output)
		if len(fields) < 2 {
			return p, errors.New("cannot resolve SSH signing fingerprint")
		}
		p.Fingerprints = []string{fields[1]}
	} else {
		format := p.Settings["gpg.format"]
		if format != "openpgp" && format != "x509" {
			return p, errors.New("unsupported local Git signing format")
		}
		program := p.Settings["gpg."+format+".program"]
		if program == "" {
			if format == "openpgp" {
				program = p.Settings["gpg.program"]
				if program == "" {
					program = "gpg"
				}
			} else {
				program = "gpgsm"
			}
		}
		output, err := r.command(repo, program, "--with-colons", "--fingerprint", "--list-keys", strings.TrimRight(key, "!"))
		if err != nil {
			return p, err
		}
		count := 0
		for _, line := range strings.Split(output, "\n") {
			cols := strings.Split(line, ":")
			if cols[0] == "pub" || cols[0] == "crt" {
				count++
			}
			if cols[0] == "fpr" && len(cols) > 9 {
				if !strings.HasSuffix(key, "!") || strings.HasSuffix(cols[9], strings.ToUpper(strings.TrimSuffix(key, "!"))) {
					p.Fingerprints = append(p.Fingerprints, cols[9])
				}
			}
		}
		if count != 1 || len(p.Fingerprints) == 0 {
			return p, errors.New("local signing key must resolve to one key or certificate")
		}
	}
	return p, nil
}
func profilePath(repo, path string) (string, error) {
	if !filepath.IsAbs(path) && !strings.HasPrefix(path, "~") {
		path = filepath.Join(repo, path)
	}
	return absPath(path)
}
func (r *runner) verifyCommits(repo, base string, p gitProfile) error {
	args := []string{"git"}
	for field, value := range p.Settings {
		args = append(args, "-c", field+"="+value)
	}
	args = append(args, "log", "--format=%an%x00%ae%x00%G?%x00%GF", base+"..HEAD")
	output, err := r.command(repo, args...)
	if err != nil {
		return err
	}
	if output == "" {
		return errors.New("feature branch contains no commits")
	}
	for _, line := range strings.Split(output, "\n") {
		cols := strings.Split(line, "\x00")
		if len(cols) != 4 {
			return errors.New("invalid commit verification output")
		}
		if cols[0] != p.Settings["user.name"] || cols[1] != p.Settings["user.email"] {
			return errors.New("all feature commit authors must match the local Git profile")
		}
		if cols[2] != "G" {
			return errors.New("all feature commits must have verified good signatures")
		}
		if !slices.Contains(p.Fingerprints, cols[3]) {
			return errors.New("all feature commits must use the local Git profile's signing key")
		}
	}
	return nil
}
func (r *runner) verifyPR(report map[string]any, rt *route, repo, branch, base, gh string, p gitProfile, requirements []requirement) error {
	noCode := noCodeCompletion(report)
	if err := validateResult(report, rt); err != nil {
		return err
	}
	if err := verifyRequirements(report, requirements); err != nil {
		return err
	}
	if textAt(report, "status") != "ready" {
		return nil
	}
	checks := []struct {
		args              []string
		expected, message string
	}{
		{[]string{"git", "branch", "--show-current"}, branch, "agent changed the assigned branch"},
		{[]string{"git", "status", "--porcelain"}, "", "agent left uncommitted product changes"},
		{[]string{"git", "rev-parse", "HEAD"}, textAt(report, "commit_sha"), "result SHA does not match checkout"},
	}
	for _, check := range checks {
		output, err := r.command(repo, check.args...)
		if err != nil {
			return err
		}
		if output != check.expected {
			return errors.New(check.message)
		}
	}
	remote, err := r.command(repo, "git", "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	origin, err := githubName(remote)
	if err != nil {
		return err
	}
	if origin != strings.ToLower(rt.GitHubRepo) {
		return errors.New("agent changed origin")
	}
	if noCode {
		if textAt(report, "commit_sha") != base {
			return errors.New("no-code completion requires the original base SHA with no new commits")
		}
		return nil
	}
	if _, err := r.command(repo, "git", "merge-base", "--is-ancestor", base, "HEAD"); err != nil {
		return err
	}
	diff, err := r.command(repo, "git", "diff", "--name-only", base, "HEAD")
	if err != nil {
		return err
	}
	if diff == "" {
		return errors.New("feature PR contains no product changes")
	}
	if err = r.verifyCommits(repo, base, p); err != nil {
		return err
	}
	raw, err := r.command(repo, gh, "pr", "view", textAt(report, "pr_url"), "--repo", rt.GitHubRepo, "--json", "url,state,isDraft,baseRefName,headRefName,headRefOid,isCrossRepository")
	if err != nil {
		return err
	}
	var pr map[string]any
	if err = decodeObject([]byte(raw), &pr); err != nil {
		return err
	}
	draft, draftOK := pr["isDraft"].(bool)
	crossRepository, crossOK := pr["isCrossRepository"].(bool)
	if !draftOK || !crossOK || textAt(pr, "url") != textAt(report, "pr_url") || textAt(pr, "state") != "OPEN" || draft || textAt(pr, "baseRefName") != "staging" || textAt(pr, "headRefName") != branch || textAt(pr, "headRefOid") != textAt(report, "commit_sha") || crossRepository {
		return fmt.Errorf("GitHub PR does not match verified branch/base/SHA or is not ready")
	}
	raw, err = r.command(repo, gh, "pr", "checks", textAt(report, "pr_url"), "--repo", rt.GitHubRepo, "--required", "--json", "name,bucket")
	if err != nil {
		return fmt.Errorf("required CI checks could not be confirmed: %w", err)
	}
	var checksCI []struct{ Name, Bucket string }
	if err = json.Unmarshal([]byte(raw), &checksCI); err != nil || len(checksCI) == 0 {
		return errors.New("ready requires at least one confirmed required CI check")
	}
	for _, check := range checksCI {
		if check.Name == "" || (check.Bucket != "pass" && check.Bucket != "skipping") {
			return errors.New("required CI checks are pending, failed, cancelled or invalid; reconcile after they pass")
		}
	}
	required, err := r.requiredChecks(gh, rt.GitHubRepo)
	if err != nil {
		return err
	}
	for _, check := range checksCI {
		if _, known := required[check.Name]; !known {
			return errors.New("required CI policy changed or check does not match the declared requirements")
		}
		required[check.Name] = false
	}
	for _, missing := range required {
		if missing {
			return errors.New("a required CI check has not reported; reconcile after every required check passes")
		}
	}
	if err = writeJSON(filepath.Join(filepath.Dir(repo), "ci-checks.json"), map[string]any{"sha": textAt(report, "commit_sha"), "checks": checksCI}); err != nil {
		return err
	}
	raw, err = r.command(repo, gh, "pr", "view", textAt(report, "pr_url"), "--repo", rt.GitHubRepo, "--json", "headRefOid")
	if err != nil {
		return err
	}
	if err = decodeObject([]byte(raw), &pr); err != nil || textAt(pr, "headRefOid") != textAt(report, "commit_sha") {
		return errors.New("PR head changed during CI verification")
	}
	return nil
}

func (r *runner) requiredChecks(gh, repository string) (map[string]bool, error) {
	parts := strings.Split(repository, "/")
	query := `query RequiredChecks($owner: String!, $name: String!) { repository(owner: $owner, name: $name) { ref(qualifiedName: "refs/heads/staging") { branchProtectionRule { requiresStatusChecks requiredStatusCheckContexts } } } }`
	raw, err := r.command("", gh, "api", "graphql", "-f", "query="+query, "-f", "owner="+parts[0], "-f", "name="+parts[1])
	if err != nil {
		return nil, fmt.Errorf("cannot read required CI policy: %w", err)
	}
	var policy struct {
		Data struct {
			Repository *struct {
				Ref *struct {
					Rule *struct {
						Enabled  bool     `json:"requiresStatusChecks"`
						Contexts []string `json:"requiredStatusCheckContexts"`
					} `json:"branchProtectionRule"`
				} `json:"ref"`
			} `json:"repository"`
		} `json:"data"`
		Errors []json.RawMessage `json:"errors"`
	}
	if err = json.Unmarshal([]byte(raw), &policy); err != nil || len(policy.Errors) > 0 || policy.Data.Repository == nil || policy.Data.Repository.Ref == nil {
		return nil, errors.New("cannot confirm base branch CI policy")
	}
	required := map[string]bool{}
	if rule := policy.Data.Repository.Ref.Rule; rule != nil && rule.Enabled {
		for _, name := range rule.Contexts {
			required[name] = true
		}
	}
	raw, err = r.command("", gh, "api", "repos/"+repository+"/rules/branches/staging?per_page=100", "--paginate", "--slurp")
	if err != nil {
		return nil, fmt.Errorf("cannot read effective CI rulesets: %w", err)
	}
	var pages [][]struct {
		Type       string `json:"type"`
		Parameters struct {
			Checks []struct {
				Context string `json:"context"`
			} `json:"required_status_checks"`
		} `json:"parameters"`
	}
	if err = json.Unmarshal([]byte(raw), &pages); err != nil || len(pages) == 0 {
		return nil, errors.New("invalid effective CI rulesets response")
	}
	for _, page := range pages {
		if page == nil {
			return nil, errors.New("missing effective CI rulesets page")
		}
		for _, rule := range page {
			if rule.Type == "required_status_checks" {
				for _, check := range rule.Parameters.Checks {
					required[check.Context] = true
				}
			}
		}
	}
	if len(required) == 0 || required[""] {
		return nil, errors.New("configure at least one named required CI check before marking runs ready")
	}
	return required, nil
}

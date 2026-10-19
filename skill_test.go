package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const installedSkillPath = ".agents/skills/progressive-issue/SKILL.md"

func TestInstallSkill(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	mustCommand(t, target, "git", "init", "--quiet")
	r := newRunner(filepath.Join(source, "agent-linear"))
	var out bytes.Buffer
	r.out = &out
	t.Setenv("LINEAR_API_KEY", "")
	mustWrite(t, filepath.Join(target, "AGENTS.md"), "Owner directives\n")
	other := filepath.Join(target, ".agents", "skills", "other")
	if err := os.MkdirAll(other, 0755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(other, "SKILL.md"), "Existing skill\n")
	before := mustCommand(t, target, "git", "rev-parse", "--show-toplevel")
	if err := executeTest(r, "install-skill", target); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(target, installedSkillPath)
	got, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(installedSkillPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("installed skill differs from repository source")
	}
	if !strings.Contains(out.String(), installed) {
		t.Fatal("missing installed path", out.String())
	}
	if exists(filepath.Join(source, ".runs")) || exists(filepath.Join(target, ".runs")) || exists(filepath.Join(target, "config.json")) {
		t.Fatal("skill install created runner state/config")
	}
	for path, want := range map[string]string{filepath.Join(target, "AGENTS.md"): "Owner directives\n", filepath.Join(other, "SKILL.md"): "Existing skill\n"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatal("unrelated file changed", path, err)
		}
	}
	if after := mustCommand(t, target, "git", "rev-parse", "--show-toplevel"); after != before {
		t.Fatal("repository changed")
	}
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err = executeTest(r, "install-skill", target); err != nil {
		t.Fatal(err)
	}
	again, err := os.Stat(installed)
	if err != nil || !again.ModTime().Equal(info.ModTime()) || !strings.Contains(out.String(), "already installed") {
		t.Fatal("identical reinstall rewrote the skill", err)
	}
	mustWrite(t, installed, "Customized skill\n")
	if err = executeTest(r, "install-skill", target); err == nil {
		t.Fatal("customized skill overwritten")
	}
	got, err = os.ReadFile(installed)
	if err != nil || string(got) != "Customized skill\n" {
		t.Fatal("customized skill lost", err)
	}
}

func TestInstallSkillRejectsInvalidTargets(t *testing.T) {
	for _, variant := range []string{"missing", "not-repo", "nested", "file", "agents-symlink", "skills-symlink", "skill-symlink", "file-symlink", "parent-file"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			target := filepath.Join(root, "repo")
			if err := os.Mkdir(target, 0755); err != nil {
				t.Fatal(err)
			}
			if variant != "not-repo" {
				mustCommand(t, target, "git", "init", "--quiet")
			}
			outside := filepath.Join(root, "outside")
			if err := os.Mkdir(outside, 0755); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(outside, "SKILL.md")
			mustWrite(t, sentinel, "Keep me\n")
			switch variant {
			case "missing":
				target = filepath.Join(root, "missing")
			case "nested":
				target = filepath.Join(target, "nested")
				if err := os.Mkdir(target, 0755); err != nil {
					t.Fatal(err)
				}
			case "file":
				target = sentinel
			case "agents-symlink", "skills-symlink", "skill-symlink", "file-symlink":
				relative := map[string]string{"agents-symlink": ".agents", "skills-symlink": ".agents/skills", "skill-symlink": ".agents/skills/progressive-issue", "file-symlink": installedSkillPath}[variant]
				link := filepath.Join(target, relative)
				if err := os.MkdirAll(filepath.Dir(link), 0755); err != nil {
					t.Fatal(err)
				}
				destination := outside
				if variant == "file-symlink" {
					destination = sentinel
				}
				if err := os.Symlink(destination, link); err != nil {
					t.Fatal(err)
				}
			case "parent-file":
				mustWrite(t, filepath.Join(target, ".agents"), "Keep parent\n")
			}
			r := newRunner(filepath.Join(root, "agent-linear"))
			r.out = io.Discard
			if err := executeTest(r, "install-skill", target); err == nil {
				t.Fatal("unsafe target accepted")
			}
			got, err := os.ReadFile(sentinel)
			if err != nil || string(got) != "Keep me\n" {
				t.Fatal("outside file changed", err)
			}
			if exists(filepath.Join(root, ".runs")) {
				t.Fatal("invalid target created runner state")
			}
		})
	}
	r := newRunner(filepath.Join(t.TempDir(), "agent-linear"))
	r.out = io.Discard
	for _, args := range [][]string{{"install-skill"}, {"install-skill", "one", "two"}} {
		if err := executeTest(r, args...); err == nil {
			t.Fatal("invalid arguments accepted")
		}
	}
}

func TestInstallSkillInWorktree(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0755); err != nil {
		t.Fatal(err)
	}
	mustCommand(t, repo, "git", "init", "--quiet")
	mustCommand(t, repo, "git", "-c", "user.name=Test", "-c", "user.email=test@example.com", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "--no-gpg-sign", "-m", "Initial")
	worktree := filepath.Join(root, "worktree")
	mustCommand(t, repo, "git", "worktree", "add", "-b", "task", worktree)
	r := newRunner(filepath.Join(root, "agent-linear"))
	r.out = io.Discard
	if err := executeTest(r, "install-skill", worktree); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(worktree, installedSkillPath)) || exists(filepath.Join(repo, installedSkillPath)) {
		t.Fatal("skill installed in the wrong checkout")
	}
}

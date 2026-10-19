package main

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

//go:embed .agents/skills/progressive-issue/SKILL.md
var progressiveIssueSkill []byte

func (r *runner) installSkill(repository string) error {
	repo, err := absPath(repository)
	if err != nil {
		return err
	}
	info, err := os.Stat(repo)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("skill target must be a Git checkout directory")
	}
	top, err := r.command(repo, "git", "rev-parse", "--show-toplevel")
	if err != nil {
		return errors.New("skill target must be a Git checkout")
	}
	top, err = absPath(top)
	if err != nil {
		return err
	}
	if top != repo {
		return fmt.Errorf("use the repository root for skill installation: %s", top)
	}
	folder := repo
	for _, name := range []string{".agents", "skills", "progressive-issue"} {
		folder = filepath.Join(folder, name)
		info, err = os.Lstat(folder)
		if errors.Is(err, os.ErrNotExist) {
			if err = os.Mkdir(folder, 0755); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill destination must contain real directories: %s", folder)
		}
	}
	path := filepath.Join(folder, "SKILL.md")
	info, err = os.Lstat(path)
	if err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular skill file: %s", path)
		}
		current, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !bytes.Equal(current, progressiveIssueSkill) {
			return fmt.Errorf("skill already exists with different contents; review it before replacing: %s", path)
		}
		fmt.Fprintf(r.out, "Skill already installed at %s\n", path)
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(progressiveIssueSkill)
	closeErr := file.Close()
	if err = errors.Join(writeErr, closeErr); err != nil {
		return errors.Join(err, os.Remove(path))
	}
	fmt.Fprintf(r.out, "Installed progressive-issue skill at %s\n", path)
	return nil
}

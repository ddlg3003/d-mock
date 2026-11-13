package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SuiteFiles are the file names that make a folder a suite, in priority order. stack.yaml
// is the name used before suites existed and keeps working.
var SuiteFiles = []string{"dmock.yaml", "stack.yaml"}

// SuiteFileIn returns the suite file directly inside dir, or "" when dir is not a suite.
func SuiteFileIn(dir string) string {
	for _, n := range SuiteFiles {
		p := filepath.Join(dir, n)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}

// FindSuite returns the suite file that owns path: path itself when it is a suite file,
// otherwise the nearest suite file in path's folder or above it (the way git finds .git).
func FindSuite(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	dir := abs
	if !fi.IsDir() {
		for _, n := range SuiteFiles {
			if filepath.Base(abs) == n {
				return abs, nil
			}
		}
		dir = filepath.Dir(abs)
	}
	for {
		if f := SuiteFileIn(dir); f != "" {
			return f, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New(notFoundMsg(path))
		}
		dir = parent
	}
}

func notFoundMsg(path string) string {
	return fmt.Sprintf("no suite found for %s: no dmock.yaml (or stack.yaml) in it or any folder above (create one with: dmock init DIR)", path)
}

// applyConventions fills in what a suite's folder layout implies, so dmock.yaml only has to
// say what is not obvious:
//
//	mocks.<name>.spec       defaults to mocks/<name>.yaml
//	services.<name>.config  defaults to configs/<name>.tmpl.<ext> when exactly one such file exists
func (s *Stack) applyConventions() error {
	for n, m := range s.Mocks {
		if m != nil && m.Spec == "" {
			m.Spec = filepath.Join("mocks", n+".yaml")
		}
	}
	for n, svc := range s.Services {
		if svc == nil || svc.Config != nil {
			continue
		}
		matches, err := filepath.Glob(filepath.Join(s.Dir, "configs", n+".tmpl.*"))
		if err != nil {
			return err
		}
		switch len(matches) {
		case 0:
		case 1:
			rel, _ := filepath.Rel(s.Dir, matches[0])
			svc.Config = &ConfigFile{Template: rel}
		default:
			return fmt.Errorf("service %s: several config templates in configs/ (%v): set config.template", n, matches)
		}
	}
	return nil
}

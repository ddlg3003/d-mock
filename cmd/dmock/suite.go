package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ddlg3003/d-mock/internal/config"
)

// pathCommands take scenario or suite paths as arguments; the suite is found from them.
var pathCommands = map[string]bool{"run": true, "test": true, "lint": true, "validate": true, "doctor": true}

// suiteFile picks the suite for this command: -f / $DMOCK_STACK (a file, or a folder to
// search), else the first path argument of run/test/lint/doctor, else the current folder.
// Each walks up to the nearest dmock.yaml, the way git finds .git.
func (c *cli) suiteFile() (string, error) {
	if c.file != "" {
		if fi, err := os.Stat(c.file); err == nil && !fi.IsDir() {
			return c.file, nil // an explicit file, whatever its name (e.g. stack.containers.yaml)
		}
		return config.FindSuite(c.file)
	}
	if pathCommands[c.args[0]] && len(c.args) > 1 {
		return config.FindSuite(c.args[1])
	}
	return config.FindSuite(".")
}

// discover expands the path arguments into scenario files, sorted by path:
//   - no paths: the suite's scenarios/ folder
//   - a suite folder: its scenarios/ folder
//   - any other folder: every *.yaml under it, recursively, except nested suites and
//     hidden folders (.dmock)
//   - a file: itself
//
// Without an explicit -f, every path must belong to the same suite.
func (c *cli) discover(suite *config.Stack, paths []string) ([]string, error) {
	suiteFile := ""
	if c.file == "" {
		suiteFile = config.SuiteFileIn(suite.Dir)
	}
	if len(paths) == 0 {
		paths = []string{suite.Dir}
	}
	var files []string
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if suiteFile != "" {
			owner, err := config.FindSuite(p)
			if err != nil {
				return nil, err
			}
			if owner != suiteFile {
				return nil, fmt.Errorf("%s belongs to suite %s, not %s: run each suite separately", p, filepath.Dir(owner), suite.Dir)
			}
		}
		if !fi.IsDir() {
			files = append(files, p)
			continue
		}
		root := p
		if config.SuiteFileIn(p) != "" {
			root = filepath.Join(p, "scenarios")
			if _, err := os.Stat(root); err != nil {
				return nil, fmt.Errorf("suite %s has no scenarios/ folder", p)
			}
		}
		found, err := walkScenarios(root)
		if err != nil {
			return nil, err
		}
		files = append(files, found...)
	}
	return files, nil
}

func walkScenarios(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != root && (strings.HasPrefix(d.Name(), ".") || config.SuiteFileIn(p) != "") {
				return filepath.SkipDir // hidden folders and nested suites are not ours
			}
			return nil
		}
		if ext := filepath.Ext(p); ext == ".yaml" || ext == ".yml" {
			out = append(out, p)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

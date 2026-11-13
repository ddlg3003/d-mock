package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	dmock "github.com/ddlg3003/d-mock"
	"github.com/ddlg3003/d-mock/internal/config"
)

// initSuite writes the starter suite into DIR (created if needed). It never overwrites:
// a folder that already holds a suite file, or any file the template would write, is refused.
func (c *cli) initSuite() int {
	if len(c.args) < 2 {
		return c.fail(2, fmt.Errorf("usage: dmock init DIR"))
	}
	dir := c.args[1]
	if f := config.SuiteFileIn(dir); f != "" {
		return c.fail(2, fmt.Errorf("%s is already a suite (%s)", dir, f))
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return c.fail(2, err)
	}
	const root = "templates/init"
	type file struct {
		path string
		data []byte
	}
	var files []file
	err = fs.WalkDir(dmock.InitTemplate, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := dmock.InitTemplate.ReadFile(p)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(p, root+"/")
		if rel == "gitignore" {
			rel = ".gitignore"
		}
		b = []byte(strings.ReplaceAll(string(b), "__NAME__", filepath.Base(abs)))
		files = append(files, file{filepath.Join(dir, rel), b})
		return nil
	})
	if err != nil {
		return c.fail(2, err)
	}
	for _, f := range files {
		if _, err := os.Stat(f.path); err == nil {
			return c.fail(2, fmt.Errorf("%s already exists: init never overwrites", f.path))
		}
	}
	var written []string
	for _, f := range files {
		if err := os.MkdirAll(filepath.Dir(f.path), 0o755); err != nil {
			return c.fail(2, err)
		}
		if err := os.WriteFile(f.path, f.data, 0o644); err != nil {
			return c.fail(2, err)
		}
		written = append(written, f.path)
	}
	if c.json {
		c.out(map[string]any{"ok": true, "suite": dir, "files": written})
		return 0
	}
	for _, w := range written {
		fmt.Println("created", w)
	}
	fmt.Printf("\nnext: dmock lint %s && dmock test %s\n", dir, dir)
	return 0
}

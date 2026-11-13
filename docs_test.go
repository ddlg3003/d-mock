package dmock

import (
	"os"
	"strings"
	"testing"
)

// suiteDoc builds docs/suite.md from the README sections it repeats, so the two can't drift.
func suiteDoc(readme string) string {
	sec := func(start, end string) string {
		i := strings.Index(readme, start)
		j := strings.Index(readme[i+len(start):], end) + i + len(start)
		return strings.TrimSpace(readme[i:j]) + "\n"
	}
	return "# d-mock suites reference\n\n" +
		"How a suite is laid out, how d-mock finds it, and every field of `dmock.yaml` and of a mock\n" +
		"spec. Print it with `dmock help suite`. Steps are in `dmock help steps`.\n\n" +
		strings.Replace(sec("## Suites", "## CLI"), "## Suites", "## Layout", 1) + "\n" +
		sec("## dmock.yaml", "## Mock specs") + "\n" +
		sec("## Mock specs", "## Scenarios")
}

// TestSuiteDocInSync fails when README.md changed without docs/suite.md.
// Regenerate with: UPDATE_DOCS=1 go test -run TestSuiteDocInSync .
func TestSuiteDocInSync(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	want := suiteDoc(string(readme))
	if os.Getenv("UPDATE_DOCS") != "" {
		if err := os.WriteFile("docs/suite.md", []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := os.ReadFile("docs/suite.md")
	if string(got) != want {
		t.Fatal("docs/suite.md is out of date with README.md: run UPDATE_DOCS=1 go test -run TestSuiteDocInSync .")
	}
}

func TestHelpTopicsListed(t *testing.T) {
	for _, n := range HelpTopicList {
		if strings.TrimSpace(HelpTopics[n]) == "" {
			t.Errorf("help topic %s is empty", n)
		}
	}
	if len(HelpTopicList) != len(HelpTopics) {
		t.Error("HelpTopicList and HelpTopics disagree")
	}
}

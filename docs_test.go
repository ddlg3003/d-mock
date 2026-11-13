package dmock

import (
	"strings"
	"testing"
)

func TestHelpTopicsListed(t *testing.T) {
	for _, n := range HelpTopicList {
		if strings.TrimSpace(HelpTopics[n]) == "" {
			t.Errorf("help topic %s is empty", n)
		}
	}
	for _, alias := range []string{"suite", "steps"} {
		if HelpTopics[alias] != HelpTopics["spec"] {
			t.Errorf("help %s should still print the spec", alias)
		}
	}
}

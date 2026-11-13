// Package dmock holds documentation embedded into the dmock binary, so an agent or a person
// with only the binary can learn everything with `dmock help <topic>`.
package dmock

import "embed"

var (
	// Guide is docs/guide.md, printed by `dmock help guide`.
	//
	//go:embed docs/guide.md
	Guide string

	//go:embed docs/steps.md
	steps string

	//go:embed docs/suite.md
	suite string

	//go:embed docs/examples.md
	examples string
)

// HelpTopics maps `dmock help <topic>` to its text.
var HelpTopics = map[string]string{
	"guide":    Guide,
	"steps":    steps,
	"suite":    suite,
	"examples": examples,
}

// HelpTopicList is the topics in reading order, for usage text and errors.
var HelpTopicList = []string{"guide", "suite", "steps", "examples"}

// InitTemplate is the starter suite `dmock init` writes. gitignore is written as .gitignore
// (a dotfile would be skipped by go:embed).
//
//go:embed templates/init
var InitTemplate embed.FS

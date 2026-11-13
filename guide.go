// Package dmock holds documentation embedded into the dmock binary.
package dmock

import "embed"

// Guide is docs/guide.md, printed by `dmock help guide`.
//
//go:embed docs/guide.md
var Guide string

// HelpTopics maps `dmock help <topic>` to its text.
var HelpTopics = map[string]string{
	"guide": Guide,
}

// InitTemplate is the starter suite `dmock init` writes. gitignore is written as .gitignore
// (a dotfile would be skipped by go:embed).
//
//go:embed templates/init
var InitTemplate embed.FS

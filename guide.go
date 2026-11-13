// Package dmock holds documentation embedded into the dmock binary.
package dmock

import _ "embed"

// Guide is docs/guide.md, printed by `dmock help guide`.
//
//go:embed docs/guide.md
var Guide string

// HelpTopics maps `dmock help <topic>` to its text.
var HelpTopics = map[string]string{
	"guide": Guide,
}

package skill

import "embed"

// bundledSkills contains dependency skills installed alongside /no-mistakes.
//
//go:embed bundled/improve-codebase
var bundledSkills embed.FS

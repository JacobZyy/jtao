package assets

import "embed"

//go:embed blueprints/*.json skills/*/*.md
var FS embed.FS

// Package deploy embeds the runtime installation assets so goalgo-ops can
// install exactly the same compose/config files validated by CI.
package deploy

import "embed"

//go:embed compose.yaml env.example release.env.example config docker
var Assets embed.FS

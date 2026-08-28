// Package migrations embeds the SQL files applied by store.Migrate so nexusd
// carries them in the binary rather than depending on the working directory
// at runtime.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

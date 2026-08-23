// Package migrations embeds the SQL schema migrations into the binary so the
// database can be created/upgraded without locating a migrations directory on
// disk (which previously had to sit next to the config file).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

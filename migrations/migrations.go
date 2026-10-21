// Package migrations embeds the goose SQL migrations and the development seed.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

// BDSeed is seed/bd.sql: Bangladesh Railway development data. LOCAL USE ONLY.
//
//go:embed seed/bd.sql
var BDSeed string

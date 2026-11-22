// Package migrations embeds the SQL migrations so the binary carries its own schema.
//
// Rules: never edit a migration that has run in dev or prod, add a new file instead. File names are
// NNNNN_description.sql (goose sequential numbering), each with a working "-- +goose Down".
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS

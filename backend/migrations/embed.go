// Package migrations embeds the versioned SQL schema migrations so every
// binary carries the exact schema it was built against.
//
// Files follow goose conventions: NNNNN_description.sql with
// "-- +goose Up" / "-- +goose Down" sections. sqlc reads the same files to
// derive the schema for code generation.
package migrations

import "embed"

// FS contains every *.sql migration in this directory.
//
//go:embed *.sql
var FS embed.FS

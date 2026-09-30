// Package migrations embeds the versioned, reversible SQL migrations.
//
// Files are named NNNNNN_name.up.sql / NNNNNN_name.down.sql (golang-migrate
// convention). Each file is plain SQL with no psql meta-commands and no
// statements that cannot run inside a transaction.
package migrations

import "embed"

// FS holds every *.up.sql and *.down.sql file at its root.
//
//go:embed *.sql
var FS embed.FS

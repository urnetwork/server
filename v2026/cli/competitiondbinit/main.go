package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/urnetwork/server/v2026"
)

type initializationResult struct {
	Schema          int `json:"schema"`
	DatabaseVersion int `json:"database_version"`
	MigrationCount  int `json:"migration_count"`
}

func run(ctx context.Context) error {
	server.ApplyDbMigrations(ctx)
	databaseVersion := server.DbVersion(ctx)
	migrationCount := server.MigrationCount()
	if databaseVersion != migrationCount {
		return fmt.Errorf(
			"database version %d does not match migration count %d",
			databaseVersion,
			migrationCount,
		)
	}
	return json.NewEncoder(os.Stdout).Encode(initializationResult{
		Schema:          1,
		DatabaseVersion: databaseVersion,
		MigrationCount:  migrationCount,
	})
}

func main() {
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "usage: competitiondbinit")
		os.Exit(2)
	}
	if err := run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "competitiondbinit: %v\n", err)
		os.Exit(1)
	}
}

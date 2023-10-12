package bringyour


import (
	"context"
    "testing"

    // "github.com/go-playground/assert/v2"
)


func TestApplyDbMigrations(t *testing.T) { (&TestEnv{ApplyDbMigrations:false}).Run(func() {
	ctx := context.Background()

	ApplyDbMigrations(ctx)
})}

package model


import (
    "context"
    "testing"

    "bringyour.com/bringyour"
    // "github.com/go-playground/assert/v2"
)


func TestAddDefaultLocations(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    AddDefaultLocations(ctx, 10)
})}


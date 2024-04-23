package jwt


import (
    "context"
    "testing"

    "github.com/go-playground/assert/v2"

    // "github.com/golang/glog"

    "bringyour.com/bringyour"
)


func TestGoogleJwk(t *testing.T) { (&bringyour.TestEnv{ApplyDbMigrations:false}).Run(func() {
    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()

    appleJwkValidator := NewGoogleJwkValidator(ctx)

    keys := appleJwkValidator.Keys()
    assert.NotEqual(t, 0, len(keys))
})}

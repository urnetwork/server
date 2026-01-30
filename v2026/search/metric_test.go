package search

import (
	// "context"
	// "crypto/rand"
	// "encoding/base64"
	// mathrand "math/rand"
	"testing"

	// "sort"
	// "fmt"
	// "slices"

	// "golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2026"
)

func TestEditDistance(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(func() {

		assert.Equal(t, EditDistance("hi", "hi"), 0)
		assert.Equal(t, EditDistance("hi", "ho"), 1)
		assert.Equal(t, EditDistance("hi", "bi"), 1)
		assert.Equal(t, EditDistance("hi", "b"), 2)
		assert.Equal(t, EditDistance("hi", ""), 2)
		assert.Equal(t, EditDistance("eenwoo", "greenwood"), 3)
		assert.Equal(t, EditDistance("a", "abcdefghijklmnop"), 15)

	})
}

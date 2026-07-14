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

	// "maps"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestEditDistance(t *testing.T) {
	(&server.TestEnv{ApplyDbMigrations: false}).Run(t, func(t testing.TB) {

		connect.AssertEqual(t, EditDistance("hi", "hi"), 0)
		connect.AssertEqual(t, EditDistance("hi", "ho"), 1)
		connect.AssertEqual(t, EditDistance("hi", "bi"), 1)
		connect.AssertEqual(t, EditDistance("hi", "b"), 2)
		connect.AssertEqual(t, EditDistance("hi", ""), 2)
		connect.AssertEqual(t, EditDistance("eenwoo", "greenwood"), 3)
		connect.AssertEqual(t, EditDistance("a", "abcdefghijklmnop"), 15)

	})
}

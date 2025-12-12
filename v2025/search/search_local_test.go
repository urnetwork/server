package search

import (
	"context"
	// "crypto/rand"
	// "encoding/base64"
	// mathrand "math/rand"
	"testing"

	// "sort"
	// "fmt"
	// "slices"

	// "golang.org/x/exp/maps"

	// "github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2025"
)

func TestSearchSubstringLocal(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testSearch := NewSearchLocalWithDefaults(
			ctx,
			NewSearchDb("test", SearchTypeSubstring),
		)
		testSearch.WaitForInitialSync(ctx)

		searchSubstring(t, ctx, testSearch)
	})
}

func TestSearchSubstringRandomLocal(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		d := 3
		minAliasLength := d + 1
		testSearch := NewSearchLocalWithDefaults(
			ctx,
			NewSearchDbWithMinAliasLength("test", SearchTypeSubstring, minAliasLength),
		)
		testSearch.WaitForInitialSync(ctx)

		searchSubstringRandom(t, ctx, testSearch, d)
	})
}

func TestSearchSubstringLocalNoWait(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testSearch := NewSearchLocalWithDefaults(
			ctx,
			NewSearchDb("test", SearchTypeSubstring),
		)

		searchSubstring(t, ctx, testSearch)
	})
}

func TestSearchSubstringRandomLocalNoWait(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		d := 3
		minAliasLength := d + 1
		testSearch := NewSearchLocalWithDefaults(
			ctx,
			NewSearchDbWithMinAliasLength("test", SearchTypeSubstring, minAliasLength),
		)

		searchSubstringRandom(t, ctx, testSearch, d)
	})
}

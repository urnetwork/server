package search

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	mathrand "math/rand"
	"testing"

	// "sort"
	"fmt"
	"slices"

	"golang.org/x/exp/maps"

	"github.com/go-playground/assert/v2"

	"github.com/urnetwork/server/v2026"
)

func TestSearchSubstring(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		testSearch := NewSearchDb("test", SearchTypeSubstring)

		searchSubstring(t, ctx, testSearch)
	})
}

func searchSubstring(t *testing.T, ctx context.Context, testSearch Search) {
	id1 := server.NewId()
	id2 := server.NewId()

	testSearch.AddRaw(ctx, "redwood city, california", id1, 0)
	testSearch.AddRaw(ctx, "redwood city, us", id1, 1)
	testSearch.AddRaw(ctx, "redwood city, united states", id1, 2)
	testSearch.AddRaw(ctx, "london, england", id2, 0)
	testSearch.AddRaw(ctx, "london, gb", id2, 1)
	testSearch.AddRaw(ctx, "london, great britain", id2, 2)

	results := testSearch.AroundRaw(ctx, "wood", 0)
	resultValueIds := []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id1})

	results = testSearch.AroundRaw(ctx, "united", 0)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id1})

	results = testSearch.AroundRaw(ctx, "redwood city", 0)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id1})

	results = testSearch.AroundRaw(ctx, "united", 0)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id1})

	results = testSearch.AroundRaw(ctx, "london", 0)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id2})

	// test with some misspelling threshold
	results = testSearch.AroundRaw(ctx, "redwd cit", 3)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id1})

	results = testSearch.AroundRaw(ctx, "lomdom", 3)
	resultValueIds = []server.Id{}
	for _, result := range results {
		resultValueIds = append(resultValueIds, result.ValueId)
	}
	assert.Equal(t, resultValueIds, []server.Id{id2})
}

func TestSearchSubstringRandom(t *testing.T) {
	server.DefaultTestEnv().Run(func() {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		d := 3
		minAliasLength := d + 1
		testSearch := NewSearchDbWithMinAliasLength("test", SearchTypeSubstring, minAliasLength)

		searchSubstringRandom(t, ctx, testSearch, d)
	})
}

func searchSubstringRandom(t *testing.T, ctx context.Context, testSearch Search, d int) {
	// number of values
	n := 500
	// number of sub tests
	k := 100

	minAliasLength := testSearch.MinAliasLength()

	values := map[server.Id]string{}

	for i := 0; i < n; i += 1 {
		b := make([]byte, 8+mathrand.Intn(20))
		_, err := rand.Read(b)
		server.Raise(err)

		value := base64.StdEncoding.EncodeToString(b)
		valueId := server.NewId()

		values[valueId] = value
	}

	for i, valueId := range maps.Keys(values) {
		value := values[valueId]
		fmt.Printf("[%d/%d] Adding search string\n", i+1, n)
		testSearch.AddRaw(ctx, value, valueId, 0)
	}

	for i := 0; i < k; i += 1 {
		valueIds := maps.Keys(values)
		valueId := valueIds[mathrand.Intn(len(valueIds))]
		value := values[valueId]
		start := mathrand.Intn(len(value) / 2)
		end := start + mathrand.Intn(len(value)+1-start)
		sub := value[start:end]

		stats := OptStats()
		results := testSearch.AroundRaw(ctx, sub, d, stats)
		resultValueIds := []server.Id{}
		for _, result := range results {
			resultValueIds = append(resultValueIds, result.ValueId)
		}

		scanCandidateCount := 0

		scanValueIds := []server.Id{}
		for _, scanValueId := range valueIds {
			scanValue := values[scanValueId]
		SubScan:
			for p := 0; p < len(scanValue); p += 1 {
				for q := p + 1; q <= len(scanValue); q += 1 {
					scanSub := scanValue[p:q]
					if len(scanSub) < minAliasLength {
						continue
					}
					scanCandidateCount += 1
					if ed := EditDistance(sub, scanSub); ed <= d {
						scanValueIds = append(scanValueIds, scanValueId)
						break SubScan
					}
				}
			}
		}

		assert.Equal(t, len(resultValueIds), len(scanValueIds))
		slices.SortFunc(resultValueIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		slices.SortFunc(scanValueIds, func(a server.Id, b server.Id) int {
			return a.Cmp(b)
		})
		assert.Equal(t, resultValueIds, scanValueIds)

		// this is a low estimate, since the scan is not finding the best match, just any match
		speedup := float64(scanCandidateCount) / float64(stats.CandidateCount)
		fmt.Printf("[%d/%d] ED compute speedup %.2fx (%d <> %d)\n", i+1, k, speedup, scanCandidateCount, stats.CandidateCount)
	}
}

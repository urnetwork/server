package search


import (
    "context"
    "testing"
    "crypto/rand"
    mathrand "math/rand"
    "encoding/base64"
    // "sort"
    "slices"

    "golang.org/x/exp/maps"

    "github.com/go-playground/assert/v2"

    "bringyour.com/bringyour"
)


func TestEditDistance(t *testing.T) { (&bringyour.TestEnv{ApplyDbMigrations:false}).Run(func() {

	assert.Equal(t, EditDistance("hi", "hi"), 0)
	assert.Equal(t, EditDistance("hi", "ho"), 1)
	assert.Equal(t, EditDistance("hi", "bi"), 1)
	assert.Equal(t, EditDistance("hi", "b"), 2)
	assert.Equal(t, EditDistance("hi", ""), 2)
	assert.Equal(t, EditDistance("eenwoo", "greenwood"), 3)
	assert.Equal(t, EditDistance("a", "abcdefghijklmnop"), 15)
	
})}


func TestSearchSubstring(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    testSearch := NewSearch("test", SearchTypeSubstring)

    id1 := bringyour.NewId()
    id2 := bringyour.NewId()

    testSearch.Add(ctx, "redwood city, california", id1, 0)
    testSearch.Add(ctx, "redwood city, us", id1, 1)
    testSearch.Add(ctx, "redwood city, united states", id1, 2)
    testSearch.Add(ctx, "london, england", id2, 0)
    testSearch.Add(ctx, "london, gb", id2, 1)
    testSearch.Add(ctx, "london, great britain", id2, 2)


    results := testSearch.Around(ctx, "wood", 0)
    resultValueIds := []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id1})


    results = testSearch.Around(ctx, "united", 0)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id1})


    results = testSearch.Around(ctx, "redwood city", 0)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id1})


    results = testSearch.Around(ctx, "united", 0)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id1})


    results = testSearch.Around(ctx, "london", 0)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id2})


    // test with some misspelling threshold
    results = testSearch.Around(ctx, "redwd cit", 3)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id1})


    results = testSearch.Around(ctx, "lomdom", 3)
    resultValueIds = []bringyour.Id{}
    for _, result := range results {
    	resultValueIds = append(resultValueIds, result.ValueId)
    }
    assert.Equal(t, resultValueIds, []bringyour.Id{id2})

})}


func TestSearchSubstringRandom(t *testing.T) { bringyour.DefaultTestEnv().Run(func() {
    ctx := context.Background()

    d := 3
    minAliasLength := d + 1
    testSearch := NewSearchWithMinAliasLength("test", SearchTypeSubstring, minAliasLength)

    // number of values
    n := 500
    // number of sub tests
	k := 100

    values := map[bringyour.Id]string{}

    for i := 0; i < n; i += 1 {
    	b := make([]byte, 8 + mathrand.Intn(20))
        _, err := rand.Read(b)
        bringyour.Raise(err)

    	value := base64.StdEncoding.EncodeToString(b)
    	valueId := bringyour.NewId()

    	values[valueId] = value
    }

    for i, valueId := range maps.Keys(values) {
    	value := values[valueId]
    	bringyour.Logger().Printf("[%d/%d] Adding search string\n", i + 1, n)
	    testSearch.Add(ctx, value, valueId, 0)
	}

	for i := 0; i < k; i += 1 {
		valueIds := maps.Keys(values)
		valueId := valueIds[mathrand.Intn(len(valueIds))]
		value := values[valueId]
		start := mathrand.Intn(len(value) / 2)
		end := start + mathrand.Intn(len(value) + 1 - start)
		sub := value[start:end]

		stats := OptStats()
		results := testSearch.Around(ctx, sub, d, stats)
		resultValueIds := []bringyour.Id{}
	    for _, result := range results {
	    	resultValueIds = append(resultValueIds, result.ValueId)
	    }

	    scanCandidateCount := 0

		scanValueIds := []bringyour.Id{}
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
		slices.SortFunc(resultValueIds, func(a bringyour.Id, b bringyour.Id)(int) {
			return a.Cmp(b)
		})
		slices.SortFunc(scanValueIds, func(a bringyour.Id, b bringyour.Id)(int) {
			return a.Cmp(b)
		})
		assert.Equal(t, resultValueIds, scanValueIds)

		// this is a low estimate, since the scan is not finding the best match, just any match
		speedup := float64(scanCandidateCount) / float64(stats.CandidateCount)
		bringyour.Logger().Printf("[%d/%d] ED compute speedup %.2fx\n", i + 1, k, speedup)
	}
})}
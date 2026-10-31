package model

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"testing"
)

func TestClientScoreCursorRejectsNilAndMalformedSamples(t *testing.T) {
	var encoded bytes.Buffer
	// Invalid payloads and valid empty samples retain distinct outcomes.
	if err := gob.NewEncoder(&encoded).Encode([]*ClientScore{}); err != nil {
		t.Fatal(err)
	}
	if sample, err := decodeClientScoreSample(encoded.Bytes()); err != nil || len(sample) != 0 {
		t.Fatalf("encoded empty sample rejected: %v", err)
	}
	if _, err := decodeClientScoreSample([]byte("not-a-gob-sample")); err == nil {
		t.Fatal("malformed sample became an empty candidate pool")
	}
	if err := validateClientScoreSample([]*ClientScore{nil}); err == nil {
		t.Fatal("nil provider reached score merging")
	}
	if err := validateClientScoreSample([]*ClientScore{onlineBackfillScore(true, 1)}); err != nil {
		t.Fatal("valid provider sample rejected")
	}
}

// Whole-page rounding is shared across turns, not a new allowance on every
// refill, and previously consumed pages are never selected again.
func TestClientScoreCursorRefillKeepsFiniteUnreadPageBudget(t *testing.T) {
	cursor := &clientScoreCursor{}
	for index := range 100 {
		cursor.pages = append(cursor.pages, clientScorePage{key: fmt.Sprint(index), expectedCount: ClientScoreSampleCount})
	}
	seen := map[string]bool{}
	reads := 0
	for {
		limit := findProviders2RefillLoadCount(20, 0, cursor.readCount)
		additional := limit - cursor.readCount
		if additional <= 0 {
			break
		}
		pages := cursor.take(additional)
		for _, page := range pages {
			if seen[page.key] {
				t.Fatal("refill repeated a previously rejected page")
			}
			seen[page.key] = true
		}
		reads++
	}
	maximum := findProviders2LoadCount(20, 2400)
	if cursor.readCount < maximum || maximum+ClientScoreSampleCount <= cursor.readCount || cursor.nextPage != len(seen) || !cursor.hasMore() {
		t.Fatalf("finite refill budget changed: rows=%d pages=%d cap=%d", cursor.readCount, cursor.nextPage, maximum)
	}
	if reads != 9 {
		t.Fatalf("bounded all-rejected refill rounds=%d want=9", reads)
	}
}

func TestClientScoreCursorPreservesFacetOrderAndZeroPages(t *testing.T) {
	cursor := newClientScoreCursor([]map[string]int{{"preferred": 1000, "empty": 0}, {"fallback": 20}}, nil)
	first := cursor.take(1000)
	if len(first) != 1 || first[0].key != "preferred" || cursor.readCount != 1000 {
		t.Fatal("initial preferred sample changed")
	}
	next := cursor.take(findProviders2RefillLoadCount(20, 0, 1000) - cursor.readCount)
	if len(next) != 1 || next[0].key != "fallback" || cursor.hasMore() || cursor.readCount != 1020 {
		t.Fatal("refill missed the unread permitted facet")
	}
}

func TestFindProviders2RefillCompensationDoesNotDoubleCountExclusions(t *testing.T) {
	for _, testCase := range []struct{ explicit, discarded, want int }{
		{0, 0, 1000}, {1000, 1000, 1020}, {1000, 1200, 1220},
		{1980, 1000, 2000}, {0, 1_000_000, 2420}, {1_000_000, 0, 2420},
	} {
		if got := findProviders2RefillLoadCount(20, testCase.explicit, testCase.discarded); got != testCase.want {
			t.Fatalf("explicit=%d discarded=%d budget=%d want=%d", testCase.explicit, testCase.discarded, got, testCase.want)
		}
	}
}

// Planning a 100k-provider market still reads only the bounded initial sample
// or bounded refill allowance. This measures cursor overhead, not Redis or
// end-to-end API latency.
func BenchmarkClientScoreCursor100kProviderMarket(b *testing.B) {
	counts := map[string]int{}
	for index := range 100_000 / ClientScoreSampleCount {
		counts[fmt.Sprint(index)] = ClientScoreSampleCount
	}
	for _, refill := range []bool{false, true} {
		b.Run(fmt.Sprintf("refill=%t", refill), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				cursor := newClientScoreCursor([]map[string]int{counts}, nil)
				cursor.take(1000)
				if refill {
					for {
						additional := findProviders2RefillLoadCount(20, 0, cursor.readCount) - cursor.readCount
						if additional <= 0 {
							break
						}
						cursor.take(additional)
					}
				}
				if cursor.readCount > 2600 {
					b.Fatal("cursor exceeded the existing compensated page allowance")
				}
			}
		})
	}
}

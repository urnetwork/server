package search

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"

	"github.com/urnetwork/server/v2026"
)

func snapshotTestSearch(parallel int) *SearchLocal {
	return snapshotTestSearchType(parallel, SearchTypePrefix, 3)
}

func snapshotTestSearchType(parallel int, searchType SearchType, minAliasLength int) *SearchLocal {
	ctx, cancel := context.WithCancel(context.Background())
	initial, done := context.WithCancel(ctx)
	done()
	return &SearchLocal{
		ctx: ctx, cancel: cancel, initialSync: initial,
		impl:                      NewSearchDbWithMinAliasLength("synthetic_snapshot", searchType, minAliasLength),
		settings:                  &SearchLocalSettings{ParallelCount: parallel},
		valueIdVariantProjections: map[server.Id]map[int]*localProjection{},
	}
}

// Check the public search against a complete alias scan, including the best
// distance and alias metadata. Equal-distance alias selection has always been
// unordered; every returned choice must be one of the complete scan's ties.
func TestSearchLocalAllAliasesMatchBruteForce(t *testing.T) {
	values := []string{
		"san jose, california", "san juan, puerto rico", "new york, us",
		"london, england", "alpha beta alpha", "beta alpha beta", "",
		"a", "café", "a\u0301", "界a界", "\xffab",
	}
	queries := []string{"", "a", "san", "sna", "jose", "new yo", "londn", "alhpa", "alpha", "cafe", "café", "界", "\xffa", "unmatched"}
	for _, searchType := range []SearchType{SearchTypeFull, SearchTypePrefix, SearchTypeSubstring} {
		for _, parallel := range []int{1, 8} {
			t.Run(fmt.Sprintf("%s/%d", searchType, parallel), func(t *testing.T) {
				s := snapshotTestSearchType(parallel, searchType, 1)
				defer s.Close()
				aliases := map[server.Id][]SearchResult{}
				for i, value := range values {
					id := server.Id{}
					id[15] = byte(i/2 + 1)
					variant := i % 2
					s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: id, ValueVariant: variant, Value: value}})
					seen := map[string]bool{}
					for _, alias := range GenerateAliases(value, searchType, 1) {
						if seen[alias.Value] {
							continue
						}
						seen[alias.Value] = true
						aliases[id] = append(aliases[id], SearchResult{Value: value, ValueVariant: variant, Alias: alias.Alias, AliasValue: alias.Value, ValueId: id})
					}
				}
				for _, query := range queries {
					for distance := 0; distance <= 3; distance++ {
						want := map[server.Id][]SearchResult{}
						for id, candidates := range aliases {
							best := distance + 1
							for _, candidate := range candidates {
								candidate.ValueDistance = EditDistance(query, candidate.AliasValue)
								if candidate.ValueDistance < best {
									best = candidate.ValueDistance
									want[id] = []SearchResult{candidate}
								} else if candidate.ValueDistance == best && best <= distance {
									want[id] = append(want[id], candidate)
								}
							}
						}
						got := s.AroundIdsRaw(context.Background(), query, distance)
						if len(got) != len(want) {
							t.Fatalf("query=%q distance=%d result count=%d, want %d", query, distance, len(got), len(want))
						}
						for id, result := range got {
							if !slices.Contains(want[id], *result) {
								t.Fatalf("query=%q distance=%d returned an alias outside the complete best-match set: %+v", query, distance, result)
							}
						}
					}
					if got := s.AnyAround(context.Background(), query, 2); got != (len(s.AroundIdsRaw(context.Background(), query, 2)) != 0) {
						t.Fatalf("query=%q existence search disagreed with the full search", query)
					}
				}
			})
		}
	}
}

func TestSearchLocalSnapshotPreservesMatchesAndLimit(t *testing.T) {
	for _, parallel := range []int{1, 8} {
		s := snapshotTestSearch(parallel)
		t.Cleanup(s.Close)
		ids := make([]server.Id, 45)
		for i := range ids {
			ids[i][15] = byte(i + 1)
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: ids[i], Value: fmt.Sprintf("alpha town %02d", i)}})
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: ids[i], ValueVariant: 1, Value: fmt.Sprintf("zebra town %02d", i)}})
		}
		check := func(query string, distance, limit int, want []server.Id) {
			t.Helper()
			got := s.AroundIds(context.Background(), query, distance, OptMostLikley(limit))
			gotIds := make([]server.Id, 0, len(got))
			for id, r := range got {
				if r.ValueDistance != distance {
					t.Fatalf("parallel=%d query=%q distance=%d, want %d", parallel, query, r.ValueDistance, distance)
				}
				gotIds = append(gotIds, id)
			}
			slices.SortFunc(gotIds, func(a, b server.Id) int { return a.Cmp(b) })
			if !slices.Equal(gotIds, want) {
				t.Fatalf("parallel=%d query=%q selected a different result set", parallel, query)
			}
		}
		check("alpha", 0, 5, ids) // All exact matches survive the optional limit.
		check("alhpa", 2, 30, ids[:30])
		s.index(&SearchValueUpdate{Remove: true, SearchValue: SearchValue{ValueId: ids[0]}})
		s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: ids[1], Value: "omega town"}})
		check("alpha", 0, 5, ids[2:])
		check("alhpa", 2, 30, ids[2:32])
		check("zebra", 0, 5, ids[1:]) // Replacing one variant retains the others.
	}
}

// Real lookups run concurrently with replacement and removal of another value.
// The stable entry must stay searchable in both serial and parallel readers;
// -race also checks that a query's released-lock snapshot remains immutable.
func TestSearchLocalConcurrentSnapshots(t *testing.T) {
	for _, parallel := range []int{1, 8} {
		t.Run(fmt.Sprint(parallel), func(t *testing.T) {
			s := snapshotTestSearch(parallel)
			defer s.Close()
			stable, changing := server.NewId(), server.NewId()
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: stable, Value: "stable location"}})
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: changing, Value: "changing location"}})
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := range 500 {
					s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: changing, ValueVariant: i % 3, Value: fmt.Sprintf("changing location %d", i)}})
					if i%7 == 0 {
						s.index(&SearchValueUpdate{Remove: true, SearchValue: SearchValue{ValueId: changing}})
					}
				}
			}()
			for range 4 {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					for range 100 {
						results := s.AroundIds(context.Background(), "stable", 2)
						if results[stable] == nil || results[stable].Value != "stable location" {
							t.Error("concurrent update lost the stable searchable value")
							return
						}
					}
				}()
			}
			close(start)
			wg.Wait()
		})
	}
}

// Synthetic allocation/CPU control, with no PostgreSQL or Redis work. The
// corpus is intentionally finite; its numbers are not Main latency estimates.
func BenchmarkSearchLocalLookup(b *testing.B) {
	s := snapshotTestSearch(8)
	defer s.Close()
	for i := range 20_000 {
		id := server.NewId()
		for variant := range 2 {
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: id, ValueVariant: variant, Value: fmt.Sprintf("synthetic %05d variant %d", i, variant)}})
		}
	}
	for _, query := range []string{"unmatched", "synthetic 123"} {
		b.Run(query, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s.AroundIds(context.Background(), query, 2, OptMostLikley(30))
			}
		})
	}
}

// An optional corpus of real city/region/country search strings may be
// generated locally from the already licensed GeoLite file. No IPs or user
// records belong in this corpus, and no external service is contacted.
func BenchmarkSearchLocalLocationNames(b *testing.B) {
	path := os.Getenv("FP2_LOCATION_NAMES_PATH")
	if path == "" {
		b.Skip("optional local location-name corpus")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	var names [][]string
	if err := json.Unmarshal(data, &names); err != nil {
		b.Fatal(err)
	}
	if len(names) < 10_000 || 100_000 < len(names) {
		b.Fatal("location corpus must contain 10000 to 100000 entries")
	}
	s := snapshotTestSearch(8)
	defer s.Close()
	for _, variants := range names {
		id := server.NewId()
		for variant, name := range variants {
			s.index(&SearchValueUpdate{SearchValue: SearchValue{ValueId: id, ValueVariant: variant, Value: NormalizeForSearch(name)}})
		}
	}
	for _, query := range []string{"san", "london", "new yo", "unmatched"} {
		b.Run(query, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				s.AroundIds(context.Background(), query, 2, OptMostLikley(30))
			}
		})
	}
}

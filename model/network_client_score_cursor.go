// A provider request consumes each sampled cache page at most once. Its
// remaining-page cursor permits bounded refill after actual request filters.
package model

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"maps"
	mathrand "math/rand"
	"slices"

	"github.com/redis/go-redis/v9"
	"github.com/urnetwork/server"
)

type clientScorePage struct {
	key           string
	expectedCount int
}

type clientScoreCursor struct {
	pages       []clientScorePage
	nextPage    int
	readCount   int
	observation *findProviders2LoadObservation
}

// Facet priority is unchanged. Within a facet, shuffle once for the whole
// request so an added read cannot revisit already-rejected pages.
func newClientScoreCursor(groups []map[string]int, observation *findProviders2LoadObservation) *clientScoreCursor {
	cursor := &clientScoreCursor{observation: observation}
	for _, counts := range groups {
		keys := slices.Collect(maps.Keys(counts))
		mathrand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
		for _, key := range keys {
			if 0 < counts[key] {
				cursor.pages = append(cursor.pages, clientScorePage{key: key, expectedCount: counts[key]})
			}
		}
	}
	return cursor
}

func (self *clientScoreCursor) hasMore() bool {
	return self != nil && self.nextPage < len(self.pages)
}

// Like the existing loader, the last whole page can exceed the requested row
// budget. Published pages hold at most ClientScoreSampleCount rows, so this
// rounding adds at most 199 rows; it does not reset on a refill.
func (self *clientScoreCursor) take(additionalRows int) []clientScorePage {
	start, rows := self.nextPage, 0
	for rows < additionalRows && self.hasMore() {
		page := self.pages[self.nextPage]
		rows += page.expectedCount
		self.nextPage++
	}
	self.readCount += rows
	return self.pages[start:self.nextPage]
}

func (self *clientScoreCursor) read(ctx context.Context, additionalRows int) (scores map[server.Id]*ClientScore, returnErr error) {
	server.Redis(ctx, func(r server.RedisClient) {
		scores, returnErr = self.readWithClient(ctx, r, additionalRows)
	})
	return
}

func (self *clientScoreCursor) readWithClient(ctx context.Context, r server.RedisClient, additionalRows int) (map[server.Id]*ClientScore, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pages := self.take(additionalRows)
	pipe := r.Pipeline()
	commands := make([]*redis.StringCmd, 0, len(pages))
	for _, page := range pages {
		commands = append(commands, pipe.Get(ctx, page.key))
	}
	if err := execClientScoreReadPipeline(ctx, pipe); err != nil {
		clientScoreReadMetrics.samples.Inc()
		return nil, fmt.Errorf("read client score samples: %w", err)
	}
	scores := map[server.Id]*ClientScore{}
	for _, command := range commands {
		data, _ := command.Bytes()
		if len(data) == 0 {
			if self.observation != nil {
				self.observation.missingPages++
			}
			continue
		}
		sample, err := decodeClientScoreSample(data)
		if err != nil {
			return nil, err
		}
		mergeClientScoreSample(scores, sample)
	}
	return scores, nil
}

// Reuse the existing finite exclusion allowance. Already-known explicit
// exclusions and actually discarded/duplicate rows overlap, so take the max
// rather than charging the same exclusion twice. The initial sample remains
// unchanged; successful requests perform no refill.
func findProviders2RefillLoadCount(count, explicitExclusions, discardedRows int) int {
	return findProviders2LoadCount(count, max(explicitExclusions, discardedRows))
}

// Copies from overlapping requested targets can share a provider. Retain a
// location-bearing copy when the other came from a group cache.
func mergeClientScoreSample(scores map[server.Id]*ClientScore, sample []*ClientScore) {
	for _, score := range sample {
		if existing := scores[score.ClientId]; existing != nil && existing.CountryLocationId != nil && score.CountryLocationId == nil {
			continue
		}
		scores[score.ClientId] = score
	}
}

func decodeClientScoreSample(data []byte) ([]*ClientScore, error) {
	var sample []*ClientScore
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&sample); err != nil {
		return nil, err
	}
	if err := validateClientScoreSample(sample); err != nil {
		return nil, err
	}
	return sample, nil
}

func validateClientScoreSample(sample []*ClientScore) error {
	for _, score := range sample {
		if score == nil {
			return fmt.Errorf("client score sample contains a nil provider")
		}
	}
	return nil
}

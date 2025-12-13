package search

import (
	"context"
	"sync"
	"time"

	"golang.org/x/exp/maps"
	// "golang.org/x/sync/semaphore"

	"github.com/urnetwork/glog/v2025"

	"github.com/urnetwork/server/v2025"
)

// search local provides a local search implementation around an upstream index
// the implementation is basic with parallel scans and fast rejects

func DefaultSearchLocalSettings() *SearchLocalSettings {
	return &SearchLocalSettings{
		UpdatePollTimeout: 15 * time.Second,
		ParallelCount:     8,
	}
}

type SearchLocalSettings struct {
	UpdatePollTimeout time.Duration
	ParallelCount     int
}

type aliasHisto struct {
	alias int
	histo map[rune]int
}

type localProjection struct {
	value        string
	valueId      server.Id
	valueVariant int

	// len -> alias value -> histo
	lenValueHistos map[int]map[string]aliasHisto
}

type SearchLocal struct {
	ctx    context.Context
	cancel context.CancelFunc

	impl     Search
	settings *SearchLocalSettings

	initialSync context.Context

	stateLock sync.RWMutex
	// value id -> variant -> projection
	valueIdVariantProjections map[server.Id]map[int]*localProjection
}

func NewSearchLocalWithDefaults(ctx context.Context, impl Search) *SearchLocal {
	return NewSearchLocal(ctx, impl, DefaultSearchLocalSettings())
}

func NewSearchLocal(ctx context.Context, impl Search, settings *SearchLocalSettings) *SearchLocal {
	cancelCtx, cancel := context.WithCancel(ctx)

	initialSync, initialSyncDone := context.WithCancel(cancelCtx)
	searchLocal := &SearchLocal{
		ctx:                       cancelCtx,
		cancel:                    cancel,
		impl:                      impl,
		initialSync:               initialSync,
		settings:                  settings,
		valueIdVariantProjections: map[server.Id]map[int]*localProjection{},
	}
	go server.HandleError(func() {
		searchLocal.update(initialSyncDone)
	}, cancel)

	return searchLocal
}

func (self *SearchLocal) update(initialSyncDone context.CancelFunc) {
	defer self.cancel()

	updateLimit := 10000

	var startValueId server.Id
	for {
		values := self.OrderedSearchValues(self.ctx, startValueId, updateLimit)
		if len(values) == 0 {
			break
		}
		for _, value := range values {
			self.index(&SearchValueUpdate{
				SearchValue: *value,
			})
		}
		startValueId = values[len(values)-1].ValueId
	}
	initialSyncDone()

	var startUpdateId int64
	for {
		orderedUpdates := self.impl.OrderedSearchRecordsAfter(self.ctx, startUpdateId, updateLimit)

		for _, update := range orderedUpdates {
			self.index(update)
		}
		if 0 < len(orderedUpdates) {
			startUpdateId = orderedUpdates[len(orderedUpdates)-1].UpdateId + 1
		}

		select {
		case <-self.ctx.Done():
			return
		case <-time.After(self.settings.UpdatePollTimeout):
		}
	}
}

func (self *SearchLocal) WaitForInitialSync(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-self.initialSync.Done():
		return true
	}
}

func (self *SearchLocal) Close() {
	self.cancel()
}

func (self *SearchLocal) index(update *SearchValueUpdate) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()

	if update.Remove {
		glog.V(1).Infof("[s][%s][%s]index update[%d] remove\n", self.Realm(), update.ValueId, update.UpdateId)
		delete(self.valueIdVariantProjections, update.ValueId)
	} else {
		// update the value

		glog.V(1).Infof("[s][%s][%s/%d]index update[%d] add %s\n", self.Realm(), update.ValueId, update.ValueVariant, update.UpdateId, server.MaskValue(update.Value))

		lenValueHistos := map[int]map[string]aliasHisto{}

		insertOne := func(value string, alias int) {
			valueHistos, ok := lenValueHistos[len(value)]
			if !ok {
				valueHistos = map[string]aliasHisto{}
				lenValueHistos[len(value)] = valueHistos
			}
			_, ok = valueHistos[value]
			if !ok {
				valueHistos[value] = aliasHisto{
					alias: alias,
					histo: createHisto(value),
				}
			}
		}

		for _, searchAlias := range GenerateAliases(update.Value, self.SearchType(), self.MinAliasLength()) {
			insertOne(searchAlias.Value, searchAlias.Alias)
		}

		p := &localProjection{
			value:          update.Value,
			valueId:        update.ValueId,
			valueVariant:   update.ValueVariant,
			lenValueHistos: lenValueHistos,
		}

		variantProjections, ok := self.valueIdVariantProjections[update.ValueId]
		if !ok {
			variantProjections = map[int]*localProjection{}
			self.valueIdVariantProjections[update.ValueId] = variantProjections
		}
		variantProjections[update.ValueVariant] = p
	}
}

func (self *SearchLocal) Realm() string {
	return self.impl.Realm()
}

func (self *SearchLocal) SearchType() SearchType {
	return self.impl.SearchType()
}

func (self *SearchLocal) MinAliasLength() int {
	return self.impl.MinAliasLength()
}

func (self *SearchLocal) AnyAround(ctx context.Context, query string, distance int) bool {
	select {
	case <-self.initialSync.Done():
		results := self.aroundIdsRawN(ctx, query, distance, 1)
		return 0 < len(results)
	default:
		return self.impl.AnyAround(ctx, query, distance)
	}

}

func (self *SearchLocal) Around(ctx context.Context, query string, distance int, options ...any) []*SearchResult {
	return self.AroundRaw(ctx, NormalizeForSearch(query), distance, options...)
}

func (self *SearchLocal) AroundRaw(ctx context.Context, query string, distance int, options ...any) []*SearchResult {

	select {
	case <-self.initialSync.Done():
		results := self.aroundIdsRawN(ctx, query, distance, 0, options...)
		return maps.Values(results)
	default:
		return self.impl.AroundRaw(ctx, query, distance, options...)
	}
}

func (self *SearchLocal) AroundIds(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult {
	return self.AroundIdsRaw(ctx, NormalizeForSearch(query), distance, options...)
}

func (self *SearchLocal) AroundIdsRaw(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult {

	select {
	case <-self.initialSync.Done():
		return self.aroundIdsRawN(ctx, query, distance, 0, options...)
	default:
		return self.impl.AroundIdsRaw(ctx, query, distance, options...)
	}

}

func (self *SearchLocal) aroundIdsRawN(ctx context.Context, query string, distance int, n int, options ...any) map[server.Id]*SearchResult {
	stats := &SearchStats{}
	limit := &SearchLimit{}
	for _, option := range options {
		switch v := option.(type) {
		case *SearchStats:
			stats = v
		case *SearchLimit:
			limit = v
		}
	}

	queryHisto := createHisto(query)

	resultsCtx, resultsCancel := context.WithCancel(ctx)
	defer resultsCancel()
	go func() {
		select {
		case <-resultsCtx.Done():
		case <-ctx.Done():
			resultsCancel()
		}
	}()

	add := func(results map[server.Id]*SearchResult, variantProjections map[int]*localProjection) (candidateCount int) {
		var r *SearchResult

		addLen := func(i int, minD int) {
			for _, p := range variantProjections {
				for v, h := range p.lenValueHistos[i] {
					if minHistoDistance(queryHisto, h.histo, distance) {
						candidateCount += 1
						d := EditDistance(query, v)
						if d <= distance && (r == nil || d < r.ValueDistance) {
							r = &SearchResult{
								Value:         p.value,
								ValueVariant:  p.valueVariant,
								Alias:         h.alias,
								AliasValue:    v,
								ValueId:       p.valueId,
								ValueDistance: d,
							}
							if d == minD {
								return
							}
						}
					}
				}
			}
		}

		addLen(len(query), 0)
		for i := 1; i <= distance && (r == nil || i < r.ValueDistance); i += 1 {
			if j := len(query) - i; 0 <= j {
				addLen(j, i)
			}
			addLen(len(query)+i, i)
		}

		if r != nil {
			results[r.ValueId] = r
			if 0 < n && n <= len(results) {
				resultsCancel()
			}
		}
		return
	}

	var partitions [][]map[int]*localProjection
	func() {
		self.stateLock.RLock()
		defer self.stateLock.RUnlock()

		if 1 < self.settings.ParallelCount {
			// ceil
			n := (len(self.valueIdVariantProjections) + self.settings.ParallelCount - 1) / self.settings.ParallelCount
			partitions = make([][]map[int]*localProjection, self.settings.ParallelCount)
			i := 0
			for _, variantProjections := range self.valueIdVariantProjections {
				if n <= len(partitions[i]) {
					i += 1
				}
				partitions[i] = append(partitions[i], maps.Clone(variantProjections))
			}
		} else {
			partitions = [][]map[int]*localProjection{
				maps.Values(self.valueIdVariantProjections),
			}
		}
	}()

	var resultsMutex sync.Mutex
	results := map[server.Id]*SearchResult{}

	var wg sync.WaitGroup

	for _, partition := range partitions {
		wg.Add(1)
		go func() {
			defer wg.Done()

			localResults := map[server.Id]*SearchResult{}
			localCandidateCount := 0

			func() {
				for _, variantProjections := range partition {
					select {
					case <-resultsCtx.Done():
						return
					default:
					}
					localCandidateCount += add(localResults, variantProjections)
				}
			}()

			func() {
				resultsMutex.Lock()
				defer resultsMutex.Unlock()

				maps.Copy(results, localResults)
				stats.CandidateCount += localCandidateCount
			}()
		}()
	}

	wg.Wait()

	if 0 < limit.MostLikely {
		results = mostLikely(query, results, limit.MostLikely)
	}

	return results
}

func (self *SearchLocal) Add(ctx context.Context, value string, valueId server.Id, valueVariant int) {
	self.AddRaw(ctx, NormalizeForSearch(value), valueId, valueVariant)
}

func (self *SearchLocal) AddRaw(ctx context.Context, value string, valueId server.Id, valueVariant int) {
	self.index(&SearchValueUpdate{
		SearchValue: SearchValue{
			Value:        value,
			ValueId:      valueId,
			ValueVariant: valueVariant,
		},
	})
	self.impl.AddRaw(ctx, value, valueId, valueVariant)
}

func (self *SearchLocal) AddInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx) {
	self.AddRawInTx(ctx, NormalizeForSearch(value), valueId, valueVariant, tx)
}

func (self *SearchLocal) AddRawInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx) {
	self.index(&SearchValueUpdate{
		SearchValue: SearchValue{
			Value:        value,
			ValueId:      valueId,
			ValueVariant: valueVariant,
		},
	})
	self.impl.AddRawInTx(ctx, value, valueId, valueVariant, tx)
}

func (self *SearchLocal) Remove(ctx context.Context, valueId server.Id) {
	self.index(&SearchValueUpdate{
		Remove: true,
		SearchValue: SearchValue{
			ValueId: valueId,
		},
	})
	self.impl.Remove(ctx, valueId)
}

func (self *SearchLocal) RemoveInTx(ctx context.Context, valueId server.Id, tx server.PgTx) {
	self.index(&SearchValueUpdate{
		Remove: true,
		SearchValue: SearchValue{
			ValueId: valueId,
		},
	})
	self.impl.RemoveInTx(ctx, valueId, tx)
}

func (self *SearchLocal) OrderedSearchRecordsAfter(ctx context.Context, startRecordId int64, limit int) []*SearchValueUpdate {
	return self.impl.OrderedSearchRecordsAfter(ctx, startRecordId, limit)
}

func (self *SearchLocal) OrderedSearchValues(ctx context.Context, startValueId server.Id, limit int) []*SearchValue {
	return self.impl.OrderedSearchValues(ctx, startValueId, limit)
}

func createHisto(v string) map[rune]int {
	h := map[rune]int{}
	for _, r := range v {
		h[r] += 1
	}
	return h
}

func minHistoDistance(a map[rune]int, b map[rune]int, distance int) bool {
	minD := 0
	// a is the larger histo
	if len(a) < len(b) {
		a, b = b, a
	}
	for r, ca := range a {
		cb := b[r]
		if cb < ca {
			// there must be deletion or change of these
			minD += ca - cb
			if distance < minD {
				return false
			}
		}
	}
	return true
}

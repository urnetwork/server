package search

import (
	"context"
	"fmt"
	"strings"

	// "unicode"

	"golang.org/x/exp/maps"

	"github.com/golang/glog"

	"github.com/urnetwork/server"
)

type SearchType string

const (
	SearchTypeFull      SearchType = "full"
	SearchTypePrefix    SearchType = "prefix"
	SearchTypeSubstring SearchType = "substring"
	// todo
	// tokenize the value as words and match the query as a single word
	// SearchTypePrefixWords SearchType = "prefix-words"
	// SearchTypeSubstringWords SearchType = "substring-words"
)

type SearchResult struct {
	Value         string
	ValueVariant  int
	Alias         int
	AliasValue    string
	ValueId       server.Id
	ValueDistance int
}

type SearchProjection struct {
	dims map[rune]int
	dord int
	vlen int
}

func computeProjection(value string) *SearchProjection {
	dims := map[rune]int{}
	for _, dim := range value {
		dims[dim] += 1
	}
	dord := len(dims)
	vlen := len(value)

	return &SearchProjection{
		dims: dims,
		dord: dord,
		vlen: vlen,
	}
}

type SearchStats struct {
	CandidateCount int
}

func OptStats() *SearchStats {
	return &SearchStats{}
}

type SearchValue struct {
	Value        string
	ValueId      server.Id
	ValueVariant int
}

type SearchValueUpdate struct {
	UpdateId int64
	Remove   bool
	SearchValue
}

type Search interface {
	Realm() string
	SearchType() SearchType
	MinAliasLength() int

	AnyAround(ctx context.Context, query string, distance int) bool
	Around(ctx context.Context, query string, distance int, options ...any) []*SearchResult
	AroundRaw(ctx context.Context, query string, distance int, options ...any) []*SearchResult
	AroundIds(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult
	AroundIdsRaw(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult
	Add(ctx context.Context, value string, valueId server.Id, valueVariant int)
	AddRaw(ctx context.Context, value string, valueId server.Id, valueVariant int)
	AddInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx)
	AddRawInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx)
	Remove(ctx context.Context, valueId server.Id)
	RemoveInTx(ctx context.Context, valueId server.Id, tx server.PgTx)

	// return is ordered by update id
	OrderedSearchRecordsAfter(ctx context.Context, startUpdateId int64) []*SearchValueUpdate
	SearchValues(ctx context.Context) []*SearchValue
}

type SearchDb struct {
	realm          string
	searchType     SearchType
	minAliasLength int
}

func NewSearchDb(realm string, searchType SearchType) *SearchDb {
	return NewSearchDbWithMinAliasLength(realm, searchType, 1)
}

func NewSearchDbWithMinAliasLength(realm string, searchType SearchType, minAliasLength int) *SearchDb {
	return &SearchDb{
		realm:          realm,
		searchType:     searchType,
		minAliasLength: minAliasLength,
	}
}

func (self *SearchDb) Realm() string {
	return self.realm
}

func (self *SearchDb) SearchType() SearchType {
	return self.searchType
}

func (self *SearchDb) MinAliasLength() int {
	return self.minAliasLength
}

func (self *SearchDb) AnyAround(ctx context.Context, query string, distance int) bool {
	results := self.Around(ctx, query, distance)
	return 0 < len(results)
}

func (self *SearchDb) Around(ctx context.Context, query string, distance int, options ...any) []*SearchResult {
	results := self.AroundIds(ctx, query, distance, options...)
	return maps.Values(results)
}

func (self *SearchDb) AroundRaw(ctx context.Context, query string, distance int, options ...any) []*SearchResult {
	results := self.AroundIdsRaw(ctx, query, distance, options...)
	return maps.Values(results)
}

func (self *SearchDb) AroundIds(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult {
	return self.AroundIdsRaw(ctx, NormalizeForSearch(query), distance, options)
}

func (self *SearchDb) AroundIdsRaw(ctx context.Context, query string, distance int, options ...any) map[server.Id]*SearchResult {
	stats := OptStats()
	for _, option := range options {
		switch v := option.(type) {
		case *SearchStats:
			stats = v
		}
	}

	sqlParts := []string{}
	// https://github.com/jackc/pgx/issues/387
	sqlArgs := server.PgNamedArgs{}

	maps.Copy(sqlArgs, map[string]any{
		"realm":    self.realm,
		"vlen":     len(query),
		"distance": distance,
	})

	// the logic in the dim filter requires at least one matching character between the strings
	// in the case of `len(query) <= distance` it may be the case that there are no matching characters
	// join in the set of values that could match with no matching characters,
	// which have len [0, vlen]
	if len(query) <= distance {
		sqlParts = append(
			sqlParts,
			`
			SELECT
				search_value.value_id,
				search_value.value_variant,
				search_value.alias,
				search_value.value,
				search_value_original.value
			FROM search_value
			INNER JOIN search_value search_value_original ON 
		        	search_value_original.realm = search_value.realm AND 
		        	search_value_original.value_id = search_value.value_id AND
		        	search_value_original.value_variant = search_value.value_variant AND
		        	search_value_original.alias = 0
			WHERE
				search_value.realm = @realm AND
				search_value.vlen BETWEEN 0 AND @vlen
			`,
		)
	}

	if 0 < len(query) {
		if 0 < len(sqlParts) {
			sqlParts = append(sqlParts, "\nUNION ALL\n")
		}

		projection := computeProjection(query)

		simMin := max(0, projection.vlen-distance)
		simMax := projection.vlen + distance

		maps.Copy(sqlArgs, map[string]any{
			"simMin": simMin,
			"simMax": simMax,
		})

		sqlParts = append(
			sqlParts,
			`
				SELECT
					search_sim_possible.value_id,
					search_sim_possible.value_variant,
					search_sim_possible.alias,
					search_value_alias.value AS alias_value,
					search_value_original.value
				FROM
				(
					SELECT
						value_id,
						value_variant,
						alias,
						SUM(sim) AS sim 
					FROM
					(
			`,
		)

		i := 0
		for dim, dlen := range projection.dims {
			id := func(name string) string {
				return fmt.Sprintf("dim_%d_%s", dim, name)
			}

			elenMin := max(0, projection.vlen+projection.dord+dlen-2*distance)
			elenMax := projection.vlen + projection.dord + dlen + 2*distance
			dordMin := max(0, projection.dord-distance)
			dordMax := projection.dord + distance
			vlenMin := max(0, projection.vlen-distance)
			vlenMax := projection.vlen + distance
			dlenMin := max(0, dlen-distance)
			dlenMax := dlen + distance

			if 0 < i {
				sqlParts = append(sqlParts, "\nUNION ALL\n")
			}
			sqlParts = append(
				sqlParts,
				`
					SELECT
						value_id,
						value_variant,
						alias,
						LEAST(dlen, @`+id("dlen")+`) AS sim
					FROM search_projection
					WHERE
						realm = @realm AND
						dim = @`+id("dim")+` AND
						elen BETWEEN @`+id("elenMin")+` AND @`+id("elenMax")+` AND
						dord BETWEEN @`+id("dordMin")+` AND @`+id("dordMax")+` AND
						vlen BETWEEN @`+id("vlenMin")+` AND @`+id("vlenMax")+` AND 
						dlen BETWEEN @`+id("dlenMin")+` AND @`+id("dlenMax"),
			)
			maps.Copy(sqlArgs, map[string]any{
				id("dlen"):    dlen,
				id("dim"):     dim,
				id("elenMin"): elenMin,
				id("elenMax"): elenMax,
				id("dordMin"): dordMin,
				id("dordMax"): dordMax,
				id("vlenMin"): vlenMin,
				id("vlenMax"): vlenMax,
				id("dlenMin"): dlenMin,
				id("dlenMax"): dlenMax,
			})
			i += 1
		}

		sqlParts = append(
			sqlParts,
			`
		    		) search_sim
		    		GROUP BY value_id, value_variant, alias
		    		HAVING SUM(sim) BETWEEN @simMin AND @simMax
		        ) search_sim_possible
		        INNER JOIN search_value search_value_alias ON
		        	search_value_alias.realm = @realm AND 
		        	search_value_alias.value_id = search_sim_possible.value_id AND
		        	search_value_alias.value_variant = search_sim_possible.value_variant AND
		        	search_value_alias.alias = search_sim_possible.alias AND
		        	ABS(LENGTH(search_value_alias.value) - search_sim_possible.sim) <= @distance
		        INNER JOIN search_value search_value_original ON 
		        	search_value_original.realm = @realm AND 
		        	search_value_original.value_id = search_sim_possible.value_id AND
		        	search_value_original.value_variant = search_sim_possible.value_variant AND
		        	search_value_original.alias = 0
	        `,
		)
	}

	matches := map[server.Id]*SearchResult{}

	server.Db(ctx, func(conn server.PgConn) {
		sql := strings.Join(sqlParts, " ")

		candidateCount := 0

		result, err := conn.Query(ctx, sql, sqlArgs)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				searchResult := &SearchResult{}

				server.Raise(result.Scan(
					&searchResult.ValueId,
					&searchResult.ValueVariant,
					&searchResult.Alias,
					&searchResult.AliasValue,
					&searchResult.Value,
				))

				// fmt.Printf("SEARCH FOUND: %s - %s\n", searchResult.Value, searchResult.AliasValue)

				minSearchResult, ok := matches[searchResult.ValueId]
				if ok && minSearchResult.ValueDistance == 0 {
					// already have a perfect match, no need to compute any more
					continue
				}

				candidateCount += 1

				searchResult.ValueDistance = EditDistance(query, searchResult.AliasValue)
				// fmt.Printf("SEARCH FOUND SET VALUE DISTANCE: %s - %s = %d\n", searchResult.Value, searchResult.AliasValue, searchResult.ValueDistance)

				if searchResult.ValueDistance <= distance {
					if !ok || searchResult.ValueDistance < minSearchResult.ValueDistance {
						matches[searchResult.ValueId] = searchResult
					}
				}
			}
		})

		stats.CandidateCount = candidateCount
	})

	return matches
}

func (self *SearchDb) Add(ctx context.Context, value string, valueId server.Id, valueVariant int) {
	server.Tx(ctx, func(tx server.PgTx) {
		self.AddInTx(ctx, value, valueId, valueVariant, tx)
	})
}

func (self *SearchDb) AddRaw(ctx context.Context, value string, valueId server.Id, valueVariant int) {
	server.Tx(ctx, func(tx server.PgTx) {
		self.AddRawInTx(ctx, value, valueId, valueVariant, tx)
	})
}

func (self *SearchDb) AddInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx) {
	self.AddRawInTx(ctx, NormalizeForSearch(value), valueId, valueVariant, tx)
}

func (self *SearchDb) AddRawInTx(ctx context.Context, value string, valueId server.Id, valueVariant int, tx server.PgTx) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
    		DELETE FROM search_projection
    		WHERE
    			realm = $1 AND
    			value_id = $2 AND
    			value_variant = $3
		`,
		self.realm,
		valueId,
		valueVariant,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
    		DELETE FROM search_value
    		WHERE
    			realm = $1 AND
				value_id = $2 AND
				value_variant = $3
		`,
		self.realm,
		valueId,
		valueVariant,
	))

	server.BatchInTx(ctx, tx, func(batch server.PgBatch) {
		insertOne := func(value string, alias int) {
			batch.Queue(
				`
		    		INSERT INTO search_value
		    		(realm, value_id, value_variant, value, alias)
		    		VALUES
		    		($1, $2, $3, $4, $5)
	    		`,
				self.realm,
				valueId,
				valueVariant,
				value,
				alias,
			)

			projection := computeProjection(value)
			for dim, dlen := range projection.dims {
				elen := projection.vlen + projection.dord + dlen
				batch.Queue(
					`
			    		INSERT INTO search_projection
			    		(realm, dim, elen, dord, dlen, vlen, value_id, value_variant, alias)
			    		VALUES
			    		($1, $2, $3, $4, $5, $6, $7, $8, $9)
		    		`,
					self.realm,
					dim,
					elen,
					projection.dord,
					dlen,
					projection.vlen,
					valueId,
					valueVariant,
					alias,
				)
			}
		}

		// alias 0 must be the full string
		insertOne(value, 0)
		switch self.searchType {
		case SearchTypeFull:
		case SearchTypePrefix:
			// compute each prefix as a full search alias
			alias := 1
			for i := len(value); 0 <= i; i -= 1 {
				valuePrefix := value[:i]
				if len(valuePrefix) < self.minAliasLength {
					continue
				}
				if len(valuePrefix) == len(value) {
					continue
				}
				insertOne(valuePrefix, alias)
				alias += 1
			}
		case SearchTypeSubstring:
			// for each suffix, compute each prefix as a full search alias
			alias := 1
			for i := 0; i < len(value); i += 1 {
				for j := len(value); i < j; j -= 1 {
					valueSub := value[i:j]
					if len(valueSub) < self.minAliasLength {
						continue
					}
					if len(valueSub) == len(value) {
						continue
					}
					insertOne(valueSub, alias)
					alias += 1
				}
			}
		}
	})

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM search_value_update
		WHERE
			realm = $1 AND
			value_id = $2 AND
			value_variant = $3
		`,
		self.realm,
		valueId,
		valueVariant,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO search_value_update
		(realm, value_id, value, value_variant)
		VALUES ($1, $2, $3, $4)
		`,
		self.realm,
		valueId,
		value,
		valueVariant,
	))
}

func (self *SearchDb) Remove(ctx context.Context, valueId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		self.RemoveInTx(ctx, valueId, tx)
	})
}

func (self *SearchDb) RemoveInTx(ctx context.Context, valueId server.Id, tx server.PgTx) {
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM search_projection
			WHERE
				realm = $1 AND
				value_id = $2
		`,
		self.realm,
		valueId,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM search_value
			WHERE
				realm = $1 AND
				value_id = $2
		`,
		self.realm,
		valueId,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		DELETE FROM search_value_update
		WHERE
			realm = $1 AND
			value_id = $2
		`,
		self.realm,
		valueId,
	))

	server.RaisePgResult(tx.Exec(
		ctx,
		`
		INSERT INTO search_value_update
		(realm, value_id, remove)
		VALUES ($1, $2, true)
		`,
		self.realm,
		valueId,
	))
}

func (self *SearchDb) OrderedSearchRecordsAfter(ctx context.Context, startUpdateId int64) (updates []*SearchValueUpdate) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				update_id,
		        value_id,
		        value_variant,
		        value,
		        remove
			FROM search_value_update
			WHERE
				realm = $1 AND
				$2 <= update_id
			`,
			self.realm,
			startUpdateId,
		)

		updates = []*SearchValueUpdate{}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var updateId int64
				var valueId server.Id
				var valueVariant *int
				var value *string
				var remove bool
				server.Raise(result.Scan(
					&updateId,
					&valueId,
					&valueVariant,
					&value,
					&remove,
				))

				if remove {
					update := &SearchValueUpdate{
						UpdateId: updateId,
						Remove:   true,
						SearchValue: SearchValue{
							ValueId: valueId,
						},
					}
					updates = append(updates, update)
				} else if valueVariant != nil && value != nil {
					update := &SearchValueUpdate{
						UpdateId: updateId,
						SearchValue: SearchValue{
							ValueId:      valueId,
							ValueVariant: *valueVariant,
							Value:        *value,
						},
					}
					updates = append(updates, update)
				} else {
					glog.Infof("[s][%s]update[%d] must have value variant and value. Malformed record, will skip.", valueId, updateId)
				}
			}
		})
	})

	return
}

func (self *SearchDb) SearchValues(ctx context.Context) (values []*SearchValue) {
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
			SELECT
				value_id,
		        value_variant,
		        value
			FROM search_value
			WHERE
				realm = $1 AND
				alias = 0
			`,
			self.realm,
		)

		values = []*SearchValue{}

		server.WithPgResult(result, err, func() {
			for result.Next() {
				var value SearchValue
				server.Raise(result.Scan(
					&value.ValueId,
					&value.ValueVariant,
					&value.Value,
				))
				values = append(values, &value)
			}
		})
	})

	return
}

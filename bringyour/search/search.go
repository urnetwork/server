package search

import (
	"fmt"
	"context"
	"strings"
	// "sort"

	"golang.org/x/exp/maps"

	"bringyour.com/bringyour"
	"bringyour.com/bringyour/ulid"
)


type SearchType string

const (
	SearchTypeFull SearchType = "full"
	SearchTypePrefix SearchType = "prefix"
	SearchTypeSubstring SearchType = "substring"
	// todo
	// tokenize the value as words and match the query as a single word
	// SearchTypePrefixWords SearchType = "prefix-words"
	// SearchTypeSubstringWords SearchType = "substring-words"
)



type SearchResult struct {
	Value string
	Alias int
	ValueAlias string
	ValueId ulid.ULID
	ValueDistance int
}

type SearchProjection struct {
	dims map[rune]int
	dord int
	vlen int
	// elen int
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


type Search struct {
	realm string
	searchType SearchType
}

func NewSearch(realm string, searchType SearchType) *Search {
	return &Search{
		realm: realm,
		searchType: searchType,
	}
}

func (self *Search) AnyAround(query string, distance int) bool {
	results := self.Around(query, distance)
	return 0 < len(results)
}

func (self *Search) AroundIds(query string, distance int) map[Id]*SearchResult {
	results := map[Id]*SearchResult{}
	for _, result := range self.Around(query, distance) {
		results[result.ValueId] = result
	}
	return results
}

func (self *Search) Around(query string, distance int) []*SearchResult {
	projection := computeProjection(query)

	sqlParts := []string{}
	sqlArgs := bringyour.PgNamedArgs{}
	// https://github.com/jackc/pgx/issues/387

	sqlParts = append(
		sqlParts,
		`
			SELECT
				search_sim_possible.value_id,
				search_sim_possible.alias,
				search_value_alias.value AS alias_value,
				search_value.value
			FROM
			(
				SELECT
					value_id,
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

		elenMin := bringyour.MaxInt(0, projection.vlen + projection.dord + dlen - 2 * distance)
		elenMax := projection.vlen + projection.dord + dlen + 2 * distance
		dordMin := bringyour.MaxInt(0, projection.dord - distance)
		dordMax := projection.dord + distance
		vlenMin := bringyour.MaxInt(0, projection.vlen - distance)
		vlenMax := projection.vlen + distance
		dlenMin := bringyour.MaxInt(0, dlen - distance)
		dlenMax := dlen + distance

		if 0 < i {
			sqlParts = append(sqlParts, "UNION ALL")
		}
		sqlParts = append(
			sqlParts,
			`
				SELECT
					value_id,
					alias,
					LEAST(dlen, @` + id("dlen") + `) AS sim
				FROM search_projection
				WHERE
					dim = @` + id("dim") + ` AND
					elen BETWEEN @` + id("elenMin") + ` AND @` + id("elenMax") + ` AND
					dord BETWEEN @` + id("dordMin") + ` AND @` + id("dordMax") + ` AND
					vlen BETWEEN @` + id("vlenMin") + ` AND @` + id("vlenMax") + ` AND 
					dlen BETWEEN @` + id("dlenMin") + ` AND @` + id("dlenMax"),
		)
		maps.Copy(sqlArgs, map[string]any{
			id("dlen"): dlen,
			id("dim"): dim,
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

	simMin := bringyour.MaxInt(0, projection.vlen - distance)
	simMax := projection.vlen + distance

    sqlParts = append(
    	sqlParts,
        `
	    		) search_sim
	    		GROUP BY value_id, alias
	    		HAVING SUM(sim) BETWEEN @simMin AND @simMax
	        ) search_sim_possible
	        INNER JOIN search_value search_value_alias ON 
	        	search_value_alias.value_id = search_sim_possible.value_id AND
	        	search_value_alias.alias = search_sim_possible.alias AND
	        	ABS(LENGTH(search_value_alias.value) - search_sim_possible.sim) <= @distance
	        INNER JOIN search_value ON 
	        	search_value.value_id = search_value_alias.value_id AND
	        	search_value.alias = 0
        `,
    )
    maps.Copy(sqlArgs, map[string]any{
		"simMin": simMin,
		"simMax": simMax,
		// "vlen": projection.vlen,
		"distance": distance,
	})

	matches := map[ulid.ULID]*SearchResult{}

	bringyour.Db(func(context context.Context, conn bringyour.PgConn) {
		sql := strings.Join(sqlParts, " ")

		/*
		debugSql := sql
		orderedKeys := maps.Keys(sqlArgs)
		sort.Strings(orderedKeys)
		for i := len(orderedKeys) - 1; 0 <= i; i -= 1 {
			k := orderedKeys[i]
			v := sqlArgs[k]
			// var s string
			// switch t := v.(type) {
			// case int:
			// 	s = fmt.Sprintf("%d", v)
			// case 
			// }
			pk := fmt.Sprintf("@%s", k)
			fmt.Printf("REPLACE %s\n", pk)
			debugSql = strings.Replace(debugSql, fmt.Sprintf("@%s", k), fmt.Sprintf("%v", v), -1)
		}
		fmt.Printf("%s\n", sql)
		fmt.Printf("%s\n", sqlArgs)
		fmt.Printf("%s\n", debugSql)
		*/

    	result, err := conn.Query(
    		context,
    		sql,
    		sqlArgs,
    	)
    	bringyour.With(result, err, func() {
    		var valueIdPg bringyour.PgUUID
    		var alias int
    		var valueAlias string
    		var value string

    		for result.Next() {
    			result.Scan(&valueIdPg, &alias, &valueAlias, &value)

    			valueId := *ulid.FromPg(valueIdPg)

				fmt.Printf("CANDIDATE %s\n", valueAlias)
    			valueDistance := EditDistance(query, valueAlias)
    			if valueDistance <= distance {
    				searchResult, contains := matches[valueId]
	    			if !contains || valueDistance < searchResult.ValueDistance {
	    				matches[valueId] = &SearchResult{
							ValueId: valueId,
							Alias: alias,
							ValueAlias: valueAlias,
							Value: value,
							ValueDistance: valueDistance,
						}
	    			}
	    		}
    		}
    	})
    })

    // searchResults := make([]SearchResult, 0, len(matches))
	// for _, searchResult := range matches {
	// 	searchResults = append(searchResults, searchResult)
	// }
	// return searchResults
	return maps.Values(matches)
}

func (self *Search) Add(ctx context.Context, value string, valueId ulid.ULID) {
    bringyour.Tx(ctx, func(tx bringyour.PgTx) {
    	self.AddInTx(ctx, value, valueId, tx)
    })
}

func (self *Search) AddInTx(ctx context.Context, value string, valueId ulid.ULID, tx bringyour.PgTx) {
	var err error

	_, err = tx.Exec(
		context,
		`
    		DELETE FROM search_projection
    		USING search_value
    		WHERE
    			search_projection.value_id = search_value.value_id AND
    			search_value.realm = $1 AND
    			search_value.value = $2 AND
    			search_value.alias = 0
		`,
		self.realm,
		value,
	)
	bringyour.Raise(err)

	_, err = tx.Exec(
		context,
		`
    		DELETE FROM search_value a
    		USING search_value b
    		WHERE
    			a.value_id = b.value_id AND
    			b.realm = $1 AND
    			b.value = $2 AND
    			b.alias = 0
		`,
		self.realm,
		value,
	)
	bringyour.Raise(err)

	insertOne := func(value string, alias int) {
		_, err = tx.Exec(
			context,
    		`
	    		INSERT INTO search_value
	    		(realm, value_id, value, alias)
	    		VALUES
	    		($1, $2, $3, $4)
    		`,
    		self.realm,
    		ulid.ToPg(&valueId),
    		value,
    		alias,
    	)
    	bringyour.Raise(err)

    	projection := computeProjection(value)
    	for dim, dlen := range projection.dims {
    		elen := projection.vlen + projection.dord + dlen
	    	_, err = tx.Exec(
	    		context,
	    		`
		    		INSERT INTO search_projection
		    		(realm, dim, elen, dord, dlen, vlen, value_id, alias)
		    		VALUES
		    		($1, $2, $3, $4, $5, $6, $7, $8)
	    		`,
	    		self.realm,
	    		dim,
	    		elen,
	    		projection.dord,
	    		dlen,
	    		projection.vlen,
	    		ulid.ToPg(&valueId),
	    		alias,
	    	)
	    	bringyour.Raise(err)
	    }
	}

	switch self.searchType {
	case SearchTypeFull:
		insertOne(value, 0)
	case SearchTypePrefix:
		// compute each prefix as a full search alias
		alias := 0
		for vlen := len(value) - 1; 0 < vlen; vlen -= 1 {
			valuePrefix := value[:vlen]
			insertOne(valuePrefix, alias)
			alias += 1
		}
	case SearchTypeSubstring:
		// for each suffix, compute each prefix as a full search alias
		alias := 0
		for suffixVlen := len(value); 0 < suffixVlen; suffixVlen -= 1 {
			suffix := value[len(value) - suffixVlen:]
			for vlen := len(suffix); 0 < vlen; vlen -= 1 {
				valuePrefix := suffix[:vlen]
				insertOne(valuePrefix, alias)
				alias += 1
			}
		}
	}
}



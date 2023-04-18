package search

import (
	"golang.org/x/exp/maps"

	"bringyour.com/bringyour/ulid"
)


type SearchType string

const (
	SearchTypeFull SearchType = "full"
	SearchTypePrefix SearchType = "prefix"
	SearchTypeSubstring SearchType = "substring"  
)



type SearchResult struct {
	value string
	id ulid.ULID
	valueDistance int
}

type SearchProjection {
	dims map[rune]int
	dord int
	vlen int
	elen int
}

func computeProjection(value string) *SearchProjection {
    dims := map[rune]int{}
    for _, dim := range value {
    	dims[d] += 1
    }
    dord := len(dims)
    vlen := len(value)
    elen := nlen + dlen + dord

    return &SearchProjection{
    	dims: dims,
    	dord: dord,
    	vlen: vlen,
    	elen: elen,
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

func (self Search) AnyAround(query string, distance int) bool {
	results := self.Around(query, distance)
	return 0 < len(results)
}

func (self Search) Around(query string, distance int) []SearchResult {
	projection := computeProjection(query)

	sqlParts = []string{}
	sqlArgs = map[string]any
	// https://github.com/jackc/pgx/issues/387
	// fixme pgx.NamedArgs{"foo": 1, "bar": 2}

	sqlParts = append(
		sqlParts,
		`
		SELECT
			search_sim_possible.value_id AS value_id,
			search_sim_possible.alias AS alias,
			search_value_alias.value AS alias_value,
			search_value.value AS value
		FROM
		(
			SELECT
				value_id,
				SUM(sim) as sim FROM
			(
		`,
	)

	for i := 0, dim, dlen := range projection.dims; i += 1 {
		elenMin := bringyour.MaxInt(0, projection.vlen + dlen + projection.dord - distance)
		elenMax := projection.vlen + dlen + projection.dord + distance
		dordMin := bringyour.MaxInt(0, projection.dord - distance)
		dordMax := projection.dord + distance
		vlenMin := bringyour.MaxInt(0, projection.vlen - distance)
		vlenMax := projection.vlen + distance
		dlenMin := bringyour.MaxInt(0, dlen - distance)
		dlenMax := dlen + distance
		simMin := bringyour.MaxInt(0, projection.vlen - distance)
		simMax := projection.vlen + k

		if 0 < i {
			sqlParts = append(sqlParts, "UNION ALL")
		}
		sqlParts = append(
			sqlParts,
			`
			SELECT
				value_id,
				alias,
				$1 - ABS($1 - dlen) AS sim
			FROM search_value
			WHERE
				dim = @dim AND
				elen BETWEEN @elenMin AND @elenMax AND
				dord BETWEEN @dordMin AND @dordMax AND
				vlen BETWEEN @vlenMin AND @vlenMax AND 
				dlen BETWEEN @dlenMin AND @dlenMax
			`
		)
		maps.Copy(sqlArgs, map[string]any{
			"dlen": dlen,
			"dim": dim,
			"elenMin": elenMin,
			"elenMax": elenMax,
			"dordMin": dordMin,
			"dordMax": dordMax,
			"vlenMin": vlenMin,
			"vlenMax": vlenMax,
			"dlenMin": dlenMin,
			"dlenMax": dlenMax,
		})
	}

    sqlParts = append(
    	sqlParts,
        `
    		) search_sim
    		GROUP BY value_id, alias
    		HAVING SUM(sim) BETWEEN @simMin AND @simMax
        ) search_sim_possible
        INNER JOIN search_value_alias ON 
        	search_value_alias.value_id = search_sim_possible.value_id AND
        	search_value_alias.alias = search_sim_possible.alias AND
        	ABS(LENGTH(search_value_alias.value) - @vlen) + ABS(@vlen - search_sim_possible.sim) <= @distance
        INNER JOIN search_value ON 
        	search_value.value_id = search_value_alias.value_id AND
        	search_value.alias = 0
        `
    )
    maps.Copy(sqlArgs, map[string]any{
		"simMin": simMin,
		"simMax": simMax,
		"vlen": vlen,
		"distance": distance
	})

	matches := map[ulid.Ulid]*SearchResult{}

	bringyour.Db(func(context context.Context, tx bringyour.PgTx) {
    	result, err := tx.Exec(
    		strings.Join(" ", sqlParts),
    		// fixme
    		sqlArgs...,
    	)
    	bringyour.With(result, err, func() {
    		var valueIdPg bringyour.DbUUID
    		var alias int
    		var valueAlias string
    		var value string

    		for result.Next() {
    			result.Scan(&valueIdPg, &alias, &valueAlias, &value)

    			valueId := ulid.FromPg(valueIdPg)
				
				// use a prefix for a fast reject    			
    			prefixLength := 2 * distance
    			valuePrefixDistance := EditDistance(query[:prefixLength], valueAlias[:prefixLength])
    			if valuePrefixDistance <= distance {
	    			valueDistance := EditDistance(query, valueAlias)
	    			if valueDistance <= distance {
	    				searchResult, contains := matches[valueId]
		    			if !contains || valueDistance < searchResult.distance {
		    				matches[valueId] = &SearchResult{
								valueId: valueId,
								alias: alias,
								valueAlias: valueAlias,
								value: value,
								valueDistance: valueDistance,
							}
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

func (self Search) Add(value string, valueId ulid.ULID) {
    bringyour.Tx(func(context context.Context, tx bringyour.PgTx) {
    	var err error

    	_, err = tx.Exec(
    		`INSERT INTO search_value (realm, value_id, value, alias) VALUES ($1, $2, $3, $4)`,
    		self.realm,
    		bringyour.ToPg(valueId),
    		value,
    		0
    	)
    	if err != nil {
    		// already exists
	    	_, err = tx.Exec(
	    		`
	    		DELETE FROM search_projection
	    		USING search_value
	    		WHERE
	    			search_projection.value_id = search_value.value_id AND
	    			search_value.realm = $1 AND
	    			search_value.value = $2
	    		`,
	    		self.realm,
	    		value,
	    	)
	    	bringyour.Raise(err)
	    	_, err = tx.Exec(
	    		`DELETE FROM search_value WHERE realm = $1 AND value = $1`,
	    		self.realm,
	    		value,
	    	)
	    	bringyour.Raise(err)
	    	_, err = tx.Exec(
	    		`INSERT INTO search_value (realm, value_id, value) VALUES ($1, $2, $3, $4)`,
	    		self.realm,
	    		bringyour.ToPg(valueId),
	    		value,
	    		0
	    	)
	    	bringyour.Raise(err)
    	}

    	switch self.searchType {
    	case SearchTypeFull:
	    	projection := computeProjection(value)
	    	for dim, dlen := range projection.dims {
		    	_, err = tx.Exec(
		    		`
		    		INSERT INTO search_projection
		    		(realm, dim, elen, dord, dlen, vlen, value_id)
		    		VALUES
		    		($1, $2, $3, $4, $5, $6, $7)
		    		`,
		    		self.realm,
		    		dim,
		    		projection.elen,
		    		projection.dord,
		    		dlen,
		    		projection.vlen,
		    		bringyour.ToPg(valueId),
		    	)
		    	bringyour.Raise(err)
		    }
		case SearchTypePrefix:
			// compute each prefix as a full search alias
			alias := 1
			for vlen := 1; vlen <= len(value); vlen += 1 {
				valuePrefix := value[:vlen]
				projection := computeProjection(valuePrefix)
		    	for dim, dlen := range projection.dims; alias += 1 {
			    	_, err = tx.Exec(
			    		`
			    		INSERT INTO search_projection
			    		(realm, dim, elen, dord, dlen, vlen, value_id, alias)
			    		VALUES
			    		($1, $2, $3, $4, $5, $6, $7, $8)
			    		`,
			    		self.realm,
			    		dim,
			    		projection.elen,
			    		projection.dord,
			    		dlen,
			    		projection.vlen,
			    		bringyour.ToPg(valueId),
			    		alias
			    	)
			    	bringyour.Raise(err)
			    }
			}
		case SearchTypeSubstring:
			// for each suffix, compute each prefix as a full search alias
			alias := 1
			for suffixVlen := len(value); 0 < suffixVlen; suffixVlen -= 1 {
				suffix := value[len(value) - suffixVlen:]
				for vlen := len(suffix); 0 < vlen; vlen -= 1 {
					valuePrefix := suffix[:vlen]
					projection := computeProjection(valuePrefix)
					// fixme insert the alias value
			    	for dim, dlen := range projection.dims; alias += 1 {
				    	_, err = tx.Exec(
				    		`
				    		INSERT INTO search_projection
				    		(realm, dim, elen, dord, dlen, vlen, value_id, alias)
				    		VALUES
				    		($1, $2, $3, $4, $5, $6, $7, $8)
				    		`,
				    		self.realm,
				    		dim,
				    		projection.elen,
				    		projection.dord,
				    		dlen,
				    		projection.vlen,
				    		bringyour.ToPg(valueId),
				    		alias
				    	)
				    	bringyour.Raise(err)
				    }
				}
			}
    	}
    	
    })
}

package search

import (
	"unicode"
)

type SearchAlias struct {
	Value string
	Alias int
}

func GenerateAliases(value string, searchType SearchType, minAliasLength int) (searchAliases []SearchAlias) {
	insertOne := func(value string, alias int) {
		searchAliases = append(searchAliases, SearchAlias{
			Value: value,
			Alias: alias,
		})
	}

	// alias 0 must be the full string
	insertOne(value, 0)

	switch searchType {
	case SearchTypeFull:
	case SearchTypePrefix:
		wordStartIndexes := func() []int {
			indexes := []int{}
			boundary := true
			for i, r := range value {
				if unicode.IsSpace(r) {
					boundary = true
				} else if boundary {
					boundary = false
					indexes = append(indexes, i)
				}
			}
			return indexes
		}
		// compute each prefix as a full search alias
		alias := 1
		for _, i0 := range wordStartIndexes() {
			for i := len(value); i0 <= i; i -= 1 {
				valuePrefix := value[i0:i]
				if len(valuePrefix) < minAliasLength {
					continue
				}
				if len(valuePrefix) == len(value) {
					continue
				}
				insertOne(valuePrefix, alias)
				alias += 1
			}
		}
	case SearchTypeSubstring:
		// for each suffix, compute each prefix as a full search alias
		alias := 1
		for i := 0; i < len(value); i += 1 {
			for j := len(value); i < j; j -= 1 {
				valueSub := value[i:j]
				if len(valueSub) < minAliasLength {
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

	return
}

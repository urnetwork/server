package search

import (

	"bringyour.com/bringyour/ulid"
)


type SearchType string

const (
	SearchTypeFull SearchType = "full"
	SearchTypePrefix SearchType = "prefix"
)




type Search struct {

}


func NewSearch(realmName string, searchType SearchType) *Search {
	// fixme
	return nil
}


func (self Search) AnyAround(query string, distance int) bool {
	return false
}

func (self Search) Add(value string, id ulid.ULID) {
	// fixme
}




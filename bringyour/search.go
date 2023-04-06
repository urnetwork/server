

type SearchType string

const (
	SearchTypeFull SearchType = "full"
	SearchTypePrefix SearchType = "prefix"
)


type Search struct {

}


func NewSearch(realmName struct, searchType SearchType) {
	
}


func (self Search) AnyAround(distance int) bool {
	return false
}




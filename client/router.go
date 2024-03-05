package client

/*
// use a linked struct since arrays of structs are not exportable
// (see notes in `client.go`)
type Destination struct {
	Path *Path
	Next *Destination
}

func NewDestination(path *Path) *Destination {
	return &Destination{
		Path: path,
	}
}

func (self *Destination) Insert(path *Path) *Destination {
	return &Destination{
		Path: path,
		Next: self,
	}
}
*/

/*
type Router interface {
	// `provideMode` is the relative provide mode for the destinations
	// e.g. if a public device, `ProvideModePublic`
	SetDestination(destinations *PathList, provideMode ProvideMode) error
}
*/

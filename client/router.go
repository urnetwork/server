package client


type Router interface {
	// `provideMode` is the relative provide mode for the destinations
	// e.g. if a public device, `ProvideModePublic`
	SetDestination(destinations []Path, provideMode ProvideMode) error
}

// The test seam of the request-path judgement of the stale handlers, apart
// from the behavioral tests so those also run against code that predates it.
package model

// Forgets the request-path judgement of the stale handlers
// (defaultLiveStaleNetworkClientHandlers), so the next Quality read judges a
// test's own handlers.
func liveStaleTestReset() {
	defaultLiveStaleNetworkClientHandlers.reset()
}

// Concurrent ownership of the disposable-cluster fixture's master census.
package model

import (
	"fmt"
	"maps"
	"slices"
	"sync"
	"testing"
)

// ForEachMaster invokes its callbacks concurrently. Only this collector owns
// the result slice, including reads after traversal has joined the callbacks.
type livenessTestMasterCensus struct {
	stateLock   sync.Mutex
	masterAddrs []string
}

// Records one successful callback without racing the other masters.
func (self *livenessTestMasterCensus) add(addr string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.masterAddrs = append(self.masterAddrs, addr)
}

// Returns a copy so later callbacks cannot mutate a caller's census.
func (self *livenessTestMasterCensus) addresses() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.masterAddrs)
}

// Every callback waits at one barrier before recording. The race-enabled
// parent detects unsynchronized appends even if the scheduler serializes
// execution after the barrier; the exact membership also catches lost rows.
func TestProviderLivenessMasterCensusJoinsConcurrentCallbacks(t *testing.T) {
	const masterCount = 64
	census := &livenessTestMasterCensus{}
	ready := make(chan struct{}, masterCount)
	release := make(chan struct{})
	var callbacks sync.WaitGroup
	want := map[string]bool{}
	for i := range masterCount {
		addr := fmt.Sprintf("master-%d.example:26379", i)
		want[addr] = true
		callbacks.Add(1)
		go func() {
			defer callbacks.Done()
			ready <- struct{}{}
			<-release
			census.add(addr)
		}()
	}
	for range masterCount {
		<-ready
	}
	close(release)
	callbacks.Wait()
	addrs := census.addresses()
	got := map[string]bool{}
	for _, addr := range addrs {
		got[addr] = true
	}
	if len(addrs) != masterCount || !maps.Equal(got, want) {
		t.Fatalf("master census %v, want every callback's distinct address", addrs)
	}
}

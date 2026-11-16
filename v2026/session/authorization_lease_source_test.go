package session

import (
	"context"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

// The state-query source the caller selects names the lease's final check
// before admission, which is a query of the caller's own admission. A recheck
// is the lease's later work and never counts under the caller's source.
func TestAuthorizationLeaseAttributesOnlyItsAdmissionCheck(t *testing.T) {
	defer Testing_SetRejectExpired(false)()
	synctest.Test(t, func(t *testing.T) {
		var stateLock sync.Mutex
		sources := []StateQuerySource{}
		check := func(ctx context.Context, credential *ByJwt) error {
			stateLock.Lock()
			defer stateLock.Unlock()
			sources = append(sources, stateQuerySourceFromContext(ctx))
			return nil
		}
		ctx := WithStateQuerySource(context.Background(), StateQueryConnectH3)
		lease, err := startAuthorizationLeaseWithCheck(ctx, scriptedLeaseCredential(), func(int) {}, nil, check)
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Close()
		lease.Recheck()
		synctest.Wait()

		stateLock.Lock()
		defer stateLock.Unlock()
		want := []StateQuerySource{StateQueryConnectH3, StateQueryUnknown}
		if !slices.Equal(sources, want) {
			t.Fatalf("check sources = %v, want %v", sources, want)
		}
	})
}

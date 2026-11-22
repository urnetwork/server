package session

// Fault injection for decision 3: Redis dial attempts time out on every one of
// 48 masters at once, the 2026-11-18 pattern (fleet-wide, minute-long bursts
// of "redis: connection pool: failed to dial ... i/o timeout" at 14:18, 16:23,
// 17:09, 17:23 and 17:38Z). The api's authentication of signed client and
// session tokens, connect admission, the hosted parent authority and lease
// rechecks all keep accepting, every acceptance is counted on its path, and no
// check waits past the check budget. Before decision 3 each of these waited
// the two second authentication budget and then refused with 503.

import (
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// Signs and verifies with a synthetic key for the rest of the test, so a
// signed token authenticates without a vault.
func testingUseByJwtTestKeyring(t *testing.T) {
	t.Helper()
	vault := newByJwtTestVault(t)
	clock := newByJwtTestClock()
	writeByJwtTestKey(t, vault, "1.0.0", clock.time.Add(-2*byJwtTestSigningDelay))
	previous := defaultByJwtKeyring
	defaultByJwtKeyring = newByJwtTestKeyring(vault, clock)
	t.Cleanup(func() { defaultByJwtKeyring = previous })
}

func TestRedisDialOutageOnEveryMasterKeepsAuthenticationAccepting(t *testing.T) {
	defer Testing_SetRevocationRecordEnabled(true)()
	defer Testing_SetRejectExpired(false)()
	testingUseByJwtTestKeyring(t)
	stalled, pop := server.Testing_StallAuthRedisDials(48)
	defer pop()

	const networks = 64
	// how long any one caller may take: a budget for each Redis step it makes
	// (an api check and its last-used sample; a connect handshake's final
	// check and its authority publication), plus scheduling. The hosted parent
	// makes one record read, within the hosted path's budget.
	bound := 2*RevocationCheckBudget + time.Second
	bounds := map[string]time.Duration{"hosted parent": HostedRevocationCheckBudget + time.Second}

	clients := make([]*ByJwt, networks)
	children := make([]*ByJwt, networks)
	sessions := make([]*ByJwt, networks)
	for i := range networks {
		clients[i] = testingTaggedClientCredential()
		children[i] = testingTaggedChildCredential(clients[i])
		sessions[i] = testingTaggedSessionCredential()
	}
	counted := func() map[string]float64 {
		return map[string]float64{
			"api client":        testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialClient),
			"api session":       testingUncheckedAcceptances(revocationCheckPathApi, revocationCheckCredentialSession),
			"connect_h1 client": testingUncheckedAcceptances(revocationCheckPathConnectH1, revocationCheckCredentialClient),
			"hosted client":     testingUncheckedAcceptances(revocationCheckPathHosted, revocationCheckCredentialClient),
			"lease client":      testingUncheckedAcceptances(revocationCheckPathLease, revocationCheckCredentialClient),
			"lease unanswered":  testingLeaseUnavailableRechecks(),
		}
	}
	before := counted()

	authenticate := func(credential *ByJwt) error {
		request, err := http.NewRequest(http.MethodPost, "https://api.example.test/connect/control", nil)
		if err != nil {
			return err
		}
		request.Header.Set("Authorization", "Bearer "+credential.Testing_Sign())
		clientSession := NewLocalClientSession(t.Context(), "192.0.2.10:443", nil)
		defer clientSession.Cancel()
		if err := clientSession.Auth(request); err != nil {
			return err
		}
		if clientSession.ByJwt == nil || clientSession.ByJwt.NetworkId != credential.NetworkId {
			return fmt.Errorf("authenticated as %v", clientSession.ByJwt)
		}
		return nil
	}

	var resultLock sync.Mutex
	failures := []string{}
	slowest := map[string]time.Duration{}
	leases := []*AuthorizationLease{}
	var retired sync.Map
	run := func(name string, work func() error) func() {
		return func() {
			start := time.Now()
			err := work()
			elapsed := time.Since(start)
			resultLock.Lock()
			defer resultLock.Unlock()
			if err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", name, err))
			}
			if slowest[name] < elapsed {
				slowest[name] = elapsed
			}
		}
	}
	var wait sync.WaitGroup
	for i := range networks {
		for _, work := range []func(){
			run("api client", func() error { return authenticate(clients[i]) }),
			run("api session", func() error { return authenticate(sessions[i]) }),
			run("connect admission", func() error {
				lease, err := StartConnectionAuthorizationLease(WithStateQuerySource(t.Context(), StateQueryConnectH1), clients[i], server.NewId(), 1, nil, func(code int) { retired.Store(i, code) })
				if err == nil {
					resultLock.Lock()
					leases = append(leases, lease)
					resultLock.Unlock()
				}
				return err
			}),
			run("hosted parent", func() error {
				return ValidateByJwtStateForParent(WithStateQuerySource(t.Context(), StateQueryHostedControl), children[i], *clients[i].ClientId)
			}),
		} {
			wait.Add(1)
			go func() {
				defer wait.Done()
				work()
			}()
		}
	}
	wait.Wait()
	defer func() {
		for _, lease := range leases {
			lease.Close()
		}
	}()
	if 0 < len(failures) {
		t.Fatalf("%d of %d callers were refused during the outage, first: %s", len(failures), 4*networks, failures[0])
	}
	for name, elapsed := range slowest {
		limit := bound
		if hosted, ok := bounds[name]; ok {
			limit = hosted
		}
		if limit < elapsed {
			t.Errorf("the slowest %s took %s, want at most %s", name, elapsed, limit)
		}
	}

	// every admitted transport rechecks now; Redis still does not answer
	for _, lease := range leases {
		lease.Recheck()
	}
	deadline := time.Now().Add(5 * time.Second)
	for counted()["lease client"]-before["lease client"] < networks && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	after := counted()
	for name, want := range map[string]float64{
		"api client":        networks,
		"api session":       networks,
		"connect_h1 client": networks,
		"hosted client":     networks,
	} {
		if got := after[name] - before[name]; got != want {
			t.Errorf("counted %.0f unchecked %s acceptances, want %.0f", got, name, want)
		}
	}
	if got := after["lease client"] - before["lease client"]; got < networks {
		t.Errorf("counted %.0f unchecked lease rechecks, want at least %d", got, networks)
	}
	if got := after["lease unanswered"] - before["lease unanswered"]; got < networks {
		t.Errorf("counted %.0f unanswered lease rechecks, want at least %d", got, networks)
	}
	retired.Range(func(key, value any) bool {
		t.Errorf("transport %v was retired with %v during the outage", key, value)
		return true
	})
	// hundreds of keys on random slots: the outage was every master's
	if got := stalled.DialedMasters(); got < 40 {
		t.Errorf("the checks dialed %d of 48 masters", got)
	}
}

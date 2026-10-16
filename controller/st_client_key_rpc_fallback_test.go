// Client-key authority reads fail over across every configured RPC url. These
// fixtures need no database: the only external party is raw JSON-RPC served on
// loopback, and every failing endpoint fails in a fixed, repeatable way.
package controller

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	gethrpc "github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/protocol"
	"github.com/urfoundation/sn/stabi"
)

// One verified-answer endpoint built from the existing raw-RPC fixture.
func newStClientKeyFallbackEndpoint(t *testing.T, fault string) (*stClientKeyHistoryRPCFixture, string) {
	t.Helper()
	root, err := crypto.HexToECDSA(strings.Repeat("12", 32))
	if err != nil {
		t.Fatal(err)
	}
	domain := protocol.ClientKeyHistoryDomain{ChainID: 945, GenesisHash: [32]byte{1}, Netuid: 521, Coordinator: common.Address{2}, SettlementVault: common.Address{3}, DeploymentIDHash: sha256.Sum256([]byte("key-history-test")), PolicyHash: [32]byte{4}, NoID: 1}
	fixture := &stClientKeyHistoryRPCFixture{domain: domain, boundary: protocol.ClientKeyEffectiveBoundary{Block: 100, Hash: [32]byte{5}}, root: crypto.PubkeyToAddress(root.PublicKey), fault: fault}
	server := gethrpc.NewServer()
	if err := server.RegisterName("eth", fixture); err != nil {
		t.Fatal(err)
	}
	if err := server.RegisterName("chain", fixture); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(server)
	t.Cleanup(func() {
		endpoint.Close()
		server.Stop()
	})
	return fixture, endpoint.URL
}

// Accepts the connection and never answers until the caller gives up, like a
// gateway whose upstream node or VPN path has stalled.
func newStClientKeyStalledEndpoint(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	requests := &atomic.Int32{}
	release := make(chan struct{})
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		endpoint.Close()
	})
	return endpoint.URL, requests
}

// A closed loopback listener refuses every connection.
func newStClientKeyRefusedEndpoint(t *testing.T) string {
	t.Helper()
	endpoint := httptest.NewServer(http.NotFoundHandler())
	url := endpoint.URL
	endpoint.Close()
	return url
}

func newStClientKeyFallbackOwner(t *testing.T, domain protocol.ClientKeyHistoryDomain, urls ...string) *stClientKeyAuthorityOwner {
	t.Helper()
	client := &CoreStClient{cfg: &StConfig{ChainId: domain.ChainID, RpcUrls: urls}, clients: map[string]*ethclient.Client{}}
	t.Cleanup(func() {
		client.stateLock.Lock()
		defer client.stateLock.Unlock()
		for _, rpcClient := range client.clients {
			rpcClient.Close()
		}
	})
	return &stClientKeyAuthorityOwner{domain: domain, client: client, rpcURLs: urls}
}

// Root cause of the 2026-10-14 registration outage class: one stalled endpoint
// held each registration read for the 60 s general attempt budget, longer than
// the caller waits, so a second configured url was never reached. Uses the
// production bound; before the fix this fails with a deadline after 25 s.
func TestStClientKeyAuthorityReadReachesSecondEndpointPastStalledFirst(t *testing.T) {
	fixture, healthy := newStClientKeyFallbackEndpoint(t, "")
	stalled, stalledRequests := newStClientKeyStalledEndpoint(t)
	owner := newStClientKeyFallbackOwner(t, fixture.domain, stalled, healthy)
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
	defer cancel()
	boundary, operator, err := owner.readBoundary(ctx, nil)
	if err != nil {
		t.Fatal("registration read did not reach the second endpoint before the caller's deadline:", err)
	}
	if stalledRequests.Load() == 0 {
		t.Fatal("the first configured endpoint was not tried first")
	}
	want := fixture.boundary
	want.Epoch = boundary.Epoch
	if boundary != want || operator.RootSigner != fixture.root || !operator.Active {
		t.Fatal("fallback read returned values other than the second endpoint's verified authority", boundary, operator)
	}
}

// Connection refusal moves to the next url at once (already true before the
// fix; kept as a guard), and the validator's single observation read at an
// exact historical boundary fails over the same way.
func TestStClientKeyAuthorityReadFailsOverPastRefusedFirst(t *testing.T) {
	fixture, healthy := newStClientKeyFallbackEndpoint(t, "")
	owner := newStClientKeyFallbackOwner(t, fixture.domain, newStClientKeyRefusedEndpoint(t), healthy)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	boundary, operator, err := owner.readBoundary(ctx, nil)
	if err != nil || boundary.Block != fixture.boundary.Block || boundary.Hash != fixture.boundary.Hash || operator.RootSigner != fixture.root {
		t.Fatal("current read did not fail over past a refused endpoint", boundary, operator, err)
	}
	requested := fixture.boundary
	boundary, operator, err = owner.readBoundary(ctx, &requested)
	if err != nil || boundary != requested || operator.RootSigner != fixture.root {
		t.Fatal("historical read did not fail over past a refused endpoint", boundary, operator, err)
	}
}

// An endpoint whose answer fails verification is never accepted or merged
// with another endpoint's answer; the next endpoint must pass every check on
// its own. When every endpoint fails, the read fails closed and names each.
func TestStClientKeyAuthorityReadNeverAcceptsAnUnverifiedEndpoint(t *testing.T) {
	healthyFixture, healthy := newStClientKeyFallbackEndpoint(t, "")
	_, companion := newStClientKeyFallbackEndpoint(t, "companion")
	_, wrongGenesis := newStClientKeyFallbackEndpoint(t, "native-wrong")
	_, wrongChain := newStClientKeyFallbackEndpoint(t, "chain")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	owner := newStClientKeyFallbackOwner(t, healthyFixture.domain, companion, healthy)
	boundary, operator, err := owner.readBoundary(ctx, nil)
	if err != nil || boundary.Hash != healthyFixture.boundary.Hash || operator.RootSigner != healthyFixture.root {
		t.Fatal("a verified second endpoint was not used after the first failed verification", boundary, operator, err)
	}

	owner = newStClientKeyFallbackOwner(t, healthyFixture.domain, wrongGenesis, companion, wrongChain)
	boundary, operator, err = owner.readBoundary(ctx, nil)
	if err == nil || boundary != (protocol.ClientKeyEffectiveBoundary{}) || operator != (stabi.STCoordinatorOperatorVersion{}) {
		t.Fatal("unverified endpoints produced an authority", boundary, operator, err)
	}
	for _, url := range []string{wrongGenesis, companion, wrongChain} {
		if !strings.Contains(err.Error(), url) {
			t.Fatal("the all-endpoints failure does not name", url, err)
		}
	}
	if ctx.Err() != nil {
		t.Fatal("non-retryable verification failures waited for the deadline instead of failing closed")
	}
}

// Every endpoint stalled: each attempt is cut at the per-endpoint bound and
// the read fails closed with the caller's deadline and no authority.
func TestStClientKeyAuthorityReadFailsClosedWhenEveryEndpointStalls(t *testing.T) {
	fixture, _ := newStClientKeyFallbackEndpoint(t, "")
	first, firstRequests := newStClientKeyStalledEndpoint(t)
	second, secondRequests := newStClientKeyStalledEndpoint(t)
	owner := newStClientKeyFallbackOwner(t, fixture.domain, first, second)
	owner.client.clientKeyAttemptBudget = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	boundary, operator, err := owner.readBoundary(ctx, nil)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || boundary != (protocol.ClientKeyEffectiveBoundary{}) || operator != (stabi.STCoordinatorOperatorVersion{}) {
		t.Fatal("stalled endpoints did not fail closed", boundary, operator, err)
	}
	if firstRequests.Load() < 2 || secondRequests.Load() < 1 {
		t.Fatal("the per-endpoint bound did not let both endpoints be tried and retried in order", firstRequests.Load(), secondRequests.Load())
	}
}

func stClientKeyFallbackBatchQueries(fixture *stClientKeyHistoryRPCFixture) ([]stabi.ClientKeyAuthorityQuery, stabi.ClientKeyAuthorityRpcLimits) {
	return []stabi.ClientKeyAuthorityQuery{{Domain: fixture.domain, Boundary: fixture.boundary}}, stabi.ClientKeyAuthorityRpcLimits{MaximumRequests: protocol.MaxClientKeyObservationBatchRpcRequests, MaximumMethods: protocol.MaxClientKeyObservationBatchRpcMethods, MaximumBytes: protocol.MaxClientKeyObservationBatchControlBytes}
}

// The validator batch observation used to dial each url and keep the first
// dial that succeeded. An Http dial does no I/O, so it always kept the first
// url and a second configured url was dead code. The read itself now fails
// over; a failed endpoint's values are never used.
func TestStClientKeyAuthorityBatchReadFailsOverAcrossEndpoints(t *testing.T) {
	fixture, healthy := newStClientKeyFallbackEndpoint(t, "")
	refused := newStClientKeyRefusedEndpoint(t)
	_, unavailable := newStClientKeyFallbackEndpoint(t, "native-unavailable")
	queries, limits := stClientKeyFallbackBatchQueries(fixture)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The premise of the old loop: dialing a refusing Http url succeeds.
	dialed, err := ethclient.DialContext(ctx, refused)
	if err != nil {
		t.Fatal("an Http dial now performs I/O; revisit the batch failover comment", err)
	}
	dialed.Close()

	for _, urls := range [][]string{{refused, healthy}, {unavailable, refused, healthy}} {
		authorities, work, err := readStClientKeyAuthoritiesFailover(ctx, urls, queries, limits)
		if err != nil || len(authorities) != 1 || authorities[0].Operator.RootSigner != fixture.root || authorities[0].Query != queries[0] || work.Requests == 0 {
			t.Fatal("batch read did not fail over to the verified second endpoint", urls, authorities, err)
		}
	}

	_, companion := newStClientKeyFallbackEndpoint(t, "companion")
	authorities, _, err := readStClientKeyAuthoritiesFailover(ctx, []string{refused, companion}, queries, limits)
	if err == nil || authorities != nil || !strings.Contains(err.Error(), refused) || !strings.Contains(err.Error(), companion) {
		t.Fatal("batch read did not fail closed naming every endpoint", authorities, err)
	}
}

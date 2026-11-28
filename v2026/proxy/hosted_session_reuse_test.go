package proxy

// A hosted proxy's next device in the process reuses the session its last
// device opened with, for two hours after the mint (P90SPEED.md §4.1, fix 4).
// Every device used to load the proxy client's credential and sign a new
// hosted session, one query and one transaction, on every reopen.

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/localclient"
	"github.com/urnetwork/server/v2026/session"
)

// The session store and the local authority as the cache sees them: mints
// numbered tokens, and builds an authority on any token it does not refuse.
type hostedSessionTestAuthority struct {
	now       time.Time
	mintCount int
	mintErr   error
	// tokens the authority refuses, with the refusal
	refused map[string]error
	// tokens an authority was built on, in order
	built []string
}

func newHostedSessionTestCache(authority *hostedSessionTestAuthority) *hostedSessionCache {
	cache := newHostedSessionCache()
	cache.now = func() time.Time {
		return authority.now
	}
	cache.mint = func(_ context.Context, clientId server.Id) (string, error) {
		if authority.mintErr != nil {
			return "", authority.mintErr
		}
		authority.mintCount += 1
		return fmt.Sprintf("token-%d-%s", authority.mintCount, clientId), nil
	}
	cache.newAuthority = func(_ context.Context, signedByJwt string, _ string, _ *localclient.AuthoritySettings) (*localclient.Authority, error) {
		authority.built = append(authority.built, signedByJwt)
		if err := authority.refused[signedByJwt]; err != nil {
			return nil, err
		}
		return &localclient.Authority{}, nil
	}
	return cache
}

func hostedSessionTestApiUrl() string {
	return "https://api.reuse.example"
}

func hostedSessionTestOpen(t *testing.T, cache *hostedSessionCache, clientId server.Id) string {
	t.Helper()
	signedByJwt, localControl, err := cache.openAuthority(t.Context(), clientId, hostedSessionTestApiUrl, &localclient.AuthoritySettings{})
	if err != nil {
		t.Fatal(err)
	}
	if localControl == nil {
		t.Fatal("no local authority")
	}
	return signedByJwt
}

// A proxy reopened 1h59m after its first device opened reuses that device's
// session, which the new authority validates again; another proxy mints its
// own; two hours after the mint the proxy mints again.
func TestReopenReusesHostedSessionWithoutMinting(t *testing.T) {
	authority := &hostedSessionTestAuthority{now: time.Unix(1_800_000_000, 0)}
	cache := newHostedSessionTestCache(authority)
	proxyClientId := server.NewId()

	first := hostedSessionTestOpen(t, cache, proxyClientId)
	if authority.mintCount != 1 {
		t.Fatalf("the first device minted %d sessions, want 1", authority.mintCount)
	}

	authority.now = authority.now.Add(time.Hour + 59*time.Minute)
	reopened := hostedSessionTestOpen(t, cache, proxyClientId)
	if authority.mintCount != 1 {
		t.Fatalf("the reopen minted %d sessions in all, want 1: the first device's session is reused", authority.mintCount)
	}
	if reopened != first {
		t.Fatal("the reopen did not reuse the first device's session")
	}
	// validated by the reopened device's own authority
	if len(authority.built) != 2 || authority.built[1] != first {
		t.Fatalf("authorities built on %v, want the reused session validated again", authority.built)
	}

	other := hostedSessionTestOpen(t, cache, server.NewId())
	if authority.mintCount != 2 || other == first {
		t.Fatal("another proxy did not mint its own session")
	}

	authority.now = authority.now.Add(time.Minute)
	afterTwoHours := hostedSessionTestOpen(t, cache, proxyClientId)
	if authority.mintCount != 3 || afterTwoHours == first {
		t.Fatalf("two hours after the mint the proxy reused its session (mints %d), want a new one", authority.mintCount)
	}
}

// A reused session the new authority refuses (revoked, rotated, a removed
// client, or a session store that cannot answer) is forgotten and the device
// mints, as without reuse; the newly minted session is the one reused next.
func TestRefusedHostedSessionFallsBackToMint(t *testing.T) {
	authority := &hostedSessionTestAuthority{now: time.Unix(1_800_000_000, 0)}
	cache := newHostedSessionTestCache(authority)
	proxyClientId := server.NewId()

	first := hostedSessionTestOpen(t, cache, proxyClientId)
	authority.refused = map[string]error{first: errors.New("401 Unauthorized")}

	authority.now = authority.now.Add(10 * time.Minute)
	second := hostedSessionTestOpen(t, cache, proxyClientId)
	if second == first || authority.mintCount != 2 {
		t.Fatalf("a refused session was not replaced by a mint: mints %d", authority.mintCount)
	}

	authority.now = authority.now.Add(10 * time.Minute)
	third := hostedSessionTestOpen(t, cache, proxyClientId)
	if third != second || authority.mintCount != 2 {
		t.Fatalf("the replacement session was not reused: mints %d", authority.mintCount)
	}
}

// A revoked proxy session cannot open a device through reuse: the authority
// refuses the remembered session, the mint refuses as it always did, and the
// open fails with the mint's refusal and remembers nothing.
func TestRevokedHostedSessionFailsAsWithoutReuse(t *testing.T) {
	authority := &hostedSessionTestAuthority{now: time.Unix(1_800_000_000, 0)}
	cache := newHostedSessionTestCache(authority)
	proxyClientId := server.NewId()

	first := hostedSessionTestOpen(t, cache, proxyClientId)
	authority.refused = map[string]error{first: session.ErrSessionRevoked}
	authority.mintErr = session.ErrSessionRevoked

	_, _, err := cache.openAuthority(t.Context(), proxyClientId, hostedSessionTestApiUrl, &localclient.AuthoritySettings{})
	if !errors.Is(err, session.ErrSessionRevoked) {
		t.Fatalf("the revoked proxy opened with err=%v, want the mint's refusal", err)
	}
	if _, ok := cache.reusable(proxyClientId); ok {
		t.Fatal("the revoked session is still remembered")
	}
}

// The remembered sessions are bounded: a full cache first drops the expired
// ones, then gives up another proxy's, which mints at its next open.
func TestHostedSessionCacheIsBounded(t *testing.T) {
	authority := &hostedSessionTestAuthority{now: time.Unix(1_800_000_000, 0)}
	cache := newHostedSessionTestCache(authority)
	cache.maxEntries = 2

	expiring := server.NewId()
	hostedSessionTestOpen(t, cache, expiring)
	authority.now = authority.now.Add(time.Hour)
	kept := server.NewId()
	hostedSessionTestOpen(t, cache, kept)
	authority.now = authority.now.Add(time.Hour)

	// the expired session goes first
	added := server.NewId()
	hostedSessionTestOpen(t, cache, added)
	if len(cache.entries) != 2 {
		t.Fatalf("the cache holds %d sessions, want 2", len(cache.entries))
	}
	if _, ok := cache.entries[expiring]; ok {
		t.Fatal("the expired session was kept over a live one")
	}
	if _, ok := cache.reusable(kept); !ok {
		t.Fatal("a reusable session was given up while an expired one was held")
	}

	// none expired: one other proxy's session is given up
	hostedSessionTestOpen(t, cache, server.NewId())
	if len(cache.entries) != 2 {
		t.Fatalf("the cache holds %d sessions, want 2", len(cache.entries))
	}
}

// Every device a manager opens shares the manager's sessions.
func TestProxyDeviceManagerSharesHostedSessions(t *testing.T) {
	manager := NewProxyDeviceManager(t.Context(), DefaultProxyDeviceManagerSettings())
	defer manager.Close()
	if manager.hostedSessions == nil {
		t.Fatal("the manager has no hosted sessions")
	}
	if manager.proxyDeviceSettings().hostedSessions != manager.hostedSessions {
		t.Fatal("a device of the manager does not use the manager's hosted sessions")
	}
}

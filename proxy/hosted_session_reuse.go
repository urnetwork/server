package proxy

import (
	"context"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/localclient"
	"github.com/urnetwork/server/session"
)

// The hosted session a proxy's device opens with, reused by the next device
// of the same proxy in this process (P90SPEED.md §4.1, fix 4).
//
// Every device construction loaded the proxy client's credential
// (session.LoadByJwtFromClientId, one query) and signed a hosted session for
// it (session.MintHostedSession, one transaction). About 80% of constructions
// on Main reopen a proxy whose previous device closed for idleness, and each
// paid both again for a session that was still valid: a hosted session's
// token lives 30 days. A proxy is served by one process (its block), so its
// next device usually opens where its last one did.
//
// Reuse keeps the session rules (session/REVOKE-FINAL.md):
//   - The local authority validates the reused token when it is built, as it
//     validates a newly minted one (localclient.NewWithSettings: signature,
//     expiry, the revocation marker, an active client, credential rotation),
//     and again on every operation it serves.
//   - A token the authority refuses is forgotten, and the device mints as
//     before: a revoked session or a removed proxy is refused by the mint, as
//     it always was.
//   - A token is reused only for hostedSessionReuseTimeout after it was
//     minted, far inside its 30-day life. Reuse renews nothing: no token's
//     life is extended.
//
// The cache holds signed tokens of this process's own proxies in memory
// only, as the devices that use them do.

// How long after its mint a hosted session token is reused by the proxy's
// next device: the window identity snapshot's ttl
// (model.ProxyWindowIdentityTtl), so a proxy that reopens on its exits also
// reopens on its session.
const hostedSessionReuseTimeout = 2 * time.Hour

// Bounds the tokens one process remembers. A process builds about 30 devices
// a minute at today's load, so two hours touch a few thousand proxies; a
// token is about 1.2 KB.
const hostedSessionReuseMaxEntries = 16 * 1024

type hostedSessionEntry struct {
	signedByJwt string
	mintTime    time.Time
}

// One per device manager. A nil cache mints for every device, as a device
// built outside a manager does.
type hostedSessionCache struct {
	timeout    time.Duration
	maxEntries int

	// the clock; time.Now in production
	now func() time.Time
	// loads the proxy client's credential and signs a hosted session for it;
	// mintHostedSessionForClient in production
	mint func(ctx context.Context, clientId server.Id) (string, error)
	// builds the device's local authority on a token, validating it;
	// localclient.NewWithSettings in production
	newAuthority func(ctx context.Context, signedByJwt string, apiUrl string, settings *localclient.AuthoritySettings) (*localclient.Authority, error)

	stateLock sync.Mutex
	// by the proxy's client id
	entries map[server.Id]hostedSessionEntry
}

func newHostedSessionCache() *hostedSessionCache {
	return &hostedSessionCache{
		timeout:      hostedSessionReuseTimeout,
		maxEntries:   hostedSessionReuseMaxEntries,
		now:          time.Now,
		mint:         mintHostedSessionForClient,
		newAuthority: localclient.NewWithSettings,
		entries:      map[server.Id]hostedSessionEntry{},
	}
}

// The production mint: the proxy client's credential, signed as its hosted
// session.
func mintHostedSessionForClient(ctx context.Context, clientId server.Id) (string, error) {
	byJwt, err := session.LoadByJwtFromClientId(ctx, clientId)
	if err != nil {
		return "", err
	}
	return session.MintHostedSession(ctx, byJwt)
}

// The signed hosted session of the proxy client and the device's local
// authority built on it: the session the proxy's previous device opened with
// while it is reusable and validates, else a newly minted one. apiUrl is read
// only to build the authority, so a refused mint returns before the device's
// network space is touched.
func (self *hostedSessionCache) openAuthority(
	ctx context.Context,
	clientId server.Id,
	apiUrl func() string,
	authoritySettings *localclient.AuthoritySettings,
) (string, *localclient.Authority, error) {
	if self == nil {
		signedByJwt, err := mintHostedSessionForClient(ctx, clientId)
		if err != nil {
			return "", nil, err
		}
		localControl, err := localclient.NewWithSettings(ctx, signedByJwt, apiUrl(), authoritySettings)
		if err != nil {
			return "", nil, err
		}
		return signedByJwt, localControl, nil
	}

	if signedByJwt, ok := self.reusable(clientId); ok {
		localControl, err := self.newAuthority(ctx, signedByJwt, apiUrl(), authoritySettings)
		if err == nil {
			return signedByJwt, localControl, nil
		}
		// refused or not checkable now: mint, as without reuse
		self.forget(clientId, signedByJwt)
		if glog.V(1) {
			glog.Infof("[pd]hosted session not reused: %s\n", err)
		}
	}

	signedByJwt, err := self.mint(ctx, clientId)
	if err != nil {
		return "", nil, err
	}
	localControl, err := self.newAuthority(ctx, signedByJwt, apiUrl(), authoritySettings)
	if err != nil {
		return "", nil, err
	}
	self.remember(clientId, signedByJwt)
	return signedByJwt, localControl, nil
}

// The client's remembered token while it is reusable.
func (self *hostedSessionCache) reusable(clientId server.Id) (string, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry, ok := self.entries[clientId]
	if !ok {
		return "", false
	}
	if self.timeout <= self.now().Sub(entry.mintTime) {
		delete(self.entries, clientId)
		return "", false
	}
	return entry.signedByJwt, true
}

// Remembers a newly minted token for the client's next device.
func (self *hostedSessionCache) remember(clientId server.Id, signedByJwt string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	now := self.now()
	if _, ok := self.entries[clientId]; !ok && self.maxEntries <= len(self.entries) {
		for entryClientId, entry := range self.entries {
			if self.timeout <= now.Sub(entry.mintTime) {
				delete(self.entries, entryClientId)
			}
		}
		// still full: give up one other entry, which mints at its next open
		for entryClientId := range self.entries {
			if len(self.entries) < self.maxEntries {
				break
			}
			delete(self.entries, entryClientId)
		}
	}
	self.entries[clientId] = hostedSessionEntry{
		signedByJwt: signedByJwt,
		mintTime:    now,
	}
}

// Forgets the client's token if it is still the one given.
func (self *hostedSessionCache) forget(clientId server.Id, signedByJwt string) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if entry, ok := self.entries[clientId]; ok && entry.signedByJwt == signedByJwt {
		delete(self.entries, clientId)
	}
}

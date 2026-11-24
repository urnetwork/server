package controller

// api's end of §14.8's report seal key (plan-SMTP-report-seal-key, decided as its option 1, QD-075; §15.8's network
// mail key, D-KEYS-17; §19.4's api row): the key the platform listener's intake seals a network's reports to, which
// api answers on its internal gate (GET /internal/gate/report-key/<network_id>,
// api/handlers/internal_gate_report_key_handlers.go) from the network's own verified rows and gkm, so POST
// /mail/enable takes no field:
//
//	repo     the gate cache's verified state names the mail repo (AclState.NetworkMailKeyRepo), the rule the reports
//	         read's audience takes too (model.SmtpApiGateOf), so whom a copy is sealed to and who may read it agree
//	read     gkm's drop-key route for that repo, from generation 0, under a 15-minute repo:<id>:deposit service token,
//	         aud gkm, that api mints itself (§7.8): the asked network, a client id of its own per token, no sidecar or
//	         member, and no other scope; gkm's url is $UR_GKM_URL (sidecarhost.GkmUrlEnvName)
//	checks   smtpedgeservice.CheckSmtpReportSealKey at the GkmAuthView of the same verified position, the snapshot's
//	         ConfigState
//	cache    a network's answer, a key or gkm's none, for at most CacheLifetime (5 min, §14.8); a cached key is held to
//	         SmtpReportSealKey.Check at the newest state at each answer and read again from gkm when it fails, so a
//	         removal stops the seal at once and a rotation reaches it within the bound
//	answers  a key; nil for none (rows naming no mail repo, gkm serving no key or refusing the read, a key failing a
//	         check, or no gkm url); an error wrapping smtpedgeservice.ErrSmtpReportKeyUnavailable when api cannot tell
//	         now (the network's state not loaded, gkm not answering, a state behind the generation), which the route
//	         answers 503 and the listener 451
//
// gkm admits this token at the drop-key route alone, as §7.8 and §10.14 have it (QD-1229, question
// smtp-report-seal-key-gkm-api-token): its guard passes a token naming no sidecar or member and holding deposit scopes
// alone there and on no other route, asks it no standing, and honors its deposit while the network's newest verified
// state at gkm holds the repo, with no RepoDelete standing on it, and the repo takes deposits (D-KEYS-17). gkm answers
// 503 while that state is not loaded, which reads here as gkm not telling now, and 403 for a repo that state does not
// hold, holds deleted or holds as taking no deposits, which reads as none until the answer is read again.
//
// Safe for concurrent use. No lock is held across I/O.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/oauth"
)

// The audience api's own drop-key token names, gkmservice.GkmAudience.
const smtpReportDropKeyAudience = "gkm"

// The source's tunables, each with its default here and nowhere else.
type SmtpReportKeySourceSettings struct {
	// How long a network's answer from gkm, a key or none, is kept: 5 min, §14.8's bound on how stale a seal key may
	// be. A cached key is checked again at each answer regardless. Non-positive keeps nothing.
	CacheLifetime time.Duration
	// The most networks' answers kept: 4,096; past it the expired go first, then the oldest read.
	CacheMaxNetworkCount int
	// How long one read of gkm may take: 5 s, the intake's own StoreTimeout, which bounds the platform listener's
	// whole call to api.
	GkmTimeout time.Duration
	// How long a drop key may live past its generation's time, GkmSettings.DropKeyLifetime's (QD-031), which
	// §10.9's rotation checks hold drop_not_after_ms to.
	DropKeyLifetime time.Duration
}

func DefaultSmtpReportKeySourceSettings() *SmtpReportKeySourceSettings {
	return &SmtpReportKeySourceSettings{
		CacheLifetime:        5 * time.Minute,
		CacheMaxNetworkCount: 4096,
		GkmTimeout:           5 * time.Second,
		DropKeyLifetime:      sidecar.DefaultGkmSettings().DropKeyLifetime,
	}
}

// What the source reads a network's verified state from. Safe for concurrent use.
type SmtpReportKeyStates interface {
	// The network's verified state: its acl part and the GkmAuthView of the same position. ok false while it is not
	// loaded, which asks for its load.
	ReportKeyState(networkId sidecar.Id) (acl *sidecar.AclState, view sidecar.GkmAuthView, ok bool)
}

// gkm's drop-key route as api reads it. Safe for concurrent use.
type SmtpReportDropKeyReader interface {
	// The repo's GenRotates after since, as gkm answers them, under api's own token for the network; an error of
	// sidecar's gkm client otherwise (sidecar.GkmErrorOf, sidecar.ErrGkmTokenRefused, sidecar.ErrGkmNoAnswer).
	DropKeys(ctx context.Context, networkId sidecar.Id, repoId sidecar.Id, since uint64) (*protocol.GkmGenerationsReply, error)
}

// One network's answer from gkm.
type smtpReportKeyEntry struct {
	repoId sidecar.Id
	// nil for none
	key      *smtpedgeservice.SmtpReportSealKey
	readTime time.Time
}

// The key api answers for a network's reports. See the file header.
type SmtpReportKeySource struct {
	states   SmtpReportKeyStates
	gkm      SmtpReportDropKeyReader
	clock    sidecar.Clock
	settings *SmtpReportKeySourceSettings

	stateLock        sync.Mutex
	networkIdEntries map[sidecar.Id]*smtpReportKeyEntry
}

// A source over states and gkm, nil gkm answering none for every network; nil clock is real time and nil settings
// the defaults.
func NewSmtpReportKeySource(states SmtpReportKeyStates, gkm SmtpReportDropKeyReader, clock sidecar.Clock, settings *SmtpReportKeySourceSettings) *SmtpReportKeySource {
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	if settings == nil {
		settings = DefaultSmtpReportKeySourceSettings()
	}
	return &SmtpReportKeySource{
		states:           states,
		gkm:              gkm,
		clock:            clock,
		settings:         settings,
		networkIdEntries: map[sidecar.Id]*smtpReportKeyEntry{},
	}
}

// The network's key to seal its reports to, nil for none, or an error wrapping
// smtpedgeservice.ErrSmtpReportKeyUnavailable. See the file header.
func (self *SmtpReportKeySource) ReportSealKey(ctx context.Context, networkId sidecar.Id) (*smtpedgeservice.SmtpReportSealKey, error) {
	if self.states == nil {
		return nil, fmt.Errorf("%w: no verified states", smtpedgeservice.ErrSmtpReportKeyUnavailable)
	}
	acl, view, ok := self.states.ReportKeyState(networkId)
	if !ok || acl == nil || view == nil {
		return nil, fmt.Errorf("%w: the network's verified state is not loaded", smtpedgeservice.ErrSmtpReportKeyUnavailable)
	}
	repoId, found := acl.NetworkMailKeyRepo()
	if !found {
		return nil, nil
	}
	now := self.clock.Now()
	if entry := self.cached(networkId, repoId, now); entry != nil {
		if entry.key == nil {
			return nil, nil
		}
		err := entry.key.Check(view, now)
		if err == nil {
			return entry.key, nil
		}
		if errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable) {
			return nil, err
		}
		// a removal or a lapse since the read: gkm may serve a generation that covers it
	}
	if self.gkm == nil {
		return nil, nil
	}
	reply, err := func() (*protocol.GkmGenerationsReply, error) {
		readCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		stop := self.clock.AfterFunc(self.settings.GkmTimeout, cancel)
		defer stop()
		return self.gkm.DropKeys(readCtx, networkId, repoId, 0)
	}()
	if err != nil {
		if !smtpReportGkmRefusal(err) {
			return nil, fmt.Errorf("%w: gkm did not answer: %w", smtpedgeservice.ErrSmtpReportKeyUnavailable, err)
		}
		// gkm refuses the read, or holds no group of the repo: none, until it is read again
		glog.Infof("[smtpreportkey]gkm refused a mail repo's drop-key read, so its network's reports are sealed to none: %v\n", err)
		self.keep(networkId, &smtpReportKeyEntry{repoId: repoId, readTime: now})
		return nil, nil
	}
	key, err := smtpedgeservice.CheckSmtpReportSealKey(view, repoId, reply, now, self.settings.DropKeyLifetime)
	switch {
	case err == nil:
		self.keep(networkId, &smtpReportKeyEntry{repoId: repoId, key: key, readTime: now})
		return key, nil
	case errors.Is(err, smtpedgeservice.ErrSmtpReportKeyUnavailable):
		return nil, err
	}
	if glog.V(1) {
		// ids and positions only
		glog.Infof("[smtpreportkey]a network's reports are sealed to none: %v\n", err)
	}
	self.keep(networkId, &smtpReportKeyEntry{repoId: repoId, readTime: now})
	return nil, nil
}

// Whether gkm's answer refuses the read until it is read again: no token it takes, no scope it honors, no group of the
// repo, or a request it cannot parse. Every other failure, a 429 or 503, the lb's 502 or 504 or no answer at all, is
// gkm not telling now.
func smtpReportGkmRefusal(err error) bool {
	if errors.Is(err, sidecar.ErrGkmTokenRefused) {
		return true
	}
	gkmError, ok := sidecar.GkmErrorOf(err)
	if !ok {
		return false
	}
	switch gkmError.Status {
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
		return true
	}
	return false
}

// The network's answer read within CacheLifetime for the same repo, or nil.
func (self *SmtpReportKeySource) cached(networkId sidecar.Id, repoId sidecar.Id, now time.Time) *smtpReportKeyEntry {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	entry := self.networkIdEntries[networkId]
	if entry == nil || entry.repoId != repoId || !now.Before(entry.readTime.Add(self.settings.CacheLifetime)) {
		return nil
	}
	return entry
}

// Keeps a network's answer, making room past CacheMaxNetworkCount: the expired first, then the oldest read.
func (self *SmtpReportKeySource) keep(networkId sidecar.Id, entry *smtpReportKeyEntry) {
	if self.settings.CacheLifetime <= 0 || self.settings.CacheMaxNetworkCount <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.networkIdEntries[networkId]; !ok && self.settings.CacheMaxNetworkCount <= len(self.networkIdEntries) {
		var oldestNetworkId sidecar.Id
		var oldest *smtpReportKeyEntry
		for keptNetworkId, kept := range self.networkIdEntries {
			if !entry.readTime.Before(kept.readTime.Add(self.settings.CacheLifetime)) {
				delete(self.networkIdEntries, keptNetworkId)
				continue
			}
			if oldest == nil || kept.readTime.Before(oldest.readTime) {
				oldestNetworkId, oldest = keptNetworkId, kept
			}
		}
		if oldest != nil && self.settings.CacheMaxNetworkCount <= len(self.networkIdEntries) {
			delete(self.networkIdEntries, oldestNetworkId)
		}
	}
	self.networkIdEntries[networkId] = entry
}

// The gate cache's verified states as the source reads them: a snapshot's acl part and its ConfigState, the
// GkmAuthView of the same position. A network not loaded asks the cache for its load.
func SmtpReportKeyStatesOf(cache sidecar.AclContractStateSource) SmtpReportKeyStates {
	return &smtpReportKeyCacheStates{cache: cache}
}

type smtpReportKeyCacheStates struct {
	cache sidecar.AclContractStateSource
}

// See SmtpReportKeyStates.
func (self *smtpReportKeyCacheStates) ReportKeyState(networkId sidecar.Id) (*sidecar.AclState, sidecar.GkmAuthView, bool) {
	snapshot := sidecar.RequestAclGateSnapshot(self.cache, networkId)
	if snapshot == nil || snapshot.Acl == nil || snapshot.Config == nil {
		return nil, nil, false
	}
	return snapshot.Acl, snapshot.Config, true
}

// gkm's drop-key route at gkmUrl under api's own token: one sidecar.GkmApi a read, presenting the repo's deposit
// scope alone, which mint signs for the asked network. Its reads are not retried, since the reporter's retry is the
// listener's.
type smtpReportGkmReader struct {
	gkmUrl    string
	transport http.RoundTripper
	clock     sidecar.Clock
	settings  *sidecar.GkmSettings
	mint      func(networkId server.Id, scopes []string) (string, error)
}

// gkm's drop-key route at gkmUrl, over transport, nil for a transport of its own, with tokens api mints under its
// svc- key (oauth.MintServiceToken). An error for a url that is no gkm's.
func NewSmtpReportGkmReader(gkmUrl string, transport http.RoundTripper, clock sidecar.Clock) (SmtpReportDropKeyReader, error) {
	if transport == nil {
		transport = &http.Transport{}
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	settings := sidecar.DefaultGkmSettings()
	settings.ReadRetryCount = 0
	self := &smtpReportGkmReader{
		gkmUrl:    gkmUrl,
		transport: transport,
		clock:     clock,
		settings:  settings,
		mint:      mintSmtpReportDropKeyToken,
	}
	// the url is checked as each read's client checks it
	if _, err := self.api(sidecar.Id{}, sidecar.Id{}); err != nil {
		return nil, err
	}
	return self, nil
}

// A client of gkm for one read of repoId's drop key in networkId.
func (self *smtpReportGkmReader) api(networkId sidecar.Id, repoId sidecar.Id) (*sidecar.GkmApi, error) {
	depositScope := oauth.ServiceTokenRepoScopePrefix + repoId.String() + ":" + sidecar.GkmScopeRoleDeposit
	return sidecar.NewGkmApi(&sidecar.GkmApiArgs{
		GkmUrl:    self.gkmUrl,
		Transport: self.transport,
		Settings:  self.settings,
		Clock:     self.clock,
		ServiceToken: func(ctx context.Context, resource string, scopes []string) (string, error) {
			// api mints this one scope and nothing beside it
			if len(scopes) != 1 || scopes[0] != depositScope {
				return "", errors.New("smtp report key: a scope beyond the repo's deposit scope")
			}
			return self.mint(server.Id(networkId), scopes)
		},
	})
}

// See SmtpReportDropKeyReader.
func (self *smtpReportGkmReader) DropKeys(ctx context.Context, networkId sidecar.Id, repoId sidecar.Id, since uint64) (*protocol.GkmGenerationsReply, error) {
	api, err := self.api(networkId, repoId)
	if err != nil {
		return nil, err
	}
	return api.DropKeys(ctx, repoId, since)
}

// api's own token for gkm's drop-key route (§7.8's self-issued deposit token): aud gkm, the repo's deposit scope
// alone, the asked network, and a client id of its own per token that names no client, with no sidecar or member, so
// nothing it carries reaches past that one public-key read. 15 minutes, as every service token, and never stored.
func mintSmtpReportDropKeyToken(networkId server.Id, scopes []string) (string, error) {
	token, _, err := oauth.MintServiceToken(&oauth.MintServiceTokenArgs{
		NetworkId: networkId,
		ClientId:  server.NewId(),
		Audience:  smtpReportDropKeyAudience,
		Scopes:    scopes,
	})
	return token, err
}

// The process's source, which api's projection install sets (installApiProjection) and the internal route reads.
var smtpReportKeySource atomic.Pointer[SmtpReportKeySource]

// Sets the process's source, nil for none, and answers what restores the one before.
func SetSmtpReportKeySource(source *SmtpReportKeySource) (restore func()) {
	previous := smtpReportKeySource.Swap(source)
	return func() {
		smtpReportKeySource.CompareAndSwap(source, previous)
	}
}

// The process's source, nil before api's projection install has set one.
func CurrentSmtpReportKeySource() *SmtpReportKeySource {
	return smtpReportKeySource.Load()
}

// The source api's projection installs over its gate cache: gkm at gkmUrl, or none for every network with no url,
// which it says once.
func newApiSmtpReportKeySource(cache sidecar.AclContractStateSource, gkmUrl string, clock sidecar.Clock) *SmtpReportKeySource {
	var gkm SmtpReportDropKeyReader
	if gkmUrl == "" {
		glog.Infof("[smtpreportkey]no gkm url, so every network's reports are sealed to none\n")
	} else if reader, err := NewSmtpReportGkmReader(gkmUrl, nil, clock); err != nil {
		glog.Errorf("[smtpreportkey]gkm's url cannot be used, so every network's reports are sealed to none: %v\n", err)
	} else {
		gkm = reader
	}
	return NewSmtpReportKeySource(SmtpReportKeyStatesOf(cache), gkm, clock, nil)
}

package sidecart3

// A T3 world device's real key and evs clients (T3WorldSettings.RealKeys; KEYS-M2-22, EVS1-M2-22, QD-804, QD-817): what
// ur daemon builds for every network it hosts (sidecar/cli/daemon_clients.go), built here for one T3Device, since the
// daemon's composition is package main:
//
//	key client   the network's KeyDelivery group client (sidecar.GkmGroupClient) over GkmApi at the world's gkm origin,
//	             with the gkm hint intake and the ReaderKey upkeep, behind t3WorldKeysGkm, which keeps the newest state
//	             the follower hands it, a joining device's rows-only state unconfirmed until it stands (QD-817), and
//	             builds the group client at the first need
//	evs client   an EvsClient over that key client at the world's evs origin, judging statements at the follower's
//	             newest verified state, its statements filed as T3Network.FileStatement files them, with the network's
//	             prefix rules and format 2 bound as the daemon binds them; no local git tier and no search, which no
//	             flow of a world reaches
//	config heads sidecar.EvsConfigHeadReader over that EvsClient on the config repo the genesis names, so the config
//	             repo's heads live on evs and a base's head is read and checked there (t3WorldKeysHeads)
//
// The component (t3WorldKeysComponent) feeds each client generation's gkm hints to the intake and ends the clients with
// the Sidecar. Every lock is taken in the daemon's order: groupLock, intakeLock, evsLock, then stateLock, never under
// stateLock. Safe for concurrent use.

import (
	"context"
	"crypto/hpke"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sync"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
)

// The component's name on the Sidecar.
const t3WorldKeysComponentName = "t3-world-keys"

// The device's clients have closed.
var errT3WorldKeysClosed = errors.New("this device's key and evs clients have closed")

// One device's key and evs clients (the file's header).
type t3WorldKeys struct {
	running *sidecar.Sidecar
	// files a statement as the world files it (T3Network.FileStatement), which the EvsClient's acts use
	fileStatement func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error
	log           sidecar.Logger
	ctx           context.Context
	cancel        context.CancelFunc
	gkm           *t3WorldKeysGkm
	heads         *t3WorldKeysHeads

	groupLock  sync.Mutex
	intakeLock sync.Mutex
	evsLock    sync.Mutex
	scopesLock sync.Mutex

	stateLock      sync.Mutex
	closed         bool
	api            *sidecar.GkmApi
	clock          sidecar.Clock
	hints          *sidecar.GkmHints
	upkeep         *sidecar.GkmUpkeep
	group          *sidecar.GkmGroupClient
	removeRecovery func()
	// closed once group is built
	groupBuiltSignal chan struct{}
	evs              *sidecar.EvsClient
	reader           *sidecar.EvsConfigHeadReader
	readerRepoId     sidecar.Id
}

// running's clients, nothing built yet, with their component on the Sidecar, which ends them when it closes.
func newT3WorldKeys(running *sidecar.Sidecar, log sidecar.Logger, fileStatement func(ctx context.Context, signedType bridge.SignedType, body proto.Message) error) (*t3WorldKeys, error) {
	ctx, cancel := context.WithCancel(context.Background())
	self := &t3WorldKeys{
		running:          running,
		fileStatement:    fileStatement,
		log:              log,
		ctx:              ctx,
		cancel:           cancel,
		groupBuiltSignal: make(chan struct{}),
	}
	self.gkm = &t3WorldKeysGkm{keys: self}
	self.heads = &t3WorldKeysHeads{keys: self}
	if _, err := running.AddComponent(&t3WorldKeysComponent{keys: self}); err != nil {
		cancel()
		return nil, err
	}
	return self, nil
}

// Ends the clients: the hint intake, the upkeep and the key client's timers stop, and nothing more is built.
func (self *t3WorldKeys) close() {
	var hints *sidecar.GkmHints
	var upkeep *sidecar.GkmUpkeep
	var removeRecovery func()
	closed := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.closed = true
		hints, upkeep, removeRecovery = self.hints, self.upkeep, self.removeRecovery
		return true
	}()
	if !closed {
		return
	}
	self.cancel()
	if removeRecovery != nil {
		removeRecovery()
	}
	if upkeep != nil {
		upkeep.Close()
	}
	if hints != nil {
		hints.Close()
	}
}

// The group client, nil before it is built.
func (self *t3WorldKeys) builtGroup() *sidecar.GkmGroupClient {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.group
}

// The network's GkmApi, gkm hint intake and key upkeep, built at the first call once the Sidecar knows its network and
// identity. The upkeep publishes and renews the device's ReaderKey and KeyPackages.
func (self *t3WorldKeys) intake() (*sidecar.GkmApi, *sidecar.GkmHints, error) {
	self.intakeLock.Lock()
	defer self.intakeLock.Unlock()
	api, hints, closed := func() (*sidecar.GkmApi, *sidecar.GkmHints, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.api, self.hints, self.closed
	}()
	if closed {
		return nil, nil, errT3WorldKeysClosed
	}
	if hints != nil {
		return api, hints, nil
	}
	apiArgs, err := self.running.GkmApiArgs()
	if err != nil {
		return nil, nil, err
	}
	api, err = sidecar.NewGkmApi(apiArgs)
	if err != nil {
		return nil, nil, err
	}
	hints, err = sidecar.NewGkmHints(self.ctx, &sidecar.GkmHintsArgs{Settings: apiArgs.Settings, Clock: apiArgs.Clock, Log: self.log})
	if err != nil {
		return nil, nil, err
	}
	upkeepArgs, err := self.running.GkmUpkeepArgs(api, hints)
	if err != nil {
		hints.Close()
		return nil, nil, err
	}
	upkeep, err := sidecar.NewGkmUpkeep(self.ctx, upkeepArgs)
	if err != nil {
		hints.Close()
		return nil, nil, err
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.api, self.hints, self.upkeep, self.clock = api, hints, upkeep, apiArgs.Clock
		return true
	}()
	if !kept {
		upkeep.Close()
		hints.Close()
		return nil, nil, errT3WorldKeysClosed
	}
	self.setRepoScopes()
	return api, hints, nil
}

// Hands the GkmApi the repo scopes the newest state the key client was given grants this sidecar at gkm, each repo's
// strongest role by §8's table for gkm's audience, as the daemon does; a joining device's unconfirmed state asks for
// the config repo's alone (QD-817).
func (self *t3WorldKeys) setRepoScopes() {
	self.scopesLock.Lock()
	defer self.scopesLock.Unlock()
	api, clock := func() (*sidecar.GkmApi, sidecar.Clock) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.api, self.clock
	}()
	view, configRepoId, confirmed := self.gkm.joiningState()
	state, ok := view.(*sidecar.ConfigState)
	identity := self.running.Identity()
	if api == nil || !ok || identity == nil {
		return
	}
	acl, err := state.Acl()
	if err != nil {
		return
	}
	if clock == nil {
		clock = sidecar.RealClock{}
	}
	now := clock.Now()
	subject := sidecar.AclSidecarSubject(identity.SidecarId)
	scopes := []sidecar.GkmRepoScope{}
	for _, repoId := range acl.RepoIds() {
		if !confirmed && repoId != configRepoId {
			continue
		}
		rights := acl.EvsAccess(subject, repoId, sidecar.AclTokenAudienceGkm, now).Rights
		role := ""
		switch {
		case rights.Has(sidecar.AclEvsAdmin):
			role = sidecar.GkmScopeRoleAdmin
		case rights.Has(sidecar.AclEvsWrite):
			role = sidecar.GkmScopeRoleWrite
		case rights.Has(sidecar.AclEvsRead):
			role = sidecar.GkmScopeRoleRead
		case rights.Has(sidecar.AclEvsDeposit):
			role = sidecar.GkmScopeRoleDeposit
		default:
			continue
		}
		scopes = append(scopes, sidecar.GkmRepoScope{RepoId: repoId, Role: role})
	}
	api.SetRepoScopes(scopes)
}

// The KeyDelivery group client, built at the first call once the network's identity is known.
func (self *t3WorldKeys) groupClient() (*sidecar.GkmGroupClient, error) {
	self.groupLock.Lock()
	defer self.groupLock.Unlock()
	group, closed := func() (*sidecar.GkmGroupClient, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.group, self.closed
	}()
	if closed {
		return nil, errT3WorldKeysClosed
	}
	if group != nil {
		return group, nil
	}
	api, hints, err := self.intake()
	if err != nil {
		return nil, err
	}
	groupArgs, err := self.running.GkmGroupClientArgs(api, nil)
	if err != nil {
		return nil, err
	}
	groupArgs.ReaderPrivateKey = self.readerPrivateKey
	groupArgs.ReaderKeySha256 = func() []byte {
		upkeep := func() *sidecar.GkmUpkeep {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return self.upkeep
		}()
		if upkeep == nil {
			return nil
		}
		return upkeep.CurrentReaderKeySha256()
	}
	groupArgs.ReaderRepoIds = self.readerRepoIds
	group, err = sidecar.NewGkmGroupClient(groupArgs)
	if err != nil {
		return nil, err
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.removeRecovery = self.running.InstallNetworkRecovery(group)
		self.group = group
		return true
	}()
	if !kept {
		return nil, errT3WorldKeysClosed
	}
	group.AttachHints(self.ctx, hints)
	switch state, configRepoId, confirmed := self.gkm.joiningState(); {
	case state == nil:
	case confirmed:
		group.AuthAdvanced(state)
	default:
		group.AuthUnconfirmed(state, configRepoId)
	}
	// built under groupLock, so this closes once
	close(self.groupBuiltSignal)
	return group, nil
}

// The private key of a ReaderKey this sidecar signed, by its digest, from the upkeep that keeps them.
func (self *t3WorldKeys) readerPrivateKey(keySha256 []byte) (hpke.PrivateKey, error) {
	upkeep := func() *sidecar.GkmUpkeep {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.upkeep
	}()
	if upkeep == nil {
		return nil, sidecar.ErrGkmKeyMissing
	}
	return upkeep.ReaderPrivateKey(keySha256)
}

// The repos this sidecar reads at the newest state the key client was given, where a Welcome's group is looked for:
// the config repo and each repo of the state's rows whose readers name it; while joining, the config repo alone.
func (self *t3WorldKeys) readerRepoIds() []sidecar.Id {
	view, joiningRepoId, confirmed := self.gkm.joiningState()
	if !confirmed && joiningRepoId != (sidecar.Id{}) {
		return []sidecar.Id{joiningRepoId}
	}
	identity := self.running.Identity()
	state, ok := view.(*sidecar.ConfigState)
	if !ok || identity == nil {
		return nil
	}
	candidates := []sidecar.Id{}
	if configRepoId, err := sidecar.IdFromBytes(state.Genesis().GetConfigRepoId()); err == nil {
		candidates = append(candidates, configRepoId)
	}
	if acl, err := state.Acl(); err == nil {
		for _, repo := range acl.Rows().GetRepos() {
			if repoId, err := sidecar.IdFromBytes(repo.GetRepoId()); err == nil {
				candidates = append(candidates, repoId)
			}
		}
	}
	repoIds := []sidecar.Id{}
	for _, repoId := range candidates {
		if !slices.Contains(repoIds, repoId) && slices.Contains(state.RepoReaders(repoId), identity.SidecarId) {
			repoIds = append(repoIds, repoId)
		}
	}
	return repoIds
}

// The follower's newest verified state, a start's own included, as the EvsClient's Config: nil before it holds one.
func (self *t3WorldKeys) verifiedConfig() sidecar.GkmAuthView {
	follower := self.running.ConfigFollower()
	if follower == nil {
		return nil
	}
	state := follower.VerifiedState()
	if state == nil {
		return nil
	}
	return state
}

// This device's epoch pin for a group it is a leaf of, which a push's sealed metadata carries for HeadCheck.
func (self *t3WorldKeys) groupHead(groupId sidecar.Id) (*protocol.GroupHead, bool) {
	group := self.builtGroup()
	if group == nil {
		return nil, false
	}
	return group.GroupHead(groupId)
}

// The device's EvsClient, built at the first call over the key client, judging statements at the follower's newest
// verified state, with its evs component on the Sidecar's generations, format 2 and the prefix rules bound as the
// daemon binds them.
func (self *t3WorldKeys) evsClient() (*sidecar.EvsClient, error) {
	self.evsLock.Lock()
	defer self.evsLock.Unlock()
	client, closed := func() (*sidecar.EvsClient, bool) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		return self.evs, self.closed
	}()
	if closed {
		return nil, errT3WorldKeysClosed
	}
	if client != nil {
		return client, nil
	}
	args, err := self.running.EvsClientArgs(self.gkm, self.verifiedConfig, self.fileStatement)
	if err != nil {
		return nil, err
	}
	args.GroupHead = self.groupHead
	client, err = sidecar.NewEvsClient(args)
	if err != nil {
		return nil, err
	}
	self.running.BindEvsFormat2(client)
	client.EnablePrefixRules(self.running.EvsPrefixRulesArgs())
	if _, err := self.running.AddComponent(sidecar.NewEvsComponent(client)); err != nil {
		client.Close()
		return nil, err
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.closed {
			return false
		}
		self.evs = client
		return true
	}()
	if !kept {
		return nil, errT3WorldKeysClosed
	}
	return client, nil
}

// The head reader over the config repo genesis names, or, with nil, the one the follower's newest verified state's
// genesis names, built at the first call on the EvsClient. A read it cannot make is unavailable, which withholds the
// base.
func (self *t3WorldKeys) headReader(genesis *bridge.Genesis) (*sidecar.EvsConfigHeadReader, error) {
	unavailable := func(what string, err error) error {
		if err == nil {
			return fmt.Errorf("%w: %s", sidecar.ErrConfigHeadUnavailable, what)
		}
		return fmt.Errorf("%w: %s: %w", sidecar.ErrConfigHeadUnavailable, what, err)
	}
	if genesis == nil {
		follower := self.running.ConfigFollower()
		if follower == nil {
			return nil, unavailable("no config follower runs", nil)
		}
		state := follower.VerifiedState()
		if state == nil {
			return nil, unavailable("no verified state names the config repo yet", nil)
		}
		genesis = state.Genesis()
	}
	configRepoId, err := sidecar.IdFromBytes(genesis.GetConfigRepoId())
	if err != nil {
		return nil, unavailable("the genesis names no config repo", err)
	}
	client, err := self.evsClient()
	if err != nil {
		return nil, unavailable("no evs client", err)
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.reader != nil {
		if self.readerRepoId != configRepoId {
			return nil, unavailable("another config repo than the one this network's reader reads", nil)
		}
		return self.reader, nil
	}
	reader, err := sidecar.NewEvsConfigHeadReader(&sidecar.EvsConfigHeadReaderArgs{Repo: client.Repo(configRepoId)})
	if err != nil {
		return nil, unavailable("the head reader", err)
	}
	self.reader, self.readerRepoId = reader, configRepoId
	return reader, nil
}

// A device's follower settings with real keys, as ur daemon's (urConfigFollowerSettings): a login device trusts the
// first genesis api serves where enrollment recorded none, the first owner's bootstrap starting from there, and keeps
// the log's whole tail with its spend check.
func t3WorldKeysFollowerSettings() *sidecar.SidecarConfigFollowerSettings {
	settings := sidecar.DefaultSidecarConfigFollowerSettings()
	settings.TrustFirstUse = true
	settings.ConfigClient = sidecar.DefaultConfigClientSettings()
	settings.ConfigClient.RetainFullTail = true
	settings.ConfigClient.LogSettings = sidecar.DefaultConfigLogSettings()
	settings.ConfigClient.LogSettings.AclSpendCheck = sidecar.NewSpendCheck(nil)
	return settings
}

// The component on the Sidecar: each client generation's gkm hints go to the key client's intake, and the Sidecar's
// close ends the clients.
type t3WorldKeysComponent struct {
	keys *t3WorldKeys
}

func (self *t3WorldKeysComponent) Name() string {
	return t3WorldKeysComponentName
}

// One generation: the intake built if it is not, the group client too where it can be, and the generation's gkm
// notices handed to the intake until it ends.
func (self *t3WorldKeysComponent) Attach(env *sidecar.SidecarEnv) error {
	_, hints, err := self.keys.intake()
	if err != nil {
		return err
	}
	if _, err := self.keys.groupClient(); err != nil && self.keys.log.V(1).Enabled() {
		self.keys.log.Infof("[t3]a device's group client is not built yet: %s", err)
	}
	if env.Subprotocol == nil {
		return nil
	}
	for _, topic := range []string{sidecar.PushTopicGkm, sidecar.PushTopicGkmGroupPrefix} {
		unsubscribe, err := env.Subprotocol.Subscribe(topic, hints.Receive)
		if err != nil {
			continue
		}
		go func() {
			<-env.Ctx.Done()
			unsubscribe()
		}()
	}
	return nil
}

func (self *t3WorldKeysComponent) Close() {
	self.keys.close()
}

// The key client a device's follower and EvsClient take (the file's header). It keeps the newest verified state given
// and, while the device joins its network, the follower's newest rows-only state.
type t3WorldKeysGkm struct {
	keys *t3WorldKeys

	stateLock               sync.Mutex
	newest                  sidecar.GkmAuthView
	unconfirmed             sidecar.GkmAuthView
	unconfirmedConfigRepoId sidecar.Id
}

var (
	_ sidecar.GkmClient       = (*t3WorldKeysGkm)(nil)
	_ sidecar.GkmGroupDrafter = (*t3WorldKeysGkm)(nil)
	_ sidecar.GkmConfigJoiner = (*t3WorldKeysGkm)(nil)
)

// This host forms and follows groups through their signed generation and KeyDelivery wraps.
func (self *t3WorldKeysGkm) KeyDeliveryOnly() bool { return true }

// The newest verified state given, else the newest unconfirmed one with the config repo it serves; confirmed says
// which. A nil state before either.
func (self *t3WorldKeysGkm) joiningState() (state sidecar.GkmAuthView, configRepoId sidecar.Id, confirmed bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.newest != nil {
		return self.newest, sidecar.Id{}, true
	}
	return self.unconfirmed, self.unconfirmedConfigRepoId, false
}

// The follower's rows-only state before it stands (QD-817), kept unless a verified or newer unconfirmed one was given,
// and handed to the group client for the config group's join alone. Never blocks on I/O.
func (self *t3WorldKeysGkm) AuthUnconfirmed(state sidecar.GkmAuthView, configRepoId sidecar.Id) {
	if state == nil {
		return
	}
	kept := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.newest != nil || self.unconfirmed != nil && state.Pin().GetVersion() <= self.unconfirmed.Pin().GetVersion() {
			return false
		}
		self.unconfirmed, self.unconfirmedConfigRepoId = state, configRepoId
		return true
	}()
	if !kept {
		return
	}
	self.keys.setRepoScopes()
	if group, err := self.group(); err == nil {
		self.keys.stateLock.Lock()
		upkeep := self.keys.upkeep
		self.keys.stateLock.Unlock()
		if upkeep != nil {
			upkeep.Refresh()
		}
		group.AuthUnconfirmed(state, configRepoId)
	}
}

// Whether the keychain holds a generation of the repo, and a channel closed once that may have changed: the group
// client's answer where it is built, else the keychain read with no key client and a channel closed once it is.
func (self *t3WorldKeysGkm) RepoKeyHeld(repoId sidecar.Id) (bool, <-chan struct{}) {
	// armed before the look, so a build between them wakes the follower to ask the group client
	built := self.keys.groupBuiltSignal
	if group := self.keys.builtGroup(); group != nil {
		return group.RepoKeyHeld(repoId)
	}
	return self.keys.running.GkmRepoKeyHeld(repoId), built
}

// The group client, building it where it is not built yet.
func (self *t3WorldKeysGkm) group() (*sidecar.GkmGroupClient, error) {
	group, err := self.keys.groupClient()
	switch {
	case err == nil:
		return group, nil
	case errors.Is(err, sidecar.ErrGkmUnavailable):
		return nil, err
	}
	return nil, fmt.Errorf("%w: %w", sidecar.ErrGkmUnavailable, err)
}

// Keeps state when it is newer than the one held, and hands it to the group client. Never blocks on I/O.
func (self *t3WorldKeysGkm) AuthAdvanced(state sidecar.GkmAuthView) {
	if state == nil {
		return
	}
	advanced := func() bool {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		if self.newest != nil && state.Pin().GetVersion() <= self.newest.Pin().GetVersion() {
			return false
		}
		self.newest = state
		self.unconfirmed, self.unconfirmedConfigRepoId = nil, sidecar.Id{}
		return true
	}()
	if !advanced {
		return
	}
	self.keys.setRepoScopes()
	if group, err := self.group(); err == nil {
		group.AuthAdvanced(state)
	}
}

func (self *t3WorldKeysGkm) CurrentRepoKey(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.CurrentRepoKey(ctx, repoId)
}

func (self *t3WorldKeysGkm) RepoKey(ctx context.Context, repoId sidecar.Id, generation uint64) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.RepoKey(ctx, repoId, generation)
}

func (self *t3WorldKeysGkm) EnsureWritable(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.EnsureWritable(ctx, repoId)
}

func (self *t3WorldKeysGkm) DropKey(ctx context.Context, repoId sidecar.Id) (*sidecar.GkmDropKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.DropKey(ctx, repoId)
}

func (self *t3WorldKeysGkm) CreateGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.CreateGroup(ctx, args)
}

// KF1's bootstrap draft (QD-824), for the network's first device before base 1.
func (self *t3WorldKeysGkm) DraftGroup(ctx context.Context, args *sidecar.GkmCreateGroupArgs) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.DraftGroup(ctx, args)
}

func (self *t3WorldKeysGkm) RotateRepo(ctx context.Context, repoId sidecar.Id) (*sidecar.EvsRepoKey, error) {
	group, err := self.group()
	if err != nil {
		return nil, err
	}
	return group.RotateRepo(ctx, repoId)
}

func (self *t3WorldKeysGkm) ReformGroup(ctx context.Context, args *sidecar.GkmReformArgs) (sidecar.Id, error) {
	group, err := self.group()
	if err != nil {
		return sidecar.Id{}, err
	}
	return group.ReformGroup(ctx, args)
}

func (self *t3WorldKeysGkm) ForgetRepo(ctx context.Context, repoId sidecar.Id) error {
	group, err := self.group()
	if err != nil {
		return err
	}
	return group.ForgetRepo(ctx, repoId)
}

// The config heads a device's follower reads: the EvsConfigHeadReader over its EvsClient (the file's header), and its
// ref for a confirmed ACL document's save, which a device the log names a config writer saves through.
type t3WorldKeysHeads struct {
	keys *t3WorldKeys
}

var _ sidecar.ConfigHeadAnchorReader = (*t3WorldKeysHeads)(nil)
var _ sidecar.ConfigHeadTipReader = (*t3WorldKeysHeads)(nil)

func (self *t3WorldKeysHeads) CurrentConfigHead(ctx context.Context) (*sidecar.ConfigHead, uint64, [sha256.Size]byte, error) {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return nil, 0, [sha256.Size]byte{}, err
	}
	return reader.CurrentConfigHead(ctx)
}

func (self *t3WorldKeysHeads) CachedConfigHead(version uint64, chain []byte) *sidecar.ConfigHead {
	self.keys.stateLock.Lock()
	reader := self.keys.reader
	self.keys.stateLock.Unlock()
	if reader == nil {
		return nil
	}
	return reader.CachedConfigHead(version, chain)
}

func (self *t3WorldKeysHeads) ReadConfigHead(ctx context.Context, configVersion uint64, configHeadSha256 []byte) (*sidecar.ConfigHead, error) {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return nil, err
	}
	return reader.ReadConfigHead(ctx, configVersion, configHeadSha256)
}

func (self *t3WorldKeysHeads) ReadConfigHeadAtAnchor(ctx context.Context, configVersion uint64, configHeadSha256 []byte, anchor sidecar.ConfigHeadAnchor) (*sidecar.ConfigHead, error) {
	if anchor == nil {
		return nil, fmt.Errorf("%w: no base rows to judge the anchor's pushes by", sidecar.ErrConfigHeadUnavailable)
	}
	reader, err := self.keys.headReader(anchor.Genesis())
	if err != nil {
		return nil, err
	}
	return reader.ReadConfigHeadAtAnchor(ctx, configVersion, configHeadSha256, anchor)
}

// The confirmed ACL save's ref on evs (sidecar/cli/daemon_acl_writer.go's): the stock follower is a reader, and only an
// explicit ACL confirmation selects this writer, which saves through the head reader's own ref.
func (self *t3WorldKeysHeads) ConfirmedAclConfigRef() sidecar.ConfigHeadRef {
	return &t3WorldKeysAclRef{t3WorldKeysHeads: self}
}

func (self *t3WorldKeysHeads) CurrentConfigHeadTip(ctx context.Context) (*sidecar.ConfigHeadTip, error) {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return nil, err
	}
	return reader.CurrentConfigHeadTip(ctx)
}

type t3WorldKeysAclRef struct {
	*t3WorldKeysHeads
}

func (self *t3WorldKeysAclRef) PrepareConfigHead(ctx context.Context, head *sidecar.ConfigHead, version uint64) (*sidecar.ConfigHeadPush, error) {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return nil, err
	}
	return reader.ConfirmedAclConfigRef().PrepareConfigHead(ctx, head, version)
}

func (self *t3WorldKeysAclRef) CommitConfigHead(ctx context.Context, push *sidecar.ConfigHeadPush) error {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return err
	}
	return reader.ConfirmedAclConfigRef().CommitConfigHead(ctx, push)
}

func (self *t3WorldKeysAclRef) ConfigHeadPushes(ctx context.Context, after uint64, through uint64) ([][]byte, error) {
	reader, err := self.keys.headReader(nil)
	if err != nil {
		return nil, err
	}
	return reader.ConfirmedAclConfigRef().ConfigHeadPushes(ctx, after, through)
}

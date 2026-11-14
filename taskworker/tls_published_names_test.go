package taskworker

// §21.6's published-names source over the gate cache (QD-770): over a network with a label and an alias the list
// gives each grant of the state under each name, with the audience and expiry PublishedNames reads; a network the
// cache does not hold is asked for and read once its install lands, at the ask or after it; a network whose state
// never loads fails the page rather than list nothing; the newest runtime's source is the one installed, an older
// runtime's end removing none; and the runtime's gate cache loads a network the source asks for from the log. No
// sleeps: each load the cache's install callback reports is awaited, a time guard only failing a broken test.

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The gate cache as a test holds it: each network's installed state, the loads asked for, and what an ask installs.
type tlsPublishedNamesTestStates struct {
	stateLock sync.Mutex
	snapshots map[sidecar.Id]*sidecar.AclGateSnapshot
	// what an ask for a network installs; a network with none loads nothing
	onAsk map[sidecar.Id]*sidecar.AclGateSnapshot
	// when set, an ask runs this in place of installing at once
	askHook   func(networkId sidecar.Id)
	asked     []sidecar.Id
	callbacks map[int]sidecar.AclGateInstallFunction
	nextIndex int
}

var _ tlsPublishedNamesStates = (*tlsPublishedNamesTestStates)(nil)
var _ tlsPublishedNamesStates = (*sidecar.AclGateCache)(nil)

func newTlsPublishedNamesTestStates() *tlsPublishedNamesTestStates {
	return &tlsPublishedNamesTestStates{
		snapshots: map[sidecar.Id]*sidecar.AclGateSnapshot{},
		onAsk:     map[sidecar.Id]*sidecar.AclGateSnapshot{},
		callbacks: map[int]sidecar.AclGateInstallFunction{},
	}
}

func (self *tlsPublishedNamesTestStates) State(networkId sidecar.Id) (*sidecar.AclGateSnapshot, sidecar.AclGateStatus) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	snapshot := self.snapshots[networkId]
	if snapshot == nil {
		return nil, sidecar.AclGateUnknown
	}
	return snapshot, sidecar.AclGateLoaded
}

func (self *tlsPublishedNamesTestStates) Changed(networkId sidecar.Id) {
	hook, snapshot := func() (func(sidecar.Id), *sidecar.AclGateSnapshot) {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.asked = append(self.asked, networkId)
		return self.askHook, self.onAsk[networkId]
	}()
	if hook != nil {
		hook(networkId)
		return
	}
	if snapshot != nil {
		self.install(networkId, snapshot)
	}
}

func (self *tlsPublishedNamesTestStates) AddInstallCallback(callback sidecar.AclGateInstallFunction) func() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	index := self.nextIndex
	self.nextIndex += 1
	self.callbacks[index] = callback
	return func() {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		delete(self.callbacks, index)
	}
}

// Installs a network's state and tells the callbacks, as the cache's load does.
func (self *tlsPublishedNamesTestStates) install(networkId sidecar.Id, snapshot *sidecar.AclGateSnapshot) {
	callbacks := func() []sidecar.AclGateInstallFunction {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.snapshots[networkId] = snapshot
		callbacks := []sidecar.AclGateInstallFunction{}
		for _, callback := range self.callbacks {
			callbacks = append(callbacks, callback)
		}
		return callbacks
	}()
	for _, callback := range callbacks {
		callback(networkId, snapshot)
	}
}

// The loads asked for, in order.
func (self *tlsPublishedNamesTestStates) askedNetworks() []sidecar.Id {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.asked)
}

// How many install callbacks are registered.
func (self *tlsPublishedNamesTestStates) callbackCount() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return len(self.callbacks)
}

// A network's verified state in which one device publishes blog on 443, public and with no end, and shop on 8443 to
// members until ends.
func tlsPublishedNamesTestSnapshot(t testing.TB, ends time.Time) *sidecar.AclGateSnapshot {
	t.Helper()
	view := sidecartest.NewFakeAclView()
	device := view.AddDevice(view.AddMember(bridge.MemberRole_MEMBER_ROLE_OWNER))
	atom := sidecartest.FakeAclSidecarAtom(device)
	rule := func(host string, port int, audience protocol.AclAudience, expires time.Time) *protocol.AclRule {
		rule := &protocol.AclRule{
			RuleId:   sidecar.NewId().Bytes(),
			SrcAtoms: [][]byte{atom.Atom},
			Targets:  []*protocol.AclTarget{{PublicName: host}, {PublicName: "tls/" + strconv.Itoa(port) + "/" + host}},
			Actions:  []uint32{uint32(protocol.AclAction_ACL_ACTION_PUBLISH), uint32(protocol.AclAction_ACL_ACTION_LISTEN)},
			Audience: audience,
		}
		if !expires.IsZero() {
			rule.Expires = expires.UnixMilli()
		}
		return rule
	}
	rows := &protocol.AclRows{
		HasDocument: true,
		Atoms:       []*protocol.AclAtom{atom},
		Rules: []*protocol.AclRule{
			rule("blog", 443, protocol.AclAudience_ACL_AUDIENCE_PUBLIC, time.Time{}),
			rule("shop", 8443, protocol.AclAudience_ACL_AUDIENCE_MEMBERS, ends),
		},
	}
	state, err := sidecar.NewAclState(rows, view)
	if err != nil {
		t.Fatal(err)
	}
	return &sidecar.AclGateSnapshot{Pin: view.Pin(), Acl: state}
}

// A network with a log, as api's append path leaves its head.
func tlsPublishedNamesTestHead(ctx context.Context, networkId server.Id) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO network_acl_head (network_id, version, chain) VALUES ($1, 1, $2)`,
			networkId,
			make([]byte, 32),
		))
	})
}

// A dns_label row of a network's.
func tlsPublishedNamesTestLabel(ctx context.Context, root string, label string, networkId server.Id, kind string) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`INSERT INTO dns_label (root, label, network_id, kind, state, state_time) VALUES ($1, $2, $3, $4, $5, $6)`,
			root,
			label,
			networkId,
			kind,
			model.DnsLabelStateLive,
			server.NowUtc(),
		))
	})
}

// The list over the source for a network with a label and an alias: each grant of its state under each, with the
// state's audience and expiry, the grant's served names in full, and the list's order.
func TestTlsPublishedNamesSourceOverANetworkWithALabelAndAnAlias(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		ends := now.Add(time.Hour).Truncate(time.Millisecond)
		networkId := server.NewId()
		tlsPublishedNamesTestHead(ctx, networkId)
		tlsPublishedNamesTestLabel(ctx, "ur.example.", "abcdefghij", networkId, model.DnsLabelKindLabel)
		tlsPublishedNamesTestLabel(ctx, "ur.example.", "acme", networkId, model.DnsLabelKindAlias)
		states := newTlsPublishedNamesTestStates()
		states.snapshots[sidecar.Id(networkId)] = tlsPublishedNamesTestSnapshot(t, ends)
		remove := installTlsPublishedNamesSourceWithSettings(states, &tlsPublishedNamesSourceSettings{})
		defer remove()

		names, cursor, err := model.ListTlsPublishedNames(ctx, nil, now)
		if err != nil || cursor != nil {
			t.Fatalf("the list answered %v, %v", cursor, err)
		}
		got := []string{}
		for _, name := range names {
			if name.NetworkId != networkId {
				t.Fatalf("a name of another network")
			}
			expires := ""
			if !name.ExpireTime.IsZero() {
				expires = " " + strconv.FormatInt(name.ExpireTime.UnixMilli(), 10)
			}
			got = append(got, name.Host+":"+strconv.Itoa(name.Port)+" "+name.Label+" "+name.Name+" "+name.Audience.String()+expires)
		}
		endsText := " " + strconv.FormatInt(ends.UnixMilli(), 10)
		want := []string{
			"blog:443 abcdefghij blog.abcdefghij.ur.example ACL_AUDIENCE_PUBLIC",
			"blog:443 acme blog.acme.ur.example ACL_AUDIENCE_PUBLIC",
			"shop:8443 abcdefghij shop.abcdefghij.ur.example ACL_AUDIENCE_MEMBERS" + endsText,
			"shop:8443 acme shop.acme.ur.example ACL_AUDIENCE_MEMBERS" + endsText,
		}
		if !slices.Equal(got, want) {
			t.Fatalf("listed\n%v\nwant\n%v", got, want)
		}
		if asked := states.askedNetworks(); len(asked) != 0 {
			t.Fatalf("a loaded network asked for again: %v", asked)
		}

		// past the shop grant's end, the list drops it
		names, _, err = model.ListTlsPublishedNames(ctx, nil, ends)
		if err != nil || len(names) != 2 || names[0].Host != "blog" || names[1].Host != "blog" {
			t.Fatalf("at the end of shop's grant: %d names, %v", len(names), err)
		}
	})
}

// A network whose state never loads fails the page, never listing no names for it: once the cache was asked for it,
// at once with no wait, and after the wait with one.
func TestTlsPublishedNamesSourceFailsThePageOfANetworkItCannotRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		if !server.SidecarMg2Linked() {
			return
		}
		ctx := context.Background()
		now := server.NowUtc()
		loadedId, unreadableId := server.NewId(), server.NewId()
		tlsPublishedNamesTestHead(ctx, loadedId)
		tlsPublishedNamesTestHead(ctx, unreadableId)
		states := newTlsPublishedNamesTestStates()
		states.snapshots[sidecar.Id(loadedId)] = tlsPublishedNamesTestSnapshot(t, now.Add(time.Hour))
		remove := installTlsPublishedNamesSourceWithSettings(states, &tlsPublishedNamesSourceSettings{})
		defer remove()

		names, cursor, err := model.ListTlsPublishedNames(ctx, nil, now)
		if !errors.Is(err, errTlsPublishedNamesNotLoaded) || names != nil || cursor != nil {
			t.Fatalf("the list answered %d names, %v, %v", len(names), cursor, err)
		}
		if asked := states.askedNetworks(); !slices.Equal(asked, []sidecar.Id{sidecar.Id(unreadableId)}) {
			t.Fatalf("asked for %v", asked)
		}
		if states.callbackCount() != 0 {
			t.Fatal("the source left an install callback behind")
		}
	})
}

// A network the cache does not hold is asked for, and read once its install lands: at the ask, or after it while the
// source waits. With no wait, one the ask does not install fails at once.
func TestTlsPublishedNamesSourceLoadsANetworkItDoesNotHold(t *testing.T) {
	ctx := t.Context()
	networkId := sidecar.NewId()
	snapshot := tlsPublishedNamesTestSnapshot(t, time.Time{})

	states := newTlsPublishedNamesTestStates()
	states.onAsk[networkId] = snapshot
	source := newTlsPublishedNamesSource(states, &tlsPublishedNamesSourceSettings{})
	if got, err := source.state(ctx, networkId); err != nil || got != snapshot {
		t.Fatalf("an install at the ask: %v", err)
	}

	// the install lands after the ask returned, while the source waits
	states = newTlsPublishedNamesTestStates()
	asked := make(chan struct{})
	release := make(chan struct{})
	installed := make(chan struct{})
	states.askHook = func(askedId sidecar.Id) {
		close(asked)
		go func() {
			defer close(installed)
			<-release
			states.install(askedId, snapshot)
		}()
	}
	// a guard, never the proof
	source = newTlsPublishedNamesSource(states, &tlsPublishedNamesSourceSettings{LoadWait: time.Minute})
	type answer struct {
		snapshot *sidecar.AclGateSnapshot
		err      error
	}
	answers := make(chan answer, 1)
	go func() {
		got, err := source.state(ctx, networkId)
		answers <- answer{snapshot: got, err: err}
	}()
	<-asked
	select {
	case <-answers:
		t.Fatal("the source answered before the install")
	default:
	}
	close(release)
	<-installed
	if got := <-answers; got.err != nil || got.snapshot != snapshot {
		t.Fatalf("an install after the ask: %v", got.err)
	}
	if states.callbackCount() != 0 {
		t.Fatal("the source left an install callback behind")
	}

	// no install and no wait: the network fails, once asked for
	states = newTlsPublishedNamesTestStates()
	source = newTlsPublishedNamesSource(states, &tlsPublishedNamesSourceSettings{})
	if _, err := source.state(ctx, networkId); !errors.Is(err, errTlsPublishedNamesNotLoaded) {
		t.Fatalf("a network that never loads: %v", err)
	}
	if asked := states.askedNetworks(); !slices.Equal(asked, []sidecar.Id{networkId}) {
		t.Fatalf("asked for %v", asked)
	}

	// a waiting source ends with its caller
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	source = newTlsPublishedNamesSource(states, &tlsPublishedNamesSourceSettings{LoadWait: time.Minute})
	if _, err := source.state(canceled, networkId); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled wait: %v", err)
	}
}

// The newest runtime's source is the process's, an older runtime's end removing none, and none is left once the newest
// ends, when the list answers unavailable.
func TestTlsPublishedNamesSourceIsTheNewestRuntimes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		current := func() *tlsPublishedNamesSource {
			tlsPublishedNamesInstall.stateLock.Lock()
			defer tlsPublishedNamesInstall.stateLock.Unlock()
			return tlsPublishedNamesInstall.current
		}
		older, newer := newTlsPublishedNamesTestStates(), newTlsPublishedNamesTestStates()
		removeOlder := installTlsPublishedNamesSource(older)
		removeNewer := installTlsPublishedNamesSource(newer)
		removeOlder()
		if source := current(); source == nil || source.states != newer {
			t.Fatal("the older runtime's end removed the newer runtime's source")
		}
		if server.SidecarMg2Linked() {
			if _, _, err := model.ListTlsPublishedNames(ctx, nil, server.NowUtc()); err != nil {
				t.Fatalf("with the newer runtime's source: %v", err)
			}
		}
		removeNewer()
		if current() != nil {
			t.Fatal("a source after the newest runtime's end")
		}
		if _, _, err := model.ListTlsPublishedNames(ctx, nil, server.NowUtc()); !errors.Is(err, model.ErrTlsPublishedNamesUnavailable) {
			t.Fatalf("with no source: %v", err)
		}
	})
}

// The runtime's gate cache is the source's (gkmReconcilerStateFromHost), and a network it does not hold, whose log has
// not moved since it started, is loaded from the log when the source asks for it, at the log's tip.
func TestTlsPublishedNamesSourceLoadsThroughTheRuntimesGateCache(t *testing.T) {
	net := sidecartest.NewNet(t.Context(), nil)
	defer net.Close()
	tier, err := net.AdmissionTier()
	if err != nil {
		t.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	admission := sidecartest.NewAdmissionNet(t, tier)
	defer admission.Close()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	if _, err := gkmReconcilerStateFromHost(ctx, net.Host()); err != nil {
		t.Fatal(err)
	}
	source := func() *tlsPublishedNamesSource {
		tlsPublishedNamesInstall.stateLock.Lock()
		defer tlsPublishedNamesInstall.stateLock.Unlock()
		return tlsPublishedNamesInstall.current
	}()
	if source == nil {
		t.Fatal("the runtime installed no published-names source")
	}
	cache, ok := source.states.(*sidecar.AclGateCache)
	if !ok {
		t.Fatalf("the source reads %T, not the runtime's gate cache", source.states)
	}
	networkId := admission.NetworkId()
	if _, status := cache.State(networkId); status == sidecar.AclGateLoaded {
		t.Fatal("the network was loaded before any ask")
	}
	snapshot, err := source.state(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.Equal(snapshot.Pin, admission.Tip()) {
		t.Fatalf("loaded at %v, the log's tip is %v", snapshot.Pin, admission.Tip())
	}
	if names := snapshot.Acl.PublishedNames(net.Host().ClockOrReal().Now()); names == nil {
		t.Fatal("the loaded state lists no names, not even an empty list")
	}
}

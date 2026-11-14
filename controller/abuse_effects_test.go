package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21's effects (abuse_effects.go) on a fake of each owner: TLS's kill switch,
// DNS's suspensions, certs' revocation request, the contract layer's close,
// network_abuse_state and the hint writer. The fake holds what each owner would
// hold and the order every call came in, and it has no call that appends to the
// authorization log or writes rev/, so a test that lists every call an effect
// made proves it made no other.

const testAbuseEffectRoot = "root.example"

// Every owner's state, and every call in order.
type testAbuseOwners struct {
	stateLock sync.Mutex

	networkId server.Id
	// the label and an alias that name the network under the root, and a label
	// that names another
	label      string
	alias      string
	otherLabel string

	ops []string
	// host to reason
	tlsKills map[string]string
	// host to until
	tlsUntils     map[string]time.Time
	zoneSuspended bool
	zoneReason    string
	dnsHosts      map[string]bool
	revocations   []*model.TlsCertRevocationRequest
	// network_abuse_state's row: "", restricted or suspended
	state           string
	stateCaseId     server.Id
	restrictedUntil *time.Time
	subjectIds      []server.Id
	closedIds       []server.Id
	hintClientIds   []server.Id
	hints           []*sidecar.ClientHint
	standing        []*model.AbuseAction
	now             time.Time

	// fails the next call whose op starts with the key, once
	failures map[string]error
	// raises instead of answering, for the next call whose op starts with it
	raise string
	// called at each step, with no lock held
	step func(step string, detail string)
}

func newTestAbuseOwners() *testAbuseOwners {
	return &testAbuseOwners{
		networkId:     server.NewId(),
		label:         "abcdefghij",
		alias:         "shopfront",
		otherLabel:    "zyxwvutsrq",
		tlsKills:      map[string]string{},
		tlsUntils:     map[string]time.Time{},
		dnsHosts:      map[string]bool{},
		hintClientIds: []server.Id{server.NewId(), server.NewId()},
		now:           time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC),
		failures:      map[string]error{},
	}
}

// Records the call and answers its failure, if one is set.
func (self *testAbuseOwners) op(op string) error {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.ops = append(self.ops, op)
	if self.raise != "" && strings.HasPrefix(op, self.raise) {
		self.raise = ""
		panic(errors.New("the owner's store did not answer"))
	}
	for prefix, err := range self.failures {
		if strings.HasPrefix(op, prefix) {
			delete(self.failures, prefix)
			return err
		}
	}
	return nil
}

func (self *testAbuseOwners) deps() *AbuseEffectsDeps {
	return &AbuseEffectsDeps{
		SetTlsSuspension: func(ctx context.Context, args *sidecar.TlsSuspendArgs) error {
			if err := self.op("tls.set " + args.Host); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.tlsKills[args.Host] = args.Reason
			self.tlsUntils[args.Host] = args.Until
			return nil
		},
		LiftTlsSuspension: func(ctx context.Context, networkId server.Id, host string) error {
			if err := self.op("tls.lift " + host); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			delete(self.tlsKills, host)
			delete(self.tlsUntils, host)
			return nil
		},
		SuspendZone: func(ctx context.Context, networkId server.Id, reason string, until *time.Time) error {
			if err := self.op("dns.zone"); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.zoneSuspended = true
			self.zoneReason = reason
			return nil
		},
		SuspendHost: func(ctx context.Context, networkId server.Id, host string) error {
			if err := self.op("dns.host " + host); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.dnsHosts[host] = true
			return nil
		},
		RestoreDns: func(ctx context.Context, networkId server.Id, host string) error {
			if err := self.op("dns.restore " + host); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if host == "" {
				self.zoneSuspended = false
				self.zoneReason = ""
			} else {
				delete(self.dnsHosts, host)
			}
			return nil
		},
		RequestTlsCertRevocation: func(ctx context.Context, request *model.TlsCertRevocationRequest) (int, error) {
			if err := self.op("cert.revoke"); err != nil {
				return 0, err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.revocations = append(self.revocations, request)
			return 1, nil
		},
		NetworkSubjects: func(ctx context.Context, networkId server.Id, after server.Id, limit int) ([]server.Id, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			subjectIds := []server.Id{}
			for _, subjectId := range self.subjectIds {
				if 0 < subjectId.Cmp(after) {
					subjectIds = append(subjectIds, subjectId)
				}
			}
			if limit < len(subjectIds) {
				return subjectIds[:limit], true
			}
			return subjectIds, false
		},
		CloseSubjectContracts: func(ctx context.Context, networkId server.Id, subjectId server.Id) error {
			if err := self.op("close " + subjectId.String()); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if !slices.Contains(self.closedIds, subjectId) {
				self.closedIds = append(self.closedIds, subjectId)
			}
			return nil
		},
		SetNetworkSuspended: func(ctx context.Context, networkId server.Id, caseId server.Id, now time.Time) error {
			if err := self.op("state.suspend"); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.state = "suspended"
			self.stateCaseId = caseId
			self.restrictedUntil = nil
			return nil
		},
		EndNetworkSuspended: func(ctx context.Context, networkId server.Id, restrictedUntil *time.Time, now time.Time) (bool, error) {
			if err := self.op("state.end"); err != nil {
				return false, err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if self.state != "suspended" {
				return false, nil
			}
			if restrictedUntil != nil && now.Before(*restrictedUntil) {
				self.state = "restricted"
				self.restrictedUntil = restrictedUntil
			} else {
				self.state = ""
			}
			return true, nil
		},
		StandingActions: func(ctx context.Context, networkId server.Id, now time.Time) []*model.AbuseAction {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			return slices.Clone(self.standing)
		},
		ResolveLabel: func(ctx context.Context, root string, label string) (server.Id, error) {
			if root != testAbuseEffectRoot {
				return server.Id{}, sidecar.ErrNetworkLabelUnknown
			}
			switch label {
			case self.label, self.alias:
				return self.networkId, nil
			case self.otherLabel:
				return server.NewId(), nil
			}
			return server.Id{}, sidecar.ErrNetworkLabelUnknown
		},
		RootNames: func() []string {
			return []string{testAbuseEffectRoot}
		},
		HintClientIds: func(ctx context.Context, networkId server.Id, limit int) []server.Id {
			return self.hintClientIds
		},
		Hint: func(ctx context.Context, clientIds []server.Id, hint *sidecar.ClientHint) error {
			if err := self.op("hint"); err != nil {
				return err
			}
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			self.hints = append(self.hints, hint)
			return nil
		},
		Now: func() time.Time {
			return self.now
		},
		Step: func(step string, detail string) {
			if self.step != nil {
				self.step(step, detail)
			}
		},
	}
}

// Every call so far, and forgets them.
func (self *testAbuseOwners) takeOps() []string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	ops := self.ops
	self.ops = nil
	return ops
}

// What the owners hold, as one line a test compares.
func (self *testAbuseOwners) held() string {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	kills := []string{}
	for host, reason := range self.tlsKills {
		kills = append(kills, host+"="+reason)
	}
	slices.Sort(kills)
	hosts := []string{}
	for host := range self.dnsHosts {
		hosts = append(hosts, host)
	}
	slices.Sort(hosts)
	return fmt.Sprintf(
		"state=%s kills=%v zone=%t hosts=%v revocations=%d closed=%d",
		self.state, kills, self.zoneSuspended, hosts, len(self.revocations), len(self.closedIds),
	)
}

// An action of the fake's network under a new case.
func (self *testAbuseOwners) action(kind model.AbuseActionKind, target string) *model.AbuseAction {
	networkId := self.networkId
	return &model.AbuseAction{
		ActionId:    server.NewId(),
		CaseId:      server.NewId(),
		NetworkId:   &networkId,
		Category:    model.AbuseCategoryPhishing,
		Kind:        kind,
		Target:      target,
		ByMachine:   true,
		ReviewState: model.AbuseReviewStatePending,
		CreateTime:  self.now,
	}
}

// The lift of action, reviewed to reviewState, as the effects take it.
func testAbuseLiftEffect(action *model.AbuseAction, reviewState model.AbuseReviewState) *AbuseEffect {
	lifted := *action
	lifted.ReviewState = reviewState
	return &AbuseEffect{
		Action: &model.AbuseAction{
			ActionId:  server.NewId(),
			CaseId:    action.CaseId,
			NetworkId: action.NetworkId,
			Category:  action.Category,
			Kind:      model.AbuseActionKindLift,
			Target:    action.ActionId.String(),
		},
		Lifted: &lifted,
	}
}

func testAbuseHostName(owners *testAbuseOwners, host string) string {
	return host + "." + owners.label + "." + testAbuseEffectRoot
}

// A host kill sets the TLS kill before the DNS write, under a reason that holds
// the case id and no free text, carries the action's until to the kill, and
// prompts the network's sidecars once.
func TestAbuseEffectHostKillKillsBeforeDns(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	action := owners.action(model.AbuseActionKindHostKill, testAbuseHostName(owners, "Shop")+".")
	until := owners.now.Add(48 * time.Hour)
	action.UntilTime = &until

	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"tls.set shop", "dns.host shop", "hint"}) {
		t.Fatalf("the calls were %v", ops)
	}
	reason := "abuse:" + action.CaseId.String()
	if owners.tlsKills["shop"] != reason {
		t.Fatalf("the kill's reason reads %q", owners.tlsKills["shop"])
	}
	if !owners.tlsUntils["shop"].Equal(until) {
		t.Fatalf("the kill's until reads %s", owners.tlsUntils["shop"])
	}
	if len(owners.hints) != 1 || owners.hints[0].Topic != sidecar.PushTopicCore ||
		owners.hints[0].Version != uint64(owners.now.UnixMicro()) || len(owners.hints[0].Body) != 0 {
		t.Fatalf("the hint reads %+v", owners.hints)
	}
}

// The TLS kill and the DNS suspension are separate systems: a DNS write that
// fails leaves the kill standing and the effect failed, with no hint, and the
// retry finishes the rest.
func TestAbuseEffectDnsFailureLeavesTheKillAndRetries(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	action := owners.action(model.AbuseActionKindHostKill, testAbuseHostName(owners, "shop"))
	dnsErr := errors.New("the zone's write failed")
	owners.failures["dns.host"] = dnsErr

	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); !errors.Is(err, dnsErr) {
		t.Fatalf("the failed effect answered %v", err)
	}
	if _, killed := owners.tlsKills["shop"]; !killed || owners.dnsHosts["shop"] || len(owners.hints) != 0 {
		t.Fatalf("after the failure: %s, %d hints", owners.held(), len(owners.hints))
	}
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
		t.Fatal(err)
	}
	if _, killed := owners.tlsKills["shop"]; !killed || !owners.dnsHosts["shop"] || len(owners.hints) != 1 {
		t.Fatalf("after the retry: %s, %d hints", owners.held(), len(owners.hints))
	}
}

// Every kind applied a second time leaves what the owners hold as it was.
func TestAbuseEffectSecondApplyChangesNothing(t *testing.T) {
	ctx := context.Background()
	for _, kind := range []model.AbuseActionKind{
		model.AbuseActionKindHostKill,
		model.AbuseActionKindHostSuspend,
		model.AbuseActionKindLabelKill,
		model.AbuseActionKindZoneSuspend,
		model.AbuseActionKindNetworkSuspend,
	} {
		owners := newTestAbuseOwners()
		owners.subjectIds = testAbuseSubjectIds(3)
		effects := NewAbuseEffects(owners.deps(), nil)
		action := owners.action(kind, testAbuseHostName(owners, "shop"))
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		held := owners.held()
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatalf("%s again: %v", kind, err)
		}
		if again := owners.held(); again != held {
			t.Errorf("%s: a second apply changed %s to %s", kind, held, again)
		}
	}
}

// A label kill takes every host at the edge first, then the zone; a zone or a
// host suspension is DNS's alone.
func TestAbuseEffectLabelAndDnsKinds(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		kind   model.AbuseActionKind
		target string
		ops    []string
	}{
		{kind: model.AbuseActionKindLabelKill, target: "abcdefghij.root.example", ops: []string{"tls.set *", "dns.zone", "hint"}},
		{kind: model.AbuseActionKindZoneSuspend, target: "abcdefghij.root.example", ops: []string{"dns.zone", "hint"}},
		{kind: model.AbuseActionKindHostSuspend, target: "shop.abcdefghij.root.example", ops: []string{"dns.host shop", "hint"}},
		{kind: model.AbuseActionKindHostSuspend, target: "shop", ops: []string{"dns.host shop", "hint"}},
		// an alias names the same network, and a host below a host keeps its labels
		{kind: model.AbuseActionKindHostKill, target: "a.shop.shopfront.root.example", ops: []string{"tls.set a.shop", "dns.host a.shop", "hint"}},
		// DNS withholds no apex alone, so an apex's kill is the edge's
		{kind: model.AbuseActionKindHostKill, target: "abcdefghij.root.example", ops: []string{"tls.set @", "hint"}},
		{kind: model.AbuseActionKindNoticeForward, target: "shop.abcdefghij.root.example", ops: nil},
	} {
		owners := newTestAbuseOwners()
		effects := NewAbuseEffects(owners.deps(), nil)
		action := owners.action(c.kind, c.target)
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatalf("%s %s: %v", c.kind, c.target, err)
		}
		if ops := owners.takeOps(); !slices.Equal(ops, c.ops) {
			t.Errorf("%s %s: the calls were %v", c.kind, c.target, ops)
		}
		if owners.zoneSuspended && owners.zoneReason != "abuse:"+action.CaseId.String() {
			t.Errorf("%s: the zone's reason reads %q", c.kind, owners.zoneReason)
		}
	}
}

// A revocation asks for every issued certificate of the network, with
// cessationOfOperation and the case's reason. Its lift returns nothing: a
// revoked certificate stays revoked.
func TestAbuseEffectCertRevokeAndItsLift(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	action := owners.action(model.AbuseActionKindCertRevoke, "abcdefghij.root.example")
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"cert.revoke", "hint"}) {
		t.Fatalf("the calls were %v", ops)
	}
	request := owners.revocations[0]
	if request.NetworkId != owners.networkId || request.CertId != (server.Id{}) || request.KeyCompromise ||
		request.Reason != "abuse:"+action.CaseId.String() {
		t.Fatalf("the request reads %+v", request)
	}
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(action, model.AbuseReviewStateLiftedWrong)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"hint"}) {
		t.Fatalf("a revocation's lift called %v", ops)
	}
}

// A lift returns exactly what its action withheld: the host's kill and its DNS
// suspension, and nothing a label kill of the same network still holds.
func TestAbuseEffectLiftRestoresExactlyWhatWasSuspended(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	hostKill := owners.action(model.AbuseActionKindHostKill, testAbuseHostName(owners, "shop"))
	otherKill := owners.action(model.AbuseActionKindHostKill, testAbuseHostName(owners, "blog"))
	labelKill := owners.action(model.AbuseActionKindLabelKill, "abcdefghij.root.example")
	for _, action := range []*model.AbuseAction{hostKill, otherKill, labelKill} {
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	owners.standing = []*model.AbuseAction{otherKill, labelKill}
	owners.takeOps()

	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(hostKill, model.AbuseReviewStateLifted)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"tls.lift shop", "dns.restore shop", "hint"}) {
		t.Fatalf("the lift called %v", ops)
	}
	reason := func(action *model.AbuseAction) string { return "abuse:" + action.CaseId.String() }
	want := fmt.Sprintf("state= kills=[*=%s blog=%s] zone=true hosts=[blog] revocations=0 closed=0", reason(labelKill), reason(otherKill))
	if held := owners.held(); held != want {
		t.Fatalf("after the lift the owners hold %s", held)
	}

	// the label kill's lift returns the zone and the * kill, and leaves the other host's
	owners.standing = []*model.AbuseAction{otherKill}
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(labelKill, model.AbuseReviewStateLifted)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"tls.lift *", "dns.restore ", "hint"}) {
		t.Fatalf("the label kill's lift called %v", ops)
	}
	want = fmt.Sprintf("state= kills=[blog=%s] zone=false hosts=[blog] revocations=0 closed=0", reason(otherKill))
	if held := owners.held(); held != want {
		t.Fatalf("after both lifts the owners hold %s", held)
	}
}

// Two cases' kills of one host: lifting one frees nothing while the other
// stands, and the last lift frees the host.
func TestAbuseEffectLiftKeepsWhatAnotherActionHolds(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	first := owners.action(model.AbuseActionKindHostKill, testAbuseHostName(owners, "shop"))
	second := owners.action(model.AbuseActionKindHostKill, "shop")
	for _, action := range []*model.AbuseAction{first, second} {
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	owners.standing = []*model.AbuseAction{second}
	owners.takeOps()
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(first, model.AbuseReviewStateLiftedWrong)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"hint"}) {
		t.Fatalf("a lift under another standing kill called %v", ops)
	}
	owners.standing = nil
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(second, model.AbuseReviewStateLiftedWrong)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"tls.lift shop", "dns.restore shop", "hint"}) {
		t.Fatalf("the last lift called %v", ops)
	}
}

// n subject ids in id order.
func testAbuseSubjectIds(n int) []server.Id {
	subjectIds := []server.Id{}
	for range n {
		subjectIds = append(subjectIds, server.NewId())
	}
	slices.SortFunc(subjectIds, func(a server.Id, b server.Id) int { return a.Cmp(b) })
	return subjectIds
}

// A suspension sets the network's state, kills every host, suspends the zone
// and closes every subject's contracts, in that order, a page of subjects at a
// time. At each step the steps before it already hold. The calls it makes are
// all it makes: none appends to the authorization log or writes rev/, and no
// certificate is revoked.
func TestAbuseEffectNetworkSuspendFansOut(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	owners.subjectIds = testAbuseSubjectIds(5)
	settings := DefaultAbuseEffectsSettings()
	settings.SubjectPageSize = 2
	steps := []string{}
	owners.step = func(step string, detail string) {
		steps = append(steps, step)
		owners.stateLock.Lock()
		defer owners.stateLock.Unlock()
		_, killed := owners.tlsKills[sidecar.TlsKillAllHosts]
		switch step {
		case AbuseEffectStepTls:
			if owners.state != "suspended" {
				t.Errorf("the kill came before the network's state")
			}
		case AbuseEffectStepDns:
			if !killed {
				t.Errorf("the zone came before the kill")
			}
		case AbuseEffectStepClose:
			if owners.state != "suspended" || !killed || !owners.zoneSuspended {
				t.Errorf("a close came before the deny was whole")
			}
		case AbuseEffectStepHint:
			if len(owners.closedIds) != len(owners.subjectIds) {
				t.Errorf("the hint came with %d of %d subjects closed", len(owners.closedIds), len(owners.subjectIds))
			}
		}
	}
	effects := NewAbuseEffects(owners.deps(), settings)
	action := owners.action(model.AbuseActionKindNetworkSuspend, owners.networkId.String())

	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
		t.Fatal(err)
	}
	wantOps := []string{"state.suspend", "tls.set *", "dns.zone"}
	for _, subjectId := range owners.subjectIds {
		wantOps = append(wantOps, "close "+subjectId.String())
	}
	wantOps = append(wantOps, "hint")
	if ops := owners.takeOps(); !slices.Equal(ops, wantOps) {
		t.Fatalf("the calls were %v", ops)
	}
	wantSteps := []string{AbuseEffectStepState, AbuseEffectStepTls, AbuseEffectStepDns}
	for range owners.subjectIds {
		wantSteps = append(wantSteps, AbuseEffectStepClose)
	}
	wantSteps = append(wantSteps, AbuseEffectStepHint)
	if !slices.Equal(steps, wantSteps) {
		t.Fatalf("the steps were %v", steps)
	}
	if owners.stateCaseId != action.CaseId || len(owners.revocations) != 0 {
		t.Fatalf("the suspension holds %s under %s", owners.held(), owners.stateCaseId)
	}
	if !slices.Equal(owners.closedIds, owners.subjectIds) {
		t.Fatalf("closed %v of %v", owners.closedIds, owners.subjectIds)
	}
}

// A close that fails part way leaves the deny standing and the effect failed,
// and the retry closes every subject.
func TestAbuseEffectNetworkSuspendRetriesAFailedClose(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	owners.subjectIds = testAbuseSubjectIds(4)
	effects := NewAbuseEffects(owners.deps(), nil)
	action := owners.action(model.AbuseActionKindNetworkSuspend, owners.networkId.String())
	closeErr := errors.New("the close requests were not written")
	owners.failures["close "+owners.subjectIds[2].String()] = closeErr

	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); !errors.Is(err, closeErr) {
		t.Fatalf("the failed effect answered %v", err)
	}
	if owners.state != "suspended" || len(owners.closedIds) != 2 || len(owners.hints) != 0 {
		t.Fatalf("after the failure: %s", owners.held())
	}
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(owners.closedIds, owners.subjectIds) || len(owners.hints) != 1 {
		t.Fatalf("after the retry: %s", owners.held())
	}
}

// A suspension's lift ends the state, the * kill and the zone's suspension. A
// lift that is not "wrong" leaves the network restricted for the rest of
// §21.7's window from the suspension's start; a lift as wrong leaves no row.
// While another suspension stands, the lift frees nothing and the row names
// the one that stands.
func TestAbuseEffectNetworkSuspendLift(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		reviewState model.AbuseReviewState
		state       string
	}{
		{reviewState: model.AbuseReviewStateLifted, state: "restricted"},
		{reviewState: model.AbuseReviewStateLiftedWrong, state: ""},
	} {
		owners := newTestAbuseOwners()
		effects := NewAbuseEffects(owners.deps(), nil)
		action := owners.action(model.AbuseActionKindNetworkSuspend, owners.networkId.String())
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatal(err)
		}
		owners.takeOps()
		owners.now = owners.now.Add(24 * time.Hour)
		if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(action, c.reviewState)); err != nil {
			t.Fatal(err)
		}
		if ops := owners.takeOps(); !slices.Equal(ops, []string{"state.end", "tls.lift *", "dns.restore ", "hint"}) {
			t.Fatalf("%s: the lift called %v", c.reviewState, ops)
		}
		if owners.state != c.state || len(owners.tlsKills) != 0 || owners.zoneSuspended {
			t.Fatalf("%s: after the lift the owners hold %s", c.reviewState, owners.held())
		}
		if c.state == "restricted" {
			want := action.CreateTime.Add(DefaultAbuseEffectsSettings().RestrictedWindow)
			if owners.restrictedUntil == nil || !owners.restrictedUntil.Equal(want) {
				t.Fatalf("restricted until %v, want %s", owners.restrictedUntil, want)
			}
		}
	}

	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	first := owners.action(model.AbuseActionKindNetworkSuspend, owners.networkId.String())
	second := owners.action(model.AbuseActionKindNetworkSuspend, owners.networkId.String())
	for _, action := range []*model.AbuseAction{first, second} {
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatal(err)
		}
	}
	owners.standing = []*model.AbuseAction{first}
	owners.takeOps()
	if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(second, model.AbuseReviewStateLiftedWrong)); err != nil {
		t.Fatal(err)
	}
	if ops := owners.takeOps(); !slices.Equal(ops, []string{"state.suspend", "hint"}) {
		t.Fatalf("a lift under another suspension called %v", ops)
	}
	if owners.state != "suspended" || owners.stateCaseId != first.CaseId || !owners.zoneSuspended || len(owners.tlsKills) != 1 {
		t.Fatalf("under the standing suspension the owners hold %s under %s", owners.held(), owners.stateCaseId)
	}
}

// One hint for each standing change, an action's or a lift's, none for a
// forwarded notice and none for a barred case. A hint that fails or raises
// fails no effect.
func TestAbuseEffectHintsEachStandingChange(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	hintCount := 0
	for _, kind := range []model.AbuseActionKind{
		model.AbuseActionKindHostKill,
		model.AbuseActionKindLabelKill,
		model.AbuseActionKindCertRevoke,
		model.AbuseActionKindZoneSuspend,
		model.AbuseActionKindHostSuspend,
		model.AbuseActionKindNetworkSuspend,
	} {
		action := owners.action(kind, "shop")
		if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: action}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		hintCount += 1
		if len(owners.hints) != hintCount {
			t.Fatalf("%s: %d hints, want %d", kind, len(owners.hints), hintCount)
		}
		if err := effects.ApplyAbuseEffect(ctx, testAbuseLiftEffect(action, model.AbuseReviewStateLifted)); err != nil {
			t.Fatalf("%s's lift: %v", kind, err)
		}
		hintCount += 1
		if len(owners.hints) != hintCount {
			t.Fatalf("%s's lift: %d hints, want %d", kind, len(owners.hints), hintCount)
		}
	}

	notice := owners.action(model.AbuseActionKindNoticeForward, "shop")
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: notice}); err != nil {
		t.Fatal(err)
	}
	barred := owners.action(model.AbuseActionKindHostKill, "shop")
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: barred, Barred: true}); err != nil {
		t.Fatal(err)
	}
	barredLift := testAbuseLiftEffect(barred, model.AbuseReviewStateLifted)
	barredLift.Barred = true
	if err := effects.ApplyAbuseEffect(ctx, barredLift); err != nil {
		t.Fatal(err)
	}
	if len(owners.hints) != hintCount {
		t.Fatalf("a notice or a barred case hinted: %d hints, want %d", len(owners.hints), hintCount)
	}

	owners.failures["hint"] = errors.New("the hint's script failed")
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "blog")}); err != nil {
		t.Fatalf("a failed hint failed the effect: %v", err)
	}
	owners.raise = "hint"
	if err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "wiki")}); err != nil {
		t.Fatalf("a hint that raised failed the effect: %v", err)
	}
	if _, killed := owners.tlsKills["wiki"]; !killed {
		t.Fatal("the kill behind a failed hint is missing")
	}
}

// What the effects refuse, each leaving the owners untouched: a kind another
// task builds, an action with no network, a host of another network or of
// none, a custom host, a wildcard, an apex's host suspension, a lift with no
// lifted action, and hooks that are not all there. An owner that raises is an
// error, never a panic.
func TestAbuseEffectRefusals(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	for _, c := range []struct {
		name   string
		effect func() *AbuseEffect
		want   error
	}{
		{name: "a mail halt", want: ErrAbuseEffectKindUnbuilt, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindMailHalt, "mail.abcdefghij.root.example")}
		}},
		{name: "a label's mail halt", want: ErrAbuseEffectKindUnbuilt, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindMailLabelHalt, "blue-42.abcdefghij.root.example")}
		}},
		{name: "a mail halt's lift", want: ErrAbuseEffectKindUnbuilt, effect: func() *AbuseEffect {
			return testAbuseLiftEffect(owners.action(model.AbuseActionKindMailHalt, "mail.abcdefghij.root.example"), model.AbuseReviewStateLifted)
		}},
		{name: "an unknown kind", want: ErrAbuseEffectKindUnbuilt, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKind("screen_allow"), "shop")}
		}},
		{name: "no network", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			action := owners.action(model.AbuseActionKindLabelKill, "abcdefghij.root.example")
			action.NetworkId = nil
			return &AbuseEffect{Action: action}
		}},
		{name: "another network's host", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "shop.zyxwvutsrq.root.example")}
		}},
		{name: "a host under no label", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "shop.nobodyhere.root.example")}
		}},
		{name: "a custom host", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "shop.customer.example")}
		}},
		{name: "a wildcard", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "*.abcdefghij.root.example")}
		}},
		{name: "an apex's host suspension", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{Action: owners.action(model.AbuseActionKindHostSuspend, "abcdefghij.root.example")}
		}},
		{name: "a lift of nothing", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			effect := testAbuseLiftEffect(owners.action(model.AbuseActionKindHostKill, "shop"), model.AbuseReviewStateLifted)
			effect.Lifted = nil
			return effect
		}},
		{name: "no action", want: ErrAbuseEffectTarget, effect: func() *AbuseEffect {
			return &AbuseEffect{}
		}},
	} {
		if err := effects.ApplyAbuseEffect(ctx, c.effect()); !errors.Is(err, c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if ops := owners.takeOps(); len(ops) != 0 {
		t.Fatalf("a refused effect called %v", ops)
	}

	deps := owners.deps()
	deps.RestoreDns = nil
	if err := NewAbuseEffects(deps, nil).ApplyAbuseEffect(ctx, &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "shop")}); !errors.Is(err, ErrAbuseEffectsUnavailable) {
		t.Fatalf("effects missing a hook answered %v", err)
	}
	if ops := owners.takeOps(); len(ops) != 0 {
		t.Fatalf("effects missing a hook called %v", ops)
	}

	owners.raise = "dns.host"
	err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, "shop")})
	if err == nil || errors.Is(err, ErrAbuseEffectsUnavailable) {
		t.Fatalf("an owner that raised answered %v", err)
	}
	if _, killed := owners.tlsKills["shop"]; !killed || owners.dnsHosts["shop"] {
		t.Fatalf("after the raise the owners hold %s", owners.held())
	}
}

// A refusal's message names no target, since it reaches a log (I-13).
func TestAbuseEffectRefusalNamesNoTarget(t *testing.T) {
	ctx := context.Background()
	owners := newTestAbuseOwners()
	effects := NewAbuseEffects(owners.deps(), nil)
	for _, target := range []string{"canary-host.zyxwvutsrq.root.example", "canary-host.customer.example", "canary-host.nobodyhere.root.example"} {
		err := effects.ApplyAbuseEffect(ctx, &AbuseEffect{Action: owners.action(model.AbuseActionKindHostKill, target)})
		if err == nil || strings.Contains(err.Error(), "canary") || strings.Contains(err.Error(), owners.networkId.String()) {
			t.Errorf("the refusal reads %v", err)
		}
	}
}

// Every row a snapshot of root serves, by owner and type.
func testAbuseDnsRows(t testing.TB, ctx context.Context, root string) map[dnsservice.DnsRecordKey]dnsservice.DnsRecordKind {
	t.Helper()
	snapshot, err := model.OpenDnsSnapshot(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	rows := map[dnsservice.DnsRecordKey]dnsservice.DnsRecordKind{}
	for {
		set, err := snapshot.Next()
		if err != nil {
			return rows
		}
		rows[set.Key()] = set.Kind
	}
}

// The network's TLS kills as network_tls_suspension holds them, host to reason.
func testAbuseTlsKills(t testing.TB, ctx context.Context, networkId server.Id) map[string]string {
	t.Helper()
	suspensions, err := model.GetTlsSuspensions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kills := map[string]string{}
	for _, suspension := range suspensions {
		if server.Id(suspension.NetworkId) == networkId {
			kills[suspension.Host] = suspension.Reason
		}
	}
	return kills
}

// The effects on TLS's, DNS's and the contract layer's real models, through
// ApplyAbuseAction and LiftAbuseAction: a host kill writes the kill's row and
// withholds the host; a suspension sets the network's state, the * kill and the
// zone's suspension and stops the network's open contract, and appends nothing
// to the authorization log's head; each lift returns exactly its own action's
// part, and the last leaves the zone as it was served before any of it.
func TestAbuseEffectsOnTheRealModels(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)

		id := server.NewId().String()
		rootName := fmt.Sprintf("t%s.example", strings.ReplaceAll(id[len(id)-12:], "-", ""))
		root := rootName + "."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "abuse-"+networkId.String()[:8], server.NewId())
		label, err := model.EnsureNetworkLabel(ctx, networkId, root)
		if err != nil {
			t.Fatal(err)
		}
		apex := label + "." + root
		if _, err := model.DnsWrite(ctx, &model.DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: []dnsservice.DnsRecordSet{
			{Owner: "*." + apex, Type: dns.TypeA, Ttl: 60, Kind: dnsservice.DnsRecordKindEdge},
			{Owner: "shop." + apex, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"shop"`}},
			{Owner: "blog." + apex, Type: dns.TypeTXT, Ttl: 300, Kind: dnsservice.DnsRecordKindUser, Rdata: []string{`"blog"`}},
		}}); err != nil {
			t.Fatal(err)
		}
		before := testAbuseDnsRows(t, ctx, root)

		// an open contract of the network's device with a peer outside it
		clientId, counterpartyId := server.NewId(), server.NewId()
		otherNetworkId := server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		model.Testing_CreateDevice(ctx, otherNetworkId, server.NewId(), counterpartyId, "", "")
		if _, err := model.CreateContractNoEscrow(ctx, networkId, clientId, otherNetworkId, counterpartyId, 1024); err != nil {
			t.Fatal(err)
		}

		deps := DefaultAbuseEffectsDeps()
		deps.RootNames = func() []string { return []string{rootName} }
		defer SetAbuseEffects(NewAbuseEffects(deps, nil))()

		host := "shop." + label + "." + rootName
		var caseId server.Id
		server.Tx(ctx, func(tx server.PgTx) {
			caseId, _, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
				NetworkId:     &networkId,
				Category:      model.AbuseCategoryPhishing,
				Target:        model.AbuseTarget{Kind: model.AbuseTargetKindHost, Name: host},
				ReporterClass: model.AbuseReporterClassWebRisk,
				JoinKind:      model.AbuseJoinKindHost,
				JoinKey:       host,
			}, server.NowUtc(), model.DefaultAbuseSettings())
		})
		if err != nil {
			t.Fatal(err)
		}
		reason := "abuse:" + caseId.String()
		withheld := func(owner string) bool {
			return testAbuseDnsRows(t, ctx, root)[dnsservice.DnsRecordKey{Owner: owner, Type: dnsservice.DnsWithheldType}] == dnsservice.DnsRecordKindWithheld
		}

		hostKill := &model.AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: model.AbuseActionKindHostKill, Target: host}
		result, err := ApplyAbuseAction(ctx, hostKill)
		if err != nil || !result.Applied {
			t.Fatalf("the host kill: %+v, %v", result, err)
		}
		if kills := testAbuseTlsKills(t, ctx, networkId); len(kills) != 1 || kills["shop"] != reason {
			t.Fatalf("after the host kill the kills are %v", kills)
		}
		if !withheld("shop."+apex) || withheld(apex) {
			t.Fatal("the host kill did not withhold the host alone")
		}
		if !model.HasOpenContractHole(ctx, clientId, counterpartyId) {
			t.Fatal("a host kill stopped a contract")
		}

		suspend := &model.AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: model.AbuseActionKindNetworkSuspend, Target: networkId.String()}
		result, err = ApplyAbuseAction(ctx, suspend)
		if err != nil || !result.Applied {
			t.Fatalf("the suspension: %+v, %v", result, err)
		}
		if stateCaseId, suspended := model.GetNetworkAbuseSuspended(ctx, networkId); !suspended || stateCaseId != caseId {
			t.Fatalf("the network's state reads %s, %t", stateCaseId, suspended)
		}
		if kills := testAbuseTlsKills(t, ctx, networkId); len(kills) != 2 || kills[sidecar.TlsKillAllHosts] != reason {
			t.Fatalf("after the suspension the kills are %v", kills)
		}
		if !withheld(apex) {
			t.Fatal("the suspension did not withhold the zone")
		}
		if model.HasOpenContractHole(ctx, clientId, counterpartyId) {
			t.Fatal("a suspended network's contract still forwards")
		}
		// a suspension is server state: the authorization log's head has no row for it (I-4)
		headCount := 0
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_acl_head WHERE network_id = $1`, networkId).Scan(&headCount))
		})
		if headCount != 0 {
			t.Fatalf("the suspension left %d authorization log heads", headCount)
		}

		// the suspension's lift returns the zone and the * kill, and the host's kill stands
		lift, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId: server.NewId(), CaseId: caseId, LiftedActionId: suspend.ActionId, ReviewState: model.AbuseReviewStateLiftedWrong,
		})
		if err != nil || !lift.Applied || lift.RevocationStands {
			t.Fatalf("the suspension's lift: %+v, %v", lift, err)
		}
		if _, suspended := model.GetNetworkAbuseSuspended(ctx, networkId); suspended {
			t.Fatal("the lifted suspension still stands")
		}
		if kills := testAbuseTlsKills(t, ctx, networkId); len(kills) != 1 || kills["shop"] != reason {
			t.Fatalf("after the suspension's lift the kills are %v", kills)
		}
		if withheld(apex) || !withheld("shop."+apex) {
			t.Fatal("the suspension's lift did not return the zone alone")
		}

		lift, err = LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId: server.NewId(), CaseId: caseId, LiftedActionId: hostKill.ActionId, ReviewState: model.AbuseReviewStateLifted,
		})
		if err != nil || !lift.Applied {
			t.Fatalf("the host kill's lift: %+v, %v", lift, err)
		}
		if kills := testAbuseTlsKills(t, ctx, networkId); len(kills) != 0 {
			t.Fatalf("after every lift the kills are %v", kills)
		}
		after := testAbuseDnsRows(t, ctx, root)
		if len(after) != len(before) {
			t.Fatalf("the zone serves %d rows, and served %d before", len(after), len(before))
		}
		for key, kind := range before {
			if after[key] != kind {
				t.Errorf("%+v came back as %q", key, after[key])
			}
		}
	})
}

// A lift of a revocation tells the reviewer that the revocation stands.
func TestLiftAbuseActionSaysARevocationStands(t *testing.T) {
	requireAbuseTables(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		newTestAbuseDeps(t)
		_, _, caseId := newTestAbuseCase(t, ctx, false)
		revoke := &model.AbuseActionArgs{ActionId: server.NewId(), CaseId: caseId, Kind: model.AbuseActionKindCertRevoke, Target: "shop.root.example"}
		if _, err := ApplyAbuseAction(ctx, revoke); err != nil {
			t.Fatal(err)
		}
		lift, err := LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId: server.NewId(), CaseId: caseId, LiftedActionId: revoke.ActionId, ReviewState: model.AbuseReviewStateLifted,
		})
		if err != nil || !lift.RevocationStands {
			t.Fatalf("the revocation's lift: %+v, %v", lift, err)
		}
		kill := testAbuseKillArgs(caseId)
		if _, err := ApplyAbuseAction(ctx, kill); err != nil {
			t.Fatal(err)
		}
		lift, err = LiftAbuseAction(ctx, &model.AbuseLiftArgs{
			ActionId: server.NewId(), CaseId: caseId, LiftedActionId: kill.ActionId, ReviewState: model.AbuseReviewStateLifted,
		})
		if err != nil || lift.RevocationStands {
			t.Fatalf("a kill's lift: %+v, %v", lift, err)
		}
	})
}

// A binary that does not link MG5 installs no effects, so every action stays
// owed; one that does installs the real ones.
func TestInstallAbuseEffects(t *testing.T) {
	restore := InstallAbuseEffects()
	defer restore()
	effects, _, _ := currentAbuseActionDeps()
	_, real := effects.(*abuseEffects)
	if real != server.SidecarMg5Linked() {
		t.Fatalf("installed %T with MG5 linked %t", effects, server.SidecarMg5Linked())
	}
}

//go:build sidecar_pending

package model

// §12.8's record API (dns_records_model.go; DNS-M5-12, §22.5's §12 row): a write and its admin event, the
// compare-and-swap losing, each refusal in its order with no journal row, entries at the limit under two writers on a
// barrier, and under a decision the gate's legs through the real write: a retry past not_after with a stale version
// answers the stored bytes with no write, two racers for one effect key write once, an action mismatch is refused
// and a decision is spent once. The abuse facts and the limits are fixtures (SetDnsRecordsAbuseGate,
// Testing_SetDnsLimits); the decisions are real, signed by approval_use_model_test.go's fixture devices.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// Limits by key, -1 for a key it does not hold; only dns_entries is lifted by a higher tier.
type testDnsLimits struct {
	stateLock sync.Mutex
	limits    map[string]int64
}

func (self *testDnsLimits) LimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string) (int64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if limit, ok := self.limits[limitKey]; ok {
		return limit, true
	}
	return -1, true
}

func (self *testDnsLimits) UpgradeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string, usedCount int64) bool {
	return limitKey == DnsEntriesLimitKey
}

func (self *testDnsLimits) set(limitKey string, limit int64) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.limits[limitKey] = limit
}

// One root, one network with its label, its abuse facts and the limits.
type testDnsRecordsWorld struct {
	root      string
	networkId server.Id
	apex      string
	abuse     *sidecartest.FakeAbuseGate
	limits    *testDnsLimits
}

// A world whose network stands at standing, verified or not, with no limit set.
func newTestDnsRecordsWorld(t testing.TB, ctx context.Context, standing sidecar.AbuseStanding, verified bool) *testDnsRecordsWorld {
	t.Helper()
	root := testDnsRoot(t, ctx)
	networkId, apex := testDnsNetwork(t, ctx, root)
	world := &testDnsRecordsWorld{
		root:      root,
		networkId: networkId,
		apex:      apex,
		abuse:     sidecartest.NewFakeAbuseGate(),
		limits:    &testDnsLimits{limits: map[string]int64{}},
	}
	world.abuse.Set(connect.Id(networkId), standing, verified)
	t.Cleanup(SetDnsRecordsAbuseGate(world.abuse))
	t.Cleanup(Testing_SetDnsLimits(world.limits))
	return world
}

// A write as the network's credential would send it.
func (self *testDnsRecordsWorld) write(ctx context.Context, request *sidecar.DnsRecordsRequest) ([]byte, error) {
	return WriteDnsRecords(ctx, &DnsRecordsWrite{
		Root:    self.root,
		Caller:  &DnsRecordsCaller{NetworkId: self.networkId, ActorKind: AdminEventActorSystem},
		Request: request,
	})
}

// The root's serial now.
func (self *testDnsRecordsWorld) serial(t testing.TB, ctx context.Context) uint64 {
	t.Helper()
	state, err := GetDnsRootState(ctx, self.root)
	if err != nil {
		t.Fatal(err)
	}
	return state.Serial
}

// One upsert of type with values at owner, and the TTL left to the default.
func testDnsUpsert(owner string, rrType string, values ...string) *sidecar.DnsRecordsRequest {
	return &sidecar.DnsRecordsRequest{Upserts: []sidecar.DnsRecordUpsert{{Name: owner, Type: rrType, Values: values}}}
}

// The version an answer names.
func testDnsAnswerVersion(t testing.TB, body []byte) uint64 {
	t.Helper()
	answer := &sidecar.DnsRecordsAnswer{}
	if err := json.Unmarshal(body, answer); err != nil {
		t.Fatalf("answer %q: %v", body, err)
	}
	return answer.Version
}

// err as the refusal it should be.
func testDnsApiError(t testing.TB, err error) *sidecar.DnsApiError {
	t.Helper()
	apiError := &sidecar.DnsApiError{}
	if !errors.As(err, &apiError) {
		t.Fatalf("not a refusal: %v", err)
	}
	return apiError
}

// A write lands its RRsets in one serial with one admin event, the zone's set made on first need, the answer naming
// the version; GET /network/dns shows them with dns_entries' count; a delete removes one and a write of what the zone
// holds takes no serial.
func TestDnsRecordsWriteLandsAndShows(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		world.limits.set(DnsEntriesLimitKey, 10)
		request := &sidecar.DnsRecordsRequest{
			Upserts: []sidecar.DnsRecordUpsert{
				{Name: "shop." + world.apex, Type: "A", Values: []string{"11.22.33.10", "11.22.33.11"}},
				{Name: "_note.shop." + world.apex, Type: "TXT", Ttl: 600, Values: []string{`"synthetic note"`}},
			},
		}
		body, err := world.write(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		version := testDnsAnswerVersion(t, body)
		if version != world.serial(t, ctx) {
			t.Fatalf("version %d, serial %d", version, world.serial(t, ctx))
		}
		view, err := GetNetworkDns(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		if view.Version != version || view.EntryCount != 2 || view.EntryLimit != 10 || view.Apex() != world.apex {
			t.Fatalf("view %+v", view)
		}
		kinds := map[string]string{}
		for _, recordSet := range view.RecordSets {
			kinds[recordSet.Name+" "+recordSet.Type] = recordSet.Kind
		}
		if kinds["shop."+world.apex+" A"] != "user" || kinds["_note.shop."+world.apex+" TXT"] != "user" || kinds[world.apex+" TXT"] != "system" {
			t.Fatalf("kinds %v", kinds)
		}
		events := GetNetworkAdminEvents(ctx, world.networkId, 0, 100)
		recordEvents := 0
		for _, event := range events {
			if event.Action == AdminEventActionDnsRecords {
				recordEvents += 1
			}
		}
		if recordEvents != 1 {
			t.Fatalf("%d record events", recordEvents)
		}

		// the same RRset again takes no serial
		again, err := world.write(ctx, testDnsUpsert("shop."+world.apex, "A", "11.22.33.11", "11.22.33.10"))
		if err != nil || testDnsAnswerVersion(t, again) != version || world.serial(t, ctx) != version {
			t.Fatalf("a write of what the zone holds: %s %v", again, err)
		}
		deleted, err := world.write(ctx, &sidecar.DnsRecordsRequest{
			ExpectVersion: &version,
			Deletes:       []sidecar.DnsRecordDelete{{Name: "shop." + world.apex, Type: "A"}},
		})
		if err != nil || testDnsAnswerVersion(t, deleted) <= version {
			t.Fatalf("a delete: %s %v", deleted, err)
		}
		if count := DnsEntryCount(ctx, world.networkId); count != 1 {
			t.Fatalf("%d entries after the delete", count)
		}
	})
}

// Two writers that read one version: the first wins, the second loses with 409 and writes nothing.
func TestDnsRecordsCompareAndSwapLoses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		view, err := GetNetworkDns(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		read := view.Version
		first := testDnsUpsert("alpha."+world.apex, "TXT", `"first"`)
		first.ExpectVersion = &read
		if _, err := world.write(ctx, first); err != nil {
			t.Fatal(err)
		}
		serial := world.serial(t, ctx)
		second := testDnsUpsert("bravo."+world.apex, "TXT", `"second"`)
		second.ExpectVersion = &read
		_, err = world.write(ctx, second)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusConflict || apiError.Code != sidecar.DnsApiCodeVersionConflict {
			t.Fatalf("the loser: %+v", apiError)
		}
		if world.serial(t, ctx) != serial {
			t.Fatalf("the loser wrote")
		}
	})
}

// Each refusal answers in §12.8's order, a public name's verification before probation's types and the limits, and
// none writes a journal row.
func TestDnsRecordsRefusesInOrder(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		cases := []struct {
			name     string
			standing sidecar.AbuseStanding
			verified bool
			limits   map[string]int64
			// written first, standing, verified and with no limit
			before  func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest
			request func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest
			status  int
			code    string
			limit   string
		}{
			{
				name:     "unverified public name",
				standing: sidecar.AbuseStandingProbation,
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("shop."+world.apex, "A", "11.22.33.10")
				},
				status: http.StatusPaymentRequired, code: sidecar.DnsApiCodeQuota, limit: DnsVerificationLimitKey,
			},
			{
				name:     "probation's types",
				standing: sidecar.AbuseStandingRestricted, verified: true,
				limits: map[string]int64{DnsEntriesLimitKey: 0},
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("shop."+world.apex, "CNAME", "target.example.")
				},
				status: http.StatusForbidden, code: sidecar.DnsApiCodeRefused,
			},
			{
				name:     "probation's count",
				standing: sidecar.AbuseStandingProbation, verified: true,
				limits: map[string]int64{DnsEntriesLimitKey: 10, DnsProbationEntriesLimitKey: 1},
				before: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a"`)
				},
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("bravo."+world.apex, "TXT", `"b"`)
				},
				status: http.StatusTooManyRequests, code: sidecar.DnsApiCodeQuota, limit: DnsProbationEntriesLimitKey,
			},
			{
				name:     "dns_entries",
				standing: sidecar.AbuseStandingStandard, verified: true,
				limits: map[string]int64{DnsEntriesLimitKey: 1, DnsChangesPerHourLimitKey: 0},
				before: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a"`)
				},
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("bravo."+world.apex, "TXT", `"b"`)
				},
				status: http.StatusPaymentRequired, code: sidecar.DnsApiCodeQuota, limit: DnsEntriesLimitKey,
			},
			{
				name:     "dns_changes_per_hour",
				standing: sidecar.AbuseStandingStandard, verified: true,
				limits: map[string]int64{DnsChangesPerHourLimitKey: 1},
				before: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a"`)
				},
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a again"`)
				},
				status: http.StatusTooManyRequests, code: sidecar.DnsApiCodeQuota, limit: DnsChangesPerHourLimitKey,
			},
			{
				name:     "§12.7's address rule",
				standing: sidecar.AbuseStandingStandard, verified: true,
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("shop."+world.apex, "A", "10.0.0.1")
				},
				status: http.StatusBadRequest, code: sidecar.DnsApiCodeInvalid,
			},
			{
				name:     "a system record",
				standing: sidecar.AbuseStandingStandard, verified: true,
				before: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a"`)
				},
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return &sidecar.DnsRecordsRequest{Deletes: []sidecar.DnsRecordDelete{{Name: world.apex, Type: "TXT"}}}
				},
				status: http.StatusBadRequest, code: sidecar.DnsApiCodeInvalid,
			},
			{
				name:     "the screen",
				standing: sidecar.AbuseStandingStandard, verified: true,
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("paypal-login."+world.apex, "TXT", `"x"`)
				},
				status: http.StatusForbidden, code: sidecar.DnsApiCodeRefused,
			},
			{
				name:     "a name outside the zone",
				standing: sidecar.AbuseStandingStandard, verified: true,
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("www.another0."+world.root, "TXT", `"x"`)
				},
				status: http.StatusBadRequest, code: sidecar.DnsApiCodeInvalid,
			},
			{
				name:     "suspended",
				standing: sidecar.AbuseStandingSuspended, verified: true,
				request: func(world *testDnsRecordsWorld) *sidecar.DnsRecordsRequest {
					return testDnsUpsert("alpha."+world.apex, "TXT", `"a"`)
				},
				status: http.StatusForbidden, code: sidecar.DnsApiCodeRefused,
			},
		}
		for _, c := range cases {
			world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
			if c.before != nil {
				if _, err := world.write(ctx, c.before(world)); err != nil {
					t.Fatalf("%s: the write before: %v", c.name, err)
				}
			}
			world.abuse.Set(connect.Id(world.networkId), c.standing, c.verified)
			for limitKey, limit := range c.limits {
				world.limits.set(limitKey, limit)
			}
			serial := world.serial(t, ctx)
			_, err := world.write(ctx, c.request(world))
			apiError := testDnsApiError(t, err)
			if apiError.Status != c.status || apiError.Code != c.code {
				t.Errorf("%s: %+v, want %d %s", c.name, apiError, c.status, c.code)
			}
			if c.limit != "" && (apiError.Refusal == nil || apiError.Refusal.LimitKey != c.limit) {
				t.Errorf("%s: refusal %+v, want %s", c.name, apiError.Refusal, c.limit)
			}
			if c.limit == DnsChangesPerHourLimitKey && (apiError.Refusal == nil || apiError.Refusal.RetryAfterMs == 0) {
				t.Errorf("%s: no window to wait", c.name)
			}
			if world.serial(t, ctx) != serial {
				t.Errorf("%s: a refusal wrote", c.name)
			}
		}
	})
}

// With no abuse facts installed every write answers unavailable, the closed direction.
func TestDnsRecordsUnavailableWithoutStanding(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		restore := SetDnsRecordsAbuseGate(nil)
		defer restore()
		_, err := world.write(ctx, testDnsUpsert("alpha."+world.apex, "TXT", `"a"`))
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusServiceUnavailable {
			t.Fatalf("%+v", apiError)
		}
	})
}

// Two writers each adding one RRset when the network holds its limit less one: the per-network lock lets one count
// at a time, so one lands and the other is refused, whichever runs first.
func TestDnsRecordsEntriesAtTheLimitUnderTwoWriters(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsRecordsWorld(t, ctx, sidecar.AbuseStandingStandard, true)
		world.limits.set(DnsEntriesLimitKey, 1)

		firstLocked := make(chan struct{})
		releaseFirst := make(chan struct{})
		secondBefore := make(chan struct{})
		var stateLock sync.Mutex
		lockedCount := 0
		beforeCount := 0
		Testing_DnsRecordsBarrier = func(name string, networkId server.Id) {
			if networkId != world.networkId {
				return
			}
			stateLock.Lock()
			switch name {
			case DnsRecordsBarrierLocked:
				lockedCount += 1
			case DnsRecordsBarrierBefore:
				beforeCount += 1
			}
			first := name == DnsRecordsBarrierLocked && lockedCount == 1
			second := name == DnsRecordsBarrierBefore && beforeCount == 2
			stateLock.Unlock()
			if first {
				close(firstLocked)
				<-releaseFirst
			}
			if second {
				close(secondBefore)
			}
		}
		defer func() { Testing_DnsRecordsBarrier = nil }()

		errs := make(chan error, 2)
		go func() {
			_, err := world.write(ctx, testDnsUpsert("alpha."+world.apex, "TXT", `"a"`))
			errs <- err
		}()
		<-firstLocked
		go func() {
			_, err := world.write(ctx, testDnsUpsert("bravo."+world.apex, "TXT", `"b"`))
			errs <- err
		}()
		<-secondBefore
		close(releaseFirst)
		landed := 0
		refused := 0
		for range 2 {
			err := <-errs
			if err == nil {
				landed += 1
				continue
			}
			if apiError := testDnsApiError(t, err); apiError.Refusal != nil && apiError.Refusal.LimitKey == DnsEntriesLimitKey {
				refused += 1
			}
		}
		if landed != 1 || refused != 1 || DnsEntryCount(ctx, world.networkId) != 1 {
			t.Fatalf("%d landed, %d refused, %d entries", landed, refused, DnsEntryCount(ctx, world.networkId))
		}
	})
}

// A world for writes under a decision: approval_use_model_test.go's devices and state, its network in the DB with a
// label, the spend state installed as this process's gate states.
type testDnsDecisionWorld struct {
	*testDnsRecordsWorld
	approval *testApprovalWorld
}

func newTestDnsDecisionWorld(t testing.TB, ctx context.Context) *testDnsDecisionWorld {
	t.Helper()
	approval := newTestApprovalWorld()
	root := testDnsRoot(t, ctx)
	Testing_CreateNetwork(ctx, approval.networkId, "dnsd-"+approval.networkId.String()[24:], server.NewId())
	label, err := EnsureNetworkLabel(ctx, approval.networkId, root)
	if err != nil {
		t.Fatal(err)
	}
	// the zone's system set first, so a test's write is its own one serial
	if _, err := EnsureNetworkZone(ctx, approval.networkId, root); err != nil {
		t.Fatal(err)
	}
	records := &testDnsRecordsWorld{
		root:      root,
		networkId: approval.networkId,
		apex:      label + "." + root,
		abuse:     sidecartest.NewFakeAbuseGate(),
		limits:    &testDnsLimits{limits: map[string]int64{}},
	}
	records.abuse.Set(connect.Id(approval.networkId), sidecar.AbuseStandingStandard, true)
	t.Cleanup(SetDnsRecordsAbuseGate(records.abuse))
	t.Cleanup(Testing_SetDnsLimits(records.limits))
	SetApprovalGateStates(func(ctx context.Context, networkId server.Id) (sidecar.SpendState, error) {
		return approval.state, nil
	})
	t.Cleanup(func() { SetApprovalGateStates(nil) })
	return &testDnsDecisionWorld{testDnsRecordsWorld: records, approval: approval}
}

// A dns_set of value at owner, A with ttl 300, or the template of owner and A with scope set.
func (self *testDnsDecisionWorld) action(t testing.TB, owner string, value string, template bool) *sidecar.StoredCanonicalAction {
	t.Helper()
	action := &protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionDnsSet), DnsName: owner, DnsType: "A", DnsValue: value, DnsTtl: 300}
	if template {
		action = &protocol.CanonicalAction{Kind: uint32(sidecar.SidecarActionDnsSet), Scope: true, DnsName: owner, DnsType: "A"}
	}
	stored, err := sidecar.NewCanonicalAction(action)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

// The write of use under decision, with use's effect key, as the laptop sends it: its RRset value, which a test may
// make differ from the action's.
func (self *testDnsDecisionWorld) decisionWrite(ctx context.Context, approvalId sidecar.Id, decision []byte, use *sidecar.StoredCanonicalAction, template *sidecar.StoredCanonicalAction, value string, expect *uint64, now time.Time) ([]byte, error) {
	effectKey := sidecar.ApprovalEffectKey(approvalId, use.ActionId(approvalId))
	laptop := server.Id(self.approval.laptop.sidecarId)
	request := &sidecar.DnsRecordsRequest{
		ExpectVersion: expect,
		Upserts:       []sidecar.DnsRecordUpsert{{Name: use.Action.GetDnsName(), Type: "A", Ttl: 300, Values: []string{value}}},
		Approval:      &sidecar.DnsRecordsApproval{Decisions: [][]byte{decision}, Action: use.Bytes},
	}
	if template != nil {
		request.Approval.Template = template.Bytes
	}
	return WriteDnsRecords(ctx, &DnsRecordsWrite{
		Root:      self.root,
		Caller:    &DnsRecordsCaller{NetworkId: self.networkId, SidecarId: &laptop, ActorKind: AdminEventActorSidecar, ActorId: &laptop},
		Request:   request,
		EffectKey: effectKey[:],
		Now:       now,
	})
}

// A decision's write commits and stores its answer; after the zone has moved on, a retry past not_after with the
// stale version it first named answers those same bytes and writes nothing.
func TestDnsRecordsDecisionRetryAnswersTheStoredBytes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsDecisionWorld(t, ctx)
		now := server.NowUtc()
		owner := "shop." + world.apex
		use := world.action(t, owner, "11.22.33.30", false)
		approvalId := sidecar.NewId()
		decision := world.approval.decision(t, use, approvalId, 1, now.Add(time.Hour), world.approval.phone)
		read, err := GetDnsZoneVersion(ctx, world.root, world.networkId)
		if err != nil {
			t.Fatal(err)
		}
		first, err := world.decisionWrite(ctx, approvalId, decision, use, nil, "11.22.33.30", &read, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := world.write(ctx, testDnsUpsert("other."+world.apex, "TXT", `"moves the zone"`)); err != nil {
			t.Fatal(err)
		}
		serial := world.serial(t, ctx)
		retry, err := world.decisionWrite(ctx, approvalId, decision, use, nil, "11.22.33.30", &read, now.Add(2*time.Hour))
		if err != nil || string(retry) != string(first) {
			t.Fatalf("the retry: %q %v, want %q", retry, err, first)
		}
		if world.serial(t, ctx) != serial {
			t.Fatalf("the retry wrote")
		}
	})
}

// Two requests for one effect key at once, held together between the spend check and the claim: one write, and both
// answer its bytes.
func TestDnsRecordsDecisionRacersWriteOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsDecisionWorld(t, ctx)
		now := server.NowUtc()
		use := world.action(t, "shop."+world.apex, "11.22.33.30", false)
		approvalId := sidecar.NewId()
		decision := world.approval.decision(t, use, approvalId, 1, now.Add(time.Hour), world.approval.phone)
		serial := world.serial(t, ctx)

		arrived := make(chan struct{}, 2)
		release := make(chan struct{})
		hookCtx := context.WithValue(ctx, approvalUseClaimHookKey{}, func() {
			arrived <- struct{}{}
			<-release
		})
		bodies := make(chan []byte, 2)
		errs := make(chan error, 2)
		for range 2 {
			go func() {
				body, err := world.decisionWrite(hookCtx, approvalId, decision, use, nil, "11.22.33.30", nil, now)
				bodies <- body
				errs <- err
			}()
		}
		<-arrived
		<-arrived
		close(release)
		answers := []string{}
		for range 2 {
			if err := <-errs; err != nil {
				t.Fatal(err)
			}
			answers = append(answers, string(<-bodies))
		}
		if answers[0] != answers[1] {
			t.Fatalf("answers %v", answers)
		}
		if world.serial(t, ctx) != serial+1 {
			t.Fatalf("serial %d, want %d", world.serial(t, ctx), serial+1)
		}
	})
}

// A write whose RRset is not the decision's action is refused at the spend check and writes nothing (AD1, AU1).
func TestDnsRecordsDecisionMismatchRefused(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsDecisionWorld(t, ctx)
		now := server.NowUtc()
		use := world.action(t, "shop."+world.apex, "11.22.33.30", false)
		approvalId := sidecar.NewId()
		decision := world.approval.decision(t, use, approvalId, 1, now.Add(time.Hour), world.approval.phone)
		serial := world.serial(t, ctx)
		_, err := world.decisionWrite(ctx, approvalId, decision, use, nil, "11.22.33.31", nil, now)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusForbidden || apiError.Code != sidecar.DnsApiCodeRefused {
			t.Fatalf("%+v", apiError)
		}
		if world.serial(t, ctx) != serial {
			t.Fatalf("a mismatch wrote")
		}
		if rows := testApprovalRows(t, ctx, approvalId); len(rows) != 0 {
			t.Fatalf("rows %v", rows)
		}
	})
}

// A decision of two uses under a template spends each once: a third action it covers is 409 spent and writes
// nothing.
func TestDnsRecordsDecisionSpentOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		world := newTestDnsDecisionWorld(t, ctx)
		now := server.NowUtc()
		owner := "shop." + world.apex
		template := world.action(t, owner, "", true)
		approvalId := sidecar.NewId()
		decision := world.approval.decision(t, template, approvalId, 2, now.Add(time.Hour), world.approval.phone)
		for _, value := range []string{"11.22.33.30", "11.22.33.31"} {
			use := world.action(t, owner, value, false)
			if _, err := world.decisionWrite(ctx, approvalId, decision, use, template, value, nil, now); err != nil {
				t.Fatal(err)
			}
		}
		serial := world.serial(t, ctx)
		thirdUse := world.action(t, owner, "11.22.33.32", false)
		_, err := world.decisionWrite(ctx, approvalId, decision, thirdUse, template, "11.22.33.32", nil, now)
		if apiError := testDnsApiError(t, err); apiError.Status != http.StatusConflict || apiError.Code != sidecar.DnsApiCodeSpent {
			t.Fatalf("%+v", apiError)
		}
		if world.serial(t, ctx) != serial {
			t.Fatalf("a spent decision wrote")
		}
	})
}

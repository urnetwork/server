//go:build sidecar_pending

package quotahost

// FS-47's §21 case on T3 (M5-out; §21.7, §20.7's allow row, D-QUOTA-7, QD-328; QUOTA-M5-01), QUOTA's half: what
// §21's closure-growth case reads, and what the reviewed tightening that follows it does, through this package's
// Quota, evs's gate, the {sq_} script, network_quota_state, network_sidecar_limit and the authorization log's
// append limits on the private stack's Postgres and Redis.
//
//	growth      erasers' closure uploads taken at the storage limit count in the network's day total
//	            (model.NetworkClosureBytesToday), and evs's usage batch carrying their bytes, then the nightly
//	            count, put the stored count past the limit: the three reads QD-328's test makes, the day total
//	            above zero, storage_over, and the count above model.NetworkStorageLimitByteCount. A network
//	            under its limit takes its closures with no count, one at its limit that took none reads zero,
//	            and the next UTC day's total starts at zero.
//	tightening  the rows a reviewed limit_tighten writes, citing the case: evs_closure_bytes_per_day at the 4 MiB
//	            floor and a lower config_statements_per_day. An eraser then takes three full re-keys, and its
//	            fourth, or a CLOSURE push's records past its allowance, gets 429 with retry_after_ms to the
//	            next 00:00 UTC, while another eraser's pass, each allowance its own, and a network of the same
//	            tier keeps the tier's; the log refuses the network's statement past the lowered day limit with
//	            429 while a MemberRevoke still appends (§7.9's exemption); a row below the floor or setting the
//	            floor or the pacing is refused, one written to the table directly reads the floor or is
//	            ignored, and lifting the rows gives the tier's limits back.
//
// ABUSE-M5-19 owns the rest of the case, and its run joins this one when it lands: the worker that opens one case,
// with no machine action, from these reads, and POST /abuse/action's limit_tighten, which writes these rows and
// lifts them with its case. Until then the case the rows cite is opened here as a staff filing
// (model.OpenAbuseCaseInTx), and the rows are written as that action writes them, through
// model.ValidateNetworkSidecarLimit and model.SetNetworkSidecarLimit.

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/sidecartest/quotasuite"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The storage limit of the tier the tests' networks are on.
const fs47AbuseStorageLimit = int64(8 * 1024 * 1024)

// The fixed floor no row or §21 action takes an eraser's allowance below (owner decision 146).
const fs47AbuseClosureFloor = int64(4 * 1024 * 1024)

// The tier's own allowance, which the tightening takes to the floor.
const fs47AbuseClosureAllowance = int64(64 * 1024 * 1024)

// The tier's statements a day, which the tightening lowers to fs47AbuseStatementsPerDay.
const fs47AbuseTierStatementsPerDay = int64(1000)

const fs47AbuseStatementsPerDay = int64(2)

// pro.yml's keys as the tests' tier reads them: the storage limit, the closure bounds, and §7.9's append limits
// with only the day's statements bounded.
func fs47AbuseLimits() map[string]int64 {
	return map[string]int64{
		"storage":                   fs47AbuseStorageLimit,
		"evs_closure_bytes_floor":   fs47AbuseClosureFloor,
		"evs_closure_bytes_per_day": fs47AbuseClosureAllowance,
		"evs_closure_uploads_per_s": 4,
		"config_statements_per_min": -1,
		"config_statements_burst":   -1,
		"config_statements_per_day": fs47AbuseTierStatementsPerDay,
		"config_bases_per_min":      -1,
		"config_bases_per_day":      -1,
	}
}

// Whether a closure refusal only says when the eraser's turn or minute comes, which waiting lifts.
func fs47AbusePacingRefusal(refusal *sidecar.QuotaRefusal) bool {
	switch refusal.LimitKey {
	case "evs_closure_uploads_per_s", "evs_closure_uploads_per_min":
		return true
	}
	return false
}

// The world of one test: this package's Quota on model's limits and stores, at a fake clock, and evs's gate on it.
type fs47AbuseWorld struct {
	ctx   context.Context
	clock *sidecartest.FakeClock
	quota *HostQuota
	gate  *evsservice.QuotaGate
}

func newFs47AbuseWorld(t *testing.T, ctx context.Context) *fs47AbuseWorld {
	t.Helper()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	quota, err := NewLocalQuota(ctx, nil, clock, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { quota.Drain(context.Background()) })
	return &fs47AbuseWorld{ctx: ctx, clock: clock, quota: quota}
}

// A network of the tests' tier with a balance, so it is never spent, its stored count moved to storedByteCount by
// evs's usage batch. It has a network row, which a network_sidecar_limit row needs.
func (self *fs47AbuseWorld) network(t *testing.T, storedByteCount int64) connect.Id {
	t.Helper()
	networkId := connect.NewId()
	model.Testing_CreateNetwork(self.ctx, server.Id(networkId), "fs47-abuse-"+networkId.String(), server.NewId())
	t3SetBalances(self.ctx, self.clock.Now(), server.Id(networkId), []*sidecartest.QuotaBalance{{ByteCount: 1024 * 1024 * 1024}})
	if 0 < storedByteCount {
		self.reportStored(t, networkId, storedByteCount)
	}
	return networkId
}

// evs's usage batch moving the network's stored count by storedByteDelta, as uploads land.
func (self *fs47AbuseWorld) reportStored(t *testing.T, networkId connect.Id, storedByteDelta int64) {
	t.Helper()
	if err := self.quota.ReportUsage(self.ctx, fs47T3StoredBatch(networkId, storedByteDelta)); err != nil {
		t.Fatal(err)
	}
}

// evs's gate on the Quota, once it has read overNetworkId as over its limit.
func (self *fs47AbuseWorld) openGate(t *testing.T, overNetworkId connect.Id) {
	t.Helper()
	self.gate = fs47T3Gate(t, self.ctx, &quotasuite.Target{Quota: self.quota, Clock: self.clock}, overNetworkId)
}

// One closure upload of byteCount by eraserId at the limit, through evs's gate, asked again at each turn or
// minute the gate answers, as an eraser obeying retry_after_ms does: nil once taken, or the first refusal that
// waiting does not lift.
func (self *fs47AbuseWorld) takeClosure(t *testing.T, networkId connect.Id, eraserId connect.Id, byteCount int64) *sidecar.QuotaRefusal {
	t.Helper()
	for range 1000 {
		refusal, err := self.gate.ChargeClosure(self.ctx, networkId, eraserId, byteCount, connect.NewId())
		if err != nil {
			t.Fatal(err)
		}
		if refusal == nil || !fs47AbusePacingRefusal(refusal) {
			return refusal
		}
		self.clock.Advance(time.Duration(refusal.RetryAfterMs) * time.Millisecond)
	}
	t.Fatal("an eraser's turn never came")
	return nil
}

// The limits the Quota serves the network, read afresh.
func (self *fs47AbuseWorld) limits(t *testing.T, networkId connect.Id) map[string]int64 {
	t.Helper()
	self.quota.forgetLimits(server.Id(networkId))
	limits, err := self.quota.Limits(self.ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	return limits.Limits
}

// The milliseconds from now to the next 00:00 UTC, which a day's refusal lifts at.
func fs47AbuseMsToNextDay(now time.Time) uint32 {
	day := now.UTC().Truncate(24 * time.Hour)
	return sidecar.QuotaRetryAfterMs(now, day.Add(24*time.Hour))
}

// What QD-328's closure-growth test reads of a network: its closure bytes for the UTC day of now, storage_over,
// its stored count, and its storage limit.
type fs47AbuseGrowthReads struct {
	closureByteCount int64
	storageOver      bool
	storedByteCount  int64
	limitByteCount   int64
}

func fs47AbuseReadGrowth(t *testing.T, ctx context.Context, networkId connect.Id, now time.Time) *fs47AbuseGrowthReads {
	t.Helper()
	closureByteCount, err := model.NetworkClosureBytesToday(ctx, server.Id(networkId), now)
	if err != nil {
		t.Fatal(err)
	}
	state, err := model.GetNetworkQuotaState(ctx, server.Id(networkId))
	if err != nil {
		t.Fatal(err)
	}
	reads := &fs47AbuseGrowthReads{
		closureByteCount: closureByteCount,
		limitByteCount:   model.NetworkStorageLimitByteCount(ctx, server.Id(networkId)),
	}
	if state != nil {
		reads.storageOver = state.StorageOver
		reads.storedByteCount = state.StoredByteCount
	}
	return reads
}

// The case §21's review acts on, opened as a staff filing that names the network, standing for the case
// ABUSE-M5-19's worker opens.
func fs47AbuseOpenCase(t *testing.T, ctx context.Context, networkId connect.Id, now time.Time) server.Id {
	t.Helper()
	caseNetworkId := server.Id(networkId)
	var caseId server.Id
	var opened bool
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		caseId, opened, err = model.OpenAbuseCaseInTx(ctx, tx, &model.AbuseCaseArgs{
			NetworkId:     &caseNetworkId,
			Category:      model.AbuseCategoryOther,
			Target:        model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: caseNetworkId.String()},
			ReporterClass: model.AbuseReporterClassStaff,
			JoinKind:      model.AbuseJoinKindNetwork,
			JoinKey:       caseNetworkId.String(),
		}, now, model.DefaultAbuseSettings())
	})
	if err != nil || !opened {
		t.Fatalf("the case: opened %t, %v", opened, err)
	}
	return caseId
}

// The member a signed MemberAdmit admits.
func fs47AbuseAdmittedMemberId(t *testing.T, signedBytes []byte) []byte {
	t.Helper()
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedBytes, signed); err != nil {
		t.Fatal(err)
	}
	admit := &protocol.MemberAdmit{}
	if err := proto.Unmarshal(signed.GetBody(), admit); err != nil {
		t.Fatal(err)
	}
	return admit.GetMemberId()
}

// The network's authorization log on a sequencer of its own, its append limits on the test's Redis under the
// network's limits (model.NetworkSidecarLimits) and on the log's clock: statementsPerDay statements pass, the next
// is refused with 429 to the next 00:00 UTC, and a MemberRevoke of an admitted member still appends.
func fs47AbuseCheckStatements(t *testing.T, ctx context.Context, networkId connect.Id, statementsPerDay int64) {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	log := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.NewId(), sidecar.NewId())
	settings := model.DefaultConfigLogSettings()
	settings.Clock = log.Clock()
	settings.Limiter = model.Testing_NewConfigLogRedisLimiterAt(nil, log.Clock().Now)
	sequencer := model.NewConfigLogSequencer(cluster, settings, nil)
	logNetworkId := server.Id(networkId)

	signedHeader, rows, genesis, configPushes := log.BaseOne()
	pin, err := sequencer.AppendBase(ctx, logNetworkId, &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	if err != nil {
		t.Fatal(err)
	}
	log.SetTip(pin)
	memberIds := [][]byte{}
	for range statementsPerDay {
		admit := log.MemberAdmit()
		memberIds = append(memberIds, fs47AbuseAdmittedMemberId(t, admit))
		pin, err := sequencer.AppendStatement(ctx, logNetworkId, admit)
		if err != nil {
			t.Fatalf("statement %d of %d a day: %v", len(memberIds), statementsPerDay, err)
		}
		log.SetTip(pin)
	}
	_, err = sequencer.AppendStatement(ctx, logNetworkId, log.MemberAdmit())
	limitErr := &model.ConfigLogLimitError{}
	if !errors.As(err, &limitErr) {
		t.Fatalf("a statement past %d a day: %v", statementsPerDay, err)
	}
	if want := fs47AbuseMsToNextDay(log.Clock().Now()); limitErr.RetryAfterMs != want {
		t.Fatalf("the day's refusal waits %d ms, want %d to 00:00 UTC", limitErr.RetryAfterMs, want)
	}
	// revocations are exempt, so an admin still removes what the day's statements admitted
	revoke := log.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_REVOKE, &bridge.MemberRevoke{Header: log.Header(), MemberId: memberIds[0]})
	pin, err = sequencer.AppendStatement(ctx, logNetworkId, revoke)
	if err != nil {
		t.Fatalf("a revocation past the day's limit: %v", err)
	}
	log.SetTip(pin)
}

// QD-328's three reads on T3: a network whose erasers took closure bytes at the storage limit, those bytes then
// stored past it, reads a day total above zero, storage_over and a count above its limit; a network under its limit
// takes its closures with no count, one at its limit that took none reads zero, and the next UTC day starts at zero.
func TestFs47T3ClosureGrowthReads(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, fs47AbuseLimits())()
		ctx := t.Context()
		world := newFs47AbuseWorld(t, ctx)
		growing := world.network(t, fs47AbuseStorageLimit)
		idle := world.network(t, fs47AbuseStorageLimit)
		under := world.network(t, fs47AbuseStorageLimit/2)
		world.openGate(t, growing)
		if !world.gate.Over(idle) || world.gate.Over(under) {
			t.Fatalf("the gate reads the network at its limit over %t, the one under it %t", world.gate.Over(idle), world.gate.Over(under))
		}

		// two erasers re-key at the limit, each one full i/ segment
		uploadByteCount := int64(sidecar.QuotaClosureUploadMaxByteCount)
		closureByteCount := int64(0)
		for range 2 {
			if refusal := world.takeClosure(t, growing, connect.NewId(), uploadByteCount); refusal != nil {
				t.Fatalf("a closure upload at the limit: %+v", refusal)
			}
			closureByteCount += uploadByteCount
		}
		// under its limit an erasure's upload is an ordinary write: it passes with no count
		if refusal, err := world.gate.ChargeClosure(ctx, under, connect.NewId(), uploadByteCount, connect.NewId()); err != nil || refusal != nil {
			t.Fatalf("a closure upload under the limit: %+v, %v", refusal, err)
		}
		// the uploads' bytes land, in evs's batch and then in the nightly count of what the listing shows
		world.reportStored(t, growing, closureByteCount)
		if err := model.SetNetworkStoredByteCount(ctx, server.Id(growing), fs47AbuseStorageLimit+closureByteCount); err != nil {
			t.Fatal(err)
		}

		now := world.clock.Now()
		reads := fs47AbuseReadGrowth(t, ctx, growing, now)
		if reads.closureByteCount != closureByteCount || !reads.storageOver || reads.limitByteCount != fs47AbuseStorageLimit || reads.storedByteCount != fs47AbuseStorageLimit+closureByteCount {
			t.Fatalf("the growing network reads %+v, want %d closure bytes over a limit of %d", reads, closureByteCount, fs47AbuseStorageLimit)
		}
		if reads := fs47AbuseReadGrowth(t, ctx, idle, now); reads.closureByteCount != 0 || !reads.storageOver || reads.storedByteCount != fs47AbuseStorageLimit {
			t.Fatalf("the network at its limit with no closures reads %+v", reads)
		}
		if reads := fs47AbuseReadGrowth(t, ctx, under, now); reads.closureByteCount != 0 || reads.storageOver {
			t.Fatalf("the network under its limit reads %+v", reads)
		}
		// the day's total is the day's: the next UTC day reads zero until its own closures
		if reads := fs47AbuseReadGrowth(t, ctx, growing, now.Add(24*time.Hour)); reads.closureByteCount != 0 {
			t.Fatalf("the next day reads %d closure bytes", reads.closureByteCount)
		}
	})
}

// The reviewed tightening on T3: the rows limit_tighten writes, citing the case, take each eraser's allowance to the
// 4 MiB floor and lower the network's config_statements_per_day, and nothing else: another eraser's allowance, a
// network of the same tier, and the revocations the log exempts are untouched; rows that would set the floor or the
// pacing, or go below the floor, are refused, and read as the floor or nothing when written directly; a lift gives
// the tier's limits back.
func TestFs47T3ReviewedTightening(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(tb testing.TB) {
		defer model.Testing_SetEnforceSidecarLimits(true)()
		// the process's limit snapshot is dropped at the end, so the next test reads limits as a fresh process does
		defer model.Testing_SetNetworkSidecarLimitSettings(model.DefaultNetworkSidecarLimitSettings())()
		defer model.Testing_SetSidecarLimits(model.NetworkTierFree, fs47AbuseLimits())()
		ctx := t.Context()
		world := newFs47AbuseWorld(t, ctx)
		networkId := world.network(t, fs47AbuseStorageLimit)
		siblingId := world.network(t, fs47AbuseStorageLimit)
		world.openGate(t, networkId)
		reason := "abuse:" + fs47AbuseOpenCase(t, ctx, networkId, world.clock.Now()).String()

		// what may not stand is refused before anything writes it (owner decision 146)
		for _, row := range []struct {
			limitKey   string
			limitValue int64
		}{
			{limitKey: "evs_closure_bytes_per_day", limitValue: fs47AbuseClosureFloor - 1},
			{limitKey: "evs_closure_bytes_floor", limitValue: fs47AbuseClosureFloor},
			{limitKey: "evs_closure_uploads_per_s", limitValue: 1},
		} {
			if err := model.ValidateNetworkSidecarLimit(row.limitKey, row.limitValue, reason); !errors.Is(err, model.ErrNetworkSidecarLimitRefused) {
				t.Fatalf("a %s row of %d: %v", row.limitKey, row.limitValue, err)
			}
			if err := model.SetNetworkSidecarLimit(ctx, server.Id(networkId), row.limitKey, row.limitValue, reason); !errors.Is(err, model.ErrNetworkSidecarLimitRefused) {
				t.Fatalf("a %s row of %d written: %v", row.limitKey, row.limitValue, err)
			}
		}
		// the review's rows: allowances at the floor, the statements a day lowered
		tightening := map[string]int64{
			"evs_closure_bytes_per_day": fs47AbuseClosureFloor,
			"config_statements_per_day": fs47AbuseStatementsPerDay,
		}
		for limitKey, limitValue := range tightening {
			if err := model.ValidateNetworkSidecarLimit(limitKey, limitValue, reason); err != nil {
				t.Fatal(err)
			}
			if err := model.SetNetworkSidecarLimit(ctx, server.Id(networkId), limitKey, limitValue, reason); err != nil {
				t.Fatal(err)
			}
		}
		limits := world.limits(t, networkId)
		if limits["evs_closure_bytes_per_day"] != fs47AbuseClosureFloor || limits["config_statements_per_day"] != fs47AbuseStatementsPerDay {
			t.Fatalf("the tightened network's limits: %v", limits)
		}
		// abuse limits are tightened per network (D-QUOTA-7): the tier's other networks keep theirs
		if siblingLimits := world.limits(t, siblingId); siblingLimits["evs_closure_bytes_per_day"] != fs47AbuseClosureAllowance || siblingLimits["config_statements_per_day"] != fs47AbuseTierStatementsPerDay {
			t.Fatalf("a network of the same tier: %v", siblingLimits)
		}

		// at the floor an eraser takes ⌊4 MiB / 1,088 KiB⌋ = 3 full re-keys a day, and the fourth waits for 00:00 UTC
		uploadByteCount := int64(sidecar.QuotaClosureUploadMaxByteCount)
		eraserId := connect.NewId()
		for range 3 {
			if refusal := world.takeClosure(t, networkId, eraserId, uploadByteCount); refusal != nil {
				t.Fatalf("a re-key within the floor: %+v", refusal)
			}
		}
		refusal := world.takeClosure(t, networkId, eraserId, uploadByteCount)
		if refusal == nil || refusal.LimitKey != "evs_closure_bytes_per_day" || refusal.Limit != fs47AbuseClosureFloor || refusal.Used != 3*uploadByteCount {
			t.Fatalf("the fourth re-key at the floor: %+v", refusal)
		}
		if want := fs47AbuseMsToNextDay(world.clock.Now()); refusal.RetryAfterMs != want {
			t.Fatalf("the allowance's refusal waits %d ms, want %d to 00:00 UTC", refusal.RetryAfterMs, want)
		}
		// a CLOSURE push's inline records spend the same allowance: one byte past what is left of it is refused
		refusal, err := world.gate.ChargeClosureRecords(ctx, networkId, eraserId, fs47AbuseClosureFloor-3*uploadByteCount+1, connect.NewId())
		if err != nil || refusal == nil || refusal.LimitKey != "evs_closure_bytes_per_day" {
			t.Fatalf("a CLOSURE push's records past the allowance: %+v, %v", refusal, err)
		}
		// no eraser spends another's allowance
		if refusal := world.takeClosure(t, networkId, connect.NewId(), uploadByteCount); refusal != nil {
			t.Fatalf("another eraser's re-key: %+v", refusal)
		}

		// the statements a day bound the admissions, each a new eraser, while revocations still append
		fs47AbuseCheckStatements(t, ctx, networkId, fs47AbuseStatementsPerDay)

		// rows written to the table directly, past the validation: one under the floor reads the floor, and one
		// setting the pacing is ignored
		server.Tx(ctx, func(tx server.PgTx) {
			for limitKey, limitValue := range map[string]int64{
				"evs_closure_bytes_per_day": 1024 * 1024,
				"evs_closure_uploads_per_s": 1,
			} {
				server.RaisePgResult(tx.Exec(
					ctx,
					`
						INSERT INTO network_sidecar_limit (network_id, limit_key, limit_value, reason)
						VALUES ($1, $2, $3, $4)
						ON CONFLICT (network_id, limit_key) DO UPDATE
						SET limit_value = EXCLUDED.limit_value, reason = EXCLUDED.reason
					`,
					server.Id(networkId),
					limitKey,
					limitValue,
					reason,
				))
			}
		})
		model.ReloadNetworkSidecarLimits(ctx)
		limits = world.limits(t, networkId)
		if limits["evs_closure_bytes_per_day"] != fs47AbuseClosureFloor || limits["evs_closure_uploads_per_s"] != 4 {
			t.Fatalf("rows written directly read %v", limits)
		}
		// a fresh eraser still gets its three re-keys, where a 1 MiB allowance would refuse the first
		clampedEraserId := connect.NewId()
		for range 3 {
			if refusal := world.takeClosure(t, networkId, clampedEraserId, uploadByteCount); refusal != nil {
				t.Fatalf("a re-key under a row below the floor: %+v", refusal)
			}
		}

		// the lift clears the rows: the tier's limits return, and the eraser held at the floor takes its fourth
		for _, limitKey := range []string{"evs_closure_bytes_per_day", "evs_closure_uploads_per_s", "config_statements_per_day"} {
			if cleared, err := model.ClearNetworkSidecarLimit(ctx, server.Id(networkId), limitKey); err != nil || !cleared {
				t.Fatalf("the lift of %s: cleared %t, %v", limitKey, cleared, err)
			}
		}
		limits = world.limits(t, networkId)
		if limits["evs_closure_bytes_per_day"] != fs47AbuseClosureAllowance || limits["config_statements_per_day"] != fs47AbuseTierStatementsPerDay {
			t.Fatalf("the lifted network's limits: %v", limits)
		}
		if refusal := world.takeClosure(t, networkId, eraserId, uploadByteCount); refusal != nil {
			t.Fatalf("a re-key after the lift: %+v", refusal)
		}
	})
}

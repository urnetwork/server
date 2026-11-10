package model

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §7.9's append limits (TEAM-M2-14) on a fake clock: the limiter's own clock, in µs, which the script uses in
// place of the Redis master's.
type testConfigLogLimitClock struct {
	nowMicros int64
}

func (self *testConfigLogLimitClock) advance(d time.Duration) {
	self.nowMicros += int64(d / time.Microsecond)
}

// pro.yml's free tier numbers (config/all/pro.yml), which the test stack's pro config does not carry.
const testConfigLogLimitsYml = `
free:
  sidecar:
    config_statements_per_min: 60
    config_statements_burst: 600
    config_statements_per_day: 2000
    config_bases_per_min: 1
    config_bases_per_day: 200
`

func newTestConfigLogLimiter() (*configLogRedisLimiter, *testConfigLogLimitClock) {
	clock := &testConfigLogLimitClock{nowMicros: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC).UnixMicro()}
	limiter := NewConfigLogRedisLimiter(nil).(*configLogRedisLimiter)
	limiter.nowMicros = func() int64 { return clock.nowMicros }
	limits := parseProSidecarYml(testConfigLogLimitsYml).TierFor(NetworkTierFree).Sidecar
	limiter.limits = func(ctx context.Context, networkId server.Id) *ProSidecarLimits {
		return &limits
	}
	return limiter, clock
}

func requireConfigLogLimited(t testing.TB, err error, minMs uint32, maxMs uint32) {
	t.Helper()
	limitErr := &ConfigLogLimitError{}
	if !errors.As(err, &limitErr) || limitErr.RetryAfterMs < minMs || maxMs < limitErr.RetryAfterMs {
		t.Fatalf("limit = %v, want a wait in [%d, %d] ms", err, minMs, maxMs)
	}
}

// A network takes 600 statements at once, the 601st waits a second, and 60 a minute go on after; the day's
// 2,000 then stop it until the next UTC day. Free tier, pro.yml's numbers.
func TestConfigLogLimitsStatements(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		limiter, clock := newTestConfigLogLimiter()
		networkId := server.NewId()
		for i := range 600 {
			if err := limiter.ChargeStatement(ctx, networkId); err != nil {
				t.Fatalf("statement %d: %v", i, err)
			}
		}
		requireConfigLogLimited(t, limiter.ChargeStatement(ctx, networkId), 999, 1000)
		clock.advance(time.Second)
		connect.AssertEqual(t, limiter.ChargeStatement(ctx, networkId), nil)

		// a minute's 60 at a time until the day's 2,000 are spent
		charged := 601
		for charged < 2000 {
			clock.advance(time.Minute)
			for range 60 {
				if charged == 2000 {
					break
				}
				if err := limiter.ChargeStatement(ctx, networkId); err != nil {
					t.Fatalf("statement %d: %v", charged, err)
				}
				charged += 1
			}
		}
		clock.advance(time.Minute)
		err := limiter.ChargeStatement(ctx, networkId)
		limitErr := &ConfigLogLimitError{}
		if !errors.As(err, &limitErr) {
			t.Fatalf("past the day's 2,000: %v", err)
		}
		untilMidnight := time.UnixMicro(clock.nowMicros).UTC().Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(time.UnixMicro(clock.nowMicros).UTC())
		connect.AssertEqual(t, int64(limitErr.RetryAfterMs), (untilMidnight.Microseconds()+999)/1000)
		// the answer is 429 with Retry-After and the wait in the body
		connect.AssertEqual(t, limitErr.Error()[:3], "429")
		body := limitErr.HttpErrorResultBody().(map[string]any)
		connect.AssertEqual(t, body["code"], "rate_limited")
		connect.AssertEqual(t, body["retry_after_ms"], limitErr.RetryAfterMs)
		connect.AssertEqual(t, 0 < limitErr.RetryAfterSeconds(), true)
		clock.advance(untilMidnight)
		connect.AssertEqual(t, limiter.ChargeStatement(ctx, networkId), nil)

		// another network has its own buckets
		connect.AssertEqual(t, limiter.ChargeStatement(ctx, server.NewId()), nil)
	})
}

// One base a minute and 200 a UTC day.
func TestConfigLogLimitsBases(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		limiter, clock := newTestConfigLogLimiter()
		networkId := server.NewId()
		connect.AssertEqual(t, limiter.ChargeBase(ctx, networkId), nil)
		requireConfigLogLimited(t, limiter.ChargeBase(ctx, networkId), 59999, 60000)
		clock.advance(30 * time.Second)
		requireConfigLogLimited(t, limiter.ChargeBase(ctx, networkId), 29999, 30000)
		clock.advance(30 * time.Second)
		connect.AssertEqual(t, limiter.ChargeBase(ctx, networkId), nil)
		for i := 2; i < 200; i += 1 {
			clock.advance(time.Minute)
			if err := limiter.ChargeBase(ctx, networkId); err != nil {
				t.Fatalf("base %d: %v", i, err)
			}
		}
		clock.advance(time.Minute)
		limitErr := &ConfigLogLimitError{}
		if err := limiter.ChargeBase(ctx, networkId); !errors.As(err, &limitErr) || limitErr.RetryAfterMs <= 60000 {
			t.Fatalf("past the day's 200: %v", err)
		}
		// statements are not bases
		connect.AssertEqual(t, limiter.ChargeStatement(ctx, networkId), nil)
	})
}

// A limiter for a sequencer test: refuses every charge, counting each.
type testRefusingConfigLogLimiter struct {
	statementCount int
	baseCount      int
}

func (self *testRefusingConfigLogLimiter) ChargeStatement(ctx context.Context, networkId server.Id) error {
	self.statementCount += 1
	return &ConfigLogLimitError{RetryAfterMs: 1000}
}

func (self *testRefusingConfigLogLimiter) ChargeBase(ctx context.Context, networkId server.Id) error {
	self.baseCount += 1
	return &ConfigLogLimitError{RetryAfterMs: 60000}
}

// The sequencer charges a statement and a base once before its append and appends nothing past a limit;
// a retry of an entry already in the log is answered without a charge.
func TestConfigLogSequencerChargesItsLimits(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	limiter := &testRefusingConfigLogLimiter{}
	settings.Limiter = limiter
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	networkId := server.Id(world.NetworkId())

	signedHeader, rows, genesis, configPushes := world.BaseOne()
	_, err := sequencer.AppendBase(t.Context(), networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	requireConfigLogLimited(t, err, 60000, 60000)
	connect.AssertEqual(t, limiter.baseCount, 1)
	tip, err := sequencer.Tip(t.Context(), networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, tip == nil, true)

	settings.Limiter = nil
	pin, err := sequencer.AppendBase(t.Context(), networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	if err != nil {
		t.Fatal(err)
	}
	world.SetTip(pin)
	settings.Limiter = limiter
	// the same base again is its retry: answered, never charged
	again, err := sequencer.AppendBase(t.Context(), networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, again.GetVersion(), pin.GetVersion())
	connect.AssertEqual(t, limiter.baseCount, 1)

	statement := world.MemberAdmit()
	_, limitErr := sequencer.AppendStatement(t.Context(), networkId, statement)
	requireConfigLogLimited(t, limitErr, 1000, 1000)
	connect.AssertEqual(t, limiter.statementCount, 1)
	tip, err = sequencer.Tip(t.Context(), networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, tip.GetVersion(), pin.GetVersion())

	// a 429 is no ConfigLogError: the router answers its own status and body
	configLogError := &ConfigLogError{}
	connect.AssertEqual(t, errors.As(limitErr, &configLogError), false)
	connect.AssertEqual(t, limitErr.Error()[:3], "429")
}

// The exempt types and the repeat rule's order: revocations of every kind, Suspend, BridgeWithdraw and
// RecoveryVeto never count; compromise is over lost and removed, any of the three over reaped, a since_n
// over none and an earlier since_n over a later.
func TestConfigLogExemptionsAndReasons(t *testing.T) {
	for _, signedType := range []bridge.SignedType{
		bridge.SignedType_SIGNED_SIDECAR_REVOKE, bridge.SignedType_SIGNED_MEMBER_REVOKE, bridge.SignedType_SIGNED_DELEGATION_REVOKE,
		bridge.SignedType_SIGNED_KEY_REVOKE, bridge.SignedType_SIGNED_GRANT_REVOKE, bridge.SignedType_SIGNED_SUSPEND,
		bridge.SignedType_SIGNED_BRIDGE_WITHDRAW, bridge.SignedType_SIGNED_RECOVERY_VETO,
	} {
		connect.AssertEqual(t, configLogExemptTypes[signedType], true)
	}
	for _, signedType := range []bridge.SignedType{
		bridge.SignedType_SIGNED_MEMBER_ADMIT, bridge.SignedType_SIGNED_SIDECAR_ADMIT, bridge.SignedType_SIGNED_REPO_CREATE,
	} {
		connect.AssertEqual(t, configLogExemptTypes[signedType], false)
	}
	compromise := configLogRevokeReasonStrength(bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_COMPROMISE)
	lost := configLogRevokeReasonStrength(bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_LOST)
	removed := configLogRevokeReasonStrength(bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED)
	reaped := configLogRevokeReasonStrength(bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REAPED)
	connect.AssertEqual(t, lost < compromise && removed < compromise, true)
	connect.AssertEqual(t, lost == removed, true)
	connect.AssertEqual(t, reaped < lost, true)
	connect.AssertEqual(t, configLogSinceStronger(7, 0), true)
	connect.AssertEqual(t, configLogSinceStronger(5, 7), true)
	connect.AssertEqual(t, configLogSinceStronger(7, 5), false)
	connect.AssertEqual(t, configLogSinceStronger(7, 7), false)
	connect.AssertEqual(t, configLogSinceStronger(0, 7), false)
}

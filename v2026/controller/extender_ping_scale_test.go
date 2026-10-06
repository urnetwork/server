package controller

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/jwt"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// The ingest at the scale target (connect/GEOMAP.md §5.8 item 3, D27): report
// verification measured through the endpoint's own path, the budget it must
// meet on one core against the target's rate, the per-user report rate limit
// against an extender's posts, and a replay posted on a later day.

// Pings verified a second on one core that the ingest must sustain: two
// ed25519 verifications each, the pinger's signature and the target's
// co-signature (§5.8 item 3).
const IngestVerifyPerSecondPerCore = 10_000

// The target's pings a day, as the table growth model counts them
// (taskworker/work/ping_retention_scale_test.go): a million extenders pinging
// 64 peers eight times a peer a day, and a million providers of whom half make
// a probe pass of 32 pings a day.
const ingestTargetPingsPerDay = 1_000_000*64*8 + 1_000_000*32/2

// A report's keys held in memory, so the benchmark runs the endpoint's checks
// with no database behind them.
type memoryPingReportKeys struct {
	providerPublicKey ed25519.PublicKey
	keyHexExtenders   map[string]*model.NetworkExtender
}

// Implements pingReportKeys.
func (self *memoryPingReportKeys) providerKey() (ed25519.PublicKey, error) {
	return self.providerPublicKey, nil
}

// Implements pingReportKeys.
func (self *memoryPingReportKeys) extenderByKey(publicKey []byte) *model.NetworkExtender {
	return self.keyHexExtenders[hex.EncodeToString(publicKey)]
}

// A report as an extender's reporter posts it -- a full batch of co-signed
// claims, one a target -- with the keys it verifies under, in memory.
type testPingVerifyReport struct {
	args             *ExtenderPingReportArgs
	reporterClientId server.Id
	now              time.Time
	keys             *memoryPingReportKeys
	settings         *ExtenderPingReportSettings
}

// One extender's report of `pingCount` co-signed pings to as many targets.
func newTestPingVerifyReport(t testing.TB, pingCount int) *testPingVerifyReport {
	t.Helper()
	// an extender identity held in memory only: its row as the extender
	// table would return it, and the key it signs with
	newMemoryTestPingExtender := func(clientId server.Id) *testPingExtender {
		seed, err := connect.NewExtenderKeySeed()
		if err != nil {
			t.Fatal(err)
		}
		privateKey, err := connect.ExtenderPrivateKeyFromSeed(seed)
		if err != nil {
			t.Fatal(err)
		}
		return &testPingExtender{
			extender: &model.NetworkExtender{
				ExtenderId: server.NewId(),
				NetworkId:  server.NewId(),
				ClientId:   clientId,
				PublicKey:  privateKey.Public().(ed25519.PublicKey),
				Active:     true,
			},
			privateKey: privateKey,
		}
	}
	now := server.NowUtc()
	reporterClientId := server.NewId()
	pinger := newMemoryTestPingExtender(reporterClientId)
	keys := &memoryPingReportKeys{
		keyHexExtenders: map[string]*model.NetworkExtender{
			hex.EncodeToString(pinger.extender.PublicKey): pinger.extender,
		},
	}
	args := &ExtenderPingReportArgs{}
	for i := 0; i < pingCount; i += 1 {
		target := newMemoryTestPingExtender(server.NewId())
		keys.keyHexExtenders[hex.EncodeToString(target.extender.PublicKey)] = target.extender
		attestation := newTestPingAttestation(t, pinger.attestor(), target.extender.PublicKey, uint32(20+i), uint64(now.UnixMilli()))
		args.Pings = append(args.Pings, testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation)))
	}
	return &testPingVerifyReport{
		args:             args,
		reporterClientId: reporterClientId,
		now:              now,
		keys:             keys,
		settings:         DefaultExtenderPingReportSettings(),
	}
}

// Verifies one full report per iteration through verifyPingReport, the
// endpoint's path, with its keys in memory, and reports pings a second on the
// one core the loop runs on.
func BenchmarkExtenderPingReportVerify(b *testing.B) {
	report := newTestPingVerifyReport(b, connect.DefaultExtenderPingReporterSettings().MaxBatchCount)
	b.ResetTimer()
	for i := 0; i < b.N; i += 1 {
		pings, rejected, err := verifyPingReport(report.args, report.reporterClientId, report.now, report.keys, report.settings)
		if err != nil || rejected != 0 || len(pings) != len(report.args.Pings) {
			b.Fatalf("verified %d of %d with %d rejected: %v", len(pings), len(report.args.Pings), rejected, err)
		}
		for _, ping := range pings {
			if ping.Cosign != model.NetworkPingCosignCosigned {
				b.Fatalf("a co-signed claim verified as %d", ping.Cosign)
			}
		}
	}
	b.ReportMetric(float64(b.N*len(report.args.Pings))/b.Elapsed().Seconds(), "pings/s")
}

// The core the budget is stated on: one where an ed25519 verification costs
// this much cpu time. Two of them leave 10 us of the budget's 100 us a ping to
// the rest of the path.
const ingestReferenceVerifyCpuTime = 45 * time.Microsecond

// One core verifies at least IngestVerifyPerSecondPerCore pings a second
// through the endpoint's path, and that budget covers the target's rate on one
// core with a margin. Both costs are cpu time over their loops alone, so a
// busy host slows the wall clock but not the measurement: the path's cost a
// ping, and beside it a bare ed25519 verification's, the best of five rounds
// each. The budget holds at the reference core's verification speed, so what
// is asserted is the path's own cost against its two verifications -- a third
// verification, or a lookup a claim, fails it on any host -- while the rate on
// this host is logged as measured. Skipped under the race detector, which
// instruments the arithmetic it measures.
func TestExtenderPingReportVerifiesAtTheIngestBudget(t *testing.T) {
	targetPingsPerSecond := float64(ingestTargetPingsPerDay) / (24 * time.Hour).Seconds()
	if float64(IngestVerifyPerSecondPerCore) < 1.5*targetPingsPerSecond {
		t.Fatalf("the budget of %d a second is under one and a half times the target's %.0f", IngestVerifyPerSecondPerCore, targetPingsPerSecond)
	}
	if controllerRaceEnabled {
		t.Skip("the race detector instruments the verification it would measure")
	}
	processCpuTime := func() time.Duration {
		var usage syscall.Rusage
		if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
			t.Fatal(err)
		}
		return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
	}
	report := newTestPingVerifyReport(t, connect.DefaultExtenderPingReporterSettings().MaxBatchCount)
	reportCount := 50
	pingCount := reportCount * len(report.args.Pings)
	verifyPublicKey, verifyPrivateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	verifyMessage := make([]byte, 180)
	verifySignature := ed25519.Sign(verifyPrivateKey, verifyMessage)

	bestPingCpuTime := time.Duration(0)
	bestVerifyCpuTime := time.Duration(0)
	for round := 0; round < 5; round += 1 {
		cpuStart := processCpuTime()
		wallStart := time.Now()
		for i := 0; i < reportCount; i += 1 {
			pings, rejected, err := verifyPingReport(report.args, report.reporterClientId, report.now, report.keys, report.settings)
			if err != nil || rejected != 0 || len(pings) != len(report.args.Pings) {
				t.Fatalf("verified %d of %d with %d rejected: %v", len(pings), len(report.args.Pings), rejected, err)
			}
		}
		pingCpuTime := (processCpuTime() - cpuStart) / time.Duration(pingCount)
		pingWallTime := time.Since(wallStart) / time.Duration(pingCount)

		cpuStart = processCpuTime()
		for i := 0; i < pingCount; i += 1 {
			if !ed25519.Verify(verifyPublicKey, verifyMessage, verifySignature) {
				t.Fatal("a bare verification failed")
			}
		}
		verifyCpuTime := (processCpuTime() - cpuStart) / time.Duration(pingCount)
		t.Logf(
			"round %d: %s of cpu a ping (%s of wall clock), %s a bare verification",
			round+1,
			pingCpuTime,
			pingWallTime,
			verifyCpuTime,
		)
		if bestPingCpuTime == 0 || pingCpuTime < bestPingCpuTime {
			bestPingCpuTime = pingCpuTime
		}
		if bestVerifyCpuTime == 0 || verifyCpuTime < bestVerifyCpuTime {
			bestVerifyCpuTime = verifyCpuTime
		}
	}
	hostPingsPerSecond := 1 / bestPingCpuTime.Seconds()
	referencePingsPerSecond := hostPingsPerSecond * float64(bestVerifyCpuTime) / float64(ingestReferenceVerifyCpuTime)
	t.Logf(
		"a ping costs %s, %.2f bare verifications of %s: %.0f pings a cpu second on this host, %.0f at the reference core's %s a verification, against the budget of %d; the target's %.0f a second is %.2f cores at the budget",
		bestPingCpuTime,
		float64(bestPingCpuTime)/float64(bestVerifyCpuTime),
		bestVerifyCpuTime,
		hostPingsPerSecond,
		referencePingsPerSecond,
		ingestReferenceVerifyCpuTime,
		IngestVerifyPerSecondPerCore,
		targetPingsPerSecond,
		targetPingsPerSecond/float64(IngestVerifyPerSecondPerCore),
	)
	if referencePingsPerSecond < float64(IngestVerifyPerSecondPerCore) {
		t.Fatalf("at the reference core the path verifies %.0f pings a second, under the budget of %d", referencePingsPerSecond, IngestVerifyPerSecondPerCore)
	}
}

// An extender pinging 64 peers eight times a peer a day stays under the
// per-user report rate limit in its worst hour. The reporter posts when a
// batch fills or a flush timeout after its first ping, so an hour holds at
// most one timed post a flush timeout, one more for the edge, and one full
// batch a batch of pings; the bound puts every ping of the busiest day,
// jitter included, into that one hour. Pure.
func TestExtenderPingReportRateLimitCoversAnExtender(t *testing.T) {
	settings := DefaultExtenderPingReportSettings()
	reporterSettings := connect.DefaultExtenderPingReporterSettings()
	pingerSettings := connect.DefaultExtenderPeerPingerSettings()
	peerCount := 64
	// an ipv4 and an ipv6 address, the most a record lists
	addressFamilyCount := 2
	pingsPerRefresh := peerCount * addressFamilyCount * pingerSettings.ProbeCount
	refreshesPerDay := int((24 * time.Hour) / pingerSettings.RefreshTimeout)
	connect.AssertEqual(t, pingsPerRefresh*refreshesPerDay/peerCount, 8)
	// jitter can bring a refresh early; count the most that fit in a day
	shortestRefresh := time.Duration(float64(pingerSettings.RefreshTimeout) * (1 - pingerSettings.Jitter))
	busiestDayPings := pingsPerRefresh * (int((24*time.Hour)/shortestRefresh) + 1)

	timedPosts := int(settings.RateLimitWindow/reporterSettings.FlushTimeout) + 1
	fullPosts := (busiestDayPings + reporterSettings.MaxBatchCount - 1) / reporterSettings.MaxBatchCount
	worstWindowPosts := timedPosts + fullPosts
	averageWindowPosts := float64(pingsPerRefresh*refreshesPerDay) / float64(24*time.Hour/settings.RateLimitWindow) / float64(pingerSettings.ProbeCount*addressFamilyCount)
	t.Logf(
		"worst window %d posts (%d timed, %d full) of %d allowed; an evenly spread day posts about %.1f a window, one a peer's refresh",
		worstWindowPosts,
		timedPosts,
		fullPosts,
		settings.RateLimit,
		averageWindowPosts,
	)
	if settings.RateLimit <= worstWindowPosts {
		t.Fatalf("an extender posts up to %d reports a window, at or over the limit of %d", worstWindowPosts, settings.RateLimit)
	}
}

// A claim posted again on the next day is a replay (§5.7): the unique key
// carries the day and cannot see the first day's row, the ingest's lookup
// across the replay window does, and the second post stores nothing.
func TestExtenderPingReportRejectsAReplayOnTheNextDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())
		now := server.NowUtc()
		nextDay := now.Add(24 * time.Hour)

		// a timestamp inside the claim window of both posts: ahead of the first
		// by less than the forward skew, behind the second by less than the
		// backward one
		settings := DefaultExtenderPingReportSettings()
		attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 44, uint64(now.Add(time.Minute).UnixMilli()))
		args := &ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))},
		}
		result, err := extenderPingReport(args, clientSession, now, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 1)
		connect.AssertEqual(t, result.Rejected, 0)

		result, err = extenderPingReport(args, clientSession, nextDay, settings)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Error, "")
		connect.AssertEqual(t, result.Accepted, 0)
		connect.AssertEqual(t, result.Rejected, 1)

		storedPings := model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{})
		connect.AssertEqual(t, len(storedPings), 1)
		connect.AssertEqual(t, model.NetworkPingPartitionName(storedPings[0].CreateTime), model.NetworkPingPartitionName(now))
	})
}

// The report budget is each reporting client's own: two extenders activated
// under one user each post the whole allowance in the same window, and each
// is held after its own. An account-wide budget would hold the second
// extender at its first post, and an operator runs its whole fleet under one
// account.
func TestExtenderPingReportRateLimitIsPerClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultExtenderPingReportSettings()
		networkId := server.NewId()
		userId := server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, fmt.Sprintf("ping-rate-test-%s", networkId), userId)
		// an extender's client under the one user
		newUserSession := func() *session.ClientSession {
			deviceId := server.NewId()
			clientId := server.NewId()
			model.Testing_CreateDevice(ctx, networkId, deviceId, clientId, "extender", "ping-rate-test")
			clientSession := session.Testing_CreateClientSession(ctx, &jwt.ByJwt{
				NetworkId: networkId,
				UserId:    userId,
				DeviceId:  &deviceId,
				ClientId:  &clientId,
			})
			clientSession.ClientAddress = net.JoinHostPort("192.0.2.1", "443")
			return clientSession
		}
		clientSessions := []*session.ClientSession{newUserSession(), newUserSession()}
		post := func(clientSession *session.ClientSession) *ExtenderPingReportResult {
			result, err := extenderPingReport(&ExtenderPingReportArgs{
				Pings: []*ExtenderPingArgs{{}},
			}, clientSession, server.NowUtc(), settings)
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		for i, clientSession := range clientSessions {
			for postIndex := 0; postIndex < settings.RateLimit; postIndex += 1 {
				if result := post(clientSession); result.Error != "" {
					t.Fatalf("extender %d was held at post %d of its allowance of %d: %s", i, postIndex+1, settings.RateLimit, result.Error)
				}
			}
		}
		for i, clientSession := range clientSessions {
			if result := post(clientSession); result.Error == "" {
				t.Fatalf("extender %d was not held past its allowance of %d", i, settings.RateLimit)
			}
		}
	})
}

// A stored ping outlives every copy of its claim the report can accept: the
// keep timeout covers the claim window at both skews and the retention, each
// with a sweep interval to spare, and a longer backward skew lengthens it.
// Pure.
func TestExtenderPingReportKeepTimeoutCoversEveryReplay(t *testing.T) {
	settings := DefaultExtenderPingReportSettings()
	connect.AssertEqual(t, settings.KeepTimeout(), 25*time.Hour+5*time.Minute)
	if settings.KeepTimeout() < settings.MaxBackwardClockSkew+settings.MaxForwardClockSkew+settings.SweepTimeout {
		t.Fatalf("the keep timeout %s is under the claim window and a sweep interval", settings.KeepTimeout())
	}
	if settings.KeepTimeout() < settings.Retention+settings.MaxForwardClockSkew+settings.SweepTimeout {
		t.Fatalf("the keep timeout %s is under the retention, the forward skew and a sweep interval", settings.KeepTimeout())
	}
	longer := *settings
	longer.MaxBackwardClockSkew = 48 * time.Hour
	connect.AssertEqual(t, longer.KeepTimeout(), 48*time.Hour+5*time.Minute+time.Hour)
}

// A claim posted just before a day boundary, as far ahead of the operator's
// clock as the report accepts, can be posted again until the backward skew
// closes on it a day later. Every sweep until then, at the sweep's own
// cadence and the day's partition rule, keeps the day holding its row, so the
// copy is refused as a replay at the last instant the report accepts the
// claim at all -- where a drop at the bare retention would already have taken
// the row and let the copy in as a second sample. Past that instant the claim
// is out of time, and the first sweep past the keep timeout drops its day.
func TestExtenderPingReportRefusesAReplayUntilTheClaimExpires(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		settings := DefaultExtenderPingReportSettings()
		partitionSettings := model.DefaultNetworkPingPartitionSettings()
		clientSession, attestor := newTestPingProvider(t, ctx)
		target := newTestPingExtender(t, ctx, server.NewId())

		// a synthetic midnight a month out, and a first post a minute before it
		boundary := server.NowUtc().AddDate(0, 1, 0).Truncate(24 * time.Hour)
		firstPostTime := boundary.Add(-time.Minute)
		claimTime := firstPostTime.Add(settings.MaxForwardClockSkew)
		lastPostTime := claimTime.Add(settings.MaxBackwardClockSkew)
		if !boundary.Before(lastPostTime.Add(-settings.Retention)) {
			t.Fatal("a drop at the retention would not come before the claim expires, so this proves nothing")
		}
		sweep := func(now time.Time) {
			model.MaintainNetworkPingPartitions(ctx, now, settings.KeepTimeout(), partitionSettings)
		}
		partitionPresent := func(partitionName string) bool {
			for _, partition := range model.GetNetworkPingPartitions(ctx) {
				if partition.Name == partitionName {
					return true
				}
			}
			return false
		}
		post := func(now time.Time, args *ExtenderPingReportArgs) *ExtenderPingReportResult {
			result, err := extenderPingReport(args, clientSession, now, settings)
			if err != nil {
				t.Fatal(err)
			}
			connect.AssertEqual(t, result.Error, "")
			return result
		}

		sweep(firstPostTime)
		attestation := newTestPingAttestation(t, attestor, target.extender.PublicKey, 45, uint64(claimTime.UnixMilli()))
		args := &ExtenderPingReportArgs{
			Pings: []*ExtenderPingArgs{testPingArgs(attestation, connect.ExtenderPingCosigned, target.cosign(t, attestation))},
		}
		connect.AssertEqual(t, post(firstPostTime, args).Accepted, 1)
		firstPartitionName := model.NetworkPingPartitionName(firstPostTime)

		for sweepTime := firstPostTime.Add(settings.SweepTimeout); sweepTime.Before(lastPostTime); sweepTime = sweepTime.Add(settings.SweepTimeout) {
			sweep(sweepTime)
		}
		sweep(lastPostTime.Add(-time.Millisecond))
		if !partitionPresent(firstPartitionName) {
			t.Fatalf("the sweep dropped %s while its claim could still be posted", firstPartitionName)
		}
		replay := post(lastPostTime, args)
		connect.AssertEqual(t, replay.Accepted, 0)
		connect.AssertEqual(t, replay.Rejected, 1)
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{})), 1)

		outOfTime := post(lastPostTime.Add(time.Millisecond), args)
		connect.AssertEqual(t, outOfTime.Accepted, 0)
		connect.AssertEqual(t, outOfTime.Rejected, 1)

		sweep(boundary.Add(settings.KeepTimeout()))
		if !partitionPresent(firstPartitionName) {
			t.Fatalf("the sweep dropped %s at its keep timeout, not past it", firstPartitionName)
		}
		sweep(boundary.Add(settings.KeepTimeout() + time.Millisecond))
		if partitionPresent(firstPartitionName) {
			t.Fatalf("the sweep kept %s past its keep timeout", firstPartitionName)
		}
		connect.AssertEqual(t, len(model.GetNetworkPings(ctx, target.extender.ExtenderId, time.Time{})), 0)
	})
}

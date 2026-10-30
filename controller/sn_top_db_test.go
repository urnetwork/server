package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type snTopDbFixture struct {
	ctx      context.Context
	settings *model.VerifySettings
	cfg      *StConfig
	client   *stubStClient
	anchor   time.Time
	// the completed tempo's window: [start, end)
	start time.Time
	end   time.Time
}

const (
	snTopDbEpoch      = 7
	snTopDbEpochStart = 1_000_000
	// the head is 1900 blocks into the epoch: tempo 5 runs, tempo 4
	// [start+1440, start+1800) is complete, and it ended 100 blocks (20
	// minutes) ago
	snTopDbHead = snTopDbEpochStart + 1900
)

func newSnTopDbFixture(t testing.TB, ctx context.Context) *snTopDbFixture {
	self := &snTopDbFixture{ctx: ctx}
	self.anchor = server.NowUtc().Truncate(time.Minute)
	self.start = self.anchor.Add(-460 * model.SnTopBlockTime)
	self.end = self.anchor.Add(-100 * model.SnTopBlockTime)

	self.cfg = &StConfig{Enabled: true, ChainId: 945, ContractAddress: common.HexToAddress("0x2000000000000000000000000000000000000025")}
	self.client = newStubStClient(&StEpochState{
		Epoch:           snTopDbEpoch,
		EpochStartBlock: snTopDbEpochStart,
		TEpochBlocks:    50400,
		HeadBlock:       snTopDbHead,
		HeadBlockTime:   self.anchor,
	})
	SetStConfig(self.cfg)
	SetStClient(self.client)
	snTopDbResetChainState()
	// the API's snapshot cache must not carry another test's database
	snTopResetLatestSnapshotCache()
	t.Cleanup(func() {
		SetStClient(nil)
		SetStConfig(nil)
		snTopDbResetChainState()
		snTopResetLatestSnapshotCache()
	})

	self.settings = model.DefaultVerifySettings()
	self.settings.CohortSize = 10
	self.settings.CohortLifetimeLimit = 20
	self.settings.CohortDeploymentKey = self.cfg.DeploymentKey()
	SetVerifySettings(self.settings)
	model.SetStEpochSummaryCache(ctx, self.cfg.DeploymentKey(), &model.StEpochSummary{
		Epoch:        snTopDbEpoch,
		StartBlock:   snTopDbEpochStart,
		TEpochBlocks: 50400,
	}, time.Hour)
	return self
}

func snTopDbResetChainState() {
	snChainStateCache.lock.Lock()
	snChainStateCache.state = nil
	snChainStateCache.lock.Unlock()
}

func (self *snTopDbFixture) network(t testing.TB, label string) (server.Id, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	name := fmt.Sprintf("sntop%s%s", label, strings.ReplaceAll(networkId.String(), "-", "")[:10])
	model.Testing_CreateNetwork(self.ctx, networkId, name, userId)
	return networkId, session.Testing_CreateClientSession(self.ctx, session.NewByJwt(networkId, userId, name, false, false))
}

// A provider client of the network, providing, on the given egress (none:
// it never becomes verify-eligible), present for [start, end] of the tempo.
func (self *snTopDbFixture) provider(t testing.TB, networkId server.Id, ip string, presentUntil time.Time) server.Id {
	clientId := server.NewId()
	model.Testing_CreateDevice(self.ctx, networkId, server.NewId(), clientId, "sntop", "test")
	modesJson, err := json.Marshal([]model.ProvideMode{model.ProvideModePublic})
	server.Raise(err)
	server.Redis(self.ctx, func(r server.RedisClient) {
		server.Raise(r.Set(self.ctx, fmt.Sprintf("{pm_%s}pms", clientId), modesJson, 0).Err())
	})
	if ip != "" {
		model.FeedVerifyEgress(self.ctx, clientId, netip.MustParseAddr(ip), self.settings)
	}
	var addressHash [32]byte
	copy(addressHash[:], clientId.Bytes())
	model.AddClientReliabilityStatsRange(self.ctx, networkId, clientId, addressHash, self.start, presentUntil, &model.ClientReliabilityStats{
		ConnectionEstablishedCount: 1,
		ProvideEnabledCount:        1,
		ReceiveMessageCount:        1,
		ReceiveByteCount:           1024,
		SendMessageCount:           1,
		SendByteCount:              1024,
	})
	return clientId
}

// Hops of a complete trail through the client, confirmed inside the tempo.
func (self *snTopDbFixture) hops(clientId server.Id, networkId server.Id, prefixes ...byte) {
	trailId := server.NewId()
	hops := []*model.SnTrailHop{{
		TrailId: trailId, HopIndex: 0, ClientId: server.NewId(), PrefixHash: [32]byte{0xee},
		Seed: true, Complete: true, ConfirmTime: self.start.Add(time.Minute),
	}}
	for i, prefix := range prefixes {
		hops = append(hops, &model.SnTrailHop{
			TrailId: trailId, HopIndex: i + 1, ClientId: clientId, NetworkId: &networkId,
			PrefixHash: [32]byte{prefix, byte(i)}, Complete: true,
			ConfirmTime: self.start.Add(time.Duration(2+i) * time.Minute),
		})
	}
	model.Testing_InsertSnTrailHops(self.ctx, hops)
}

func snTopDbNear(t testing.TB, got float64, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func snTopDbRow(t testing.TB, rows []SnTopRow, position int) SnTopRow {
	t.Helper()
	for _, row := range rows {
		if row.Position == position {
			return row
		}
	}
	t.Fatalf("no row at position %d", position)
	return SnTopRow{}
}

// The snapshot task end to end: pinned prefixes, reliability presence, the
// cohort, complete-trail hops and the prior EMA in; ranked rows, labels and
// the next state out; then the routes' controllers and /sn/head read it.
func TestComputeSnTopSnapshotDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()

		// before the first snapshot the board is empty, not an error
		empty, err := GetSnTop(&SnTopArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, empty.Snapshot.Id, "")
		connect.AssertEqual(t, empty.Snapshot.OpenSeats, 200)
		connect.AssertEqual(t, len(empty.Rows), 0)
		connect.AssertEqual(t, empty.Next, (*string)(nil))

		full := fixture.end.Add(-time.Minute)
		half := fixture.start.Add(35 * time.Minute)
		networkA, sessionA := fixture.network(t, "a")
		networkB, sessionB := fixture.network(t, "b")
		networkC, sessionC := fixture.network(t, "c")
		networkD, _ := fixture.network(t, "d")
		// A and B share 198.51.100.8/29; A also has .16/29 for half the tempo
		a1 := fixture.provider(t, networkA, "198.51.100.9", full)
		a2 := fixture.provider(t, networkA, "198.51.100.17", half)
		b1 := fixture.provider(t, networkB, "198.51.100.12", full)
		// C provides but its egress never attributes to it alone
		c1 := fixture.provider(t, networkC, "", full)
		d1 := fixture.provider(t, networkD, "198.51.100.41", full)
		// the cohort measures A's and D's providers, not B's
		model.Testing_AddVerifyCohortMembers(ctx, fixture.cfg.DeploymentKey(), snTopDbEpoch, []server.Id{a1, a2, d1})
		// complete trails showed D's provider in two prefixes
		fixture.hops(d1, networkD, 0xd1, 0xd2)
		_, _ = SetNetworkPointsLeaderboardPublic(&SetNetworkPointsRankingPublicArgs{Public: true}, sessionA)

		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Skipped, "")
		connect.AssertEqual(t, result.TempoSource, model.SnTopTempoSourceChain)
		connect.AssertEqual(t, result.TempoIndex, int64(snTopDbEpoch*140+4))
		connect.AssertEqual(t, result.TotalRanked, 3)
		connect.AssertEqual(t, result.EligibleProviders, 4)
		connect.AssertEqual(t, result.TrailHops, 2)
		// the same tempo is never folded twice
		again, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, again.Skipped, "folded")

		first, err := GetSnTop(&SnTopArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		snapshot := first.Snapshot
		connect.AssertEqual(t, snapshot.Id, result.SnapshotId.String())
		connect.AssertEqual(t, snapshot.TempoIndex, int64(snTopDbEpoch*140+4))
		connect.AssertEqual(t, snapshot.Block, uint64(snTopDbEpochStart+1800))
		connect.AssertEqual(t, snapshot.SettlementEpoch, uint64(snTopDbEpoch))
		connect.AssertEqual(t, snapshot.TotalRanked, 3)
		connect.AssertEqual(t, snapshot.HeadSeats, 200)
		connect.AssertEqual(t, snapshot.OpenSeats, 200)
		connect.AssertEqual(t, snapshot.ClaimedFleets, 0)
		connect.AssertEqual(t, snapshot.ProjectedCutoff, 0.0)
		connect.AssertEqual(t, snapshot.ClaimedCutoff, (*float64)(nil))
		connect.AssertEqual(t, snapshot.BindingOpen, false)
		connect.AssertEqual(t, snapshot.MeasuredProviders, 3)
		connect.AssertEqual(t, snapshot.CohortCapacity, 10)
		connect.AssertEqual(t, snapshot.PrecisionAt200, (*float64)(nil))
		connect.AssertEqual(t, snapshot.Stale, false)
		connect.AssertEqual(t, len(first.Rows), 3)
		connect.AssertEqual(t, first.Me, (*SnTopRow)(nil))

		// D: two trail prefixes; A: half of the shared /29 plus its own at
		// presence ½; B: half of the shared /29, outside the cohort
		rowD, rowA, rowB := snTopDbRow(t, first.Rows, 1), snTopDbRow(t, first.Rows, 2), snTopDbRow(t, first.Rows, 3)
		snTopDbNear(t, rowD.ProjectedScore, 2)
		connect.AssertEqual(t, rowD.Prefixes, 2)
		connect.AssertEqual(t, rowD.SharedPrefixes, 0)
		connect.AssertEqual(t, rowD.Label, model.SnTopLabelEstimate)
		connect.AssertEqual(t, rowD.ClaimPrompt, true)
		connect.AssertEqual(t, rowD.Name, "")
		snTopDbNear(t, rowA.ProjectedScore, 1)
		snTopDbNear(t, rowA.RawLastTempo, 1)
		snTopDbNear(t, rowA.Presence, 0.75)
		connect.AssertEqual(t, rowA.Prefixes, 2)
		connect.AssertEqual(t, rowA.SharedPrefixes, 1)
		connect.AssertEqual(t, rowA.Measurable, true)
		connect.AssertEqual(t, rowA.VerifyEligible, true)
		// A turned its name on for the leaderboards
		connect.AssertEqual(t, strings.HasPrefix(rowA.Name, "sntopa"), true)
		snTopDbNear(t, rowB.ProjectedScore, 0.5)
		connect.AssertEqual(t, rowB.Measurable, false)
		connect.AssertEqual(t, rowB.Label, model.SnTopLabelNotMeasurable)
		connect.AssertEqual(t, rowB.ClaimPrompt, false)
		connect.AssertEqual(t, rowB.Name, "")
		for _, row := range first.Rows {
			connect.AssertEqual(t, row.IsMe, false)
			connect.AssertEqual(t, len(row.Fleets), 0)
		}
		firstJson, _ := json.Marshal(first)
		for _, networkId := range []server.Id{networkA, networkB, networkC, networkD} {
			if strings.Contains(string(firstJson), networkId.String()) {
				t.Fatalf("the public board carries a network id: %s", firstJson)
			}
		}

		// the next tempo: B's provider loses eligibility and complete trails
		// showed A's provider in six prefixes
		model.RemoveVerifyEgressForClient(ctx, b1)
		fixture.start = fixture.start.Add(model.SnTopTempoDuration)
		fixture.end = fixture.end.Add(model.SnTopTempoDuration)
		fixture.hops(a1, networkA, 0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6)
		fixture.hops(d1, networkD, 0xd1, 0xd2)
		model.AddClientReliabilityStatsRange(ctx, networkA, a2, [32]byte{0xa2}, fixture.start, fixture.start.Add(35*time.Minute), &model.ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1, ReceiveByteCount: 1024, SendMessageCount: 1, SendByteCount: 1024})
		model.AddClientReliabilityStatsRange(ctx, networkC, c1, [32]byte{0xc1}, fixture.start, fixture.end.Add(-time.Minute), &model.ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1})
		fixture.client.state = &StEpochState{
			Epoch: snTopDbEpoch, EpochStartBlock: snTopDbEpochStart, TEpochBlocks: 50400,
			HeadBlock: snTopDbHead + 360, HeadBlockTime: fixture.anchor.Add(model.SnTopTempoDuration),
		}
		snTopDbResetChainState()
		// An anchor refresh cannot advance time or refold a completed interval.
		recent, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, recent.Skipped, "folded")
		second, err := computeSnTopSnapshotAt(&ComputeSnTopSnapshotArgs{}, anonymous, fixture.anchor.Add(model.SnTopTempoDuration))
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, second.Skipped, "")
		connect.AssertEqual(t, second.TempoIndex, int64(snTopDbEpoch*140+5))

		// signed in as B: the public rows plus B's own row as `me`
		board, err := GetSnTop(&SnTopArgs{Limit: 2}, sessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, board.Snapshot.Id, second.SnapshotId.String())
		connect.AssertEqual(t, len(board.Rows), 2)
		// A: ¼·(6 + ½) + ¾·1 = 2.375 overtakes D: ¼·2 + ¾·2 = 2
		snTopDbNear(t, board.Rows[0].ProjectedScore, 2.375)
		connect.AssertEqual(t, board.Rows[0].Trend, 1)
		connect.AssertEqual(t, board.Rows[0].Position, 1)
		snTopDbNear(t, board.Rows[1].ProjectedScore, 2)
		connect.AssertEqual(t, board.Rows[1].Trend, -1)
		connect.AssertNotEqual(t, board.Next, (*string)(nil))
		connect.AssertEqual(t, board.Me.IsMe, true)
		connect.AssertEqual(t, board.Me.Position, 3)
		// B had no eligible provider this tempo: it decays by ¾
		snTopDbNear(t, board.Me.ProjectedScore, 0.375)
		connect.AssertEqual(t, board.Me.VerifyEligible, false)
		nextPage, err := GetSnTop(&SnTopArgs{Limit: 2, Cursor: *board.Next}, sessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(nextPage.Rows), 1)
		connect.AssertEqual(t, nextPage.Rows[0].Position, 3)
		connect.AssertEqual(t, nextPage.Rows[0].IsMe, true)
		connect.AssertEqual(t, nextPage.Next, (*string)(nil))
		// the cursor stays pinned to its snapshot and filter
		_, err = GetSnTop(&SnTopArgs{Cursor: *board.Next, Filter: "claimed"}, sessionB)
		connect.AssertNotEqual(t, err, nil)
		pinned, err := GetSnTop(&SnTopArgs{Snapshot: result.SnapshotId.String()}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, pinned.Snapshot.Id, result.SnapshotId.String())
		connect.AssertEqual(t, len(pinned.Rows), 3)
		_, err = GetSnTop(&SnTopArgs{Snapshot: server.NewId().String()}, anonymous)
		connect.AssertNotEqual(t, err, nil)
		_, err = GetSnTop(&SnTopArgs{Filter: "everything"}, anonymous)
		connect.AssertNotEqual(t, err, nil)
		claimedOnly, err := GetSnTop(&SnTopArgs{Filter: "claimed"}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(claimedOnly.Rows), 0)

		// A's detail
		meA, err := GetSnTopMe(sessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, meA.Me.Row.Position, 1)
		connect.AssertEqual(t, meA.Me.Row.IsMe, true)
		connect.AssertEqual(t, meA.Me.Row.ClaimPrompt, true)
		connect.AssertEqual(t, meA.Me.NotMeasurableReason, "")
		connect.AssertEqual(t, meA.Me.DistinctPrefixes, 7)
		connect.AssertEqual(t, meA.Me.SharedPrefixes, 0)
		// solo: no fleet has claimed, so nothing is split: ¼·6.5 + ¾·1.5
		snTopDbNear(t, meA.Me.SoloScore, 2.75)
		// today's point is the newest snapshot; a fold that crossed UTC
		// midnight also recorded the day before
		history := meA.Me.History
		if len(history) < 1 || 2 < len(history) {
			t.Fatalf("history = %+v", history)
		}
		connect.AssertEqual(t, history[len(history)-1].Day, server.NowUtc().Format("2006-01-02"))
		connect.AssertEqual(t, history[len(history)-1].Rank, 1)
		// C provides from a shared egress: never ranked, and told why
		meC, err := GetSnTopMe(sessionC)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, meC.Me.Row.Position, 0)
		connect.AssertEqual(t, meC.Me.Row.Label, model.SnTopLabelNotMeasurable)
		connect.AssertEqual(t, meC.Me.NotMeasurableReason, model.SnTopReasonSharedEgress)
		connect.AssertEqual(t, len(meC.Me.History), 0)
		boardC, err := GetSnTop(&SnTopArgs{}, sessionC)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, boardC.Me, (*SnTopRow)(nil))
		// B has no reliability observations in the new interval and is
		// ranked only by its decaying prior score.
		meB, err := GetSnTopMe(sessionB)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, meB.Me.NotMeasurableReason, model.SnTopReasonNotProviding)

		// /sn/head reads the same snapshot
		headA, err := SnHead(sessionA)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, headA.Error, (*SnHeadError)(nil))
		snTopDbNear(t, headA.Score, 2.375)
		connect.AssertEqual(t, headA.RankEstimate, 1)
		connect.AssertEqual(t, headA.Eligible, true)
		connect.AssertEqual(t, headA.Floor, 0.0)
		connect.AssertEqual(t, headA.Cutoff, 200)
		headC, err := SnHead(sessionC)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, headC.Score, 0.0)
		connect.AssertEqual(t, headC.RankEstimate, 0)
		connect.AssertEqual(t, headC.Eligible, false)

		// the EMA state carries every ranked network to the next tempo
		priorNetworks, _ := model.GetSnTopEmaState(ctx)
		connect.AssertEqual(t, len(priorNetworks), 3)
		snTopDbNear(t, priorNetworks[networkA].Ema, 2.375)
		connect.AssertEqual(t, priorNetworks[networkA].LastPosition, 1)
		connect.AssertEqual(t, priorNetworks[networkA].MeasurableBits, uint8(0x03))
	})
}

// A real trail that completes writes one indexed hop row per hop, with the
// egress prefix its FINAL proof carries, in the terminal row's transaction.
func TestVerifyCompleteTrailWritesSnTrailHopsDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		testVerifyInstallServerKey()
		settings := model.DefaultVerifySettings()
		SetVerifySettings(settings)
		const m = connect.VerifyMMin
		providerIps := map[server.Id]string{}
		var seedProviderId server.Id
		for i := 0; i < m; i++ {
			ip := fmt.Sprintf("203.0.113.%d", 10+10*i)
			providerId := testVerifyProvider(ctx, netip.MustParseAddr(ip), settings)
			providerIps[providerId] = ip
			if i == 0 {
				seedProviderId = providerId
			}
		}
		validatorId, vpk, vpkKey := testVerifyValidator(ctx)
		before := server.NowUtc().Add(-time.Minute)
		result, err := Verify(testVerifySeedArgs(t, validatorId, vpk, vpkKey, m), testVerifySession(ctx, providerIps[seedProviderId]))
		if err != nil {
			t.Fatal(err)
		}
		assign := result.(*connect.VerifyAssignResult)
		var final *connect.VerifyFinalResult
		for step := 0; final == nil; step++ {
			if m < step {
				t.Fatal("trail did not finalize")
			}
			result, err := Verify(testVerifyExtendArgs(t, validatorId, vpk, vpkKey, assign), testVerifySession(ctx, providerIps[server.Id(assign.NextHop)]))
			if err != nil {
				t.Fatal(err)
			}
			if next, ok := result.(*connect.VerifyAssignResult); ok {
				assign = next
			} else {
				final = result.(*connect.VerifyFinalResult)
			}
		}
		if final.Status != connect.VerifyStatusComplete {
			t.Fatalf("final status = %s", final.Status)
		}
		evidence := model.GetSnTopTrailEvidence(ctx, before, server.NowUtc().Add(time.Minute))
		// the seed is not a server assignment and never counts
		connect.AssertEqual(t, evidence.Hops, m-1)
		connect.AssertEqual(t, len(evidence.Prefixes), m-1)
		_, seedCounted := evidence.Prefixes[seedProviderId]
		connect.AssertEqual(t, seedCounted, false)
		for _, hop := range final.Proof.Hops[1:] {
			clientId := server.Id(hop.ClientId)
			connect.AssertEqual(t, evidence.Prefixes[clientId], [][32]byte{hop.EgressIpHash})
		}
	})
}

// A claimed fleet end to end through the chain sync's interfaces: its
// network ranks with its fleet row (the exact replica, the validator's
// result and the incentive as a fraction), is not prompted to claim again,
// sets the claimed floor and takes a seat; the validator's summary names the
// tempo, and /sn/head reports the binding.
func TestComputeSnTopSnapshotClaimedFleetDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		fixture := newSnTopDbFixture(t, ctx)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()

		full := fixture.end.Add(-time.Minute)
		networkA, sessionA := fixture.network(t, "a")
		networkD, sessionD := fixture.network(t, "d")
		a1 := fixture.provider(t, networkA, "198.51.100.9", full)
		d1 := fixture.provider(t, networkD, "198.51.100.41", full)
		model.Testing_AddVerifyCohortMembers(ctx, fixture.cfg.DeploymentKey(), snTopDbEpoch, []server.Id{a1, d1})
		fixture.hops(d1, networkD, 0xd1, 0xd2)

		key := model.SnTopFleetKey{FleetId: [32]byte{0xf1}, Hotkey: [32]byte{0xb1}, Generation: 1, Uid: 42}
		restore := SetSnTopSources(SnTopSources{
			ClaimedFleets: func(context.Context, uint64) []model.SnTopFleetInput {
				return []model.SnTopFleetInput{{Key: key, ValidToEpoch: 12, ClientIds: []server.Id{d1}, NetworkIds: []server.Id{networkD}}}
			},
			ValidatorSummary: func(context.Context) *SnTopValidatorSummary {
				return &SnTopValidatorSummary{
					NativeEpoch: 4321, NativeSnapshotBlock: snTopDbHead - 100, SettlementEpoch: snTopDbEpoch,
					MaximumHeadFleets: 200, EntryCount: 1, ReceivedAt: fixture.end,
					Entries: []SnTopValidatorSummaryEntry{{
						Rank: 1, Uid: 42, Hotkey: key.Hotkey, Score: big.NewRat(2, 1), Selected: true,
						Fleets: []SnTopValidatorSummaryFleet{{FleetId: key.FleetId, Generation: 1}},
					}},
				}
			},
			Incentive: func(context.Context) *SnTopIncentive {
				return &SnTopIncentive{
					At:      fixture.anchor.Add(-5 * time.Minute),
					ByUid:   map[uint16]uint16{42: 1180},
					Holders: map[uint16][32]byte{42: key.Hotkey},
				}
			},
		})
		defer restore()

		result, err := ComputeSnTopSnapshot(&ComputeSnTopSnapshotArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, result.Skipped, "")
		// The estimate cadence is independent of the summary's native epoch.
		connect.AssertEqual(t, result.TempoSource, model.SnTopTempoSourceChain)
		connect.AssertEqual(t, result.TempoIndex, int64(snTopDbEpoch*140+4))

		board, err := GetSnTop(&SnTopArgs{}, sessionA)
		connect.AssertEqual(t, err, nil)
		snapshot := board.Snapshot
		connect.AssertEqual(t, snapshot.TempoIndex, int64(snTopDbEpoch*140+4))
		connect.AssertEqual(t, snapshot.ClaimedFleets, 1)
		connect.AssertEqual(t, snapshot.OpenSeats, 199)
		connect.AssertEqual(t, snapshot.ClaimedCutoff, (*float64)(nil))
		connect.AssertEqual(t, snapshot.ValidatorSummaryAt.Equal(fixture.end), true)
		connect.AssertEqual(t, snapshot.IncentiveAt.Equal(fixture.anchor.Add(-5*time.Minute)), true)
		connect.AssertEqual(t, len(board.Rows), 2)

		rowD, rowA := board.Rows[0], board.Rows[1]
		snTopDbNear(t, rowD.ProjectedScore, 2)
		connect.AssertEqual(t, rowD.Measurable, true)
		// claimed already: not prompted to claim (contract amendment)
		connect.AssertEqual(t, rowD.ClaimPrompt, false)
		// anonymous claimed row: no name, shown by its UID
		connect.AssertEqual(t, rowD.Name, "")
		connect.AssertEqual(t, len(rowD.Fleets), 1)
		fleet := rowD.Fleets[0]
		connect.AssertEqual(t, fleet.Uid, 42)
		connect.AssertEqual(t, fleet.Generation, uint64(1))
		connect.AssertEqual(t, fleet.ValidToEpoch, uint64(12))
		connect.AssertEqual(t, fleet.ReplicaScore, (*float64)(nil))
		connect.AssertEqual(t, *fleet.ValidatorScore, 2.0)
		connect.AssertEqual(t, *fleet.ValidatorRank, 1)
		connect.AssertEqual(t, *fleet.ValidatorRankOf, 1)
		connect.AssertEqual(t, *fleet.Selected, true)
		// a fraction from 0 to 1: 1180 of 65535 is 1.8%
		if *fleet.Incentive < 0.0179 || 0.0181 < *fleet.Incentive {
			t.Fatalf("incentive = %v, want the fraction 0.018", *fleet.Incentive)
		}
		connect.AssertEqual(t, fleet.IncentiveStale, false)
		snTopDbNear(t, rowA.ProjectedScore, 1)
		connect.AssertEqual(t, rowA.ClaimPrompt, true)
		connect.AssertEqual(t, rowA.IsMe, true)
		connect.AssertEqual(t, len(rowA.Fleets), 0)

		claimedOnly, err := GetSnTop(&SnTopArgs{Filter: "claimed"}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(claimedOnly.Rows), 1)
		connect.AssertEqual(t, claimedOnly.Rows[0].Position, 1)
		unclaimed, err := GetSnTop(&SnTopArgs{Filter: "unclaimed"}, anonymous)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, len(unclaimed.Rows), 1)
		connect.AssertEqual(t, unclaimed.Rows[0].Position, 2)

		meD, err := GetSnTopMe(sessionD)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, meD.Me.Row.ClaimPrompt, false)
		connect.AssertEqual(t, len(meD.Me.Row.Fleets), 1)
		headD, err := SnHead(sessionD)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, headD.Bound, true)
		connect.AssertEqual(t, headD.Uid, uint64(42))
		connect.AssertEqual(t, headD.Rank, 1)
		connect.AssertEqual(t, headD.RankEstimate, 1)

		// An unsigned summary adapter cannot seed exact state.
		_, priorFleets := model.GetSnTopEmaState(ctx)
		connect.AssertEqual(t, len(priorFleets), 0)
	})
}

// Before the board's first snapshot /sn/top/me gives no not-measurable
// reason: the board is empty and says nothing about the caller, who may well
// be providing. Once a snapshot exists, a caller without a row in it had no
// verify-eligible provider in the tempo: not providing.
func TestSnTopMeNotMeasurableReasonDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		snTopResetLatestSnapshotCache()
		t.Cleanup(snTopResetLatestSnapshotCache)
		networkId := server.NewId()
		userId := server.NewId()
		name := fmt.Sprintf("sntopreason%s", strings.ReplaceAll(networkId.String(), "-", "")[:10])
		model.Testing_CreateNetwork(ctx, networkId, name, userId)
		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, name, false, false))

		empty, err := GetSnTopMe(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, empty.Snapshot.Id, "")
		connect.AssertEqual(t, empty.Me.NotMeasurableReason, "")
		connect.AssertEqual(t, empty.Me.Row.IsMe, true)
		connect.AssertEqual(t, empty.Me.Row.Position, 0)
		connect.AssertEqual(t, len(empty.Me.History), 0)

		// a snapshot that ranks another network only
		now := server.NowUtc()
		other := server.NewId()
		snapshot := &model.SnTopSnapshot{
			SnapshotId:     server.NewId(),
			TempoIndex:     1,
			TempoSource:    model.SnTopTempoSourceChain,
			TempoStartTime: now.Add(-model.SnTopTempoDuration),
			TempoEndTime:   now,
			ComputedAt:     now,
			TotalRanked:    1,
			HeadSeats:      model.SnTopHeadSeats,
			OpenSeats:      model.SnTopHeadSeats,
			BindingNote:    snTopBindingNote(false),
			PresenceSource: snTopPresenceReliability,
		}
		model.WriteSnTopSnapshot(ctx, snapshot, &model.SnTopProjection{
			Networks: []*model.SnTopNetworkProjection{{
				NetworkId: other, Position: 1, ProjectedScore: 1, Label: model.SnTopLabelEstimate, Measurable: true, VerifyEligible: true,
			}},
			NetworkStates: map[server.Id]model.SnTopUnitState{other: {Ema: 1, LastTempoIndex: 1, LastPosition: 1}},
		})
		snTopResetLatestSnapshotCache()
		ranked, err := GetSnTopMe(clientSession)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, ranked.Snapshot.Id, snapshot.SnapshotId.String())
		connect.AssertEqual(t, ranked.Me.NotMeasurableReason, model.SnTopReasonNotProviding)
		connect.AssertEqual(t, ranked.Me.Row.Position, 0)
		connect.AssertEqual(t, ranked.Me.Row.Label, model.SnTopLabelNotMeasurable)
	})
}

package controller

// GET /sn/head follows measurability (owner decision 2026-11-17). The
// published claim guide says to claim only while the network is measurable
// and prompted to claim, but /sn/head called every network the board ranked
// inside the cutoff eligible, so the ur.io Top 200 page told not-measurable
// networks "You qualify". Boards and networks are synthetic.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// snHeadDbNetwork is a network and a signed-in session of it.
func snHeadDbNetwork(t testing.TB, ctx context.Context, label string) (server.Id, *session.ClientSession) {
	networkId := server.NewId()
	userId := server.NewId()
	name := fmt.Sprintf("snhead%s%s", label, strings.ReplaceAll(networkId.String(), "-", "")[:10])
	model.Testing_CreateNetwork(ctx, networkId, name, userId)
	return networkId, session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, name, false, false))
}

// snHeadDbBoard writes the board's newest snapshot with the given rows and
// makes the API read it.
func snHeadDbBoard(t testing.TB, ctx context.Context, rows []*model.SnTopNetworkProjection) {
	now := server.NowUtc()
	snapshot := &model.SnTopSnapshot{
		SnapshotId:     server.NewId(),
		TempoIndex:     1,
		TempoSource:    model.SnTopTempoSourceChain,
		TempoStartTime: now.Add(-model.SnTopTempoDuration),
		TempoEndTime:   now,
		ComputedAt:     now,
		TotalRanked:    len(rows),
		HeadSeats:      model.SnTopHeadSeats,
		OpenSeats:      model.SnTopHeadSeats,
		BindingOpen:    true,
		BindingNote:    snTopBindingNote(true),
		PresenceSource: snTopPresenceReliability,
	}
	networkStates := map[server.Id]model.SnTopUnitState{}
	for _, row := range rows {
		networkStates[row.NetworkId] = model.SnTopUnitState{Ema: row.ProjectedScore, LastTempoIndex: 1, LastPosition: row.Position}
	}
	model.WriteSnTopSnapshot(ctx, snapshot, &model.SnTopProjection{Networks: rows, NetworkStates: networkStates})
	snTopResetLatestSnapshotCache()
}

// The root cause of the Top 200 page's "You qualify" for 176 of the top 200
// (support pass 20261117T180454Z): eligible came from the rank alone. Inside
// the cutoff, a network the validator can't measure is not eligible, whatever
// the reason, and a measurable one is.
func TestSnHeadIsEligibleOnlyWhileMeasurableDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		snTopResetLatestSnapshotCache()
		t.Cleanup(snTopResetLatestSnapshotCache)
		measurableId, measurableSession := snHeadDbNetwork(t, ctx, "m")
		cohortId, cohortSession := snHeadDbNetwork(t, ctx, "c")
		sharedId, sharedSession := snHeadDbNetwork(t, ctx, "s")
		providingId, providingSession := snHeadDbNetwork(t, ctx, "p")
		snHeadDbBoard(t, ctx, []*model.SnTopNetworkProjection{
			{NetworkId: measurableId, Position: 1, ProjectedScore: 4, Label: model.SnTopLabelEstimate, Measurable: true, VerifyEligible: true, ClaimPrompt: true},
			{NetworkId: cohortId, Position: 2, ProjectedScore: 3, Label: model.SnTopLabelNotMeasurable, VerifyEligible: true, NotMeasurableReason: model.SnTopReasonNotInCohort},
			{NetworkId: sharedId, Position: 3, ProjectedScore: 2, Label: model.SnTopLabelNotMeasurable, NotMeasurableReason: model.SnTopReasonSharedEgress},
			{NetworkId: providingId, Position: 4, ProjectedScore: 1, Label: model.SnTopLabelNotMeasurable, NotMeasurableReason: model.SnTopReasonNotProviding},
		})

		cases := []struct {
			name     string
			session  *session.ClientSession
			position int
			eligible bool
		}{
			{name: "measurable", session: measurableSession, position: 1, eligible: true},
			{name: "not in the cohort", session: cohortSession, position: 2, eligible: false},
			{name: "shared egress", session: sharedSession, position: 3, eligible: false},
			{name: "not providing", session: providingSession, position: 4, eligible: false},
		}
		for _, c := range cases {
			head, err := SnHead(c.session)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if head.Error != nil || head.RankEstimate != c.position || head.Score <= 0 {
				t.Errorf("%s: not read from the board: %+v", c.name, head)
			}
			if head.Eligible != c.eligible {
				t.Errorf("%s at #%d: eligible = %v, want %v", c.name, head.RankEstimate, head.Eligible, c.eligible)
			}
		}
	})
}

// /sn/head says what the page needs to explain a network it doesn't qualify:
// in_head is the rank alone, measurable and its reason are the board's (as
// /sn/top/me says them), and claim_prompt is the board's flag the Earnings
// tile prompts on. A network with no row in the snapshot is not providing.
func TestSnHeadExplainsMeasurabilityDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		snTopResetLatestSnapshotCache()
		t.Cleanup(snTopResetLatestSnapshotCache)
		measurableId, measurableSession := snHeadDbNetwork(t, ctx, "m")
		cohortId, cohortSession := snHeadDbNetwork(t, ctx, "c")
		outsideId, outsideSession := snHeadDbNetwork(t, ctx, "o")
		_, absentSession := snHeadDbNetwork(t, ctx, "a")

		// before the first snapshot nothing qualifies and no reason is known
		before, err := SnHead(cohortSession)
		if err != nil {
			t.Fatal(err)
		}
		if before.Eligible || before.Measurable || before.ClaimPrompt || before.NotMeasurableReason != "" {
			t.Fatalf("before the first snapshot: %+v", before)
		}

		snHeadDbBoard(t, ctx, []*model.SnTopNetworkProjection{
			{NetworkId: measurableId, Position: 1, ProjectedScore: 4, Label: model.SnTopLabelEstimate, Measurable: true, VerifyEligible: true, ClaimPrompt: true},
			{NetworkId: cohortId, Position: 26, ProjectedScore: 3, Label: model.SnTopLabelNotMeasurable, VerifyEligible: true, NotMeasurableReason: model.SnTopReasonNotInCohort},
			{NetworkId: outsideId, Position: SnHeadCutoff + 1, ProjectedScore: 0.5, Label: model.SnTopLabelEstimate, Measurable: true, VerifyEligible: true},
		})

		type standing struct {
			rankEstimate int
			inHead       bool
			eligible     bool
			measurable   bool
			reason       string
			claimPrompt  bool
		}
		cases := []struct {
			name    string
			session *session.ClientSession
			want    standing
		}{
			{name: "measurable inside", session: measurableSession, want: standing{rankEstimate: 1, inHead: true, eligible: true, measurable: true, claimPrompt: true}},
			{name: "not in the cohort inside", session: cohortSession, want: standing{rankEstimate: 26, inHead: true, reason: model.SnTopReasonNotInCohort}},
			{name: "measurable outside", session: outsideSession, want: standing{rankEstimate: SnHeadCutoff + 1, measurable: true}},
			{name: "no row", session: absentSession, want: standing{reason: model.SnTopReasonNotProviding}},
		}
		for _, c := range cases {
			head, err := SnHead(c.session)
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			got := standing{
				rankEstimate: head.RankEstimate,
				inHead:       head.InHead,
				eligible:     head.Eligible,
				measurable:   head.Measurable,
				reason:       head.NotMeasurableReason,
				claimPrompt:  head.ClaimPrompt,
			}
			if got != c.want {
				t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
			}
			if head.Cutoff != SnHeadCutoff || head.Error != nil {
				t.Errorf("%s: %+v", c.name, head)
			}
		}
	})
}

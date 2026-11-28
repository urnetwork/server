package controller

import (
	"testing"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The head standing a board row gives (GET /sn/head, the epoch email): the
// rank alone puts a network in the head, and only a measurable one is
// eligible, which is what the Top 200 page shows "You qualify" on (owner
// decision 2026-11-17). Rows are synthetic.
func TestSnHeadStandingFromSnTopRow(t *testing.T) {
	row := func(position int, score float64, measurable bool, reason string, claimPrompt bool, claimed bool) *model.SnTopNetworkRow {
		return &model.SnTopNetworkRow{
			SnTopNetworkProjection: model.SnTopNetworkProjection{
				NetworkId:           server.NewId(),
				Position:            position,
				ProjectedScore:      score,
				Measurable:          measurable,
				NotMeasurableReason: reason,
				ClaimPrompt:         claimPrompt,
				Claimed:             claimed,
			},
		}
	}
	type want struct {
		score        float64
		rankEstimate int
		inHead       bool
		eligible     bool
		measurable   bool
		reason       string
		claimPrompt  bool
	}
	cases := []struct {
		name string
		row  *model.SnTopNetworkRow
		want want
	}{
		{
			name: "no row: not providing",
			row:  nil,
			want: want{reason: model.SnTopReasonNotProviding},
		},
		{
			name: "unranked shared egress: no rank, its own reason",
			row:  row(0, 0, false, model.SnTopReasonSharedEgress, false, false),
			want: want{reason: model.SnTopReasonSharedEgress},
		},
		{
			name: "measurable inside: eligible and prompted",
			row:  row(12, 3.5, true, model.SnTopReasonNone, true, false),
			want: want{score: 3.5, rankEstimate: 12, inHead: true, eligible: true, measurable: true, claimPrompt: true},
		},
		{
			name: "outside the cohort inside the cutoff: in the head, not eligible",
			row:  row(26, 2.25, false, model.SnTopReasonNotInCohort, false, false),
			want: want{score: 2.25, rankEstimate: 26, inHead: true, reason: model.SnTopReasonNotInCohort},
		},
		{
			name: "shared egress inside the cutoff: in the head, not eligible",
			row:  row(150, 1.5, false, model.SnTopReasonSharedEgress, false, false),
			want: want{score: 1.5, rankEstimate: 150, inHead: true, reason: model.SnTopReasonSharedEgress},
		},
		{
			name: "not providing inside the cutoff: in the head, not eligible",
			row:  row(199, 0.75, false, model.SnTopReasonNotProviding, false, false),
			want: want{score: 0.75, rankEstimate: 199, inHead: true, reason: model.SnTopReasonNotProviding},
		},
		{
			name: "the 200th measurable row is inside",
			row:  row(SnHeadCutoff, 0.5, true, model.SnTopReasonNone, true, false),
			want: want{score: 0.5, rankEstimate: SnHeadCutoff, inHead: true, eligible: true, measurable: true, claimPrompt: true},
		},
		{
			name: "measurable outside the cutoff: not in the head",
			row:  row(SnHeadCutoff+1, 0.5, true, model.SnTopReasonNone, false, false),
			want: want{score: 0.5, rankEstimate: SnHeadCutoff + 1, measurable: true},
		},
		{
			name: "a zero score is not in the head",
			row:  row(3, 0, true, model.SnTopReasonNone, false, false),
			want: want{rankEstimate: 3, measurable: true},
		},
		{
			name: "claimed and measurable: eligible, not prompted again",
			row:  row(4, 6, true, model.SnTopReasonNone, false, true),
			want: want{score: 6, rankEstimate: 4, inHead: true, eligible: true, measurable: true},
		},
	}
	for _, c := range cases {
		result := &SnHeadResult{}
		snHeadStandingFromSnTopRow(c.row, result)
		got := want{
			score:        result.Score,
			rankEstimate: result.RankEstimate,
			inHead:       result.InHead,
			eligible:     result.Eligible,
			measurable:   result.Measurable,
			reason:       result.NotMeasurableReason,
			claimPrompt:  result.ClaimPrompt,
		}
		if got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

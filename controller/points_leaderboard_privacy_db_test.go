package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// LEADERTOP Q12 (owner decision 2026-10-29): on the public points board an
// anonymous row never carries its network id; a named row and the caller's
// own row do, and the caller's row says is_me. Read at the wire, as an app or
// a scraper sees it.
func TestPointsLeaderboardAnonymousRowsOmitNetworkIdDb(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		now := server.NowUtc()
		windows := []model.PointsEpochWindow{{Epoch: 1, Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}}
		pointsLeaderboardOperatorWindowFunc = func(time.Time) []model.PointsEpochWindow { return windows }
		defer func() { pointsLeaderboardOperatorWindowFunc = pointsLeaderboardOperatorWindows }()

		ids := []server.Id{}
		sessions := []*session.ClientSession{}
		for i := 0; i < 3; i += 1 {
			networkId := server.NewId()
			userId := server.NewId()
			name := fmt.Sprintf("points_privacy_%d_%s", i, strings.ReplaceAll(networkId.String(), "-", "")[:8])
			model.Testing_CreateNetwork(ctx, networkId, name, userId)
			model.Testing_InsertAccountPointForBlock(ctx, networkId, model.PointsToNanoPoints(float64(10*(i+1))), 1, now)
			ids = append(ids, networkId)
			sessions = append(sessions, session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, name, false, false)))
		}
		// network 2 shows its name; 0 and 1 stay anonymous
		_, err := SetNetworkPointsLeaderboardPublic(&SetNetworkPointsRankingPublicArgs{Public: true}, sessions[2])
		connect.AssertEqual(t, err, nil)
		anonymous := session.NewLocalClientSession(ctx, "0.0.0.0:0", nil)
		defer anonymous.Cancel()
		_, err = RebuildPointsLeaderboard(&RebuildPointsLeaderboardArgs{}, anonymous)
		connect.AssertEqual(t, err, nil)

		wire := func(clientSession *session.ClientSession) []map[string]any {
			result, err := GetPointsLeaderboard(&PointsLeaderboardArgs{}, clientSession)
			connect.AssertEqual(t, err, nil)
			resultJson, err := json.Marshal(result)
			connect.AssertEqual(t, err, nil)
			var decoded struct {
				Rows []map[string]any `json:"rows"`
			}
			connect.AssertEqual(t, json.Unmarshal(resultJson, &decoded), nil)
			connect.AssertEqual(t, len(decoded.Rows), 3)
			return decoded.Rows
		}

		// signed out: points order is n2 (30, named), n1 (20), n0 (10)
		rows := wire(anonymous)
		connect.AssertEqual(t, rows[0]["network_id"], ids[2].String())
		for _, row := range rows[1:] {
			if _, ok := row["network_id"]; ok {
				t.Fatalf("an anonymous points row carries network_id: %v", row)
			}
		}
		// signed in as n0: its own anonymous row keeps its id for the apps,
		// and says is_me; n1's row still carries nothing
		rows = wire(sessions[0])
		connect.AssertEqual(t, rows[2]["network_id"], ids[0].String())
		connect.AssertEqual(t, rows[2]["is_me"], true)
		if _, ok := rows[1]["network_id"]; ok {
			t.Fatalf("another network's anonymous row carries network_id: %v", rows[1])
		}
		connect.AssertEqual(t, rows[1]["is_me"], false)
	})
}

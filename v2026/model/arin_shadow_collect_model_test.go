package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	"github.com/urnetwork/server/v2026"
)

type arinCollectTestOwner struct {
	current   server.ArinShadowOwnerSnapshot
	available bool
}

func (o *arinCollectTestOwner) ArinShadowCurrentConnection() (server.ArinShadowOwnerSnapshot, bool) {
	row := o.current
	row.At = server.NowUtc()
	return row, o.available
}

func TestArinCurrentCollectJoinsOwningScoreSnapshotWholePGCohortAndFreshLiveOwners(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		city := testing_healthGateCity(ctx, t)
		ids := testing_connectQualifyingProviders(ctx, t, city, 2)
		epoch := server.NowUtc().Add(-72 * time.Hour).Unix()
		lookup := server.NowUtc().Add(-48 * time.Hour).Truncate(time.Microsecond)
		owners := map[server.Id]*arinCollectTestOwner{}
		for _, id := range ids {
			testing_setProviderEgressHealth(ctx, id, 1, 1)
			server.Db(ctx, func(conn server.PgConn) {
				rows, err := conn.Query(ctx, `SELECT connection_id,handler_id FROM network_client_connection WHERE client_id=$1 AND connected`, id)
				server.WithPgResult(rows, err, func() {
					for rows.Next() {
						owner := &arinCollectTestOwner{available: true}
						owner.current.ClientId = id
						owner.current.Address = netip.MustParseAddr("192.0.2.1")
						server.Raise(rows.Scan(&owner.current.ConnectionId, &owner.current.HandlerId))
						owners[owner.current.ConnectionId] = owner
					}
				})
			})
		}
		for id := range owners {
			if err := SetConnectionLocation(ctx, id, city.LocationId, &ConnectionLocationScores{ArinLookupAt: &lookup, ArinDatabaseBuildEpoch: epoch, ArinQualityVerified: true}); err != nil {
				t.Fatal(err)
			}
		}
		testing_rollUpEgress(ctx)
		capture, _ := shadowScoreTestSource(t, 16)
		if err := UpdateClientScores(ctx, time.Hour, 1); err != nil {
			t.Fatal(err)
		}
		snapshot := capture.Snapshot()
		if snapshot == nil {
			t.Fatal("real owning source did not seal")
		}
		writer, err := mmdbwriter.New(mmdbwriter.Options{DatabaseType: "urnetwork arindb", BuildEpoch: epoch, IncludeReservedNetworks: true, Description: map[string]string{"en": "synthetic ARIN capture database"}})
		if err != nil {
			t.Fatal(err)
		}
		_, prefix, _ := net.ParseCIDR("192.0.2.0/24")
		err = writer.Insert(prefix, mmdbtype.Map{"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2), "quality_state": mmdbtype.String("subscriber"), "non_quality": mmdbtype.Bool(false), "risk": mmdbtype.Bool(false)})
		if err != nil {
			t.Fatal(err)
		}
		var data bytes.Buffer
		if _, err = writer.WriteTo(&data); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "private-fixture.mmdb")
		if err = os.WriteFile(path, data.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data.Bytes())
		pin := hex.EncodeToString(sum[:])
		recorder, err := server.OpenArinShadowRecorder(path, pin, path, pin, server.NowUtc().Add(-time.Second), 16)
		if err != nil {
			t.Fatal("open synthetic recorder", err)
		}
		defer recorder.Close()
		resolve := func(ctx context.Context, keys []server.Id) ([]server.ArinShadowCaptureTarget, error) {
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("unbounded owner resolver")
			}
			targets := make([]server.ArinShadowCaptureTarget, len(keys))
			for i, key := range keys {
				targets[i] = server.ArinShadowCaptureTarget{ConnectionId: key, Owner: owners[key]}
			}
			return targets, nil
		}
		report, err := CollectArinShadowCurrentPublic(ctx, recorder, snapshot, []string{"all", city.CountryCode}, resolve)
		if err != nil || !report.CensusComplete || !report.ObservationComplete || !report.NativeMembershipComplete || report.ActualMainCoverage || report.Providers != 2 || report.CapturedConnections != int64(len(owners)) || !report.EarliestLookupAt.Equal(lookup) || report.CohortGenerationSHA256 != snapshot.generation || !report.NativeSourceCompletedAt.Equal(snapshot.sourceCompletedAt) {
			t.Fatal("end-to-end owning source/capture join failed", err)
		}
		for _, bucket := range report.Buckets {
			if bucket.VerifiedSubscriber != 2 || bucket.ActiveQuality != 2 || bucket.CandidateQuality != 2 {
				t.Fatal("live matrix did not preserve same native denominator")
			}
		}
		for _, owner := range owners {
			owner.available = false
			break
		}
		partial, err := CollectArinShadowCurrentPublic(ctx, recorder, snapshot, []string{"all", city.CountryCode}, resolve)
		if err != nil || !partial.CensusComplete || partial.ObservationComplete || partial.Providers != 2 || partial.Reasons["owner_unavailable"] != 1 {
			t.Fatal("closed owner disappeared from full denominator", err)
		}
		for _, bucket := range partial.Buckets {
			if bucket.QualityRemoved != 0 || bucket.QualityIndeterminate != 1 {
				t.Fatal("missing live owner became a known policy exclusion")
			}
		}
		encoded, _ := json.Marshal(report)
		for _, owner := range owners {
			if strings.Contains(string(encoded), owner.current.ClientId.String()) || strings.Contains(string(encoded), owner.current.Address.String()) {
				t.Fatal("private address or identity escaped aggregate")
			}
		}
	})
}

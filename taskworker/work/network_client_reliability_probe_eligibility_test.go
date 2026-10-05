package work

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

type testingProbeEligibilityPublication struct {
	*testingReliabilityPublication
	clients map[string]server.Id
	cycles  map[string]string
}

// Real one-hour histories cross the admission floor as five old failures leave
// the window. The inverse control acquires five recent failures instead.
func newTestingProbeEligibilityPublication(t testing.TB) *testingProbeEligibilityPublication {
	t.Helper()
	ctx := t.Context()
	newEnd := server.NowUtc().Truncate(model.ReliabilityBlockDuration).Add(-2 * model.ReliabilityBlockDuration)
	oldEnd := newEnd.Add(-10 * model.ReliabilityBlockDuration)
	fixture := &testingProbeEligibilityPublication{
		testingReliabilityPublication: &testingReliabilityPublication{ctx: ctx, oldEnd: oldEnd, newEnd: newEnd},
		clients:                       map[string]server.Id{},
		cycles:                        map[string]string{},
	}
	location := &model.Location{LocationType: model.LocationTypeCity, City: "Eligibility City", Region: "Eligibility Region", Country: "Eligibility Country", CountryCode: "zz"}
	model.CreateLocation(ctx, location)
	valid := &model.ClientReliabilityStats{ConnectionEstablishedCount: 1, ProvideEnabledCount: 1, ReceiveMessageCount: 1}
	failing := &model.ClientReliabilityStats{ConnectionNewCount: model.ReliabilityAllowDisconnectCountPerBlock + 1}
	type history struct {
		networkId   server.Id
		clientId    server.Id
		addressHash [32]byte
	}
	histories := map[string]history{}
	for i, name := range []string{"recovered", "missing", "future", "lost", "risk"} {
		networkId, clientId := server.NewId(), server.NewId()
		model.Testing_CreateDevice(ctx, networkId, server.NewId(), clientId, "", "")
		connectionId, _, _, addressHash, err := model.ConnectNetworkClient(ctx, clientId, fmt.Sprintf("192.0.2.%d:20001", 91+i), model.CreateNetworkClientHandler(ctx))
		if err != nil {
			t.Fatal(err)
		}
		if err := model.SetConnectionLocation(ctx, connectionId, location.LocationId, &model.ConnectionLocationScores{ArinRisk: name == "risk"}); err != nil {
			t.Fatal(err)
		}
		if name != "missing" {
			model.SetProvide(ctx, clientId, map[model.ProvideMode][]byte{model.ProvideModePublic: []byte("public-secret")})
		}
		model.AddClientReliabilityStatsRange(ctx, networkId, clientId, addressHash, oldEnd.Add(-12*time.Hour), oldEnd, valid)
		if name != "lost" {
			model.AddClientReliabilityStatsRange(ctx, networkId, clientId, addressHash, oldEnd.Add(-time.Hour), oldEnd.Add(-56*time.Minute), failing)
		}
		fixture.clients[name] = clientId
		histories[name] = history{networkId: networkId, clientId: clientId, addressHash: addressHash}
	}
	model.UpdateClientLocationReliabilities(ctx, oldEnd.Add(-12*time.Hour), server.NowUtc())
	model.UpdateClientReliabilityScores(ctx, oldEnd, false)
	model.UpdateNetworkReliabilityWindowScores(ctx, oldEnd, false)
	model.SetProvide(ctx, fixture.clients["missing"], map[model.ProvideMode][]byte{model.ProvideModePublic: []byte("public-secret")})
	// Both implementations now see the same old scores, leaving rejected cycles
	// false and the never-eligible provider without any scheduling row.
	model.UpdateClientLocationReliabilities(ctx, oldEnd.Add(-time.Hour), server.NowUtc())
	for name, h := range histories {
		model.AddClientReliabilityStatsRange(ctx, h.networkId, h.clientId, h.addressHash, oldEnd.Add(model.ReliabilityBlockDuration), newEnd, valid)
		if name == "lost" {
			model.AddClientReliabilityStatsRange(ctx, h.networkId, h.clientId, h.addressHash, newEnd.Add(-4*time.Minute), newEnd, failing)
		}
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `INSERT INTO client_reliability_rollup(singleton_id,max_drained_block,update_time)
			VALUES(1,$1,$2) ON CONFLICT(singleton_id) DO UPDATE SET max_drained_block=EXCLUDED.max_drained_block,update_time=EXCLUDED.update_time`, newEnd.Unix()/60, server.NowUtc()))
		// Persist old identity/progress and a future retry. Publishing eligibility
		// may not reset any of these, or manufacture accepted measurement credit.
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle
			SET cycle_started_at=$1,next_attempt_at=$1,success_count=2,error_count=3,outcome_count=7,claim_ordinal=9`, oldEnd.Add(-5*time.Hour)))
		server.RaisePgResult(tx.Exec(ctx, `UPDATE provider_egress_probe_cycle SET next_attempt_at=$2 WHERE client_id=$1`, fixture.clients["future"], server.NowUtc().Add(time.Hour)))
	})
	fixture.clientSession = session.Testing_CreateClientSession(ctx, nil)
	fixture.assertGeneration(t, false)
	for name := range fixture.clients {
		exists, eligible, cycle := fixture.cycle(t, name)
		wantExists := name != "missing" && name != "risk"
		if exists != wantExists || eligible != (name == "lost") {
			t.Fatalf("invalid old eligibility fixture %s: exists=%t eligible=%t", name, exists, eligible)
		}
		fixture.cycles[name] = cycle
	}
	return fixture
}

// Preserve every scheduling column except the hint being published. PostgreSQL's
// eligibility trigger may also invalidate derived completed-priority readiness.
func (self *testingProbeEligibilityPublication) cycle(t testing.TB, name string) (exists, eligible bool, state string) {
	t.Helper()
	server.Db(self.ctx, func(conn server.PgConn) {
		rows, err := conn.Query(self.ctx, `SELECT eligible,(to_jsonb(cycle)-'eligible'-'completed_priority_ready')::text
			FROM provider_egress_probe_cycle AS cycle WHERE client_id=$1`, self.clients[name])
		server.WithPgResult(rows, err, func() {
			if rows.Next() {
				exists = true
				server.Raise(rows.Scan(&eligible, &state))
			}
		})
	})
	return
}

func (self *testingProbeEligibilityPublication) assertGeneration(t testing.TB, fresh bool) {
	t.Helper()
	wantEnd := self.oldEnd.Unix()/60 + 1
	if fresh {
		wantEnd = self.newEnd.Unix()/60 + 1
	}
	for name, clientId := range self.clients {
		server.Db(self.ctx, func(conn server.PgConn) {
			rows, err := conn.Query(self.ctx, `SELECT max_block_number,independent_reliability_weight
				FROM client_connection_reliability_score WHERE client_id=$1 AND lookback_index=1`, clientId)
			server.WithPgResult(rows, err, func() {
				if !rows.Next() {
					t.Fatalf("%s has no real one-hour score", name)
				}
				var end int64
				var weight float64
				server.Raise(rows.Scan(&end, &weight))
				wantPass := fresh != (name == "lost")
				if end != wantEnd || (weight >= 0.95) != wantPass {
					t.Errorf("%s fresh=%t: end=%d weight=%g, want end=%d passes=%t", name, fresh, end, weight, wantEnd, wantPass)
				}
			})
		})
	}
}

func (self *testingProbeEligibilityPublication) assertPublished(t testing.TB) {
	t.Helper()
	self.assertGeneration(t, true)
	for name := range self.clients {
		exists, eligible, state := self.cycle(t, name)
		if exists != (name != "risk") || eligible != (name != "risk" && name != "lost") {
			t.Errorf("score publication left %s unsynchronized: exists=%t eligible=%t", name, exists, eligible)
		}
		if name != "missing" && state != self.cycles[name] {
			t.Errorf("score publication changed %s cycle identity, progress or pace", name)
		}
	}
	now := server.NowUtc().Add(time.Second)
	fleet := model.GetProviderUrlProbeFleet(self.ctx, now)
	if fleet.Eligible != 3 || fleet.Due != 2 || fleet.MissingCycles != 0 || fleet.RunsNeeded != 30 || fleet.QuotaComplete != 0 {
		t.Errorf("score publication lost the authoritative cohort or invented quota: %+v", fleet)
	}
	due := model.ClaimProviderUrlProbeDue(self.ctx, now, 10, 0, 1)
	want := map[server.Id]bool{self.clients["recovered"]: true, self.clients["missing"]: true}
	for _, provider := range due {
		if !want[provider.ClientId] {
			t.Errorf("claimed an excluded or future-paced provider: %s", provider.ClientId)
		}
		delete(want, provider.ClientId)
	}
	if len(want) != 0 {
		t.Errorf("newly eligible providers remained hidden from immediate claim: missing=%d", len(want))
	}
	if duplicate := model.ClaimProviderUrlProbeDue(self.ctx, now, 10, 0, 1); len(duplicate) != 0 {
		t.Errorf("score publication bypassed claim exclusivity: %+v", duplicate)
	}
}

// A failing seven-day checkpoint must leave recovered providers immediately
// claimable; waiting for the next location pass or task retry loses probe work.
func TestUpdateReliabilitiesPublishesProbeEligibilityBeforeNetworkFailure(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingProbeEligibilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "client_reliability_running_window", "WHEN (NEW.lookback_index=1000)")
		defer remove()
		fixture.runFailure(t)
		fixture.assertPublished(t)
	})
}

// Failure while publishing a changed hint rolls back its score generation and
// all cycle seeds together, while completed running checkpoints remain durable.
func TestUpdateReliabilitiesProbeEligibilityPublicationIsAtomic(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newTestingProbeEligibilityPublication(t)
		defer fixture.clientSession.Cancel()
		remove := fixture.installFailure(t, "provider_egress_probe_cycle", fmt.Sprintf("WHEN (NEW.client_id='%s'::uuid AND NEW.eligible)", fixture.clients["recovered"]))
		defer remove()
		fixture.runFailure(t)
		fixture.assertGeneration(t, false)
		fixture.assertRunning(t, map[int]bool{0: true, 1: true, 2: true})
		for name := range fixture.clients {
			exists, eligible, state := fixture.cycle(t, name)
			if exists != (name != "missing" && name != "risk") || eligible != (name == "lost") || state != fixture.cycles[name] {
				t.Errorf("failed hint publication changed %s: exists=%t eligible=%t", name, exists, eligible)
			}
		}
	})
}

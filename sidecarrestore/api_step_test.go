//go:build sidecar_pending

package sidecarrestore

// §19.10's api step on private Postgres and T2's fake first cluster (OPS-M3-03). The heads and identity tables are
// MG1's and MG2's, so this runs under sidecar_pending: a restored head advances to the store's verified tip, a network
// whose log began after the sidecar dump gets its head, an orphan's log gets none, removals' effects apply once,
// rev/ objects are owed for a client removed 19 minutes ago and not 21, two runs give one result, and the step
// refuses a database the restore script did not mark and a run while the routes are open.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// T2's fake blobs, its first cluster holding every log a test writes.
func testFakeBlobs(t testing.TB) *sidecartest.FakeBlobs {
	t.Helper()
	return sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
}

// model's sequencer as the suite world's appender.
type testWorldSequencer struct {
	sequencer *model.ConfigLogSequencer
}

func (self testWorldSequencer) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
}

func (self testWorldSequencer) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	return self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
}

func (self testWorldSequencer) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	return self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
}

// One network's log as its owner's devices wrote it on the first cluster, through a sequencer that advances no head:
// what the store holds past the restored rows.
type testLogWorld struct {
	t           testing.TB
	ctx         context.Context
	networkId   server.Id
	ownerUserId server.Id
	world       *sidecartest.ConfigLogSuiteWorld
	sequencer   *model.ConfigLogSequencer
	tip         *bridge.AuthPin
}

// A log at base 1 for networkId, owned by ownerUserId, on cluster.
func newTestLogWorld(t testing.TB, ctx context.Context, cluster sidecar.BlobCluster, networkId server.Id, ownerUserId server.Id) *testLogWorld {
	t.Helper()
	world := sidecartest.NewConfigLogSuiteWorldFor(ctx, cluster, sidecar.Id(networkId), sidecar.Id(ownerUserId), sidecar.NewId())
	settings := model.DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	self := &testLogWorld{t: t, ctx: ctx, networkId: networkId, ownerUserId: ownerUserId, world: world, sequencer: model.NewConfigLogSequencer(cluster, settings, nil)}
	if err := world.Start(testWorldSequencer{sequencer: self.sequencer}); err != nil {
		t.Fatal(err)
	}
	tip, err := self.sequencer.Tip(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	self.tip = tip
	return self
}

// Appends signed and moves the world's tip to it.
func (self *testLogWorld) post(signed []byte) {
	self.t.Helper()
	pin, err := self.sequencer.AppendStatement(self.ctx, self.networkId, signed)
	if err != nil {
		self.t.Fatal(err)
	}
	self.world.SetTip(pin)
	self.tip = pin
}

// The owner admits a fresh device of its own, and answers its id.
func (self *testLogWorld) admit() server.Id {
	self.t.Helper()
	publicKey, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		self.t.Fatal(err)
	}
	sidecarId := server.NewId()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            self.world.Header(),
		NetworkId:         sidecar.Id(self.networkId).Bytes(),
		SidecarId:         sidecar.Id(sidecarId).Bytes(),
		IdentityPublicKey: publicKey,
		IdentityAlg:       "ed25519",
		MemberId:          sidecar.Id(self.ownerUserId).Bytes(),
	}))
	return sidecarId
}

// The owner revokes one of its devices.
func (self *testLogWorld) revoke(sidecarId server.Id) {
	self.t.Helper()
	self.post(self.world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_REVOKE, &bridge.SidecarRevoke{
		Header:    self.world.Header(),
		SidecarId: sidecar.Id(sidecarId).Bytes(),
		Reason:    bridge.SidecarRevokeReason_SIDECAR_REVOKE_REASON_REMOVED,
	}))
}

// The chain value the log holds at n.
func (self *testLogWorld) chainAt(n uint64) []byte {
	self.t.Helper()
	read, err := self.sequencer.Read(self.ctx, self.networkId, n-1)
	if err != nil || len(read.Entries) == 0 {
		self.t.Fatalf("no entry at %d: %v", n, err)
	}
	entry := &bridge.AuthEntry{}
	if err := proto.Unmarshal(read.Entries[0], entry); err != nil {
		self.t.Fatal(err)
	}
	chain, err := bridge.AuthChainDigest(entry.GetN(), entry.GetPrevChain(), entry.GetKind(), entry.GetTimeMs(), entry.GetBody())
	if err != nil {
		self.t.Fatal(err)
	}
	return chain[:]
}

// Sets the comment xops's restore script leaves on the database it loaded under incidentId, on the test's database.
func testMarkRestoredDatabase(t testing.TB, ctx context.Context, incidentId server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.RaisePgResult(conn.Exec(ctx, fmt.Sprintf(
			`DO $$ BEGIN EXECUTE format('COMMENT ON DATABASE %%I IS %%L', current_database(), %s); END $$`,
			"'"+RestoreMarker(incidentId)+"'",
		)))
	})
}

// The api step's deps over the world's cluster and clock, the hold as held says.
func testApiStepDeps(cluster sidecar.BlobCluster, clock sidecar.Clock, held bool, now time.Time) *ApiStepDeps {
	settings := model.DefaultConfigLogSettings()
	settings.Clock = clock
	return &ApiStepDeps{
		Sequencer:    model.NewConfigLogSequencer(cluster, settings, model.AdvanceNetworkAclHead),
		FirstCluster: cluster,
		RestoreHeld:  func() bool { return held },
		Now:          func() time.Time { return now },
	}
}

// The detail of the list's one row of kind for rowId, empty for none; more than one fails.
func testListDetail(t testing.TB, incident *Incident, kind string, rowId server.Id) string {
	t.Helper()
	details := []string{}
	for _, row := range incident.RowsOfKind(kind) {
		if row.RowId == rowId.String() {
			details = append(details, row.Detail)
		}
	}
	if 1 < len(details) {
		t.Fatalf("%s lists %s %d times: %q", kind, rowId, len(details), details)
	}
	if len(details) == 0 {
		return ""
	}
	return details[0]
}

func TestApiStepRefusesADatabaseTheScriptDidNotMark(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, server.NowUtc())
		rowCount := len(incident.Rows())
		err = RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrIncidentMarker), true)
		// another incident's marker
		testMarkRestoredDatabase(t, ctx, server.NewId())
		err = RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrIncidentMarker), true)
		connect.AssertEqual(t, len(incident.Rows()), rowCount)
	})
}

func TestApiStepRefusesWhileTheRoutesAreOpen(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		networkId, ownerUserId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "restore-open", ownerUserId)
		world := newTestLogWorld(t, ctx, cluster, networkId, ownerUserId)
		world.admit()

		deps := testApiStepDeps(cluster, world.world.Clock(), false, server.NowUtc())
		err = RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrRestoreHoldOpen), true)
		head, err := model.GetNetworkAclHead(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head == nil, true)

		// the operator's statement stands in for sidecar.yml, and is listed
		err = RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}, Statement: "the lb answers 503 for api"})
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindRestoreHold, incident.IncidentId()), "stated by hand: the lb answers 503 for api")
	})
}

func TestApiStepAdvancesHeadsAndReappliesRemovalsOnce(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}

		// a network whose head the sidecar dump held at base 1, its log run on past both dumps: devices admitted and
		// one revoked
		restoredId, restoredOwner := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, restoredId, "restore-heads", restoredOwner)
		restored := newTestLogWorld(t, ctx, cluster, restoredId, restoredOwner)
		_, err = model.AdvanceNetworkAclHead(ctx, restoredId, 1, restored.chainAt(1), false)
		connect.AssertEqual(t, err, nil)
		removed := restored.admit()
		restored.admit()
		restored.revoke(removed)

		// a network whose log began after the sidecar dump: its row came back, its head did not
		laterId, laterOwner := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, laterId, "restore-later", laterOwner)
		later := newTestLogWorld(t, ctx, cluster, laterId, laterOwner)
		later.admit()

		// a network newer than both dumps: a log and no row
		orphanId := server.NewId()
		orphan := newTestLogWorld(t, ctx, cluster, orphanId, server.NewId())
		orphan.admit()

		deps := testApiStepDeps(cluster, restored.world.Clock(), true, server.NowUtc())
		out := &bytes.Buffer{}
		connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: out}), nil)

		head, err := model.GetNetworkAclHead(ctx, restoredId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head.Version, restored.tip.GetVersion())
		connect.AssertEqual(t, bytes.Equal(head.Chain, restored.tip.GetChain()), true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindAclHeadAdvanced, restoredId), fmt.Sprintf("1 -> %d", restored.tip.GetVersion()))
		head, err = model.GetNetworkAclHead(ctx, laterId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head.Version, later.tip.GetVersion())
		connect.AssertEqual(t, testListDetail(t, incident, ListKindAclHeadAdvanced, laterId), fmt.Sprintf("0 -> %d", later.tip.GetVersion()))
		head, err = model.GetNetworkAclHead(ctx, orphanId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head == nil, true)

		// every entry's effects were owed: the restored rows held none of their admin events
		effects := testListDetail(t, incident, ListKindEffectsReapplied, restoredId)
		connect.AssertEqual(t, strings.HasSuffix(effects, " entries") && effects != "0 entries", true)
		revoked := false
		for _, event := range model.GetNetworkAdminEvents(ctx, restoredId, 0, 1000) {
			revoked = revoked || strings.HasSuffix(event.Action, "sidecar_revoke")
		}
		connect.AssertEqual(t, revoked, true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindEffectsReapplied, orphanId), "")

		// a second run changes nothing and lists nothing new
		rowCount := len(incident.Rows())
		connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		connect.AssertEqual(t, len(incident.Rows()), rowCount)
		head, err = model.GetNetworkAclHead(ctx, restoredId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, head.Version, restored.tip.GetVersion())
	})
}

func TestApiStepOwesRevObjectsForTwentyMinutes(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		networkId, userId := server.NewId(), server.NewId()
		model.Testing_CreateNetwork(ctx, networkId, "restore-rev", userId)
		userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "restore-rev"})
		mint := func() server.Id {
			result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
			if err != nil || result.Error != nil || result.ByClientJwt == nil {
				t.Fatalf("client: %v %+v", err, result)
			}
			byJwt, err := session.ParseByJwt(ctx, *result.ByClientJwt)
			if err != nil {
				t.Fatal(err)
			}
			return *byJwt.ClientId
		}
		now := server.NowUtc()
		recent, old := mint(), mint()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`, recent, now.Add(-19*time.Minute)))
			server.RaisePgResult(tx.Exec(ctx, `UPDATE network_client SET active = false, deactivate_time = $2 WHERE client_id = $1`, old, now.Add(-21*time.Minute)))
		})
		deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, now)
		connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindRevOwed, incident.IncidentId()), "1 objects")
		connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindRevOwed, incident.IncidentId()), "1 objects")
	})
}

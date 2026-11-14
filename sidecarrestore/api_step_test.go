//go:build sidecar_pending

package sidecarrestore

// §19.10's api step on private Postgres and T2's fake first cluster (OPS-M3-03). The heads and identity tables are
// MG1's and MG2's, so this runs under sidecar_pending: a restored head advances to the store's verified tip, a network
// whose log began after the sidecar dump gets its head, an orphan's log gets none, removals' effects apply once,
// rev/ objects are owed for a client removed 19 minutes ago and not 21, two runs give one result, and the step
// refuses a database the restore script did not mark and a run while the routes are open.
//
// The push tokens' raise (OPS-M8-01): runs of the step under one incident raise every row once, by 2^32; the dump
// loaded again under the incident is raised again, once; a database before MG8 skips the raise; and a database holding
// MG8's rows refuses a binary that does not link MG8. On T3's PostgresLossFixture (SIDECAR_RESTORE_T3=1) the same holds
// through xops's real dumps and restore, the frame running each step twice.

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

// A client of userSession's network as api mints one, holding a push token row: token on platform, or a tombstone
// when token is empty, written as POST /sidecar/push-token writes it.
func testPushTokenClient(t testing.TB, ctx context.Context, userSession *session.ClientSession, platform string, token string) server.Id {
	t.Helper()
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
	if err != nil || result.Error != nil || result.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, result)
	}
	byJwt, err := session.ParseByJwt(ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.SetPushToken(ctx, *byJwt.ClientId, platform, token, nil); err != nil {
		t.Fatal(err)
	}
	return *byJwt.ClientId
}

// Each client's push token row, which must exist.
func testPushTokenRows(t testing.TB, ctx context.Context, clientIds []server.Id) map[server.Id]*model.SidecarPushTokenRow {
	t.Helper()
	clientIdRows := map[server.Id]*model.SidecarPushTokenRow{}
	for _, clientId := range clientIds {
		row, err := model.GetPushToken(ctx, clientId)
		if err != nil || row == nil {
			t.Fatalf("the push token row of %s: %v", clientId, err)
		}
		clientIdRows[clientId] = row
	}
	return clientIdRows
}

// Three clients with push token rows, an APNs token, an FCM token and a tombstone, in a fresh network.
func testPushTokenClients(t testing.TB, ctx context.Context) []server.Id {
	t.Helper()
	networkId, userId := server.NewId(), server.NewId()
	model.Testing_CreateNetwork(ctx, networkId, "restore-push", userId)
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: "restore-push"})
	return []server.Id{
		testPushTokenClient(t, ctx, userSession, sidecar.PushTokenPlatformApns, "synthetic-apns-token"),
		testPushTokenClient(t, ctx, userSession, sidecar.PushTokenPlatformFcm, "synthetic-fcm-token"),
		testPushTokenClient(t, ctx, userSession, sidecar.PushTokenPlatformFcm, ""),
	}
}

// Fails unless each row in after is its row in before raised exactly once: rev up by 2^32, update_time later, and the
// token and platform as they were.
func testRaisedOnce(t testing.TB, before map[server.Id]*model.SidecarPushTokenRow, after map[server.Id]*model.SidecarPushTokenRow) {
	t.Helper()
	for clientId, row := range before {
		raised := after[clientId]
		if raised.Rev != row.Rev+model.PushTokenRestoreRevisionRaise || !raised.UpdateTime.After(row.UpdateTime) ||
			raised.Platform != row.Platform || raised.Token != row.Token || raised.Slot != row.Slot {
			t.Fatalf("%s raised to %s, not once by %d", row, raised, model.PushTokenRestoreRevisionRaise)
		}
	}
}

// What xops's restore script does to sidecar_push_token when it loads the dump again under the incident: a TRUNCATE,
// which gives the table new storage, then the dump's rows as they were.
func testReloadPushTokenRows(t testing.TB, ctx context.Context, rows map[server.Id]*model.SidecarPushTokenRow) {
	t.Helper()
	nullable := func(value string) *string {
		if value == "" {
			return nil
		}
		return &value
	}
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(ctx, `TRUNCATE sidecar_push_token`))
		for _, row := range rows {
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO sidecar_push_token (client_id, platform, token, rev, update_time, removed_time, slot)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				row.ClientId, nullable(row.Platform), nullable(row.Token), row.Rev, row.UpdateTime, row.RemovedTime, row.Slot,
			))
		}
	})
}

func TestApiStepRaisesPushTokensOncePerIncident(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		clientIds := testPushTokenClients(t, ctx)
		before := testPushTokenRows(t, ctx, clientIds)
		storage, present, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, present, true)

		// the step runs again under the incident, as the frame and an operator run it: the rows rise once
		deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, server.NowUtc())
		for range 3 {
			connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		}
		testRaisedOnce(t, before, testPushTokenRows(t, ctx, clientIds))
		raisedRows := incident.RowsOfKind(ListKindPushTokensRaised)
		connect.AssertEqual(t, len(raisedRows), 1)
		connect.AssertEqual(t, raisedRows[0].Table, "sidecar_push_token")
		connect.AssertEqual(t, raisedRows[0].RowId, incident.IncidentId().String())
		connect.AssertEqual(t, raisedRows[0].Detail, pushTokenRaiseDetail(storage, int64(len(clientIds))))
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindPushTokensSkipped)), 0)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindRestoreHold, incident.IncidentId()), "sidecar.yml restore_hold")

		// another incident's database is another restore: its rows rise again, once
		other := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, other.IncidentId())
		raised := testPushTokenRows(t, ctx, clientIds)
		for range 2 {
			connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: other, Out: &bytes.Buffer{}}), nil)
		}
		testRaisedOnce(t, raised, testPushTokenRows(t, ctx, clientIds))
	})
}

func TestApiStepRaisesPushTokensAgainOnceTheDumpLoadsAgain(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		clientIds := testPushTokenClients(t, ctx)
		dumped := testPushTokenRows(t, ctx, clientIds)
		deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, server.NowUtc())
		connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		testRaisedOnce(t, dumped, testPushTokenRows(t, ctx, clientIds))
		firstStorage, _, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)

		// the script loads the dump again under the incident, and this list, which it would rewrite, still holds the
		// first raise: the rows are the dump's again, in new storage, so the step raises them again, once
		testReloadPushTokenRows(t, ctx, dumped)
		secondStorage, present, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, present, true)
		connect.AssertEqual(t, secondStorage != firstStorage, true)
		for range 2 {
			connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		}
		testRaisedOnce(t, dumped, testPushTokenRows(t, ctx, clientIds))
		details := []string{}
		for _, row := range incident.RowsOfKind(ListKindPushTokensRaised) {
			details = append(details, row.Detail)
		}
		connect.AssertEqual(t, details, []string{
			pushTokenRaiseDetail(firstStorage, int64(len(clientIds))),
			pushTokenRaiseDetail(secondStorage, int64(len(clientIds))),
		})
	})
}

func TestApiStepSkipsThePushTokenRaiseBeforeMg8(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		// a database restored from dumps taken before MG8 applied: no sidecar_push_token
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE sidecar_push_token`))
		})
		_, present, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, present, false)

		// a binary before MG8, and one that links it but runs on such a database, both end the step and raise nothing
		for _, linked := range []bool{false, true} {
			deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, server.NowUtc())
			deps.PushTokensLinked = func() bool { return linked }
			connect.AssertEqual(t, RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}}), nil)
		}
		connect.AssertEqual(t, testListDetail(t, incident, ListKindPushTokensSkipped, incident.IncidentId()), "no table: the database predates MG8")
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindPushTokensRaised)), 0)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindRestoreHold, incident.IncidentId()), "sidecar.yml restore_hold")
	})
}

func TestApiStepRefusesAPushTokenRaiseThisBinaryCannotMake(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		incident := testIncident(t, IncidentKindPostgres)
		testMarkRestoredDatabase(t, ctx, incident.IncidentId())
		cluster, err := testFakeBlobs(t).FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		clientIds := testPushTokenClients(t, ctx)
		before := testPushTokenRows(t, ctx, clientIds)

		// the restored database holds MG8's rows and this binary does not link MG8: skipping would leave each row below
		// the lost primary's revision, so the step refuses and does not reach its end
		deps := testApiStepDeps(cluster, sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), true, server.NowUtc())
		deps.PushTokensLinked = func() bool { return false }
		err = RunApiStep(ctx, deps, &StepRun{Incident: incident, Out: &bytes.Buffer{}})
		connect.AssertEqual(t, errors.Is(err, ErrPushTokensUnlinked), true)
		connect.AssertEqual(t, testPushTokenRows(t, ctx, clientIds), before)
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindPushTokensRaised)), 0)
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindPushTokensSkipped)), 0)
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindRestoreHold)), 0)
	})
}

// §19.10's raise on T3 (OPS-M8-01): push tokens set before both dumps and one changed after them, a change the loss
// takes; the primary lost and restored through xops's scripts, the sidecar dump the newer, and the frame run through the
// api step, each step twice: every restored row rose by 2^32 once, above the revision the lost primary reached, and a
// third run under the incident raises nothing more. Runs where SIDECAR_RESTORE_T3=1, with FS-42's schedules.
func TestFs42PushTokensRaisedOncePerIncident(t *testing.T) {
	requireFs42T3(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := NewPostgresLossFixture(t, nil)
		defer fixture.Close()
		blobs := testFakeBlobs(t)
		first, err := blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId))()
		network := newFs42Network(t, ctx, networkId, "push")
		networkLog := newTestLogWorld(t, ctx, first, network.networkId, network.userId)
		_, err = model.AdvanceNetworkAclHead(ctx, network.networkId, 1, networkLog.chainAt(1), false)
		connect.AssertEqual(t, err, nil)

		setToken := func(clientId server.Id, platform string, token string) {
			if _, err := model.SetPushToken(ctx, clientId, platform, token, nil); err != nil {
				t.Fatal(err)
			}
		}
		apnsClientId, fcmClientId, tombstoneClientId := network.clientId, network.mint(t), network.mint(t)
		setToken(apnsClientId, sidecar.PushTokenPlatformApns, "synthetic-apns-before")
		setToken(fcmClientId, sidecar.PushTokenPlatformFcm, "synthetic-fcm-before")
		setToken(tombstoneClientId, sidecar.PushTokenPlatformFcm, "synthetic-fcm-gone")
		setToken(tombstoneClientId, "", "")
		clientIds := []server.Id{apnsClientId, fcmClientId, tombstoneClientId}

		fixture.TakeMainDump()
		fixture.TakeSidecarDump()
		dumped := testPushTokenRows(t, ctx, clientIds)
		// after both dumps: a change the lost primary applied
		setToken(fcmClientId, sidecar.PushTokenPlatformFcm, "synthetic-fcm-after")
		lost := testPushTokenRows(t, ctx, []server.Id{fcmClientId})[fcmClientId]

		incident := fixture.LosePrimary(server.NewId())
		frame := fs42Frame(blobs, networkLog)
		connect.AssertEqual(t, fixture.RunFrameThrough(ctx, frame, StepApi, nil, &bytes.Buffer{}, nil), nil)
		restored := testPushTokenRows(t, ctx, clientIds)
		testRaisedOnce(t, dumped, restored)
		connect.AssertEqual(t, lost.Rev < restored[fcmClientId].Rev, true)
		storage, present, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, present, true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindPushTokensRaised, incident.IncidentId()), pushTokenRaiseDetail(storage, int64(len(clientIds))))

		connect.AssertEqual(t, frame.RunStep(ctx, incident, StepApi, &bytes.Buffer{}, ""), nil)
		connect.AssertEqual(t, testPushTokenRows(t, ctx, clientIds), restored)
	})
}

// A database before MG8 on T3 (OPS-M8-01): the primary holds no sidecar_push_token when both dumps are taken, so the
// restored database holds none, and the api step ends with the raise skipped and listed.
func TestFs42PushTokenRaiseSkippedBeforeMg8(t *testing.T) {
	requireFs42T3(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		fixture := NewPostgresLossFixture(t, nil)
		defer fixture.Close()
		blobs := testFakeBlobs(t)
		first, err := blobs.FirstCluster()
		if err != nil {
			t.Fatal(err)
		}
		networkId := server.NewId()
		defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q]\n", networkId))()
		network := newFs42Network(t, ctx, networkId, "before-mg8")
		networkLog := newTestLogWorld(t, ctx, first, network.networkId, network.userId)
		_, err = model.AdvanceNetworkAclHead(ctx, network.networkId, 1, networkLog.chainAt(1), false)
		connect.AssertEqual(t, err, nil)
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `DROP TABLE sidecar_push_token`))
		})

		fixture.TakeMainDump()
		fixture.TakeSidecarDump()
		incident := fixture.LosePrimary(server.NewId())
		_, present, err := pushTokenStorage(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, present, false)
		connect.AssertEqual(t, fixture.RunFrameThrough(ctx, fs42Frame(blobs, networkLog), StepApi, nil, &bytes.Buffer{}, nil), nil)
		connect.AssertEqual(t, incident.StepEnded(StepApi), true)
		connect.AssertEqual(t, testListDetail(t, incident, ListKindPushTokensSkipped, incident.IncidentId()), "no table: the database predates MG8")
		connect.AssertEqual(t, len(incident.RowsOfKind(ListKindPushTokensRaised)), 0)
	})
}

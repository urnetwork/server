//go:build sidecar_pending

package sidecarrestore

// FS-42 at M3 on T3 (D-OPS-14, D-OPS-18, FI-12; OPS-M3-04): the primary lost, both dumps restored through xops's real
// scripts on the private server, in each order of their snapshot times, and the restore frame run through the api
// step, every step twice, with FI-12's clauses checked after each step:
//
//	main older   a network, member and sidecar newer than the main dump come back with their ids, and a client
//	             deactivated between the dumps stays so
//	main newer   a network deleted between the dumps stays deleted with its sidecar rows, a client deactivated between
//	             them stays so, and a client created after the sidecar dump is deactivated to re-enroll and listed
//	either way   a network newer than both is lost and its log listed as orphaned with its repos, and a revocation
//	             filed after both dumps is honoured once api replays proj/: the head stands at the log's tip and the
//	             revocation's effects are applied
//
// It needs Docker, gpg, python3 and xops's sibling checkout, and runs only where SIDECAR_RESTORE_T3=1 says the
// machine is T3 (server/local/sidecar-restore-t3/run.sh), nightly and at each Gn-out.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// Skips unless this machine is T3 for the restore drill.
func requireFs42T3(t *testing.T) {
	t.Helper()
	if os.Getenv("SIDECAR_RESTORE_T3") != "1" {
		t.Skip("FS-42 runs on T3 through server/local/sidecar-restore-t3/run.sh, which sets SIDECAR_RESTORE_T3=1")
	}
}

// A network as api creates one, with its admin's user, and a sidecar's client of the admin's, strict_exp, minted as api
// mints one, which writes its owner row: what the sidecar dump names.
type fs42Network struct {
	networkId server.Id
	userId    server.Id
	clientId  server.Id
	session   *session.ClientSession
}

// The network networkId, named for label, with one client. The network must be in preview_networks, which a
// strict_exp mint needs before stage 2.
func newFs42Network(t testing.TB, ctx context.Context, networkId server.Id, label string) *fs42Network {
	t.Helper()
	self := &fs42Network{networkId: networkId, userId: server.NewId()}
	name := fmt.Sprintf("fs42-%s-%s", label, strings.ToLower(self.networkId.String()[:8]))
	model.Testing_CreateNetwork(ctx, self.networkId, name, self.userId)
	self.session = session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: self.networkId, UserId: self.userId, NetworkName: name})
	self.clientId = self.mint(t)
	return self
}

// Another client of the network's admin.
func (self *fs42Network) mint(t testing.TB) server.Id {
	t.Helper()
	result, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic", StrictExp: true}, self.session)
	if err != nil || result.Error != nil || result.ByClientJwt == nil {
		t.Fatalf("client: %v %+v", err, result)
	}
	byJwt, err := session.ParseByJwt(self.session.Ctx, *result.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	return *byJwt.ClientId
}

// Deactivates one of the network's clients as the admin's remove does.
func (self *fs42Network) remove(t testing.TB, clientId server.Id) {
	t.Helper()
	result, err := model.RemoveNetworkClient(&model.RemoveNetworkClientArgs{ClientId: clientId}, self.session)
	if err != nil || result == nil || result.Error != nil {
		t.Fatalf("remove client: %v %+v", err, result)
	}
}

// What FS-42's checks read from the env's database, which is the restored one once the primary is lost.
type fs42Rows struct {
	t   testing.TB
	ctx context.Context
}

// Whether the network's row is there.
func (self *fs42Rows) network(networkId server.Id) bool {
	return self.count(`SELECT count(*) FROM network WHERE network_id = $1`, networkId) == 1
}

// Whether the user's row is there.
func (self *fs42Rows) user(userId server.Id) bool {
	return self.count(`SELECT count(*) FROM network_user WHERE user_id = $1`, userId) == 1
}

// Whether the client's row is there, and whether it is active.
func (self *fs42Rows) client(clientId server.Id) (found bool, active bool) {
	server.Db(self.ctx, func(conn server.PgConn) {
		result, err := conn.Query(self.ctx, `SELECT active FROM network_client WHERE client_id = $1`, clientId)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				found = true
				server.Raise(result.Scan(&active))
			}
		})
	})
	return
}

// How many sidecar owner rows name the client.
func (self *fs42Rows) clientOwners(clientId server.Id) int {
	return self.count(`SELECT count(*) FROM network_client_owner WHERE client_id = $1`, clientId)
}

func (self *fs42Rows) count(sql string, args ...any) (count int) {
	server.Db(self.ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(self.ctx, sql, args...).Scan(&count))
	})
	return
}

// The frame FS-42 runs at M3: the ops and api steps over the test's first cluster, and every other step ended by hand.
func fs42Frame(blobs *sidecartest.FakeBlobs, world *testLogWorld) *Frame {
	frame := NewFrame()
	first, _ := blobs.FirstCluster()
	frame.Register(StepOps, func(ctx context.Context, run *StepRun) error {
		settings := model.DefaultConfigLogSettings()
		settings.Clock = world.world.Clock()
		return RunOpsStep(ctx, &OrphanLogDeps{
			Blobs:     blobs,
			Sequencer: model.NewConfigLogSequencer(first, settings, nil),
			Tasks:     sidecartest.NewFakeTasks(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil),
			Now:       server.NowUtc,
		}, run)
	})
	frame.Register(StepApi, func(ctx context.Context, run *StepRun) error {
		return RunApiStep(ctx, testApiStepDeps(first, world.world.Clock(), true, server.NowUtc()), run)
	})
	return frame
}

// FS-42's M3 schedule with the main dump taken first, so older, when mainFirst, and second otherwise.
func runFs42Schedule(t testing.TB, mainFirst bool) {
	ctx := t.Context()
	fixture := NewPostgresLossFixture(t, nil)
	defer fixture.Close()
	blobs := testFakeBlobs(t)
	first, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}

	keptId, deletedId, betweenId, lostId := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	defer server.Testing_PushSidecarConfig(fmt.Sprintf("sidecar:\n  preview_networks: [%q, %q, %q, %q]\n", keptId, deletedId, betweenId, lostId))()

	// before both dumps: a network with a sidecar's log and its head at base 1, and one deleted between the dumps
	kept := newFs42Network(t, ctx, keptId, "kept")
	keptLog := newTestLogWorld(t, ctx, first, kept.networkId, kept.userId)
	_, err = model.AdvanceNetworkAclHead(ctx, kept.networkId, 1, keptLog.chainAt(1), false)
	connect.AssertEqual(t, err, nil)
	removedDevice := keptLog.admit()
	deleted := newFs42Network(t, ctx, deletedId, "deleted")

	firstDump, secondDump := fixture.TakeSidecarDump, fixture.TakeMainDump
	if mainFirst {
		firstDump, secondDump = fixture.TakeMainDump, fixture.TakeSidecarDump
	}
	firstDump()

	// between the dumps
	between := newFs42Network(t, ctx, betweenId, "between")
	kept.remove(t, kept.clientId)
	if !mainFirst {
		connect.AssertEqual(t, func() bool {
			removed, _ := model.RemoveNetwork(ctx, deleted.networkId, &deleted.userId)
			return removed
		}(), true)
	}
	secondDump()

	// after both dumps: a network the restore loses, with a log, and a revocation in the kept network's log
	lost := newFs42Network(t, ctx, lostId, "lost")
	lostLog := newTestLogWorld(t, ctx, first, lost.networkId, lost.userId)
	lostLog.admit()
	keptLog.revoke(removedDevice)

	incident := fixture.LosePrimary(server.NewId())
	rows := &fs42Rows{t: t, ctx: ctx}
	out := &bytes.Buffer{}
	checked := []string{}
	err = fixture.RunFrameThrough(ctx, fs42Frame(blobs, keptLog), StepApi, nil, out, func(step string) {
		checked = append(checked, step)
		// FI-12 after every step: what both dumps decided stands
		connect.AssertEqual(t, rows.network(kept.networkId), true)
		found, active := rows.client(kept.clientId)
		connect.AssertEqual(t, found, true)
		connect.AssertEqual(t, active, false)
		connect.AssertEqual(t, rows.network(lost.networkId), false)
		connect.AssertEqual(t, rows.user(lost.userId), false)
		connect.AssertEqual(t, rows.network(between.networkId), true)
		connect.AssertEqual(t, rows.user(between.userId), true)
		found, active = rows.client(between.clientId)
		connect.AssertEqual(t, found, true)
		if mainFirst {
			// the sidecar dump is newer: the network, member and client newer than the main dump keep their ids
			connect.AssertEqual(t, active, true)
			connect.AssertEqual(t, rows.clientOwners(between.clientId), 1)
			connect.AssertEqual(t, rows.network(deleted.networkId), true)
		} else {
			// the main dump is newer: a client created after the sidecar dump re-enrolls, and a deletion stands
			connect.AssertEqual(t, active, false)
			connect.AssertEqual(t, testListDetail(t, incident, "client-deactivated", between.clientId) != "", true)
			connect.AssertEqual(t, rows.network(deleted.networkId), false)
			connect.AssertEqual(t, rows.clientOwners(deleted.clientId), 0)
		}
		// the lost network's log is the deleted-network path's, listed once the ops step ran
		connect.AssertEqual(t, testListDetail(t, incident, ListKindOrphanLog, lost.networkId) != "", true)
	})
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, checked, []string{StepOps, StepAbuse, StepDns, StepApi})

	// once api replayed proj/: the head at the log's tip, the revocation's effects applied
	head, err := model.GetNetworkAclHead(ctx, kept.networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, head.Version, keptLog.tip.GetVersion())
	revoked := false
	for _, event := range model.GetNetworkAdminEvents(ctx, kept.networkId, 0, 1000) {
		revoked = revoked || strings.HasSuffix(event.Action, "sidecar_revoke")
	}
	connect.AssertEqual(t, revoked, true)
	connect.AssertEqual(t, incident.StepEnded(StepApi), true)
	sidecarTime, mainTime, err := incident.SnapshotTimes()
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, mainTime.Before(sidecarTime), mainFirst)
}

func TestFs42MainDumpOlder(t *testing.T) {
	requireFs42T3(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		runFs42Schedule(t, true)
	})
}

func TestFs42MainDumpNewer(t *testing.T) {
	requireFs42T3(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		runFs42Schedule(t, false)
	})
}

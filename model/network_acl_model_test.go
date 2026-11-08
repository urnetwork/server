//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §8.17's head, §8.9's acks and D-ACL-3's subject column, which need MG2.

// A chain digest for a position, fixed by its inputs.
func aclTestChain(networkId server.Id, version uint64, fork byte) []byte {
	chain := sha256.Sum256(append(append(networkId.Bytes(), byte(version)), fork))
	return chain[:]
}

// The head only rises: a lower or equal position changes nothing, the head's
// own position with another digest is a fork, refused and counted, and the
// document flag moves only with a rise.
func TestAdvanceNetworkAclHeadOnlyRaisesIt(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		if head, err := GetNetworkAclHead(ctx, networkId); head != nil || err != nil {
			t.Fatalf("a head before any: %+v %v", head, err)
		}
		advance := func(version uint64, fork byte, hasDocument bool) (bool, error) {
			return AdvanceNetworkAclHead(ctx, networkId, version, aclTestChain(networkId, version, fork), hasDocument)
		}
		if advanced, err := advance(3, 0, false); !advanced || err != nil {
			t.Fatalf("first head: %t %v", advanced, err)
		}
		if advanced, err := advance(3, 0, true); advanced || err != nil {
			t.Fatalf("the same position again: %t %v", advanced, err)
		}
		if advanced, err := advance(2, 0, true); advanced || err != nil {
			t.Fatalf("a lower position: %t %v", advanced, err)
		}
		forksBefore := testutil.ToFloat64(networkAclHeadForkCounter)
		if advanced, err := advance(3, 1, false); advanced || !errors.Is(err, ErrNetworkAclHeadFork) {
			t.Fatalf("a second digest at the head: %t %v", advanced, err)
		}
		if testutil.ToFloat64(networkAclHeadForkCounter) != forksBefore+1 {
			t.Fatal("the fork was not counted")
		}
		head, err := GetNetworkAclHead(ctx, networkId)
		if err != nil || head.Version != 3 || !bytes.Equal(head.Chain, aclTestChain(networkId, 3, 0)) || head.HasDocument {
			t.Fatalf("head after refusals: %+v %v", head, err)
		}
		if advanced, err := advance(9, 0, true); !advanced || err != nil {
			t.Fatalf("a higher position: %t %v", advanced, err)
		}
		head, _ = GetNetworkAclHead(ctx, networkId)
		if head.Version != 9 || !head.HasDocument {
			t.Fatalf("head after the rise: %+v", head)
		}
		for _, invalid := range []struct {
			version uint64
			chain   []byte
		}{
			{version: 0, chain: aclTestChain(networkId, 10, 0)},
			{version: 1 << 63, chain: aclTestChain(networkId, 10, 0)},
			{version: 10, chain: []byte{1, 2, 3}},
		} {
			if _, err := AdvanceNetworkAclHead(ctx, networkId, invalid.version, invalid.chain, false); !errors.Is(err, ErrNetworkAclHeadInvalid) {
				t.Errorf("version %d, %d-byte chain: %v", invalid.version, len(invalid.chain), err)
			}
		}
	})
}

// The snapshot holds every network with a head and no other.
func TestGetNetworkAclHeadsHoldsEveryHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		first := server.NewId()
		second := server.NewId()
		without := server.NewId()
		for version, networkId := range []server.Id{first, second} {
			if _, err := AdvanceNetworkAclHead(ctx, networkId, uint64(version+1), aclTestChain(networkId, uint64(version+1), 0), version == 1); err != nil {
				t.Fatal(err)
			}
		}
		heads, err := GetNetworkAclHeads(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if heads[first].GetVersion() != 1 || heads[second].GetVersion() != 2 || !heads[second].HasDocument || heads[without] != nil {
			t.Fatalf("heads %+v", heads)
		}
	})
}

// The version, or 0 for no head.
func (self *NetworkAclHead) GetVersion() uint64 {
	if self == nil {
		return 0
	}
	return self.Version
}

// The head goes with its network.
func TestNetworkAclHeadGoesWithItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "acl-head-"+networkId.String(), server.NewId())
		if _, err := AdvanceNetworkAclHead(ctx, networkId, 1, aclTestChain(networkId, 1, 0), false); err != nil {
			t.Fatal(err)
		}
		server.Tx(ctx, func(tx server.PgTx) {
			removeNetworkAclHeadInTx(ctx, tx, networkId)
		})
		if head, _ := GetNetworkAclHead(ctx, networkId); head != nil {
			t.Fatalf("head kept: %+v", head)
		}
	})
}

// An ack records the newest version per client, at or below its network's head,
// and only a recorded ack is observed; a network with no head records nothing.
func TestAckNetworkAclKeepsTheNewestVersionUnderTheHead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		clientId := server.NewId()
		now := server.NowUtc()
		entryTimeMs := now.Add(-2 * time.Second).UnixMilli()
		ack := func(version uint64) bool {
			recorded, err := AckNetworkAcl(ctx, networkId, clientId, version, entryTimeMs, now)
			if err != nil {
				t.Fatal(err)
			}
			return recorded
		}
		if ack(1) {
			t.Fatal("recorded with no head")
		}
		if _, err := AdvanceNetworkAclHead(ctx, networkId, 5, aclTestChain(networkId, 5, 0), false); err != nil {
			t.Fatal(err)
		}
		sumBefore := aclTestHistogramCount(t)
		cases := []struct {
			version  uint64
			recorded bool
			want     uint64
		}{
			{version: 3, recorded: true, want: 3},
			{version: 2, recorded: false, want: 3},
			{version: 3, recorded: false, want: 3},
			{version: 6, recorded: false, want: 3},
			{version: 5, recorded: true, want: 5},
		}
		for i, c := range cases {
			if recorded := ack(c.version); recorded != c.recorded {
				t.Errorf("case %d: recorded %t", i, recorded)
			}
			if version, found := GetNetworkAclAck(ctx, clientId); !found || version != c.want {
				t.Errorf("case %d: version %d %t", i, version, found)
			}
		}
		if aclTestHistogramCount(t) != sumBefore+2 {
			t.Errorf("observed %d more delays, want 2", aclTestHistogramCount(t)-sumBefore)
		}
		if _, err := AckNetworkAcl(ctx, networkId, clientId, 0, 0, now); !errors.Is(err, ErrNetworkAclHeadInvalid) {
			t.Errorf("version 0: %v", err)
		}
	})
}

// The number of delays the histogram has observed.
func aclTestHistogramCount(t testing.TB) uint64 {
	t.Helper()
	metric := &dto.Metric{}
	if err := networkAclAckDelayHistogram.Write(metric); err != nil {
		t.Fatal(err)
	}
	return metric.GetHistogram().GetSampleCount()
}

// The sweep removes acks past their 7 days and keeps the rest.
func TestRemoveOldNetworkAclAcksAt7Days(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		if _, err := AdvanceNetworkAclHead(ctx, networkId, 9, aclTestChain(networkId, 9, 0), false); err != nil {
			t.Fatal(err)
		}
		retention := DefaultNetworkAclAckSettings().Retention
		if retention != 7*24*time.Hour {
			t.Fatalf("retention = %s", retention)
		}
		now := server.NowUtc()
		cutoff := now.Add(-retention)
		old := server.NewId()
		kept := server.NewId()
		for clientId, ackTime := range map[server.Id]time.Time{old: cutoff.Add(-time.Second), kept: cutoff.Add(time.Second)} {
			if recorded, err := AckNetworkAcl(ctx, networkId, clientId, 1, 0, ackTime); !recorded || err != nil {
				t.Fatalf("ack: %t %v", recorded, err)
			}
		}
		if removed := RemoveOldNetworkAclAcks(ctx, cutoff); removed != 1 {
			t.Fatalf("removed %d", removed)
		}
		if _, found := GetNetworkAclAck(ctx, old); found {
			t.Error("the old ack was kept")
		}
		if _, found := GetNetworkAclAck(ctx, kept); !found {
			t.Error("the young ack was removed")
		}
	})
}

// POST /acl/ack's model: the bearer's own client, a valid version, and the
// answer says whether it counted.
func TestAclAckTakesTheBearersClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newClientOwnerFixture(t)
		client := fixture.mint(t, fixture.root, &AuthNetworkClientArgs{Description: "acl-ack"})
		clientSession := fixture.sessionOf(t, client)
		if _, err := AdvanceNetworkAclHead(fixture.ctx, fixture.networkId, 4, aclTestChain(fixture.networkId, 4, 0), true); err != nil {
			t.Fatal(err)
		}
		result, err := AclAck(&AclAckArgs{Version: 4, TimeMs: server.NowUtc().UnixMilli()}, clientSession)
		if err != nil || !result.Recorded {
			t.Fatalf("ack: %+v %v", result, err)
		}
		if version, _ := GetNetworkAclAck(fixture.ctx, *client.ClientId); version != 4 {
			t.Fatalf("version %d", version)
		}
		result, err = AclAck(&AclAckArgs{Version: 4}, clientSession)
		if err != nil || result.Recorded {
			t.Fatalf("repeat: %+v %v", result, err)
		}
		for _, invalid := range []*AclAckArgs{nil, {Version: 0}, {Version: 4, TimeMs: -1}} {
			if _, err := AclAck(invalid, clientSession); err == nil {
				t.Errorf("%+v accepted", invalid)
			}
		}
		if _, err := AclAck(&AclAckArgs{Version: 4}, fixture.root); err == nil {
			t.Error("a network token's ack accepted")
		}
	})
}

// A client's stored subject, NULL as nil.
func aclTestSubject(t testing.TB, ctx context.Context, clientId server.Id) (subject *server.Id) {
	t.Helper()
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(ctx, `SELECT acl_subject_client_id FROM network_client WHERE client_id = $1`, clientId).Scan(&subject))
	})
	return
}

// Each mint writes its subject in its own statement: a top-level client itself,
// a child its source's subject, a sidecar's own client (asking for strict_exp
// under a parent with none) itself, a sidecar's own child the sidecar, and under
// a parent minted before the column the meaning NULL already has (D-ACL-3).
func TestAuthNetworkClientWritesItsSubject(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		fixture := newClientOwnerFixture(t)
		fixture.pushPreview(t)
		ctx := fixture.ctx
		device := fixture.mint(t, fixture.root, &AuthNetworkClientArgs{Description: "device", AdminRefused: true})
		deviceSession := fixture.sessionOf(t, device)
		window := fixture.mint(t, deviceSession, &AuthNetworkClientArgs{SourceClientId: device.ClientId, Description: "window"})
		windowSession := fixture.sessionOf(t, window)
		windowChild := fixture.mint(t, windowSession, &AuthNetworkClientArgs{SourceClientId: window.ClientId, Description: "window child"})
		sidecar := fixture.mint(t, deviceSession, &AuthNetworkClientArgs{
			SourceClientId: device.ClientId,
			Description:    "sidecar",
			AdminRefused:   true,
			StrictExp:      true,
		})
		sidecarSession := fixture.sessionOf(t, sidecar)
		sidecarChild := fixture.mint(t, sidecarSession, &AuthNetworkClientArgs{
			SourceClientId: sidecar.ClientId,
			Description:    "sidecar child",
			AdminRefused:   true,
			StrictExp:      true,
		})
		cases := []struct {
			name     string
			clientId server.Id
			subject  server.Id
		}{
			{name: "top-level", clientId: *device.ClientId, subject: *device.ClientId},
			{name: "window child", clientId: *window.ClientId, subject: *device.ClientId},
			{name: "window's child", clientId: *windowChild.ClientId, subject: *device.ClientId},
			{name: "sidecar child of its device", clientId: *sidecar.ClientId, subject: *sidecar.ClientId},
			{name: "sidecar's own child", clientId: *sidecarChild.ClientId, subject: *sidecar.ClientId},
		}
		for _, c := range cases {
			if subject := aclTestSubject(t, ctx, c.clientId); subject == nil || *subject != c.subject {
				t.Errorf("%s: subject %v, want %s", c.name, subject, c.subject)
			}
		}

		// parents minted before the column: a top-level one is itself, a child
		// keeps NULL for the backfill
		legacyTop := server.NewId()
		legacyChild := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, active) VALUES ($1, $2, $3, true)`, legacyTop, fixture.networkId, server.NewId()))
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, source_client_id, active) VALUES ($1, $2, $3, $4, true)`, legacyChild, fixture.networkId, server.NewId(), legacyTop))
		})
		for parent, want := range map[server.Id]*server.Id{legacyTop: &legacyTop, legacyChild: nil} {
			childId := server.NewId()
			server.Tx(ctx, func(tx server.PgTx) {
				column, value := networkClientMintAclSubjectInsert(false, "$1", "$3")
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, source_client_id`+column+`) VALUES ($1, $2, $3`+value+`)`, childId, fixture.networkId, parent))
			})
			subject := aclTestSubject(t, ctx, childId)
			if (subject == nil) != (want == nil) || (want != nil && *subject != *want) {
				t.Errorf("child of %s: subject %v, want %v", parent, subject, want)
			}
		}
	})
}

// The other insert sites mint top-level clients, each its own subject.
func TestTopLevelInsertSitesWriteTheirSubject(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		userId := server.NewId()
		Testing_CreateNetwork(ctx, networkId, "acl-subject-"+networkId.String(), userId)

		deviceClientId := server.NewId()
		Testing_CreateDevice(ctx, networkId, server.NewId(), deviceClientId, "device", "spec")
		blockReportClientId := Testing_CreateBlockReportClient(ctx, networkId)
		prober := shardTestOwner(t, ctx, shardTestKey(1))

		clientSession := session.Testing_CreateClientSession(ctx, session.NewByJwt(networkId, userId, "acl-subject-"+networkId.String(), false, false))
		noAuth := session.Testing_CreateClientSession(ctx, nil)
		code, err := DeviceCreateAdoptCode(&DeviceCreateAdoptCodeArgs{DeviceName: "adopted", DeviceSpec: "spec"}, noAuth)
		if err != nil || code.Error != nil {
			t.Fatalf("adopt code: %+v %v", code, err)
		}
		if result, err := DeviceAdd(&DeviceAddArgs{Code: code.AdoptCode}, clientSession); err != nil || result.Error != nil {
			t.Fatalf("device add: %+v %v", result, err)
		}
		status, err := DeviceAdoptStatus(&DeviceAdoptStatusArgs{AdoptCode: code.AdoptCode}, noAuth)
		if err != nil || status.Error != nil {
			t.Fatalf("adopt status: %+v %v", status, err)
		}
		adopted, err := DeviceConfirmAdopt(&DeviceConfirmAdoptArgs{
			AdoptCode:             code.AdoptCode,
			AdoptSecret:           code.AdoptSecret,
			AssociatedNetworkName: status.AssociatedNetworkName,
		}, noAuth)
		if err != nil || adopted.Error != nil {
			t.Fatalf("confirm adopt: %+v %v", adopted, err)
		}
		adoptedJwt, err := session.ParseByJwt(ctx, adopted.ByClientJwt)
		if err != nil || adoptedJwt.ClientId == nil {
			t.Fatalf("adopted token: %v", err)
		}

		for name, clientId := range map[string]server.Id{
			"Testing_CreateDevice":            deviceClientId,
			"Testing_CreateBlockReportClient": blockReportClientId,
			"BeginProberShard":                prober.ClientId,
			"DeviceConfirmAdopt":              *adoptedJwt.ClientId,
		} {
			if subject := aclTestSubject(t, ctx, clientId); subject == nil || *subject != clientId {
				t.Errorf("%s: subject %v", name, subject)
			}
		}
	})
}

// The backfill fills what clients minted before the column hold: a top-level
// client itself, a sidecar's own client itself by its binding, a sidecar's child
// the sidecar, and a window client's child its device, a child waiting for a
// later pass while its source has none; a filled row is never written again.
func TestBackfillNetworkClientAclSubjects(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		device := server.NewId()
		window := server.NewId()
		sidecar := server.NewId()
		sidecarChild := server.NewId()
		windowChild := server.NewId()
		filled := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			insert := func(clientId server.Id, sourceClientId *server.Id) {
				server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, source_client_id, active) VALUES ($1, $2, $3, $4, true)`,
					clientId, networkId, server.NewId(), sourceClientId))
			}
			insert(device, nil)
			insert(window, &device)
			insert(sidecar, &device)
			insert(sidecarChild, &sidecar)
			insert(windowChild, &window)
			server.RaisePgResult(tx.Exec(ctx, `INSERT INTO network_client (client_id, network_id, device_id, acl_subject_client_id, active) VALUES ($1, $2, $3, $4, true)`,
				filled, networkId, server.NewId(), device))
			server.RaisePgResult(tx.Exec(ctx, `
				INSERT INTO sidecar_identity_binding (sidecar_id, generation, client_id, client_public_key, signatures, create_time)
				VALUES ($1, 1, $2, $3, $4, $5)
			`, server.NewId(), sidecar, make([]byte, 32), []byte{1}, server.NowUtc()))
		})
		passes := 0
		for {
			passes += 1
			after := server.Id{}
			left := int64(0)
			for {
				last, _, leftCount, err := BackfillNetworkClientAclSubjects(ctx, after, 2)
				if err != nil {
					t.Fatal(err)
				}
				left += leftCount
				if last == after {
					break
				}
				after = last
			}
			if left == 0 || 5 < passes {
				break
			}
		}
		want := map[server.Id]server.Id{device: device, window: device, sidecar: sidecar, sidecarChild: sidecar, windowChild: device, filled: device}
		for clientId, subject := range want {
			if got := aclTestSubject(t, ctx, clientId); got == nil || *got != subject {
				t.Errorf("%s: subject %v, want %s", clientId, got, subject)
			}
		}
		if _, _, _, err := BackfillNetworkClientAclSubjects(ctx, server.Id{}, 0); err == nil {
			t.Error("an empty batch accepted")
		}
	})
}

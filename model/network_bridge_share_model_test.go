//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §17.16's network_bridge_share, which needs MG6. The tables here are synthetic
// ciphertext: the server never seals or opens one, so a stand-in seal is enough
// to show that what it stores is what it was given.

// A network with an active sidecar, and that sidecar's session.
type bridgeShareTestSide struct {
	networkId     server.Id
	sidecarId     server.Id
	clientSession *session.ClientSession
}

// A sidecar bound in a network of its own.
func newBridgeShareTestSide(t testing.TB, ctx context.Context) *bridgeShareTestSide {
	t.Helper()
	networkId := server.NewId()
	sidecarId := server.NewId()
	record := testSidecarBindingRecord(t, networkId, sidecarId, testSidecarKey(t), 1)
	if outcome, _ := BindSidecarIdentity(ctx, record, server.NowUtc()); outcome != SidecarBindAppended {
		t.Fatalf("bind: %s", outcome)
	}
	clientId := record.ClientId
	return &bridgeShareTestSide{
		networkId: networkId,
		sidecarId: sidecarId,
		clientSession: session.Testing_CreateClientSession(ctx, &session.ByJwt{
			NetworkId: networkId,
			ClientId:  &clientId,
		}),
	}
}

// Seals plaintext under a fresh key, as a stand-in for §17.9's seal under K.
func bridgeShareTestSeal(t testing.TB, plaintext []byte) []byte {
	t.Helper()
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	return append(nonce, aead.Seal(nil, nonce, plaintext, nil)...)
}

// Lets every network share outbound, for the test.
func bridgeShareTestAllowOutbound(t testing.TB) {
	t.Helper()
	restore := Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
		return true, nil
	})
	t.Cleanup(restore)
}

// A share x files and y accepts, with tables at version 1.
func bridgeShareTestAccepted(t testing.TB, ctx context.Context, x *bridgeShareTestSide, y *bridgeShareTestSide, status NetworkBridgeShareAcceptStatus) (server.Id, []byte, []byte) {
	t.Helper()
	shareId := server.NewId()
	targetTable := bridgeShareTestSeal(t, []byte("targets of the share"))
	aliasTable := bridgeShareTestSeal(t, []byte("aliases of the share"))
	server.Tx(ctx, func(tx server.PgTx) {
		if err := CreateNetworkBridgeShareInTx(ctx, tx, shareId, x.networkId, targetTable, 1); err != nil {
			t.Fatalf("create: %v", err)
		}
	})
	server.Tx(ctx, func(tx server.PgTx) {
		if err := AcceptNetworkBridgeShareInTx(ctx, tx, shareId, y.networkId, aliasTable, 1, status); err != nil {
			t.Fatalf("accept: %v", err)
		}
	})
	return shareId, targetTable, aliasTable
}

// The leading status of a route error, or 0.
func bridgeShareTestStatus(err error) string {
	if err == nil {
		return "200"
	}
	status, _, _ := strings.Cut(err.Error(), " ")
	return status
}

// A sidecar of either network reads both tables, and a third network's sidecar
// reaches the share by neither route, as though it did not exist.
func TestNetworkBridgeShareServesBothSidesAndNoThird(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		z := newBridgeShareTestSide(t, ctx)
		shareId, targetTable, aliasTable := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		for _, side := range []*bridgeShareTestSide{x, y} {
			result, err := GetNetworkBridgeShareForSidecar(shareId, side.clientSession)
			if err != nil {
				t.Fatal(err)
			}
			if result.NetworkId != x.networkId || result.InvitedNetworkId == nil || *result.InvitedNetworkId != y.networkId ||
				!bytes.Equal(result.TargetTable, targetTable) || !bytes.Equal(result.AliasTable, aliasTable) ||
				result.TargetTableVersion != 1 || result.AliasTableVersion != 1 ||
				result.AcceptStatus != NetworkBridgeShareAcceptStatusActive {
				t.Fatalf("read %+v", result)
			}
		}
		if _, err := GetNetworkBridgeShareForSidecar(shareId, z.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Fatalf("a third network's read: %v", err)
		}
		put := &PutNetworkBridgeShareArgs{Table: bridgeShareTestSeal(t, []byte("a third's table")), Version: 9}
		if _, err := PutNetworkBridgeShareForSidecar(shareId, put, z.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Fatalf("a third network's write: %v", err)
		}
		if _, err := GetNetworkBridgeShareForSidecar(server.NewId(), x.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Fatalf("a share that does not exist: %v", err)
		}
		share, err := GetNetworkBridgeShare(ctx, shareId)
		if err != nil || share.TargetTableVersion != 1 || share.AliasTableVersion != 1 {
			t.Fatalf("the third's write changed the share: %+v %v", share, err)
		}
	})
}

// Each side replaces its own table at a higher version alone, a lower or equal
// one refused, and the other side's table untouched.
func TestNetworkBridgeSharePutReplacesOwnSideAtAHigherVersion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		shareId, _, aliasTable := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)

		newTargets := bridgeShareTestSeal(t, []byte("targets, second version"))
		result, err := PutNetworkBridgeShareForSidecar(shareId, &PutNetworkBridgeShareArgs{Table: newTargets, Version: 2}, x.clientSession)
		if err != nil || !bytes.Equal(result.TargetTable, newTargets) || result.TargetTableVersion != 2 {
			t.Fatalf("a higher version: %+v %v", result, err)
		}
		if !bytes.Equal(result.AliasTable, aliasTable) || result.AliasTableVersion != 1 {
			t.Fatal("the targets' write touched the aliases")
		}
		for _, version := range []int64{2, 1} {
			replay := &PutNetworkBridgeShareArgs{Table: bridgeShareTestSeal(t, []byte("an older table")), Version: version}
			if _, err := PutNetworkBridgeShareForSidecar(shareId, replay, x.clientSession); bridgeShareTestStatus(err) != "409" {
				t.Fatalf("version %d at version 2: %v", version, err)
			}
		}

		newAliases := bridgeShareTestSeal(t, []byte("aliases, third version"))
		put := &PutNetworkBridgeShareArgs{Table: newAliases, Version: 3, AcceptStatus: NetworkBridgeShareAcceptStatusRefusedNoSpace}
		result, err = PutNetworkBridgeShareForSidecar(shareId, put, y.clientSession)
		if err != nil || !bytes.Equal(result.AliasTable, newAliases) || result.AliasTableVersion != 3 ||
			result.AcceptStatus != NetworkBridgeShareAcceptStatusRefusedNoSpace {
			t.Fatalf("the aliases at version 3: %+v %v", result, err)
		}
		if !bytes.Equal(result.TargetTable, newTargets) || result.TargetTableVersion != 2 {
			t.Fatal("the aliases' write touched the targets")
		}
		// a write with no status keeps the one stored
		put = &PutNetworkBridgeShareArgs{Table: aliasTable, Version: 4}
		result, err = PutNetworkBridgeShareForSidecar(shareId, put, y.clientSession)
		if err != nil || result.AcceptStatus != NetworkBridgeShareAcceptStatusRefusedNoSpace {
			t.Fatalf("a write with no status: %+v %v", result, err)
		}
	})
}

// Each accept status round-trips, and the share's refusals hold: a second
// accept, a network accepting its own share, an empty or oversized table, a
// version below 1, an unknown status, and a status from the sharing side.
func TestNetworkBridgeShareStatusesAndRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		for _, status := range []NetworkBridgeShareAcceptStatus{
			NetworkBridgeShareAcceptStatusActive,
			NetworkBridgeShareAcceptStatusRefusedTooLarge,
			NetworkBridgeShareAcceptStatusRefusedNoSpace,
		} {
			y := newBridgeShareTestSide(t, ctx)
			shareId, _, _ := bridgeShareTestAccepted(t, ctx, x, y, status)
			share, err := GetNetworkBridgeShare(ctx, shareId)
			if err != nil || share.AcceptStatus != status {
				t.Fatalf("status %s came back %+v %v", status, share, err)
			}
		}

		y := newBridgeShareTestSide(t, ctx)
		shareId, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		table := bridgeShareTestSeal(t, []byte("a table"))
		server.Tx(ctx, func(tx server.PgTx) {
			if err := AcceptNetworkBridgeShareInTx(ctx, tx, shareId, newBridgeShareTestSide(t, ctx).networkId, table, 1, NetworkBridgeShareAcceptStatusActive); !errors.Is(err, ErrNetworkBridgeShareInvalid) {
				t.Errorf("a second accept: %v", err)
			}
		})
		ownShareId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			if err := CreateNetworkBridgeShareInTx(ctx, tx, ownShareId, x.networkId, table, 1); err != nil {
				t.Fatal(err)
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			if err := AcceptNetworkBridgeShareInTx(ctx, tx, ownShareId, x.networkId, table, 1, NetworkBridgeShareAcceptStatusActive); !errors.Is(err, ErrNetworkBridgeShareInvalid) {
				t.Errorf("a network accepting its own share: %v", err)
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			if err := CreateNetworkBridgeShareInTx(ctx, tx, ownShareId, x.networkId, table, 1); !errors.Is(err, ErrNetworkBridgeShareInvalid) {
				t.Errorf("a share id filed twice: %v", err)
			}
		})
		oversized := make([]byte, DefaultNetworkBridgeShareSettings().TableMaxByteCount+1)
		cases := []struct {
			name string
			put  *PutNetworkBridgeShareArgs
			side *bridgeShareTestSide
		}{
			{name: "an empty table", put: &PutNetworkBridgeShareArgs{Version: 5}, side: x},
			{name: "an oversized table", put: &PutNetworkBridgeShareArgs{Table: oversized, Version: 5}, side: x},
			{name: "version 0", put: &PutNetworkBridgeShareArgs{Table: table, Version: 0}, side: y},
			{name: "an unknown status", put: &PutNetworkBridgeShareArgs{Table: table, Version: 5, AcceptStatus: "accepted"}, side: y},
			{name: "a status from the sharing side", put: &PutNetworkBridgeShareArgs{Table: table, Version: 5, AcceptStatus: NetworkBridgeShareAcceptStatusActive}, side: x},
		}
		for _, c := range cases {
			if _, err := PutNetworkBridgeShareForSidecar(shareId, c.put, c.side.clientSession); bridgeShareTestStatus(err) != "400" {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	})
}

// §21.7 (C-20): with no standing check installed, or one that refuses, the
// sharing network can neither file nor replace its targets, while the invited
// side, which shares nothing outbound, still replaces its aliases.
func TestNetworkBridgeShareOutboundNeedsStanding(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		restore := Testing_SetNetworkBridgeShareOutboundCheck(nil)
		t.Cleanup(restore)
		table := bridgeShareTestSeal(t, []byte("targets"))
		server.Tx(ctx, func(tx server.PgTx) {
			if err := CreateNetworkBridgeShareInTx(ctx, tx, server.NewId(), x.networkId, table, 1); !errors.Is(err, ErrNetworkBridgeShareOutboundRefused) {
				t.Fatalf("a filing with no standing check: %v", err)
			}
		})
		Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return true, nil
		})
		shareId, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return networkId != x.networkId, nil
		})
		put := &PutNetworkBridgeShareArgs{Table: table, Version: 2}
		if _, err := PutNetworkBridgeShareForSidecar(shareId, put, x.clientSession); bridgeShareTestStatus(err) != "403" {
			t.Fatalf("the sharing side on probation: %v", err)
		}
		if _, err := PutNetworkBridgeShareForSidecar(shareId, put, y.clientSession); err != nil {
			t.Fatalf("the invited side: %v", err)
		}
		Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return false, errors.New("standing unreadable")
		})
		put = &PutNetworkBridgeShareArgs{Table: table, Version: 3}
		if _, err := PutNetworkBridgeShareForSidecar(shareId, put, x.clientSession); bridgeShareTestStatus(err) != "403" {
			t.Fatalf("the sharing side with its standing unreadable: %v", err)
		}
	})
}

// The routes are a sidecar's: a client of the network bound to no sidecar, and a
// network credential, are refused.
func TestNetworkBridgeShareRoutesNeedASidecar(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		shareId, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		appClientId := server.NewId()
		callers := []*session.ClientSession{
			session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: x.networkId, ClientId: &appClientId}),
			session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: x.networkId}),
		}
		for i, caller := range callers {
			if _, err := GetNetworkBridgeShareForSidecar(shareId, caller); bridgeShareTestStatus(err) != "403" {
				t.Errorf("caller %d read: %v", i, err)
			}
			put := &PutNetworkBridgeShareArgs{Table: bridgeShareTestSeal(t, []byte("t")), Version: 2}
			if _, err := PutNetworkBridgeShareForSidecar(shareId, put, caller); bridgeShareTestStatus(err) != "403" {
				t.Errorf("caller %d write: %v", i, err)
			}
		}
	})
}

// A revocation removes the row, and so does the deletion of either network.
func TestNetworkBridgeShareGoesWithItsRevocationAndItsNetworks(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		revoked, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		if !RemoveNetworkBridgeShare(ctx, revoked) || RemoveNetworkBridgeShare(ctx, revoked) {
			t.Fatal("a revocation removed other than one row")
		}
		if _, err := GetNetworkBridgeShare(ctx, revoked); !errors.Is(err, ErrNetworkBridgeShareNotFound) {
			t.Fatalf("a revoked share read %v", err)
		}
		bySharing, _, _ := bridgeShareTestAccepted(t, ctx, x, y, NetworkBridgeShareAcceptStatusActive)
		byInvited, _, _ := bridgeShareTestAccepted(t, ctx, y, x, NetworkBridgeShareAcceptStatusActive)
		other := newBridgeShareTestSide(t, ctx)
		kept, _, _ := bridgeShareTestAccepted(t, ctx, other, newBridgeShareTestSide(t, ctx), NetworkBridgeShareAcceptStatusActive)
		server.Tx(ctx, func(tx server.PgTx) {
			removeNetworkBridgeSharesInTx(ctx, tx, x.networkId)
		})
		for _, shareId := range []server.Id{bySharing, byInvited} {
			if _, err := GetNetworkBridgeShare(ctx, shareId); !errors.Is(err, ErrNetworkBridgeShareNotFound) {
				t.Fatalf("a share of the deleted network read %v", err)
			}
		}
		if _, err := GetNetworkBridgeShare(ctx, kept); err != nil {
			t.Fatalf("another network's share: %v", err)
		}
	})
}

// I-2: the row holds ids, versions, the status and the bytes it was given, so a
// canary sealed into a table shows nowhere in it, and every column is one api-9
// names.
func TestNetworkBridgeShareRowHoldsOnlyCiphertext(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		canaries := []string{"192.0.2.0/24", "198.51.100.7:8443", "canary-target-host-key"}
		shareId := server.NewId()
		server.Tx(ctx, func(tx server.PgTx) {
			targetTable := bridgeShareTestSeal(t, []byte(strings.Join(canaries, " ")))
			if err := CreateNetworkBridgeShareInTx(ctx, tx, shareId, x.networkId, targetTable, 1); err != nil {
				t.Fatal(err)
			}
		})
		server.Tx(ctx, func(tx server.PgTx) {
			aliasTable := bridgeShareTestSeal(t, []byte("100.80.200.0/24 canary-alias"))
			if err := AcceptNetworkBridgeShareInTx(ctx, tx, shareId, y.networkId, aliasTable, 1, NetworkBridgeShareAcceptStatusActive); err != nil {
				t.Fatal(err)
			}
		})
		var row string
		columns := []string{}
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT row_to_json(network_bridge_share)::text FROM network_bridge_share WHERE share_id = $1`, shareId).Scan(&row))
			result, err := conn.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'network_bridge_share' ORDER BY ordinal_position`)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var column string
					server.Raise(result.Scan(&column))
					columns = append(columns, column)
				}
			})
		})
		for _, canary := range append(canaries, "100.80.200.0/24", "canary-alias") {
			if strings.Contains(row, canary) {
				t.Errorf("the row shows %q", canary)
			}
		}
		want := []string{"share_id", "network_id", "invited_network_id", "target_table", "target_table_version", "alias_table", "alias_table_version", "accept_status"}
		if strings.Join(columns, ",") != strings.Join(want, ",") {
			t.Fatalf("columns %v, api-9's %v", columns, want)
		}
	})
}

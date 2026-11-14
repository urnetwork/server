//go:build sidecar_pending

package model

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/session"
)

// §8.11's invites (ACL-M6-03), which need MG6: the code hashed and never stored
// in the clear, uses counted, a window past 7 days refused, the row gone at its
// last use and at its window's close, a claim and its completion, the tier and
// sidecar checks, and the row going with its network. Run through
// .stack/server-test.sh. The target and alias tables are bridge_share's
// stand-in seals: api never opens one.

// A fresh 256-bit code and its hash.
func aclShareTestCode(t testing.TB) ([]byte, []byte) {
	t.Helper()
	code := make([]byte, 32)
	if _, err := rand.Read(code); err != nil {
		t.Fatal(err)
	}
	codeHash := sha256.Sum256(code)
	return code, codeHash[:]
}

// x's invite of useCount uses through the route, its code and the answer.
func aclShareTestInvite(t testing.TB, x *bridgeShareTestSide, useCount int, windowSeconds int64) ([]byte, []byte, *AclInviteResult, []byte) {
	t.Helper()
	code, codeHash := aclShareTestCode(t)
	targetTable := bridgeShareTestSeal(t, []byte("targets of the invite"))
	result, err := AclInvite(&AclInviteArgs{
		CodeHash:           codeHash,
		UseCount:           useCount,
		WindowSeconds:      windowSeconds,
		TargetTable:        targetTable,
		TargetTableVersion: 1,
	}, x.clientSession)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	return code, codeHash, result, targetTable
}

// y's claim through the route.
func aclShareTestClaim(y *bridgeShareTestSide, codeHash []byte) (*NetworkBridgeShareResult, error) {
	return AclAcceptInvite(&AclAcceptInviteArgs{CodeHash: codeHash}, y.clientSession)
}

// Every byte string any row of network_acl_share or network_bridge_share holds.
func aclShareTestStoredBytes(t testing.TB, ctx context.Context) [][]byte {
	t.Helper()
	stored := [][]byte{}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(ctx, `SELECT code_hash, network_id::text, share_id::text FROM network_acl_share`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var codeHash []byte
				var networkId, shareId string
				server.Raise(result.Scan(&codeHash, &networkId, &shareId))
				stored = append(stored, codeHash, []byte(networkId), []byte(shareId))
			}
		})
		result, err = conn.Query(ctx, `SELECT target_table, COALESCE(alias_table, ''::bytea) FROM network_bridge_share`)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var targetTable, aliasTable []byte
				server.Raise(result.Scan(&targetTable, &aliasTable))
				stored = append(stored, targetTable, aliasTable)
			}
		})
	})
	return stored
}

// The code's hash is what api keeps, and the code itself is in no row.
func TestNetworkAclInviteKeepsOnlyTheCodesHash(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		code, codeHash, result, targetTable := aclShareTestInvite(t, x, 1, 0)

		invite := GetNetworkAclInvite(ctx, codeHash)
		if invite == nil || invite.NetworkId != x.networkId || invite.ShareId != result.ShareId || invite.UseCount != 1 {
			t.Fatalf("the invite's row: %+v", invite)
		}
		share, err := GetNetworkBridgeShare(ctx, result.ShareId)
		if err != nil || !bytes.Equal(share.TargetTable, targetTable) || share.InvitedNetworkId != nil || share.AcceptStatus != "" {
			t.Fatalf("the invite's share: %+v, %v", share, err)
		}
		for _, value := range aclShareTestStoredBytes(t, ctx) {
			if bytes.Contains(value, code) {
				t.Fatal("a row holds the code in the clear")
			}
		}
		if GetNetworkAclInvite(ctx, code) != nil {
			t.Error("the code itself finds the invite")
		}
		// a code filed again is refused, and leaves no second share
		_, err = AclInvite(&AclInviteArgs{CodeHash: codeHash, UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, x.clientSession)
		if status := bridgeShareTestStatus(err); status != "400" {
			t.Errorf("a code filed again: %s", status)
		}
		for name, codeHash := range map[string][]byte{"short": codeHash[:31], "none": nil} {
			_, err := AclInvite(&AclInviteArgs{CodeHash: codeHash, UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, x.clientSession)
			if status := bridgeShareTestStatus(err); status != "400" {
				t.Errorf("a %s code hash: %s", name, status)
			}
		}
		var shareCount int
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(ctx, `SELECT COUNT(*) FROM network_bridge_share WHERE network_id = $1`, x.networkId).Scan(&shareCount))
		})
		if shareCount != 1 {
			t.Errorf("%d shares for one invite", shareCount)
		}
	})
}

// Each claim takes one use: the first binds the invite's own share, a later
// one a new share of the same targets, a repeat takes none, and the row goes at
// its last use, so a claim after it finds nothing.
func TestNetworkAclInviteCountsItsUses(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y1, y2, y3 := newBridgeShareTestSide(t, ctx), newBridgeShareTestSide(t, ctx), newBridgeShareTestSide(t, ctx)
		_, codeHash, invited, targetTable := aclShareTestInvite(t, x, 2, 0)

		first, err := aclShareTestClaim(y1, codeHash)
		if err != nil || first.ShareId != invited.ShareId || first.NetworkId != x.networkId || first.InvitedNetworkId == nil ||
			*first.InvitedNetworkId != y1.networkId || !bytes.Equal(first.TargetTable, targetTable) || first.AcceptStatus != "" {
			t.Fatalf("the first claim: %+v, %v", first, err)
		}
		if invite := GetNetworkAclInvite(ctx, codeHash); invite == nil || invite.UseCount != 1 {
			t.Fatalf("after the first claim: %+v", invite)
		}
		// y1's lost answer, claimed again: the same share, no use taken
		again, err := aclShareTestClaim(y1, codeHash)
		if err != nil || again.ShareId != first.ShareId {
			t.Fatalf("y1's repeat: %+v, %v", again, err)
		}
		if invite := GetNetworkAclInvite(ctx, codeHash); invite == nil || invite.UseCount != 1 {
			t.Fatalf("after y1's repeat: %+v", invite)
		}

		second, err := aclShareTestClaim(y2, codeHash)
		if err != nil || second.ShareId == first.ShareId || second.NetworkId != x.networkId || *second.InvitedNetworkId != y2.networkId ||
			!bytes.Equal(second.TargetTable, targetTable) || second.TargetTableVersion != 1 {
			t.Fatalf("the second claim: %+v, %v", second, err)
		}
		if GetNetworkAclInvite(ctx, codeHash) != nil {
			t.Fatal("the invite's row outlived its last use")
		}
		if _, err := aclShareTestClaim(y3, codeHash); bridgeShareTestStatus(err) != "404" {
			t.Errorf("a claim past the last use: %v", err)
		}
		// each share is its own network's: y1's is not y2's to read
		if _, err := GetNetworkBridgeShareForSidecar(second.ShareId, y1.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Errorf("y1 reading y2's share: %v", err)
		}
	})
}

// A window past 7 days is refused, none takes 7 days, and a claim after the
// window closes is refused and removes the row; the sweep removes the rest.
func TestNetworkAclInviteWindowIsSevenDaysAtMost(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		week := int64(7 * 24 * time.Hour / time.Second)
		_, codeHash := aclShareTestCode(t)
		_, err := AclInvite(&AclInviteArgs{
			CodeHash: codeHash, UseCount: 1, WindowSeconds: week + 1,
			TargetTable: bridgeShareTestSeal(t, []byte("targets")), TargetTableVersion: 1,
		}, x.clientSession)
		if status := bridgeShareTestStatus(err); status != "400" {
			t.Errorf("a window past 7 days: %s", status)
		}
		if GetNetworkAclInvite(ctx, codeHash) != nil {
			t.Error("a refused window left a row")
		}
		now := server.NowUtc()
		_, codeHash, result, _ := aclShareTestInvite(t, x, 1, 0)
		if window := result.ExpireTime.Sub(now); window < 7*24*time.Hour-time.Minute || 7*24*time.Hour+time.Minute < window {
			t.Errorf("no window took %v", window)
		}
		_, shortHash, _, _ := aclShareTestInvite(t, x, 1, 60)

		// the window's close, at the claim and at the sweep
		later := now.Add(7*24*time.Hour + time.Hour)
		if _, err := ClaimNetworkAclInvite(ctx, y.networkId, codeHash, later); !errors.Is(err, ErrNetworkAclShareNotFound) {
			t.Errorf("a claim after the window: %v", err)
		}
		if GetNetworkAclInvite(ctx, codeHash) != nil {
			t.Error("a closed window's row outlived its claim")
		}
		if removed := RemoveExpiredNetworkAclInvites(ctx, now.Add(time.Hour)); removed != 1 {
			t.Errorf("the sweep removed %d", removed)
		}
		if GetNetworkAclInvite(ctx, shortHash) != nil {
			t.Error("the sweep left a closed window")
		}
	})
}

// Y completes the share it claimed once, with its aliases and status; a retry
// of the same bytes answers as the first, other bytes are refused, and a share
// Y did not claim is none of Y's.
func TestNetworkAclInviteCompletion(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y, z := newBridgeShareTestSide(t, ctx), newBridgeShareTestSide(t, ctx)
		_, codeHash, _, _ := aclShareTestInvite(t, x, 1, 0)
		claimed, err := aclShareTestClaim(y, codeHash)
		if err != nil {
			t.Fatal(err)
		}
		aliasTable := bridgeShareTestSeal(t, []byte("aliases"))
		complete := &AclAcceptInviteArgs{ShareId: &claimed.ShareId, AliasTable: aliasTable, AliasTableVersion: 1, AcceptStatus: NetworkBridgeShareAcceptStatusActive}

		if _, err := AclAcceptInvite(complete, z.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Errorf("z completing y's share: %v", err)
		}
		if _, err := AclAcceptInvite(complete, x.clientSession); bridgeShareTestStatus(err) != "404" {
			t.Errorf("x completing its own share: %v", err)
		}
		completed, err := AclAcceptInvite(complete, y.clientSession)
		if err != nil || !bytes.Equal(completed.AliasTable, aliasTable) || completed.AcceptStatus != NetworkBridgeShareAcceptStatusActive || completed.AliasTableVersion != 1 {
			t.Fatalf("y's completion: %+v, %v", completed, err)
		}
		if retried, err := AclAcceptInvite(complete, y.clientSession); err != nil || retried.AcceptStatus != NetworkBridgeShareAcceptStatusActive {
			t.Errorf("a retried completion: %+v, %v", retried, err)
		}
		other := *complete
		other.AliasTable = bridgeShareTestSeal(t, []byte("other aliases"))
		if _, err := AclAcceptInvite(&other, y.clientSession); bridgeShareTestStatus(err) != "409" {
			t.Errorf("a second completion: %v", err)
		}
		for name, bad := range map[string]*AclAcceptInviteArgs{
			"no table":     {ShareId: &claimed.ShareId, AliasTableVersion: 1, AcceptStatus: NetworkBridgeShareAcceptStatusActive},
			"no status":    {ShareId: &claimed.ShareId, AliasTable: aliasTable, AliasTableVersion: 1},
			"a version 0":  {ShareId: &claimed.ShareId, AliasTable: aliasTable, AcceptStatus: NetworkBridgeShareAcceptStatusActive},
			"tables alone": {AliasTable: aliasTable, AliasTableVersion: 1, AcceptStatus: NetworkBridgeShareAcceptStatusActive},
		} {
			if _, err := AclAcceptInvite(bad, y.clientSession); bridgeShareTestStatus(err) != "400" {
				t.Errorf("%s: %v", name, err)
			}
		}
		// a refusal is a completion too
		_, refusedHash, _, _ := aclShareTestInvite(t, x, 1, 0)
		refused, err := aclShareTestClaim(z, refusedHash)
		if err != nil {
			t.Fatal(err)
		}
		shortfall := bridgeShareTestSeal(t, []byte("shortfall"))
		result, err := AclAcceptInvite(&AclAcceptInviteArgs{ShareId: &refused.ShareId, AliasTable: shortfall, AliasTableVersion: 1, AcceptStatus: NetworkBridgeShareAcceptStatusRefusedNoSpace}, z.clientSession)
		if err != nil || result.AcceptStatus != NetworkBridgeShareAcceptStatusRefusedNoSpace {
			t.Errorf("a refusal: %+v, %v", result, err)
		}
	})
}

// A network claims nothing of its own invite, a bearer that is no sidecar
// files and claims nothing, and a plan without acl_network_shares refuses both
// sides.
func TestNetworkAclInviteRefusals(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		y := newBridgeShareTestSide(t, ctx)
		_, codeHash, _, targetTable := aclShareTestInvite(t, x, 1, 0)
		if _, err := aclShareTestClaim(x, codeHash); bridgeShareTestStatus(err) != "400" {
			t.Errorf("x claiming its own invite: %v", err)
		}

		appClientId := server.NewId()
		app := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: y.networkId, ClientId: &appClientId})
		if _, err := aclShareTestClaim(&bridgeShareTestSide{networkId: y.networkId, clientSession: app}, codeHash); bridgeShareTestStatus(err) != "403" {
			t.Errorf("a claim by no sidecar: %v", err)
		}
		_, otherHash := aclShareTestCode(t)
		if _, err := AclInvite(&AclInviteArgs{CodeHash: otherHash, UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, app); bridgeShareTestStatus(err) != "403" {
			t.Errorf("an invite by no sidecar: %v", err)
		}
		for name, useCount := range map[string]int{"no use": 0, "past the most": DefaultNetworkAclShareSettings().UseMaxCount + 1} {
			_, hash := aclShareTestCode(t)
			if _, err := AclInvite(&AclInviteArgs{CodeHash: hash, UseCount: useCount, TargetTable: targetTable, TargetTableVersion: 1}, x.clientSession); bridgeShareTestStatus(err) != "400" {
				t.Errorf("%s: %v", name, err)
			}
		}

		// §21.7's standing refuses x's invite and a claim of an invite x filed
		restore := Testing_SetNetworkBridgeShareOutboundCheck(func(ctx context.Context, networkId server.Id) (bool, error) {
			return networkId != x.networkId, nil
		})
		_, standingHash := aclShareTestCode(t)
		if _, err := AclInvite(&AclInviteArgs{CodeHash: standingHash, UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, x.clientSession); bridgeShareTestStatus(err) != "403" {
			t.Errorf("an invite under probation: %v", err)
		}
		if _, err := aclShareTestClaim(y, codeHash); bridgeShareTestStatus(err) != "403" {
			t.Errorf("a claim of an invite under probation: %v", err)
		}
		restore()

		// a plan that includes no shares
		defer Testing_SetEnforceSidecarLimits(true)()
		defer Testing_SetSidecarLimits(NetworkTierFree, map[string]int64{networkAclShareLimitKey: 0})()
		defer Testing_SetSidecarLimits(NetworkTierPro, map[string]int64{networkAclShareLimitKey: 0})()
		defer Testing_SetSidecarLimits(NetworkTierTeam, map[string]int64{networkAclShareLimitKey: 0})()
		_, tierHash := aclShareTestCode(t)
		if _, err := AclInvite(&AclInviteArgs{CodeHash: tierHash, UseCount: 1, TargetTable: targetTable, TargetTableVersion: 1}, x.clientSession); bridgeShareTestStatus(err) != "403" {
			t.Errorf("an invite on a plan without shares: %v", err)
		}
		if _, err := aclShareTestClaim(y, codeHash); bridgeShareTestStatus(err) != "403" {
			t.Errorf("a claim on a plan without shares: %v", err)
		}
		if invite := GetNetworkAclInvite(ctx, codeHash); invite == nil || invite.UseCount != 1 {
			t.Errorf("a refused claim took a use: %+v", invite)
		}
	})
}

// The invites a network filed go with it.
func TestNetworkAclInviteGoesWithItsNetwork(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		bridgeShareTestAllowOutbound(t)
		x := newBridgeShareTestSide(t, ctx)
		other := newBridgeShareTestSide(t, ctx)
		_, codeHash, _, _ := aclShareTestInvite(t, x, 3, 0)
		_, otherHash, _, _ := aclShareTestInvite(t, other, 3, 0)
		server.Tx(ctx, func(tx server.PgTx) {
			removeNetworkAclSharesInTx(ctx, tx, x.networkId)
		})
		if GetNetworkAclInvite(ctx, codeHash) != nil {
			t.Error("x's invite outlived x")
		}
		if GetNetworkAclInvite(ctx, otherHash) == nil {
			t.Error("another network's invite went with x")
		}
	})
}

package model

// Which extenders the public seed bundle may publish a revocation for, on the real
// tables (connect/EXTENDER-UPDATE2.md §2.3, taskworker/work/extender_seed_publish.go):
// only an inactive extender with a revocation time. A revocation drops the key from
// every client that holds it from any source, so an extender that merely left the
// cohort, or came back, must not get one.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
)

func testSeedModelExtender(ctx context.Context, index int) (server.Id, []byte) {
	publicKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{byte(index + 1)}, ed25519.SeedSize)).Public().(ed25519.PublicKey)
	extenderId := server.NewId()
	now := server.NowUtc()
	Testing_CreateNetworkExtender(ctx, &NetworkExtender{
		ExtenderId:  extenderId,
		NetworkId:   server.NewId(),
		ClientId:    server.NewId(),
		PublicKey:   publicKey,
		CreateTime:  now,
		TcpPort:     443,
		UdpPort:     443,
		DnsPort:     53,
		DnsTld:      connect.DefaultExtenderDnsTld,
		CountryCode: "us",
		Active:      true,
	}, []*NetworkExtenderAddress{{
		IpVersion:    4,
		Ip:           netip.MustParseAddr(fmt.Sprintf("198.51.100.%d", index+1)),
		Carriers:     []string{connect.ExtenderCarrierTcp},
		ActivateTime: now,
		Active:       true,
	}})
	return extenderId, publicKey
}

func testSeedModelSetExtenderState(ctx context.Context, extenderId server.Id, active bool, revokeTime *time.Time) {
	server.Tx(ctx, func(tx server.PgTx) {
		server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE network_extender SET active = $2, revoke_time = $3 WHERE extender_id = $1`,
			extenderId,
			active,
			revokeTime,
		))
	})
}

func TestGetRevokedNetworkExtendersByPublicKey(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		revokeTime := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

		_, activeKey := testSeedModelExtender(ctx, 0)
		revokedId, revokedKey := testSeedModelExtender(ctx, 1)
		testSeedModelSetExtenderState(ctx, revokedId, false, &revokeTime)
		// left the pool with no revocation: not withdrawn
		inactiveId, inactiveKey := testSeedModelExtender(ctx, 2)
		testSeedModelSetExtenderState(ctx, inactiveId, false, nil)
		// revoked once and active again: its record is good
		returnedId, returnedKey := testSeedModelExtender(ctx, 3)
		testSeedModelSetExtenderState(ctx, returnedId, true, &revokeTime)
		// revoked, but not one of the keys asked about
		otherId, _ := testSeedModelExtender(ctx, 4)
		testSeedModelSetExtenderState(ctx, otherId, false, &revokeTime)
		unknownKey := bytes.Repeat([]byte{0xee}, ed25519.PublicKeySize)

		revocations := GetRevokedNetworkExtendersByPublicKey(ctx, [][]byte{activeKey, revokedKey, inactiveKey, returnedKey, unknownKey})
		if len(revocations) != 1 {
			t.Fatalf("%d revocations, want only the revoked extender's", len(revocations))
		}
		if revocations[0].ExtenderId != revokedId || !bytes.Equal(revocations[0].PublicKey, revokedKey) || !revocations[0].RevokeTime.Equal(revokeTime) {
			t.Fatalf("revocation of the wrong extender or time: %v %s", revocations[0].ExtenderId, revocations[0].RevokeTime)
		}

		if revocations := GetRevokedNetworkExtendersByPublicKey(ctx, nil); len(revocations) != 0 {
			t.Fatalf("no keys asked about: %d revocations", len(revocations))
		}

		// the lookup is bounded: keys past the guard are not read
		bounded := slices.Repeat([][]byte{unknownKey}, networkExtenderSeedRevocationLookupMax)
		if revocations := GetRevokedNetworkExtendersByPublicKey(ctx, append(bounded, revokedKey)); len(revocations) != 0 {
			t.Fatalf("a key past the lookup guard was read: %d revocations", len(revocations))
		}
	})
}

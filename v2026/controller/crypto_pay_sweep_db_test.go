package controller

// The issued address sweep's tables and the issued address lookups, on the
// test database (model/crypto_address_sweep_model.go,
// model/crypto_payment_model.go).
//
// Needs the test database (server.DefaultTestEnv) and pro.yml.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect/v2026"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// The sweep lists a coin's issued addresses in pool index order after a
// position, at most a limit, each with its cursor: every invoice whatever its
// state (closed long ago, past the retention unlink), never another coin's.
// The cursor and the position are written back as given. The issued address
// probe and lookup see every invoice of the coin, with its time.
func TestCryptoAddressSweepTablesReadAndWrite(t *testing.T) {
	skipWithoutProYml(t)
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId, _, _ := cryptoDbTestNetwork(t, ctx, "sweeptables")
		eth := model.CryptoAssetEth
		address := func(i int) string {
			return fmt.Sprintf("0xsyntheticsweep%026d", i)
		}
		for i := range 5 {
			cryptoDbCreateCoinbaseIntent(t, ctx, eth, &networkId, StripeItemData1Tib, 3, int64(20+i), address(i))
		}
		now := server.NowUtc()
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET expires_at = $2 WHERE asset = 'eth' AND address = $1`, address(1), now.Add(-90*24*time.Hour))
		cryptoDbExec(ctx, `UPDATE crypto_payment_intent SET unlinked_at = $2, network_id = NULL WHERE asset = 'eth' AND address = $1`, address(2), now.Add(-time.Hour))
		cryptoDbCreateCoinbaseIntent(t, ctx, model.CryptoAssetSol, &networkId, StripeItemData1Tib, 3, 21, "So1syntheticsweep")

		indexes := func(listed []*model.CryptoAddressSweepAddress) []int64 {
			out := []int64{}
			for _, address := range listed {
				out = append(out, address.AddressIndex)
			}
			return out
		}
		listed := model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, -1, 3)
		connect.AssertEqual(t, indexes(listed), []int64{20, 21, 22})
		connect.AssertEqual(t, listed[1].Address, address(1))
		connect.AssertEqual(t, listed[0].Cursor == nil, true)
		connect.AssertEqual(t, indexes(model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, 22, 10)), []int64{23, 24})
		connect.AssertEqual(t, len(model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, 24, 10)), 0)
		connect.AssertEqual(t, len(model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, -1, 0)), 0)

		// the rotation's position
		connect.AssertEqual(t, model.GetCryptoAddressSweepPositionInConn(nil, ctx, eth), int64(-1))
		model.SetCryptoAddressSweepPositionInConn(nil, ctx, eth, 22, now)
		connect.AssertEqual(t, model.GetCryptoAddressSweepPositionInConn(nil, ctx, eth), int64(22))
		connect.AssertEqual(t, model.GetCryptoAddressSweepPositionInConn(nil, ctx, model.CryptoAssetSol), int64(-1))
		model.SetCryptoAddressSweepPositionInConn(nil, ctx, eth, -1, now)
		connect.AssertEqual(t, model.GetCryptoAddressSweepPositionInConn(nil, ctx, eth), int64(-1))

		// an address's cursor, as written
		height := int64(26_350_000)
		readAt := now.Truncate(time.Second)
		model.SetCryptoAddressSweepCursorInConn(nil, ctx, &model.CryptoAddressSweepCursor{Asset: eth, Address: address(1), Height: &height, ReadAt: &readAt}, now)
		listed = model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, 20, 1)
		connect.AssertEqual(t, indexes(listed), []int64{21})
		cursor := listed[0].Cursor
		connect.AssertEqual(t, cursor != nil, true)
		connect.AssertEqual(t, *cursor.Height, height)
		connect.AssertEqual(t, cursor.ReadAt.Equal(readAt), true)
		connect.AssertEqual(t, cursor.Signature, "")
		connect.AssertEqual(t, cursor.LastError, "")
		// written again, as given: a failed read keeps its position and says why
		model.SetCryptoAddressSweepCursorInConn(nil, ctx, &model.CryptoAddressSweepCursor{Asset: eth, Address: address(1), Height: &height, Signature: "synthetic-signature", LastError: "http 502"}, now)
		cursor = model.ListCryptoAddressesToSweepInConn(nil, ctx, eth, 20, 1)[0].Cursor
		connect.AssertEqual(t, cursor.Signature, "synthetic-signature")
		connect.AssertEqual(t, cursor.LastError, "http 502")
		connect.AssertEqual(t, cursor.ReadAt == nil, true)

		// every invoice of the coin is an issued address, with its time
		connect.AssertEqual(t, model.CryptoPaymentAddressesIssuedInConn(nil, ctx, eth), true)
		connect.AssertEqual(t, model.CryptoPaymentAddressesIssuedInConn(nil, ctx, model.CryptoAssetTao), false)
		issued := model.GetCryptoPaymentIssuedAddressesInConn(nil, ctx, eth, []string{address(1), address(2), "0xsyntheticnotours"})
		connect.AssertEqual(t, len(issued), 2)
		connect.AssertEqual(t, issued[address(1)].AddressIndex, int64(21))
		connect.AssertEqual(t, issued[address(2)].AddressIndex, int64(22))
		connect.AssertEqual(t, issued[address(2)].CreateTime.IsZero(), false)
		connect.AssertEqual(t, len(model.GetCryptoPaymentIssuedAddressesInConn(nil, ctx, eth, []string{})), 0)
	})
}

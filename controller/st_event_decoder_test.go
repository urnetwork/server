package controller

import (
	"math/big"
	"reflect"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/urfoundation/sn/stabi"
)

func settlementVaultEventLog(t *testing.T, name string, indexed []common.Hash, values ...any) *types.Log {
	t.Helper()
	parsed, err := stabi.STSettlementVaultMetaData.ParseABI()
	if err != nil {
		t.Fatal(err)
	}
	event, ok := parsed.Events[name]
	if !ok {
		t.Fatalf("settlement-vault ABI has no %s event", name)
	}
	data, err := event.Inputs.NonIndexed().Pack(values...)
	if err != nil {
		t.Fatal(err)
	}
	return &types.Log{Topics: append([]common.Hash{event.ID}, indexed...), Data: data}
}

func decodeReleaseEvent(t *testing.T, log *types.Log) (string, map[string]any) {
	t.Helper()
	client := &CoreStClient{
		coordinator: stabi.NewSTCoordinator(),
		vault:       stabi.NewSTSettlementVault(),
		reserve:     stabi.NewSTReserveSink(),
	}
	for _, decoder := range client.stEventDecoders() {
		if kind, data, ok := decoder(log); ok {
			return kind, data
		}
	}
	t.Fatal("release event was not decoded")
	return "", nil
}

func TestStEventDecoderMirrorsEmissionDustDeferral(t *testing.T) {
	pool := common.HexToHash("0x1234")
	log := settlementVaultEventLog(
		t,
		"EmissionDustDeferred",
		[]common.Hash{common.BigToHash(big.NewInt(7)), common.BigToHash(big.NewInt(3)), pool},
		big.NewInt(175_960_612),
		big.NewInt(99_999),
		uint64(100_000),
	)
	kind, data := decodeReleaseEvent(t, log)
	want := map[string]any{
		"e": "7", "no_id": "3", "pool_hotkey": pool.Hex(),
		"observed_alpha_rao": "175960612", "tao_equivalent_rao": "99999", "minimum_transfer_tao_rao": "100000",
	}
	if kind != "EmissionDustDeferred" || !reflect.DeepEqual(data, want) {
		t.Fatalf("decoded dust deferral = %s %#v, want %#v", kind, data, want)
	}
}

func TestStEventDecoderMirrorsDeferredClaimCredit(t *testing.T) {
	coldkey := common.HexToHash("0xabcd")
	log := settlementVaultEventLog(
		t,
		"ClaimPaymentDeferred",
		[]common.Hash{coldkey},
		big.NewInt(175_960_612),
		big.NewInt(99_999),
		uint64(100_000),
		uint8(1),
	)
	kind, data := decodeReleaseEvent(t, log)
	want := map[string]any{
		"coldkey": coldkey.Hex(), "credit_alpha_rao": "175960612", "tao_equivalent_rao": "99999",
		"minimum_transfer_tao_rao": "100000", "reason": "1",
	}
	if kind != "ClaimPaymentDeferred" || !reflect.DeepEqual(data, want) {
		t.Fatalf("decoded claim deferral = %s %#v, want %#v", kind, data, want)
	}
}

func TestStEventDecoderDistinguishesActualClaimPayment(t *testing.T) {
	coldkey := common.HexToHash("0xabcd")
	relayer := common.HexToAddress("0x0000000000000000000000000000000000000521")
	log := settlementVaultEventLog(
		t,
		"ClaimPaid",
		[]common.Hash{coldkey, common.BytesToHash(relayer.Bytes())},
		big.NewInt(351_921_226),
	)
	kind, data := decodeReleaseEvent(t, log)
	want := map[string]any{"coldkey": coldkey.Hex(), "amount": "351921226", "caller": relayer.Hex()}
	if kind != "ClaimPaid" || !reflect.DeepEqual(data, want) {
		t.Fatalf("decoded claim payment = %s %#v, want %#v", kind, data, want)
	}
}

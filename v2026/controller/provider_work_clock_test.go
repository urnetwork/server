// The real epoch authority reader retains committed Frontier bytes, including
// millisecond remainders omitted by the ordinary public timestamp projection.
package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http/httptest"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/urfoundation/sn/v2026/payoutartifact"
	"github.com/urfoundation/sn/v2026/stabi"
	"github.com/urnetwork/server/v2026"
)

// The inherited raw ABI fixture remains the independent deployment witness;
// only its block hashes are remapped to the full original test header preimages.
type providerWorkClockRpcFixture struct {
	*stPayoutPolicyRpcFixture
	headers map[uint64]*types.Header
	missing bool
}

// This reproduces the actual Frontier seconds/base-fee public projection.
func (self *providerWorkClockRpcFixture) GetBlockByNumber(_ context.Context, tag rpc.BlockNumber, full bool) (map[string]any, error) {
	if full || tag == 0 {
		return nil, nil
	}
	block := uint64(tag)
	if tag == rpc.FinalizedBlockNumber {
		block = 275
	}
	header := self.headers[block]
	if header == nil {
		return nil, errors.New("synthetic clock block unavailable")
	}
	if self.missing {
		return map[string]any{"number": hexutil.EncodeUint64(block), "hash": header.Hash(), "timestamp": hexutil.EncodeUint64(header.Time / 1000)}, nil
	}
	raw, err := json.Marshal(header)
	if err != nil {
		return nil, err
	}
	var rendered map[string]any
	if err := json.Unmarshal(raw, &rendered); err != nil {
		return nil, err
	}
	rendered["hash"], rendered["timestamp"], rendered["baseFeePerGas"] = header.Hash(), hexutil.EncodeUint64(header.Time/1000), "0x7"
	return rendered, nil
}

// Map the full-header selector back to the existing deterministic ABI owner;
// no response is built from the proposed production verification outcome.
func (self *providerWorkClockRpcFixture) Call(ctx context.Context, call map[string]hexutil.Bytes, selector rpc.BlockNumberOrHash) (hexutil.Bytes, error) {
	if selector.BlockHash != nil {
		for block, header := range self.headers {
			if header.Hash() == *selector.BlockHash {
				selector = rpc.BlockNumberOrHashWithHash(common.Hash(stPayoutPolicyBoundary(block).Hash), selector.RequireCanonical)
				break
			}
		}
	}
	return self.stPayoutPolicyRpcFixture.Call(ctx, call, selector)
}

// A bounded full production read must preserve both exact preimages, while an
// older incomplete projection stays unknown instead of inventing a clock.
func TestProviderWorkActualEpochReaderRetainsFrontierHeaderPreimages(t *testing.T) {
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		base, _, cfg, _ := newStPayoutPolicyFixture(t, 1)
		base.configure(275, "")
		fixture := &providerWorkClockRpcFixture{stPayoutPolicyRpcFixture: base, headers: map[uint64]*types.Header{}}
		for _, block := range []uint64{200, 250, 275} {
			fixture.headers[block] = &types.Header{Number: new(big.Int).SetUint64(block), Difficulty: big.NewInt(0), Time: uint64(stPayoutPolicyTime(block).Unix())*1000 + 731, GasLimit: 100000, Extra: []byte{byte(block)}}
		}
		endpoint := rpc.NewServer()
		if err := errors.Join(endpoint.RegisterName("eth", fixture), endpoint.RegisterName("chain", fixture)); err != nil {
			t.Fatal(err)
		}
		httpEndpoint := httptest.NewServer(endpoint)
		defer httpEndpoint.Close()
		defer endpoint.Stop()
		copyCfg := *cfg
		copyCfg.RpcUrls = []string{httpEndpoint.URL}
		client := &CoreStClient{cfg: &copyCfg, coordinator: stabi.NewSTCoordinator(), clients: map[string]*ethclient.Client{}}
		defer func() {
			for _, connection := range client.clients {
				connection.Close()
			}
		}()
		authority, err := client.PayoutEpochAuthority(t.Context(), 1)
		if err != nil {
			t.Fatal("actual Frontier epoch read failed", err)
		}
		for index, block := range []uint64{200, 250} {
			raw, err := rlp.EncodeToBytes(fixture.headers[block])
			if err != nil {
				t.Fatal(err)
			}
			got := authority.StartHeader
			if index == 1 {
				got = authority.EndHeader
			}
			if !bytes.Equal(got, raw) {
				t.Fatal("actual reader lost committed millisecond remainder", block)
			}
		}
		if authority.ClockProfile != payoutartifact.FrontierWindowClockProfile {
			t.Fatal("Frontier units lost their exact profile")
		}
		fixture.missing = true
		authority, err = client.PayoutEpochAuthority(t.Context(), 1)
		if err != nil || len(authority.StartHeader) != 0 || len(authority.EndHeader) != 0 || authority.ClockProfile != "" {
			t.Fatal("missing originals became a complete clock", err)
		}
	})
}

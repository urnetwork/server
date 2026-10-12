package controller

// st_ledger_v2_prune.go — the SN25 prune-risk alert (VERSION2-FINAL.md §8.1,
// §17 dissolution row). Dissolving the pool's subnet strands vault2's
// custody. Runtime 475 dissolves one when a network registration arrives at
// the subnet limit: the non-immune subnet with the lowest moving alpha price,
// the earliest registered on a tie (pallets/subtensor/src/subnets/subnet.rs:
// 192-235, coinbase/root.rs:344-376). The driver reads that census from
// Subtensor storage at the finalized head, at most once per
// stLedgerV2PruneCheckInterval, and alerts while the pool's subnet ranks
// among the lowest StConfig.PruneAlertPositions subnets of that order.

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"math/big"
	"math/bits"
	"slices"
	"sync"
	"time"

	"github.com/centrifuge/go-substrate-rpc-client/v4/scale"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
	"github.com/holiman/uint256"
)

const (
	// stLedgerV2DefaultPruneAlertPositions is the alert's margin when st.yml
	// sets none: the pool's subnet among the three next to be dissolved.
	stLedgerV2DefaultPruneAlertPositions = 3
	// stLedgerV2PruneCheckInterval spaces the census reads. Moving prices are
	// slow averages, so a read every few minutes misses nothing.
	stLedgerV2PruneCheckInterval = 10 * time.Minute
	// stLedgerV2CensusPageSize is the most keys state_getKeysPaged returns.
	stLedgerV2CensusPageSize = 1000
	// The runtime's ValueQuery defaults for the census's absent values
	// (pallets/subtensor/src/lib.rs:994,1456; runtime/src/lib.rs:962,1042).
	// Every other census value defaults to zero or empty.
	stSubtensorDefaultSubnetLimit    = 128
	stSubtensorDefaultImmunityPeriod = 1_296_000
)

// stLedgerV2Subnet is one added subnet as get_network_to_prune sees it.
type stLedgerV2Subnet struct {
	Netuid       uint16
	RegisteredAt uint64
	// MovingPrice is get_moving_alpha_price in TAO per alpha as 32-bit fixed
	// point: SubnetMovingPrice's I96F32 bits with a negative price read as
	// zero, which is how its U64F64 conversion compares, or 1.0 for a stable
	// subnet (SubnetMechanism 0) (staking/stake_utils.rs:33-44).
	MovingPrice uint256.Int
}

// stLedgerV2Census is the chain state a network registration and
// get_network_to_prune read, at one finalized block.
type stLedgerV2Census struct {
	Block          uint64
	SubnetLimit    uint16
	ImmunityPeriod uint64
	// The lengths of DissolveCleanupQueue and NetworkRegistrationQueue.
	CleanupQueue      uint64
	RegistrationQueue uint64
	// Subnets are the subnets NetworksAdded marks added, root excluded, in
	// storage order.
	Subnets []stLedgerV2Subnet
}

// prunes reports whether a network registration now dissolves a subnet: the
// added subnets and those still being dissolved reach the limit, and the
// dissolving ones do not free a netuid for each queued registration
// (subnets/subnet.rs:194-215).
func (self *stLedgerV2Census) prunes() bool {
	return uint64(self.SubnetLimit) <= uint64(len(self.Subnets))+self.CleanupQueue && self.CleanupQueue <= self.RegistrationQueue
}

// pruneOrder returns the non-immune subnets in the order registrations at the
// limit dissolve them: lowest moving price first, then earliest registration,
// then storage order (the netuid's little-endian bytes), since
// get_network_to_prune keeps the first of equal candidates it iterates
// (coinbase/root.rs:344-376). A subnet is immune before block
// RegisteredAt + ImmunityPeriod, a sum that saturates.
func (self *stLedgerV2Census) pruneOrder() []stLedgerV2Subnet {
	order := []stLedgerV2Subnet{}
	for _, subnet := range self.Subnets {
		if self.Block < subnet.RegisteredAt || self.Block-subnet.RegisteredAt < self.ImmunityPeriod {
			continue
		}
		order = append(order, subnet)
	}
	slices.SortFunc(order, func(a stLedgerV2Subnet, b stLedgerV2Subnet) int {
		if c := a.MovingPrice.Cmp(&b.MovingPrice); c != 0 {
			return c
		}
		if c := cmp.Compare(a.RegisteredAt, b.RegisteredAt); c != 0 {
			return c
		}
		return cmp.Compare(bits.ReverseBytes16(a.Netuid), bits.ReverseBytes16(b.Netuid))
	})
	return order
}

// stLedgerV2CensusReader reads the census; CoreStClient implements it.
type stLedgerV2CensusReader interface {
	ledgerV2Census(ctx context.Context) (*stLedgerV2Census, error)
}

var _ stLedgerV2CensusReader = (*CoreStClient)(nil)

// Last census read per deployment and subnet.
var stLedgerV2PruneChecks sync.Map

// stLedgerV2CheckPrune reads the census at most once per
// stLedgerV2PruneCheckInterval, a failed read included, and raises the
// prune-risk alert. A client that cannot read the census is not checked.
func stLedgerV2CheckPrune(ctx context.Context, cfg *StConfig, client StLedgerV2Client, now time.Time) error {
	reader, ok := client.(stLedgerV2CensusReader)
	if !ok {
		return nil
	}
	key := fmt.Sprintf("%s|%d", cfg.DeploymentKey(), cfg.Netuid)
	if last, ok := stLedgerV2PruneChecks.Load(key); ok && now.Sub(last.(time.Time)) < stLedgerV2PruneCheckInterval {
		return nil
	}
	stLedgerV2PruneChecks.Store(key, now)
	census, err := reader.ledgerV2Census(ctx)
	if err != nil {
		return fmt.Errorf("subnet census: %w", err)
	}
	stLedgerV2PruneAlert(cfg, census)
	return nil
}

// stLedgerV2PruneAlert raises StLedgerV2AlertPruneRisk when the pool's subnet
// is no longer added, or ranks among the lowest PruneAlertPositions subnets
// of the prune order. The subject names the rank and whether a registration
// now dissolves a subnet, so each step closer alerts once.
func stLedgerV2PruneAlert(cfg *StConfig, census *stLedgerV2Census) {
	netuid := uint16(cfg.Netuid)
	if !slices.ContainsFunc(census.Subnets, func(subnet stLedgerV2Subnet) bool { return subnet.Netuid == netuid }) {
		stLedgerV2Alert(cfg, StLedgerV2AlertPruneRisk, fmt.Sprintf("netuid %d not added", netuid),
			"subnet %d is not an added subnet at block %d; if it was dissolved, vault2's custody is stranded", netuid, census.Block)
		return
	}
	positions := cfg.PruneAlertPositions
	if positions == 0 {
		positions = stLedgerV2DefaultPruneAlertPositions
	}
	order := census.pruneOrder()
	rank := slices.IndexFunc(order, func(subnet stLedgerV2Subnet) bool { return subnet.Netuid == netuid })
	if rank < 0 || positions <= uint64(rank) {
		return
	}
	subject, dissolves := fmt.Sprintf("netuid %d rank %d", netuid, rank+1), "no subnet"
	if census.prunes() {
		subject, dissolves = subject+" at the limit", fmt.Sprintf("subnet %d", order[0].Netuid)
	}
	stLedgerV2Alert(cfg, StLedgerV2AlertPruneRisk, subject,
		"at block %d subnet %d ranks %d of %d non-immune subnets by moving price, lowest first (%s TAO per alpha, the lowest %s); %d subnets are added and %d dissolving under a limit of %d, so a registration now dissolves %s",
		census.Block, netuid, rank+1, len(order), stLedgerV2PriceText(&order[rank].MovingPrice), stLedgerV2PriceText(&order[0].MovingPrice),
		len(census.Subnets), census.CleanupQueue, census.SubnetLimit, dissolves)
}

func stLedgerV2PriceText(price *uint256.Int) string {
	return new(big.Float).SetMantExp(new(big.Float).SetInt(price.ToBig()), -32).Text('f', 9)
}

// ---------------------------------------------------------------------------
// Census read
// ---------------------------------------------------------------------------

// stSubtensorStorageKey is a SubtensorModule storage key: twox128 of the
// pallet and of the item (controller/sn_substrate.go), then each netuid map
// key under its Identity hasher, little-endian.
func stSubtensorStorageKey(item string, netuid ...uint16) []byte {
	key := append(twox128([]byte("SubtensorModule")), twox128([]byte(item))...)
	for _, n := range netuid {
		key = binary.LittleEndian.AppendUint16(key, n)
	}
	return key
}

// ledgerV2Census reads the census at the finalized head through the
// substrate JSON-RPC methods the configured endpoints serve beside the EVM
// ones, the whole read again at the next endpoint after a failure.
func (self *CoreStClient) ledgerV2Census(ctx context.Context) (*stLedgerV2Census, error) {
	var census *stLedgerV2Census
	err := self.eachRpc(ctx, func(ctx context.Context, client *ethclient.Client) error {
		var err error
		census, err = stReadLedgerV2Census(ctx, client.Client())
		return err
	})
	return census, err
}

// stReadLedgerV2Census reads the finalized head, the NetworksAdded keys at it,
// then every census value at it in one state_queryStorageAt.
func stReadLedgerV2Census(ctx context.Context, client *rpc.Client) (*stLedgerV2Census, error) {
	var hash string
	if err := client.CallContext(ctx, &hash, "chain_getFinalizedHead"); err != nil {
		return nil, fmt.Errorf("chain_getFinalizedHead: %w", err)
	}
	var header *struct {
		Number hexutil.Uint64 `json:"number"`
	}
	if err := client.CallContext(ctx, &header, "chain_getHeader", hash); err != nil {
		return nil, fmt.Errorf("chain_getHeader: %w", err)
	}
	if header == nil {
		return nil, fmt.Errorf("chain_getHeader: no header for %s", hash)
	}

	prefix := stSubtensorStorageKey("NetworksAdded")
	netuids := []uint16{}
	var start *string
	for {
		var page []string
		if err := client.CallContext(ctx, &page, "state_getKeysPaged", hexutil.Encode(prefix), stLedgerV2CensusPageSize, start, hash); err != nil {
			return nil, fmt.Errorf("state_getKeysPaged: %w", err)
		}
		for _, key := range page {
			raw, err := hexutil.Decode(key)
			if err != nil || len(raw) != len(prefix)+2 || !bytes.HasPrefix(raw, prefix) {
				return nil, fmt.Errorf("state_getKeysPaged: malformed NetworksAdded key %q", key)
			}
			// Root is never counted or dissolved.
			if netuid := binary.LittleEndian.Uint16(raw[len(prefix):]); netuid != 0 {
				netuids = append(netuids, netuid)
			}
		}
		if len(page) < stLedgerV2CensusPageSize {
			break
		}
		start = &page[len(page)-1]
	}

	keys := []string{}
	for _, item := range []string{"SubnetLimit", "NetworkImmunityPeriod", "DissolveCleanupQueue", "NetworkRegistrationQueue"} {
		keys = append(keys, hexutil.Encode(stSubtensorStorageKey(item)))
	}
	for _, netuid := range netuids {
		for _, item := range []string{"NetworksAdded", "NetworkRegisteredAt", "SubnetMechanism", "SubnetMovingPrice"} {
			keys = append(keys, hexutil.Encode(stSubtensorStorageKey(item, netuid)))
		}
	}
	var sets []struct {
		Changes [][2]*string `json:"changes"`
	}
	if err := client.CallContext(ctx, &sets, "state_queryStorageAt", keys, hash); err != nil {
		return nil, fmt.Errorf("state_queryStorageAt: %w", err)
	}
	if len(sets) != 1 {
		return nil, fmt.Errorf("state_queryStorageAt: %d change sets, want 1", len(sets))
	}
	values := &stCensusValues{values: map[string]*string{}}
	for _, change := range sets[0].Changes {
		if change[0] != nil {
			values.values[*change[0]] = change[1]
		}
	}

	census := &stLedgerV2Census{
		Block:             uint64(header.Number),
		SubnetLimit:       uint16(values.unsigned("SubnetLimit", 2, stSubtensorDefaultSubnetLimit)),
		ImmunityPeriod:    values.unsigned("NetworkImmunityPeriod", 8, stSubtensorDefaultImmunityPeriod),
		CleanupQueue:      values.length("DissolveCleanupQueue"),
		RegistrationQueue: values.length("NetworkRegistrationQueue"),
	}
	for _, netuid := range netuids {
		added := values.unsigned("NetworksAdded", 1, 0, netuid)
		if 1 < added {
			values.fail("malformed", "NetworksAdded", netuid)
		}
		registeredAt := values.unsigned("NetworkRegisteredAt", 8, 0, netuid)
		mechanism := values.unsigned("SubnetMechanism", 2, 0, netuid)
		price := values.price(netuid)
		if mechanism == 0 {
			price = *uint256.NewInt(1 << 32)
		}
		if added == 1 {
			census.Subnets = append(census.Subnets, stLedgerV2Subnet{Netuid: netuid, RegisteredAt: registeredAt, MovingPrice: price})
		}
	}
	if values.err != nil {
		return nil, values.err
	}
	return census, nil
}

// stCensusValues decodes state_queryStorageAt's answers and keeps the first
// error.
type stCensusValues struct {
	values map[string]*string
	err    error
}

func (self *stCensusValues) fail(reason string, item string, netuid ...uint16) {
	if self.err == nil {
		self.err = fmt.Errorf("state_queryStorageAt: %s %s%v", reason, item, netuid)
	}
}

// raw returns an item's value and whether it is present. A value of the
// wrong size (any size when size is negative) or a key without an answer
// fails.
func (self *stCensusValues) raw(item string, size int, netuid ...uint16) ([]byte, bool) {
	encoded, answered := self.values[hexutil.Encode(stSubtensorStorageKey(item, netuid...))]
	if !answered {
		self.fail("no answer for", item, netuid...)
		return nil, false
	}
	if encoded == nil {
		return nil, false
	}
	value, err := hexutil.Decode(*encoded)
	if err != nil || (0 <= size && len(value) != size) {
		self.fail("malformed", item, netuid...)
		return nil, false
	}
	return value, true
}

// unsigned decodes a little-endian unsigned value of size bytes.
func (self *stCensusValues) unsigned(item string, size int, absent uint64, netuid ...uint16) uint64 {
	value, present := self.raw(item, size, netuid...)
	if !present {
		return absent
	}
	var padded [8]byte
	copy(padded[:], value)
	return binary.LittleEndian.Uint64(padded[:])
}

// length decodes a SCALE vector's compact length prefix.
func (self *stCensusValues) length(item string) uint64 {
	value, present := self.raw(item, -1)
	if !present {
		return 0
	}
	length, err := scale.NewDecoder(bytes.NewReader(value)).DecodeUintCompact()
	if err != nil || !length.IsUint64() {
		self.fail("malformed", item)
		return 0
	}
	return length.Uint64()
}

// price decodes SubnetMovingPrice, an I96F32 stored as a little-endian i128,
// reading a negative price as zero.
func (self *stCensusValues) price(netuid uint16) uint256.Int {
	var price uint256.Int
	value, present := self.raw("SubnetMovingPrice", 16, netuid)
	if !present || value[15]&0x80 != 0 {
		return price
	}
	bigEndian := slices.Clone(value)
	slices.Reverse(bigEndian)
	price.SetBytes(bigEndian)
	return price
}

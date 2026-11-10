package model

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// §7.9's ticks and retention at api (TEAM-M2-14).

// api appends only a tick its verifier takes at the tip: every reaped id an ephemeral admission, and no
// activation until TEAM-M2-05's window lands; a refused tick appends nothing.
func TestConfigLogTickIsCheckedAtTheTip(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	networkId := server.Id(world.NetworkId())
	tip, err := sequencer.Tip(t.Context(), networkId)
	if err != nil {
		t.Fatal(err)
	}

	genesis, err := testConfigLogGenesisOf(world.SignedGenesis())
	if err != nil {
		t.Fatal(err)
	}
	for _, tick := range []*bridge.ApiTick{
		{ReapedSidecarIds: [][]byte{genesis.GetOwnerSidecarId()}},
		{ReapedSidecarIds: [][]byte{sidecar.NewId().Bytes()}},
		{ReapedSidecarIds: [][]byte{[]byte("short")}},
		{ActivatedProposalIds: [][]byte{make([]byte, 32)}},
	} {
		_, err := sequencer.AppendTick(t.Context(), networkId, tick)
		configLogError := &ConfigLogError{}
		if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden {
			t.Fatalf("tick %v: %v", tick, err)
		}
	}
	after, err := sequencer.Tip(t.Context(), networkId)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, after.GetVersion(), tip.GetVersion())
	connect.AssertEqual(t, bytes.Equal(after.GetChain(), tip.GetChain()), true)

	// with no live ephemeral admission there is no tick to make
	result, err := appendConfigLogTickAt(t.Context(), sequencer, networkId, DefaultConfigLogTickSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.N == nil && !result.More, true)
	live, err := sequencer.LiveEphemeralAdmissions(t.Context(), networkId, 10)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(live), 0)
}

// The Genesis a world's Signed Genesis carries.
func testConfigLogGenesisOf(signedGenesis []byte) (*bridge.Genesis, error) {
	signed := &bridge.Signed{}
	if err := proto.Unmarshal(signedGenesis, signed); err != nil {
		return nil, err
	}
	message, err := sidecar.ParseConfigStatement(bridge.SignedPurposeAuthority, bridge.SignedType_SIGNED_GENESIS, signed.GetBody())
	if err != nil {
		return nil, err
	}
	genesis, ok := message.(*bridge.Genesis)
	if !ok {
		return nil, errors.New("not a genesis")
	}
	return genesis, nil
}

// The retention point: the newest base at least 30 days old sets it, from its folded_through and the base
// before it; younger bases retire nothing yet, and the first base keeps everything.
func TestConfigLogRetentionPoint(t *testing.T) {
	day := 24 * time.Hour
	keep := 30 * day
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	at := func(age time.Duration) int64 {
		return now.Add(-age).UnixMilli()
	}
	bases := []*configLogBaseInfo{
		{n: 1, timeMs: at(90 * day), foldedThrough: 0},
		{n: 1100, timeMs: at(60 * day), foldedThrough: 1099},
		{n: 2200, timeMs: at(31 * day), foldedThrough: 1500},
		{n: 3300, timeMs: at(2 * day), foldedThrough: 3299},
	}
	cases := []struct {
		bases []*configLogBaseInfo
		point uint64
	}{
		{bases: bases[:1], point: 1},
		{bases: bases[:2], point: 1},
		// base 2200, 31 days old: min(1100, 1501)
		{bases: bases[:3], point: 1100},
		// base 3300 is 2 days old, so 2200 still decides
		{bases: bases, point: 1100},
		// with 3300 past 30 days too: min(2200, 3300)
		{bases: append(append([]*configLogBaseInfo{}, bases[:3]...), &configLogBaseInfo{n: 3300, timeMs: at(31 * day), foldedThrough: 3299}), point: 2200},
		// a fold that stayed: min(previous base, folded_through + 1)
		{bases: []*configLogBaseInfo{{n: 1, timeMs: at(90 * day)}, {n: 50, timeMs: at(40 * day), foldedThrough: 0}}, point: 1},
		{bases: nil, point: 1},
	}
	for _, c := range cases {
		connect.AssertEqual(t, configLogRetentionPoint(c.bases, now.UnixMilli(), keep), c.point)
	}
	retained := configLogRetainedRows([]*configLogBaseInfo{
		{n: 10, rowsSha256: []byte("a")},
		{n: 20, rowsSha256: []byte("b")},
		{n: 30, rowsSha256: []byte("a")},
	}, 20)
	connect.AssertEqual(t, retained, map[string]bool{"a": true, "b": true})
}

// A pass deletes the entries below the point, never entry 0 or one at or past it, and the rows no retained
// base names once 30 days old; bounded passes resume where the last stopped.
func TestConfigLogRetentionDeletesOnlyPastThePoint(t *testing.T) {
	ctx := t.Context()
	clock := sidecartest.NewFakeClock(sidecartest.DefaultClockStart())
	blobs := sidecartest.NewFakeBlobs(clock, sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	networkId := sidecar.NewId()
	other := sidecar.NewId()
	put := func(key string) {
		if _, err := cluster.PutIfNoneMatch(ctx, key, []byte(key), nil); err != nil {
			t.Fatal(err)
		}
	}
	for n := uint64(0); n <= 12; n += 1 {
		put(sidecar.ConfigEntryKey(networkId, n))
	}
	put(sidecar.ConfigEntryKey(other, 1))
	rowsSha := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	put(sidecar.ConfigRowsKey(networkId, rowsSha(1)))
	put(sidecar.ConfigRowsKey(networkId, rowsSha(2)))
	clock.Advance(31 * 24 * time.Hour)
	// rows written just now, as an append writes them ahead of its entry
	put(sidecar.ConfigRowsKey(networkId, rowsSha(3)))

	settings := DefaultConfigLogRetentionSettings()
	settings.DeleteMaxCount = 3
	settings.ListPageCount = 2
	retained := map[string]bool{string(rowsSha(2)): true}
	deletedCount := 0
	for pass := 0; ; pass += 1 {
		result, err := retainConfigLogBelow(ctx, cluster, networkId, 8, retained, clock.Now(), settings)
		if err != nil {
			t.Fatal(err)
		}
		deletedCount += result.DeletedCount
		if !result.More {
			break
		}
		if 10 < pass {
			t.Fatal("retention never finished")
		}
	}
	// entries 1..7 and the old unnamed rows
	connect.AssertEqual(t, deletedCount, 8)
	exists := func(key string) bool {
		_, err := cluster.Head(ctx, key)
		return err == nil
	}
	connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, 0)), true)
	for n := uint64(1); n < 8; n += 1 {
		connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, n)), false)
	}
	for n := uint64(8); n <= 12; n += 1 {
		connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(networkId, n)), true)
	}
	connect.AssertEqual(t, exists(sidecar.ConfigEntryKey(other, 1)), true)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(1))), false)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(2))), true)
	connect.AssertEqual(t, exists(sidecar.ConfigRowsKey(networkId, rowsSha(3))), true)

	// a point of 1 retires nothing
	result, err := retainConfigLogBelow(ctx, cluster, other, 1, nil, clock.Now(), settings)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.DeletedCount, 0)
}

// A network whose log holds only base 1 keeps everything: its retention point is 1.
func TestConfigLogRetentionKeepsASingleBase(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := world.PostMemberAdmit(suite); err != nil {
			t.Fatal(err)
		}
	}
	networkId := server.Id(world.NetworkId())
	result, err := sequencer.RetainNetwork(t.Context(), networkId, world.Clock().Now().Add(400*24*time.Hour), DefaultConfigLogRetentionSettings())
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, result.Point, uint64(1))
	connect.AssertEqual(t, result.DeletedCount, 0)
	read, err := sequencer.Read(t.Context(), networkId, 0)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, len(read.Entries), 4)
	connect.AssertEqual(t, read.SignedGenesis != nil, true)
}

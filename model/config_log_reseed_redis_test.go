package model

// §7.9's site-loss uploads in Redis {acl_<networkId>}r (config_log_reseed_redis.go; QD-724), on the private stack's
// Redis: each sidecar's newest upload in its own field, read alike by every api instance; the key's ttl and bounds;
// and the resumed marker, which only a base past a written adopted run sets. An api instance is a ConfigReseedUploads
// of its own, since one holds nothing in its process. Time past the ttl is the key's expiry moved to now, which is
// all a passing day does to it.

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar/protocol/bridge"

	"github.com/urnetwork/server"
)

// The hash as the scripts left it.
func testConfigReseedHash(t testing.TB, ctx context.Context, networkId server.Id) map[string]string {
	t.Helper()
	var fieldValues map[string]string
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		fieldValues, err = r.HGetAll(ctx, configReseedKey(networkId)).Result()
		server.Raise(err)
	})
	return fieldValues
}

// The hash's field names, sorted.
func testConfigReseedFields(t testing.TB, ctx context.Context, networkId server.Id) []string {
	t.Helper()
	fields := []string{}
	for field := range testConfigReseedHash(t, ctx, networkId) {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	return fields
}

// How long the key has left, negative for a key that is gone or never expires.
func testConfigReseedTtl(t testing.TB, ctx context.Context, networkId server.Id) time.Duration {
	t.Helper()
	var ttl time.Duration
	server.Redis(ctx, func(r server.RedisClient) {
		var err error
		ttl, err = r.TTL(ctx, configReseedKey(networkId)).Result()
		server.Raise(err)
	})
	return ttl
}

// Leaves the key ttl to live, as time passing does; a ttl of 0 is the key past its expiry.
func testConfigReseedExpireIn(t testing.TB, ctx context.Context, networkId server.Id, ttl time.Duration) {
	t.Helper()
	server.Redis(ctx, func(r server.RedisClient) {
		server.Raise(r.PExpire(ctx, configReseedKey(networkId), ttl).Err())
	})
}

func testConfigReseedList(t testing.TB, uploads *ConfigReseedUploads, ctx context.Context, networkId server.Id) *ConfigReseedHeld {
	t.Helper()
	held, err := uploads.List(ctx, networkId)
	if err != nil {
		t.Fatal(err)
	}
	return held
}

func testConfigReseedPut(t testing.TB, uploads *ConfigReseedUploads, ctx context.Context, networkId server.Id, upload *ConfigReseedUpload) bool {
	t.Helper()
	resumed, err := uploads.Put(ctx, networkId, upload)
	if err != nil {
		t.Fatal(err)
	}
	return resumed
}

func testConfigReseedSidecarIds(held *ConfigReseedHeld) []server.Id {
	sidecarIds := []server.Id{}
	for _, upload := range held.Uploads {
		sidecarIds = append(sidecarIds, upload.SidecarId)
	}
	return sidecarIds
}

// Uploads keep each sidecar's newest in its own field, in the order they came, bounded by count: a device that
// uploads again replaces its field and moves to the end, and past the bound the oldest leaves.
func TestConfigReseedUploadsKeepEachSidecarsNewest(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		settings := DefaultConfigReseedSettings()
		settings.MaxUploadCount = 2
		uploads := NewConfigReseedUploads(settings)
		networkId := server.NewId()
		first, second, third := server.NewId(), server.NewId(), server.NewId()
		chain := bytes.Repeat([]byte{7}, 32)
		testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: first, Pins: []*bridge.AuthPin{{Version: 1, Chain: chain}}})
		testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: second})
		testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: first, Pins: []*bridge.AuthPin{{Version: 2, Chain: chain}}})

		held := testConfigReseedList(t, uploads, ctx, networkId)
		connect.AssertEqual(t, testConfigReseedSidecarIds(held), []server.Id{second, first})
		connect.AssertEqual(t, len(held.Uploads[1].Pins), 1)
		connect.AssertEqual(t, held.Uploads[1].Pins[0].GetVersion(), uint64(2))
		connect.AssertEqual(t, bytes.Equal(held.Uploads[1].Pins[0].GetChain(), chain), true)
		// one field a sidecar, and the order
		expected := []string{configReseedFieldOrder, configReseedUploadField(first), configReseedUploadField(second)}
		slices.Sort(expected)
		connect.AssertEqual(t, testConfigReseedFields(t, ctx, networkId), expected)

		testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: third})
		held = testConfigReseedList(t, uploads, ctx, networkId)
		connect.AssertEqual(t, testConfigReseedSidecarIds(held), []server.Id{first, third})
		_, found := testConfigReseedHash(t, ctx, networkId)[configReseedUploadField(second)]
		connect.AssertEqual(t, found, false)

		// another network's key holds its own
		connect.AssertEqual(t, len(testConfigReseedList(t, uploads, ctx, server.NewId()).Uploads), 0)

		connect.AssertEqual(t, uploads.Clear(ctx, networkId), nil)
		connect.AssertEqual(t, len(testConfigReseedList(t, uploads, ctx, networkId).Uploads), 0)
		connect.AssertEqual(t, len(testConfigReseedHash(t, ctx, networkId)), 0)
	})
}

// Two members' uploads that reach two api instances adopt: a third instance reads both from the network's key and
// adopts the run through the tip, each upload verbatim as its device sent it.
func TestConfigReseedUploadsAdoptAcrossApiInstances(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newReseedWorld(t, true)
		owner, member := self.kept(self.ownerSidecarId), self.kept(self.memberSidecarId)
		firstApi, secondApi, thirdApi := NewConfigReseedUploads(nil), NewConfigReseedUploads(nil), NewConfigReseedUploads(nil)

		testConfigReseedPut(t, firstApi, self.ctx, self.networkId, owner)
		// one member's devices adopt nothing in a two-member network, on any instance
		_, err := AdoptConfigRun(self.tip, testConfigReseedList(t, secondApi, self.ctx, self.networkId).Uploads)
		if !errors.Is(err, ErrConfigReseedNoRun) {
			t.Fatalf("one member's upload: %v", err)
		}
		testConfigReseedPut(t, secondApi, self.ctx, self.networkId, member)

		held := testConfigReseedList(t, thirdApi, self.ctx, self.networkId)
		connect.AssertEqual(t, testConfigReseedSidecarIds(held), []server.Id{self.ownerSidecarId, self.memberSidecarId})
		for i, sent := range []*ConfigReseedUpload{owner, member} {
			connect.AssertEqual(t, bytes.Equal(held.Uploads[i].SignedGenesis, sent.SignedGenesis), true)
			connect.AssertEqual(t, slices.EqualFunc(held.Uploads[i].Entries, sent.Entries, bytes.Equal), true)
			connect.AssertEqual(t, slices.EqualFunc(held.Uploads[i].Rows, sent.Rows, bytes.Equal), true)
		}
		adoption, err := AdoptConfigRun(self.tip, held.Uploads)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())
		connect.AssertEqual(t, bytes.Equal(adoption.Tip.GetChain(), self.tip.GetChain()), true)
		connect.AssertEqual(t, adoption.StoppedAt, uint64(0))
	})
}

// The key lives the ttl after its last write: uploads past it are gone, on every instance, and nothing adopts from
// them; an upload before then gives the key its whole ttl again.
func TestConfigReseedUploadsAreGonePastTheTtl(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		self := newReseedWorld(t, true)
		connect.AssertEqual(t, DefaultConfigReseedSettings().UploadTtl, 24*time.Hour)
		uploads := NewConfigReseedUploads(nil)
		withinTtl := func() {
			t.Helper()
			ttl := testConfigReseedTtl(t, self.ctx, self.networkId)
			if ttl <= 24*time.Hour-time.Minute || 24*time.Hour < ttl {
				t.Fatalf("the key's ttl: %s", ttl)
			}
		}

		testConfigReseedPut(t, uploads, self.ctx, self.networkId, self.kept(self.ownerSidecarId))
		withinTtl()
		// an hour before its end, the second member's upload starts the ttl again
		testConfigReseedExpireIn(t, self.ctx, self.networkId, time.Hour)
		testConfigReseedPut(t, uploads, self.ctx, self.networkId, self.kept(self.memberSidecarId))
		withinTtl()
		connect.AssertEqual(t, len(testConfigReseedList(t, uploads, self.ctx, self.networkId).Uploads), 2)

		// a settings ttl is the key's
		short := DefaultConfigReseedSettings()
		short.UploadTtl = 2 * time.Hour
		testConfigReseedPut(t, NewConfigReseedUploads(short), self.ctx, self.networkId, self.kept(self.memberSidecarId))
		if ttl := testConfigReseedTtl(t, self.ctx, self.networkId); ttl <= 2*time.Hour-time.Minute || 2*time.Hour < ttl {
			t.Fatalf("the key's ttl under settings: %s", ttl)
		}

		// past the ttl
		testConfigReseedExpireIn(t, self.ctx, self.networkId, 0)
		held := testConfigReseedList(t, NewConfigReseedUploads(nil), self.ctx, self.networkId)
		connect.AssertEqual(t, len(held.Uploads), 0)
		connect.AssertEqual(t, len(testConfigReseedHash(t, self.ctx, self.networkId)), 0)
		_, err := AdoptConfigRun(self.tip, held.Uploads)
		if !errors.Is(err, ErrConfigReseedNoRun) {
			t.Fatalf("past the ttl: %v", err)
		}
		// the devices upload again, and adopt
		testConfigReseedPut(t, uploads, self.ctx, self.networkId, self.kept(self.ownerSidecarId))
		testConfigReseedPut(t, uploads, self.ctx, self.networkId, self.kept(self.memberSidecarId))
		adoption, err := AdoptConfigRun(self.tip, testConfigReseedList(t, uploads, self.ctx, self.networkId).Uploads)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, adoption.Tip.GetVersion(), self.tip.GetVersion())
	})
}

// An upload past its own byte bound is refused and kept nowhere; past the network's byte bound the oldest uploads
// leave, and the newest stays whatever its size.
func TestConfigReseedUploadsKeepToTheirByteBounds(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		networkId := server.NewId()
		upload := func(byteCount int) *ConfigReseedUpload {
			return &ConfigReseedUpload{SidecarId: server.NewId(), Entries: [][]byte{bytes.Repeat([]byte{1}, byteCount)}}
		}
		storedByteCount := func(upload *ConfigReseedUpload) int64 {
			t.Helper()
			uploadBytes, err := configReseedUploadBytes(upload)
			if err != nil {
				t.Fatal(err)
			}
			return int64(len(uploadBytes))
		}
		first, second, third, large := upload(100), upload(100), upload(100), upload(400)

		settings := DefaultConfigReseedSettings()
		settings.MaxUploadByteCount = storedByteCount(large)
		settings.MaxNetworkByteCount = 2*storedByteCount(first) + 10
		uploads := NewConfigReseedUploads(settings)

		_, err := uploads.Put(ctx, networkId, upload(401))
		if !errors.Is(err, ErrConfigReseedUploadTooLarge) {
			t.Fatalf("an upload past its bound: %v", err)
		}
		connect.AssertEqual(t, len(testConfigReseedHash(t, ctx, networkId)), 0)

		testConfigReseedPut(t, uploads, ctx, networkId, first)
		testConfigReseedPut(t, uploads, ctx, networkId, second)
		connect.AssertEqual(t, testConfigReseedSidecarIds(testConfigReseedList(t, uploads, ctx, networkId)), []server.Id{first.SidecarId, second.SidecarId})
		testConfigReseedPut(t, uploads, ctx, networkId, third)
		connect.AssertEqual(t, testConfigReseedSidecarIds(testConfigReseedList(t, uploads, ctx, networkId)), []server.Id{second.SidecarId, third.SidecarId})
		// one upload past the network's bound alone: it stays, and every older one leaves
		testConfigReseedPut(t, uploads, ctx, networkId, large)
		connect.AssertEqual(t, testConfigReseedSidecarIds(testConfigReseedList(t, uploads, ctx, networkId)), []server.Id{large.SidecarId})
		expected := []string{configReseedFieldOrder, configReseedUploadField(large.SidecarId)}
		slices.Sort(expected)
		connect.AssertEqual(t, testConfigReseedFields(t, ctx, networkId), expected)
	})
}

// The resumed marker is set by the first base past a written adopted run and by nothing else: no base where no run
// is written, no base of the run itself; the written position only rises; the marker keeps the first resume base's
// position, drops the uploads, and from then the network keeps none. Another network's marker is its own.
func TestConfigReseedMarkerIsSetByTheBasePastTheAdoptedRun(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		uploads := NewConfigReseedUploads(nil)
		networkId, otherNetworkId := server.NewId(), server.NewId()
		markResumed := func(networkId server.Id, n uint64) bool {
			t.Helper()
			resumed, err := uploads.MarkResumed(ctx, networkId, n)
			if err != nil {
				t.Fatal(err)
			}
			return resumed
		}
		isResumed := func(networkId server.Id) bool {
			t.Helper()
			resumed, err := uploads.Resumed(ctx, networkId)
			if err != nil {
				t.Fatal(err)
			}
			return resumed
		}

		// no adopted run is written: a base resumes nothing, and writes nothing
		connect.AssertEqual(t, markResumed(networkId, 5), false)
		connect.AssertEqual(t, isResumed(networkId), false)
		connect.AssertEqual(t, len(testConfigReseedHash(t, ctx, networkId)), 0)

		for _, sidecarId := range []server.Id{server.NewId(), server.NewId()} {
			connect.AssertEqual(t, testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: sidecarId}), false)
			testConfigReseedPut(t, uploads, ctx, otherNetworkId, &ConfigReseedUpload{SidecarId: sidecarId})
		}
		connect.AssertEqual(t, uploads.MarkWritten(ctx, networkId, 7), nil)
		connect.AssertEqual(t, uploads.MarkWritten(ctx, otherNetworkId, 7), nil)
		// a shorter run written again lowers nothing
		connect.AssertEqual(t, uploads.MarkWritten(ctx, networkId, 5), nil)
		held := testConfigReseedList(t, uploads, ctx, networkId)
		connect.AssertEqual(t, held.WrittenTip, uint64(7))
		connect.AssertEqual(t, held.Resumed, false)
		connect.AssertEqual(t, len(held.Uploads), 2)

		// a retry of one of the run's own bases, at or below its tip
		connect.AssertEqual(t, markResumed(networkId, 6), false)
		connect.AssertEqual(t, markResumed(networkId, 7), false)
		connect.AssertEqual(t, isResumed(networkId), false)

		// the resume base, at tip+1
		connect.AssertEqual(t, markResumed(networkId, 8), true)
		connect.AssertEqual(t, isResumed(networkId), true)
		expected := []string{configReseedFieldResumed, configReseedFieldWritten}
		connect.AssertEqual(t, testConfigReseedFields(t, ctx, networkId), expected)
		connect.AssertEqual(t, testConfigReseedHash(t, ctx, networkId)[configReseedFieldResumed], "8")
		// a later base keeps the first's position
		connect.AssertEqual(t, markResumed(networkId, 9), true)
		connect.AssertEqual(t, testConfigReseedHash(t, ctx, networkId)[configReseedFieldResumed], "8")

		// a resumed network keeps no upload
		connect.AssertEqual(t, testConfigReseedPut(t, uploads, ctx, networkId, &ConfigReseedUpload{SidecarId: server.NewId()}), true)
		held = testConfigReseedList(t, uploads, ctx, networkId)
		connect.AssertEqual(t, held.Resumed, true)
		connect.AssertEqual(t, len(held.Uploads), 0)
		connect.AssertEqual(t, testConfigReseedFields(t, ctx, networkId), expected)

		// the other network, its run written but no base past it, stays in re-seed mode with its uploads
		connect.AssertEqual(t, isResumed(otherNetworkId), false)
		connect.AssertEqual(t, len(testConfigReseedList(t, uploads, ctx, otherNetworkId).Uploads), 2)

		// a run discarded keeps its written position, so the base past it still resumes
		connect.AssertEqual(t, uploads.Clear(ctx, otherNetworkId), nil)
		connect.AssertEqual(t, testConfigReseedFields(t, ctx, otherNetworkId), []string{configReseedFieldWritten})
		connect.AssertEqual(t, markResumed(otherNetworkId, 8), true)
	})
}

// A read that finds the marker gives the key its whole ttl again, so a resumed network that exchanges tokens stays
// out of re-seed mode; a read that finds none touches nothing; and a marker past its ttl is gone, the network in
// re-seed mode again.
func TestConfigReseedMarkerLivesWhileItIsRead(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		uploads := NewConfigReseedUploads(nil)
		networkId := server.NewId()
		connect.AssertEqual(t, uploads.MarkWritten(ctx, networkId, 3), nil)

		// no marker: the read leaves the key's ttl as it is
		testConfigReseedExpireIn(t, ctx, networkId, time.Hour)
		resumed, err := ConfigReseedResumed(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, false)
		if ttl := testConfigReseedTtl(t, ctx, networkId); time.Hour < ttl {
			t.Fatalf("a read with no marker moved the ttl: %s", ttl)
		}

		resumed, err = uploads.MarkResumed(ctx, networkId, 4)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, true)
		testConfigReseedExpireIn(t, ctx, networkId, time.Hour)
		resumed, err = ConfigReseedResumed(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, true)
		if ttl := testConfigReseedTtl(t, ctx, networkId); ttl <= 24*time.Hour-time.Minute || 24*time.Hour < ttl {
			t.Fatalf("the key's ttl after a read of its marker: %s", ttl)
		}

		// unread for the whole ttl
		testConfigReseedExpireIn(t, ctx, networkId, 0)
		resumed, err = ConfigReseedResumed(ctx, networkId)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, false)
		// and a network no key was ever written for
		resumed, err = ConfigReseedResumed(ctx, server.NewId())
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, resumed, false)
	})
}

// The sequencer's hook sets the marker only while sidecar.yml's reseed is true, the switch that allows re-seed mode
// at all.
func TestConfigReseedBaseLandedMarksOnlyWhileReseedIsOn(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		uploads := NewConfigReseedUploads(nil)
		networkId := server.NewId()
		baseLanded := NewConfigReseedBaseLanded(nil)
		connect.AssertEqual(t, uploads.MarkWritten(ctx, networkId, 3), nil)

		baseLanded(ctx, networkId, &bridge.AuthPin{Version: 4})
		connect.AssertEqual(t, testConfigReseedList(t, uploads, ctx, networkId).Resumed, false)

		t.Cleanup(server.Testing_PushSidecarConfig("sidecar:\n  reseed: true\n"))
		// a base of the adopted run
		baseLanded(ctx, networkId, &bridge.AuthPin{Version: 3})
		connect.AssertEqual(t, testConfigReseedList(t, uploads, ctx, networkId).Resumed, false)
		baseLanded(ctx, networkId, &bridge.AuthPin{Version: 4})
		connect.AssertEqual(t, testConfigReseedList(t, uploads, ctx, networkId).Resumed, true)
	})
}

// An upload's stored form gives back what its device sent, and bytes it never wrote are no upload: List skips a
// field holding them, so they count for nothing.
func TestConfigReseedUploadBytesRoundTrip(t *testing.T) {
	sidecarId := server.NewId()
	upload := &ConfigReseedUpload{
		SidecarId:     sidecarId,
		SignedGenesis: []byte("genesis"),
		Entries:       [][]byte{[]byte("entry 1"), {}, []byte("entry 3")},
		Rows:          [][]byte{[]byte("rows")},
		Pins:          []*bridge.AuthPin{{Version: 9, Chain: bytes.Repeat([]byte{3}, 32)}, {Version: 10, Chain: bytes.Repeat([]byte{4}, 32)}},
	}
	uploadBytes, err := configReseedUploadBytes(upload)
	connect.AssertEqual(t, err, nil)
	parsed, err := parseConfigReseedUpload(sidecarId, uploadBytes)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.SidecarId, sidecarId)
	connect.AssertEqual(t, bytes.Equal(parsed.SignedGenesis, upload.SignedGenesis), true)
	connect.AssertEqual(t, slices.EqualFunc(parsed.Entries, upload.Entries, bytes.Equal), true)
	connect.AssertEqual(t, slices.EqualFunc(parsed.Rows, upload.Rows, bytes.Equal), true)
	connect.AssertEqual(t, slices.EqualFunc(parsed.Pins, upload.Pins, func(a *bridge.AuthPin, b *bridge.AuthPin) bool { return proto.Equal(a, b) }), true)

	// pins alone: no genesis
	pinsOnly, err := configReseedUploadBytes(&ConfigReseedUpload{SidecarId: sidecarId, Pins: upload.Pins})
	connect.AssertEqual(t, err, nil)
	parsed, err = parseConfigReseedUpload(sidecarId, pinsOnly)
	connect.AssertEqual(t, err, nil)
	connect.AssertEqual(t, parsed.SignedGenesis == nil, true)
	connect.AssertEqual(t, len(parsed.Pins), 2)

	field := func(number protowire.Number, value []byte) []byte {
		return protowire.AppendBytes(protowire.AppendTag(nil, number, protowire.BytesType), value)
	}
	for name, malformed := range map[string][]byte{
		"a field it does not write": field(5, []byte("x")),
		"a varint field":            protowire.AppendVarint(protowire.AppendTag(nil, configReseedUploadFieldEntry, protowire.VarintType), 1),
		"a cut entry":               uploadBytes[:len(uploadBytes)-1],
		"two geneses":               slices.Concat(field(configReseedUploadFieldGenesis, []byte("a")), field(configReseedUploadFieldGenesis, []byte("b"))),
		"a pin that is no AuthPin":  field(configReseedUploadFieldPin, []byte{0xff}),
		"a cut tag":                 {0x80},
	} {
		if _, err := parseConfigReseedUpload(sidecarId, malformed); err == nil {
			t.Errorf("%s parsed", name)
		}
	}

	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := t.Context()
		uploads := NewConfigReseedUploads(nil)
		networkId := server.NewId()
		testConfigReseedPut(t, uploads, ctx, networkId, upload)
		other := &ConfigReseedUpload{SidecarId: server.NewId(), Entries: [][]byte{[]byte("entry")}}
		testConfigReseedPut(t, uploads, ctx, networkId, other)
		server.Redis(ctx, func(r server.RedisClient) {
			server.Raise(r.HSet(ctx, configReseedKey(networkId), configReseedUploadField(sidecarId), []byte{0x80}).Err())
		})
		connect.AssertEqual(t, testConfigReseedSidecarIds(testConfigReseedList(t, uploads, ctx, networkId)), []server.Id{other.SidecarId})
	})
}

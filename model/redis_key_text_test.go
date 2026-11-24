package model

import (
	"fmt"
	mathrand "math/rand/v2"
	"slices"
	"testing"

	"github.com/urnetwork/server"
)

// The Redis keys connect builds for every resident lookup, contract lease read
// and stream authorization are built without fmt (CPUOPT7.md §12). Each is
// byte for byte the key fmt built, and allocates less.

func redisKeyTextTestIds() []server.Id {
	ids := []server.Id{
		{},
		{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	}
	// deterministic so a failing id reproduces; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 7))
	for range 10000 {
		var id server.Id
		for i := range id {
			id[i] = byte(r.IntN(256))
		}
		ids = append(ids, id)
	}
	return ids
}

func TestResidentKeyTextIsFmtForm(t *testing.T) {
	for _, id := range redisKeyTextTestIds() {
		if got, want := residentKey(id), fmt.Sprintf("ncr_%s", id); got != want {
			t.Fatalf("residentKey(%s) = %q, want %q", id, got, want)
		}
	}
}

func TestContractHoleKeysTextIsFmtForm(t *testing.T) {
	ids := redisKeyTextTestIds()
	for i, sourceId := range ids {
		for _, destinationId := range []server.Id{ids[(i+1)%len(ids)], sourceId} {
			lower, higher := sourceId, destinationId
			if 0 < lower.Cmp(higher) {
				lower, higher = higher, lower
			}
			prefix := fmt.Sprintf("contract-hole:v2:{%s:%s}:", lower, higher)
			want := []string{prefix + "count", prefix + "members", prefix + "closed", prefix + "publication", prefix + "oversized"}
			if got := contractHoleKeys(sourceId, destinationId); !slices.Equal(got, want) {
				t.Fatalf("contractHoleKeys(%s, %s) = %q, want %q", sourceId, destinationId, got, want)
			}
			if got := contractHoleKeys(destinationId, sourceId); !slices.Equal(got, want) {
				t.Fatalf("contractHoleKeys(%s, %s) = %q, want %q", destinationId, sourceId, got, want)
			}
		}
	}
}

func TestStreamAuthorityKeyTextIsFmtForm(t *testing.T) {
	for _, id := range redisKeyTextTestIds() {
		for _, suffix := range []string{"", "binding", "e_" + id.String()} {
			if got, want := streamAuthorityKey(id, suffix), fmt.Sprintf("{sa_%s}%s", id, suffix); got != want {
				t.Fatalf("streamAuthorityKey(%s, %q) = %q, want %q", id, suffix, got, want)
			}
		}
	}
}

// Without fmt a key costs its id texts and its own string: no boxed id, and
// none of fmt's work.
func TestRedisKeyTextAllocations(t *testing.T) {
	sourceId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	destinationId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f9")
	suffix := "binding"
	var key string
	var keys []string
	for _, c := range []struct {
		name string
		f    func()
		want float64
	}{
		// the id's text and the key
		{name: "residentKey", f: func() { key = residentKey(sourceId) }, want: 2},
		// the two ids' text, the prefix, the five keys and their slice
		{name: "contractHoleKeys", f: func() { keys = contractHoleKeys(sourceId, destinationId) }, want: 9},
		// the id's text and the key
		{name: "streamAuthorityKey", f: func() { key = streamAuthorityKey(sourceId, suffix) }, want: 2},
	} {
		if allocations := testing.AllocsPerRun(1000, c.f); allocations != c.want {
			t.Errorf("%s allocates %.1f times, want %.0f", c.name, allocations, c.want)
		}
	}
	if key == "" || len(keys) != 5 {
		t.Fatal("no keys")
	}
}

func BenchmarkContractHoleKeys(b *testing.B) {
	sourceId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	destinationId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f9")
	b.ReportAllocs()
	for b.Loop() {
		_ = contractHoleKeys(sourceId, destinationId)
	}
}

func BenchmarkResidentKey(b *testing.B) {
	clientId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	b.ReportAllocs()
	for b.Loop() {
		_ = residentKey(clientId)
	}
}

func BenchmarkStreamAuthorityKey(b *testing.B) {
	streamId := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	b.ReportAllocs()
	for b.Loop() {
		_ = streamAuthorityKey(streamId, "binding")
	}
}

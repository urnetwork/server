package main

import (
	"bytes"
	"net"
	"net/netip"
	"reflect"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

func TestSubscriberRecordCacheEvictionPreservesRealMMDBEvidence(t *testing.T) {
	writer, err := mmdbwriter.New(mmdbwriter.Options{IPVersion: 4, DatabaseType: "synthetic immutable evidence", IncludeReservedNetworks: true, RecordSize: 32})
	if err != nil {
		t.Fatal(err)
	}
	address := func(n int) netip.Addr {
		return netip.AddrFrom4([4]byte{198, 18 + byte(n>>16), byte(n >> 8), byte(n)})
	}
	record := func(n int) mmdbtype.Map {
		state := "subscriber"
		if n%2 != 0 {
			state = "excluded"
		}
		value := subscriberFixtureRecord(state, n%3 == 0)
		value["distinct_source_record"] = mmdbtype.Uint32(n)
		value["source_evidence"] = mmdbtype.Slice{mmdbtype.Map{"sequence": mmdbtype.Uint32(n)}}
		return value
	}
	for n := 0; n <= subscriberRecordCacheLimit; n++ {
		if err := writer.Insert(&net.IPNet{IP: net.IP(address(n).AsSlice()), Mask: net.CIDRMask(32, 32)}, record(n)); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if _, err := writer.WriteTo(&output); err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.OpenBytes(output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cache := subscriberRecordCache{}
	for n := 0; n <= subscriberRecordCacheLimit; n++ {
		got, err := cache.decode(db.Lookup(address(n)))
		if err != nil || !reflect.DeepEqual(got, record(n)) {
			t.Fatal("decoded evidence differs at real MMDB record", n, err)
		}
	}
	if len(cache.records) != subscriberRecordCacheLimit || cache.misses != subscriberRecordCacheLimit+1 || cache.hits != 0 {
		t.Fatal("distinct records exceeded the cache bound or lost accounting")
	}
	misses := cache.misses
	got, err := cache.decode(db.Lookup(address(0)))
	if err != nil || !reflect.DeepEqual(got, record(0)) || cache.misses != misses+1 {
		t.Fatal("evicted record was not redecoded with exact subscriber/risk evidence", err)
	}
	// A failed lookup can carry an otherwise cached offset; neither lookup nor
	// decode failure may turn into a cached classification or a successful miss.
	for _, bad := range []mmdb.Result{
		db.Lookup(netip.MustParseAddr("2001:db8::1")),
		db.LookupOffset(uintptr(output.Len() + 1024)),
	} {
		hits, misses, size := cache.hits, cache.misses, len(cache.records)
		if _, err := cache.decode(bad); err == nil || cache.hits != hits || cache.misses != misses || len(cache.records) != size {
			t.Fatal("invalid reader result was cached as classification evidence")
		}
	}
	missing := db.Lookup(netip.MustParseAddr("203.0.113.1"))
	for range 2 {
		got, err := cache.decode(missing)
		if err != nil || got != nil {
			t.Fatal("missing record inherited cached evidence", err)
		}
	}
}

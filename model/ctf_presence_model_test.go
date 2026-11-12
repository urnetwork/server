package model

import (
	"bytes"
	"slices"
	"testing"
	"time"

	"github.com/urnetwork/server"
)

// The set's layout is PostgreSQL's set_bit and get_bit on bytea: bit n is the
// bit n%8 from the least significant of byte n/8.
func TestCtfSampleSetLayoutIsPostgresBitOrder(t *testing.T) {
	var set CtfSampleSet
	set.Set(0)
	set.Set(15)
	set.Set(671)
	if set[0] != 0x01 || set[1] != 0x80 || set[83] != 0x80 {
		t.Fatalf("sample set bytes %x %x %x, want 01 80 80", set[0], set[1], set[83])
	}
	if len(set) != 84 {
		t.Fatalf("sample set holds %d bytes, want the 84 of ctf_presence.samples", len(set))
	}
	if !set.Has(15) || set.Has(14) || set.Has(-1) || set.Has(672) {
		t.Fatal("sample set membership is wrong")
	}
	if set.Count() != 3 || !slices.Equal(set.Indexes(), []int{0, 15, 671}) {
		t.Fatalf("sample set counts %d %v", set.Count(), set.Indexes())
	}
	var excused CtfSampleSet
	excused.Set(15)
	excused.Set(16)
	if set.CountExcept(&excused) != 2 {
		t.Fatalf("count except excused = %d, want 2", set.CountExcept(&excused))
	}
	defer func() {
		if recover() == nil {
			t.Fatal("setting sample 672 did not panic")
		}
	}()
	set.Set(672)
}

// Mainnet epochs are 50,400 blocks: every sample covers exactly 75 blocks, and
// the epoch's first and last blocks fall in its first and last samples.
func TestCtfSampleIndexIsBlockPlaced(t *testing.T) {
	const start = 9_549_628
	const blocks = 50_400
	for _, c := range []struct {
		block uint64
		index int
		ok    bool
	}{
		{start, 0, true},
		{start + 74, 0, true},
		{start + 75, 1, true},
		{start + 75*336, 336, true},
		{start + blocks - 1, 671, true},
		{start + blocks, 0, false},
		{start - 1, 0, false},
	} {
		index, ok := CtfSampleIndex(start, blocks, c.block)
		if index != c.index || ok != c.ok {
			t.Errorf("block %d: sample %d %t, want %d %t", c.block, index, ok, c.index, c.ok)
		}
	}
	for i := range CtfSamplesPerEpoch {
		first := CtfSampleFirstBlock(start, blocks, i)
		if first != start+75*uint64(i) {
			t.Fatalf("sample %d starts at %d, want %d", i, first, start+75*uint64(i))
		}
		if index, ok := CtfSampleIndex(start, blocks, first); !ok || index != i {
			t.Fatalf("sample %d's first block is in sample %d", i, index)
		}
	}
	if _, ok := CtfSampleIndex(start, 0, start); ok {
		t.Fatal("an epoch of no blocks has a sample")
	}
	// a short testnet epoch places every block in one sample and leaves some
	// samples without a block, which are never taken
	seen := map[int]bool{}
	for block := uint64(100); block < 100+360; block++ {
		index, ok := CtfSampleIndex(100, 360, block)
		if !ok || index < 0 || CtfSamplesPerEpoch <= index {
			t.Fatalf("block %d of a 360-block epoch is in sample %d", block, index)
		}
		seen[index] = true
	}
	if len(seen) != 360 {
		t.Fatalf("a 360-block epoch fills %d samples, want 360", len(seen))
	}
}

// A testnet epoch shorter than 672 blocks leaves samples that cover no block;
// a mainnet epoch leaves none.
func TestCtfUncoveredSamples(t *testing.T) {
	for blocks, want := range map[uint64]int{50_400: 0, 672: 0, 673: 0, 671: 1, 360: 312, 300: 372} {
		uncovered := CtfUncoveredSamples(blocks)
		if uncovered.Count() != want {
			t.Errorf("an epoch of %d blocks leaves %d uncovered samples, want %d", blocks, uncovered.Count(), want)
		}
		covered := map[int]bool{}
		for block := uint64(0); block < blocks; block++ {
			index, _ := CtfSampleIndex(0, blocks, block)
			if uncovered.Has(index) {
				t.Fatalf("block %d of %d is in uncovered sample %d", block, blocks, index)
			}
			if CtfSampleFirstBlock(0, blocks, index) > block || block >= CtfSampleFirstBlock(0, blocks, index+1) {
				t.Fatalf("block %d of %d is outside its sample %d's blocks", block, blocks, index)
			}
			covered[index] = true
		}
		if len(covered)+uncovered.Count() != CtfSamplesPerEpoch {
			t.Fatalf("an epoch of %d blocks covers %d samples and leaves %d", blocks, len(covered), uncovered.Count())
		}
	}
}

// Half of the samples not excused, rounded up (§4.2, CtfPresence.required).
func TestCtfRequiredSamples(t *testing.T) {
	for excused, want := range map[int]int{0: 336, 1: 336, 2: 335, 3: 335, 671: 1, 672: 0} {
		if got := CtfRequiredSamples(excused); got != want {
			t.Errorf("required with %d excused = %d, want %d", excused, got, want)
		}
	}
}

func TestCtfNormalizeCountryCode(t *testing.T) {
	for in, want := range map[string]string{"IS": "is", " de ": "de", "xk": "xk", "": "zz", "usa": "zz", "1a": "zz", "é": "zz"} {
		if got := CtfNormalizeCountryCode(in); got != want {
			t.Errorf("normalize %q = %q, want %q", in, got, want)
		}
	}
}

// A sample is excused when its quarter hour overlaps a declared outage; the
// quarter hours spread the epoch's boundaries evenly.
func TestCtfExcusedSamples(t *testing.T) {
	start := time.Date(2026, 11, 27, 18, 29, 0, 0, time.UTC)
	end := start.Add(7 * 24 * time.Hour)
	outage := func(from time.Duration, to time.Duration) *CtfPresenceOutage {
		return &CtfPresenceOutage{StartTime: start.Add(from), EndTime: start.Add(to)}
	}
	excused := CtfExcusedSamples(start, end, []*CtfPresenceOutage{
		// exactly samples 4 and 5
		outage(60*time.Minute, 90*time.Minute),
		// inside sample 10
		outage(150*time.Minute+time.Minute, 150*time.Minute+2*time.Minute),
		// past the epoch's end, and before its start: nothing more
		outage(7*24*time.Hour, 8*24*time.Hour),
		outage(-time.Hour, 0),
	})
	if !slices.Equal(excused.Indexes(), []int{4, 5, 10}) {
		t.Fatalf("excused samples %v, want [4 5 10]", excused.Indexes())
	}
	all := CtfExcusedSamples(start, end, []*CtfPresenceOutage{outage(-time.Hour, 8*24*time.Hour)})
	if all.Count() != CtfSamplesPerEpoch {
		t.Fatalf("an outage over the whole epoch excuses %d samples", all.Count())
	}
	if none := CtfExcusedSamples(end, start, []*CtfPresenceOutage{outage(0, time.Hour)}); none.Count() != 0 {
		t.Fatal("an epoch without a span excuses samples")
	}
}

// The export's Online set crosses Redis byte for byte.
func TestCtfPresenceProvidersRoundTrip(t *testing.T) {
	providers := &CtfPresenceProviders{
		MeasuredAt: time.UnixMilli(1_795_000_000_123).UTC(),
		Providers: []*CtfPresenceProvider{
			{ClientId: server.NewId(), NetworkId: server.NewId(), CountryCode: "is", Public: true},
			{ClientId: server.NewId(), NetworkId: server.NewId(), CountryCode: "zz"},
		},
	}
	encoded := encodeCtfPresenceProviders(providers)
	if len(encoded) != 12+2*35 {
		t.Fatalf("encoded %d bytes", len(encoded))
	}
	decoded, err := decodeCtfPresenceProviders(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if !decoded.MeasuredAt.Equal(providers.MeasuredAt) || len(decoded.Providers) != 2 {
		t.Fatalf("decoded %v", decoded)
	}
	for i, provider := range providers.Providers {
		if *decoded.Providers[i] != *provider {
			t.Fatalf("provider %d decoded as %v, want %v", i, decoded.Providers[i], provider)
		}
	}
	for _, bad := range [][]byte{encoded[:11], encoded[:len(encoded)-1], append(bytes.Clone(encoded), 0)} {
		if _, err := decodeCtfPresenceProviders(bad); err == nil {
			t.Errorf("decoded %d bytes of a set", len(bad))
		}
	}
	corrupt := bytes.Clone(encoded)
	corrupt[len(corrupt)-2] = 'Z'
	if _, err := decodeCtfPresenceProviders(corrupt); err == nil {
		t.Error("decoded an uppercase country")
	}
	flagged := bytes.Clone(encoded)
	flagged[len(flagged)-1] = 0x02
	if _, err := decodeCtfPresenceProviders(flagged); err == nil {
		t.Error("decoded an unknown flag")
	}
}

// The Online set holds each Online provider once, with the country it is
// published under and whether it holds a Public key; providers that are not
// Online are left out.
func TestNewCtfPresenceProvidersTakesTheExportDecision(t *testing.T) {
	online, networkOnly, offline, noEgress := server.NewId(), server.NewId(), server.NewId(), server.NewId()
	networkId := server.NewId()
	city, country := server.NewId(), server.NewId()
	score := func(clientId server.Id, isOnline bool, isNetworkOnly bool) *ClientScore {
		return &ClientScore{ClientId: clientId, NetworkId: networkId, Online: isOnline, NetworkOnly: isNetworkOnly}
	}
	is, de := " IS", "de"
	providers := newCtfPresenceProviders(
		time.Unix(1_795_000_000, 0),
		map[server.Id]map[server.Id]*ClientScore{
			city:    {online: score(online, true, false), networkOnly: score(networkOnly, true, true)},
			country: {online: score(online, true, false), offline: score(offline, false, false), noEgress: score(noEgress, true, false)},
		},
		map[server.Id]*clientScoreEgress{
			online:      {publishedCountryCode: &is},
			networkOnly: {publishedCountryCode: &de},
			offline:     {publishedCountryCode: &de},
		},
	)
	got := map[server.Id]string{}
	public := map[server.Id]bool{}
	for _, provider := range providers.Providers {
		if provider.NetworkId != networkId {
			t.Fatalf("provider %s has network %s", provider.ClientId, provider.NetworkId)
		}
		got[provider.ClientId] = provider.CountryCode
		public[provider.ClientId] = provider.Public
	}
	if len(got) != 3 || got[online] != "is" || got[noEgress] != "zz" || got[networkOnly] != "de" {
		t.Fatalf("online set %v", got)
	}
	if !public[online] || !public[noEgress] || public[networkOnly] {
		t.Fatalf("public flags %v", public)
	}
	if !slices.IsSortedFunc(providers.Providers, func(a, b *CtfPresenceProvider) int { return a.ClientId.Cmp(b.ClientId) }) {
		t.Fatal("online set is not by client id")
	}
}

// A census turned on without its first epoch, or with a declared network that
// does not parse, stays off rather than run without the operator's networks.
func TestCtfDataConfigRefusesAnIncompleteCensus(t *testing.T) {
	networkId := server.NewId()
	config := newCtfDataConfig(&CtfDataConfig{Census: CtfCensusConfig{Enabled: true, FirstEpoch: 7, OwnNetworkIds: []string{" " + networkId.String() + " "}}})
	if !config.Census.Enabled || !slices.Equal(config.OwnNetworkIds(), []server.Id{networkId}) {
		t.Fatalf("census config %+v", config)
	}
	if config := newCtfDataConfig(&CtfDataConfig{Census: CtfCensusConfig{Enabled: true}}); config.Census.Enabled {
		t.Fatal("a census without its first epoch is on")
	}
	if config := newCtfDataConfig(&CtfDataConfig{Census: CtfCensusConfig{Enabled: true, FirstEpoch: 7, OwnNetworkIds: []string{networkId.String(), "fleet"}}}); config.Census.Enabled || len(config.OwnNetworkIds()) != 0 {
		t.Fatal("a census with an unreadable own network is on")
	}
	defaults := newCtfDataConfig(nil)
	if defaults.Presence.Enabled || defaults.Census.Enabled || defaults.PresenceRetentionEpochs() != 4 || defaults.ObjectRetentionEpochs() != 8 || defaults.MemberRetentionEpochs() != 60 {
		t.Fatalf("default config %+v", defaults)
	}
}

func TestCtfCensusMemberNearMiss(t *testing.T) {
	for failed, want := range map[uint32]bool{
		0:                            false,
		1 << CtfReasonPresence:       true,
		1 << CtfReasonCountryChanged: true,
		CtfNearMissReasons:           true,
		1<<CtfReasonPresence | 1<<CtfReasonNoWallet: false,
		1 << CtfReasonDuplicate:                     false,
	} {
		member := &CtfCensusMember{Failed: failed}
		if member.NearMiss() != want {
			t.Errorf("failed %b near miss %t, want %t", failed, member.NearMiss(), want)
		}
	}
}

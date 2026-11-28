package model

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/bits"
	mathrand "math/rand/v2"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server/v2026"
)

// A page as a reader before CPUOPT8 reads it: a plain gob decode.
func clientScorePageOldReaderDecode(data []byte) ([]*ClientScore, error) {
	var sample []*ClientScore
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&sample); err != nil {
		return nil, err
	}
	return sample, nil
}

// The value a gob encode and decode of sample gives, the value every typed
// decode must equal.
func clientScorePageGobRoundTrip(t testing.TB, sample []*ClientScore) []*ClientScore {
	t.Helper()
	decoded, err := clientScorePageOldReaderDecode(gobEncodeForTest(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// Whether a and b hold equal values, floats compared by their bits so that a
// NaN equals the same NaN. Shared and separate maps, and shared and separate
// pointers to equal values, are equal, as reflect.DeepEqual has them.
func clientScorePageTestEqual(a reflect.Value, b reflect.Value) bool {
	if a.Kind() != b.Kind() || a.Type() != b.Type() {
		return false
	}
	switch a.Kind() {
	case reflect.Float32, reflect.Float64:
		return math.Float64bits(a.Float()) == math.Float64bits(b.Float())
	case reflect.Pointer:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		return clientScorePageTestEqual(a.Elem(), b.Elem())
	case reflect.Map:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		if a.Len() != b.Len() {
			return false
		}
		for _, key := range a.MapKeys() {
			bValue := b.MapIndex(key)
			if !bValue.IsValid() || !clientScorePageTestEqual(a.MapIndex(key), bValue) {
				return false
			}
		}
		return true
	case reflect.Slice:
		if a.IsNil() || b.IsNil() {
			return a.IsNil() == b.IsNil()
		}
		fallthrough
	case reflect.Array:
		if a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !clientScorePageTestEqual(a.Index(i), b.Index(i)) {
				return false
			}
		}
		return true
	case reflect.Struct:
		for i := range a.NumField() {
			if !clientScorePageTestEqual(a.Field(i), b.Field(i)) {
				return false
			}
		}
		return true
	case reflect.String:
		return a.String() == b.String()
	case reflect.Bool:
		return a.Bool() == b.Bool()
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return a.Int() == b.Int()
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return a.Uint() == b.Uint()
	}
	panic(fmt.Sprintf("clientScorePageTestEqual: kind %s", a.Kind()))
}

// Fails unless the typed encode and decode of sample gives exactly the value
// the gob encode and decode gives.
func assertClientScorePageMatchesGob(t testing.TB, sample []*ClientScore) {
	t.Helper()
	want := clientScorePageGobRoundTrip(t, sample)
	page, err := encodeClientScorePageTyped(sample)
	if err != nil {
		t.Fatalf("typed encode: %v", err)
	}
	if !isTypedClientScorePage(page) {
		t.Fatal("a typed page is not detected as typed")
	}
	got, _, err := decodeClientScorePageTyped(page)
	if err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	if !clientScorePageTestEqual(reflect.ValueOf(want), reflect.ValueOf(got)) {
		t.Fatalf("the typed round trip differs from gob's:\ngob   %s\ntyped %s", clientScorePageTestString(want), clientScorePageTestString(got))
	}
	// and through the readers' entry point
	read, err := decodeClientScoreSample(page)
	if err != nil || !clientScorePageTestEqual(reflect.ValueOf(want), reflect.ValueOf(read)) {
		t.Fatalf("decodeClientScoreSample of a typed page differs from gob's: %v", err)
	}
}

func clientScorePageTestString(sample []*ClientScore) string {
	var b bytes.Buffer
	for _, score := range sample {
		fmt.Fprintf(&b, "%+v; ", *score)
	}
	return b.String()
}

// The exported fields of ClientScore, in the order of the typed format's field
// bits. A field added to ClientScore fails
// TestClientScorePageCodecCoversEveryField until it is added to the codec and
// here.
var clientScorePageTestFields = []string{
	"EgressValidUntil",
	"UrlProbeSuccessWeight",
	"ClientId",
	"NetworkId",
	"Scores",
	"ReliabilityWeight",
	"IndependentReliabilityWeight",
	"Tiers",
	"MinRelativeLatencyMillis",
	"MaxBytesPerSecond",
	"HasLatencyTest",
	"HasSpeedTest",
	"NetworkOnly",
	"ReputationFailedNames",
	"IpFamilies",
	"Online",
	"Modern",
	"CityLocationId",
	"RegionLocationId",
	"CountryLocationId",
	"LookbackIndex",
	"LookbackClientScores",
	"ScaledWeights",
	"PassesMinimums",
	"CapabilityDowngrades",
	"PostQuantumPositive",
	"PostQuantumValidUntil",
	"PostQuantumModernValidUntil",
}

// Every field gob encodes has a field bit, in the order the format writes.
func TestClientScorePageCodecCoversEveryField(t *testing.T) {
	exported := []string{}
	scoreType := reflect.TypeOf(ClientScore{})
	for i := range scoreType.NumField() {
		if scoreType.Field(i).IsExported() {
			exported = append(exported, scoreType.Field(i).Name)
		}
	}
	if !reflect.DeepEqual(exported, clientScorePageTestFields) {
		t.Fatalf("ClientScore's exported fields changed; add each new one to the typed page codec (network_client_score_page_codec.go) and to clientScorePageTestFields:\nfields %v\ncodec  %v", exported, clientScorePageTestFields)
	}
	if fieldBits := bits.OnesCount32(clientScorePageFields); fieldBits != len(clientScorePageTestFields) {
		t.Fatalf("the codec has %d field bits for %d fields", fieldBits, len(clientScorePageTestFields))
	}
}

// Times gob sends through MarshalBinary and the typed format must keep alike:
// UTC, the reader's local zone, fixed zones of minutes and of seconds (binary
// version 2), and a zero instant with a zone, which gob sends because it is
// not the zero value.
func clientScorePageTestTime(r *mathrand.Rand) time.Time {
	instant := time.Unix(r.Int64N(4_000_000_000)-1_000_000_000, r.Int64N(1_000_000_000))
	switch r.IntN(7) {
	case 0:
		return time.Time{}
	case 1:
		return instant.UTC()
	case 2:
		return instant.In(time.FixedZone("", -5*60*60))
	case 3:
		return instant.In(time.FixedZone("lmt", 9*60*60+18*60+59))
	case 4:
		return time.Time{}.In(time.FixedZone("", 30*60))
	case 5:
		return instant.Local()
	default:
		return instant.UTC().Round(time.Second)
	}
}

func clientScorePageTestFloat(r *mathrand.Rand) float64 {
	switch r.IntN(9) {
	case 0:
		return 0
	case 1:
		return math.Copysign(0, -1)
	case 2:
		return math.NaN()
	case 3:
		return math.Inf(1)
	case 4:
		return math.Inf(-1)
	case 5:
		return math.SmallestNonzeroFloat64
	default:
		return (r.Float64() - 0.25) * math.Pow(10, float64(r.IntN(12)-6))
	}
}

func clientScorePageTestInt(r *mathrand.Rand) int64 {
	switch r.IntN(7) {
	case 0:
		return 0
	case 1:
		return math.MinInt64
	case 2:
		return math.MaxInt64
	case 3:
		return -r.Int64N(1000)
	default:
		return r.Int64N(1 << uint(r.IntN(40)+1))
	}
}

var clientScorePageTestKeys = []string{RankModeQuality, RankModeSpeed, RankModeOnline, "", "é-ü", "a longer map key of forty bytes or more.."}

func clientScorePageTestText(r *mathrand.Rand) string {
	if r.IntN(3) == 0 {
		return ""
	}
	return clientScorePageTestKeys[r.IntN(len(clientScorePageTestKeys))] + fmt.Sprintf("%d", r.IntN(4))
}

// A score with every exported field drawn from r: nil and empty maps, zero
// and edge values, pointers to zero values, and lookbacks to depth 2. A field
// of a type the generator does not know fails the test, so a field added to
// ClientScore is generated, and checked against gob, before it is written.
func clientScorePageTestScore(t testing.TB, r *mathrand.Rand, depth int) *ClientScore {
	t.Helper()
	score := &ClientScore{}
	value := reflect.ValueOf(score).Elem()
	stringMapLength := func() int {
		switch r.IntN(4) {
		case 0:
			return -1
		case 1:
			return 0
		default:
			return 1 + r.IntN(len(clientScorePageTestKeys))
		}
	}
	for i := range value.NumField() {
		field := value.Type().Field(i)
		if !field.IsExported() {
			continue
		}
		target := value.Field(i)
		switch target.Interface().(type) {
		case *time.Time:
			switch r.IntN(4) {
			case 0:
			case 1:
				zero := time.Time{}
				target.Set(reflect.ValueOf(&zero))
			default:
				t := clientScorePageTestTime(r)
				target.Set(reflect.ValueOf(&t))
			}
		case time.Time:
			target.Set(reflect.ValueOf(clientScorePageTestTime(r)))
		case float64:
			target.SetFloat(clientScorePageTestFloat(r))
		case server.Id:
			if r.IntN(4) != 0 {
				target.Set(reflect.ValueOf(server.NewId()))
			}
		case *server.Id:
			switch r.IntN(3) {
			case 0:
			case 1:
				target.Set(reflect.ValueOf(&server.Id{}))
			default:
				id := server.NewId()
				target.Set(reflect.ValueOf(&id))
			}
		case int:
			target.SetInt(clientScorePageTestInt(r))
		case int64:
			target.SetInt(clientScorePageTestInt(r))
		case bool:
			target.SetBool(r.IntN(2) == 0)
		case string:
			target.SetString(clientScorePageTestText(r))
		case uint8:
			target.SetUint(uint64(r.IntN(256)))
		case map[string]int:
			if n := stringMapLength(); 0 <= n {
				m := map[string]int{}
				for _, key := range clientScorePageTestKeys[:n] {
					m[key] = int(clientScorePageTestInt(r))
				}
				target.Set(reflect.ValueOf(m))
			}
		case map[string]float32:
			if n := stringMapLength(); 0 <= n {
				m := map[string]float32{}
				for _, key := range clientScorePageTestKeys[:n] {
					m[key] = float32(clientScorePageTestFloat(r))
				}
				target.Set(reflect.ValueOf(m))
			}
		case map[string]bool:
			if n := stringMapLength(); 0 <= n {
				m := map[string]bool{}
				for _, key := range clientScorePageTestKeys[:n] {
					m[key] = r.IntN(2) == 0
				}
				target.Set(reflect.ValueOf(m))
			}
		case map[int]*ClientScore:
			switch {
			case 2 <= depth || r.IntN(4) == 0:
			case r.IntN(4) == 0:
				target.Set(reflect.ValueOf(map[int]*ClientScore{}))
			default:
				m := map[int]*ClientScore{}
				for range 1 + r.IntN(3) {
					m[int(clientScorePageTestInt(r)%1000)] = clientScorePageTestScore(t, r, depth+1)
				}
				target.Set(reflect.ValueOf(m))
			}
		default:
			t.Fatalf("ClientScore.%s of type %s has no generator; add it here and to the typed page codec", field.Name, field.Type)
		}
	}
	// unexported fields, which neither format writes
	if r.IntN(4) == 0 {
		score.minimumScores = map[string]int{RankModeQuality: 1}
		score.passesReliability = true
		score.countryFloor = true
		score.capabilityCopy = &ClientScore{ClientId: score.ClientId}
	}
	return score
}

// Random pages of every field's edge values decode exactly as gob's round
// trip does. The generator is seeded so a failure reproduces.
func TestClientScorePageCodecMatchesGob(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 1122))
	for range 3000 {
		sample := make([]*ClientScore, r.IntN(12))
		for i := range sample {
			sample[i] = clientScorePageTestScore(t, r, 0)
		}
		assertClientScorePageMatchesGob(t, sample)
	}
}

// The values each format must keep apart, one by one.
func TestClientScorePageCodecEdgeValues(t *testing.T) {
	zero := time.Time{}
	zoned := time.Time{}.In(time.FixedZone("", 3600))
	instant := time.Date(2026, 11, 22, 7, 40, 0, 123, time.UTC)
	signalingNan := math.Float32frombits(0x7f800001)
	for _, sample := range [][]*ClientScore{
		nil,
		{},
		{{}},
		// nil and empty maps
		{{Scores: map[string]int{}, Tiers: nil, ScaledWeights: map[string]float32{}, PassesMinimums: map[string]bool{}, LookbackClientScores: map[int]*ClientScore{}}},
		// a pointer to the zero time survives, a zero time field does not, and a
		// zoned zero instant is not the zero time
		{{EgressValidUntil: &zero, PostQuantumValidUntil: zoned}, {EgressValidUntil: &zoned, PostQuantumModernValidUntil: zero}, {EgressValidUntil: &instant}},
		// a pointer to the zero id survives
		{{CityLocationId: &server.Id{}, RegionLocationId: nil, CountryLocationId: &server.Id{1}}},
		// negative zero is zero, a NaN keeps its bits, and a float32 goes through float64
		{{UrlProbeSuccessWeight: math.Copysign(0, -1), ReliabilityWeight: math.NaN(), IndependentReliabilityWeight: math.Inf(-1)}},
		{{ScaledWeights: map[string]float32{"a": float32(math.Copysign(0, -1)), "b": float32(math.NaN()), "c": signalingNan, "d": math.MaxFloat32}}},
		// the int extremes and an empty key
		{{MinRelativeLatencyMillis: math.MinInt, MaxBytesPerSecond: math.MaxInt64, LookbackIndex: -1, Scores: map[string]int{"": math.MinInt}}},
		// lookbacks with negative and repeated shapes, nested twice
		{{LookbackClientScores: map[int]*ClientScore{-7: {LookbackIndex: -7}, 0: {}, 1: {LookbackClientScores: map[int]*ClientScore{2: {Online: true}}}}}},
		// every bool and byte
		{{HasLatencyTest: true, HasSpeedTest: true, NetworkOnly: true, Online: true, Modern: true, PostQuantumPositive: true, IpFamilies: 255, CapabilityDowngrades: 1, ReputationFailedNames: "vendor-a,vendor-b"}},
	} {
		assertClientScorePageMatchesGob(t, sample)
	}
}

// A page of realistic shape: providers with three lookbacks, the rank maps of
// two modes with few distinct values, the location ids of one country and the
// export's few validity instants, as the export writes them (measured on the
// largest native pages of 2026-11-22: about 650 gob bytes a provider). The
// ids are generated.
func clientScorePageTestRealisticSample(r *mathrand.Rand, n int) []*ClientScore {
	countryId := server.NewId()
	regionIds := make([]server.Id, 12)
	cityIds := make([]server.Id, 60)
	for i := range regionIds {
		regionIds[i] = server.NewId()
	}
	for i := range cityIds {
		cityIds[i] = server.NewId()
	}
	validUntils := []time.Time{
		time.Date(2026, 11, 22, 8, 10, 0, 0, time.UTC),
		time.Date(2026, 11, 22, 8, 15, 0, 0, time.UTC),
	}
	rank := func() (map[string]int, map[string]int) {
		scores, tiers := map[string]int{}, map[string]int{}
		for _, mode := range []RankMode{RankModeQuality, RankModeSpeed} {
			score := MaxClientScore
			if r.IntN(10) == 0 {
				score = 33 + r.IntN(17)
			}
			scores[mode] = score
			tiers[mode] = min(3, score/16)
		}
		return scores, tiers
	}
	sample := make([]*ClientScore, n)
	for i := range sample {
		clientId, networkId := server.NewId(), server.NewId()
		ipFamilies := uint8(ClientScoreIpFamilyV4)
		if r.IntN(5) == 0 {
			ipFamilies |= ClientScoreIpFamilyV6
		}
		validUntil := validUntils[r.IntN(len(validUntils))]
		cityId, regionId := cityIds[r.IntN(len(cityIds))], regionIds[r.IntN(len(regionIds))]
		scores, tiers := rank()
		score := &ClientScore{
			EgressValidUntil:             &validUntil,
			UrlProbeSuccessWeight:        0.5 + r.Float64()/2,
			ClientId:                     clientId,
			NetworkId:                    networkId,
			Scores:                       scores,
			ReliabilityWeight:            r.Float64(),
			IndependentReliabilityWeight: r.Float64(),
			Tiers:                        tiers,
			MinRelativeLatencyMillis:     r.IntN(290),
			MaxBytesPerSecond:            r.Int64N(5_600_000),
			HasLatencyTest:               true,
			HasSpeedTest:                 true,
			IpFamilies:                   ipFamilies,
			Online:                       true,
			CityLocationId:               &cityId,
			RegionLocationId:             &regionId,
			CountryLocationId:            &countryId,
			LookbackClientScores:         map[int]*ClientScore{},
			ScaledWeights: map[string]float32{
				RankModeQuality: float32(r.IntN(n)) / float32(n),
				RankModeSpeed:   float32(r.IntN(n)) / float32(n),
			},
			PassesMinimums: map[string]bool{RankModeQuality: true, RankModeSpeed: true},
		}
		if r.IntN(100) < 44 {
			score.Modern = true
			score.PostQuantumPositive = true
			score.PostQuantumValidUntil = validUntil.Add(-time.Hour)
			score.PostQuantumModernValidUntil = score.PostQuantumValidUntil
		}
		if r.IntN(100) < 28 {
			score.CapabilityDowngrades = uint8(1 + r.IntN(2))
		}
		for lookbackIndex := range 3 {
			lookbackScores, lookbackTiers := rank()
			score.LookbackClientScores[lookbackIndex] = &ClientScore{
				ClientId:                     clientId,
				NetworkId:                    networkId,
				Scores:                       lookbackScores,
				ReliabilityWeight:            r.Float64(),
				IndependentReliabilityWeight: r.Float64(),
				Tiers:                        lookbackTiers,
				MinRelativeLatencyMillis:     r.IntN(290),
				MaxBytesPerSecond:            r.Int64N(5_600_000),
				HasLatencyTest:               true,
				HasSpeedTest:                 true,
				IpFamilies:                   ipFamilies,
				LookbackIndex:                lookbackIndex,
			}
		}
		sample[i] = score
	}
	return sample
}

// A typed page shares a map between scores only when the two are equal, and
// never with another decode, so cached pages stay independent. A gob decode
// shares none.
func TestClientScorePageCodecSharesOnlyEqualMaps(t *testing.T) {
	sample := []*ClientScore{
		{Scores: map[string]int{RankModeQuality: 50, RankModeSpeed: 50}, Tiers: map[string]int{RankModeQuality: 3}},
		{Scores: map[string]int{RankModeQuality: 50, RankModeSpeed: 50}, Tiers: map[string]int{RankModeQuality: 2}},
		{Scores: map[string]int{RankModeSpeed: 50, RankModeQuality: 50}, Tiers: map[string]int{RankModeQuality: 3, RankModeSpeed: 3}},
	}
	page, err := encodeClientScorePageTyped(sample)
	if err != nil {
		t.Fatal(err)
	}
	decode := func() []*ClientScore {
		decoded, _, err := decodeClientScorePageTyped(page)
		if err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	same := func(a any, b any) bool {
		return reflect.ValueOf(a).UnsafePointer() == reflect.ValueOf(b).UnsafePointer()
	}
	first, second := decode(), decode()
	if !same(first[0].Scores, first[1].Scores) || !same(first[0].Scores, first[2].Scores) {
		t.Fatal("equal maps of one page are not shared")
	}
	if same(first[0].Tiers, first[1].Tiers) || same(first[0].Tiers, first[2].Tiers) || same(first[1].Tiers, first[2].Tiers) {
		t.Fatal("different maps are shared")
	}
	if same(first[0].Scores, second[0].Scores) {
		t.Fatal("two decodes share a map")
	}
	gobDecoded := clientScorePageGobRoundTrip(t, sample)
	if same(gobDecoded[0].Scores, gobDecoded[1].Scores) {
		t.Fatal("a gob decode shared a map")
	}
	assertClientScorePageMatchesGob(t, sample)
}

// TestClientScorePageCacheConcurrentReaders over typed pages: concurrent
// readers of the same and different pages, with evictions, each decode
// through the pooled decoders and get the page's scores, and what they do to
// their copies stays theirs though the page's equal maps are one map.
func TestClientScorePageCacheConcurrentReadersOfTypedPages(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 8))
	type page struct {
		data     []byte
		checksum [sha256.Size]byte
		want     []*ClientScore
	}
	pages := []page{}
	maxBytes := int64(-1)
	for range 3 {
		sample := clientScorePageTestRealisticSample(r, 40)
		data, err := encodeClientScorePageTyped(sample)
		if err != nil {
			t.Fatal(err)
		}
		want, size, err := decodeClientScorePageTyped(data)
		if err != nil {
			t.Fatal(err)
		}
		pages = append(pages, page{data: data, checksum: sha256.Sum256(data), want: want})
		maxBytes += size
	}
	// room for any two of the three pages, so readers evict and decode again
	cache := withClientScorePageCacheForTest(t, maxBytes-1)
	var wg sync.WaitGroup
	for reader := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				page := pages[(reader+i)%len(pages)]
				scores, err := decodeClientScorePage(page.data, page.checksum)
				if err != nil || len(scores) != len(page.want) {
					t.Errorf("read count=%d err=%v", len(scores), err)
					return
				}
				for j, score := range scores {
					want := page.want[j]
					if score.ClientId != want.ClientId || !reflect.DeepEqual(score.Scores, want.Scores) || !reflect.DeepEqual(score.LookbackClientScores, want.LookbackClientScores) {
						t.Error("a reader got another page's or a changed score")
						return
					}
					// what a reader may do to its copy: replace fields, and a
					// map with a changed clone
					score.Online = !score.Online
					scaledWeights := maps.Clone(score.ScaledWeights)
					scaledWeights[RankModeQuality] = 1
					score.ScaledWeights = scaledWeights
					score.PassesMinimums = nil
				}
			}
		}()
	}
	wg.Wait()
	if cached, bytes := cache.size(); 2 < cached || maxBytes < bytes {
		t.Fatalf("bound not kept: pages=%d bytes=%d", cached, bytes)
	}
	for _, page := range pages {
		fresh, _, err := decodeClientScorePageTyped(page.data)
		if err != nil || !reflect.DeepEqual(fresh, page.want) {
			t.Fatalf("a typed page no longer decodes to its scores: %v", err)
		}
	}
}

// The readers' decode of a gob page, as the export has always written it, is
// exactly the old reader's, through the page cache too.
func TestClientScorePageReaderReadsGobPages(t *testing.T) {
	withClientScorePageCacheForTest(t, clientScorePageCacheMaxBytes)
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 1))
	scores := map[server.Id]*ClientScore{}
	for _, score := range clientScorePageTestRealisticSample(r, 450) {
		scores[score.ClientId] = score
	}
	payload := exportClientScores(false, RankModeQuality, scores)
	pages := 0
	for _, facets := range []map[ipFamilyFacet]clientScoreFacetPayload{payload.facets, payload.nativeFacets} {
		for _, facetPayload := range facets {
			for i := range facetPayload.counts {
				data := facetPayload.encodeSample(i)
				if isTypedClientScorePage(data) {
					t.Fatal("the gob export wrote a typed page")
				}
				want, err := clientScorePageOldReaderDecode(data)
				if err != nil {
					t.Fatal(err)
				}
				got, err := decodeClientScoreSample(data)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("the reader's decode of a gob page differs: %v", err)
				}
				cached, err := decodeClientScorePageShared(data, sha256.Sum256(data))
				if err != nil || !reflect.DeepEqual(cached, want) {
					t.Fatalf("the cached decode of a gob page differs: %v", err)
				}
				pages += 1
			}
		}
	}
	if pages < 6 {
		t.Fatalf("only %d pages", pages)
	}
}

// A reader before CPUOPT8 fails a typed page with an error. It never reads
// one as other scores, which is why the export writes them only once every
// reader reads them.
func TestClientScorePageOldReaderFailsTypedPages(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 2))
	samples := [][]*ClientScore{nil, {{}}, clientScorePageTestRealisticSample(r, 1), clientScorePageTestRealisticSample(r, 200)}
	for range 200 {
		sample := make([]*ClientScore, r.IntN(6))
		for i := range sample {
			sample[i] = clientScorePageTestScore(t, r, 0)
		}
		samples = append(samples, sample)
	}
	for _, sample := range samples {
		page, err := encodeClientScorePageTyped(sample)
		if err != nil {
			t.Fatal(err)
		}
		if decoded, err := clientScorePageOldReaderDecode(page); err == nil {
			t.Fatalf("an old reader decoded a typed page as %d scores", len(decoded))
		}
	}
}

// The export writes gob until typed_score_pages is set: by default, in
// provider.yml's absence and with a value that cannot be read. Every page of
// every facet, union, native and capability, is then gob, which a reader
// before CPUOPT8 reads; set, every page is typed and reads as the same scores.
func TestClientScorePageExportWritesGobUnlessTypedScorePages(t *testing.T) {
	if DefaultEgressIndexSettings().TypedScorePages || DefaultEgressIndexSettings().scorePageFormat() != clientScorePageFormatGob {
		t.Fatal("the default settings write typed pages")
	}
	for _, c := range []struct {
		config string
		want   clientScorePageFormat
	}{
		{config: "", want: clientScorePageFormatGob},
		{config: "egress_index:\n  max_failure_index: 6\n", want: clientScorePageFormatGob},
		{config: "egress_index:\n  typed_score_pages: false\n", want: clientScorePageFormatGob},
		{config: "egress_index:\n  typed_score_pages: maybe\n", want: clientScorePageFormatGob},
		{config: "egress_index:\n  typed_score_pages: true\n", want: clientScorePageFormatTyped},
	} {
		pop := server.Config.PushSimpleResource(providerConfigResourceName, []byte(c.config))
		got := egressIndexSettings().scorePageFormat()
		pop()
		if got != c.want {
			t.Fatalf("%q: format %s, want %s", c.config, got, c.want)
		}
	}

	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 3))
	scores := map[server.Id]*ClientScore{}
	for _, score := range clientScorePageTestRealisticSample(r, 450) {
		scores[score.ClientId] = score
	}
	// a capability facet holds the classified copies
	for _, score := range scores {
		if score.CapabilityDowngrades != 0 {
			classified := *score
			classified.CapabilityDowngrades = ClientScoreCapabilityDowngradeUdp
			score.capabilityCopy = &classified
		}
	}
	// every page of a payload, by the facet it belongs to
	type labeledPage struct {
		label string
		data  []byte
	}
	pagesOf := func(payload clientScoreExportPayload) []labeledPage {
		pages := []labeledPage{}
		for i := range payload.counts {
			pages = append(pages, labeledPage{label: "union", data: payload.encodeSample(i)})
		}
		for kind, facets := range map[string]map[ipFamilyFacet]clientScoreFacetPayload{
			"facet":             payload.facets,
			"native":            payload.nativeFacets,
			"capability":        payload.capabilityFacets,
			"native capability": payload.nativeCapabilityFacets,
		} {
			for facet, facetPayload := range facets {
				for i := range facetPayload.counts {
					pages = append(pages, labeledPage{label: fmt.Sprintf("%s %s", kind, facet), data: facetPayload.encodeSample(i)})
				}
			}
		}
		return pages
	}
	for _, forceMinimum := range []bool{false, true} {
		gobCounter := testutil.ToFloat64(clientScorePageEncodesGob)
		gobPages := pagesOf(exportClientScoresInFormat(forceMinimum, RankModeQuality, scores, clientScorePageFormatGob))
		if len(gobPages) < 6 {
			t.Fatalf("only %d pages", len(gobPages))
		}
		if written := testutil.ToFloat64(clientScorePageEncodesGob) - gobCounter; int(written) != len(gobPages) {
			t.Fatalf("%d gob pages counted, %d written", int(written), len(gobPages))
		}
		gobScores := map[string]*ClientScore{}
		for _, page := range gobPages {
			if isTypedClientScorePage(page.data) {
				t.Fatal("the export wrote a typed page while typed_score_pages is off")
			}
			sample, err := clientScorePageOldReaderDecode(page.data)
			if err != nil {
				t.Fatalf("an old reader cannot read a page the export wrote with typed_score_pages off: %v", err)
			}
			for _, score := range sample {
				gobScores[fmt.Sprintf("%s %s", page.label, score.ClientId)] = score
			}
		}

		typedCounter := testutil.ToFloat64(clientScorePageEncodesTyped)
		typedPages := pagesOf(exportClientScoresInFormat(forceMinimum, RankModeQuality, scores, clientScorePageFormatTyped))
		if written := testutil.ToFloat64(clientScorePageEncodesTyped) - typedCounter; int(written) != len(typedPages) || len(typedPages) != len(gobPages) {
			t.Fatalf("%d typed pages counted, %d written, %d gob", int(written), len(typedPages), len(gobPages))
		}
		typedScores := 0
		for _, page := range typedPages {
			if !isTypedClientScorePage(page.data) {
				t.Fatal("the export wrote a gob page while typed_score_pages is on")
			}
			sample, err := decodeClientScoreSample(page.data)
			if err != nil {
				t.Fatal(err)
			}
			for _, score := range sample {
				if !reflect.DeepEqual(score, gobScores[fmt.Sprintf("%s %s", page.label, score.ClientId)]) {
					t.Fatalf("%s: a typed page reads as other scores than the gob page", page.label)
				}
				typedScores += 1
			}
		}
		if typedScores != len(gobScores) {
			t.Fatalf("the typed pages hold %d scores, the gob pages %d", typedScores, len(gobScores))
		}
	}
}

// A page the typed format cannot hold is written as gob, the bytes the gob
// export writes for it, and counted.
func TestClientScorePageTypedFallsBackToGob(t *testing.T) {
	for _, sample := range [][]*ClientScore{
		{{ClientId: server.NewId()}, nil},
		{{LookbackClientScores: map[int]*ClientScore{0: nil}}},
		{{EgressValidUntil: func() *time.Time {
			t := time.Date(2026, 1, 1, 0, 0, 0, 0, time.FixedZone("", 40000*60))
			return &t
		}()}},
	} {
		if _, err := encodeClientScorePageTyped(sample); !errors.Is(err, errClientScorePageUnencodable) {
			t.Fatalf("the typed encode took a page it cannot hold: %v", err)
		}
		fallbacks := testutil.ToFloat64(clientScorePageEncodesGobFallback)
		page := encodeClientScorePage(updateClientScoresPhaseMetrics, clientScorePageFormatTyped, sample)
		if testutil.ToFloat64(clientScorePageEncodesGobFallback)-fallbacks != 1 {
			t.Fatal("the fallback was not counted")
		}
		if isTypedClientScorePage(page) || !bytes.Equal(page, encodeClientScorePage(updateClientScoresPhaseMetrics, clientScorePageFormatGob, sample)) {
			t.Fatal("the fallback is not the gob page")
		}
	}
	// lookbacks past the depth bound
	deep := &ClientScore{}
	for range clientScorePageMaxDepth {
		deep = &ClientScore{LookbackClientScores: map[int]*ClientScore{0: deep}}
	}
	if _, err := encodeClientScorePageTyped([]*ClientScore{deep}); !errors.Is(err, errClientScorePageUnencodable) {
		t.Fatalf("the typed encode took lookbacks past its depth: %v", err)
	}
}

// A malformed typed page fails to decode, and never panics: every truncation,
// every single byte changed, another version, unknown field bits, counts past
// the page and trailing bytes.
func TestClientScorePageCodecRejectsMalformedPages(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 4))
	page, err := encodeClientScorePageTyped(clientScorePageTestRealisticSample(r, 3))
	if err != nil {
		t.Fatal(err)
	}
	decode := func(data []byte) (err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("decode panicked: %v", recovered)
			}
		}()
		_, _, err = decodeClientScorePageTyped(data)
		return err
	}
	if err := decode(page); err != nil {
		t.Fatal(err)
	}
	for n := range len(page) {
		if decode(page[:n]) == nil {
			t.Fatalf("a page truncated to %d of %d bytes decoded", n, len(page))
		}
	}
	for i := range len(page) {
		for _, b := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff, page[i] ^ 0x01} {
			changed := bytes.Clone(page)
			changed[i] = b
			decode(changed)
		}
	}
	other := bytes.Clone(page)
	other[1] = clientScorePageVersion + 1
	if !errors.Is(decode(other), errClientScorePageInvalid) {
		t.Fatal("another version decoded")
	}
	if decode(append(bytes.Clone(page), 0)) == nil {
		t.Fatal("trailing bytes decoded")
	}
	unknown := []byte{clientScorePageMagic, clientScorePageVersion, 1, 1, 0, 0, 0}
	unknown = binary.LittleEndian.AppendUint32(unknown, clientScorePageFields+1)
	if decode(unknown) == nil {
		t.Fatal("unknown field bits decoded")
	}
	huge := []byte{clientScorePageMagic, clientScorePageVersion}
	huge = binary.AppendUvarint(huge, 1<<40)
	if decode(huge) == nil {
		t.Fatal("a count past the page decoded")
	}
}

// Malformed pages never panic, and a page that decodes encodes back to the
// same scores.
func FuzzClientScorePageDecode(f *testing.F) {
	// deterministic seeds; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 5))
	for _, n := range []int{0, 1, 3} {
		page, err := encodeClientScorePageTyped(clientScorePageTestRealisticSample(r, n))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(page)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		sample, _, err := decodeClientScorePageTyped(data)
		if err != nil {
			return
		}
		page, err := encodeClientScorePageTyped(sample)
		if err != nil {
			t.Fatal(err)
		}
		again, _, err := decodeClientScorePageTyped(page)
		if err != nil || !clientScorePageTestEqual(reflect.ValueOf(sample), reflect.ValueOf(again)) {
			t.Fatalf("a decoded page does not encode back to its scores: %v", err)
		}
	})
}

// A realistic 200 provider page decodes in under 1,500 allocations, and in
// under a tenth of gob's (about 21 thousand).
func TestClientScorePageTypedDecodeAllocations(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 6))
	sample := clientScorePageTestRealisticSample(r, ClientScoreSampleCount)
	gobPage := gobEncodeForTest(t, sample)
	typedPage, err := encodeClientScorePageTyped(sample)
	if err != nil {
		t.Fatal(err)
	}
	gobAllocations := testing.AllocsPerRun(20, func() {
		if _, err := decodeClientScoreSample(gobPage); err != nil {
			t.Fatal(err)
		}
	})
	typedAllocations := testing.AllocsPerRun(20, func() {
		if _, err := decodeClientScoreSample(typedPage); err != nil {
			t.Fatal(err)
		}
	})
	if 1500 < typedAllocations || gobAllocations < 10*typedAllocations {
		t.Fatalf("a page decode allocates %.0f objects from gob and %.0f typed", gobAllocations, typedAllocations)
	}
}

// The heap a typed decode estimates for the page cache is the heap it
// retains, within a quarter, and less than a gob decode's estimate.
func TestClientScorePageTypedSizeEstimate(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 7))
	sample := clientScorePageTestRealisticSample(r, ClientScoreSampleCount)
	page, err := encodeClientScorePageTyped(sample)
	if err != nil {
		t.Fatal(err)
	}
	const decodes = 64
	retained := make([][]*ClientScore, decodes)
	estimate := int64(0)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := range retained {
		decoded, size, err := decodeClientScorePageTyped(page)
		if err != nil {
			t.Fatal(err)
		}
		retained[i] = decoded
		estimate += size
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	measured := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	runtime.KeepAlive(retained)
	if measured < estimate*3/4 || estimate*5/4 < measured {
		t.Fatalf("typed pages retain %d bytes, estimated %d", measured, estimate)
	}
	if gobEstimate := decodes * clientScorePageDecodedBytesPerByte * int64(len(gobEncodeForTest(t, sample))); gobEstimate < estimate {
		t.Fatalf("the typed estimate %d is above gob's %d", estimate, gobEstimate)
	}
}

func clientScorePageBenchmarkPages(b *testing.B) (sample []*ClientScore, gobPage []byte, typedPage []byte) {
	// deterministic for comparable runs; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(8, 1122))
	sample = clientScorePageTestRealisticSample(r, ClientScoreSampleCount)
	gobPage = gobEncodeForTest(b, sample)
	typedPage, err := encodeClientScorePageTyped(sample)
	if err != nil {
		b.Fatal(err)
	}
	return sample, gobPage, typedPage
}

// One page decode, as a page cache miss runs it.
func BenchmarkClientScorePageDecodeGob(b *testing.B) {
	_, gobPage, _ := clientScorePageBenchmarkPages(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeClientScoreSample(gobPage); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(gobPage)), "page-bytes")
}

func BenchmarkClientScorePageDecodeTyped(b *testing.B) {
	_, _, typedPage := clientScorePageBenchmarkPages(b)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := decodeClientScoreSample(typedPage); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(typedPage)), "page-bytes")
}

// One page encode, as the export runs it.
func BenchmarkClientScorePageEncodeGob(b *testing.B) {
	sample, _, _ := clientScorePageBenchmarkPages(b)
	b.ReportAllocs()
	for b.Loop() {
		encodeClientScorePage(updateClientScoresPhaseMetrics, clientScorePageFormatGob, sample)
	}
}

func BenchmarkClientScorePageEncodeTyped(b *testing.B) {
	sample, _, _ := clientScorePageBenchmarkPages(b)
	b.ReportAllocs()
	for b.Loop() {
		encodeClientScorePage(updateClientScoresPhaseMetrics, clientScorePageFormatTyped, sample)
	}
}

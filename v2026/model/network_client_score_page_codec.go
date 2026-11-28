// Score pages, the []*ClientScore samples the score export publishes
// (network_client_score_targets.go), in a typed binary format beside gob
// (CPUOPT8.md).
//
// A gob decode of a page reflects over every field and allocates each score,
// map, lookback score, id pointer and map key string on its own: a 200 record
// page with three lookbacks per provider is about 21 thousand objects. A typed
// page is read without reflection into four page-sized slabs: the scores with
// their lookback scores, the location ids, the validity instants, and one
// string holding the page's distinct map keys and names. Only the maps are
// allocated one by one, and equal maps of one page are one map
// (decodeClientScorePageStringMap): a page's rank maps hold a few distinct
// values. Decoders are pooled, so a decode allocates what it returns and a
// short key for each distinct map.
//
// Readers accept both formats. A typed page starts with clientScorePageMagic,
// a byte no gob stream starts with, then the format version; every other page
// is gob. A typed page decodes to exactly the value a gob encode and decode of
// the same scores gives (TestClientScorePageCodec*):
//   - nil and empty maps stay apart, as gob sends an empty map and omits a nil
//     one;
//   - a set time pointer decodes to its own pointer even to the zero time, as
//     gob omits only a nil one, while a zero time field stays zero, as gob
//     omits it;
//   - a set location id pointer decodes to its own pointer even to the zero id,
//     as gob sends arrays whole;
//   - a float keeps its bits, except that negative zero decodes as zero, as gob
//     omits a zero field; a float32 map value goes through float64 as gob's
//     does;
//   - a time goes through AppendBinary and UnmarshalBinary, as gob's does;
//   - unexported fields are not written;
//   - an empty page decodes to a nil slice.
//
// The slabs change no answer: a cached page is never written, and a reader
// copies each score it keeps (network_client_score_page_cache.go).
//
// The export writes typed pages only while EgressIndexSettings.TypedScorePages
// is set (`typed_score_pages` in the egress_index block of provider.yml), and
// falls back to gob for a page the format cannot hold: a nil score or lookback
// score, lookbacks nested past clientScorePageMaxDepth, or a time
// AppendBinary refuses. Gob refuses the first two as well. A reader that
// predates this file fails a typed page as undecodable, never as a wrong
// answer, so the flag waits until every reader runs this code (CPUOPT8.md,
// "Rollout").
//
// The format:
//
//	page   = magic version uvarint(records) uvarint(scores) uvarint(ids)
//	         uvarint(times) uvarint(strings) string* score{records}
//	string = uvarint(length) bytes
//	score  = uint32le(fields) value* , one value per set field, in bit order
//
// A field bit is set exactly when gob sends the field. Values: a time is
// uvarint(length) and AppendBinary's bytes; a float64 is its bits as uint64le;
// an int is a varint; an id is its 16 bytes; a string is uvarint(index into
// the page's strings); a uint8 is one byte; a bool field has no value, its bit
// is the value. A map is uvarint(entries) and its entries in key order: a
// string key is uvarint(index), an int value a varint, a float32 value its
// bits as uint32le, a bool value one byte, and a lookback map's int key a
// varint followed by the lookback score. scores counts the lookback scores
// too, ids the location id pointers and times the validity instant pointers.
package model

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

const (
	// The first byte of a typed page. No gob stream starts with it: a gob
	// stream opens with the byte length of its first message, a byte below
	// 0x80 or a negated byte count, 0xf8 to 0xff.
	clientScorePageMagic byte = 0xc5
	// The format the bytes after the magic hold. A reader refuses any other.
	clientScorePageVersion byte = 1
	// The deepest lookback nesting either side accepts, the top score at
	// depth zero. The export writes one level of lookbacks.
	clientScorePageMaxDepth = 8
)

// The field bits of a score, in the order of the values that follow them.
const (
	clientScorePageEgressValidUntil uint32 = 1 << iota
	clientScorePageUrlProbeSuccessWeight
	clientScorePageClientId
	clientScorePageNetworkId
	clientScorePageScores
	clientScorePageReliabilityWeight
	clientScorePageIndependentReliabilityWeight
	clientScorePageTiers
	clientScorePageMinRelativeLatencyMillis
	clientScorePageMaxBytesPerSecond
	clientScorePageHasLatencyTest
	clientScorePageHasSpeedTest
	clientScorePageNetworkOnly
	clientScorePageReputationFailedNames
	clientScorePageIpFamilies
	clientScorePageOnline
	clientScorePageModern
	clientScorePageCityLocationId
	clientScorePageRegionLocationId
	clientScorePageCountryLocationId
	clientScorePageLookbackIndex
	clientScorePageLookbackClientScores
	clientScorePageScaledWeights
	clientScorePagePassesMinimums
	clientScorePageCapabilityDowngrades
	clientScorePagePostQuantumPositive
	clientScorePagePostQuantumValidUntil
	clientScorePagePostQuantumModernValidUntil

	// every field bit of version 1
	clientScorePageFields = clientScorePagePostQuantumModernValidUntil<<1 - 1
)

// A typed page that is truncated, malformed, or of another version.
var errClientScorePageInvalid = errors.New("invalid typed client score page")

// A value the typed format cannot hold. The export writes it as gob.
var errClientScorePageUnencodable = errors.New("client score page cannot be typed")

// Whether data is a typed page, as opposed to gob.
func isTypedClientScorePage(data []byte) bool {
	return 0 < len(data) && data[0] == clientScorePageMagic
}

// The format the score export writes its pages in.
type clientScorePageFormat uint8

const (
	// what every reader reads, the default
	clientScorePageFormatGob clientScorePageFormat = iota
	// the typed format, which only readers from CPUOPT8 on read
	clientScorePageFormatTyped
)

func (self clientScorePageFormat) String() string {
	if self == clientScorePageFormatTyped {
		return "typed"
	}
	return "gob"
}

// Every reader counts its page decodes, cache misses only, by format, so a
// rollout can see the typed pages arrive and the gob pages leave.
var clientScorePageDecodes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_score_page_decodes_total",
	Help: "Client score page decodes by page format (gob or typed) and result (ok or error)",
}, []string{"format", "result"})

var clientScorePageDecodeCounters = [2][2]prometheus.Counter{
	{clientScorePageDecodes.WithLabelValues("gob", "error"), clientScorePageDecodes.WithLabelValues("gob", "ok")},
	{clientScorePageDecodes.WithLabelValues("typed", "error"), clientScorePageDecodes.WithLabelValues("typed", "ok")},
}

func clientScorePageDecodeCounter(typed bool, ok bool) prometheus.Counter {
	formatIndex, resultIndex := 0, 0
	if typed {
		formatIndex = 1
	}
	if ok {
		resultIndex = 1
	}
	return clientScorePageDecodeCounters[formatIndex][resultIndex]
}

// The export counts the pages it writes by format: gob, typed, or
// gob_fallback for a page the typed format cannot hold while typed is set.
var clientScorePageEncodes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_client_score_page_encodes_total",
	Help: "Client score pages the score export encoded by format: gob, typed, or gob_fallback for a page the typed format cannot hold",
}, []string{"format"})

var clientScorePageEncodesGob = clientScorePageEncodes.WithLabelValues("gob")
var clientScorePageEncodesTyped = clientScorePageEncodes.WithLabelValues("typed")
var clientScorePageEncodesGobFallback = clientScorePageEncodes.WithLabelValues("gob_fallback")

func init() {
	prometheus.MustRegister(clientScorePageDecodes, clientScorePageEncodes)
}

// The page of sample in format, measured as a gob_encode span like every
// other value the export encodes. A typed page the format cannot hold is
// written as gob, the bytes every reader reads, as before.
func encodeClientScorePage(metrics *updateClientScoresPhaseMetricSet, format clientScorePageFormat, sample []*ClientScore) []byte {
	if format != clientScorePageFormatTyped {
		clientScorePageEncodesGob.Inc()
		return encodeClientScoreGobValue(metrics, sample)
	}
	span := metrics.start(updateClientScoresPhaseGobEncode)
	page, err := encodeClientScorePageTyped(sample)
	if err != nil {
		span.finish()
		clientScorePageEncodesGobFallback.Inc()
		if clientScorePageFallbackLogged.CompareAndSwap(false, true) {
			glog.Errorf("[nclm]a client score page cannot be typed (%s); it and any other such page are written as gob\n", err)
		}
		return encodeClientScoreGobValue(metrics, sample)
	}
	metrics.addWork(updateClientScoresPhaseGobEncode, 1, len(page))
	span.finish()
	clientScorePageEncodesTyped.Inc()
	return page
}

// Whether this process has logged a page that fell back to gob. The counter
// counts every one.
var clientScorePageFallbackLogged atomic.Bool

// The reusable state of one page encode. The page it returns is new.
type clientScorePageEncoder struct {
	body          []byte
	stringIndexes map[string]uint64
	strings       []string
	scoreCount    int
	idCount       int
	timeCount     int
}

var clientScorePageEncoders = sync.Pool{
	New: func() any {
		return &clientScorePageEncoder{stringIndexes: map[string]uint64{}}
	},
}

// The scratch an encoder or decoder keeps between pages. A larger one is
// dropped rather than pinned by the pool.
const clientScorePageMaxRetainedBytes = 4 * 1024 * 1024

func (self *clientScorePageEncoder) release() {
	if clientScorePageMaxRetainedBytes < cap(self.body) {
		self.body = nil
	} else {
		self.body = self.body[:0]
	}
	if 1024 < len(self.stringIndexes) {
		self.stringIndexes = map[string]uint64{}
	} else {
		clear(self.stringIndexes)
	}
	clear(self.strings)
	self.strings = self.strings[:0]
	self.scoreCount, self.idCount, self.timeCount = 0, 0, 0
	clientScorePageEncoders.Put(self)
}

// The typed page of sample: the same scores in the same order.
func encodeClientScorePageTyped(sample []*ClientScore) ([]byte, error) {
	encoder := clientScorePageEncoders.Get().(*clientScorePageEncoder)
	defer encoder.release()
	for _, score := range sample {
		if err := encoder.appendScore(score, 0); err != nil {
			return nil, err
		}
	}
	size := 2 + 5*binary.MaxVarintLen64 + len(encoder.body)
	for _, s := range encoder.strings {
		size += binary.MaxVarintLen64 + len(s)
	}
	page := make([]byte, 0, size)
	page = append(page, clientScorePageMagic, clientScorePageVersion)
	page = binary.AppendUvarint(page, uint64(len(sample)))
	page = binary.AppendUvarint(page, uint64(encoder.scoreCount))
	page = binary.AppendUvarint(page, uint64(encoder.idCount))
	page = binary.AppendUvarint(page, uint64(encoder.timeCount))
	page = binary.AppendUvarint(page, uint64(len(encoder.strings)))
	for _, s := range encoder.strings {
		page = binary.AppendUvarint(page, uint64(len(s)))
		page = append(page, s...)
	}
	page = append(page, encoder.body...)
	return page, nil
}

// The index of s in the page's strings, added on first use.
func (self *clientScorePageEncoder) stringIndex(s string) uint64 {
	index, ok := self.stringIndexes[s]
	if !ok {
		index = uint64(len(self.strings))
		self.stringIndexes[s] = index
		self.strings = append(self.strings, s)
	}
	return index
}

func (self *clientScorePageEncoder) appendScore(score *ClientScore, depth int) error {
	if score == nil {
		return fmt.Errorf("%w: nil score", errClientScorePageUnencodable)
	}
	if clientScorePageMaxDepth <= depth {
		return fmt.Errorf("%w: lookbacks nested past %d", errClientScorePageUnencodable, clientScorePageMaxDepth)
	}
	self.scoreCount += 1
	fields := uint32(0)
	set := func(field uint32, ok bool) {
		if ok {
			fields |= field
		}
	}
	// gob reaches a time pointer's GobEncode without dereferencing it, so it
	// sends every set pointer, to the zero time too
	set(clientScorePageEgressValidUntil, score.EgressValidUntil != nil)
	set(clientScorePageUrlProbeSuccessWeight, score.UrlProbeSuccessWeight != 0)
	set(clientScorePageClientId, score.ClientId != (server.Id{}))
	set(clientScorePageNetworkId, score.NetworkId != (server.Id{}))
	set(clientScorePageScores, score.Scores != nil)
	set(clientScorePageReliabilityWeight, score.ReliabilityWeight != 0)
	set(clientScorePageIndependentReliabilityWeight, score.IndependentReliabilityWeight != 0)
	set(clientScorePageTiers, score.Tiers != nil)
	set(clientScorePageMinRelativeLatencyMillis, score.MinRelativeLatencyMillis != 0)
	set(clientScorePageMaxBytesPerSecond, score.MaxBytesPerSecond != 0)
	set(clientScorePageHasLatencyTest, score.HasLatencyTest)
	set(clientScorePageHasSpeedTest, score.HasSpeedTest)
	set(clientScorePageNetworkOnly, score.NetworkOnly)
	set(clientScorePageReputationFailedNames, score.ReputationFailedNames != "")
	set(clientScorePageIpFamilies, score.IpFamilies != 0)
	set(clientScorePageOnline, score.Online)
	set(clientScorePageModern, score.Modern)
	set(clientScorePageCityLocationId, score.CityLocationId != nil)
	set(clientScorePageRegionLocationId, score.RegionLocationId != nil)
	set(clientScorePageCountryLocationId, score.CountryLocationId != nil)
	set(clientScorePageLookbackIndex, score.LookbackIndex != 0)
	set(clientScorePageLookbackClientScores, score.LookbackClientScores != nil)
	set(clientScorePageScaledWeights, score.ScaledWeights != nil)
	set(clientScorePagePassesMinimums, score.PassesMinimums != nil)
	set(clientScorePageCapabilityDowngrades, score.CapabilityDowngrades != 0)
	set(clientScorePagePostQuantumPositive, score.PostQuantumPositive)
	set(clientScorePagePostQuantumValidUntil, score.PostQuantumValidUntil != (time.Time{}))
	set(clientScorePagePostQuantumModernValidUntil, score.PostQuantumModernValidUntil != (time.Time{}))

	self.body = binary.LittleEndian.AppendUint32(self.body, fields)
	var err error
	if fields&clientScorePageEgressValidUntil != 0 {
		if err = self.appendTime(*score.EgressValidUntil); err != nil {
			return err
		}
		self.timeCount += 1
	}
	if fields&clientScorePageUrlProbeSuccessWeight != 0 {
		self.body = binary.LittleEndian.AppendUint64(self.body, math.Float64bits(score.UrlProbeSuccessWeight))
	}
	if fields&clientScorePageClientId != 0 {
		self.body = append(self.body, score.ClientId[:]...)
	}
	if fields&clientScorePageNetworkId != 0 {
		self.body = append(self.body, score.NetworkId[:]...)
	}
	if fields&clientScorePageScores != 0 {
		appendClientScorePageStringMap(self, score.Scores, func(b []byte, value int) []byte {
			return binary.AppendVarint(b, int64(value))
		})
	}
	if fields&clientScorePageReliabilityWeight != 0 {
		self.body = binary.LittleEndian.AppendUint64(self.body, math.Float64bits(score.ReliabilityWeight))
	}
	if fields&clientScorePageIndependentReliabilityWeight != 0 {
		self.body = binary.LittleEndian.AppendUint64(self.body, math.Float64bits(score.IndependentReliabilityWeight))
	}
	if fields&clientScorePageTiers != 0 {
		appendClientScorePageStringMap(self, score.Tiers, func(b []byte, value int) []byte {
			return binary.AppendVarint(b, int64(value))
		})
	}
	if fields&clientScorePageMinRelativeLatencyMillis != 0 {
		self.body = binary.AppendVarint(self.body, int64(score.MinRelativeLatencyMillis))
	}
	if fields&clientScorePageMaxBytesPerSecond != 0 {
		self.body = binary.AppendVarint(self.body, score.MaxBytesPerSecond)
	}
	if fields&clientScorePageReputationFailedNames != 0 {
		self.body = binary.AppendUvarint(self.body, self.stringIndex(score.ReputationFailedNames))
	}
	if fields&clientScorePageIpFamilies != 0 {
		self.body = append(self.body, score.IpFamilies)
	}
	for _, locationId := range []struct {
		field uint32
		id    *server.Id
	}{
		{field: clientScorePageCityLocationId, id: score.CityLocationId},
		{field: clientScorePageRegionLocationId, id: score.RegionLocationId},
		{field: clientScorePageCountryLocationId, id: score.CountryLocationId},
	} {
		if fields&locationId.field != 0 {
			self.body = append(self.body, locationId.id[:]...)
			self.idCount += 1
		}
	}
	if fields&clientScorePageLookbackIndex != 0 {
		self.body = binary.AppendVarint(self.body, int64(score.LookbackIndex))
	}
	if fields&clientScorePageLookbackClientScores != 0 {
		var keyBuffer [8]int
		lookbackIndexes := keyBuffer[:0]
		for lookbackIndex := range score.LookbackClientScores {
			lookbackIndexes = append(lookbackIndexes, lookbackIndex)
		}
		slices.Sort(lookbackIndexes)
		self.body = binary.AppendUvarint(self.body, uint64(len(lookbackIndexes)))
		for _, lookbackIndex := range lookbackIndexes {
			self.body = binary.AppendVarint(self.body, int64(lookbackIndex))
			if err = self.appendScore(score.LookbackClientScores[lookbackIndex], depth+1); err != nil {
				return err
			}
		}
	}
	if fields&clientScorePageScaledWeights != 0 {
		appendClientScorePageStringMap(self, score.ScaledWeights, func(b []byte, value float32) []byte {
			return binary.LittleEndian.AppendUint32(b, math.Float32bits(value))
		})
	}
	if fields&clientScorePagePassesMinimums != 0 {
		appendClientScorePageStringMap(self, score.PassesMinimums, func(b []byte, value bool) []byte {
			if value {
				return append(b, 1)
			}
			return append(b, 0)
		})
	}
	if fields&clientScorePageCapabilityDowngrades != 0 {
		self.body = append(self.body, score.CapabilityDowngrades)
	}
	if fields&clientScorePagePostQuantumValidUntil != 0 {
		if err = self.appendTime(score.PostQuantumValidUntil); err != nil {
			return err
		}
	}
	if fields&clientScorePagePostQuantumModernValidUntil != 0 {
		if err = self.appendTime(score.PostQuantumModernValidUntil); err != nil {
			return err
		}
	}
	return nil
}

func (self *clientScorePageEncoder) appendTime(t time.Time) error {
	start := len(self.body)
	// the length, one byte: AppendBinary writes 15 or 16
	self.body = append(self.body, 0)
	body, err := t.AppendBinary(self.body)
	if err != nil {
		self.body = self.body[:start]
		return fmt.Errorf("%w: %w", errClientScorePageUnencodable, err)
	}
	self.body = body
	self.body[start] = byte(len(self.body) - start - 1)
	return nil
}

// Appends a string-keyed map's entries in key order, each value by
// appendValue.
func appendClientScorePageStringMap[V any](encoder *clientScorePageEncoder, m map[string]V, appendValue func([]byte, V) []byte) {
	var keyBuffer [8]string
	keys := keyBuffer[:0]
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	encoder.body = binary.AppendUvarint(encoder.body, uint64(len(keys)))
	for _, key := range keys {
		encoder.body = binary.AppendUvarint(encoder.body, encoder.stringIndex(key))
		encoder.body = appendValue(encoder.body, m[key])
	}
}

// The reusable state of one page decode. The scores, maps and strings a
// decode returns are its own; only the decoder's index of the page's strings
// is reused, and cleared before reuse.
type clientScorePageDecoder struct {
	data    []byte
	offset  int
	strings []string
	scores  []ClientScore
	ids     []server.Id
	times   []time.Time
	// the page's string-keyed maps by value type
	intMaps     clientScorePageMaps[int]
	float32Maps clientScorePageMaps[float32]
	boolMaps    clientScorePageMaps[bool]
	// the estimated heap of the maps decoded so far
	mapBytes int64
}

var clientScorePageDecoders = sync.Pool{
	New: func() any {
		return &clientScorePageDecoder{}
	},
}

func (self *clientScorePageDecoder) release() {
	clear(self.strings)
	if clientScorePageMaxRetainedBytes < cap(self.strings)*int(unsafe.Sizeof("")) {
		self.strings = nil
	} else {
		self.strings = self.strings[:0]
	}
	self.data, self.offset = nil, 0
	self.scores, self.ids, self.times = nil, nil, nil
	self.intMaps.release()
	self.float32Maps.release()
	self.boolMaps.release()
	self.mapBytes = 0
	clientScorePageDecoders.Put(self)
}

// The scores of a typed page, and the estimated heap they retain.
func decodeClientScorePageTyped(data []byte) (sample []*ClientScore, size int64, returnErr error) {
	if len(data) < 2 || data[0] != clientScorePageMagic {
		return nil, 0, fmt.Errorf("%w: not a typed page", errClientScorePageInvalid)
	}
	if data[1] != clientScorePageVersion {
		return nil, 0, fmt.Errorf("%w: version %d", errClientScorePageInvalid, data[1])
	}
	decoder := clientScorePageDecoders.Get().(*clientScorePageDecoder)
	defer decoder.release()
	decoder.data, decoder.offset = data, 2

	// Every count is bounded by the bytes left, so a malformed page cannot
	// make the slabs larger than its length allows: each score needs its
	// four bytes of fields, each id its 16 bytes, each time at least its
	// length byte and each string its length.
	recordCount, err := decoder.count(4)
	if err != nil {
		return nil, 0, err
	}
	scoreCount, err := decoder.count(4)
	if err != nil {
		return nil, 0, err
	}
	idCount, err := decoder.count(16)
	if err != nil {
		return nil, 0, err
	}
	timeCount, err := decoder.count(1)
	if err != nil {
		return nil, 0, err
	}
	stringCount, err := decoder.count(1)
	if err != nil {
		return nil, 0, err
	}
	if scoreCount < recordCount {
		return nil, 0, fmt.Errorf("%w: fewer scores than records", errClientScorePageInvalid)
	}

	// One string holds every string of the page, length prefixes included,
	// and the page's strings are its substrings.
	tableStart := decoder.offset
	for range stringCount {
		length, err := decoder.count(1)
		if err != nil {
			return nil, 0, err
		}
		decoder.offset += length
	}
	table := string(data[tableStart:decoder.offset])
	position := 0
	for range stringCount {
		// read and bounded once above
		length, n := binary.Uvarint(data[tableStart+position:])
		position += n
		decoder.strings = append(decoder.strings, table[position:position+int(length)])
		position += int(length)
	}

	scores := make([]ClientScore, scoreCount)
	ids := make([]server.Id, idCount)
	times := make([]time.Time, timeCount)
	decoder.scores, decoder.ids, decoder.times = scores, ids, times
	if 0 < recordCount {
		sample = make([]*ClientScore, recordCount)
	}
	for i := range sample {
		if sample[i], err = decoder.score(0); err != nil {
			return nil, 0, err
		}
	}
	if len(decoder.scores) != 0 || len(decoder.ids) != 0 || len(decoder.times) != 0 {
		return nil, 0, fmt.Errorf("%w: fewer values than declared", errClientScorePageInvalid)
	}
	if decoder.offset != len(data) {
		return nil, 0, fmt.Errorf("%w: trailing bytes", errClientScorePageInvalid)
	}
	size = int64(unsafe.Sizeof(ClientScore{}))*int64(scoreCount) +
		int64(unsafe.Sizeof((*ClientScore)(nil)))*int64(recordCount) +
		int64(unsafe.Sizeof(server.Id{}))*int64(idCount) +
		int64(unsafe.Sizeof(time.Time{}))*int64(timeCount) +
		int64(len(table)) +
		decoder.mapBytes
	return sample, size, nil
}

// A count no larger than the bytes left allow at unit bytes each.
func (self *clientScorePageDecoder) count(unit int) (int, error) {
	value, err := self.uvarint()
	if err != nil {
		return 0, err
	}
	if uint64(len(self.data)-self.offset)/uint64(unit) < value {
		return 0, fmt.Errorf("%w: count past the page", errClientScorePageInvalid)
	}
	return int(value), nil
}

func (self *clientScorePageDecoder) uvarint() (uint64, error) {
	value, n := binary.Uvarint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated", errClientScorePageInvalid)
	}
	self.offset += n
	return value, nil
}

func (self *clientScorePageDecoder) varint() (int64, error) {
	value, n := binary.Varint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated", errClientScorePageInvalid)
	}
	self.offset += n
	return value, nil
}

// An int that fits the platform int, as gob requires of an int field.
func (self *clientScorePageDecoder) int() (int, error) {
	value, err := self.varint()
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%w: int out of range", errClientScorePageInvalid)
	}
	return int(value), nil
}

// The next n bytes, borrowed from the page.
func (self *clientScorePageDecoder) bytes(n int) ([]byte, error) {
	if len(self.data)-self.offset < n {
		return nil, fmt.Errorf("%w: truncated", errClientScorePageInvalid)
	}
	b := self.data[self.offset : self.offset+n]
	self.offset += n
	return b, nil
}

func (self *clientScorePageDecoder) byte() (byte, error) {
	b, err := self.bytes(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (self *clientScorePageDecoder) float64() (float64, error) {
	b, err := self.bytes(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

func (self *clientScorePageDecoder) string() (string, error) {
	index, err := self.uvarint()
	if err != nil {
		return "", err
	}
	if uint64(len(self.strings)) <= index {
		return "", fmt.Errorf("%w: string index past the page", errClientScorePageInvalid)
	}
	return self.strings[index], nil
}

func (self *clientScorePageDecoder) id() (server.Id, error) {
	b, err := self.bytes(16)
	if err != nil {
		return server.Id{}, err
	}
	return server.Id(b), nil
}

// The next location id, in the page's id slab.
func (self *clientScorePageDecoder) idPointer() (*server.Id, error) {
	if len(self.ids) == 0 {
		return nil, fmt.Errorf("%w: more ids than declared", errClientScorePageInvalid)
	}
	id, err := self.id()
	if err != nil {
		return nil, err
	}
	pointer := &self.ids[0]
	self.ids = self.ids[1:]
	*pointer = id
	return pointer, nil
}

func (self *clientScorePageDecoder) time() (time.Time, error) {
	length, err := self.byte()
	if err != nil {
		return time.Time{}, err
	}
	b, err := self.bytes(int(length))
	if err != nil {
		return time.Time{}, err
	}
	var t time.Time
	if err := t.UnmarshalBinary(b); err != nil {
		return time.Time{}, fmt.Errorf("%w: %w", errClientScorePageInvalid, err)
	}
	return t, nil
}

// The estimated heap of a map of n entries with slotBytes per entry
// (internal/runtime/maps): its header, and the groups of eight slots and
// eight control bytes that hold the entries, one for up to eight entries and
// otherwise enough at seven per group. Allocations round up to 16 bytes.
func clientScorePageMapBytes(n int, slotBytes int) int64 {
	const headerBytes = 48
	const groupSlots = 8
	groups := 0
	switch {
	case n == 0:
	case n <= groupSlots:
		groups = 1
	default:
		groups = (n + groupSlots - 2) / (groupSlots - 1)
	}
	roundUp := func(b int) int64 {
		return int64((b + 15) &^ 15)
	}
	if groups == 0 {
		return roundUp(headerBytes)
	}
	return roundUp(headerBytes) + roundUp(groups*(groupSlots+groupSlots*slotBytes))
}

// One kind of string-keyed map a decoder reads: the entries of the map being
// read, reused, and the maps this page already decoded, by their encoding.
type clientScorePageMaps[V any] struct {
	entries []clientScorePageMapEntry[V]
	maps    map[string]map[string]V
}

type clientScorePageMapEntry[V any] struct {
	key   string
	value V
}

// Forgets the page's maps, keeping small tables for the next page.
func (self *clientScorePageMaps[V]) release() {
	clear(self.entries)
	self.entries = self.entries[:0]
	if 1024 < len(self.maps) {
		self.maps = nil
	} else {
		clear(self.maps)
	}
}

// A string-keyed map, each value read by readValue. A map whose encoding this
// page already held is that same map. Equal maps can be one because a decoded
// page is never written: the page cache shares it with every reader, and a
// reader that changes a score's map replaces it with a changed clone
// (network_client_score_page_cache.go). The export writes a map's entries in
// key order, so equal maps of one page have equal encodings.
func decodeClientScorePageStringMap[V any](decoder *clientScorePageDecoder, maps *clientScorePageMaps[V], readValue func() (V, error)) (map[string]V, error) {
	start := decoder.offset
	// each entry takes at least two bytes, its key and its value
	n, err := decoder.count(2)
	if err != nil {
		return nil, err
	}
	entries := maps.entries[:0]
	for range n {
		key, err := decoder.string()
		if err != nil {
			return nil, err
		}
		value, err := readValue()
		if err != nil {
			return nil, err
		}
		entries = append(entries, clientScorePageMapEntry[V]{key: key, value: value})
	}
	maps.entries = entries
	encoding := decoder.data[start:decoder.offset]
	if m, ok := maps.maps[string(encoding)]; ok {
		return m, nil
	}
	m := make(map[string]V, n)
	for _, entry := range entries {
		m[entry.key] = entry.value
	}
	if maps.maps == nil {
		maps.maps = map[string]map[string]V{}
	}
	maps.maps[string(encoding)] = m
	decoder.mapBytes += clientScorePageMapBytes(n, int(unsafe.Sizeof(clientScorePageMapEntry[V]{})))
	return m, nil
}

// A float32 map value, through float64 as a gob decode converts a float32.
func (self *clientScorePageDecoder) float32() (float32, error) {
	b, err := self.bytes(4)
	if err != nil {
		return 0, err
	}
	value := float64(math.Float32frombits(binary.LittleEndian.Uint32(b)))
	return float32(value), nil
}

func (self *clientScorePageDecoder) bool() (bool, error) {
	value, err := self.byte()
	if err != nil {
		return false, err
	}
	if 1 < value {
		return false, fmt.Errorf("%w: invalid bool", errClientScorePageInvalid)
	}
	return value == 1, nil
}

// The next score, in the page's score slab, with its lookback scores.
func (self *clientScorePageDecoder) score(depth int) (*ClientScore, error) {
	if clientScorePageMaxDepth <= depth {
		return nil, fmt.Errorf("%w: lookbacks nested past %d", errClientScorePageInvalid, clientScorePageMaxDepth)
	}
	if len(self.scores) == 0 {
		return nil, fmt.Errorf("%w: more scores than declared", errClientScorePageInvalid)
	}
	score := &self.scores[0]
	self.scores = self.scores[1:]
	b, err := self.bytes(4)
	if err != nil {
		return nil, err
	}
	fields := binary.LittleEndian.Uint32(b)
	if fields&^clientScorePageFields != 0 {
		return nil, fmt.Errorf("%w: unknown fields %#x", errClientScorePageInvalid, fields&^clientScorePageFields)
	}
	if fields&clientScorePageEgressValidUntil != 0 {
		if len(self.times) == 0 {
			return nil, fmt.Errorf("%w: more times than declared", errClientScorePageInvalid)
		}
		t, err := self.time()
		if err != nil {
			return nil, err
		}
		pointer := &self.times[0]
		self.times = self.times[1:]
		*pointer = t
		score.EgressValidUntil = pointer
	}
	if fields&clientScorePageUrlProbeSuccessWeight != 0 {
		if score.UrlProbeSuccessWeight, err = self.float64(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageClientId != 0 {
		if score.ClientId, err = self.id(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageNetworkId != 0 {
		if score.NetworkId, err = self.id(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageScores != 0 {
		if score.Scores, err = decodeClientScorePageStringMap(self, &self.intMaps, self.int); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageReliabilityWeight != 0 {
		if score.ReliabilityWeight, err = self.float64(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageIndependentReliabilityWeight != 0 {
		if score.IndependentReliabilityWeight, err = self.float64(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageTiers != 0 {
		if score.Tiers, err = decodeClientScorePageStringMap(self, &self.intMaps, self.int); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageMinRelativeLatencyMillis != 0 {
		if score.MinRelativeLatencyMillis, err = self.int(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageMaxBytesPerSecond != 0 {
		if score.MaxBytesPerSecond, err = self.varint(); err != nil {
			return nil, err
		}
	}
	score.HasLatencyTest = fields&clientScorePageHasLatencyTest != 0
	score.HasSpeedTest = fields&clientScorePageHasSpeedTest != 0
	score.NetworkOnly = fields&clientScorePageNetworkOnly != 0
	if fields&clientScorePageReputationFailedNames != 0 {
		if score.ReputationFailedNames, err = self.string(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageIpFamilies != 0 {
		if score.IpFamilies, err = self.byte(); err != nil {
			return nil, err
		}
	}
	score.Online = fields&clientScorePageOnline != 0
	score.Modern = fields&clientScorePageModern != 0
	if fields&clientScorePageCityLocationId != 0 {
		if score.CityLocationId, err = self.idPointer(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageRegionLocationId != 0 {
		if score.RegionLocationId, err = self.idPointer(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageCountryLocationId != 0 {
		if score.CountryLocationId, err = self.idPointer(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageLookbackIndex != 0 {
		if score.LookbackIndex, err = self.int(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageLookbackClientScores != 0 {
		// each entry takes at least its key byte and a score's four bytes
		n, err := self.count(5)
		if err != nil {
			return nil, err
		}
		self.mapBytes += clientScorePageMapBytes(n, int(unsafe.Sizeof(0)+unsafe.Sizeof((*ClientScore)(nil))))
		lookbackClientScores := make(map[int]*ClientScore, n)
		for range n {
			lookbackIndex, err := self.int()
			if err != nil {
				return nil, err
			}
			lookbackClientScore, err := self.score(depth + 1)
			if err != nil {
				return nil, err
			}
			lookbackClientScores[lookbackIndex] = lookbackClientScore
		}
		score.LookbackClientScores = lookbackClientScores
	}
	if fields&clientScorePageScaledWeights != 0 {
		if score.ScaledWeights, err = decodeClientScorePageStringMap(self, &self.float32Maps, self.float32); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePagePassesMinimums != 0 {
		if score.PassesMinimums, err = decodeClientScorePageStringMap(self, &self.boolMaps, self.bool); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePageCapabilityDowngrades != 0 {
		if score.CapabilityDowngrades, err = self.byte(); err != nil {
			return nil, err
		}
	}
	score.PostQuantumPositive = fields&clientScorePagePostQuantumPositive != 0
	if fields&clientScorePagePostQuantumValidUntil != 0 {
		if score.PostQuantumValidUntil, err = self.time(); err != nil {
			return nil, err
		}
	}
	if fields&clientScorePagePostQuantumModernValidUntil != 0 {
		if score.PostQuantumModernValidUntil, err = self.time(); err != nil {
			return nil, err
		}
	}
	return score, nil
}

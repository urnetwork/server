package model

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"io"
	"math"
	mathrand "math/rand/v2"
	"reflect"
	"runtime"
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/urnetwork/server"
)

// A record as a reader before CPUOPT7 §12 reads it: a plain gob decode, as
// loadResident did.
func networkClientResidentOldReaderDecode(data []byte) (*NetworkClientResident, error) {
	var resident NetworkClientResident
	if err := gob.NewDecoder(bytes.NewBuffer(data)).Decode(&resident); err != nil {
		return nil, err
	}
	return &resident, nil
}

// The record a writer before CPUOPT7 §12 wrote: NominateResident's gob encode.
func networkClientResidentOldWriterEncode(t testing.TB, resident *NetworkClientResident) []byte {
	t.Helper()
	b := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(b).Encode(resident); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Fails unless a and b hold equal fields, a nil and an empty port list told
// apart.
func assertNetworkClientResidentFieldsEqual(t testing.TB, want *NetworkClientResident, got *NetworkClientResident) {
	t.Helper()
	wantValue, gotValue := reflect.ValueOf(want).Elem(), reflect.ValueOf(got).Elem()
	for i := range wantValue.NumField() {
		name := wantValue.Type().Field(i).Name
		wantField, gotField := wantValue.Field(i), gotValue.Field(i)
		if wantField.Kind() == reflect.Slice && wantField.IsNil() != gotField.IsNil() {
			t.Fatalf("%s: nil %t, want nil %t", name, gotField.IsNil(), wantField.IsNil())
		}
		if !reflect.DeepEqual(wantField.Interface(), gotField.Interface()) {
			t.Fatalf("%s = %#v, want %#v", name, gotField.Interface(), wantField.Interface())
		}
	}
}

// Fails unless the typed encode and decode of resident gives exactly the value
// the gob encode and decode gives, field by field, and the readers' entry
// point reads it so.
func assertNetworkClientResidentMatchesGob(t testing.TB, resident *NetworkClientResident) {
	t.Helper()
	want, err := networkClientResidentOldReaderDecode(networkClientResidentOldWriterEncode(t, resident))
	if err != nil {
		t.Fatal(err)
	}
	record := encodeNetworkClientResidentTyped(resident)
	if !isTypedNetworkClientResident(record) {
		t.Fatal("a typed record is not detected as typed")
	}
	got, err := decodeNetworkClientResidentTyped(record)
	if err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	assertNetworkClientResidentFieldsEqual(t, want, got)
	read, err := loadResident(record)
	if err != nil {
		t.Fatalf("loadResident of a typed record: %v", err)
	}
	assertNetworkClientResidentFieldsEqual(t, want, read)
}

// Every field of NetworkClientResident, drawn by its type, with the values
// gob distinguishes: zero and set ids, empty, long and non-utf-8 names, nil,
// empty and long port lists, and the int extremes.
func networkClientResidentTestValue(t testing.TB, r *mathrand.Rand) *NetworkClientResident {
	t.Helper()
	resident := &NetworkClientResident{}
	value := reflect.ValueOf(resident).Elem()
	for i := range value.NumField() {
		field := value.Field(i)
		switch field.Interface().(type) {
		case server.Id:
			var id server.Id
			if r.IntN(8) != 0 {
				for j := range id {
					id[j] = byte(r.IntN(256))
				}
			}
			field.Set(reflect.ValueOf(id))
		case string:
			var text []byte
			switch r.IntN(6) {
			case 0:
			case 1:
				// past one byte of length
				text = make([]byte, 128+r.IntN(400))
				for j := range text {
					text[j] = byte('a' + r.IntN(26))
				}
			case 2:
				// gob sends any bytes
				text = make([]byte, 1+r.IntN(20))
				for j := range text {
					text[j] = byte(r.IntN(256))
				}
			default:
				text = make([]byte, 1+r.IntN(24))
				for j := range text {
					text[j] = byte('a' + r.IntN(26))
				}
			}
			field.SetString(string(text))
		case []int:
			var ports []int
			switch r.IntN(5) {
			case 0:
			case 1:
				ports = []int{}
			default:
				ports = make([]int, 1+r.IntN(40))
				for j := range ports {
					switch r.IntN(6) {
					case 0:
						ports[j] = math.MaxInt64
					case 1:
						ports[j] = math.MinInt64
					case 2:
						ports[j] = -r.IntN(1 << 20)
					default:
						ports[j] = r.IntN(65536)
					}
				}
			}
			field.Set(reflect.ValueOf(ports))
		default:
			t.Fatalf("no test values for field %s of type %s", value.Type().Field(i).Name, field.Type())
		}
	}
	return resident
}

// Every field gob encodes is a field the typed format writes, in its order.
func TestNetworkClientResidentCodecCoversEveryField(t *testing.T) {
	fields := []string{}
	residentType := reflect.TypeOf(NetworkClientResident{})
	for i := range residentType.NumField() {
		fields = append(fields, residentType.Field(i).Name)
	}
	if !slices.Equal(fields, networkClientResidentTypedFields) {
		t.Fatalf("NetworkClientResident's fields changed; write each new one in the typed record (network_client_resident_codec.go), in a new format version, and list it in networkClientResidentTypedFields:\nfields %v\ncodec  %v", fields, networkClientResidentTypedFields)
	}
}

// Over seeded records of every field's edge values, the typed round trip is
// gob's, field by field.
func TestNetworkClientResidentCodecMatchesGob(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 1))
	for range 3000 {
		assertNetworkClientResidentMatchesGob(t, networkClientResidentTestValue(t, r))
	}
}

// Each case gob distinguishes, one by one.
func TestNetworkClientResidentCodecEdgeValues(t *testing.T) {
	id := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	for _, resident := range []*NetworkClientResident{
		{},
		{ClientId: id, InstanceId: id, ResidentId: id},
		// gob omits an empty slice, which decodes as nil
		{ResidentInternalPorts: []int{}},
		{ResidentInternalPorts: []int{0}},
		{ResidentInternalPorts: []int{math.MinInt64, -1, 0, 1, math.MaxInt64}},
		{ResidentHost: "h", ResidentService: "", ResidentBlock: "b"},
		{ResidentHost: "", ResidentService: "s", ResidentBlock: ""},
		{ResidentHost: string([]byte{0xff, 0x00, 0xfe}), ResidentService: "é", ResidentBlock: string(make([]byte, 300))},
	} {
		assertNetworkClientResidentMatchesGob(t, resident)
	}
}

// A typed record round-trips: it decodes to its resident, and encodes back to
// the same bytes.
func TestNetworkClientResidentTypedRoundTrip(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 2))
	for range 1000 {
		resident := networkClientResidentTestValue(t, r)
		if resident.ResidentInternalPorts != nil && len(resident.ResidentInternalPorts) == 0 {
			// the one value that does not come back as itself, as with gob
			resident.ResidentInternalPorts = nil
		}
		record := encodeNetworkClientResidentTyped(resident)
		decoded, err := decodeNetworkClientResidentTyped(record)
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkClientResidentFieldsEqual(t, resident, decoded)
		if again := encodeNetworkClientResidentTyped(decoded); !bytes.Equal(again, record) {
			t.Fatalf("a decoded record encodes to other bytes:\n%x\n%x", record, again)
		}
	}
}

// Gob records an old writer wrote, at server f09d3edd6, with NominateResident's
// encoder: a full record and one with only its ids.
var networkClientResidentCapturedGobRecords = []struct {
	gob  string
	want NetworkClientResident
}{
	{
		gob: "ffa27f030101154e6574776f726b436c69656e745265736964656e7401ff800001070108436c69656e74496401ff8200010a496e7374616e6365496401ff8200010a5265736964656e74496401ff8200010c5265736964656e74486f7374010c00010f5265736964656e7453657276696365010c00010d5265736964656e74426c6f636b010c0001155265736964656e74496e7465726e616c506f72747301ff8400000012ff8101010102496401ff820001060120000013ff83020101055b5d696e7401ff840001040000ff81ff80011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fff8011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fff9011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffa010e686f73742d612e6578616d706c650107636f6e6e6563740107626c6f636b2d610103fe36b2fe36b4fe36b600",
		want: NetworkClientResident{
			ClientId:              server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"),
			InstanceId:            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f9"),
			ResidentId:            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fa"),
			ResidentHost:          "host-a.example",
			ResidentService:       "connect",
			ResidentBlock:         "block-a",
			ResidentInternalPorts: []int{7001, 7002, 7003},
		},
	},
	{
		gob: "ffa27f030101154e6574776f726b436c69656e745265736964656e7401ff800001070108436c69656e74496401ff8200010a496e7374616e6365496401ff8200010a5265736964656e74496401ff8200010c5265736964656e74486f7374010c00010f5265736964656e7453657276696365010c00010d5265736964656e74426c6f636b010c0001155265736964656e74496e7465726e616c506f72747301ff8400000012ff8101010102496401ff820001060120000013ff83020101055b5d696e7401ff84000104000054ff80011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffb011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffc011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffd00",
		want: NetworkClientResident{
			ClientId:   server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fb"),
			InstanceId: server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fc"),
			ResidentId: server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fd"),
		},
	},
}

// A new reader reads an old writer's gob: the records captured from the
// encoder before the typed format, and seeded records from the gob writer,
// each as the old reader read it. The gob writer with the flag off writes what
// the old writer writes, and the old reader reads it.
func TestNetworkClientResidentReaderReadsGobRecords(t *testing.T) {
	// gob numbers a process's types in the order they're first sent, so the
	// captured bytes carry the type ids of the process that wrote them. Send
	// another type first, as a full package run's earlier tests do, so that
	// nothing below leans on the order.
	type networkClientResidentTestEarlierGobType struct{ Value int }
	if err := gob.NewEncoder(io.Discard).Encode(networkClientResidentTestEarlierGobType{Value: 1}); err != nil {
		t.Fatal(err)
	}
	for _, captured := range networkClientResidentCapturedGobRecords {
		record, err := hex.DecodeString(captured.gob)
		if err != nil {
			t.Fatal(err)
		}
		if isTypedNetworkClientResident(record) {
			t.Fatal("a gob record is detected as typed")
		}
		resident, err := loadResident(record)
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkClientResidentFieldsEqual(t, &captured.want, resident)
		// the writer with the flag off writes the old writer's bytes, both in
		// this process, and the old reader reads them
		written := encodeNetworkClientResident(NetworkClientResidentFormatGob, &captured.want)
		if old := networkClientResidentOldWriterEncode(t, &captured.want); !bytes.Equal(written, old) {
			t.Fatalf("the gob writer's bytes differ from the old writer's:\n%x\n%x", old, written)
		}
		oldRead, err := networkClientResidentOldReaderDecode(written)
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkClientResidentFieldsEqual(t, &captured.want, oldRead)
	}
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 3))
	for range 500 {
		record := encodeNetworkClientResident(NetworkClientResidentFormatGob, networkClientResidentTestValue(t, r))
		want, err := networkClientResidentOldReaderDecode(record)
		if err != nil {
			t.Fatal(err)
		}
		got, err := loadResident(record)
		if err != nil {
			t.Fatal(err)
		}
		assertNetworkClientResidentFieldsEqual(t, want, got)
	}
}

// A reader before the typed format fails every typed record with an error,
// never reading it as another resident. That reader's nomination replaces such
// a record as corrupt, which is why the flag waits for every reader.
func TestNetworkClientResidentOldReaderFailsTypedRecords(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 4))
	for range 500 {
		record := encodeNetworkClientResidentTyped(networkClientResidentTestValue(t, r))
		if resident, err := networkClientResidentOldReaderDecode(record); err == nil {
			t.Fatalf("an old reader read a typed record as %+v", resident)
		}
	}
}

// With the flag off the writer writes gob, which the old reader reads; with it
// on, typed. The counters count each.
func TestNetworkClientResidentWriterFollowsFormat(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 5))
	for range 200 {
		resident := networkClientResidentTestValue(t, r)

		gobBefore := testutil.ToFloat64(networkClientResidentEncodesGob)
		record := encodeNetworkClientResident(NetworkClientResidentFormatGob, resident)
		if testutil.ToFloat64(networkClientResidentEncodesGob)-gobBefore != 1 {
			t.Fatal("a gob encode was not counted")
		}
		if isTypedNetworkClientResident(record) {
			t.Fatal("the gob writer wrote a typed record")
		}
		if _, err := networkClientResidentOldReaderDecode(record); err != nil {
			t.Fatalf("an old reader cannot read the gob writer's record: %v", err)
		}

		typedBefore := testutil.ToFloat64(networkClientResidentEncodesTyped)
		record = encodeNetworkClientResident(NetworkClientResidentFormatTyped, resident)
		if testutil.ToFloat64(networkClientResidentEncodesTyped)-typedBefore != 1 {
			t.Fatal("a typed encode was not counted")
		}
		if !isTypedNetworkClientResident(record) || record[1] != networkClientResidentVersion {
			t.Fatal("the typed writer did not write a typed record")
		}
		decodedBefore := testutil.ToFloat64(networkClientResidentDecodeCounter(true, true))
		if _, err := loadResident(record); err != nil {
			t.Fatal(err)
		}
		if testutil.ToFloat64(networkClientResidentDecodeCounter(true, true))-decodedBefore != 1 {
			t.Fatal("a typed decode was not counted")
		}
	}
}

// A malformed typed record is refused, never read as another resident:
// every truncation, another version, a byte that is neither gob nor the magic,
// lengths and counts past the record, varints not in their shortest form,
// trailing bytes; and any single changed byte either fails or reads as the
// resident whose encoding it is.
func TestNetworkClientResidentCodecRejectsMalformedRecords(t *testing.T) {
	resident := &NetworkClientResident{
		ClientId:              server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"),
		InstanceId:            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f9"),
		ResidentId:            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fa"),
		ResidentHost:          "host-a.example",
		ResidentService:       "connect",
		ResidentBlock:         "block-a",
		ResidentInternalPorts: []int{7001, 7002, -3},
	}
	record := encodeNetworkClientResidentTyped(resident)
	decode := func(data []byte) (decoded *NetworkClientResident, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("decode panicked: %v", recovered)
			}
		}()
		return loadResident(data)
	}
	errorsBefore := testutil.ToFloat64(networkClientResidentDecodeCounter(true, false))
	for n := 1; n < len(record); n += 1 {
		if decoded, err := decode(record[:n]); err == nil {
			t.Fatalf("a record truncated to %d of %d bytes decoded as %+v", n, len(record), decoded)
		}
	}
	if testutil.ToFloat64(networkClientResidentDecodeCounter(true, false))-errorsBefore != float64(len(record)-1) {
		t.Fatal("the typed decode errors were not counted")
	}
	for _, version := range []byte{0, networkClientResidentVersion + 1, 0xff} {
		other := bytes.Clone(record)
		other[1] = version
		if _, err := decode(other); !errors.Is(err, errNetworkClientResidentInvalid) {
			t.Fatalf("version %d decoded: %v", version, err)
		}
	}
	// a first byte gob cannot start with, other than the magic, goes to gob,
	// which refuses it
	for prefix := 0x80; prefix < 0xf8; prefix += 1 {
		if byte(prefix) == networkClientResidentMagic {
			continue
		}
		other := bytes.Clone(record)
		other[0] = byte(prefix)
		if decoded, err := decode(other); err == nil {
			t.Fatalf("prefix %#x decoded as %+v", prefix, decoded)
		}
	}
	if _, err := decode(append(bytes.Clone(record), 0)); !errors.Is(err, errNetworkClientResidentInvalid) {
		t.Fatalf("trailing bytes decoded: %v", err)
	}

	head := []byte{networkClientResidentMagic, networkClientResidentVersion}
	head = append(head, make([]byte, 3*len(server.Id{}))...)
	withLengths := func(lengths ...uint64) []byte {
		data := bytes.Clone(head)
		for _, length := range lengths {
			data = binary.AppendUvarint(data, length)
		}
		return data
	}
	// lengths and counts past the record, each refused before it allocates:
	// the moderate count first, so a missing bound fails here and not by
	// exhausting memory on the larger ones
	for _, data := range [][]byte{
		append(withLengths(0, 0, 0), binary.AppendUvarint([]byte{}, 100_000)...),
		withLengths(1<<62, 0, 0),
		withLengths(0, math.MaxUint64, 0),
		withLengths(2, 2, 2, 'a', 'b'),
		append(withLengths(0, 0, 0), binary.AppendUvarint(nil, 1<<40)...),
		append(withLengths(0, 0, 0), binary.AppendUvarint(nil, 3)...),
		// an overlong varint
		append(withLengths(0, 0, 0), 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01),
	} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		decoded, err := decode(data)
		runtime.ReadMemStats(&after)
		if err == nil {
			t.Fatalf("%x decoded as %+v", data, decoded)
		}
		if allocated := after.TotalAlloc - before.TotalAlloc; 64*1024 < allocated {
			t.Fatalf("a malformed record allocated %d bytes", allocated)
		}
	}
	// varints not in their shortest form: a length and a port
	for _, data := range [][]byte{
		append(withLengths(0, 0), 0x80, 0x00, 0),
		append(withLengths(0, 0, 0), 0x81, 0x00, 0x02),
		append(withLengths(0, 0, 0, 1), 0x80, 0x00),
	} {
		if decoded, err := decode(data); !errors.Is(err, errNetworkClientResidentInvalid) {
			t.Fatalf("%x decoded as %+v: %v", data, decoded, err)
		}
	}
	// any one changed byte fails, or reads as the resident it encodes
	for i := range len(record) {
		for _, b := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff, record[i] ^ 0x01} {
			changed := bytes.Clone(record)
			changed[i] = b
			decoded, err := decode(changed)
			if err != nil {
				continue
			}
			if again := encodeNetworkClientResidentTyped(decoded); !bytes.Equal(again, changed) {
				t.Fatalf("%x decoded as %+v, whose record is %x", changed, decoded, again)
			}
		}
	}
}

// Malformed records never panic, and a record that decodes is exactly the
// encoding of what it decodes to.
func FuzzNetworkClientResidentDecode(f *testing.F) {
	// deterministic seeds; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(12, 6))
	for range 4 {
		f.Add(encodeNetworkClientResidentTyped(networkClientResidentTestValue(f, r)))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		resident, err := decodeNetworkClientResidentTyped(data)
		if err != nil {
			return
		}
		if again := encodeNetworkClientResidentTyped(resident); !bytes.Equal(again, data) {
			t.Fatalf("%x decoded as %+v, whose record is %x", data, resident, again)
		}
	})
}

// A typed record decodes in three allocations, the resident, its names and its
// ports, where gob's decoder compiles the type for each record.
func TestNetworkClientResidentTypedDecodeAllocations(t *testing.T) {
	resident := &networkClientResidentCapturedGobRecords[0].want
	gobRecord := encodeNetworkClientResident(NetworkClientResidentFormatGob, resident)
	typedRecord := encodeNetworkClientResident(NetworkClientResidentFormatTyped, resident)
	gobAllocations := testing.AllocsPerRun(100, func() {
		if _, err := loadResident(gobRecord); err != nil {
			t.Fatal(err)
		}
	})
	typedAllocations := testing.AllocsPerRun(100, func() {
		if _, err := loadResident(typedRecord); err != nil {
			t.Fatal(err)
		}
	})
	if typedAllocations != 3 || gobAllocations < 10*typedAllocations {
		t.Fatalf("a record decode allocates %.0f objects from gob and %.0f typed", gobAllocations, typedAllocations)
	}
}

func BenchmarkNetworkClientResidentDecodeGob(b *testing.B) {
	record := encodeNetworkClientResident(NetworkClientResidentFormatGob, &networkClientResidentCapturedGobRecords[0].want)
	b.ReportAllocs()
	b.SetBytes(int64(len(record)))
	for b.Loop() {
		if _, err := loadResident(record); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNetworkClientResidentDecodeTyped(b *testing.B) {
	record := encodeNetworkClientResident(NetworkClientResidentFormatTyped, &networkClientResidentCapturedGobRecords[0].want)
	b.ReportAllocs()
	b.SetBytes(int64(len(record)))
	for b.Loop() {
		if _, err := loadResident(record); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNetworkClientResidentEncodeGob(b *testing.B) {
	resident := &networkClientResidentCapturedGobRecords[0].want
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeNetworkClientResident(NetworkClientResidentFormatGob, resident)
	}
}

func BenchmarkNetworkClientResidentEncodeTyped(b *testing.B) {
	resident := &networkClientResidentCapturedGobRecords[0].want
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeNetworkClientResident(NetworkClientResidentFormatTyped, resident)
	}
}

// The sizes of the record in each format, for CPUOPT7 §12.
func TestNetworkClientResidentRecordSizes(t *testing.T) {
	resident := &networkClientResidentCapturedGobRecords[0].want
	gobRecord := encodeNetworkClientResident(NetworkClientResidentFormatGob, resident)
	typedRecord := encodeNetworkClientResident(NetworkClientResidentFormatTyped, resident)
	if len(gobRecord) <= len(typedRecord) {
		t.Fatalf("typed %d bytes, gob %d", len(typedRecord), len(gobRecord))
	}
	t.Logf("a record is %d bytes in gob and %d typed", len(gobRecord), len(typedRecord))
}

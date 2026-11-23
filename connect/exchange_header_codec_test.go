package connect

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"reflect"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	connectlib "github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// A header as a reader before CPUOPT7 §12 reads it: a plain gob decode, as
// ReadHeader did.
func exchangeHeaderOldReaderDecode(data []byte) (*ExchangeHeader, error) {
	var header ExchangeHeader
	if err := gob.NewDecoder(bytes.NewBuffer(data)).Decode(&header); err != nil {
		return nil, err
	}
	return &header, nil
}

// The header a writer before CPUOPT7 §12 wrote: WriteHeader's gob encode.
func exchangeHeaderOldWriterEncode(t testing.TB, header *ExchangeHeader) []byte {
	t.Helper()
	b := bytes.NewBuffer(nil)
	if err := gob.NewEncoder(b).Encode(header); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// Fails unless want and got hold equal fields, compared one by one as the
// comparable values they are.
func assertExchangeHeaderFieldsEqual(t testing.TB, want *ExchangeHeader, got *ExchangeHeader) {
	t.Helper()
	wantValue, gotValue := reflect.ValueOf(want).Elem(), reflect.ValueOf(got).Elem()
	for i := range wantValue.NumField() {
		if wantValue.Field(i).Interface() != gotValue.Field(i).Interface() {
			t.Fatalf("%s = %#v, want %#v", wantValue.Type().Field(i).Name, gotValue.Field(i).Interface(), wantValue.Field(i).Interface())
		}
	}
	if *want != *got {
		t.Fatalf("header = %+v, want %+v", *got, *want)
	}
}

// Fails unless the typed encode and decode of header gives exactly the value
// the gob encode and decode gives, field by field, and the readers' entry
// point reads it so.
func assertExchangeHeaderMatchesGob(t testing.TB, header *ExchangeHeader) {
	t.Helper()
	want, err := exchangeHeaderOldReaderDecode(exchangeHeaderOldWriterEncode(t, header))
	if err != nil {
		t.Fatal(err)
	}
	data := encodeExchangeHeaderTyped(header)
	if !isTypedExchangeHeader(data) {
		t.Fatal("a typed header is not detected as typed")
	}
	got, err := decodeExchangeHeaderTyped(data)
	if err != nil {
		t.Fatalf("typed decode: %v", err)
	}
	assertExchangeHeaderFieldsEqual(t, want, got)
	read, err := decodeExchangeHeader(data)
	if err != nil {
		t.Fatalf("decodeExchangeHeader of a typed header: %v", err)
	}
	assertExchangeHeaderFieldsEqual(t, want, read)
}

// Client ips as the edge resolves them and as gob distinguishes them: none,
// IPv4, IPv6, IPv4-mapped IPv6, the unspecified addresses, and zoned IPv6.
func exchangeHeaderTestClientIp(r *mathrand.Rand) netip.Addr {
	switch r.IntN(8) {
	case 0:
		return netip.Addr{}
	case 1:
		return netip.AddrFrom4([4]byte{192, 0, 2, byte(r.IntN(256))})
	case 2:
		return netip.AddrFrom4([4]byte{})
	case 3:
		return netip.IPv6Unspecified()
	case 4:
		return netip.AddrFrom16(netip.AddrFrom4([4]byte{198, 51, 100, byte(r.IntN(256))}).As16())
	case 5:
		zone := make([]byte, 1+r.IntN(20))
		for i := range zone {
			zone[i] = byte('a' + r.IntN(26))
		}
		return netip.MustParseAddr("2001:db8::1").WithZone(string(zone))
	default:
		var ip [16]byte
		copy(ip[:], netip.MustParseAddr("2001:db8::").AsSlice())
		for i := 4; i < 16; i += 1 {
			ip[i] = byte(r.IntN(256))
		}
		return netip.AddrFrom16(ip)
	}
}

// An int as gob sends it: zero, small of either sign, and the extremes.
func exchangeHeaderTestInt(r *mathrand.Rand) int64 {
	switch r.IntN(6) {
	case 0:
		return 0
	case 1:
		return math.MaxInt64
	case 2:
		return math.MinInt64
	case 3:
		return -r.Int64N(1 << 20)
	default:
		return r.Int64N(1 << 20)
	}
}

// Every field of ExchangeHeader, drawn by its type.
func exchangeHeaderTestValue(t testing.TB, r *mathrand.Rand) *ExchangeHeader {
	t.Helper()
	header := &ExchangeHeader{}
	value := reflect.ValueOf(header).Elem()
	for i := range value.NumField() {
		field := value.Field(i)
		switch {
		case field.Type() == reflect.TypeOf(netip.Addr{}):
			field.Set(reflect.ValueOf(exchangeHeaderTestClientIp(r)))
		case field.Type() == reflect.TypeOf(server.Id{}):
			var id server.Id
			if r.IntN(8) != 0 {
				for j := range id {
					id[j] = byte(r.IntN(256))
				}
			}
			field.Set(reflect.ValueOf(id))
		case field.Kind() == reflect.Int:
			field.SetInt(exchangeHeaderTestInt(r))
		case field.Kind() == reflect.Bool:
			field.SetBool(r.IntN(2) == 0)
		case field.Kind() == reflect.Uint8:
			field.SetUint(uint64(r.IntN(256)))
		case field.Kind() == reflect.Uint32:
			switch r.IntN(4) {
			case 0:
				field.SetUint(0)
			case 1:
				field.SetUint(math.MaxUint32)
			default:
				field.SetUint(uint64(r.Uint32()))
			}
		default:
			t.Fatalf("no test values for field %s of type %s", value.Type().Field(i).Name, field.Type())
		}
	}
	return header
}

// Every field gob encodes is a field the typed format writes.
func TestExchangeHeaderCodecCoversEveryField(t *testing.T) {
	fields := []string{}
	headerType := reflect.TypeOf(ExchangeHeader{})
	for i := range headerType.NumField() {
		fields = append(fields, headerType.Field(i).Name)
	}
	written := slices.Clone(exchangeHeaderTypedFields)
	slices.Sort(fields)
	slices.Sort(written)
	if !slices.Equal(fields, written) {
		t.Fatalf("ExchangeHeader's fields changed; write each new one in the typed header (exchange_header_codec.go), in a new format version, and list it in exchangeHeaderTypedFields:\nfields %v\ncodec  %v", fields, written)
	}
}

// Over seeded headers of every field's edge values, the typed round trip is
// gob's, field by field.
func TestExchangeHeaderCodecMatchesGob(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(13, 1))
	for range 3000 {
		assertExchangeHeaderMatchesGob(t, exchangeHeaderTestValue(t, r))
	}
}

// Each case gob distinguishes, one by one.
func TestExchangeHeaderCodecEdgeValues(t *testing.T) {
	id := server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8")
	for _, header := range []*ExchangeHeader{
		{},
		{Op: ExchangeOpForward, ClientId: id, ResidentId: id},
		{Version: math.MinInt64, UnreliableTransferMaxMessageByteCount: math.MaxInt64},
		{ResidentMoveVersion: math.MaxUint32, Carrier: ResidentCarrierH3Dns, IpFamily: 6},
		{UnreliableTransfer: true, UnreliableFlowIsolation: true, UnreliableFlowReserve: true, ProvideIntent: true},
		{Op: 0xff, Carrier: 0xff, IpFamily: 0xff},
		{ClientIp: netip.MustParseAddr("192.0.2.1")},
		{ClientIp: netip.MustParseAddr("::ffff:192.0.2.1")},
		{ClientIp: netip.MustParseAddr("0.0.0.0")},
		{ClientIp: netip.MustParseAddr("::")},
		{ClientIp: netip.MustParseAddr("2001:db8::1%zone0")},
		// a zone past one byte of length
		{ClientIp: netip.MustParseAddr("2001:db8::1").WithZone(string(bytes.Repeat([]byte{'z'}, 200)))},
	} {
		assertExchangeHeaderMatchesGob(t, header)
	}
}

// A typed header round-trips: it decodes to its header, and encodes back to
// the same bytes.
func TestExchangeHeaderTypedRoundTrip(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(13, 2))
	for range 1000 {
		header := exchangeHeaderTestValue(t, r)
		data := encodeExchangeHeaderTyped(header)
		decoded, err := decodeExchangeHeaderTyped(data)
		if err != nil {
			t.Fatal(err)
		}
		assertExchangeHeaderFieldsEqual(t, header, decoded)
		if again := encodeExchangeHeaderTyped(decoded); !bytes.Equal(again, data) {
			t.Fatalf("a decoded header encodes to other bytes:\n%x\n%x", data, again)
		}
		// the length prefix is the client ip's MarshalBinary length
		clientIpBytes, err := header.ClientIp.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		if exchangeHeaderClientIpByteCount(header) != len(clientIpBytes) {
			t.Fatalf("client ip %s: %d bytes, MarshalBinary %d", header.ClientIp, exchangeHeaderClientIpByteCount(header), len(clientIpBytes))
		}
	}
}

// Gob headers an old writer wrote, at server f09d3edd6, with WriteHeader's
// encoder: a transport header with every field the edge sets, a forward
// header, and a header of other values with a zoned IPv6 client ip.
var exchangeHeaderCapturedGobHeaders = []struct {
	gob  string
	want ExchangeHeader
}{
	{
		gob: "fe01147f0301010e45786368616e676548656164657201ff8000010d010756657273696f6e0104000108436c69656e74496401ff8200010a5265736964656e74496401ff820001024f700106000112556e72656c6961626c655472616e736665720102000125556e72656c6961626c655472616e736665724d61784d65737361676542797465436f756e740104000117556e72656c6961626c65466c6f7749736f6c6174696f6e0102000115556e72656c6961626c65466c6f775265736572766501020001135265736964656e744d6f766556657273696f6e0106000107436172726965720106000108497046616d696c79010600010d50726f76696465496e74656e740102000108436c69656e74497001ff8400000012ff8101010102496401ff820001060120000010ff83060101044164647201ff8400000051ff80021001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fff8011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffa0101010101fe0960010102010102010401010104c000020700",
		want: ExchangeHeader{
			ClientId:                              server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8"),
			ResidentId:                            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fa"),
			Op:                                    ExchangeOpTransport,
			UnreliableTransfer:                    true,
			UnreliableTransferMaxMessageByteCount: 1200,
			UnreliableFlowIsolation:               true,
			ResidentMoveVersion:                   1,
			Carrier:                               ResidentCarrierH3,
			IpFamily:                              4,
			ProvideIntent:                         true,
			ClientIp:                              netip.MustParseAddr("192.0.2.7"),
		},
	},
	{
		gob: "fe01147f0301010e45786368616e676548656164657201ff8000010d010756657273696f6e0104000108436c69656e74496401ff8200010a5265736964656e74496401ff820001024f700106000112556e72656c6961626c655472616e736665720102000125556e72656c6961626c655472616e736665724d61784d65737361676542797465436f756e740104000117556e72656c6961626c65466c6f7749736f6c6174696f6e0102000115556e72656c6961626c65466c6f775265736572766501020001135265736964656e744d6f766556657273696f6e0106000107436172726965720106000108497046616d696c79010600010d50726f76696465496e74656e740102000108436c69656e74497001ff8400000012ff8101010102496401ff820001060120000010ff83060101044164647201ff840000003bff80021001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffb011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffd010200",
		want: ExchangeHeader{
			ClientId:   server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fb"),
			ResidentId: server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fd"),
			Op:         ExchangeOpForward,
		},
	},
	{
		gob: "fe01147f0301010e45786368616e676548656164657201ff8000010d010756657273696f6e0104000108436c69656e74496401ff8200010a5265736964656e74496401ff820001024f700106000112556e72656c6961626c655472616e736665720102000125556e72656c6961626c655472616e736665724d61784d65737361676542797465436f756e740104000117556e72656c6961626c65466c6f7749736f6c6174696f6e0102000115556e72656c6961626c65466c6f775265736572766501020001135265736964656e744d6f766556657273696f6e0106000107436172726965720106000108497046616d696c79010600010d50726f76696465496e74656e740102000108436c69656e74497001ff8400000012ff8101010102496401ff820001060120000010ff83060101044164647201ff8400000062ff800104011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7fffe011001ff9a2b3c4d5e6f70ff81ff92ffa3ffb4ffc5ffd6ffe7ffff01010205020101fcffffffff01010106021520010db80000000000000000000000077a6f6e653000",
		want: ExchangeHeader{
			Version:                               2,
			ClientId:                              server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7fe"),
			ResidentId:                            server.RequireParseId("019a2b3c-4d5e-6f70-8192-a3b4c5d6e7ff"),
			Op:                                    ExchangeOpTransport,
			UnreliableTransferMaxMessageByteCount: -3,
			UnreliableFlowReserve:                 true,
			ResidentMoveVersion:                   math.MaxUint32,
			Carrier:                               ResidentCarrierH1,
			IpFamily:                              6,
			ClientIp:                              netip.MustParseAddr("2001:db8::7%zone0"),
		},
	},
}

// A new reader reads an old writer's gob: the headers captured from the
// encoder before the typed format, and seeded headers from the gob writer,
// each as the old reader read it.
func TestExchangeHeaderReaderReadsGobHeaders(t *testing.T) {
	for _, captured := range exchangeHeaderCapturedGobHeaders {
		data, err := hex.DecodeString(captured.gob)
		if err != nil {
			t.Fatal(err)
		}
		if isTypedExchangeHeader(data) {
			t.Fatal("a gob header is detected as typed")
		}
		header, err := decodeExchangeHeader(data)
		if err != nil {
			t.Fatal(err)
		}
		assertExchangeHeaderFieldsEqual(t, &captured.want, header)
		// the writer with the flag off still writes these exact bytes
		if written := encodeExchangeHeader(false, &captured.want); !bytes.Equal(written, data) {
			t.Fatalf("the gob writer's bytes changed:\n%x\n%x", data, written)
		}
	}
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(13, 3))
	for range 500 {
		data := encodeExchangeHeader(false, exchangeHeaderTestValue(t, r))
		want, err := exchangeHeaderOldReaderDecode(data)
		if err != nil {
			t.Fatal(err)
		}
		got, err := decodeExchangeHeader(data)
		if err != nil {
			t.Fatal(err)
		}
		assertExchangeHeaderFieldsEqual(t, want, got)
	}
}

// A reader before the typed format fails every typed header with an error,
// never reading it as another header. That fails the connection, which is
// why the flag waits for every connect process.
func TestExchangeHeaderOldReaderFailsTypedHeaders(t *testing.T) {
	// deterministic for reproducible failures; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(13, 4))
	for range 500 {
		data := encodeExchangeHeaderTyped(exchangeHeaderTestValue(t, r))
		if header, err := exchangeHeaderOldReaderDecode(data); err == nil {
			t.Fatalf("an old reader read a typed header as %+v", header)
		}
	}
}

// A malformed typed header is refused, never read as another header: every
// truncation, another version, a byte that is neither gob nor the magic,
// unknown flags, a resident move version past uint32, a client ip of a length
// MarshalBinary never writes or past the header, varints not in their
// shortest form, trailing bytes; and any single changed byte either fails or
// reads as the header whose encoding it is.
func TestExchangeHeaderCodecRejectsMalformedHeaders(t *testing.T) {
	header := &exchangeHeaderCapturedGobHeaders[0].want
	data := encodeExchangeHeaderTyped(header)
	decode := func(data []byte) (decoded *ExchangeHeader, err error) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Fatalf("decode panicked: %v", recovered)
			}
		}()
		return decodeExchangeHeader(data)
	}
	errorsBefore := testutil.ToFloat64(exchangeHeaderDecodeCounter(true, false))
	for n := 1; n < len(data); n += 1 {
		if decoded, err := decode(data[:n]); err == nil {
			t.Fatalf("a header truncated to %d of %d bytes decoded as %+v", n, len(data), decoded)
		}
	}
	if testutil.ToFloat64(exchangeHeaderDecodeCounter(true, false))-errorsBefore != float64(len(data)-1) {
		t.Fatal("the typed decode errors were not counted")
	}
	if _, err := decode(nil); err == nil {
		t.Fatal("an empty header decoded")
	}
	for _, version := range []byte{0, exchangeHeaderVersion + 1, 0xff} {
		other := bytes.Clone(data)
		other[1] = version
		if _, err := decode(other); !errors.Is(err, errExchangeHeaderInvalid) {
			t.Fatalf("version %d decoded: %v", version, err)
		}
	}
	// a first byte gob cannot start with, other than the magic, goes to gob,
	// which refuses it
	for prefix := 0x80; prefix < 0xf8; prefix += 1 {
		if byte(prefix) == exchangeHeaderMagic {
			continue
		}
		other := bytes.Clone(data)
		other[0] = byte(prefix)
		if decoded, err := decode(other); err == nil {
			t.Fatalf("prefix %#x decoded as %+v", prefix, decoded)
		}
	}
	if _, err := decode(append(bytes.Clone(data), 0)); !errors.Is(err, errExchangeHeaderInvalid) {
		t.Fatalf("trailing bytes decoded: %v", err)
	}

	// a header of zero values up to its flags, then the rest as given
	build := func(flags byte, tail ...byte) []byte {
		built := []byte{exchangeHeaderMagic, exchangeHeaderVersion, 0}
		built = append(built, make([]byte, 2*len(server.Id{}))...)
		built = append(built, byte(ExchangeOpTransport), flags)
		return append(built, tail...)
	}
	if _, err := decode(build(0, 0, 0, 0, 0, 0)); err != nil {
		t.Fatalf("the zero header does not decode: %v", err)
	}
	for _, malformed := range [][]byte{
		// unknown flags
		build(exchangeHeaderFlags+1, 0, 0, 0, 0, 0),
		build(0x80, 0, 0, 0, 0, 0),
		// a resident move version past uint32
		build(0, append(append([]byte{0}, binary.AppendUvarint(nil, math.MaxUint32+1)...), 0, 0, 0)...),
		// client ips of lengths MarshalBinary never writes
		build(0, 0, 0, 0, 0, 1, 7),
		build(0, 0, 0, 0, 0, 5, 1, 2, 3, 4, 5),
		build(0, 0, 0, 0, 0, 15, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15),
		// a client ip past the header
		build(0, append([]byte{0, 0, 0, 0}, binary.AppendUvarint(nil, 1<<40)...)...),
		build(0, 0, 0, 0, 0, 16, 1, 2, 3),
		// varints not in their shortest form
		build(0, 0x80, 0x00, 0, 0, 0, 0),
		build(0, 0, 0x80, 0x00, 0, 0, 0),
		build(0, 0, 0, 0, 0, 0x80, 0x00),
		// an overlong varint
		build(0, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x01, 0, 0, 0, 0),
	} {
		if decoded, err := decode(malformed); !errors.Is(err, errExchangeHeaderInvalid) {
			t.Fatalf("%x decoded as %+v: %v", malformed, decoded, err)
		}
	}
	// any one changed byte fails, or reads as the header it encodes
	for i := range len(data) {
		for _, b := range []byte{0x00, 0x01, 0x7f, 0x80, 0xff, data[i] ^ 0x01} {
			changed := bytes.Clone(data)
			changed[i] = b
			decoded, err := decode(changed)
			if err != nil {
				continue
			}
			if again := encodeExchangeHeaderTyped(decoded); !bytes.Equal(again, changed) {
				t.Fatalf("%x decoded as %+v, whose header is %x", changed, decoded, again)
			}
		}
	}
}

// Malformed headers never panic, and a header that decodes is exactly the
// encoding of what it decodes to.
func FuzzExchangeHeaderDecode(f *testing.F) {
	// deterministic seeds; never shapes traffic
	r := mathrand.New(mathrand.NewPCG(13, 5))
	for range 4 {
		f.Add(encodeExchangeHeaderTyped(exchangeHeaderTestValue(f, r)))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		header, err := decodeExchangeHeaderTyped(data)
		if err != nil {
			return
		}
		if again := encodeExchangeHeaderTyped(header); !bytes.Equal(again, data) {
			t.Fatalf("%x decoded as %+v, whose header is %x", data, header, again)
		}
	})
}

// A typed header decodes in one allocation, the header, where gob's decoder
// compiles the header type for each connection.
func TestExchangeHeaderTypedDecodeAllocations(t *testing.T) {
	header := &exchangeHeaderCapturedGobHeaders[0].want
	gobHeader := encodeExchangeHeader(false, header)
	typedHeader := encodeExchangeHeader(true, header)
	gobAllocations := testing.AllocsPerRun(100, func() {
		if _, err := decodeExchangeHeader(gobHeader); err != nil {
			t.Fatal(err)
		}
	})
	typedAllocations := testing.AllocsPerRun(100, func() {
		if _, err := decodeExchangeHeader(typedHeader); err != nil {
			t.Fatal(err)
		}
	})
	if typedAllocations != 1 || gobAllocations < 50*typedAllocations {
		t.Fatalf("a header decode allocates %.0f objects from gob and %.0f typed", gobAllocations, typedAllocations)
	}
}

// With TypedExchangeHeaders off a buffer writes the gob header, which an old
// reader reads; with it on, the typed header. ReadHeader reads either.
func TestExchangeBufferWritesTheHeaderFormatOfItsSettings(t *testing.T) {
	for _, typed := range []bool{false, true} {
		settings := DefaultExchangeSettings()
		settings.TypedExchangeHeaders = typed
		header := &exchangeHeaderCapturedGobHeaders[0].want
		writerConn, readerConn := net.Pipe()
		writeDone := make(chan error, 1)
		go func() {
			writeDone <- NewDefaultExchangeBuffer(settings).WriteHeader(context.Background(), writerConn, header)
		}()
		// the frame as it crossed the socket
		raw := NewReceiveOnlyExchangeBuffer(settings)
		frame, err := raw.framer.Read(raw.connReader(readerConn))
		if err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		if isTypedExchangeHeader(frame) != typed {
			t.Fatalf("TypedExchangeHeaders=%t wrote a frame starting %#x", typed, frame[0])
		}
		if !typed {
			old, err := exchangeHeaderOldReaderDecode(frame)
			if err != nil {
				t.Fatalf("an old reader cannot read the gob writer's header: %v", err)
			}
			assertExchangeHeaderFieldsEqual(t, header, old)
		}
		connectlib.MessagePoolReturn(frame)

		go func() {
			writeDone <- NewDefaultExchangeBuffer(settings).WriteHeader(context.Background(), writerConn, header)
		}()
		read, err := NewReceiveOnlyExchangeBuffer(DefaultExchangeSettings()).ReadHeader(context.Background(), readerConn)
		if err != nil {
			t.Fatal(err)
		}
		if err := <-writeDone; err != nil {
			t.Fatal(err)
		}
		assertExchangeHeaderFieldsEqual(t, header, read)
		writerConn.Close()
		readerConn.Close()
	}
}

// The handshake through the accept path with the dialer and the acceptor on
// either format: the acceptor reads the dialer's header, echoes it in its own
// format, the dialer reads the echo, and the connection runs.
func TestExchangeHandshakeAcrossHeaderFormats(t *testing.T) {
	warmLocalTransportPool()
	for _, c := range []struct {
		dialerTyped   bool
		acceptorTyped bool
	}{
		{dialerTyped: false, acceptorTyped: false},
		{dialerTyped: true, acceptorTyped: false},
		{dialerTyped: false, acceptorTyped: true},
		{dialerTyped: true, acceptorTyped: true},
	} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			acceptorSettings := DefaultExchangeSettings()
			acceptorSettings.ExchangePingTimeout = time.Second
			acceptorSettings.TypedExchangeHeaders = c.acceptorTyped
			resident := newResidentCallbackLifecycleFixture(t, ctx, acceptorSettings)
			exchange := &Exchange{
				ctx:             ctx,
				cancel:          cancel,
				settings:        acceptorSettings,
				residents:       map[server.Id]*Resident{resident.clientId: resident},
				residentChanges: map[server.Id]chan struct{}{},
				connections:     map[server.Id]map[server.Id]context.CancelFunc{},
			}
			resident.exchange = exchange
			resident.residentId = server.NewId()
			dialerConn, acceptedConn := net.Pipe()
			handleDone := make(chan struct{})
			go func() {
				defer close(handleDone)
				exchange.handleExchangeConnection(acceptedConn)
			}()

			dialerSettings := DefaultExchangeSettings()
			dialerSettings.TypedExchangeHeaders = c.dialerTyped
			header := &ExchangeHeader{
				ClientId:   resident.clientId,
				ResidentId: resident.residentId,
				Op:         ExchangeOpForward,
			}
			if err := NewDefaultExchangeBuffer(dialerSettings).WriteHeader(ctx, dialerConn, header); err != nil {
				t.Fatal(err)
			}
			dialerReceive := NewReceiveOnlyExchangeBuffer(dialerSettings)
			echo, err := dialerReceive.framer.Read(dialerReceive.connReader(dialerConn))
			if err != nil {
				t.Fatalf("dialer typed=%t, acceptor typed=%t: no echo: %v", c.dialerTyped, c.acceptorTyped, err)
			}
			if isTypedExchangeHeader(echo) != c.acceptorTyped {
				t.Fatalf("the acceptor (typed=%t) echoed a header starting %#x", c.acceptorTyped, echo[0])
			}
			echoed, err := decodeExchangeHeader(echo)
			connectlib.MessagePoolReturn(echo)
			if err != nil {
				t.Fatal(err)
			}
			assertExchangeHeaderFieldsEqual(t, header, echoed)
			// the accepted forward's writer runs: its first ping
			ping, err := dialerReceive.ReadMessage(dialerConn)
			if err != nil || len(ping) != 0 {
				t.Fatalf("accepted forward ping = %d bytes, %v", len(ping), err)
			}
			connectlib.MessagePoolReturn(ping)
			dialerConn.Close()
			<-handleDone
			if err := resident.CloseAndWait(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
}

func BenchmarkExchangeHeaderDecodeGob(b *testing.B) {
	data := encodeExchangeHeader(false, &exchangeHeaderCapturedGobHeaders[0].want)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := decodeExchangeHeader(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeHeaderDecodeTyped(b *testing.B) {
	data := encodeExchangeHeader(true, &exchangeHeaderCapturedGobHeaders[0].want)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := decodeExchangeHeader(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkExchangeHeaderEncodeGob(b *testing.B) {
	header := &exchangeHeaderCapturedGobHeaders[0].want
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeExchangeHeader(false, header)
	}
}

func BenchmarkExchangeHeaderEncodeTyped(b *testing.B) {
	header := &exchangeHeaderCapturedGobHeaders[0].want
	b.ReportAllocs()
	for b.Loop() {
		_ = encodeExchangeHeader(true, header)
	}
}

// The sizes of the header in each format, for CPUOPT7 §12.
func TestExchangeHeaderSizes(t *testing.T) {
	header := &exchangeHeaderCapturedGobHeaders[0].want
	gobHeader := encodeExchangeHeader(false, header)
	typedHeader := encodeExchangeHeader(true, header)
	if len(gobHeader) <= len(typedHeader) {
		t.Fatalf("typed %d bytes, gob %d", len(typedHeader), len(gobHeader))
	}
	t.Logf("a transport header is %d bytes in gob and %d typed", len(gobHeader), len(typedHeader))
}

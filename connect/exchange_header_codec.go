// The exchange header (ExchangeHeader, resident.go), which the dialer of each
// exchange connection writes and its acceptor reads and echoes back, in a
// typed binary format beside gob (CPUOPT7.md §12).
//
// A gob header is a whole gob stream, the header's type definition and then
// its value, about 380 bytes. Reading one builds a new decoder, which
// compiles a decoder for the type on every connection: with the resident
// record's decode, gob's compileDec was 0.5–0.8% of a connect process
// (CPUOPT7.md §10.3.1). A typed header is about 50 bytes, read without
// reflection into the header alone.
//
// Readers accept both formats. A typed header starts with
// exchangeHeaderMagic, a byte no gob stream starts with, then the format
// version; every other header is gob. A typed header decodes to exactly the
// value a gob encode and decode of the same header gives
// (TestExchangeHeaderCodec*): the client ip goes through its MarshalBinary
// and UnmarshalBinary, as gob's does.
//
// Connect writes typed headers only while its settings say so
// (`typed_exchange_headers` in connect.yml, exchange_record_format.go). Only
// connect reads exchange headers, but a connect from before this file fails a
// typed header, and with it the connection, so the flag waits until every
// connect process runs this code, and goes off before any connect rolls back
// below it (CPUOPT7.md §12, "Rollback floor"). An acceptor echoes the header
// in its own format, which the dialer reads whatever its own.
//
// The format:
//
//	header = magic version varint(Version) id(ClientId) id(ResidentId)
//	         byte(Op) byte(flags) varint(UnreliableTransferMaxMessageByteCount)
//	         uvarint(ResidentMoveVersion) byte(Carrier) byte(IpFamily)
//	         uvarint(len(client ip)) client ip
//	flags  = UnreliableTransfer 1 | UnreliableFlowIsolation 2 |
//	         UnreliableFlowReserve 4 | ProvideIntent 8
//
// An id is its 16 bytes, and the client ip is its MarshalBinary bytes: none
// when it is invalid, 4, 16, or 16 and a zone. Every length is bounded by the
// bytes left, and every varint must be in its shortest form, so a header that
// decodes is exactly the encoding of what it decodes to.
package connect

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"
	"math"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/server"
)

const (
	// The first byte of a typed exchange header. No gob stream starts with
	// it: a gob stream opens with the byte length of its first message, a
	// byte below 0x80 or a negated byte count, 0xf8 to 0xff. The typed
	// resident record and score page have their own, 0xc6 and 0xc5.
	exchangeHeaderMagic byte = 0xc7
	// The format the bytes after the magic hold. A reader refuses any other.
	exchangeHeaderVersion byte = 1
)

// The flag bits of a typed header.
const (
	exchangeHeaderUnreliableTransfer byte = 1 << iota
	exchangeHeaderUnreliableFlowIsolation
	exchangeHeaderUnreliableFlowReserve
	exchangeHeaderProvideIntent

	// every flag bit of version 1
	exchangeHeaderFlags = exchangeHeaderProvideIntent<<1 - 1
)

// The fields of ExchangeHeader version 1 writes, in its order.
// TestExchangeHeaderCodecCoversEveryField fails when the struct gains a field
// not listed here.
var exchangeHeaderTypedFields = []string{
	"Version",
	"ClientId",
	"ResidentId",
	"Op",
	"UnreliableTransfer",
	"UnreliableFlowIsolation",
	"UnreliableFlowReserve",
	"ProvideIntent",
	"UnreliableTransferMaxMessageByteCount",
	"ResidentMoveVersion",
	"Carrier",
	"IpFamily",
	"ClientIp",
}

// A typed header that is truncated, malformed, or of another version.
var errExchangeHeaderInvalid = errors.New("invalid typed exchange header")

// Whether data is a typed header, as opposed to gob.
func isTypedExchangeHeader(data []byte) bool {
	return 0 < len(data) && data[0] == exchangeHeaderMagic
}

// Every connect process counts the headers it reads by format and result, so
// a rollout can see the typed headers arrive and the gob headers leave, and a
// header this process could not read.
var exchangeHeaderDecodes = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_header_decodes_total",
		Help:      "Exchange headers read, by header format (gob or typed) and result (ok or error)",
	},
	[]string{"format", "result"},
)

var exchangeHeaderDecodeCounters = [2][2]prometheus.Counter{
	{exchangeHeaderDecodes.WithLabelValues("gob", "error"), exchangeHeaderDecodes.WithLabelValues("gob", "ok")},
	{exchangeHeaderDecodes.WithLabelValues("typed", "error"), exchangeHeaderDecodes.WithLabelValues("typed", "ok")},
}

func exchangeHeaderDecodeCounter(typed bool, ok bool) prometheus.Counter {
	formatIndex, resultIndex := 0, 0
	if typed {
		formatIndex = 1
	}
	if ok {
		resultIndex = 1
	}
	return exchangeHeaderDecodeCounters[formatIndex][resultIndex]
}

// The headers this process writes, by format: a dialer's and an acceptor's
// echo.
var exchangeHeaderEncodes = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Namespace: "urnetwork",
		Subsystem: "connect",
		Name:      "exchange_header_encodes_total",
		Help:      "Exchange headers written, a dialer's and an acceptor's echo, by format (gob or typed)",
	},
	[]string{"format"},
)

var exchangeHeaderEncodesGob = exchangeHeaderEncodes.WithLabelValues("gob")
var exchangeHeaderEncodesTyped = exchangeHeaderEncodes.WithLabelValues("typed")

func init() {
	prometheus.MustRegister(exchangeHeaderDecodes, exchangeHeaderEncodes)
}

// The header bytes of header, typed or gob. The gob header is the bytes
// WriteHeader wrote before the typed format.
func encodeExchangeHeader(typed bool, header *ExchangeHeader) []byte {
	if typed {
		exchangeHeaderEncodesTyped.Inc()
		return encodeExchangeHeaderTyped(header)
	}
	exchangeHeaderEncodesGob.Inc()
	b := bytes.NewBuffer(nil)
	e := gob.NewEncoder(b)
	e.Encode(header)
	return b.Bytes()
}

// The typed header of header. Every value has one.
func encodeExchangeHeaderTyped(header *ExchangeHeader) []byte {
	clientIpByteCount := exchangeHeaderClientIpByteCount(header)
	size := 2 + 2*binary.MaxVarintLen64 + 2*len(server.Id{}) + 4 + 2*binary.MaxVarintLen64 + binary.MaxVarintLen64 + clientIpByteCount
	data := make([]byte, 0, size)
	data = append(data, exchangeHeaderMagic, exchangeHeaderVersion)
	data = binary.AppendVarint(data, int64(header.Version))
	data = append(data, header.ClientId[:]...)
	data = append(data, header.ResidentId[:]...)
	flags := byte(0)
	if header.UnreliableTransfer {
		flags |= exchangeHeaderUnreliableTransfer
	}
	if header.UnreliableFlowIsolation {
		flags |= exchangeHeaderUnreliableFlowIsolation
	}
	if header.UnreliableFlowReserve {
		flags |= exchangeHeaderUnreliableFlowReserve
	}
	if header.ProvideIntent {
		flags |= exchangeHeaderProvideIntent
	}
	data = append(data, byte(header.Op), flags)
	data = binary.AppendVarint(data, int64(header.UnreliableTransferMaxMessageByteCount))
	data = binary.AppendUvarint(data, uint64(header.ResidentMoveVersion))
	data = append(data, byte(header.Carrier), header.IpFamily)
	data = binary.AppendUvarint(data, uint64(clientIpByteCount))
	// AppendBinary of a netip.Addr cannot fail
	data, _ = header.ClientIp.AppendBinary(data)
	return data
}

// The length of the client ip's MarshalBinary bytes.
func exchangeHeaderClientIpByteCount(header *ExchangeHeader) int {
	switch {
	case !header.ClientIp.IsValid():
		return 0
	case header.ClientIp.Is4():
		return 4
	default:
		return 16 + len(header.ClientIp.Zone())
	}
}

// The header of header bytes in either format, counted by format and result.
// A header that cannot be read is an error, never another header.
func decodeExchangeHeader(data []byte) (*ExchangeHeader, error) {
	typed := isTypedExchangeHeader(data)
	var header *ExchangeHeader
	var err error
	if typed {
		header, err = decodeExchangeHeaderTyped(data)
	} else {
		header, err = decodeExchangeHeaderGob(data)
	}
	exchangeHeaderDecodeCounter(typed, err == nil).Inc()
	if err != nil {
		return nil, err
	}
	return header, nil
}

// The header of gob header bytes, as ReadHeader decoded it before the typed
// format.
func decodeExchangeHeaderGob(data []byte) (*ExchangeHeader, error) {
	var header ExchangeHeader
	b := bytes.NewBuffer(data)
	e := gob.NewDecoder(b)
	err := e.Decode(&header)
	if err != nil {
		return nil, err
	}
	return &header, nil
}

// The header of a typed header. It holds nothing of data, which the caller
// returns to the message pool.
func decodeExchangeHeaderTyped(data []byte) (*ExchangeHeader, error) {
	if len(data) < 2 || data[0] != exchangeHeaderMagic {
		return nil, fmt.Errorf("%w: not a typed header", errExchangeHeaderInvalid)
	}
	if data[1] != exchangeHeaderVersion {
		return nil, fmt.Errorf("%w: version %d", errExchangeHeaderInvalid, data[1])
	}
	reader := &exchangeHeaderReader{data: data, offset: 2}
	header := &ExchangeHeader{}
	var err error
	if header.Version, err = reader.int(); err != nil {
		return nil, err
	}
	if header.ClientId, err = reader.id(); err != nil {
		return nil, err
	}
	if header.ResidentId, err = reader.id(); err != nil {
		return nil, err
	}
	b, err := reader.bytes(2)
	if err != nil {
		return nil, err
	}
	header.Op = ExchangeOp(b[0])
	flags := b[1]
	if flags&^exchangeHeaderFlags != 0 {
		return nil, fmt.Errorf("%w: unknown flags %#x", errExchangeHeaderInvalid, flags&^exchangeHeaderFlags)
	}
	header.UnreliableTransfer = flags&exchangeHeaderUnreliableTransfer != 0
	header.UnreliableFlowIsolation = flags&exchangeHeaderUnreliableFlowIsolation != 0
	header.UnreliableFlowReserve = flags&exchangeHeaderUnreliableFlowReserve != 0
	header.ProvideIntent = flags&exchangeHeaderProvideIntent != 0
	if header.UnreliableTransferMaxMessageByteCount, err = reader.int(); err != nil {
		return nil, err
	}
	residentMoveVersion, err := reader.uvarint()
	if err != nil {
		return nil, err
	}
	if math.MaxUint32 < residentMoveVersion {
		return nil, fmt.Errorf("%w: resident move version out of range", errExchangeHeaderInvalid)
	}
	header.ResidentMoveVersion = uint32(residentMoveVersion)
	if b, err = reader.bytes(2); err != nil {
		return nil, err
	}
	header.Carrier = ResidentCarrier(b[0])
	header.IpFamily = b[1]
	clientIpByteCount, err := reader.uvarint()
	if err != nil {
		return nil, err
	}
	if uint64(len(data)-reader.offset) < clientIpByteCount {
		return nil, fmt.Errorf("%w: client ip past the header", errExchangeHeaderInvalid)
	}
	if b, err = reader.bytes(int(clientIpByteCount)); err != nil {
		return nil, err
	}
	// none, 4, 16, or 16 and a zone; any other length is refused
	if err := header.ClientIp.UnmarshalBinary(b); err != nil {
		return nil, fmt.Errorf("%w: client ip: %w", errExchangeHeaderInvalid, err)
	}
	if reader.offset != len(data) {
		return nil, fmt.Errorf("%w: trailing bytes", errExchangeHeaderInvalid)
	}
	return header, nil
}

// The read position of one typed header decode.
type exchangeHeaderReader struct {
	data   []byte
	offset int
}

func (self *exchangeHeaderReader) bytes(n int) ([]byte, error) {
	if n < 0 || len(self.data)-self.offset < n {
		return nil, fmt.Errorf("%w: truncated", errExchangeHeaderInvalid)
	}
	b := self.data[self.offset : self.offset+n]
	self.offset += n
	return b, nil
}

func (self *exchangeHeaderReader) id() (server.Id, error) {
	b, err := self.bytes(len(server.Id{}))
	if err != nil {
		return server.Id{}, err
	}
	return server.Id(b), nil
}

// A uvarint in its shortest form.
func (self *exchangeHeaderReader) uvarint() (uint64, error) {
	value, n := binary.Uvarint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated or overlong varint", errExchangeHeaderInvalid)
	}
	var shortest [binary.MaxVarintLen64]byte
	if n != binary.PutUvarint(shortest[:], value) {
		return 0, fmt.Errorf("%w: varint not in its shortest form", errExchangeHeaderInvalid)
	}
	self.offset += n
	return value, nil
}

// A varint in its shortest form that fits the platform int, as gob requires
// of an int.
func (self *exchangeHeaderReader) int() (int, error) {
	value, n := binary.Varint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated or overlong varint", errExchangeHeaderInvalid)
	}
	var shortest [binary.MaxVarintLen64]byte
	if n != binary.PutVarint(shortest[:], value) {
		return 0, fmt.Errorf("%w: varint not in its shortest form", errExchangeHeaderInvalid)
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%w: int out of range", errExchangeHeaderInvalid)
	}
	self.offset += n
	return int(value), nil
}

// The resident record, the NetworkClientResident stored under
// `ncr_<client id>` (network_client_resident_model.go), in a typed binary
// format beside gob (CPUOPT7.md §12).
//
// A gob decode of the record builds a new decoder, which reads the type
// definition every gob stream opens with and compiles a decoder for it, on
// every read. In connect, loadResident was 0.5–0.8% of a process, and gob's
// compileDec, shared with the exchange header's decode, about as much
// (CPUOPT7.md §10.3.1). A typed record is read without reflection: the struct,
// one string holding its three names, and its ports.
//
// Readers accept both formats. A typed record starts with
// networkClientResidentMagic, a byte no gob stream starts with, then the
// format version; every other record is gob. A typed record decodes to exactly
// the value a gob encode and decode of the same record gives
// (TestNetworkClientResidentCodec*): an empty port list decodes as nil, as gob
// omits an empty slice.
//
// Connect writes typed records only while its settings say so
// (`typed_resident_records` in connect.yml, connect/exchange_record_format.go).
// A reader from before this file fails a typed record as undecodable, never as
// another resident: connect's nomination then replaces it as corrupt, and the
// api and the taskworker see no resident. So the flag waits until every reader
// runs this code, and it goes off, and the typed records are replaced or
// expire, before any reader rolls back below it (CPUOPT7.md §12, "Rollback
// floor"). The nomination and removal compare the exact bytes they read, so
// records of both formats side by side keep their compare-and-set.
//
// The format:
//
//	record = magic version id(ClientId) id(InstanceId) id(ResidentId)
//	         uvarint(len(ResidentHost)) uvarint(len(ResidentService))
//	         uvarint(len(ResidentBlock)) ResidentHost ResidentService
//	         ResidentBlock uvarint(ports) varint(port)*
//
// An id is its 16 bytes. Every length and count is bounded by the bytes left,
// so a malformed record cannot make a decode allocate more than its own
// length allows, and every varint must be in its shortest form, so a record
// that decodes is exactly the encoding of what it decodes to.
package model

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/server/v2026"
)

const (
	// The first byte of a typed resident record. No gob stream starts with
	// it: a gob stream opens with the byte length of its first message, a
	// byte below 0x80 or a negated byte count, 0xf8 to 0xff. The typed score
	// page (network_client_score_page_codec.go) has its own, 0xc5.
	networkClientResidentMagic byte = 0xc6
	// The format the bytes after the magic hold. A reader refuses any other.
	networkClientResidentVersion byte = 1
)

// The fields of NetworkClientResident version 1 writes, in its order.
// TestNetworkClientResidentCodecCoversEveryField fails when the struct gains
// a field not listed here.
var networkClientResidentTypedFields = []string{
	"ClientId",
	"InstanceId",
	"ResidentId",
	"ResidentHost",
	"ResidentService",
	"ResidentBlock",
	"ResidentInternalPorts",
}

// A typed record that is truncated, malformed, or of another version.
var errNetworkClientResidentInvalid = errors.New("invalid typed resident record")

// Whether data is a typed record, as opposed to gob.
func isTypedNetworkClientResident(data []byte) bool {
	return 0 < len(data) && data[0] == networkClientResidentMagic
}

// The format a nomination writes its resident record in.
type NetworkClientResidentFormat uint8

const (
	// what every reader reads, the default
	NetworkClientResidentFormatGob NetworkClientResidentFormat = iota
	// the typed format, which only readers from CPUOPT7 §12 on read
	NetworkClientResidentFormatTyped
)

func (self NetworkClientResidentFormat) String() string {
	if self == NetworkClientResidentFormatTyped {
		return "typed"
	}
	return "gob"
}

// Every reader counts its record decodes by format and result, so a rollout
// can see the typed records arrive and the gob records leave, and a reader
// that cannot read a record.
var networkClientResidentDecodes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_network_client_resident_decodes_total",
	Help: "Resident record decodes by record format (gob or typed) and result (ok or error)",
}, []string{"format", "result"})

var networkClientResidentDecodeCounters = [2][2]prometheus.Counter{
	{networkClientResidentDecodes.WithLabelValues("gob", "error"), networkClientResidentDecodes.WithLabelValues("gob", "ok")},
	{networkClientResidentDecodes.WithLabelValues("typed", "error"), networkClientResidentDecodes.WithLabelValues("typed", "ok")},
}

func networkClientResidentDecodeCounter(typed bool, ok bool) prometheus.Counter {
	formatIndex, resultIndex := 0, 0
	if typed {
		formatIndex = 1
	}
	if ok {
		resultIndex = 1
	}
	return networkClientResidentDecodeCounters[formatIndex][resultIndex]
}

// Nominations count the records they encode by format.
var networkClientResidentEncodes = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_network_client_resident_encodes_total",
	Help: "Resident records encoded by resident nominations, by format (gob or typed)",
}, []string{"format"})

var networkClientResidentEncodesGob = networkClientResidentEncodes.WithLabelValues("gob")
var networkClientResidentEncodesTyped = networkClientResidentEncodes.WithLabelValues("typed")

func init() {
	prometheus.MustRegister(networkClientResidentDecodes, networkClientResidentEncodes)
}

// The record of resident in format. The gob record is the bytes nominations
// wrote before the typed format.
func encodeNetworkClientResident(format NetworkClientResidentFormat, resident *NetworkClientResident) []byte {
	if format == NetworkClientResidentFormatTyped {
		networkClientResidentEncodesTyped.Inc()
		return encodeNetworkClientResidentTyped(resident)
	}
	networkClientResidentEncodesGob.Inc()
	b := bytes.NewBuffer(nil)
	e := gob.NewEncoder(b)
	e.Encode(resident)
	return b.Bytes()
}

// The typed record of resident. Every value has one.
func encodeNetworkClientResidentTyped(resident *NetworkClientResident) []byte {
	size := 2 + 3*len(server.Id{}) +
		3*binary.MaxVarintLen64 + len(resident.ResidentHost) + len(resident.ResidentService) + len(resident.ResidentBlock) +
		binary.MaxVarintLen64 + binary.MaxVarintLen64*len(resident.ResidentInternalPorts)
	data := make([]byte, 0, size)
	data = append(data, networkClientResidentMagic, networkClientResidentVersion)
	data = append(data, resident.ClientId[:]...)
	data = append(data, resident.InstanceId[:]...)
	data = append(data, resident.ResidentId[:]...)
	data = binary.AppendUvarint(data, uint64(len(resident.ResidentHost)))
	data = binary.AppendUvarint(data, uint64(len(resident.ResidentService)))
	data = binary.AppendUvarint(data, uint64(len(resident.ResidentBlock)))
	data = append(data, resident.ResidentHost...)
	data = append(data, resident.ResidentService...)
	data = append(data, resident.ResidentBlock...)
	data = binary.AppendUvarint(data, uint64(len(resident.ResidentInternalPorts)))
	for _, port := range resident.ResidentInternalPorts {
		data = binary.AppendVarint(data, int64(port))
	}
	return data
}

// The resident of a non-empty record in either format, counted by format and
// result. A record that cannot be read is an error, never another resident.
func decodeNetworkClientResident(data []byte) (*NetworkClientResident, error) {
	typed := isTypedNetworkClientResident(data)
	var resident *NetworkClientResident
	var err error
	if typed {
		resident, err = decodeNetworkClientResidentTyped(data)
	} else {
		resident, err = decodeNetworkClientResidentGob(data)
	}
	networkClientResidentDecodeCounter(typed, err == nil).Inc()
	if err != nil {
		// the counter counts every one; the record's bytes are not logged
		if glog.V(1) {
			glog.Infof("[ncrm]resident record unreadable (typed=%t) = %s\n", typed, err)
		}
		return nil, err
	}
	return resident, nil
}

// The resident of a gob record, as every reader decoded it before the typed
// format.
func decodeNetworkClientResidentGob(data []byte) (*NetworkClientResident, error) {
	b := bytes.NewBuffer(data)
	e := gob.NewDecoder(b)
	var resident NetworkClientResident
	err := e.Decode(&resident)
	if err != nil {
		return nil, err
	}
	return &resident, nil
}

// The resident of a typed record: the struct, one string for its three names
// and, when it has ports, its ports.
func decodeNetworkClientResidentTyped(data []byte) (*NetworkClientResident, error) {
	if len(data) < 2 || data[0] != networkClientResidentMagic {
		return nil, fmt.Errorf("%w: not a typed record", errNetworkClientResidentInvalid)
	}
	if data[1] != networkClientResidentVersion {
		return nil, fmt.Errorf("%w: version %d", errNetworkClientResidentInvalid, data[1])
	}
	reader := &networkClientResidentReader{data: data, offset: 2}
	resident := &NetworkClientResident{}
	var err error
	if resident.ClientId, err = reader.id(); err != nil {
		return nil, err
	}
	if resident.InstanceId, err = reader.id(); err != nil {
		return nil, err
	}
	if resident.ResidentId, err = reader.id(); err != nil {
		return nil, err
	}
	// each length is bounded by the bytes left after the lengths read so far
	hostLength, err := reader.count(1)
	if err != nil {
		return nil, err
	}
	serviceLength, err := reader.count(1)
	if err != nil {
		return nil, err
	}
	blockLength, err := reader.count(1)
	if err != nil {
		return nil, err
	}
	nameBytes, err := reader.bytes(hostLength + serviceLength + blockLength)
	if err != nil {
		return nil, err
	}
	// one string holds the three names
	names := string(nameBytes)
	resident.ResidentHost = names[:hostLength]
	resident.ResidentService = names[hostLength : hostLength+serviceLength]
	resident.ResidentBlock = names[hostLength+serviceLength:]
	// each port takes at least a byte
	portCount, err := reader.count(1)
	if err != nil {
		return nil, err
	}
	if 0 < portCount {
		ports := make([]int, portCount)
		for i := range ports {
			if ports[i], err = reader.int(); err != nil {
				return nil, err
			}
		}
		resident.ResidentInternalPorts = ports
	}
	if reader.offset != len(data) {
		return nil, fmt.Errorf("%w: trailing bytes", errNetworkClientResidentInvalid)
	}
	return resident, nil
}

// The read position of one typed record decode.
type networkClientResidentReader struct {
	data   []byte
	offset int
}

func (self *networkClientResidentReader) bytes(n int) ([]byte, error) {
	if n < 0 || len(self.data)-self.offset < n {
		return nil, fmt.Errorf("%w: truncated", errNetworkClientResidentInvalid)
	}
	b := self.data[self.offset : self.offset+n]
	self.offset += n
	return b, nil
}

func (self *networkClientResidentReader) id() (server.Id, error) {
	b, err := self.bytes(len(server.Id{}))
	if err != nil {
		return server.Id{}, err
	}
	return server.Id(b), nil
}

// A uvarint in its shortest form.
func (self *networkClientResidentReader) uvarint() (uint64, error) {
	value, n := binary.Uvarint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated or overlong varint", errNetworkClientResidentInvalid)
	}
	var shortest [binary.MaxVarintLen64]byte
	if n != binary.PutUvarint(shortest[:], value) {
		return 0, fmt.Errorf("%w: varint not in its shortest form", errNetworkClientResidentInvalid)
	}
	self.offset += n
	return value, nil
}

// A count no larger than the bytes left allow at unit bytes each.
func (self *networkClientResidentReader) count(unit int) (int, error) {
	value, err := self.uvarint()
	if err != nil {
		return 0, err
	}
	if uint64(len(self.data)-self.offset)/uint64(unit) < value {
		return 0, fmt.Errorf("%w: count past the record", errNetworkClientResidentInvalid)
	}
	return int(value), nil
}

// A varint in its shortest form that fits the platform int, as gob requires
// of an int.
func (self *networkClientResidentReader) int() (int, error) {
	value, n := binary.Varint(self.data[self.offset:])
	if n <= 0 {
		return 0, fmt.Errorf("%w: truncated or overlong varint", errNetworkClientResidentInvalid)
	}
	var shortest [binary.MaxVarintLen64]byte
	if n != binary.PutVarint(shortest[:], value) {
		return 0, fmt.Errorf("%w: varint not in its shortest form", errNetworkClientResidentInvalid)
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%w: int out of range", errNetworkClientResidentInvalid)
	}
	self.offset += n
	return int(value), nil
}

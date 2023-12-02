package client


import (
	"fmt"
	"bytes"
	"encoding/hex"
	"hash/fnv"

	"bringyour.com/protocol"
	"bringyour.com/connect"
)


// note: publicly exported types must be fully contained in the `client` package tree
// the `gomobile` native interface compiler won't be able to map types otherwise
// a number of types (struct, function, interface) are redefined in `client`,
// somtimes in a simplified way, and then internally converted back to the native type
// examples:
// - fixed primitive arrays are not exportable. Use slices instead.
// - raw structs are not exportable. Use pointers to structs instead.
//   e.g. Id that is to be exported needs to be *Id
// - redefined primitive types are not exportable. Use type aliases instead.
// - arrays of structs are not exportable. See https://github.com/golang/go/issues/13445
//   use the "ExportableList" workaround from `gomobile.go`
//
// additionally, the entire bringyour.com/bringyour tree cannot be used because it pulls in the
// `warp` environment expectations, which is not compatible with the client lib


// this value is set via the linker, e.g.
// -ldflags "-X client.Version=$WARP_VERSION"
var Version string


type Id struct {
	id [16]byte
}

func newId(id [16]byte) *Id {
	return &Id{
		id: id,
	}
}

func NewIdFromString(src string) (*Id, error) {
	dst, err := parseUuid(src)
	if err != nil {
		return nil, err
	}
	return newId(dst), nil
}

func (self *Id) Bytes() []byte {
	return self.id[:]
}

func (self *Id) String() string {
	return encodeUuid(self.id)
}

func (self *Id) cmp(b Id) int {
	for i, v := range self.id {
		if v < b.id[i] {
			return -1
		}
		if b.id[i] < v {
			return 1
		}
	}
	return 0
}

func (self *Id) toConnectId() connect.Id {
	return self.id
}

func (self *Id) MarshalJSON() ([]byte, error) {
	var buf [16]byte
	copy(buf[0:16], self.id[0:16])
	var buff bytes.Buffer
	buff.WriteByte('"')
	buff.WriteString(encodeUuid(buf))
	buff.WriteByte('"')
	b := buff.Bytes()
	gmLog("MARSHAL ID TO: %s", string(b))
	return b, nil
}

func (self *Id) UnmarshalJSON(src []byte) error {
	if len(src) != 38 {
		return fmt.Errorf("invalid length for UUID: %v", len(src))
	}
	buf, err := parseUuid(string(src[1 : len(src)-1]))
	if err != nil {
		return err
	}
	self.id = buf
	return nil
}

// Android support

func (self *Id) IdEquals(b *Id) bool {
	if b == nil {
		return false
	}
	return self.id == b.id
}

func (self *Id) IdHashCode() int32 {
	h := fnv.New32()
	h.Write(self.id[:])
	return int32(h.Sum32())
}


// parseUuid converts a string UUID in standard form to a byte array.
func parseUuid(src string) (dst [16]byte, err error) {
	switch len(src) {
	case 36:
		src = src[0:8] + src[9:13] + src[14:18] + src[19:23] + src[24:]
	case 32:
		// dashes already stripped, assume valid
	default:
		// assume invalid.
		return dst, fmt.Errorf("cannot parse UUID %v", src)
	}

	buf, err := hex.DecodeString(src)
	if err != nil {
		return dst, err
	}

	copy(dst[:], buf)
	return dst, err
}


func encodeUuid(src [16]byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", src[0:4], src[4:6], src[6:8], src[8:10], src[10:16])
}


type Path struct {
	ClientId *Id
	StreamId *Id
}

func NewPath(clientId *Id, streamId *Id) *Path {
	return &Path{
		ClientId: clientId,
		StreamId: streamId,
	}
}

func fromConnectPath(path connect.Path) *Path {
	return &Path{
		ClientId: newId(path.ClientId),
		StreamId: newId(path.StreamId),
	}
}

func (self *Path) toConnectPath() connect.Path {
	path := connect.Path{}
	if self.ClientId != nil {
		path.ClientId = connect.Id(self.ClientId.id)
	}
	if self.StreamId != nil {
		path.StreamId = connect.Id(self.StreamId.id)
	}
	return path
}


type ProvideMode = int
const (
	ProvideModeNone ProvideMode = ProvideMode(protocol.ProvideMode_None)
	ProvideModeNetwork ProvideMode = ProvideMode(protocol.ProvideMode_Network)
	ProvideModePublic ProvideMode = ProvideMode(protocol.ProvideMode_Public)
)


type LocationType = string
const (
	LocationTypeCountry LocationType = "country"
	LocationTypeRegion LocationType = "region"
	LocationTypeCity LocationType = "city"
)

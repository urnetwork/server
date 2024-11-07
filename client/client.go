package client

import (
	"bytes"
	"encoding/hex"
	"fmt"
	// "hash/fnv"
	"encoding/json"
	"flag"
	"math"
	"os"

	// "net/http"
	// _ "net/http/pprof"

	// "github.com/golang/glog"

	"bringyour.com/connect"
	"bringyour.com/protocol"
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

func init() {
	initGlog()

	// initPprof()
}

func initGlog() {
	flag.Set("logtostderr", "true")
	flag.Set("stderrthreshold", "INFO")
	flag.Set("v", "0")
	// unlike unix, the android/ios standard is for diagnostics to go to stdout
	os.Stderr = os.Stdout
}

// func initPprof() {
// 	go func() {
// 		glog.Infof("pprof = %s\n", http.ListenAndServe("localhost:6060", nil))
// 	}()
// }

// this value is set via the linker, e.g.
// -ldflags "-X client.Version=$WARP_VERSION"
var Version string

type Id struct {
	id [16]byte
	// store this on the object to support gomobile "equals" and "hashCode"
	IdStr string
}

func newId(id [16]byte) *Id {
	return &Id{
		id:    id,
		IdStr: encodeUuid(id),
	}
}

func ParseId(src string) (*Id, error) {
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
	return self.IdStr
}

func (self *Id) Cmp(b *Id) int {
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
	self.IdStr = encodeUuid(buf)
	return nil
}

// Android support

// func (self *Id) IdEquals(b *Id) bool {
// 	if b == nil {
// 		return false
// 	}
// 	return self.id == b.id
// }

// func (self *Id) IdHashCode() int32 {
// 	h := fnv.New32()
// 	h.Write(self.id[:])
// 	return int32(h.Sum32())
// }

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

type TransferPath struct {
	SourceId      *Id
	DestinationId *Id
	StreamId      *Id
}

func NewTransferPath(sourceId *Id, destinationId *Id, streamId *Id) *TransferPath {
	return &TransferPath{
		SourceId:      sourceId,
		DestinationId: destinationId,
		StreamId:      streamId,
	}
}

func fromConnect(path connect.TransferPath) *TransferPath {
	return &TransferPath{
		SourceId:      newId(path.SourceId),
		DestinationId: newId(path.DestinationId),
		StreamId:      newId(path.StreamId),
	}
}

func (self *TransferPath) toConnect() connect.TransferPath {
	path := connect.TransferPath{}
	if self.SourceId != nil {
		path.SourceId = connect.Id(self.SourceId.id)
	}
	if self.DestinationId != nil {
		path.DestinationId = connect.Id(self.DestinationId.id)
	}
	if self.StreamId != nil {
		path.StreamId = connect.Id(self.StreamId.id)
	}
	return path
}

type ProvideMode = int

const (
	ProvideModeNone             ProvideMode = ProvideMode(protocol.ProvideMode_None)
	ProvideModeNetwork          ProvideMode = ProvideMode(protocol.ProvideMode_Network)
	ProvideModeFriendsAndFamily ProvideMode = ProvideMode(protocol.ProvideMode_FriendsAndFamily)
	ProvideModePublic           ProvideMode = ProvideMode(protocol.ProvideMode_Public)
)

type LocationType = string

const (
	LocationTypeCountry LocationType = "country"
	LocationTypeRegion  LocationType = "region"
	LocationTypeCity    LocationType = "city"
)

type ByteCount = int64

type NanoCents = int64

func UsdToNanoCents(usd float64) NanoCents {
	return NanoCents(math.Round(usd * float64(1000000000)))
}

func NanoCentsToUsd(nanoCents NanoCents) float64 {
	return float64(nanoCents) / float64(1000000000)
}

// merged location and location group
type ConnectLocation struct {
	ConnectLocationId *ConnectLocationId `json:"connect_location_id,omitempty"`
	Name              string             `json:"name,omitempty"`

	ProviderCount int32 `json:"provider_count,omitempty"`
	Promoted      bool  `json:"promoted,omitempty"`
	MatchDistance int32 `json:"match_distance,omitempty"`

	LocationType LocationType `json:"location_type,omitempty"`

	City        string `json:"city,omitempty"`
	Region      string `json:"region,omitempty"`
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"country_code,omitempty"`

	CityLocationId    *Id `json:"city_location_id,omitempty"`
	RegionLocationId  *Id `json:"region_location_id,omitempty"`
	CountryLocationId *Id `json:"country_location_id,omitempty"`
}

func (self *ConnectLocation) IsGroup() bool {
	return self.ConnectLocationId.IsGroup()
}

func (self *ConnectLocation) IsDevice() bool {
	return self.ConnectLocationId.IsDevice()
}

func (self *ConnectLocation) ToRegion() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Region,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeRegion,

		City:        "",
		Region:      self.Region,
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  self.RegionLocationId,
		CountryLocationId: self.CountryLocationId,
	}
}

func (self *ConnectLocation) ToCountry() *ConnectLocation {
	return &ConnectLocation{
		ConnectLocationId: self.ConnectLocationId,
		Name:              self.Country,

		ProviderCount: self.ProviderCount,
		Promoted:      false,
		MatchDistance: 0,

		LocationType: LocationTypeCountry,

		City:        "",
		Region:      "",
		Country:     self.Country,
		CountryCode: self.CountryCode,

		CityLocationId:    nil,
		RegionLocationId:  nil,
		CountryLocationId: self.CountryLocationId,
	}
}

// merged location and location group
type ConnectLocationId struct {
	// if set, the location is a direct connection to another device
	ClientId        *Id  `json:"client_id,omitempty"`
	LocationId      *Id  `json:"location_id,omitempty"`
	LocationGroupId *Id  `json:"location_group_id,omitempty"`
	BestAvailable   bool `json:"best_available,omitempty"`
}

func (self *ConnectLocationId) IsGroup() bool {
	return self.LocationGroupId != nil
}

func (self *ConnectLocationId) IsDevice() bool {
	return self.ClientId != nil
}

func (self *ConnectLocationId) Cmp(b *ConnectLocationId) int {
	// - direct
	// - group
	if self.ClientId != nil && b.ClientId != nil {
		if c := self.ClientId.Cmp(b.ClientId); c != 0 {
			return c
		}
	} else if self.ClientId != nil {
		return -1
	} else if b.ClientId != nil {
		return 1
	}

	if self.LocationGroupId != nil && b.LocationGroupId != nil {
		if c := self.LocationGroupId.Cmp(b.LocationGroupId); c != 0 {
			return c
		}
	} else if self.LocationGroupId != nil {
		return -1
	} else if b.LocationGroupId != nil {
		return 1
	}

	if self.LocationId != nil && b.LocationId != nil {
		if c := self.LocationId.Cmp(b.LocationId); c != 0 {
			return c
		}
	} else if self.LocationId != nil {
		return -1
	} else if b.LocationId != nil {
		return 1
	}

	if self.BestAvailable != b.BestAvailable {
		if self.BestAvailable {
			return -1
		} else {
			return 1
		}
	}

	return 0
}

func (self *ConnectLocationId) String() string {
	jsonBytes, err := json.Marshal(self)
	if err != nil {
		panic(err)
	}
	return string(jsonBytes)
}

type ProvideSecretKey struct {
	ProvideMode      ProvideMode `json:"provide_mode"`
	ProvideSecretKey string      `json:"provide_secret_key"`
}

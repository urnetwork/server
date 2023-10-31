package client


import (
	"bringyour.com/protocol"
	"bringyour.com/connect"

	"bringyour.com/bringyour/model"
)


// note: publicly exported types must be fully contained in the `client` package tree
// the `gomobile` native interface compiler won't be able to map types otherwise
// a number of types (struct, function, interface) are redefined in `client`,
// somtimes in a simplified way, and then internally converted back to the native type
// examples:
// - fixed primitive arrays are not exportable. Use slices instead.
// - raw structs are not exportable. Use pointers to structs instead.
// - redefined primitive types are not exportable. Use type aliases instead.
// - arrays of structs are not exportable. See https://github.com/golang/go/issues/13445
//   use the "ExportableList" workaround from `gomobile.go`


// this value is set via the linker, e.g.
// -ldflags "-X client.Version=$WARP_VERSION"
var Version string


type Id = []byte


type Path struct {
	ClientId Id
	StreamId Id
}

func NewPath(clientId Id, streamId Id) *Path {
	return &Path{
		ClientId: clientId,
		StreamId: streamId,
	}
}

func fromConnectPath(path connect.Path) *Path {
	return &Path{
		ClientId: Id(path.ClientId.Bytes()),
		StreamId: Id(path.StreamId.Bytes()),
	}
}

func (self *Path) toConnectPath() connect.Path {
	clientId, err := connect.IdFromBytes(self.ClientId)
	if err != nil {
		panic(err)
	}
	streamId, err := connect.IdFromBytes(self.StreamId)
	if err != nil {
		panic(err)
	}
	return connect.Path{
		ClientId: clientId,
		StreamId: streamId,
	}
}


type ProvideMode = int
const (
	ProvideModeNone ProvideMode = ProvideMode(protocol.ProvideMode_None)
	ProvideModeNetwork ProvideMode = ProvideMode(protocol.ProvideMode_Network)
	ProvideModePublic ProvideMode = ProvideMode(protocol.ProvideMode_Public)
)


type LocationType = string
const (
    LocationTypeCity LocationType = LocationType(model.LocationTypeCity)
    LocationTypeRegion LocationType = LocationType(model.LocationTypeRegion)
    LocationTypeCountry LocationType = LocationType(model.LocationTypeCountry)
)

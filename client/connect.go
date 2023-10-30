package client


import (
	"bringyour.com/protocol"
	"bringyour.com/connect"
)


type Path struct {
	ClientId Id
	StreamId Id
}

func FromConnectPath(path connect.Path) Path {
	return Path{
		ClientId: Id(path.ClientId),
		StreamId: Id(path.StreamId),
	}
}

func (self *Path) ToConnectPath() connect.Path {
	return connect.Path{
		ClientId: connect.Id(self.ClientId),
		StreamId: connect.Id(self.StreamId),
	}
}


type ProvideMode int
const (
	ProvideModeNone ProvideMode = ProvideMode(protocol.ProvideMode_None)
	ProvideModeNetwork ProvideMode = ProvideMode(protocol.ProvideMode_Network)
	ProvideModePublic ProvideMode = ProvideMode(protocol.ProvideMode_Public)
)

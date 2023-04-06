package bringyour

import (
	"net/http"
)


type Ipv4 []byte


type ClientSession struct {
	ClientIpv4 *Ipv4
}

func NewClientSessionFromRequest(req http.Request) *ClientSession {
	// fixme pull out the origin ip from the header
	// otherwise just use the ip on the request
}

func (self ClientSession) ClientIpv4DotNotation() *string {
	if self.ClientIpv4 == nil {
		return nil
	}

	return fmt.Sprintf(
		"%u.%u.%u.%u",
		self.ClientIpv4[0],
		self.ClientIpv4[1],
		self.ClientIpv4[2],
		self.ClientIpv4[3]
	)
}

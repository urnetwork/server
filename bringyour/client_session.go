package bringyour

import (
	"fmt"
	"net/http"

	"bringyour.com/bringyour/jwt"
)


type Ipv4 [4]byte


type ClientSession struct {
	ClientIpv4 *Ipv4
	ByJwt *jwt.ByJwt
}

func NewClientSessionFromRequest(req *http.Request) *ClientSession {
	// fixme pull out the origin ip from the header
	// otherwise just use the ip on the request
	// fixme
	return nil
}

func (self ClientSession) ClientIpv4DotNotation() *string {
	if self.ClientIpv4 == nil {
		return nil
	}

	ipv4DotNotation := fmt.Sprintf(
		"%u.%u.%u.%u",
		self.ClientIpv4[0],
		self.ClientIpv4[1],
		self.ClientIpv4[2],
		self.ClientIpv4[3],
	)
	return &ipv4DotNotation
}

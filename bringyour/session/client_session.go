package session

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
	session := ClientSession{}
	
	clientIpv4DotNotation := req.Header.Get("X-Forwarded-For")
	if clientIpv4DotNotation != "" {
		var ipv4 Ipv4
		_, err := fmt.Sscanf(
			clientIpv4DotNotation,
			"%u.%u.%u.%u",
			&ipv4[0],
			&ipv4[1],
			&ipv4[2],
			&ipv4[3],
		)
		if err == nil {
			session.ClientIpv4 = &ipv4
		}
	}
	if session.ClientIpv4 == nil {
		// either the load balancer is not correctly configured or there is no load balancer
		var ipv4 Ipv4
		var port int
		_, err := fmt.Sscanf(
			req.RemoteAddr,
			"%u.%u.%u.%u:%u",
			&ipv4[0],
			&ipv4[1],
			&ipv4[2],
			&ipv4[3],
			&port,
		)
		if err == nil {
			session.ClientIpv4 = &ipv4
		}
	}

	return &session
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

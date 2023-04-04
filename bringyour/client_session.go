package bringyour

import (
	"net/http"
)


type Ipv4 []byte


type ClientSession struct {
	ClientIpv4 Ipv4
}

func NewClientSessionFromRequest(req http.Request) *ClientSession {
	// fixme pull out the origin ip from the header
	// otherwise just use the ip on the request
}

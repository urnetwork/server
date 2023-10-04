package session

import (
	"context"
	"net/http"
	"strings"
	"strconv"

	"bringyour.com/bringyour/jwt"
)


type ClientSession struct {
	Ctx context.Context
	Cancel context.CancelFunc
	// ip:port
	ClientAddress string
	ByJwt *jwt.ByJwt
}

func NewClientSessionFromRequest(req *http.Request) *ClientSession {
	cancelCtx, cancel := context.WithCancel(req.Context())

	clientAddress := req.Header.Get("X-Forwarded-For")
	if clientAddress == "" {
		clientAddress = req.RemoteAddr
	}

	return &ClientSession{
		Ctx: cancelCtx,
		Cancel: cancel,
		ClientAddress: clientAddress,
	}
}

func (self *ClientSession) ClientIpPort() (ip string, port int) {
	parts := strings.Split(self.ClientAddress, ":")
	ip = parts[0]
	if 1 < len(parts) {
		port, _ = strconv.Atoi(parts[1])
	}
	return
}

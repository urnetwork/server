package datasource

import (
	"context"
	"fmt"
	"net"
	"time"
)

func PingPort(ctx context.Context, ip net.IP, port int) bool {
	dialer := net.Dialer{
		Timeout: 1 * time.Second,
	}

	c, err := dialer.DialContext(ctx, "tcp", fmt.Sprintf("%s:%d", ip, port))
	if err != nil {
		return false
	}
	c.Close()
	return true

}

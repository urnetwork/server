// Redis dial failures measured where the pool maps the requested address and
// receives the dial result. Background pool work and retries use this path too.
package server

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"

	"github.com/prometheus/client_golang/prometheus"
)

var redisDialFailures = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_redis_dial_failures_total",
	Help: "Failed Redis pool dial calls, including retries and background dials, by selected address relative to the configured authority and returned error kind.",
}, []string{"route", "reason"})

// Compare the actual dial argument with the configured authority. The labels
// describe address selection before DNS, forwarding, or listener acceptance.
func redisDialFailureRoute(authorityHost, authorityPort, requestedAddr, dialAddr string) string {
	requestedHost, _, requestedErr := net.SplitHostPort(requestedAddr)
	dialHost, dialPort, dialErr := net.SplitHostPort(dialAddr)
	switch {
	case requestedErr != nil || dialErr != nil || authorityHost == "" || authorityPort == "" || dialHost == "" || dialPort == "":
		return "unknown"
	case dialHost != authorityHost:
		return "other_host"
	case requestedHost == "" || requestedHost == "?":
		return "missing_host_fallback"
	case redisLoopbackHost(authorityHost) && requestedHost != authorityHost:
		return "loopback_override"
	case dialPort == authorityPort:
		return "authority_endpoint"
	default:
		return "authority_host_other_port"
	}
}

// Only a returned typed cause selects a reason. A context that ended while
// a different error was being returned cannot replace that error.
func redisDialFailureReason(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		return "timeout"
	default:
		return "other"
	}
}

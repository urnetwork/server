package controller

// The per-client limit of POST /connect/control (model/connect_control_limit.go),
// applied when a request's credential is verified and before its client state
// query, alongside the recorded refusal replay
// (router.WrapWithInputRequireClientUnlessAnswered). The limit is keyed on
// the client id of the verified credential. A client over its limit is
// answered 429 with Retry-After, without the state query: its excess costs
// the api a signature check and one Redis round trip.
//
// An address in the deployment's exclude_subnets (server.IsRateLimitExcluded)
// is infrastructure, exempt from every caller limit as at the load balancer,
// and spends no budget. A Redis failure admits the request undecided.
//
// urnetwork_connect_control_client_limit_total counts every decision.

import (
	"context"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

var connectControlClientLimitCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_control_client_limit_total",
	Help: "Per-client /connect/control limit decisions for verified client credentials: allowed, limited (answered 429 before the client state query), excluded (an infrastructure address, not counted) and unavailable (Redis failed or timed out; admitted undecided).",
}, []string{"outcome"})

func init() {
	for _, outcome := range []string{"allowed", "limited", "excluded", "unavailable"} {
		connectControlClientLimitCounter.WithLabelValues(outcome)
	}
	prometheus.MustRegister(connectControlClientLimitCounter)
}

// The seams the tests replace: the limit, the clock the limit reads (-1 is
// the Redis node's clock), the decision and the infrastructure check.
var (
	connectControlLimitSettings  = model.ConnectControlLimitSettingsForProcess
	connectControlLimitNowMicros = func() int64 { return -1 }
	connectControlLimitAdmit     = model.AdmitConnectControlAt
	connectControlLimitExcluded  = server.IsRateLimitExcluded
)

// Applies settings and a fixed clock to the limit until the returned restore
// is called. A nil nowMicros keeps the Redis node's clock.
func Testing_SetConnectControlLimit(settings model.ConnectControlLimitSettings, nowMicros func() int64) (restore func()) {
	previousSettings, previousNowMicros := connectControlLimitSettings, connectControlLimitNowMicros
	connectControlLimitSettings = func() model.ConnectControlLimitSettings { return settings }
	if nowMicros != nil {
		connectControlLimitNowMicros = nowMicros
	}
	return func() {
		connectControlLimitSettings, connectControlLimitNowMicros = previousSettings, previousNowMicros
	}
}

// One unavailable decision is logged a minute, with the count since the last.
var (
	connectControlLimitUnavailableLogTime atomic.Int64
	connectControlLimitUnavailableCount   atomic.Int64
)

func logConnectControlLimitUnavailable(err error) {
	count := connectControlLimitUnavailableCount.Add(1)
	now := time.Now().Unix()
	last := connectControlLimitUnavailableLogTime.Load()
	if now-last < 60 || !connectControlLimitUnavailableLogTime.CompareAndSwap(last, now) {
		return
	}
	connectControlLimitUnavailableCount.Add(-count)
	glog.Warningf("[connect]control limit unavailable; %d requests admitted undecided: %s\n", count, err)
}

// Whether the request's address is infrastructure, from the address its
// session resolved. A context with no request session is not.
func connectControlLimitExcludedAddress(ctx context.Context) bool {
	clientAddress, ok := session.ClientAddressFromContext(ctx)
	if !ok {
		return false
	}
	clientIp, _, err := server.SplitClientAddress(clientAddress)
	if err != nil {
		return false
	}
	addr, err := netip.ParseAddr(clientIp)
	if err != nil {
		return false
	}
	return connectControlLimitExcluded(addr)
}

// Decides whether the client is over its limit, and if so how long until its
// next request would be admitted.
func checkConnectControlLimit(ctx context.Context, clientId server.Id) (limited bool, retryAfter time.Duration) {
	if connectControlLimitExcludedAddress(ctx) {
		connectControlClientLimitCounter.WithLabelValues("excluded").Inc()
		return false, 0
	}
	result, err := connectControlLimitAdmit(ctx, clientId, connectControlLimitSettings(), connectControlLimitNowMicros())
	if err != nil {
		connectControlClientLimitCounter.WithLabelValues("unavailable").Inc()
		logConnectControlLimitUnavailable(err)
		return false, 0
	}
	if !result.Allowed {
		connectControlClientLimitCounter.WithLabelValues("limited").Inc()
		if glog.V(1) {
			glog.Infof("[connect][%s]control limited, retry after %s\n", clientId, result.RetryAfter)
		}
		return true, result.RetryAfter
	}
	connectControlClientLimitCounter.WithLabelValues("allowed").Inc()
	return false, 0
}

// How a control request is answered before its client state query: refused
// as over the client's limit, or with the refusals recorded for it.
type ConnectControlAnswer struct {
	limited    bool
	retryAfter time.Duration
	refusals   *ConnectControlRefusals
}

// The prepare of router.WrapWithInputRequireClientUnlessAnswered for
// /connect/control. A request over its client's limit is answered with a
// 429. Every other request counts against the limit and then may be answered
// from the refusals recorded for it (PrepareConnectControlRefusals).
func PrepareConnectControl(ctx context.Context, clientId server.Id, connectControl *ConnectControlArgs) (*ConnectControlAnswer, bool) {
	if limited, retryAfter := checkConnectControlLimit(ctx, clientId); limited {
		return &ConnectControlAnswer{limited: true, retryAfter: retryAfter}, true
	}
	refusals, ok := PrepareConnectControlRefusals(ctx, clientId, connectControl)
	if !ok {
		return nil, false
	}
	return &ConnectControlAnswer{refusals: refusals}, true
}

// The reply of router.WrapWithInputRequireClientUnlessAnswered for
// /connect/control.
func ReplyConnectControl(ctx context.Context, answer *ConnectControlAnswer) (*ConnectControlResult, error) {
	if answer.limited {
		return nil, model.NewConnectControlLimitError(answer.retryAfter)
	}
	return ReplayConnectControlRefusals(ctx, answer.refusals)
}

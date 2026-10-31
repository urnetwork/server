package controller

// A control pack whose every frame is a PeerAudit is answered empty from its
// body alone (CPUOPT3 D3). No server code has ever consumed an audit: connect
// added them in 2023 (26d51ab3) for a peer acl that was never built, and since
// out-of-band control began in 2024-04 every connect client has sent one per
// live receive sequence a minute, one per malformed inbound frame, and one
// per packet a provider's egress policy calls an incident (Client.ReportAbuse,
// with abuse set). Each was authenticated, counted against its client's
// limit, checked against its revocation record and refused with "Cannot
// handle oob control message". In 2026-10 they were a quarter of the api's
// control requests, 87% of alt's, and two thirds of the proxy's credential
// checks.
//
// The answer is {"pack":"","error":null}, exactly what an authenticated pack
// that does nothing is answered. It is the same for every caller, smaller
// than the request, and grants nothing. Any other pack takes the path it
// took before, check for check, and a PeerAudit inside one is a no-op in
// ConnectControlFrames.
//
// The checks each path keeps, with owner question 1 answered (a), before
// authentication:
//
//	path                    checks kept                           checks skipped
//	api/alt, audit-only     per-address limits (nginx              signature and claims, session
//	                        route_rate_limits, alt/rate_limit.go), checks, per-client limit,
//	                        intended network gate, lag.dev guest   revocation record, dispatch
//	                        gate, 16 MiB body cap, then the 2 KiB
//	                        cap and the shape check
//	api/alt, any other pack all, unchanged                         none
//	proxy, audit-only       lifecycle checks and the shape check   signature and claims, contract
//	                                                               admission, revocation record,
//	                                                               dispatch
//	proxy, any other pack   all, unchanged                         none
//
// Under answer (b), after the signature, the api handler passes no discard
// and PrepareConnectControl calls DiscardConnectControl first, before the
// per-client limit; the proxy calls DiscardConnectControlPack after
// parseClaimsPreflight. Each path then also keeps the signature, claims and
// session checks.
//
// No bound changes for a request that does anything: the authentication
// deadline, the intended network check's deadline, the per-client limit's
// parameters, the revocation record and the refusal ring all apply as
// before. The discard does no I/O on at most 2 KiB, so it needs no deadline.
// A revoked credential's audit is answered 200 and granted nothing, as its
// recorded refusals already are (connect_control_refusal_replay.go), and
// every other request it makes is refused.
//
// urnetwork_connect_control_discarded_requests_total counts the requests
// answered here. Nothing is logged at any level: the counter is lossless and
// an audit carries peer ids.

import (
	"encoding/base64"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"
)

// The longest encoded pack the discard decodes. A connect client sends one
// audit per pack, about 80 characters; this admits about twenty.
const connectControlDiscardMaxPackLength = 2048

var connectControlDiscardCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_control_discarded_requests_total",
	Help: "Connect control requests whose every frame is a PeerAudit, answered empty before any credential, per-client limit, Redis or database read. abuse is claimed when any frame sets abuse: client-supplied and spoofable, telemetry only.",
}, []string{"abuse"})

var (
	connectControlDiscardAbuseClaimed = connectControlDiscardCounter.WithLabelValues("claimed")
	connectControlDiscardAbuseNone    = connectControlDiscardCounter.WithLabelValues("none")
)

func init() {
	prometheus.MustRegister(connectControlDiscardCounter)
}

// Reports whether a control pack is discarded whole, and counts it when it
// is: at most connectControlDiscardMaxPackLength characters of standard
// base64 that decode to a pack of at least one frame, every one of type
// PeerAudit with bytes that decode as one. Reads only its argument, never a
// credential, the clock, Redis or the database. Anything else is false and
// takes the ordinary path, which refuses what does not decode.
func DiscardConnectControlPack(encodedPack string) bool {
	if connectControlDiscardMaxPackLength < len(encodedPack) {
		return false
	}
	// a plain decode: the frames must not borrow a pooled buffer
	packBytes, err := base64.StdEncoding.DecodeString(encodedPack)
	if err != nil {
		return false
	}
	pack := &protocol.Pack{}
	if err := proto.Unmarshal(packBytes, pack); err != nil || len(pack.Frames) == 0 {
		return false
	}
	abuse := false
	for _, frame := range pack.Frames {
		if frame.GetMessageType() != protocol.MessageType_TransferPeerAudit {
			return false
		}
		// the dispatch's own decode, so the discard answers only what
		// ConnectControlFrames would take for an audit
		message, err := connect.FromFrame(frame)
		if err != nil {
			return false
		}
		peerAudit, ok := message.(*protocol.PeerAudit)
		if !ok {
			return false
		}
		abuse = abuse || peerAudit.GetAbuse()
	}
	if abuse {
		connectControlDiscardAbuseClaimed.Inc()
	} else {
		connectControlDiscardAbuseNone.Inc()
	}
	return true
}

// The discard of router.WrapWithInputRequireClientUnlessAnswered for
// /connect/control, run before any credential is read. A discarded request is
// replied to by ReplyConnectControl with the empty answer.
func DiscardConnectControl(connectControl *ConnectControlArgs) (*ConnectControlAnswer, bool) {
	if connectControl == nil || !DiscardConnectControlPack(connectControl.Pack) {
		return nil, false
	}
	return &ConnectControlAnswer{discarded: true}, true
}

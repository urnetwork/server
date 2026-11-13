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
//	                        gate, 16 MiB body cap, then the 512-
//	                        character cap and the shape check
//	api/alt, any other pack all, unchanged                         none
//	proxy, audit-only       lifecycle checks and the shape check   signature and claims, contract
//	                                                               admission, revocation record,
//	                                                               dispatch
//	proxy, any other pack   all, unchanged                         none
//
// The api handler passes DiscardConnectControl to its route's wrapper.
// Passing nil turns the discard off: a pack of one audit then authenticates,
// spends its client's limit and is answered by the dispatch's no-op, as every
// other audit-only pack is. The proxy's authority calls
// DiscardConnectControlPack directly; without that call it behaves the same
// way.
//
// No bound changes for a request that does anything: the authentication
// deadline, the intended network check's deadline, the per-client limit's
// parameters, the revocation record and the refusal ring all apply as
// before. The discard does no I/O on at most 512 characters, so it needs no
// deadline.
// A revoked credential's audit is answered 200 and granted nothing, as its
// recorded refusals already are (connect_control_refusal_replay.go), and
// every other request it makes is refused.
//
// urnetwork_connect_control_discarded_requests_total counts the requests
// answered here, by whether the audit claims abuse and whether the request
// presented a bearer credential. Both are read before authentication, so
// anyone can raise either: they size the discards, they prove nothing about
// who sent them. Nothing is logged at any level: the counter is lossless and
// an audit carries peer ids.

import (
	"encoding/base64"
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"
)

// The Authorization scheme of a client's credential (session.authBearerPrefix).
const connectControlDiscardBearerPrefix = "Bearer "

// The longest encoded pack the discard decodes. Every connect version sends
// one audit per pack: about 80 characters, and about 220 with every counter
// at its largest.
const connectControlDiscardMaxPackLength = 512

// The fields the scan reads, from the generated descriptors, so the scan and
// the dispatch's decoders read one schema.
var (
	connectControlDiscardPackFrames        = connectControlDiscardField(&protocol.Pack{}, "frames").Number()
	connectControlDiscardFrameMessageType  = connectControlDiscardField(&protocol.Frame{}, "message_type").Number()
	connectControlDiscardFrameMessageBytes = connectControlDiscardField(&protocol.Frame{}, "message_bytes").Number()
	connectControlDiscardAuditAbuse        = connectControlDiscardField(&protocol.PeerAudit{}, "abuse").Number()
	// every PeerAudit field, by number, with the wire type of its kind
	connectControlDiscardAuditWireTypes = func() map[protowire.Number]protowire.Type {
		wireTypes := map[protowire.Number]protowire.Type{}
		fields := (&protocol.PeerAudit{}).ProtoReflect().Descriptor().Fields()
		for i := range fields.Len() {
			field := fields.Get(i)
			if field.IsList() || field.IsMap() {
				panic("a PeerAudit field the discard scan does not read: " + string(field.Name()))
			}
			switch field.Kind() {
			case protoreflect.BytesKind, protoreflect.StringKind:
				wireTypes[field.Number()] = protowire.BytesType
			case protoreflect.BoolKind, protoreflect.EnumKind, protoreflect.Int32Kind, protoreflect.Int64Kind,
				protoreflect.Uint32Kind, protoreflect.Uint64Kind:
				wireTypes[field.Number()] = protowire.VarintType
			default:
				panic("a PeerAudit field the discard scan does not read: " + string(field.Name()))
			}
		}
		return wireTypes
	}()
)

// The named field of message's schema. The scan reads no field the schema
// lacks, so a renamed field fails at startup rather than refusing every audit.
func connectControlDiscardField(message proto.Message, name protoreflect.Name) protoreflect.FieldDescriptor {
	field := message.ProtoReflect().Descriptor().Fields().ByName(name)
	if field == nil {
		panic("the discard scan reads a field the schema does not have: " + string(name))
	}
	return field
}

var connectControlDiscardCounter = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "urnetwork_connect_control_discarded_requests_total",
	Help: "Connect control requests of one PeerAudit frame, answered empty before any credential, per-client limit, Redis or database read. abuse is claimed when the audit sets abuse; bearer is present when the request presented a non-empty bearer credential, which is not verified. Both are client-supplied telemetry with no authority.",
}, []string{"abuse", "bearer"})

// The four children, by abuse claimed and bearer present.
var connectControlDiscardCounts = [2][2]prometheus.Counter{
	{connectControlDiscardCounter.WithLabelValues("none", "absent"), connectControlDiscardCounter.WithLabelValues("none", "present")},
	{connectControlDiscardCounter.WithLabelValues("claimed", "absent"), connectControlDiscardCounter.WithLabelValues("claimed", "present")},
}

func init() {
	prometheus.MustRegister(connectControlDiscardCounter)
}

// Reports whether a control pack is discarded whole, and counts it when it
// is: at most connectControlDiscardMaxPackLength characters of standard
// base64 that decode to a pack of exactly one frame, of type PeerAudit, with
// bytes that decode as one. Reads only its argument, never a credential, the
// clock, Redis or the database. Anything else is false and takes the
// ordinary path: an audit-only pack that is longer or has more frames
// authenticates and is answered by the dispatch's no-op, and what does not
// decode is refused there.
//
// The wire scan rejects every other shape before anything is decoded, so a
// refused input costs one base64 decode, and an answered one a fixed handful
// of small allocations whatever its content
// (TestDiscardConnectControlCostIsBoundedAtTheCap).
//
// bearerPresented labels the count only: whether the request presented a
// bearer credential, read and never verified.
func DiscardConnectControlPack(encodedPack string, bearerPresented bool) bool {
	if connectControlDiscardMaxPackLength < len(encodedPack) {
		return false
	}
	// a plain decode: the frames must not borrow a pooled buffer
	packBytes, err := base64.StdEncoding.DecodeString(encodedPack)
	if err != nil {
		return false
	}
	frameBytes, abuse, ok := scanConnectControlAuditPack(packBytes)
	if !ok {
		return false
	}
	// the dispatch's own decoders, so the discard answers only what
	// ConnectControlFrames would take for an audit
	frame := &protocol.Frame{}
	if err := proto.Unmarshal(frameBytes, frame); err != nil {
		return false
	}
	message, err := connect.FromFrame(frame)
	if err != nil {
		return false
	}
	peerAudit, ok := message.(*protocol.PeerAudit)
	if !ok || peerAudit.GetAbuse() != abuse {
		return false
	}
	abuseIndex, bearerIndex := 0, 0
	if abuse {
		abuseIndex = 1
	}
	if bearerPresented {
		bearerIndex = 1
	}
	connectControlDiscardCounts[abuseIndex][bearerIndex].Inc()
	return true
}

// Scans a decoded pack without allocating. The pack must be exactly one
// frames record. Its frame must carry exactly one message type,
// TransferPeerAudit, at most one message bytes field, and nothing else. The
// audit must carry each of its fields at most once, with the wire type of the
// field's kind, and nothing else. Returns the frame's bytes and whether the
// audit claims abuse.
func scanConnectControlAuditPack(packBytes []byte) (frameBytes []byte, abuse bool, ok bool) {
	number, wireType, n := protowire.ConsumeTag(packBytes)
	if n < 0 || number != connectControlDiscardPackFrames || wireType != protowire.BytesType {
		return nil, false, false
	}
	frameBytes, m := protowire.ConsumeBytes(packBytes[n:])
	if m < 0 || n+m != len(packBytes) {
		return nil, false, false
	}

	var auditBytes []byte
	typed, carried := false, false
	for b := frameBytes; 0 < len(b); {
		number, wireType, n := protowire.ConsumeTag(b)
		if n < 0 {
			return nil, false, false
		}
		b = b[n:]
		switch {
		case number == connectControlDiscardFrameMessageType && wireType == protowire.VarintType && !typed:
			messageType, m := protowire.ConsumeVarint(b)
			if m < 0 || messageType != uint64(protocol.MessageType_TransferPeerAudit) {
				return nil, false, false
			}
			typed, b = true, b[m:]
		case number == connectControlDiscardFrameMessageBytes && wireType == protowire.BytesType && !carried:
			messageBytes, m := protowire.ConsumeBytes(b)
			if m < 0 {
				return nil, false, false
			}
			auditBytes, carried, b = messageBytes, true, b[m:]
		default:
			return nil, false, false
		}
	}
	if !typed {
		return nil, false, false
	}

	// field numbers below 64 fit the mask; PeerAudit has 13
	var seen uint64
	for b := auditBytes; 0 < len(b); {
		number, wireType, n := protowire.ConsumeTag(b)
		if n < 0 || 64 <= number {
			return nil, false, false
		}
		expected, known := connectControlDiscardAuditWireTypes[number]
		if !known || wireType != expected || seen&(1<<number) != 0 {
			return nil, false, false
		}
		seen |= 1 << number
		m := protowire.ConsumeFieldValue(number, wireType, b[n:])
		if m < 0 {
			return nil, false, false
		}
		if number == connectControlDiscardAuditAbuse {
			value, _ := protowire.ConsumeVarint(b[n:])
			abuse = value != 0
		}
		b = b[n+m:]
	}
	return frameBytes, abuse, true
}

// The discard of router.WrapWithInputRequireClientUnlessAnswered for
// /connect/control, run before any credential is read. A discarded request is
// replied to by ReplyConnectControl with the empty answer. The request's
// Authorization header labels the count and is not verified.
func DiscardConnectControl(req *http.Request, connectControl *ConnectControlArgs) (*ConnectControlAnswer, bool) {
	if connectControl == nil {
		return nil, false
	}
	authorization := req.Header.Get("Authorization")
	bearerPresented := strings.HasPrefix(authorization, connectControlDiscardBearerPrefix) && len(connectControlDiscardBearerPrefix) < len(authorization)
	if !DiscardConnectControlPack(connectControl.Pack, bearerPresented) {
		return nil, false
	}
	return &ConnectControlAnswer{discarded: true}, true
}

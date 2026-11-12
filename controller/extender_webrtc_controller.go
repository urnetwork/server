package controller

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The operator's side of the peer-to-peer webrtc extender carrier
// (connect/EXTENDER.md S; acceptance C2a, uptime C3, the relay S8).
//
// A webrtc test reaches the extender through the exchange signaling rather
// than at an address: the operator creates an SDP offer, the relay carries it
// to the exchange resident that holds the extender's client and on to the
// client, the client's extender answerer answers, and the probe runs over the
// data channel that ICE opens between this process and the extender. The
// probes themselves are connect's (ProbeExtenderWebRtcForward,
// ProbeExtenderWebRtcCarrier); what this file owns is the operator's half of
// the relay: the exchanger the probe's carrier signals through.
//
// Only the prober originates offers, and only for an extender it is testing:
// the activation, for the client the posting jwt names, once every check that
// refuses without a probe has passed and the activation's budgets are spent;
// the uptime task, for the client of an address it is probing. No route
// takes a client id to signal. Each offer is bounded in size, waits a bounded
// time, and is refused when the client already has offers pending; the
// exchange bounds the rest (connect/extender_webrtc_relay.go).
//
// The probe's ICE gathers on one family, the activation's or the address's
// (WebRtcSettings.IceIpVersion), since the extender's forward follows the
// family of the pair it was reached on (A7). Its checks go only to the
// answer's candidates on the address being tested -- the activation's source
// address, or the address an uptime probe judges, by its /32 or the /64 of an
// ipv6 one, as the camouflage and attribution rules compare an extender's
// address -- and to at most MaxAnswerCandidateCount of those, so an answer
// cannot point this host's checks anywhere else.

// ErrExtenderWebRtcSignalingUnavailable is the webrtc test of an operator
// with no signaling path to the extender.
var ErrExtenderWebRtcSignalingUnavailable = errors.New("the operator has no webrtc signaling path to the extender")

// ErrExtenderWebRtcClientNotConnected is the webrtc test of an extender whose
// client has no resident on the exchange, so nothing can relay an offer to
// it: the carrier signals through the exchange, never as the bootstrap.
var ErrExtenderWebRtcClientNotConnected = errors.New("the extender's client is not connected to the exchange, which the webrtc carrier signals through")

// ErrExtenderWebRtcOffersPending is the webrtc test of an extender whose
// client already has as many offers pending as one may.
var ErrExtenderWebRtcOffersPending = errors.New("the extender's client has webrtc offers pending already")

// ErrExtenderWebRtcNoAnswer is the webrtc test of an extender whose client
// did not answer the relayed offer in time.
var ErrExtenderWebRtcNoAnswer = errors.New("the extender's client did not answer the webrtc offer")

// ExtenderWebRtcProbeSettings bound the operator's webrtc probes and its half
// of the signaling relay (connect/EXTENDER.md S8).
type ExtenderWebRtcProbeSettings struct {
	// the most offers pending in one client's offer set; an offer past it is
	// not added and its probe fails at once. Two lets an activation of each
	// family, or an activation and the uptime probe, run at once.
	MaxPendingOfferCount int
	// how long a client's offer set lives past its newest offer; it collects
	// offers no resident took
	OfferTtl time.Duration
	// the longest a probe waits for its answer, within its context's own
	// deadline. The answer gathers the extender's candidates, which is
	// seconds; Redis counts the wait in whole seconds
	// (model.WaitExtenderWebRtcAnswer), so the wait is cut to whole seconds,
	// and the offer expires with it.
	AnswerTimeout time.Duration
	// the bound on removing an offer no resident took, once its probe stops
	// waiting
	OfferRemoveTimeout time.Duration
	// the most bytes of an offer's json the prober posts, and of an answer's
	// it takes, as the relay bounds them
	MaxOfferByteCount  int
	MaxAnswerByteCount int
	// the most of an answer's candidates on the tested address this host's
	// ICE checks go to; the rest, and every candidate on any other address,
	// are dropped before the answer applies. An answer names its candidates,
	// so without the bound and the address one answer could point this host's
	// checks at as many addresses as fit in it.
	MaxAnswerCandidateCount int
}

func DefaultExtenderWebRtcProbeSettings() *ExtenderWebRtcProbeSettings {
	return &ExtenderWebRtcProbeSettings{
		MaxPendingOfferCount:    2,
		OfferTtl:                30 * time.Second,
		AnswerTimeout:           8 * time.Second,
		OfferRemoveTimeout:      2 * time.Second,
		MaxOfferByteCount:       16 * 1024,
		MaxAnswerByteCount:      16 * 1024,
		MaxAnswerCandidateCount: 8,
	}
}

var defaultExtenderWebRtcProbeSettings = sync.OnceValue(DefaultExtenderWebRtcProbeSettings)

// What the prober's exchanger signals through: the relay's Redis keys, the
// resident registry and the exchange processes' offer channels in
// production, a fake in a test.
type extenderWebRtcProbeRelay interface {
	// the offer channel of the exchange process holding the client's
	// resident, and whether the client has one
	clientResident(ctx context.Context, clientId server.Id) (channel string, ok bool)
	// wakes that process for the client's pending offers. Advisory: a lost
	// wake leaves the offer to the process's resync takes or to expire
	wake(ctx context.Context, channel string, clientId server.Id)
	addOffer(
		ctx context.Context,
		clientId server.Id,
		offer *model.ExtenderWebRtcOffer,
		maxPendingCount int,
		ttl time.Duration,
	) (member string, added bool)
	removeOffer(ctx context.Context, clientId server.Id, member string)
	waitAnswer(
		ctx context.Context,
		clientId server.Id,
		requestId server.Id,
		timeout time.Duration,
	) (*model.ExtenderWebRtcAnswer, bool)
}

type extenderWebRtcProbeRedisRelay struct{}

// A client is connected when it has a resident (model.GetResidentForClient),
// which is where the platform locates a client: the resident's registration
// names the exchange process holding it, whose offer channel this is.
func (extenderWebRtcProbeRedisRelay) clientResident(ctx context.Context, clientId server.Id) (string, bool) {
	resident := model.GetResidentForClient(ctx, clientId, 0)
	if resident == nil {
		return "", false
	}
	return model.ExtenderWebRtcOfferChannelForResident(resident), true
}

func (extenderWebRtcProbeRedisRelay) wake(ctx context.Context, channel string, clientId server.Id) {
	if err := model.PublishExtenderWebRtcOffer(ctx, channel, clientId); err != nil {
		glog.Infof("[extender]webrtc offer wake for %s not published = %s\n", clientId, err)
	}
}

func (extenderWebRtcProbeRedisRelay) addOffer(
	ctx context.Context,
	clientId server.Id,
	offer *model.ExtenderWebRtcOffer,
	maxPendingCount int,
	ttl time.Duration,
) (string, bool) {
	return model.AddExtenderWebRtcOffer(ctx, clientId, offer, maxPendingCount, ttl)
}

func (extenderWebRtcProbeRedisRelay) removeOffer(ctx context.Context, clientId server.Id, member string) {
	model.RemoveExtenderWebRtcOffer(ctx, clientId, member)
}

func (extenderWebRtcProbeRedisRelay) waitAnswer(
	ctx context.Context,
	clientId server.Id,
	requestId server.Id,
	timeout time.Duration,
) (*model.ExtenderWebRtcAnswer, bool) {
	return model.WaitExtenderWebRtcAnswer(ctx, clientId, requestId, timeout)
}

// The exchanger of one probe (connect.WebRtcExtenderOfferExchanger): its one
// offer to the one extender client it was built for, whose answer's
// candidates it keeps to the tested address.
type extenderWebRtcRelayExchanger struct {
	clientId server.Id
	// the address being tested: the activation's source address, or the
	// address an uptime probe judges
	testedIp netip.Addr
	settings *ExtenderWebRtcProbeSettings
	relay    extenderWebRtcProbeRelay
	now      func() time.Time
}

// Relays the offer to the client and returns its answer, or why it has none.
// Every Redis call raises on a failure, which is the probe's error here.
func (self *extenderWebRtcRelayExchanger) ExchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (answer webrtc.SessionDescription, returnErr error) {
	if r := server.HandleError(func() {
		answer, returnErr = self.exchangeOffer(ctx, offer)
	}); r != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc relay failed: %v", r)
	}
	return
}

func (self *extenderWebRtcRelayExchanger) exchangeOffer(
	ctx context.Context,
	offer webrtc.SessionDescription,
) (webrtc.SessionDescription, error) {
	offerBytes, err := json.Marshal(&offer)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}
	if self.settings.MaxOfferByteCount < len(offerBytes) {
		return webrtc.SessionDescription{}, fmt.Errorf(
			"the webrtc offer is %d bytes, more than the relay's %d",
			len(offerBytes),
			self.settings.MaxOfferByteCount,
		)
	}
	channel, connected := self.relay.clientResident(ctx, self.clientId)
	if !connected {
		return webrtc.SessionDescription{}, ErrExtenderWebRtcClientNotConnected
	}

	now := self.now()
	wait := self.settings.AnswerTimeout
	if deadline, ok := ctx.Deadline(); ok {
		wait = min(wait, deadline.Sub(now))
	}
	if wait <= 0 {
		return webrtc.SessionDescription{}, context.DeadlineExceeded
	}
	// Redis waits whole seconds, at least one, and the offer expires when the
	// wait ends, so the resident never answers a probe that stopped waiting
	wait = max(time.Second, wait.Truncate(time.Second))
	relayOffer := &model.ExtenderWebRtcOffer{
		RequestId: server.NewId(),
		// the resident relays nothing past the moment this probe stops
		// waiting, and bounds its exchange with the client by it
		ExpireTimeMs: now.Add(wait).UnixMilli(),
		Offer:        string(offerBytes),
	}
	member, added := self.relay.addOffer(
		ctx,
		self.clientId,
		relayOffer,
		self.settings.MaxPendingOfferCount,
		self.settings.OfferTtl,
	)
	if !added {
		return webrtc.SessionDescription{}, ErrExtenderWebRtcOffersPending
	}
	self.relay.wake(ctx, channel, self.clientId)
	relayAnswer, ok := self.relay.waitAnswer(ctx, self.clientId, relayOffer.RequestId, wait)
	if !ok {
		// a resident that never took the offer must not relay it later, to a
		// probe that is gone
		server.HandleError(func() {
			removeCtx, removeCancel := context.WithTimeout(context.WithoutCancel(ctx), self.settings.OfferRemoveTimeout)
			defer removeCancel()
			self.relay.removeOffer(removeCtx, self.clientId, member)
		})
		return webrtc.SessionDescription{}, ErrExtenderWebRtcNoAnswer
	}
	if relayAnswer.Error != "" {
		if relayAnswer.Error == model.ExtenderWebRtcRelayErrorNoAnswer {
			return webrtc.SessionDescription{}, ErrExtenderWebRtcNoAnswer
		}
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc relay refused the offer: %s", relayAnswer.Error)
	}
	if self.settings.MaxAnswerByteCount < len(relayAnswer.Answer) {
		return webrtc.SessionDescription{}, fmt.Errorf(
			"the webrtc answer is %d bytes, more than the relay's %d",
			len(relayAnswer.Answer),
			self.settings.MaxAnswerByteCount,
		)
	}
	var answer webrtc.SessionDescription
	if err := json.Unmarshal([]byte(relayAnswer.Answer), &answer); err != nil || answer.Type != webrtc.SDPTypeAnswer {
		return webrtc.SessionDescription{}, fmt.Errorf("the webrtc answer is not an answer")
	}
	answer.SDP = boundExtenderWebRtcAnswerCandidates(answer.SDP, self.testedIp, self.settings.MaxAnswerCandidateCount)
	return answer, nil
}

// The candidate lines of an answer this host's ICE checks may go to: those on
// the tested address alone -- its /32, or the /64 of an ipv6 address
// (extenderCamouflageAddressPrefix), since a host behind nat answers from the
// public address it activated from, and an ipv6 host may gather another
// address of its /64 -- and public, at most maxCandidateCount of them, in the
// answer's own order. Every other line is kept as it is. A tested address
// that is not valid keeps no candidate. connect's own filter, which the
// carrier applies after this, still keeps the checks off this host's own
// addresses and links.
func boundExtenderWebRtcAnswerCandidates(sdp string, testedIp netip.Addr, maxCandidateCount int) string {
	testedPrefix := extenderCamouflageAddressPrefix(testedIp)
	lines := strings.SplitAfter(sdp, "\n")
	kept := make([]string, 0, len(lines))
	candidateCount := 0
	for _, line := range lines {
		candidateLine, ok := strings.CutPrefix(strings.TrimRight(line, "\r\n"), "a=")
		if !ok || !strings.HasPrefix(candidateLine, "candidate:") {
			kept = append(kept, line)
			continue
		}
		if maxCandidateCount <= candidateCount {
			continue
		}
		candidate, err := ice.UnmarshalCandidate(candidateLine)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(candidate.Address())
		if err != nil {
			// a host name: nothing here resolves it
			continue
		}
		addr = addr.Unmap()
		if !addr.IsGlobalUnicast() || addr.IsPrivate() {
			continue
		}
		if !testedPrefix.IsValid() || !testedPrefix.Contains(addr) {
			continue
		}
		candidateCount += 1
		kept = append(kept, line)
	}
	return strings.Join(kept, "")
}

// The connect settings of one webrtc probe of one extender: the probe dial
// settings with a carrier whose only extender is the one being proved,
// signaled through the relay to its client, its ICE on ipVersion alone and
// its checks on testedIp alone. The close releases the carrier; a probe
// builds its own, so its sessions end with it.
func newExtenderWebRtcProbeConnectSettings(
	ctx context.Context,
	settings *ExtenderWebRtcProbeSettings,
	relay extenderWebRtcProbeRelay,
	clientId server.Id,
	publicKey []byte,
	ipVersion int,
	testedIp netip.Addr,
) (*connect.ConnectSettings, func()) {
	exchanger := &extenderWebRtcRelayExchanger{
		clientId: clientId,
		testedIp: testedIp,
		settings: settings,
		relay:    relay,
		now:      time.Now,
	}
	webRtcSettings := connect.DefaultWebRtcExtenderSettings()
	webRtcSettings.IceIpVersion = ipVersion
	carrier := connect.NewWebRtcExtenderCarrier(
		ctx,
		webRtcSettings,
		func(extenderPublicKey []byte) (connect.WebRtcExtenderOfferExchanger, bool) {
			if !bytes.Equal(extenderPublicKey, publicKey) {
				return nil, false
			}
			return exchanger, true
		},
	)
	connectSettings := *ExtenderProbeConnectSettings()
	connectSettings.WebRtcExtenderCarrier = carrier
	return &connectSettings, carrier.Close
}

// The webrtc test of one activation (C2a): one dial over the carrier, which
// challenges the extender and verifies its key before it is used, then the
// verified `GET /hello` through it, its checks on the activation's source
// address alone. Returns the caller address the api saw,
// whose family the activation checks. One dial rather than a carrier probe
// and then a forward probe, since each dial is a signaling round trip and an
// ICE join inside the one probe budget.
func probeExtenderWebRtcActivation(
	ctx context.Context,
	clientId server.Id,
	publicKey []byte,
	ipVersion int,
	sourceIp netip.Addr,
	apiUrl string,
	tlsConfig *tls.Config,
) (string, error) {
	connectSettings, closeCarrier := newExtenderWebRtcProbeConnectSettings(
		ctx,
		defaultExtenderWebRtcProbeSettings(),
		extenderWebRtcProbeRedisRelay{},
		clientId,
		publicKey,
		ipVersion,
		sourceIp,
	)
	defer closeCarrier()
	return connect.ProbeExtenderWebRtcForward(ctx, connectSettings, publicKey, apiUrl, tlsConfig)
}

// ProbeExtenderWebRtcUptime is the uptime probe of an address accepted on
// the webrtc path (C3): the challenge alone, over the carrier, under the
// extender's published key, its ICE on the address's family.
func ProbeExtenderWebRtcUptime(
	ctx context.Context,
	target *model.NetworkExtenderProbeTarget,
	destinationHost string,
) error {
	connectSettings, closeCarrier := newExtenderWebRtcProbeConnectSettings(
		ctx,
		defaultExtenderWebRtcProbeSettings(),
		extenderWebRtcProbeRedisRelay{},
		target.ClientId,
		target.PublicKey,
		target.IpVersion,
		target.Ip,
	)
	defer closeCarrier()
	_, err := connect.ProbeExtenderWebRtcCarrier(
		ctx,
		connectSettings,
		target.PublicKey,
		destinationHost,
		ExtenderProbeDestinationPort,
	)
	return err
}

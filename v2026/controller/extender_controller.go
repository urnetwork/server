package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog/v2026"

	"github.com/urnetwork/connect/v2026"
	"github.com/urnetwork/connect/v2026/protocol"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
	"github.com/urnetwork/server/v2026/session"
)

// Extender activation and the record signing every publisher shares
// (connect/EXTENDER.md C2, B2).
//
// Activation is the only place an extender enters the directory, and it does
// so by being proved from the outside: the operator dials back to the caller's
// own address on the tcp carrier, requires a signature over a fresh
// challenge, and forwards a real `GET /hello` through it to its own api. That
// is the required test (C2a), and nothing is stored unless it passes, so an
// extender behind NAT, on the wrong family, or without the private key it
// claims simply never appears -- no reachability heuristic and no trust in
// what the caller said about itself.
//
// Every other carrier the caller claims is optional (C2a): each is challenged
// the same way when it is offered, and one that does not answer is left out of
// what is stored and signed rather than refusing the activation. A client is
// only ever sent to a carrier that answered.
//
// The probe runs synchronously inside the request. That is what lets the
// answer carry the signed record: the caller gets its identity in the
// directory and a bootstrap sample of other extenders in one round trip,
// which is the only bootstrap an app has before it has any peer (D6).

const (
	// How long a signed record stays valid (B2; connect/GEOMAP.md §2.8, D19):
	// a day, the same day a ping and a derived location live. The publish
	// drip re-releases every active extender within
	// ExtenderPublishRotationTimeout, half of this, so a record reaches every
	// client with half its life left and an extender that stays up is never
	// seen expired by a connected client; an operator that stops publishing
	// drains out of every directory within a day.
	ExtenderRecordExpireTimeout = 24 * time.Hour

	// The whole probe budget of one activation (C2). Every carrier and the
	// forward share it, so a caller cannot hold a request open by being slow
	// on one carrier. The required test runs first (C2a), and then every
	// optional carrier -- quic and each dns port -- is probed at once, each
	// under all of what is left (connect#230). A udp carrier gives no answer
	// to fail fast on, so one that never answers waits out the budget, but it
	// waits beside the others rather than before them, and loses only itself.
	// The webrtc test that runs in place of a tcp path that failed has a
	// budget of this length of its own, ending no later than
	// ExtenderActivateSettings.TcpChallengeTimeout past this one
	// (connect/EXTENDER.md S8), so an activation probes for at most the two.
	ExtenderActivateProbeTimeout = 10 * time.Second

	// The service label of the family api hosts an activation is posted to
	// (C2): `[<env>-]api-v4` and `[<env>-]api-v6`.
	extenderActivateApiServiceLabel = "api"

	// Records of other extenders returned with an activation (C2).
	ExtenderBootstrapCount = 8

	// The port a probe asks the extender to forward to. The api answers on
	// 443 and an extender's whitelist is by host, so the port is fixed rather
	// than taken from the api url, whose port is only ever non-standard in a
	// test.
	ExtenderProbeDestinationPort = 443
)

// The activation budgets (C2). An activation costs the operator several dials
// back to the caller, so the budgets are what stop a client, or an account's
// many clients, from using the api as a probe engine.
//
// The budget C2 specifies is each client's own. It was keyed by the account,
// so all of an account's providers shared six activations an hour: each
// activates both families on its own schedule, and a second device was
// refused for the first one's retries, "for this action today" (support inbox
// 8051). The account keeps a budget of its own, ten clients' worth, so
// creating more clients does not multiply what one account can probe.
//
// Every budget is kept per address family, each family with the limits
// below. One budget for both families let a dual-stack client's failing
// family spend the other's: three restarts of a provider whose ipv4 posts
// failed spent the hour, and its ipv6 could not activate either (support inbox
// 8083). A family's budget is spent by the probes of that family's address.
//
// The budgets are per host (connect#230). The address a probe dials has a
// budget of its own, over every client and every account, which bounds what
// any number of accounts can have the operator send one address; the account
// budget never did. The account's limit grows with the hosts it has proven, so
// a fleet that has passed activation restarts at once rather than queueing an
// hour per 60 hosts, while a new account keeps the floor and a new fleet ramps
// an hour at a time. Hosts that fail never raise it.
type ExtenderActivateSettings struct {
	// activations per client per family per window. A client posts one
	// activation per family a pass, and a refused pass backs off ten minutes
	// doubling (connect/net_extender_activate.go), so a client that keeps
	// failing posts three a family in its first hour and fewer after
	ClientRateLimit int
	// activations per caller address per family per window, over every client
	// and every account: the address is what the probe dials. The address is
	// the /32 of an ipv4 caller and the /64 of an ipv6 one, kept only as a
	// keyed hash. One client's whole budget, so a host posting alone never
	// meets it; a shared address, a NAT or a relay, does, and an extender
	// behind NAT cannot pass activation anyway.
	AddressRateLimit          int
	AddressIpv4PrefixBitCount int
	AddressIpv6PrefixBitCount int
	// activations per family per window of a key its network already proved
	// at the caller's address, which spend this budget, kept for that key at
	// that address, rather than the address's (connect#230). A public
	// extender relays posts to the api from its own address, so other
	// accounts' posts through it spend the address's budget; its own
	// re-activation spends one no other post can. Proved means the key's
	// last pass on the family was from the same /32 or /64, at most
	// ProvenHostMaxAge ago.
	ProvenAddressRateLimit int
	// the floor of the account's activations per family per window, over all
	// of its clients: a new account's limit. A client refused by its own
	// budget does not spend this one, and a client refused by this one does
	// not spend its own.
	AccountRateLimit int
	// what each proven host of the account adds to its limit: a host's own
	// budgets, so the limit never binds below what the account's proven hosts
	// could each spend
	AccountRateLimitPerProvenHost int
	// the most the account's limit grows to, twenty a minute: one account's
	// share of the probes
	AccountRateLimitCeiling int
	RateLimitWindow         time.Duration

	// a proven host is an identity key of the account that passed activation
	// on the family -- the tcp challenge and the verified forward (C2a) -- at
	// least ProvenHostMinAge and at most ProvenHostMaxAge ago. The least age
	// keeps the limit from growing inside the window it is spending, and the
	// most lets a host that lapsed stop counting.
	ProvenHostMinAge time.Duration
	ProvenHostMaxAge time.Duration
	// proven hosts count once per address (the address budget's /32 or /64)
	// and at most ProvenHostsPerBlock per /24 of ipv4 or /48 of ipv6, so
	// renting addresses in one block buys little
	ProvenHostsPerBlock               int
	ProvenHostBlockIpv4PrefixBitCount int
	ProvenHostBlockIpv6PrefixBitCount int
	// how long the process reuses an account's proven host candidates, which
	// it counts at each read's own time. They are read only once the
	// account's spends reach the floor, and then for each of a fleet's posts.
	// Non-positive reads them every time.
	ProvenHostCountTimeout time.Duration
	// how far past the moment a budget has room again its refusal's wait is
	// drawn, uniformly (connect#230). Without it the hosts one budget refuses
	// together are told the same second, and come back together into the
	// probe guard. Non-positive asks for the moment itself.
	BudgetRetryAfterJitter time.Duration
	// how far before the spend each budget's attempt is recorded, drawn
	// uniformly for each on its own, so the attempts of one spend share no
	// create_time that joins the caller address's hashed row to its client's
	// and account's rows (connect#230). A second is far below every budget's
	// hour. Non-positive records the spend's time.
	BudgetAttemptTimeSpread time.Duration
	// the wait a refusal by the account's budget asks for while none of the
	// account's hosts has passed activation on the family within
	// ProvenHostMaxAge (unprovenAccountRetryAfter): the window for the
	// account's first window's worth of attempts of the family over the last
	// UnprovenAccountAttemptLookback, twice as long for each window's worth
	// more, and at most UnprovenAccountRetryAfterMax. An account whose hosts
	// never pass spends its floor on probes that fail, hour after hour, and
	// every other post of its fleet is refused; told to come back when the
	// floor had room again, the fleet came back every hour, and two such
	// accounts were refused about 96,000 posts a day (research
	// extender-health-2026-11-11 §1.2). An account any of whose hosts has
	// passed keeps the wait until its budget has room. Non-positive
	// UnprovenAccountRetryAfterMax answers every account that way.
	//
	// What an activator does with the wait (connect/net_extender_activate.go):
	// from connect 3a826e16 (2026-11-01) it holds the refused family that long,
	// at least its MinRetryAfter of a minute and at most its MaxBackoff of six
	// hours, and leaves its backoff as it was; a restart, a network change or
	// a revocation still posts at once. One from before, which is every
	// release up to v2026.10.30, has no retry_after_seconds field and drops
	// it: it holds its whole loop on its own backoff, ten minutes doubling to
	// six hours, and shows the refusal's words, so for it nothing changes but
	// the words, which start as they always did.
	UnprovenAccountRetryAfterMax   time.Duration
	UnprovenAccountAttemptLookback time.Duration

	// activations the api process probes at once (connect#230). Each holds a
	// handler for up to the 10 s probe budget, or 15 s when the webrtc test
	// runs in place of a tcp path that failed (TcpChallengeTimeout), so a
	// fleet restarting at once
	// could otherwise hold thousands. One over it is refused as busy before
	// it spends any budget, with a `retry_after_seconds`. Non-positive refuses
	// every activation as busy.
	MaxConcurrentProbes int
	// the wait a busy refusal asks for, drawn uniformly between the two so
	// the activations refused together do not all come back together
	BusyRetryAfterMin time.Duration
	BusyRetryAfterMax time.Duration

	// how long the tcp carrier's challenge has, in an activation that offers
	// the webrtc carrier beside tcp, before the tcp path is judged failed and
	// the webrtc test runs in its place (C2a, connect/EXTENDER.md S8). The
	// webrtc test runs only where the tcp path failed or was not offered, and
	// never beside a tcp test that may still pass (owner, 2026-11-12). A tcp
	// port that answers passes its challenge in a second or two; one a home
	// router black-holes never answers, and the webrtc test after it, which a
	// host behind nat is accepted on, then has a budget of
	// ExtenderActivateProbeTimeout of its own. So such an activation probes
	// for at most this plus ExtenderActivateProbeTimeout, the uptime probe's
	// address budget (taskworker ExtenderProbeAddressTimeout). An activation
	// that offers tcp without webrtc gives tcp the whole budget, as it always
	// did. Non-positive gives the tcp challenge no time, so every such
	// activation is tried on webrtc.
	TcpChallengeTimeout time.Duration
	// the most extenders the operator accepts on the webrtc carrier without
	// tcp (connect/EXTENDER.md S8): active extenders with an active address
	// that lists webrtc and not tcp, the activating key's own left out, so an
	// extender already counted activates again. Past it an activation whose
	// tcp path failed or was not offered is refused before the webrtc test
	// runs, and its refusal names the cap. A first bound while the carrier is
	// new (owner, 2026-11-12): every such address costs the uptime pass a
	// relayed offer and an ICE join at the same probe concurrency
	// (taskworker ExtenderProbeConcurrency), and a webrtc test that fails
	// holds a worker for the whole address budget. Read only when the webrtc
	// test is to run. Non-positive accepts none on webrtc without tcp.
	MaxWebRtcOnlyExtenders int

	// where every activation's outcome and probe step times are counted
	// (extender_activation_metrics.go). Nil counts nothing, which is what a
	// test that builds the settings itself gets.
	metrics *extenderActivationMetrics
}

// The actions the activation budgets of one family are recorded under
// (ExtenderActivateSettings): the calling client's, the caller address's, and
// the account's.
func extenderActivateRateLimitActions(ipVersion int) (clientAction string, addressAction string, accountAction string) {
	if ipVersion == 6 {
		return model.AccountActionExtenderActivateV6,
			model.AccountActionExtenderActivateAddressV6,
			model.AccountActionExtenderActivateAccountV6
	}
	return model.AccountActionExtenderActivateV4,
		model.AccountActionExtenderActivateAddressV4,
		model.AccountActionExtenderActivateAccountV4
}

// The account's activation limit of one family with `provenHostCount` proven
// hosts (connect#230): the floor, AccountRateLimitPerProvenHost more for each
// proven host, and no more than the ceiling. Never below the floor.
func (self *ExtenderActivateSettings) accountRateLimit(provenHostCount int) int {
	limit := self.AccountRateLimit + self.AccountRateLimitPerProvenHost*max(provenHostCount, 0)
	return max(self.AccountRateLimit, min(limit, self.AccountRateLimitCeiling))
}

// The proven hosts of one family among an account's candidates at `now`
// (connect#230): those whose first activation of the family since
// ProvenHostMaxAge ago is at least ProvenHostMinAge old, counted once per
// address, and at most ProvenHostsPerBlock in a block.
func (self *ExtenderActivateSettings) provenHostCount(
	candidates []*model.NetworkExtenderProvenHostCandidate,
	ipVersion int,
	now time.Time,
) int {
	addressBitCount := self.AddressIpv4PrefixBitCount
	blockBitCount := self.ProvenHostBlockIpv4PrefixBitCount
	if ipVersion == 6 {
		addressBitCount = self.AddressIpv6PrefixBitCount
		blockBitCount = self.ProvenHostBlockIpv6PrefixBitCount
	}
	minActivateTime := now.Add(-self.ProvenHostMaxAge)
	maxActivateTime := now.Add(-self.ProvenHostMinAge)
	blockAddresses := map[netip.Prefix]map[netip.Prefix]bool{}
	for _, candidate := range candidates {
		ip := candidate.Ip.Unmap()
		if server.IpVersionForAddr(ip) != ipVersion {
			continue
		}
		if candidate.FirstActivateTime.Before(minActivateTime) || maxActivateTime.Before(candidate.FirstActivateTime) {
			continue
		}
		block := netip.PrefixFrom(ip, blockBitCount).Masked()
		addresses, ok := blockAddresses[block]
		if !ok {
			addresses = map[netip.Prefix]bool{}
			blockAddresses[block] = addresses
		}
		addresses[netip.PrefixFrom(ip, addressBitCount).Masked()] = true
	}
	provenHostCount := 0
	for _, addresses := range blockAddresses {
		provenHostCount += min(len(addresses), self.ProvenHostsPerBlock)
	}
	return provenHostCount
}

// Whether any of an account's hosts has passed activation on one family, among
// its proven host candidates (connect#230). An activation is stored only once
// its required test passed (C2a), and the candidates are the network's hosts
// that activated the family since a read reaching ProvenHostMaxAge back, less
// at most the reuse of the read (readProvenHostCandidates), so any candidate
// of the family is a pass within ProvenHostMaxAge. Unlike a
// proven host it has no least age and no block cap: a host that passed a
// minute ago is the account's hosts showing they can pass, and a fleet that
// is ramping up keeps the answers of any account. A first pass inside the
// minute a process reuses its read (ProvenHostCountTimeout) is seen at its
// next read; reading on every refused post instead would read for each post
// of a fleet that never passes, which is what the reuse is for.
func (self *ExtenderActivateSettings) accountPassed(
	candidates []*model.NetworkExtenderProvenHostCandidate,
	ipVersion int,
) bool {
	for _, candidate := range candidates {
		if server.IpVersionForAddr(candidate.Ip.Unmap()) == ipVersion {
			return true
		}
	}
	return false
}

// The wait a refusal by the account's budget asks for while none of the
// account's hosts has passed activation on the family (connect#230), given
// `attemptCount`, the account's attempts of the family over the last
// UnprovenAccountAttemptLookback, each a probe that failed: the window for the
// first window's worth of them, twice as long for each window's worth more,
// and at most UnprovenAccountRetryAfterMax. With the defaults, an hour, then
// two, four, and six hours from four windows' worth on. Zero when the longer
// wait is off.
func (self *ExtenderActivateSettings) unprovenAccountRetryAfter(attemptCount int) time.Duration {
	if self.UnprovenAccountRetryAfterMax <= 0 || self.RateLimitWindow <= 0 {
		return 0
	}
	windowCount := max(1, attemptCount/max(1, self.AccountRateLimit))
	retryAfter := self.RateLimitWindow
	for range windowCount - 1 {
		if self.UnprovenAccountRetryAfterMax <= retryAfter {
			break
		}
		retryAfter *= 2
	}
	return min(retryAfter, self.UnprovenAccountRetryAfterMax)
}

// The most attempts unprovenAccountRetryAfter reads: the windows' worth at
// which its wait reaches UnprovenAccountRetryAfterMax, so the count of an
// account that has spent its floor all day reads no further than that. Zero
// when the longer wait is off.
func (self *ExtenderActivateSettings) unprovenAccountAttemptCountLimit() int {
	if self.UnprovenAccountRetryAfterMax <= 0 || self.RateLimitWindow <= 0 {
		return 0
	}
	windowCount := 1
	for retryAfter := self.RateLimitWindow; retryAfter < self.UnprovenAccountRetryAfterMax; retryAfter *= 2 {
		windowCount += 1
	}
	return windowCount * max(1, self.AccountRateLimit)
}

// The words of a lookback in a refusal: "7 days" for whole days, else the
// words of a wait ("23 hours").
func extenderActivateLookbackText(lookback time.Duration) string {
	if day := 24 * time.Hour; day <= lookback && lookback%day == 0 {
		if lookback == day {
			return "day"
		}
		return fmt.Sprintf("%d days", lookback/day)
	}
	return model.AccountActionRetryText(lookback)
}

// The id the address budget of a caller address is recorded under
// (connect#230): the peppered hash of its /32 or /64, as the activation history
// keeps an address, cut to an id. No address is stored, and an ipv4-mapped
// address and a zoned one are the address they name.
func (self *ExtenderActivateSettings) addressBudgetId(clientIp netip.Addr) server.Id {
	addressHash := server.ClientIpHashForAddrPrefix(
		clientIp.WithZone(""),
		self.AddressIpv4PrefixBitCount,
		self.AddressIpv6PrefixBitCount,
	)
	return server.Id([16]byte(addressHash[:16]))
}

// The action of the budget of a key its network proved at the caller's
// address (ProvenAddressRateLimit).
func extenderActivateProvenAddressRateLimitAction(ipVersion int) string {
	if ipVersion == 6 {
		return model.AccountActionExtenderActivateProvenAddressV6
	}
	return model.AccountActionExtenderActivateProvenAddressV4
}

// The id the budget of one key at one caller address is recorded under
// (ProvenAddressRateLimit): the hash of the address's peppered hash and the
// key, cut to an id, so it is the key's at that /32 or /64 alone and holds no
// address.
func (self *ExtenderActivateSettings) provenAddressBudgetId(clientIp netip.Addr, publicKey []byte) server.Id {
	addressHash := server.ClientIpHashForAddrPrefix(
		clientIp.WithZone(""),
		self.AddressIpv4PrefixBitCount,
		self.AddressIpv6PrefixBitCount,
	)
	keyHash := sha256.New()
	keyHash.Write(addressHash[:])
	keyHash.Write(publicKey)
	return server.Id([16]byte(keyHash.Sum(nil)[:16]))
}

// Whether a post of a key from `clientIp` by `networkId` is the network's own
// extender re-activating where it proved itself (ProvenAddressRateLimit): the
// key's network is the caller's, its last pass on the family was from the
// same /32 or /64, and that pass was at most ProvenHostMaxAge ago.
func (self *ExtenderActivateSettings) keyProvenAtAddress(
	keyAddress *model.NetworkExtenderKeyAddress,
	networkId server.Id,
	clientIp netip.Addr,
	now time.Time,
) bool {
	if keyAddress == nil || keyAddress.NetworkId != networkId {
		return false
	}
	if keyAddress.ActivateTime.Before(now.Add(-self.ProvenHostMaxAge)) {
		return false
	}
	keyIp := keyAddress.Ip.Unmap().WithZone("")
	clientIp = clientIp.Unmap().WithZone("")
	if server.IpVersionForAddr(keyIp) != server.IpVersionForAddr(clientIp) {
		return false
	}
	prefixBitCount := self.AddressIpv4PrefixBitCount
	if clientIp.Is6() {
		prefixBitCount = self.AddressIpv6PrefixBitCount
	}
	return netip.PrefixFrom(keyIp, prefixBitCount).Masked() == netip.PrefixFrom(clientIp, prefixBitCount).Masked()
}

// The family an activation claims by the host it was posted to (C2): 4 for a
// `[<env>-]api-v4` name and 6 for a `[<env>-]api-v6` name, the family api
// hosts an extender activates each of its families on. 0 for any other host
// -- the plain api name, an ip literal, a single label, none at all -- which
// claims no family, so the family is the caller address's, as it is for an
// operator without family hosts.
func extenderActivateHostIpVersion(host string) int {
	hostName := strings.TrimSpace(host)
	if splitHostName, _, err := net.SplitHostPort(hostName); err == nil {
		hostName = splitHostName
	}
	hostName = strings.TrimSuffix(strings.ToLower(hostName), ".")
	if _, err := netip.ParseAddr(hostName); err == nil {
		return 0
	}
	label, _, ok := strings.Cut(hostName, ".")
	if !ok {
		return 0
	}
	labelParts := strings.Split(label, "-")
	if len(labelParts) < 2 || labelParts[len(labelParts)-2] != extenderActivateApiServiceLabel {
		return 0
	}
	switch labelParts[len(labelParts)-1] {
	case "v4":
		return 4
	case "v6":
		return 6
	default:
		return 0
	}
}

func DefaultExtenderActivateSettings() *ExtenderActivateSettings {
	return &ExtenderActivateSettings{
		ClientRateLimit:                   6,
		AddressRateLimit:                  6,
		AddressIpv4PrefixBitCount:         32,
		AddressIpv6PrefixBitCount:         64,
		ProvenAddressRateLimit:            6,
		AccountRateLimit:                  60,
		AccountRateLimitPerProvenHost:     6,
		AccountRateLimitCeiling:           1200,
		RateLimitWindow:                   time.Hour,
		ProvenHostMinAge:                  1 * time.Hour,
		ProvenHostMaxAge:                  7 * 24 * time.Hour,
		ProvenHostsPerBlock:               32,
		ProvenHostBlockIpv4PrefixBitCount: 24,
		ProvenHostBlockIpv6PrefixBitCount: 48,
		ProvenHostCountTimeout:            1 * time.Minute,
		BudgetRetryAfterJitter:            1 * time.Minute,
		BudgetAttemptTimeSpread:           1 * time.Second,
		// an hour, two, four, then six: the activator holds a family at most
		// its MaxBackoff of six hours whatever it is told
		// (connect/net_extender_activate.go), so a longer wait would be cut
		// there. The lookback is half the attempts' retention
		// (model.AccountActionAttemptRetention), so every attempt it counts is
		// still kept
		UnprovenAccountRetryAfterMax:   6 * time.Hour,
		UnprovenAccountAttemptLookback: 24 * time.Hour,
		MaxConcurrentProbes:            64,
		// the activator holds a retry at least a minute whatever it is told
		// (connect/net_extender_activate.go), and three minutes spreads a
		// fleet's refused posts over three rounds of the guard
		BusyRetryAfterMin: 1 * time.Minute,
		BusyRetryAfterMax: 3 * time.Minute,
		// the uptime probe's address budget less the activation's
		TcpChallengeTimeout: 5 * time.Second,
		// a hundred, up to two hundred addresses: a pass's webrtc probes that
		// all wait out their 15 s budget hold the 16 workers about three
		// minutes, inside the five-minute cadence beside the tcp addresses
		MaxWebRtcOnlyExtenders: 100,
		metrics:                defaultExtenderActivationMetrics,
	}
}

// Probe dial settings, built once: they carry the platform tls roots, which
// are expensive to load and identical for every probe.
var extenderProbeConnectSettings = sync.OnceValue(func() *connect.ConnectSettings {
	return connect.DefaultConnectSettings()
})

// ExtenderProbeConnectSettings is the dial configuration of every extender
// probe, shared by activation and the uptime task.
func ExtenderProbeConnectSettings() *connect.ConnectSettings {
	return extenderProbeConnectSettings()
}

// The inner tls configuration of the forward probe. nil keeps the platform's
// verified roots, which is production; a test points it at its fixture roots
// so the in-process api is verified normally rather than not at all.
//
// Only the INNER tls is settable. The outer connection to the extender is
// always unverified except against the extender's own identity key, and there
// is deliberately no seam that could relax the inner verification in
// production.
var extenderForwardProbeTlsConfig = func() *tls.Config {
	return nil
}

// Testing_SetExtenderForwardProbeRootCAs verifies the forward probe's inner
// tls against rootCAs rather than the platform roots, for a test whose api is
// in process under its own certificate, as this package's own tests do. It
// names roots to verify against and never turns verification off. Call it
// from the test goroutine only, and call the restore when the test ends.
func Testing_SetExtenderForwardProbeRootCAs(rootCAs *x509.CertPool) (restore func()) {
	previousTlsConfig := extenderForwardProbeTlsConfig
	extenderForwardProbeTlsConfig = func() *tls.Config {
		return &tls.Config{RootCAs: rootCAs}
	}
	return func() {
		extenderForwardProbeTlsConfig = previousTlsConfig
	}
}

type ExtenderActivateArgs struct {
	PublicKeyHex string `json:"public_key_hex"`
	// the hex X25519 static public key of the extender's tcp camouflage
	// (connect/EXTENDER.md P1, P6; EXTENDER-UPDATE2 §3, phase 14b), which a
	// client fronts its camouflaged hello with. A passing activation stores
	// it and every record of the extender then carries it, while the network
	// that posted it holds the identity key. Empty, an extender with
	// camouflage off, clears a stored key. A key that is not a usable X25519
	// key is refused before anything is probed. Nothing here proves the
	// extender holds the private half: in Phase A the probe passes over the
	// legacy attempt whatever the key.
	RealityPublicKeyHex string `json:"reality_public_key_hex,omitempty"`
	TcpPort             int    `json:"tcp_port"`
	UdpPort             int    `json:"udp_port"`
	DnsPort             int    `json:"dns_port"`
	// every dns port the caller is listening on, each probed on its own (L2).
	// Empty offers DnsPort alone, which is what an extender that predates the
	// list sends.
	DnsPorts []int    `json:"dns_ports"`
	DnsTld   string   `json:"dns_tld"`
	Carriers []string `json:"carriers"`
	// standard base64 of the DER certificate request for the extender's
	// lag.dev measurement name (lag.dev/PLAN.md C4, A10). Most activations
	// carry none, which leaves a stored request as it is. One that is carried
	// is stored as the extender's request on a successful activation, and its
	// outcome is read from GET /network/extender-measurement-cert (A8), never
	// from this answer.
	MeasurementCsr string `json:"measurement_csr,omitempty"`
	// set on every activation while the extender's owner has opted out of
	// lag.dev measurement (lag.dev/PLAN.md C4, C9, decision 18): the operator
	// withdraws its measurement name, and a request carried beside it is not
	// stored. Unset, the default, changes nothing on its own.
	MeasurementDisabled bool `json:"measurement_disabled,omitempty"`
	// the run of the extender process that posted this activation
	// (connect/EXTENDER.md G3): 32 hex characters, the same for every
	// activation of one process and new at each start. The three run fields
	// are json strings. A well-typed one never refuses an activation: each is
	// stored on the activation's history row when it is well formed, and as
	// null otherwise (extenderActivationRun). A value of another json type
	// fails the request's decoding, as any field's does. An extender that
	// predates them sends none.
	RunId string `json:"run_id,omitempty"`
	// when that process started, rfc 3339 in utc. A string here rather than a
	// time.Time, whose decoding would answer a malformed time with a 400 and
	// so refuse the activation
	RunStartTime string `json:"run_start_time,omitempty"`
	// how the run before it ended, as the host that restarted the process
	// reported it: one of extenderActivationPreviousExits, or exit-<code>
	PreviousExit string `json:"previous_exit,omitempty"`
}

// The random bytes of a run id, 32 hex characters on the wire
// (connect/EXTENDER.md G3).
const extenderActivationRunIdByteCount = 16

// The earliest run start an activation may name: the day the run fields were
// added (connect/EXTENDER.md G3), before which no process carried one, and long
// after the 1970 a host without a battery-backed clock starts at before its
// clock is set.
var extenderActivationRunStartFloor = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

// How far past the operator's clock a run start may be: the skew a host's clock
// may have. An hour keeps a clock that drifted, and drops one that is plainly
// wrong.
const extenderActivationRunStartSkew = time.Hour

// The words of a previous exit besides exit-<code>, for a code from 0 to 255
// (connect/EXTENDER.md G3): the host knew the run ran out of memory, the host
// stopped it on purpose, a signal ended it -- each of these, or another.
var extenderActivationPreviousExits = map[string]bool{
	"oom":     true,
	"stop":    true,
	"signal":  true,
	"sighup":  true,
	"sigint":  true,
	"sigquit": true,
	"sigill":  true,
	"sigtrap": true,
	"sigabrt": true,
	"sigbus":  true,
	"sigfpe":  true,
	"sigkill": true,
	"sigsegv": true,
	"sigpipe": true,
	"sigalrm": true,
	"sigterm": true,
}

// The run an activation reports, as its history row stores it
// (connect/EXTENDER.md G3): the id as 32 lower case hex characters, the start
// in utc when it is no earlier than the floor and no later than the skew past
// `now`, and the previous exit when it is one of the operator's words. Each
// field is judged on its own, and a malformed one is stored as null rather than
// refusing the activation: the run is a diagnostic, and an extender whose clock
// is wrong, or whose host names an exit the operator does not know, is still
// published. It is read only once the probes have passed, so a refusal of any
// stage, busy and the budgets among them, stores nothing of the run.
func extenderActivationRun(
	args *ExtenderActivateArgs,
	now time.Time,
) (runId *string, runStartTime *time.Time, previousExit *string) {
	if runIdBytes, err := hex.DecodeString(args.RunId); err == nil && len(runIdBytes) == extenderActivationRunIdByteCount {
		normalizedRunId := hex.EncodeToString(runIdBytes)
		runId = &normalizedRunId
	}
	if startTime, err := time.Parse(time.RFC3339, args.RunStartTime); err == nil {
		startTime = startTime.UTC()
		if !startTime.Before(extenderActivationRunStartFloor) && !now.Add(extenderActivationRunStartSkew).Before(startTime) {
			runStartTime = &startTime
		}
	}
	if normalizedPreviousExit := strings.ToLower(strings.TrimSpace(args.PreviousExit)); extenderActivationPreviousExitKnown(normalizedPreviousExit) {
		previousExit = &normalizedPreviousExit
	}
	return
}

// Whether a previous exit is one of the operator's words, or exit-<code> for a
// code from 0 to 255 written without a sign or a leading zero.
func extenderActivationPreviousExitKnown(previousExit string) bool {
	if extenderActivationPreviousExits[previousExit] {
		return true
	}
	code, ok := strings.CutPrefix(previousExit, "exit-")
	if !ok || code == "" || 3 < len(code) || (1 < len(code) && code[0] == '0') {
		return false
	}
	value := 0
	for _, digit := range code {
		if digit < '0' || '9' < digit {
			return false
		}
		value = 10*value + int(digit-'0')
	}
	return value <= 255
}

type ExtenderActivateResult struct {
	Activated bool `json:"activated"`
	// the operator's id of the extender, one per identity key and the same at
	// every activation. The extender's lag.dev measurement name is this id
	// under the measurement zone (lag.dev/PLAN.md N1, N4, A10).
	ExtenderId *server.Id `json:"extender_id,omitempty"`
	Ip         string     `json:"ip,omitempty"`
	IpVersion  int        `json:"ip_version,omitempty"`
	// the carriers that passed their probe, which is what the stored address
	// and the signed record list
	Carriers []string `json:"carriers,omitempty"`
	// the dns ports that passed their probe, ascending, which is what the
	// stored address and the signed record list (L2). Empty when the dns
	// carrier was not offered or no port answered.
	DnsPorts []int `json:"dns_ports,omitempty"`
	// the optional carriers that were offered and did not answer, each dns
	// port that did not, in the order offered, with the port, the probe's
	// error and its timing (C2a); and tcp, when its path failed and the
	// activation passed on the webrtc test that ran in its place
	// (connect/EXTENDER.md S8). They are left out of the stored address and
	// the signed record, so the extender is published on the carriers that
	// answered and no others. A carrier that was never probed is in neither
	// list: webrtc, offered beside a tcp path that passed, is not tested. Empty
	// on a refusal, which probes no optional carrier.
	RefusedCarriers []*ExtenderCarrierRefusal `json:"refused_carriers,omitempty"`
	// every probe step the activation ran -- the tcp challenge and the forward
	// through it, in the order run, then the webrtc test when it ran in place
	// of a failed tcp path or alone, then each dns port and quic, which run
	// together and are listed in that order -- with how long it ran, how much
	// of its budget was left when it started, and how it ended (C2). A
	// refusal's last step is the one that refused it.
	// Empty when the activation was refused before anything was probed.
	ProbeSteps []*ExtenderProbeStep `json:"probe_steps,omitempty"`
	// why the activation was refused, empty on success. A refusal is a normal
	// answer, not a request error: the caller is told which part of the
	// required test failed -- the tcp carrier, the forward through it, or the
	// family the forward arrived on (C2a) -- so it can fix its own binding.
	// A probe refusal also says how long the step ran and, when its budget ran
	// out first, how much it had, so a late answer reads differently from no
	// answer (connect#230).
	Error      string     `json:"error,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	// the operator patterns this extender may forward to (A5)
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// base64 of a serialized protocol.ExtenderRecord
	Record string `json:"record,omitempty"`
	// base64 of serialized protocol.ExtenderRecord messages of other active
	// extenders, the directory an app starts from (D6)
	Bootstrap []string `json:"bootstrap,omitempty"`
	// on a refusal that judged nothing about the caller -- an operator too
	// busy to probe, or a spent budget -- the seconds until the operator has
	// room for the caller's next post of this family, rounded up
	// (connect#230). The activator waits that long instead of backing off. 0,
	// and left out, on every other answer.
	RetryAfterSeconds int `json:"retry_after_seconds,omitempty"`
	// on a refusal the extender acts on, which one (connect/EXTENDER.md C2c):
	// `key_other_network` for a key another network holds, on which the
	// extender makes a key of its network's own. Left out on every other
	// answer; an extender that predates it reads `error` alone.
	Refusal string `json:"refusal,omitempty"`
	// true on every answer: this operator keeps each address's ports
	// (connect/EXTENDER.md S10), storing the ports a post offers with that
	// family's address alone and signing them per address, so an extender may
	// offer each family its own ports. An extender that reads it does so only
	// while the operator's last answer said it; one that predates it skips it.
	AddressPorts bool `json:"address_ports,omitempty"`
}

// The wire form of a wait (`retry_after_seconds`): whole seconds, rounded up
// so a caller that waits it out is never early, and at least one.
func extenderActivateRetryAfterSeconds(retryAfter time.Duration) int {
	return max(1, int((retryAfter+time.Second-1)/time.Second))
}

// One optional carrier, or one port of the dns carrier, that did not answer
// its probe and was left out of an activation (C2a).
type ExtenderCarrierRefusal struct {
	Carrier string `json:"carrier"`
	// the port the carrier was probed on
	Port int `json:"port"`
	// why the probe failed. A probe whose budget ran out first says how much
	// time it had, which is what was left of the probe budget after the
	// required test: "no answer within the 8.5 s left of the 10 s probe
	// budget".
	Error string `json:"error"`
	// how long the probe ran
	ElapsedMs int64 `json:"elapsed_ms"`
	// how much of the activation's probe budget was left when the probe
	// started. The optional carriers start together, so each has the same.
	BudgetLeftMs int64 `json:"budget_left_ms"`
}

// One probe step of an activation (C2): the challenge on one carrier and
// port, or the forward through the tcp carrier, timed on the operator's
// clock. The steps share one probe budget, so a step that ran out of it is
// told apart from one that failed on its own, and from one that was starved
// by the steps before it (connect#230).
type ExtenderProbeStep struct {
	// tcp, forward, dns or quic
	Name string `json:"name"`
	// the family the step dialed, 4 or 6
	IpVersion int `json:"ip_version"`
	// the extender port the step dialed. The forward runs over the tcp carrier
	// and names its port.
	Port int `json:"port"`
	// how long the step ran
	ElapsedMs int64 `json:"elapsed_ms"`
	// how much of the activation's probe budget was left when the step
	// started. The optional carriers start together, so each has the same.
	BudgetLeftMs int64 `json:"budget_left_ms"`
	// ok; error, the step failed before its budget ran out; or timeout, its
	// budget ran out before it answered
	Outcome string `json:"outcome"`
}

// The carrier of a connect mode, and which of an extender's ports it is
// served on.
func extenderCarrierConnectMode(carrier string) (connect.ExtenderConnectMode, bool) {
	switch carrier {
	case connect.ExtenderCarrierTcp:
		return connect.ExtenderConnectModeTcpTls, true
	case connect.ExtenderCarrierQuic:
		return connect.ExtenderConnectModeQuic, true
	case connect.ExtenderCarrierDns:
		return connect.ExtenderConnectModeDns, true
	case connect.ExtenderCarrierWebRtc:
		// no ip:port probe: reached through the exchange signaling
		// (extender_webrtc_controller.go)
		return connect.ExtenderConnectModeWebRtc, true
	default:
		return "", false
	}
}

// SignExtenderRecord builds, signs and serializes the directory record of one
// extender over the given addresses (B2).
//
// The record and the gossip message are both returned because they go to
// different places: the record answers the activating caller, and the message
// is what the publish queue carries. Signing once for both is what keeps them
// the same record.
//
// The dns ports are the union of what the addresses answered on (L2),
// ascending, because the record's own ports are one extender's, and a client
// dials the lowest first. DnsPort stays the extender's configured port, which
// is what a reader that predates the list dials. The tcp and udp ports are the
// extender's latest activation's. An address whose ports are not the record's
// -- one family a router forwards on other ports (connect/EXTENDER.md S10) --
// names its own beside them, which a reader that predates the fields skips.
//
// The country, and the continent with it, is the extender's derived location's
// while it has one, else the one it activated from (connect/GEOMAP.md §6,
// NetworkExtender.RecordCountryCode): the same precedence a provider's
// connection location follows.
func SignExtenderRecord(
	config *ExtenderConfig,
	rootPrivateKey ed25519.PrivateKey,
	extender *model.NetworkExtender,
	addresses []*model.NetworkExtenderAddress,
	issueTime time.Time,
) (*protocol.ExtenderRecord, []byte, error) {
	recordAddresses := []*protocol.ExtenderAddress{}
	dnsPorts := []uint32{}
	for _, address := range addresses {
		recordAddresses = append(recordAddresses, &protocol.ExtenderAddress{
			Ip:        address.Ip.String(),
			IpVersion: uint32(address.IpVersion),
			Carriers:  slices.Clone(address.Carriers),
		})
		for _, dnsPort := range address.DnsPorts {
			if !slices.Contains(dnsPorts, uint32(dnsPort)) {
				dnsPorts = append(dnsPorts, uint32(dnsPort))
			}
		}
	}
	slices.Sort(dnsPorts)
	// the rendezvous of the webrtc carrier (connect/EXTENDER.md S3, S8): the
	// client id the extender's device is reachable under on the exchange,
	// which a dialer signals its offer to, the client that activated last.
	// Only a record whose address lists the carrier carries it.
	var webRtcClientId []byte
	for _, address := range addresses {
		if slices.Contains(address.Carriers, connect.ExtenderCarrierWebRtc) {
			webRtcClientId = extender.ClientId.Bytes()
			break
		}
	}
	countryCode := extender.RecordCountryCode()
	directoryTier := extender.DirectoryTier
	if extender.CanaryChannel != "" {
		// Older tier-aware readers must also suppress open redistribution.
		directoryTier = connect.ExtenderDirectoryTierGated
	}
	body := &protocol.ExtenderRecordBody{
		PublicKey:      extender.PublicKey,
		WebRtcClientId: webRtcClientId,
		Addresses:      recordAddresses,
		TcpPort:        uint32(extender.TcpPort),
		UdpPort:        uint32(extender.UdpPort),
		DnsPort:        uint32(extender.DnsPort),
		DnsPorts:       dnsPorts,
		DnsTld:         extender.DnsTld,
		CountryCode:    countryCode,
		// the same mapping and the same country the geo dns sets use, so
		// "close" means one thing on both paths (connect/DESIGNNOTES4.md §2)
		ContinentCode: model.ContinentCodeForCountry(countryCode),
		IssueTimeMs:   uint64(issueTime.UnixMilli()),
		ExpireTimeMs:  uint64(issueTime.Add(ExtenderRecordExpireTimeout).UnixMilli()),
		NetworkHost:   config.NetworkHost,
		// the tier is signed in (connect/EXTENDER.md R1), so a client that
		// holds a gated record never serves it on an open channel
		DirectoryTier: uint32(directoryTier),
		CanaryChannel: extender.CanaryChannel,
		// the extender's id while it serves the lag.dev measurement api, from
		// which a reader computes its measurement name (lag.dev/PLAN.md C10)
		MeasurementId: extenderMeasurementId(config, extender, addresses, issueTime),
		// the camouflage key the extender's latest passing activation posted,
		// while the posting network holds the key (connect/EXTENDER.md P6,
		// phase 14b); nil when it posted none
		RealityPublicKey: extenderRecordRealityPublicKey(extender),
	}
	// an address's own ports where they are not the record's: a router's
	// mapping forwards one family's carriers on ports it picks while the other
	// family answers on the bound ones (connect/EXTENDER.md S10). An address on
	// the record's ports names none, so the record of an extender whose
	// addresses share their ports is the record it always was, and a reader
	// that predates the fields dials every address on the record's
	for i, address := range addresses {
		recordAddress := recordAddresses[i]
		if tcpPort := address.DialTcpPort(extender); tcpPort != extender.TcpPort {
			recordAddress.TcpPort = uint32(tcpPort)
		}
		if udpPort := address.DialUdpPort(extender); udpPort != extender.UdpPort {
			recordAddress.UdpPort = uint32(udpPort)
		}
		addressDnsPorts := []uint32{}
		for _, dnsPort := range address.DnsPorts {
			addressDnsPorts = append(addressDnsPorts, uint32(dnsPort))
		}
		slices.Sort(addressDnsPorts)
		if 0 < len(addressDnsPorts) && !slices.Equal(addressDnsPorts, body.DnsPorts) {
			recordAddress.DnsPorts = addressDnsPorts
		}
	}
	// what a client must declare before a release hands the record out
	// (connect/EXTENDER-UPDATE2.md §3.2), decided from the body as signed, so
	// a release's compatibility check reads exactly what the record says
	body.RequiredCapabilities = config.RecordRequiredCapabilities(extender, body.RealityPublicKey)
	record, err := connect.SignExtenderRecord(rootPrivateKey, body)
	if err != nil {
		return nil, nil, err
	}
	message, err := proto.Marshal(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Record{Record: record},
	})
	if err != nil {
		return nil, nil, err
	}
	return record, message, nil
}

// SignExtenderRevocation builds, signs and serializes the revocation of one
// extender (B2, C3).
//
// The issue time is what orders a revocation against records: a directory
// keeps the newest of each and treats the key as active only while its record
// is at least as new as its revocation (B5), so a re-activation after this
// must issue a strictly newer record.
func SignExtenderRevocation(
	config *ExtenderConfig,
	rootPrivateKey ed25519.PrivateKey,
	extender *model.NetworkExtender,
	issueTime time.Time,
) (*protocol.ExtenderRevocation, []byte, error) {
	revocation, err := connect.SignExtenderRevocation(rootPrivateKey, &protocol.ExtenderRevocationBody{
		PublicKey:   extender.PublicKey,
		IssueTimeMs: uint64(issueTime.UnixMilli()),
		NetworkHost: config.NetworkHost,
	})
	if err != nil {
		return nil, nil, err
	}
	message, err := proto.Marshal(&protocol.ExtenderGossipMessage{
		Message: &protocol.ExtenderGossipMessage_Revocation{Revocation: revocation},
	})
	if err != nil {
		return nil, nil, err
	}
	return revocation, message, nil
}

// The transport form of a record in an api answer.
func encodeExtenderRecord(record *protocol.ExtenderRecord) (string, error) {
	recordBytes, err := proto.Marshal(record)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(recordBytes), nil
}

// ExtenderActivate proves the caller is a reachable extender and, when it is,
// puts it in the directory and hands back its signed record (C2).
//
// A refusal is a 200 with `activated: false` and the reason in `error`. The
// caller is a provider running the activation loop on a timer (G3), not a
// person: it needs to tell "this carrier did not answer" from "the request was
// malformed", and it needs to keep the answer it can act on rather than an
// http status.
// The refusal of an extender call whose network configuration is unusable:
// the configuration error's message. Its cause stays in the log.
func extenderConfigRefusal(err error) string {
	glog.Errorf("[extender]the extender network configuration is unusable: %s\n", err)
	var configErr *extenderConfigError
	if errors.As(err, &configErr) {
		return configErr.message
	}
	return "the extender network is not configured"
}

// The refusal of an activation whose key is registered to another network.
// Nothing the caller does can pass it with that key: the extender needs a key of
// its network's own, which a current app or provider makes on this refusal
// (connect/EXTENDER.md C2c). The first sentence is what an extender that
// predates `refusal` recognizes it by, so it stays as it is.
const extenderKeyOtherNetworkRefusal = "This extender key is registered to another network, so it cannot be activated for this one. An updated app makes a new extender key for this network on its own; otherwise remove the extender key file and restart to make a new one."

// The `refusal` of an activation whose key is registered to another network.
const extenderActivateRefusalKeyOtherNetwork = "key_other_network"

// Whether the key is registered to a network other than networkId. A key no
// network has activated is not.
func extenderKeyHeldByAnotherNetwork(ctx context.Context, publicKey []byte, networkId server.Id) bool {
	keyNetworkId, registered := model.GetNetworkExtenderKeyNetworkId(ctx, publicKey)
	return registered && keyNetworkId != networkId
}

func ExtenderActivate(
	args *ExtenderActivateArgs,
	clientSession *session.ClientSession,
) (*ExtenderActivateResult, error) {
	return defaultExtenderActivateService().activate(args, clientSession)
}

// The activations of one api process (C2): the settings each is judged by,
// the probes that dial the caller back, and the guard on how many of those run
// at once (connect#230). The process has one, built on first use; a test
// builds its own, with its own settings and, where the caller address is not
// a socket it can bind, fake probes. Safe for concurrent use.
type extenderActivateService struct {
	settings *ExtenderActivateSettings
	// one token per activation past the guard (MaxConcurrentProbes), from the
	// guard to the end of its probe
	probeSlots chan struct{}
	// replaces the probes of an activation when set: a test's fake, which
	// answers for the caller without a socket. Nil dials the caller for real,
	// which is production.
	testProbes func(clientIp netip.Addr, ipVersion int) *extenderActivationProbes
	// the clock proven hosts are counted and their reads reused by:
	// server.NowUtc, and a test's own where it steps through a cache's life
	now func() time.Time

	stateLock sync.Mutex
	// the proven host candidates this process read, each reused until its
	// expire time (ProvenHostCountTimeout) and counted at each read's own time
	provenHostCandidates map[extenderProvenHostCountKey]*extenderProvenHostCandidates
}

// An account's proven hosts of one family: the network's, since an extender
// row belongs to a network.
type extenderProvenHostCountKey struct {
	networkId server.Id
	ipVersion int
}

type extenderProvenHostCandidates struct {
	candidates []*model.NetworkExtenderProvenHostCandidate
	expireTime time.Time
}

func newExtenderActivateService(settings *ExtenderActivateSettings) *extenderActivateService {
	return &extenderActivateService{
		settings:             settings,
		probeSlots:           make(chan struct{}, max(0, settings.MaxConcurrentProbes)),
		now:                  server.NowUtc,
		provenHostCandidates: map[extenderProvenHostCountKey]*extenderProvenHostCandidates{},
	}
}

// The proven hosts of one family of a network (connect#230), read in the
// caller's transaction, which is the budget spend's (provenHostCount), and
// whether any of its hosts has passed activation on the family
// (accountPassed), from the same candidates.
func (self *extenderActivateService) provenHostCountInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	ipVersion int,
) (provenHostCount int, passed bool) {
	candidates, now := self.readProvenHostCandidates(
		networkId,
		ipVersion,
		func(minActivateTime time.Time) []*model.NetworkExtenderProvenHostCandidate {
			return model.GetNetworkExtenderProvenHostCandidatesInTx(ctx, tx, networkId, ipVersion, minActivateTime)
		},
	)
	return self.settings.provenHostCount(candidates, ipVersion, now), self.settings.accountPassed(candidates, ipVersion)
}

// The proven hosts of one family of a network at this read's own time, from
// the candidates `readCandidates` reads since a time, or those this process
// read within ProvenHostCountTimeout. The spend asks only once the account has
// reached its floor, so most activations never read them, and a fleet's posts
// past the floor read them once a minute per process rather than once each.
//
// What is reused is the candidates, never the count (connect#230): a host
// becomes proven an hour after its first pass whether or not anything is
// read, and a count reused through that moment held the account's limit at
// its floor while other processes admitted past it, so a post here met a
// limit below the count and was told to wait about an hour. A read reaches
// back the reuse's length less than the lookback, so a candidate's earliest
// pass is within the lookback of every count the read serves; a host whose
// only pass in the lookback is in that edge waits for the next read.
func (self *extenderActivateService) provenHostCount(
	networkId server.Id,
	ipVersion int,
	readCandidates func(minActivateTime time.Time) []*model.NetworkExtenderProvenHostCandidate,
) int {
	candidates, now := self.readProvenHostCandidates(networkId, ipVersion, readCandidates)
	return self.settings.provenHostCount(candidates, ipVersion, now)
}

// The proven host candidates of one family of a network that provenHostCount
// counts, and the time it counts them at: those `readCandidates` reads since a
// time, or those this process read within ProvenHostCountTimeout.
func (self *extenderActivateService) readProvenHostCandidates(
	networkId server.Id,
	ipVersion int,
	readCandidates func(minActivateTime time.Time) []*model.NetworkExtenderProvenHostCandidate,
) ([]*model.NetworkExtenderProvenHostCandidate, time.Time) {
	key := extenderProvenHostCountKey{
		networkId: networkId,
		ipVersion: ipVersion,
	}
	now := self.now()
	reuse := 0 < self.settings.ProvenHostCountTimeout
	var candidates []*model.NetworkExtenderProvenHostCandidate
	reused := false
	if reuse {
		candidates, reused = func() ([]*model.NetworkExtenderProvenHostCandidate, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if read, ok := self.provenHostCandidates[key]; ok && now.Before(read.expireTime) {
				return read.candidates, true
			}
			return nil, false
		}()
	}
	if !reused {
		candidates = readCandidates(now.Add(max(self.settings.ProvenHostCountTimeout, 0) - self.settings.ProvenHostMaxAge))
		if reuse {
			func() {
				self.stateLock.Lock()
				defer self.stateLock.Unlock()
				// the reads that ran out go, so the map holds only the
				// networks whose spends reached their floor within the timeout
				for readKey, read := range self.provenHostCandidates {
					if !now.Before(read.expireTime) {
						delete(self.provenHostCandidates, readKey)
					}
				}
				self.provenHostCandidates[key] = &extenderProvenHostCandidates{
					candidates: candidates,
					expireTime: now.Add(self.settings.ProvenHostCountTimeout),
				}
			}()
		}
	}
	return candidates, now
}

// The answer to a spend one of its budgets refused (connect#230): what the
// refusal is counted as, the wait it asks for, and its words. A budget asks
// for the moment it has room, and up to BudgetRetryAfterJitter past it
// (budgetRetryAfter), so the hosts it refuses together are spread over the
// minute after rather than told the same second. The account's budget asks
// for longer while none of the account's hosts has passed activation on the
// family (`accountUnproven`): the wait unprovenAccountRetryAfter makes of the
// account's attempts over the last UnprovenAccountAttemptLookback, which
// `countAccountAttempts` counts, is counted as unproven_account and says why.
// The words a refusal of each budget had before are kept as its start, which
// is what an activator that predates the longer wait shows.
func (self *extenderActivateService) budgetRefusalAnswer(
	refusal model.AccountActionBudgetRefusal,
	ipVersion int,
	accountUnproven bool,
	countAccountAttempts func(minTime time.Time, maxCount int) int,
) (outcome string, retryAfter time.Duration, message string) {
	settings := self.settings
	outcome = extenderActivationBudgetOutcome(refusal.Action)
	unproven := accountUnproven && 0 < settings.UnprovenAccountRetryAfterMax
	retryAfter = refusal.RetryAfter
	if unproven {
		outcome = extenderActivationOutcomeUnprovenAccount
		attemptCount := countAccountAttempts(
			self.now().Add(-settings.UnprovenAccountAttemptLookback),
			settings.unprovenAccountAttemptCountLimit(),
		)
		retryAfter = max(retryAfter, settings.unprovenAccountRetryAfter(attemptCount))
	}
	refusal.RetryAfter = self.budgetRetryAfter(retryAfter)
	message = refusal.Error()
	if unproven {
		message += fmt.Sprintf(
			" None of your account's extenders has passed activation over IPv%d in the last %s, so its hosts are asked to wait longer between tries. An extender passes once the internet can reach it on its TCP port, 443 by default.",
			ipVersion,
			extenderActivateLookbackText(settings.ProvenHostMaxAge),
		)
	}
	return outcome, refusal.RetryAfter, message
}

// The wait a budget refusal asks for: the moment the budget has room, and up
// to BudgetRetryAfterJitter past it, uniformly.
func (self *extenderActivateService) budgetRetryAfter(retryAfter time.Duration) time.Duration {
	if self.settings.BudgetRetryAfterJitter <= 0 {
		return retryAfter
	}
	return retryAfter + mathrand.N(self.settings.BudgetRetryAfterJitter+1)
}

// The wait a busy refusal asks for, uniform over the settings' bounds.
func (self *extenderActivateService) busyRetryAfter() time.Duration {
	retryAfterMin := self.settings.BusyRetryAfterMin
	retryAfterMax := max(retryAfterMin, self.settings.BusyRetryAfterMax)
	if retryAfterMin == retryAfterMax {
		return retryAfterMin
	}
	return retryAfterMin + mathrand.N(retryAfterMax-retryAfterMin+1)
}

var defaultExtenderActivateService = sync.OnceValue(func() *extenderActivateService {
	return newExtenderActivateService(DefaultExtenderActivateSettings())
})

// ExtenderActivate under other settings, on a service of its own.
func extenderActivate(
	args *ExtenderActivateArgs,
	clientSession *session.ClientSession,
	settings *ExtenderActivateSettings,
) (*ExtenderActivateResult, error) {
	return newExtenderActivateService(settings).activate(args, clientSession)
}

func (self *extenderActivateService) activate(
	args *ExtenderActivateArgs,
	clientSession *session.ClientSession,
) (*ExtenderActivateResult, error) {
	settings := self.settings
	// the steps the probe ran, which every answer after it carries, refusals
	// included
	var probeSteps []*ExtenderProbeStep
	// the stage the activation has reached, which is where a refusal is
	// counted (extender_activation_metrics.go), and how it ended there when
	// the stage says more than refused: how the probe refused, why the api was
	// busy, or which budget refused
	stage := extenderActivationStageRequest
	stageOutcome := ""
	// the caller's family as the count labels it, read before the address is
	// checked so a request refused for anything is counted by family
	family := extenderActivationFamily(clientSession.ClientAddress)
	refuse := func(message string) (*ExtenderActivateResult, error) {
		settings.metrics.observe(
			stage,
			extenderActivationRefusalOutcome(stage, stageOutcome),
			family,
			probeSteps,
		)
		return &ExtenderActivateResult{
			Activated:    false,
			Error:        message,
			ProbeSteps:   probeSteps,
			AddressPorts: true,
		}, nil
	}
	// a refusal that judged nothing about the caller -- a busy api, a spent
	// budget -- with the wait until there is room (connect#230)
	refuseWithRetryAfter := func(message string, retryAfter time.Duration) (*ExtenderActivateResult, error) {
		result, err := refuse(message)
		result.RetryAfterSeconds = extenderActivateRetryAfterSeconds(retryAfter)
		return result, err
	}
	// the refusal of a key another network holds, which names itself in
	// `refusal` so the extender can make a key of its network's own (C2c)
	refuseKeyOtherNetwork := func() (*ExtenderActivateResult, error) {
		result, err := refuse(extenderKeyOtherNetworkRefusal)
		result.Refusal = extenderActivateRefusalKeyOtherNetwork
		return result, err
	}

	publicKey, err := connect.ParseExtenderPublicKeyHex(args.PublicKeyHex)
	if err != nil {
		return refuse(fmt.Sprintf("the extender public key is not readable: %s", err))
	}
	// the camouflage key the records are to carry (phase 14b), checked like
	// every other argument, before anything is probed or spent
	realityPublicKey, err := parseExtenderActivateRealityPublicKey(args.RealityPublicKeyHex)
	if err != nil {
		return refuse(fmt.Sprintf("the extender camouflage key is not readable: %s", err))
	}

	carriers := []string{}
	for _, carrier := range args.Carriers {
		carrier = strings.ToLower(strings.TrimSpace(carrier))
		if carrier == "" || slices.Contains(carriers, carrier) {
			continue
		}
		if _, ok := extenderCarrierConnectMode(carrier); !ok {
			return refuse(fmt.Sprintf("unknown carrier %q", carrier))
		}
		carriers = append(carriers, carrier)
	}
	if len(carriers) == 0 {
		return refuse("an activation must offer at least one carrier")
	}
	// the forward is proved over a required path (C2a): public tcp, or the
	// peer-to-peer webrtc carrier of a host behind nat (connect/EXTENDER.md
	// S8), so an extender that serves neither cannot be proved to forward at
	// all
	if !slices.Contains(carriers, connect.ExtenderCarrierTcp) && !slices.Contains(carriers, connect.ExtenderCarrierWebRtc) {
		return refuse("an activation must offer the tcp carrier or the webrtc carrier")
	}

	tcpPort := args.TcpPort
	if tcpPort == 0 {
		tcpPort = 443
	}
	udpPort := args.UdpPort
	if udpPort == 0 {
		udpPort = 443
	}
	dnsPort := args.DnsPort
	if dnsPort == 0 {
		// the whodis port, which every extender binds; 53 is offered only by
		// the platforms that can bind it without privilege (L2)
		dnsPort = connect.ExtenderDnsPort
	}
	for _, port := range []struct {
		name  string
		value int
	}{
		{name: "tcp_port", value: tcpPort},
		{name: "udp_port", value: udpPort},
		{name: "dns_port", value: dnsPort},
	} {
		if port.value < 1 || 65535 < port.value {
			return refuse(fmt.Sprintf("%s is out of range", port.name))
		}
	}
	// the dns ports to probe, ascending, which is the dial order of L2 and the
	// order the record lists them in. A caller that lists none offers its one
	// configured port, which is what an extender that predates the list sends.
	dnsPorts := []int{}
	for _, listedDnsPort := range args.DnsPorts {
		if listedDnsPort < 1 || 65535 < listedDnsPort {
			return refuse("dns_ports is out of range")
		}
		if !slices.Contains(dnsPorts, listedDnsPort) {
			dnsPorts = append(dnsPorts, listedDnsPort)
		}
	}
	if len(dnsPorts) == 0 {
		dnsPorts = []int{dnsPort}
	}
	slices.Sort(dnsPorts)
	dnsTld := strings.ToLower(strings.TrimSpace(args.DnsTld))
	if dnsTld == "" {
		dnsTld = connect.DefaultExtenderDnsTld
	}
	if 128 < len(dnsTld) {
		return refuse("dns_tld is too long")
	}

	if clientSession.ByJwt == nil || clientSession.ByJwt.ClientId == nil {
		// the route requires a client jwt; this is the structural guard for a
		// caller that reaches the controller another way
		return refuse("an activation requires a client")
	}

	clientIpStr, _, err := server.SplitClientAddress(clientSession.ClientAddress)
	if err != nil {
		return refuse("the caller address is not readable")
	}
	clientIp, err := netip.ParseAddr(clientIpStr)
	if err != nil {
		return refuse("the caller address is not readable")
	}
	// the address the probe dials and the record publishes, in the form of its
	// own family, and that family: an ipv4-mapped caller is its ipv4 address
	clientIp = clientIp.Unmap()
	ipVersion := server.IpVersionForAddr(clientIp)
	if ipVersion != 4 && ipVersion != 6 {
		return refuse("the caller address has no known family")
	}
	// an activation posted to a family api host is for that family (C2), and
	// the probe dials the family the request arrived on. One that arrived on
	// the other family -- an alt or proxy path that crossed families -- would
	// probe, store and answer for the caller's other address as if it were the
	// family the extender asked for, so it is refused before anything is
	// probed, spent or stored
	if hostIpVersion := extenderActivateHostIpVersion(clientSession.Host); hostIpVersion != 0 && hostIpVersion != ipVersion {
		return refuse(fmt.Sprintf(
			"the activation for ipv%d arrived over ipv%d, so the ipv%d address could not be probed",
			hostIpVersion,
			ipVersion,
			hostIpVersion,
		))
	}

	stage = extenderActivationStageConfig
	config, err := EnvExtenderConfig()
	if err != nil {
		return refuse(extenderConfigRefusal(err))
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		return refuse(extenderConfigRefusal(err))
	}
	apiHost, err := config.ApiHost()
	if err != nil {
		return refuse(extenderConfigRefusal(err))
	}
	serverName, err := config.ProbeServerName()
	if err != nil {
		return refuse(extenderConfigRefusal(err))
	}
	connectSettings := ExtenderProbeConnectSettings()

	// A key belongs to the network that first activated it
	// (model.ActivateNetworkExtender), and one another network holds is
	// refused here, before anything is probed or spent. The probes cannot
	// tell the key's owner from anyone else: an open extender signs any
	// challenge and forwards any request to the api (A4, A5), so a post
	// relayed through a listed extender passes them with its key, and would
	// otherwise take its record, and the contract parties its row names (J2),
	// to the poster's network.
	stage = extenderActivationStageOwner
	networkId := clientSession.ByJwt.NetworkId
	if extenderKeyHeldByAnotherNetwork(clientSession.Ctx, publicKey, networkId) {
		return refuseKeyOtherNetwork()
	}

	// The probe guard (connect#230): the process probes at most
	// MaxConcurrentProbes activations at once. One more is refused as busy
	// here, after every check that refuses without a probe and before any
	// budget is spent, so the refused caller loses nothing and is told when to
	// come back. The token is held to the end of the probe.
	select {
	case self.probeSlots <- struct{}{}:
	default:
		stage = extenderActivationStageBusy
		stageOutcome = extenderActivationOutcomeProbes
		retryAfter := self.busyRetryAfter()
		return refuseWithRetryAfter(
			fmt.Sprintf(
				"The operator is busy probing other extenders. Please try again in %s.",
				model.AccountActionRetryText(retryAfter),
			),
			retryAfter,
		)
	}
	releaseProbeSlot := sync.OnceFunc(func() {
		<-self.probeSlots
	})
	defer releaseProbeSlot()

	// The budgets are spent here, at the probe (ExtenderActivateSettings):
	// after every check that refuses without dialing the caller -- the
	// arguments, the client, the caller address and its family, the family
	// the activation was posted for, the operator's configuration, the key's
	// network -- and before the first dial. A malformed request, a post that
	// crossed families, an operator that cannot probe and a key another
	// network holds spend nothing, while every activation that dials the
	// caller is counted before it dials, whatever the probe then finds. The
	// calling client's budget, its address's and its account's are spent
	// together or not at all, so a client refused by its account's budget
	// keeps its own, and all are the budgets of the family the probe dials. A
	// refusal says when the budget that refused has room again, which the
	// activator waits out (`retry_after_seconds`).
	stage = extenderActivationStageBudget
	clientRateLimitAction, addressRateLimitAction, accountRateLimitAction := extenderActivateRateLimitActions(ipVersion)
	addressBudget := model.AccountActionBudget{
		Id:                settings.addressBudgetId(clientIp),
		Action:            addressRateLimitAction,
		Limit:             settings.AddressRateLimit,
		Window:            settings.RateLimitWindow,
		AttemptTimeSpread: settings.BudgetAttemptTimeSpread,
	}
	// a key its network already proved at this address is the network's own
	// extender re-activating where it lives. A public extender relays posts
	// to the api from its own address, so any account's posts through it
	// spend the address's budget; the key's own posts there spend a budget
	// of their own, which no other post can, so others cannot hold its
	// re-activation off (connect#230)
	if settings.keyProvenAtAddress(
		model.GetNetworkExtenderKeyAddress(clientSession.Ctx, publicKey, ipVersion),
		networkId,
		clientIp,
		server.NowUtc(),
	) {
		addressBudget = model.AccountActionBudget{
			Id:                settings.provenAddressBudgetId(clientIp, publicKey),
			Action:            extenderActivateProvenAddressRateLimitAction(ipVersion),
			Limit:             settings.ProvenAddressRateLimit,
			Window:            settings.RateLimitWindow,
			AttemptTimeSpread: settings.BudgetAttemptTimeSpread,
		}
	}
	// whether the account's limit past its floor was read, and whether any of
	// its hosts had passed activation on the family when it was
	// (accountPassed). The spend reads its budgets again under the locks, and
	// the last read is the one that decided
	accountLimitRead := false
	accountPassed := false
	if err := model.CheckAndRecordAccountActionBudgets(
		clientSession.Ctx,
		model.AccountActionBudget{
			Id:                *clientSession.ByJwt.ClientId,
			Action:            clientRateLimitAction,
			Limit:             settings.ClientRateLimit,
			Window:            settings.RateLimitWindow,
			AttemptTimeSpread: settings.BudgetAttemptTimeSpread,
		},
		addressBudget,
		model.AccountActionBudget{
			Id:                clientSession.ByJwt.UserId,
			Action:            accountRateLimitAction,
			Limit:             settings.AccountRateLimit,
			Window:            settings.RateLimitWindow,
			AttemptTimeSpread: settings.BudgetAttemptTimeSpread,
			// past the floor the account's limit is what its proven hosts
			// raised it to (connect#230)
			ExtendLimitInTx: func(ctx context.Context, tx server.PgTx) int {
				provenHostCount, passed := self.provenHostCountInTx(ctx, tx, networkId, ipVersion)
				accountLimitRead = true
				accountPassed = passed
				return settings.accountRateLimit(provenHostCount)
			},
		},
	); err != nil {
		if errors.Is(err, model.ErrAccountActionBudgetsBusy) {
			// other spends held a budget past the lock bound: nothing was
			// spent, so the caller is busy-refused like one over the probe
			// guard rather than answered with an error (connect#230)
			stage = extenderActivationStageBusy
			stageOutcome = extenderActivationOutcomeBudgetLock
			retryAfter := self.busyRetryAfter()
			return refuseWithRetryAfter(
				fmt.Sprintf(
					"The operator is busy activating other extenders. Please try again in %s.",
					model.AccountActionRetryText(retryAfter),
				),
				retryAfter,
			)
		}
		var budgetRefusal *model.AccountActionBudgetRefusal
		if errors.As(err, &budgetRefusal) {
			// the account's budget, refusing an account none of whose hosts
			// has passed on the family, asks for the longer wait its attempts
			// make (unprovenAccountRetryAfter)
			accountUnproven := budgetRefusal.Action == accountRateLimitAction && accountLimitRead && !accountPassed
			outcome, retryAfter, message := self.budgetRefusalAnswer(
				*budgetRefusal,
				ipVersion,
				accountUnproven,
				func(minTime time.Time, maxCount int) int {
					return model.CountAccountActionAttempts(
						clientSession.Ctx,
						clientSession.ByJwt.UserId,
						accountRateLimitAction,
						minTime,
						maxCount,
					)
				},
			)
			stageOutcome = outcome
			return refuseWithRetryAfter(message, retryAfter)
		}
		// the budget check failed rather than refused: counted as the error
		// it is, never as a refusal by the account's budget
		stageOutcome = extenderActivationOutcomeError
		return refuse(err.Error())
	}

	// the acceptance rule (C2a) over the real probes: the required test, then
	// each optional carrier offered, which is left out when it does not answer
	probes := &extenderActivationProbes{
		carrier: func(
			ctx context.Context,
			connectMode connect.ExtenderConnectMode,
			port int,
		) error {
			// a splicing (Phase B) extender sends a legacy hello to its
			// borrowed site, so the tcp probe also runs the camouflaged
			// attempt with the posted key (connect
			// net_extender_camouflage_probe.go); it passes on either attempt
			// and claims nothing about the key
			_, err := connect.ProbeExtenderCarrierWithCamouflageKey(
				ctx,
				connectSettings,
				clientIp,
				connectMode,
				port,
				dnsTld,
				serverName,
				publicKey,
				realityPublicKey,
				apiHost,
				ExtenderProbeDestinationPort,
			)
			return err
		},
		forward: func(ctx context.Context) (string, error) {
			return connect.ProbeExtenderForwardWithCamouflageKey(
				ctx,
				connectSettings,
				clientIp,
				tcpPort,
				serverName,
				publicKey,
				realityPublicKey,
				config.ApiUrl,
				extenderForwardProbeTlsConfig(),
			)
		},
		// signaled to the client the posting jwt names, which is the client
		// the record will name as the rendezvous, and to no other
		webRtc: func(ctx context.Context) (string, error) {
			return probeExtenderWebRtcActivation(
				ctx,
				*clientSession.ByJwt.ClientId,
				publicKey,
				ipVersion,
				clientIp,
				config.ApiUrl,
				extenderForwardProbeTlsConfig(),
			)
		},
	}
	if self.testProbes != nil {
		probes = self.testProbes(clientIp, ipVersion)
	}
	if probes.webRtcOnlyRoom == nil {
		// the operator's cap on extenders accepted on webrtc without tcp,
		// whatever probes run: read only when the webrtc test is to run, and
		// never counting the activating key's own extender
		maxWebRtcOnlyExtenders := settings.MaxWebRtcOnlyExtenders
		probes.webRtcOnlyRoom = func(ctx context.Context) error {
			return extenderWebRtcOnlyRoom(
				maxWebRtcOnlyExtenders,
				model.CountActiveWebRtcOnlyNetworkExtenders(ctx, publicKey),
			)
		}
	}
	outcome := probeExtenderActivation(
		clientSession.Ctx,
		newExtenderActivationClock(),
		probes,
		settings.TcpChallengeTimeout,
		carriers,
		tcpPort,
		udpPort,
		dnsPorts,
		ipVersion,
	)
	releaseProbeSlot()
	probeSteps = outcome.steps
	if outcome.refusal != "" {
		stage = outcome.refusalStep
		stageOutcome = outcome.refusalOutcome
		return refuse(outcome.refusal)
	}
	stage = extenderActivationStageStore

	// The location of the activating address (M1). The country code goes on the
	// record's country field as it always has; the four location ids go beside
	// it so the map and the by-country gauge read a named location rather than
	// a bare code. CreateLocation fills the ids in place, exactly as the mmdb
	// path of a connection does (network_client_controller.go).
	//
	// Nothing here is allowed to fail the activation: a lookup that finds
	// nothing, a country the location table refuses to name, and a country-only
	// answer with neither a city nor a region all leave what they could not
	// resolve unset and store the rest.
	countryCode := ""
	var activationLocation *model.Location
	// the genesis accuracy radius of the lookup (connect/GEOMAP.md §5.1),
	// stored with the location it qualifies and never without one
	var activationAccuracyKm *float32
	if location, scores, err := GetLocationForIp(clientSession.Ctx, clientIpStr); err == nil {
		countryCode = location.CountryCode
		// CreateLocation raises on a country it cannot name, and it is the
		// only thing here that writes: contain it so a location the table
		// refuses can never turn a successful probe into a refused activation
		if r := server.HandleError(func() {
			model.CreateLocation(clientSession.Ctx, location)
		}); r == nil {
			activationLocation = location
			activationAccuracyKm = scores.AccuracyKm
		} else if glog.V(1) {
			glog.Infof("[extender]no location row for the activating address: %s\n", r)
		}
	} else if glog.V(1) {
		glog.Infof("[extender]no location for the activating address: %s\n", err)
	}

	// the privacy-preserving address key, kept with the activation history the
	// way a connection keeps it (M1); an address that cannot be read stores
	// none rather than failing a probe that passed
	var clientAddressHash []byte
	if hash, _, err := clientSession.ClientAddressHashPort(); err == nil {
		clientAddressHash = hash[:]
	}

	// an owner who opted out of lag.dev measurement is withdrawn now, with the
	// key proven by the probes, and before this activation signs its record,
	// so the record already carries no measurement id (lag.dev/PLAN.md C9).
	// The withdrawal is keyed by the key alone, so the key's network is read
	// again first: another network may have activated it while this one
	// probed, and its measurement is not this caller's to withdraw
	if args.MeasurementDisabled {
		if extenderKeyHeldByAnotherNetwork(clientSession.Ctx, publicKey, networkId) {
			stage = extenderActivationStageOwner
			return refuseKeyOtherNetwork()
		}
		optOutExtenderMeasurement(clientSession.Ctx, publicKey, server.NowUtc())
	}

	// the run of the extender process that posted, kept with the history row
	// so a restart shows as a new run id (connect/EXTENDER.md G3)
	runId, runStartTime, previousExit := extenderActivationRun(args, server.NowUtc())

	var record *protocol.ExtenderRecord
	var expireTime time.Time
	activated := model.ActivateNetworkExtender(
		clientSession.Ctx,
		(&model.NetworkExtenderActivation{
			NetworkId:   clientSession.ByJwt.NetworkId,
			ClientId:    *clientSession.ByJwt.ClientId,
			PublicKey:   publicKey,
			TcpPort:     tcpPort,
			UdpPort:     udpPort,
			DnsPort:     dnsPort,
			DnsTld:      dnsTld,
			CountryCode: countryCode,
			IpVersion:   ipVersion,
			Ip:          clientIp,
			// only the carriers that answered: a client is never sent to one
			// that was left out (C2a)
			Carriers: outcome.carriers,
			DnsPorts: outcome.dnsPorts,
			// the operator's policy, by the activating network (R1)
			DirectoryTier: config.DirectoryTierForNetwork(clientSession.ByJwt.NetworkId),

			ClientAddressHash: clientAddressHash,
			AccuracyKm:        activationAccuracyKm,

			RunId:        runId,
			RunStartTime: runStartTime,
			PreviousExit: previousExit,
			// stored only by this passing activation, for the network that
			// holds the key (phase 14b)
			RealityPublicKey: realityPublicKey,
		}).WithLocation(activationLocation),
		func(
			extender *model.NetworkExtender,
			addresses []*model.NetworkExtenderAddress,
			issueTime time.Time,
		) ([]byte, error) {
			signedRecord, message, err := SignExtenderRecord(
				config,
				rootPrivateKey,
				extender,
				addresses,
				issueTime,
			)
			if err != nil {
				return nil, err
			}
			record = signedRecord
			expireTime = issueTime.Add(ExtenderRecordExpireTimeout)
			return message, nil
		},
	)
	if activated == nil || record == nil {
		// the store refuses a key another network holds, which another
		// network may have activated while this one probed
		if extenderKeyHeldByAnotherNetwork(clientSession.Ctx, publicKey, networkId) {
			stage = extenderActivationStageOwner
			return refuseKeyOtherNetwork()
		}
		return refuse("the activation could not be stored")
	}

	// the lag.dev measurement certificate request rides the activation
	// (lag.dev/PLAN.md A10); what becomes of it never changes this answer.
	// While the owner opts out the request is not stored: the flag wins
	if args.MeasurementCsr != "" && !args.MeasurementDisabled {
		storeExtenderMeasurementRequest(
			clientSession.Ctx,
			config,
			activated.Extender.ExtenderId,
			args.MeasurementCsr,
			server.NowUtc(),
		)
	}

	recordBase64, err := encodeExtenderRecord(record)
	if err != nil {
		return refuse("the activation record could not be encoded")
	}

	bootstrap := []string{}
	for _, other := range model.GetRandomActiveNetworkExtenders(
		clientSession.Ctx,
		ExtenderBootstrapCount,
		activated.Extender.ExtenderId,
	) {
		// the sample is signed now rather than replayed from the publish
		// queue, so a bootstrap record is as fresh as the one the caller gets
		// for itself and expires no sooner
		otherRecord, _, err := SignExtenderRecord(
			config,
			rootPrivateKey,
			other.Extender,
			other.Addresses,
			server.NowUtc(),
		)
		if err != nil {
			continue
		}
		otherRecordBase64, err := encodeExtenderRecord(otherRecord)
		if err != nil {
			continue
		}
		bootstrap = append(bootstrap, otherRecordBase64)
	}

	settings.metrics.observe(
		extenderActivationStageStore,
		extenderActivationOutcomeActivated,
		family,
		probeSteps,
	)
	extenderId := activated.Extender.ExtenderId
	return &ExtenderActivateResult{
		Activated:       true,
		ExtenderId:      &extenderId,
		Ip:              clientIp.String(),
		IpVersion:       ipVersion,
		Carriers:        outcome.carriers,
		DnsPorts:        outcome.dnsPorts,
		RefusedCarriers: outcome.refusedCarriers,
		ProbeSteps:      outcome.steps,
		ExpireTime:      &expireTime,
		AllowedHosts:    config.AllowedHosts(),
		Record:          recordBase64,
		Bootstrap:       bootstrap,
		AddressPorts:    true,
	}, nil
}

// The probes of one activation (C2): the challenge over one carrier on one
// port, and the verified `GET /hello` through the tcp carrier, which answers
// the caller address the api saw. Each is bound to the caller's address, its
// key and the operator's probe settings. probeExtenderActivation judges the
// acceptance rule over them, so the rule is tested without sockets.
type extenderActivationProbes struct {
	carrier func(ctx context.Context, connectMode connect.ExtenderConnectMode, port int) error
	forward func(ctx context.Context) (string, error)
	// the webrtc test (C2a, connect/EXTENDER.md S8): one dial over the
	// carrier through the signaling relay to the activating client, which
	// verifies the key and then the `GET /hello` through it, answering the
	// caller address the api saw. Nil is an operator with no signaling path.
	webRtc func(ctx context.Context) (string, error)
	// whether the operator takes another extender on the webrtc carrier
	// without tcp (ExtenderActivateSettings.MaxWebRtcOnlyExtenders), asked
	// just before the webrtc test runs, which is only where the tcp path
	// failed or was not offered: nil when it does, an error wrapping
	// ErrExtenderWebRtcOnlyCap past the cap, any other error when the count
	// could not be read, either of which refuses the webrtc path untried.
	// Nil asks nothing, which is a test's.
	webRtcOnlyRoom func(ctx context.Context) error
}

// ErrExtenderWebRtcOnlyCap is the refusal of the webrtc test of an activation
// whose tcp path failed or was not offered, once the operator has as many
// extenders on the webrtc carrier without tcp as it takes for now
// (ExtenderActivateSettings.MaxWebRtcOnlyExtenders, connect/EXTENDER.md S8).
var ErrExtenderWebRtcOnlyCap = errors.New("the operator accepts no more extenders on the webrtc carrier without tcp for now")

// Whether one more extender may be accepted on the webrtc carrier without
// tcp, given the cap and how many the operator has, its own left out: nil
// while there is room, else the cap's refusal, which says how many there are
// and that an extender the internet reaches on tcp is still accepted.
func extenderWebRtcOnlyRoom(maxCount int, count int) error {
	if count < maxCount {
		return nil
	}
	return fmt.Errorf(
		"%w (%d accepted, the cap is %d); an extender whose tcp port the internet reaches is accepted on tcp",
		ErrExtenderWebRtcOnlyCap,
		count,
		max(0, maxCount),
	)
}

// The clock the probes of one activation run on (C2): the time each step is
// read at, and the deadline that bounds the shared probe budget. Production
// is the real clock; a test drives a fake one, or the real one in a synctest
// bubble, so a step that takes 9.5 s, or one that waits out its budget, does
// so at once and to the instant.
type extenderActivationClock struct {
	now func() time.Time
	// context.WithDeadline on this clock: the context ends with
	// context.DeadlineExceeded when this clock reaches the deadline
	withDeadline func(ctx context.Context, deadline time.Time) (context.Context, context.CancelFunc)
}

// The real clock, whose readings carry the monotonic time, so a step is
// timed by the time that passed and not by the wall clock.
func newExtenderActivationClock() *extenderActivationClock {
	return &extenderActivationClock{
		now:          time.Now,
		withDeadline: context.WithDeadline,
	}
}

// The names of the probe steps of one activation (ExtenderProbeStep): each
// carrier's challenge, which is the carrier's name, and the forward.
const extenderProbeStepForward = "forward"

// How a probe step ended (ExtenderProbeStep).
const (
	// it answered
	extenderProbeStepOutcomeOk = "ok"
	// it failed on its own, before its budget ran out
	extenderProbeStepOutcomeError = "error"
	// its budget ran out before it answered
	extenderProbeStepOutcomeTimeout = "timeout"
)

// How the required test of an activation refused it (C2a), for the outcome
// count: the step failed or timed out, or the forward answered on the other
// family.
const extenderProbeRefusalOtherFamily = "other_family"

// What the probes of one activation found (C2a).
type extenderActivationOutcome struct {
	// why the required test failed, empty when it passed. When it is set no
	// optional carrier was probed and nothing may be stored.
	refusal string
	// the step of the required test that refused the activation, tcp or
	// forward, and how: error, timeout or other_family. Empty when the
	// required test passed.
	refusalStep    string
	refusalOutcome string
	// the carriers that answered, in the order offered: the tcp carrier the
	// required test passed on, and each optional carrier that answered
	carriers []string
	// the dns ports that answered, ascending (L2)
	dnsPorts []int
	// the optional carriers, and the dns ports, that did not answer, in the
	// order offered
	refusedCarriers []*ExtenderCarrierRefusal
	// every step probed: the required test's in the order probed, then the
	// optional carriers', which run together, listed as the dns ports in the
	// order offered and then quic. A refusal's last step is the one that
	// refused it.
	steps []*ExtenderProbeStep
}

// One probe step as it ran (C2): the step the answer lists, and the times
// and the error its messages are made from.
type extenderProbeStepRun struct {
	step *ExtenderProbeStep
	// how long it ran
	elapsed time.Duration
	// how long it was allowed to run: what was left of the shared budget, or
	// less when the caller's own context ended first
	allowance time.Duration
	// the shared budget, rather than an earlier deadline of the caller's
	// context, bounded it
	sharedBudget bool
	err          error
}

// How much time a step was given, as its timeout says it: "within the 10 s
// probe budget" for a step that had all of the shared budget, "within the
// 0.5 s left of the 10 s probe budget" for one that ran out of what was left
// of it, and "within 3 s" for one bounded by an earlier deadline of its own.
func (self *extenderProbeStepRun) withinText() string {
	if !self.sharedBudget {
		return fmt.Sprintf("within %s", extenderProbeSecondsText(self.allowance))
	}
	budgetText := extenderProbeSecondsText(ExtenderActivateProbeTimeout)
	if leftText := extenderProbeSecondsText(self.allowance); leftText != budgetText {
		return fmt.Sprintf("within the %s left of the %s probe budget", leftText, budgetText)
	}
	return fmt.Sprintf("within the %s probe budget", budgetText)
}

// The refusal of a step of the required test that did not pass (C2a). One
// that timed out says how much time it had, so an extender that answers late
// can tell that from one that does not answer at all (connect#230); one that
// failed on its own says when, and makes no claim about the budget.
func (self *extenderProbeStepRun) refusal(subject string) string {
	if self.step.Outcome == extenderProbeStepOutcomeTimeout {
		return fmt.Sprintf("the %s did not answer %s: %s", subject, self.withinText(), self.err)
	}
	return fmt.Sprintf("the %s failed after %s: %s", subject, extenderProbeSecondsText(self.elapsed), self.err)
}

// Why an optional carrier, or a dns port, was left out (C2a): the probe's own
// error, and for a probe whose budget ran out first, how much time it had.
func (self *extenderProbeStepRun) carrierRefusal(carrier string, port int) *ExtenderCarrierRefusal {
	errorText := self.err.Error()
	if self.step.Outcome == extenderProbeStepOutcomeTimeout {
		errorText = fmt.Sprintf("no answer %s: %s", self.withinText(), self.err)
	}
	return &ExtenderCarrierRefusal{
		Carrier:      carrier,
		Port:         port,
		Error:        errorText,
		ElapsedMs:    self.step.ElapsedMs,
		BudgetLeftMs: self.step.BudgetLeftMs,
	}
}

// A duration as an activation message gives it, in seconds to the hundredth:
// "3 s", "0.5 s", "2.43 s".
func extenderProbeSecondsText(d time.Duration) string {
	seconds := math.Round(d.Seconds()*100) / 100
	return strconv.FormatFloat(seconds, 'f', -1, 64) + " s"
}

// Judges one activation by the acceptance rule of C2a.
//
// Two paths are required tests, and an activation must pass one it offered:
// public tcp -- the challenge on the tcp carrier, the verified forward through
// it, and the family the forward arrived on (A7) -- and peer-to-peer webrtc --
// one dial over the carrier through the exchange signaling, which challenges
// the extender and verifies its key, then the verified forward through it and
// the same family check (connect/EXTENDER.md S8). The webrtc test runs only
// where the tcp path failed or was not offered, and never beside a tcp test
// that may still pass (owner, 2026-11-12): an extender the operator reaches on
// tcp is accepted on tcp, its device is sent no offer, and webrtc is neither
// stored for it nor listed as refused. An activation that passes no required
// path it ran is refused with the failure of each named, and nothing else is
// probed. Only once a required path has passed are the optional carriers
// probed, when they were offered. One that does not answer is left out and
// listed; it never refuses an activation a required path passed
// (connect#230). The order is part of the rule: a udp carrier gives no answer
// to fail fast on, so an optional carrier probed before the forward could
// spend the budget the forward needs and gate the activation after all.
//
// An activation that offers tcp without webrtc is judged exactly as it always
// was: the tcp path under the whole shared budget, then the optional
// carriers. One that offers webrtc without tcp runs the webrtc test under the
// whole shared budget. One that offers both runs the tcp path first with its
// challenge cut at tcpChallengeTimeout (ExtenderActivateSettings), since a
// host behind nat that binds tcp 443 gets no answer on it, and a challenge
// that waited out the shared budget would leave the webrtc test none; when the
// tcp path fails, at the challenge or after it, the webrtc test runs in its
// place under a budget of its own, ExtenderActivateProbeTimeout long and
// ending no later than tcpChallengeTimeout past the shared one, and the failed
// tcp path is listed with the refused carriers. Before the webrtc test runs the
// operator's cap is asked (ExtenderActivateSettings.MaxWebRtcOnlyExtenders),
// which refuses it untried once the operator has as many extenders on webrtc
// without tcp as it takes for now.
//
// The optional carriers -- each offered dns port and quic -- are probed all at
// once, each until it answers or the budget of the required path that passed
// ends (connect#230). A udp port with nothing bound waits out the budget, and
// probed one after the other, each port under a window of its own (L2), a dead
// port spent the time of every carrier after it, while a live one that
// answered after its window was left out as if nothing listened on it: an
// extender whose first reply came 2.4-4.3 s after the hello passed the tcp
// test and lost its dns and quic carriers. Probed together, no carrier waits
// on another, every one has all of what is left of the budget after the
// required test, and the activation still ends with the budget, never later.
//
// Every step runs on the clock: the shared budget ends
// ExtenderActivateProbeTimeout after the judging starts. Each step is timed,
// and the outcome lists every step with how long it ran and what was left of
// its budget when it started: the tcp path's, then the webrtc test's, then the
// optional carriers'. A step whose budget ran out before it answered timed
// out, and its refusal says how much time it had; a late answer and no answer
// were the same "context deadline exceeded" before (connect#230).
func probeExtenderActivation(
	ctx context.Context,
	clock *extenderActivationClock,
	probes *extenderActivationProbes,
	tcpChallengeTimeout time.Duration,
	carriers []string,
	tcpPort int,
	udpPort int,
	dnsPorts []int,
	ipVersion int,
) *extenderActivationOutcome {
	startTime := clock.now()
	probeDeadline := startTime.Add(ExtenderActivateProbeTimeout)
	probeCtx, probeCancel := clock.withDeadline(ctx, probeDeadline)
	defer probeCancel()

	// A budget steps run under: its context, which ends at its deadline, and
	// the deadline. The shared budget, and the webrtc test's own when it runs
	// in place of a tcp path that failed.
	type probeBudget struct {
		ctx      context.Context
		deadline time.Time
	}
	sharedBudget := &probeBudget{
		ctx:      probeCtx,
		deadline: probeDeadline,
	}

	// times one step under stepCtx, which ends at the budget's deadline or
	// sooner. A step that failed once its context had reached its deadline
	// timed out; any other failure is the step's own. It records nothing, so
	// the optional carriers can be timed together and listed in a fixed order.
	timeStep := func(
		budget *probeBudget,
		stepCtx context.Context,
		name string,
		port int,
		probe func(ctx context.Context) error,
	) *extenderProbeStepRun {
		stepStart := clock.now()
		err := probe(stepCtx)
		elapsed := clock.now().Sub(stepStart)
		stepDeadline, _ := stepCtx.Deadline()
		run := &extenderProbeStepRun{
			step: &ExtenderProbeStep{
				Name:         name,
				IpVersion:    ipVersion,
				Port:         port,
				ElapsedMs:    elapsed.Milliseconds(),
				BudgetLeftMs: max(0, budget.deadline.Sub(stepStart)).Milliseconds(),
				Outcome:      extenderProbeStepOutcomeOk,
			},
			elapsed:      elapsed,
			allowance:    max(0, stepDeadline.Sub(stepStart)),
			sharedBudget: !stepDeadline.Before(budget.deadline),
			err:          err,
		}
		if err != nil {
			run.step.Outcome = extenderProbeStepOutcomeError
			if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
				run.step.Outcome = extenderProbeStepOutcomeTimeout
			}
		}
		return run
	}

	// One required path as it ran: its steps in order, and, when it failed,
	// why, at which step and how, and the refusal listed for it when the
	// other path passed in its place.
	type requiredPathRun struct {
		steps          []*ExtenderProbeStep
		refusal        string
		refusalStep    string
		refusalOutcome string
		refusedCarrier *ExtenderCarrierRefusal
	}
	// times one step of a required path and records it, in order
	runStep := func(
		path *requiredPathRun,
		budget *probeBudget,
		stepCtx context.Context,
		name string,
		port int,
		probe func(ctx context.Context) error,
	) *extenderProbeStepRun {
		run := timeStep(budget, stepCtx, name, port, probe)
		path.steps = append(path.steps, run.step)
		return run
	}
	fail := func(
		path *requiredPathRun,
		run *extenderProbeStepRun,
		carrier string,
		port int,
		refusal string,
		refusalOutcome string,
	) {
		path.refusal = refusal
		path.refusalStep = run.step.Name
		path.refusalOutcome = refusalOutcome
		path.refusedCarrier = &ExtenderCarrierRefusal{
			Carrier:      carrier,
			Port:         port,
			Error:        refusal,
			ElapsedMs:    run.step.ElapsedMs,
			BudgetLeftMs: run.step.BudgetLeftMs,
		}
	}

	offersWebRtc := slices.Contains(carriers, connect.ExtenderCarrierWebRtc)
	// without webrtc the tcp path is the required test, whatever the list
	// says, as it always was; the list only says what may be stored
	runsTcp := slices.Contains(carriers, connect.ExtenderCarrierTcp) || !offersWebRtc

	// the tcp path (C2a): the challenge on the tcp carrier, the forward through
	// it, and the family the forward arrived on, under the shared budget
	probeTcpPath := func(path *requiredPathRun) {
		// the webrtc test waits on this path, so a challenge nothing answers
		// must not take its budget: it has its share alone (above)
		challengeCtx := probeCtx
		if offersWebRtc {
			var challengeCancel context.CancelFunc
			challengeCtx, challengeCancel = clock.withDeadline(probeCtx, startTime.Add(tcpChallengeTimeout))
			defer challengeCancel()
		}
		tcpRun := runStep(path, sharedBudget, challengeCtx, connect.ExtenderCarrierTcp, tcpPort, func(ctx context.Context) error {
			return probes.carrier(ctx, connect.ExtenderConnectModeTcpTls, tcpPort)
		})
		if tcpRun.err != nil {
			fail(path, tcpRun, connect.ExtenderCarrierTcp, tcpPort, tcpRun.refusal(connect.ExtenderCarrierTcp+" carrier"), tcpRun.step.Outcome)
			return
		}
		var forwardClientAddress string
		// the forward runs over the tcp carrier, so it is the tcp port it dials
		forwardRun := runStep(path, sharedBudget, probeCtx, extenderProbeStepForward, tcpPort, func(ctx context.Context) error {
			var err error
			forwardClientAddress, err = probes.forward(ctx)
			return err
		})
		if forwardRun.err != nil {
			fail(path, forwardRun, connect.ExtenderCarrierTcp, tcpPort, forwardRun.refusal(extenderProbeStepForward), forwardRun.step.Outcome)
			return
		}
		// an extender must egress on the family it was reached on (A7); a
		// forward that arrives on the other family would publish an address
		// that does not carry the traffic the record promises
		if forwardIpVersion := server.ClientAddressIpVersion(forwardClientAddress); forwardIpVersion != ipVersion {
			// the forward answered, but not as the required test needs
			forwardRun.step.Outcome = extenderProbeStepOutcomeError
			fail(
				path,
				forwardRun,
				connect.ExtenderCarrierTcp,
				tcpPort,
				fmt.Sprintf(
					"the forward arrived on ipv%d, not ipv%d, after %s",
					forwardIpVersion,
					ipVersion,
					extenderProbeSecondsText(forwardRun.elapsed),
				),
				extenderProbeRefusalOtherFamily,
			)
		}
	}

	// the webrtc path (C2a, connect/EXTENDER.md S8): the operator's cap, then
	// one dial over the carrier, which proves the key and then the forward
	// through it, and the family the forward arrived on, under budget. The
	// carrier dials no port, so the step names none. A cap that refuses it, or
	// cannot be read, sends the extender nothing and lists no step.
	probeWebRtcPath := func(path *requiredPathRun, budget *probeBudget) {
		if err := askExtenderWebRtcOnlyRoom(budget.ctx, probes); err != nil {
			path.refusal = fmt.Sprintf("the %s carrier was not tried: %s", connect.ExtenderCarrierWebRtc, err)
			path.refusalStep = connect.ExtenderCarrierWebRtc
			path.refusalOutcome = extenderActivationOutcomeError
			if errors.Is(err, ErrExtenderWebRtcOnlyCap) {
				path.refusalOutcome = extenderActivationOutcomeCap
			}
			return
		}
		var forwardClientAddress string
		webRtcRun := runStep(path, budget, budget.ctx, connect.ExtenderCarrierWebRtc, 0, func(ctx context.Context) error {
			var err error
			forwardClientAddress, err = probeRequiredWebRtc(ctx, probes)
			return err
		})
		if webRtcRun.err != nil {
			fail(path, webRtcRun, connect.ExtenderCarrierWebRtc, 0, webRtcRun.refusal(connect.ExtenderCarrierWebRtc+" carrier"), webRtcRun.step.Outcome)
			return
		}
		// the extender's forward follows the family of the ice pair it was
		// reached on (A7), which the probe pins to the activation's family
		if forwardIpVersion := server.ClientAddressIpVersion(forwardClientAddress); forwardIpVersion != ipVersion {
			webRtcRun.step.Outcome = extenderProbeStepOutcomeError
			fail(
				path,
				webRtcRun,
				connect.ExtenderCarrierWebRtc,
				0,
				fmt.Sprintf(
					"the webrtc forward arrived on ipv%d, not ipv%d, after %s",
					forwardIpVersion,
					ipVersion,
					extenderProbeSecondsText(webRtcRun.elapsed),
				),
				extenderProbeRefusalOtherFamily,
			)
		}
	}

	tcpPath := &requiredPathRun{}
	webRtcPath := &requiredPathRun{}
	// the required paths that ran, in the order they ran
	requiredPaths := []*requiredPathRun{}
	// the budget of the required path that passed, which the optional
	// carriers run under; nil while none has
	var passedBudget *probeBudget
	if runsTcp {
		requiredPaths = append(requiredPaths, tcpPath)
		probeTcpPath(tcpPath)
		if tcpPath.refusal == "" {
			passedBudget = sharedBudget
		}
	}
	// webrtc only where the tcp path failed or was not offered
	webRtcRan := false
	if offersWebRtc && passedBudget == nil {
		webRtcRan = true
		requiredPaths = append(requiredPaths, webRtcPath)
		webRtcBudget := sharedBudget
		if runsTcp {
			// in place of the tcp path that failed: a budget of its own, as
			// long as the shared one and ending no later than the challenge's
			// share past it, so the activation's probes end within both
			webRtcDeadline := clock.now().Add(ExtenderActivateProbeTimeout)
			if latestDeadline := probeDeadline.Add(max(0, tcpChallengeTimeout)); latestDeadline.Before(webRtcDeadline) {
				webRtcDeadline = latestDeadline
			}
			webRtcCtx, webRtcCancel := clock.withDeadline(ctx, webRtcDeadline)
			defer webRtcCancel()
			webRtcBudget = &probeBudget{
				ctx:      webRtcCtx,
				deadline: webRtcDeadline,
			}
		}
		probeWebRtcPath(webRtcPath, webRtcBudget)
		if webRtcPath.refusal == "" {
			passedBudget = webRtcBudget
		}
	}
	steps := []*ExtenderProbeStep{}
	for _, path := range requiredPaths {
		steps = append(steps, path.steps...)
	}
	if passedBudget == nil {
		// every required path that ran failed: refused with each failure
		// named, at the last one's step
		refusals := []string{}
		for _, path := range requiredPaths {
			refusals = append(refusals, path.refusal)
		}
		last := requiredPaths[len(requiredPaths)-1]
		return &extenderActivationOutcome{
			refusal:        strings.Join(refusals, "; "),
			refusalStep:    last.refusalStep,
			refusalOutcome: last.refusalOutcome,
			steps:          steps,
		}
	}

	answered := map[string]bool{}
	refusals := map[string][]*ExtenderCarrierRefusal{}
	if runsTcp && tcpPath.refusal == "" {
		answered[connect.ExtenderCarrierTcp] = true
	}
	if webRtcRan && webRtcPath.refusal == "" {
		answered[connect.ExtenderCarrierWebRtc] = true
		// the tcp path it ran in place of, left out and listed
		if tcpPath.refusedCarrier != nil {
			refusals[connect.ExtenderCarrierTcp] = append(refusals[connect.ExtenderCarrierTcp], tcpPath.refusedCarrier)
		}
	}

	// the optional carriers to probe, in the order their steps are listed:
	// each offered dns port, then quic
	type optionalCarrierProbe struct {
		carrier     string
		connectMode connect.ExtenderConnectMode
		port        int
		run         *extenderProbeStepRun
	}
	optionalProbes := []*optionalCarrierProbe{}
	if slices.Contains(carriers, connect.ExtenderCarrierDns) {
		// one dns port failing is not the carrier failing: an extender that
		// offers 53 and 4053 on a path that blocks 53 is still reachable on
		// 4053, and the record must say which
		for _, port := range dnsPorts {
			optionalProbes = append(optionalProbes, &optionalCarrierProbe{
				carrier:     connect.ExtenderCarrierDns,
				connectMode: connect.ExtenderConnectModeDns,
				port:        port,
			})
		}
	}
	if slices.Contains(carriers, connect.ExtenderCarrierQuic) {
		optionalProbes = append(optionalProbes, &optionalCarrierProbe{
			carrier:     connect.ExtenderCarrierQuic,
			connectMode: connect.ExtenderConnectModeQuic,
			port:        udpPort,
		})
	}
	// all at once, each under the passed path's budget (connect#230): none
	// waits on another, so a carrier with nothing bound costs only itself, and
	// a live one that is slow has all of what is left. Each probe ends with
	// that budget at the latest, so this waits no longer than the budget.
	var optionalProbesDone sync.WaitGroup
	for _, optional := range optionalProbes {
		optionalProbesDone.Add(1)
		go func() {
			defer optionalProbesDone.Done()
			optional.run = timeStep(passedBudget, passedBudget.ctx, optional.carrier, optional.port, func(ctx context.Context) error {
				return probeOptionalExtenderCarrier(ctx, probes, optional.connectMode, optional.port)
			})
		}()
	}
	optionalProbesDone.Wait()

	// the dns ports that answered, which is what the address row and the
	// record list (L2), in the order offered. Empty unless the dns carrier
	// was offered and answered.
	activeDnsPorts := []int{}
	for _, optional := range optionalProbes {
		steps = append(steps, optional.run.step)
		if optional.run.err == nil {
			answered[optional.carrier] = true
			if optional.carrier == connect.ExtenderCarrierDns {
				activeDnsPorts = append(activeDnsPorts, optional.port)
			}
		} else {
			refusals[optional.carrier] = append(
				refusals[optional.carrier],
				optional.run.carrierRefusal(optional.carrier, optional.port),
			)
		}
	}

	// in the order offered, which is the order the address row and the
	// record list them in. A carrier this does not probe is never listed.
	outcome := &extenderActivationOutcome{
		carriers:        []string{},
		dnsPorts:        activeDnsPorts,
		refusedCarriers: []*ExtenderCarrierRefusal{},
		steps:           steps,
	}
	for _, carrier := range carriers {
		if answered[carrier] {
			outcome.carriers = append(outcome.carriers, carrier)
		}
		outcome.refusedCarriers = append(outcome.refusedCarriers, refusals[carrier]...)
	}
	return outcome
}

// Asks the operator's cap on extenders accepted on webrtc without tcp
// (extenderActivationProbes.webRtcOnlyRoom) under the webrtc test's budget,
// just before the test would run. A count that panics, a database read that
// failed, is an error, which refuses the webrtc test untried rather than
// probing past a cap it could not read. No question asks nothing.
func askExtenderWebRtcOnlyRoom(ctx context.Context, probes *extenderActivationProbes) (err error) {
	if probes.webRtcOnlyRoom == nil {
		return nil
	}
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[extender]activation webrtc cap could not be read: %s\n", server.ErrorJson(r, debug.Stack()))
			err = fmt.Errorf("the operator could not count its extenders on webrtc without tcp: %v", r)
		}
	}()
	return probes.webRtcOnlyRoom(ctx)
}

// The webrtc test of one activation (probeExtenderActivation). A probe that
// panics fails that path alone, as an optional carrier's does
// (probeOptionalExtenderCarrier). No probe is a path with no signaling.
func probeRequiredWebRtc(ctx context.Context, probes *extenderActivationProbes) (forwardClientAddress string, err error) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[extender]activation probe webrtc panicked: %s\n", server.ErrorJson(r, debug.Stack()))
			err = fmt.Errorf("the probe failed: %v", r)
		}
	}()
	if probes.webRtc == nil {
		return "", ErrExtenderWebRtcSignalingUnavailable
	}
	return probes.webRtc(ctx)
}

// One optional carrier's probe, which runs in a goroutine of its own
// (probeExtenderActivation). A probe that panics fails that carrier alone: the
// panic is logged and becomes the probe's error, rather than ending the
// process from a goroutine no caller recovers.
func probeOptionalExtenderCarrier(
	ctx context.Context,
	probes *extenderActivationProbes,
	connectMode connect.ExtenderConnectMode,
	port int,
) (err error) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[extender]activation probe %s:%d panicked: %s\n", connectMode, port, server.ErrorJson(r, debug.Stack()))
			err = fmt.Errorf("the probe failed: %v", r)
		}
	}()
	return probes.carrier(ctx, connectMode, port)
}

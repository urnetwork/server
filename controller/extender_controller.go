package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	mathrand "math/rand/v2"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/protocol"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
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
	// on one carrier. The required test runs first (C2a), so an optional
	// carrier that spends what is left of it only loses itself.
	ExtenderActivateProbeTimeout = 10 * time.Second

	// The budget of one dns carrier port (L2). The dns carrier is probed once
	// per listed port, and a udp port with nothing bound gives no answer to
	// fail fast on, so a dead port under the shared budget alone would spend
	// it all and fail every port and carrier probed after it. Two dead ports
	// still leave up to four seconds of the activation budget for quic, which
	// is probed after them.
	ExtenderActivateDnsPortProbeTimeout = 3 * time.Second

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
	// how long the process reuses an account's count of proven hosts. It is
	// read only once the account's spends reach the floor, and then for each
	// of a fleet's posts. Non-positive counts every time.
	ProvenHostCountTimeout time.Duration

	// activations the api process probes at once (connect#230). Each holds a
	// handler for up to the 10 s probe budget, so a fleet restarting at once
	// could otherwise hold thousands. One over it is refused as busy before
	// it spends any budget, with a `retry_after_seconds`. Non-positive refuses
	// every activation as busy.
	MaxConcurrentProbes int
	// the wait a busy refusal asks for, drawn uniformly between the two so
	// the activations refused together do not all come back together
	BusyRetryAfterMin time.Duration
	BusyRetryAfterMax time.Duration

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
		MaxConcurrentProbes:               64,
		// the activator holds a retry at least a minute whatever it is told
		// (connect/net_extender_activate.go), and three minutes spreads a
		// fleet's refused posts over three rounds of the guard
		BusyRetryAfterMin: 1 * time.Minute,
		BusyRetryAfterMax: 3 * time.Minute,
		metrics:           defaultExtenderActivationMetrics,
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
	TcpPort      int    `json:"tcp_port"`
	UdpPort      int    `json:"udp_port"`
	DnsPort      int    `json:"dns_port"`
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
	// error and its timing (C2a). They are left out of the stored address and
	// the signed record, so the extender is published on the carriers that
	// answered and no others. Empty on a refusal, which probes no optional
	// carrier.
	RefusedCarriers []*ExtenderCarrierRefusal `json:"refused_carriers,omitempty"`
	// every probe step the activation ran, in the order run -- the tcp
	// challenge, the forward through it, each dns port, then quic -- with how
	// long it ran, how much of the probe budget was left when it started, and
	// how it ended (C2). A refusal's last step is the one that refused it.
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
	// time it had: "no answer within 3 s" for a dns port (L2), or within what
	// was left of the probe budget.
	Error string `json:"error"`
	// how long the probe ran
	ElapsedMs int64 `json:"elapsed_ms"`
	// how much of the activation's probe budget was left when the probe
	// started. A dns port is also bounded by its own 3 s.
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
	// started. A dns port is also bounded by its own 3 s.
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
// ascending, because the record names one extender's ports and not one
// address's, and a client dials the lowest first. DnsPort stays the extender's
// configured port, which is what a reader that predates the list dials.
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
	countryCode := extender.RecordCountryCode()
	directoryTier := extender.DirectoryTier
	if extender.CanaryChannel != "" {
		// Older tier-aware readers must also suppress open redistribution.
		directoryTier = connect.ExtenderDirectoryTierGated
	}
	record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:   extender.PublicKey,
		Addresses:   recordAddresses,
		TcpPort:     uint32(extender.TcpPort),
		UdpPort:     uint32(extender.UdpPort),
		DnsPort:     uint32(extender.DnsPort),
		DnsPorts:    dnsPorts,
		DnsTld:      extender.DnsTld,
		CountryCode: countryCode,
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
	})
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

	stateLock sync.Mutex
	// the proven host counts this process read, each reused until its expire
	// time (ProvenHostCountTimeout)
	provenHostCounts map[extenderProvenHostCountKey]*extenderProvenHostCount
}

// An account's proven hosts of one family: the network's, since an extender
// row belongs to a network.
type extenderProvenHostCountKey struct {
	networkId server.Id
	ipVersion int
}

type extenderProvenHostCount struct {
	provenHostCount int
	expireTime      time.Time
}

func newExtenderActivateService(settings *ExtenderActivateSettings) *extenderActivateService {
	return &extenderActivateService{
		settings:         settings,
		probeSlots:       make(chan struct{}, max(0, settings.MaxConcurrentProbes)),
		provenHostCounts: map[extenderProvenHostCountKey]*extenderProvenHostCount{},
	}
}

// The proven hosts of one family of a network (connect#230), read in the
// caller's transaction, which is the budget spend's, or the count this process
// read within ProvenHostCountTimeout. The spend asks only once the account has
// reached its floor, so most activations never read it, and a fleet's posts
// past the floor read it once a minute per process rather than once each.
func (self *extenderActivateService) provenHostCountInTx(
	ctx context.Context,
	tx server.PgTx,
	networkId server.Id,
	ipVersion int,
) int {
	key := extenderProvenHostCountKey{
		networkId: networkId,
		ipVersion: ipVersion,
	}
	reuse := 0 < self.settings.ProvenHostCountTimeout
	if reuse {
		provenHostCount, ok := func() (int, bool) {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			if count, ok := self.provenHostCounts[key]; ok && time.Now().Before(count.expireTime) {
				return count.provenHostCount, true
			}
			return 0, false
		}()
		if ok {
			return provenHostCount
		}
	}

	now := server.NowUtc()
	provenHostCount := self.settings.provenHostCount(
		model.GetNetworkExtenderProvenHostCandidatesInTx(
			ctx,
			tx,
			networkId,
			ipVersion,
			now.Add(-self.settings.ProvenHostMaxAge),
		),
		ipVersion,
		now,
	)
	if reuse {
		func() {
			self.stateLock.Lock()
			defer self.stateLock.Unlock()
			// the counts that ran out go, so the map holds only the networks
			// whose spends reached their floor within the timeout
			expireNow := time.Now()
			for countKey, count := range self.provenHostCounts {
				if !expireNow.Before(count.expireTime) {
					delete(self.provenHostCounts, countKey)
				}
			}
			self.provenHostCounts[key] = &extenderProvenHostCount{
				provenHostCount: provenHostCount,
				expireTime:      expireNow.Add(self.settings.ProvenHostCountTimeout),
			}
		}()
	}
	return provenHostCount
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
			Activated:  false,
			Error:      message,
			ProbeSteps: probeSteps,
		}, nil
	}
	// a refusal that judged nothing about the caller -- a busy api, a spent
	// budget -- with the wait until there is room (connect#230)
	refuseWithRetryAfter := func(message string, retryAfter time.Duration) (*ExtenderActivateResult, error) {
		result, err := refuse(message)
		result.RetryAfterSeconds = extenderActivateRetryAfterSeconds(retryAfter)
		return result, err
	}

	publicKey, err := connect.ParseExtenderPublicKeyHex(args.PublicKeyHex)
	if err != nil {
		return refuse(fmt.Sprintf("the extender public key is not readable: %s", err))
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
	// the forward probe runs over tcp, so an extender that does not serve tcp
	// cannot be proved to forward at all
	if !slices.Contains(carriers, connect.ExtenderCarrierTcp) {
		return refuse("an activation must offer the tcp carrier")
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
	// the activation was posted for, the operator's configuration -- and
	// before the first dial. A malformed request, a post that crossed families
	// and an operator that cannot probe spend nothing, while every activation
	// that dials the caller is counted before it dials, whatever the probe
	// then finds. The calling client's budget, its address's and its
	// account's are spent together or not at all, so a client refused by its
	// account's budget keeps its own, and all are the budgets of the family
	// the probe dials. A refusal says when the budget that refused has room
	// again, which the activator waits out (`retry_after_seconds`).
	stage = extenderActivationStageBudget
	clientRateLimitAction, addressRateLimitAction, accountRateLimitAction := extenderActivateRateLimitActions(ipVersion)
	networkId := clientSession.ByJwt.NetworkId
	if err := model.CheckAndRecordAccountActionBudgets(
		clientSession.Ctx,
		model.AccountActionBudget{
			Id:     *clientSession.ByJwt.ClientId,
			Action: clientRateLimitAction,
			Limit:  settings.ClientRateLimit,
			Window: settings.RateLimitWindow,
		},
		model.AccountActionBudget{
			Id:     settings.addressBudgetId(clientIp),
			Action: addressRateLimitAction,
			Limit:  settings.AddressRateLimit,
			Window: settings.RateLimitWindow,
		},
		model.AccountActionBudget{
			Id:     clientSession.ByJwt.UserId,
			Action: accountRateLimitAction,
			Limit:  settings.AccountRateLimit,
			Window: settings.RateLimitWindow,
			// past the floor the account's limit is what its proven hosts
			// raised it to (connect#230)
			ExtendLimitInTx: func(ctx context.Context, tx server.PgTx) int {
				return settings.accountRateLimit(self.provenHostCountInTx(ctx, tx, networkId, ipVersion))
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
			stageOutcome = extenderActivationBudgetOutcome(budgetRefusal.Action)
			return refuseWithRetryAfter(err.Error(), budgetRefusal.RetryAfter)
		}
		stageOutcome = extenderActivationOutcomeAccount
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
			_, err := connect.ProbeExtenderCarrier(
				ctx,
				connectSettings,
				clientIp,
				connectMode,
				port,
				dnsTld,
				serverName,
				publicKey,
				apiHost,
				ExtenderProbeDestinationPort,
			)
			return err
		},
		forward: func(ctx context.Context) (string, error) {
			return connect.ProbeExtenderForward(
				ctx,
				connectSettings,
				clientIp,
				tcpPort,
				serverName,
				publicKey,
				config.ApiUrl,
				extenderForwardProbeTlsConfig(),
			)
		},
	}
	if self.testProbes != nil {
		probes = self.testProbes(clientIp, ipVersion)
	}
	outcome := probeExtenderActivation(
		clientSession.Ctx,
		newExtenderActivationClock(),
		probes,
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
	// so the record already carries no measurement id (lag.dev/PLAN.md C9)
	if args.MeasurementDisabled {
		optOutExtenderMeasurement(clientSession.Ctx, publicKey, server.NowUtc())
	}

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
}

// The clock the probes of one activation run on (C2): the time each step is
// read at, and the deadlines that bound the shared probe budget and each dns
// port's own. Production is the real clock; a test drives a fake one, so a
// step that takes 9.5 s, or one that waits out its budget, does so at once
// and to the instant.
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
	// every step probed, in the order probed. A refusal's last step is the
	// one that refused it.
	steps []*ExtenderProbeStep
}

// One probe step as it ran (C2): the step the answer lists, and the times
// and the error its messages are made from.
type extenderProbeStepRun struct {
	step *ExtenderProbeStep
	// how long it ran
	elapsed time.Duration
	// how long it was allowed to run: what was left of the shared budget, or
	// a dns port's own budget when that ended first (L2)
	allowance time.Duration
	// the shared budget, rather than a dns port's own, bounded it
	sharedBudget bool
	err          error
}

// How much time a step was given, as its timeout says it: "within 3 s" for a
// dns port that ran out of its own budget, "within the 10 s probe budget" for
// a step that had all of the shared budget, and "within the 0.5 s left of the
// 10 s probe budget" for one that ran out of what was left of it.
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
// The required test runs first: the challenge on the tcp carrier, the
// verified forward through it, and the family the forward arrived on (A7). If
// any of those fails, the activation is refused with that failure named, and
// nothing else is probed. Only then is each optional carrier probed, when it
// was offered, under what is left of the shared budget. One that does not
// answer is left out and listed; it never refuses an activation the required
// test passed (connect#230). The order is part of the rule: a udp carrier
// gives no answer to fail fast on, so an optional carrier probed before the
// forward could spend the budget the forward needs and gate the activation
// after all.
//
// The dns carrier goes before quic. Each dns port is bounded on its own (L2),
// so the dns carrier always leaves quic the rest of the budget, while quic is
// bounded only by the budget: a quic carrier that never answers, probed
// first, would leave the dns carrier no time to answer in.
//
// Every step runs on the clock: the shared budget ends
// ExtenderActivateProbeTimeout after the judging starts, and a dns port ends
// at its own ExtenderActivateDnsPortProbeTimeout or at the end of the shared
// budget, whichever comes first. Each step is timed, and the outcome lists
// every step with how long it ran and what was left of the shared budget when
// it started. A step whose budget ran out before it answered timed out, and
// its refusal says how much time it had; a late answer and no answer were the
// same "context deadline exceeded" before (connect#230).
func probeExtenderActivation(
	ctx context.Context,
	clock *extenderActivationClock,
	probes *extenderActivationProbes,
	carriers []string,
	tcpPort int,
	udpPort int,
	dnsPorts []int,
	ipVersion int,
) *extenderActivationOutcome {
	probeDeadline := clock.now().Add(ExtenderActivateProbeTimeout)
	probeCtx, probeCancel := clock.withDeadline(ctx, probeDeadline)
	defer probeCancel()

	steps := []*ExtenderProbeStep{}
	// runs one step under stepCtx, which ends at the shared budget or sooner,
	// and records it. A step that failed once its context had reached its
	// deadline timed out; any other failure is the step's own.
	runStep := func(
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
				BudgetLeftMs: max(0, probeDeadline.Sub(stepStart)).Milliseconds(),
				Outcome:      extenderProbeStepOutcomeOk,
			},
			elapsed:      elapsed,
			allowance:    max(0, stepDeadline.Sub(stepStart)),
			sharedBudget: !stepDeadline.Before(probeDeadline),
			err:          err,
		}
		if err != nil {
			run.step.Outcome = extenderProbeStepOutcomeError
			if errors.Is(stepCtx.Err(), context.DeadlineExceeded) {
				run.step.Outcome = extenderProbeStepOutcomeTimeout
			}
		}
		steps = append(steps, run.step)
		return run
	}
	refuse := func(run *extenderProbeStepRun, refusal string, refusalOutcome string) *extenderActivationOutcome {
		return &extenderActivationOutcome{
			refusal:        refusal,
			refusalStep:    run.step.Name,
			refusalOutcome: refusalOutcome,
			steps:          steps,
		}
	}

	tcpRun := runStep(probeCtx, connect.ExtenderCarrierTcp, tcpPort, func(ctx context.Context) error {
		return probes.carrier(ctx, connect.ExtenderConnectModeTcpTls, tcpPort)
	})
	if tcpRun.err != nil {
		return refuse(tcpRun, tcpRun.refusal(connect.ExtenderCarrierTcp+" carrier"), tcpRun.step.Outcome)
	}
	var forwardClientAddress string
	// the forward runs over the tcp carrier, so it is the tcp port it dials
	forwardRun := runStep(probeCtx, extenderProbeStepForward, tcpPort, func(ctx context.Context) error {
		var err error
		forwardClientAddress, err = probes.forward(ctx)
		return err
	})
	if forwardRun.err != nil {
		return refuse(forwardRun, forwardRun.refusal(extenderProbeStepForward), forwardRun.step.Outcome)
	}
	// an extender must egress on the family it was reached on (A7); a forward
	// that arrives on the other family would publish an address that does not
	// carry the traffic the record promises
	if forwardIpVersion := server.ClientAddressIpVersion(forwardClientAddress); forwardIpVersion != ipVersion {
		// the forward answered, but not as the required test needs
		forwardRun.step.Outcome = extenderProbeStepOutcomeError
		return refuse(
			forwardRun,
			fmt.Sprintf(
				"the forward arrived on ipv%d, not ipv%d, after %s",
				forwardIpVersion,
				ipVersion,
				extenderProbeSecondsText(forwardRun.elapsed),
			),
			extenderProbeRefusalOtherFamily,
		)
	}

	answered := map[string]bool{
		connect.ExtenderCarrierTcp: true,
	}
	refusals := map[string][]*ExtenderCarrierRefusal{}

	// the dns ports that answered, which is what the address row and the
	// record list (L2). Empty unless the dns carrier was offered and answered.
	activeDnsPorts := []int{}
	if slices.Contains(carriers, connect.ExtenderCarrierDns) {
		// one dns port failing is not the carrier failing: an extender that
		// offers 53 and 4053 on a path that blocks 53 is still reachable on
		// 4053, and the record must say which
		for _, port := range dnsPorts {
			portDeadline := clock.now().Add(ExtenderActivateDnsPortProbeTimeout)
			if probeDeadline.Before(portDeadline) {
				portDeadline = probeDeadline
			}
			portCtx, portCancel := clock.withDeadline(probeCtx, portDeadline)
			portRun := runStep(portCtx, connect.ExtenderCarrierDns, port, func(ctx context.Context) error {
				return probes.carrier(ctx, connect.ExtenderConnectModeDns, port)
			})
			portCancel()
			if portRun.err == nil {
				activeDnsPorts = append(activeDnsPorts, port)
			} else {
				refusals[connect.ExtenderCarrierDns] = append(
					refusals[connect.ExtenderCarrierDns],
					portRun.carrierRefusal(connect.ExtenderCarrierDns, port),
				)
			}
		}
		answered[connect.ExtenderCarrierDns] = 0 < len(activeDnsPorts)
	}
	if slices.Contains(carriers, connect.ExtenderCarrierQuic) {
		quicRun := runStep(probeCtx, connect.ExtenderCarrierQuic, udpPort, func(ctx context.Context) error {
			return probes.carrier(ctx, connect.ExtenderConnectModeQuic, udpPort)
		})
		if quicRun.err == nil {
			answered[connect.ExtenderCarrierQuic] = true
		} else {
			refusals[connect.ExtenderCarrierQuic] = append(
				refusals[connect.ExtenderCarrierQuic],
				quicRun.carrierRefusal(connect.ExtenderCarrierQuic, udpPort),
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

package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"net/netip"
	"slices"
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
// own address over every carrier the caller claims, requires a signature over
// a fresh challenge on each, and then forwards a real `GET /hello` through the
// tcp carrier to its own api. Nothing is stored until all of that passes, so
// an extender behind NAT, on the wrong family, or without the private key it
// claims simply never appears -- no reachability heuristic and no trust in
// what the caller said about itself.
//
// The probe runs synchronously inside the request. That is what lets the
// answer carry the signed record: the caller gets its identity in the
// directory and a bootstrap sample of other extenders in one round trip,
// which is the only bootstrap an app has before it has any peer (D6).

const (
	// How long a signed record stays valid (B2). Long enough that a missed
	// drip rotation does not expire an extender, short enough that an operator
	// that stops publishing drains out within a fortnight.
	ExtenderRecordExpireTimeout = 14 * 24 * time.Hour

	// The whole probe budget of one activation (C2). Every carrier and the
	// forward share it, so a caller cannot hold a request open by being slow
	// on one carrier.
	ExtenderActivateProbeTimeout = 10 * time.Second

	// The budget of one dns carrier port (L2). The dns carrier is probed once
	// per listed port, and a udp port with nothing bound gives no answer to
	// fail fast on, so a dead port under the shared budget alone would spend
	// it all and fail every port and the forward behind it. Two dead ports
	// still leave four seconds of the activation budget for the rest.
	ExtenderActivateDnsPortProbeTimeout = 3 * time.Second

	// Activation attempts per client per hour (C2). An activation costs the
	// operator several dials back to the caller, so the budget is what stops
	// one client from using the api as a probe engine.
	ExtenderActivateRateLimitAction = "extender_activate"
	ExtenderActivateRateLimit       = 6
	ExtenderActivateRateLimitWindow = time.Hour

	// Records of other extenders returned with an activation (C2).
	ExtenderBootstrapCount = 8

	// The port a probe asks the extender to forward to. The api answers on
	// 443 and an extender's whitelist is by host, so the port is fixed rather
	// than taken from the api url, whose port is only ever non-standard in a
	// test.
	ExtenderProbeDestinationPort = 443
)

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
}

type ExtenderActivateResult struct {
	Activated bool   `json:"activated"`
	Ip        string `json:"ip,omitempty"`
	IpVersion int    `json:"ip_version,omitempty"`
	// the carriers that passed their probe, which is what the stored address
	// and the signed record list
	Carriers []string `json:"carriers,omitempty"`
	// the dns ports that passed their probe, ascending, which is what the
	// stored address and the signed record list (L2). Empty when the dns
	// carrier was not offered.
	DnsPorts []int `json:"dns_ports,omitempty"`
	// why the activation was refused, empty on success. A refusal is a normal
	// answer, not a request error: the caller is told which carrier failed so
	// it can fix its own binding.
	Error      string     `json:"error,omitempty"`
	ExpireTime *time.Time `json:"expire_time,omitempty"`
	// the operator patterns this extender may forward to (A5)
	AllowedHosts []string `json:"allowed_hosts,omitempty"`
	// base64 of a serialized protocol.ExtenderRecord
	Record string `json:"record,omitempty"`
	// base64 of serialized protocol.ExtenderRecord messages of other active
	// extenders, the directory an app starts from (D6)
	Bootstrap []string `json:"bootstrap,omitempty"`
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
	record, err := connect.SignExtenderRecord(rootPrivateKey, &protocol.ExtenderRecordBody{
		PublicKey:   extender.PublicKey,
		Addresses:   recordAddresses,
		TcpPort:     uint32(extender.TcpPort),
		UdpPort:     uint32(extender.UdpPort),
		DnsPort:     uint32(extender.DnsPort),
		DnsPorts:    dnsPorts,
		DnsTld:      extender.DnsTld,
		CountryCode: extender.CountryCode,
		// the same mapping the geo dns sets use, so "close" means one thing
		// on both paths (connect/DESIGNNOTES4.md §2)
		ContinentCode: model.ContinentCodeForCountry(extender.CountryCode),
		IssueTimeMs:   uint64(issueTime.UnixMilli()),
		ExpireTimeMs:  uint64(issueTime.Add(ExtenderRecordExpireTimeout).UnixMilli()),
		NetworkHost:   config.NetworkHost,
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
func ExtenderActivate(
	args *ExtenderActivateArgs,
	clientSession *session.ClientSession,
) (*ExtenderActivateResult, error) {
	refuse := func(message string) (*ExtenderActivateResult, error) {
		return &ExtenderActivateResult{
			Activated: false,
			Error:     message,
		}, nil
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
	ipVersion := server.ClientAddressIpVersion(clientSession.ClientAddress)
	if ipVersion != 4 && ipVersion != 6 {
		return refuse("the caller address has no known family")
	}

	config, err := EnvExtenderConfig()
	if err != nil {
		return refuse(err.Error())
	}
	rootPrivateKey, err := config.RootPrivateKey()
	if err != nil {
		return refuse(err.Error())
	}
	apiHost, err := config.ApiHost()
	if err != nil {
		return refuse(err.Error())
	}
	serverName, err := config.ProbeServerName()
	if err != nil {
		return refuse(err.Error())
	}

	if err := model.CheckAndRecordAccountActionRateLimit(
		clientSession.Ctx,
		clientSession.ByJwt.UserId,
		ExtenderActivateRateLimitAction,
		ExtenderActivateRateLimit,
		ExtenderActivateRateLimitWindow,
	); err != nil {
		return refuse(err.Error())
	}

	probeCtx, probeCancel := context.WithTimeout(clientSession.Ctx, ExtenderActivateProbeTimeout)
	defer probeCancel()

	connectSettings := ExtenderProbeConnectSettings()
	probeCarrier := func(
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
	}

	// the dns ports that answered, which is what the address row and the
	// record list (L2). Empty unless the dns carrier was offered.
	activeDnsPorts := []int{}
	for _, carrier := range carriers {
		connectMode, _ := extenderCarrierConnectMode(carrier)
		if carrier == connect.ExtenderCarrierDns {
			// one dns port failing is not the carrier failing: an extender
			// that offers 53 and 4053 on a path that blocks 53 is still
			// reachable on 4053, and the record must say which. Only a carrier
			// with no port left is refused.
			var firstErr error
			for _, port := range dnsPorts {
				portCtx, portCancel := context.WithTimeout(
					probeCtx,
					ExtenderActivateDnsPortProbeTimeout,
				)
				err := probeCarrier(portCtx, connectMode, port)
				portCancel()
				if err == nil {
					activeDnsPorts = append(activeDnsPorts, port)
				} else if firstErr == nil {
					firstErr = err
				}
			}
			if len(activeDnsPorts) == 0 {
				return refuse(fmt.Sprintf("the %s carrier did not answer: %s", carrier, firstErr))
			}
			continue
		}
		port := tcpPort
		if carrier == connect.ExtenderCarrierQuic {
			port = udpPort
		}
		if err := probeCarrier(probeCtx, connectMode, port); err != nil {
			return refuse(fmt.Sprintf("the %s carrier did not answer: %s", carrier, err))
		}
	}

	forwardClientAddress, err := connect.ProbeExtenderForward(
		probeCtx,
		connectSettings,
		clientIp,
		tcpPort,
		serverName,
		publicKey,
		config.ApiUrl,
		extenderForwardProbeTlsConfig(),
	)
	if err != nil {
		return refuse(fmt.Sprintf("the forward did not answer: %s", err))
	}
	// an extender must egress on the family it was reached on (A7); a forward
	// that arrives on the other family would publish an address that does not
	// carry the traffic the record promises
	if forwardIpVersion := server.ClientAddressIpVersion(forwardClientAddress); forwardIpVersion != ipVersion {
		return refuse(fmt.Sprintf(
			"the forward arrived on ipv%d, not ipv%d",
			forwardIpVersion,
			ipVersion,
		))
	}

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
	if location, _, err := GetLocationForIp(clientSession.Ctx, clientIpStr); err == nil {
		countryCode = location.CountryCode
		// CreateLocation raises on a country it cannot name, and it is the
		// only thing here that writes: contain it so a location the table
		// refuses can never turn a successful probe into a refused activation
		if r := server.HandleError(func() {
			model.CreateLocation(clientSession.Ctx, location)
		}); r == nil {
			activationLocation = location
		} else if glog.V(1) {
			glog.Infof("[extender]no location row for the activating address: %s\n", r)
		}
	} else if glog.V(1) {
		glog.Infof("[extender]no location for the activating address: %s\n", err)
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
			Carriers:    carriers,
			DnsPorts:    activeDnsPorts,
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

	return &ExtenderActivateResult{
		Activated:    true,
		Ip:           clientIp.String(),
		IpVersion:    ipVersion,
		Carriers:     carriers,
		DnsPorts:     activeDnsPorts,
		ExpireTime:   &expireTime,
		AllowedHosts: config.AllowedHosts(),
		Record:       recordBase64,
		Bootstrap:    bootstrap,
	}, nil
}

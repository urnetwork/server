// Public UDP enrollment is explicit: one logical service/alias/carrier row
// expands into every requested address family. No DNS selection or active
// service guess can silently remove a configured path.
package monitor

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
)

const (
	publicUdpAttemptTimeout = 5 * time.Second
	publicUdpMaxTargets     = 32
	publicUdpConcurrency    = 4
	publicUdpMaxPackets     = 2048
	publicUdpMaxBytes       = 1024 * 1024
)

// An independently declared denominator prevents a partial inventory join
// from becoming a healthy empty or reduced matrix. Disabled means unarmed,
// never verified healthy. Values other than Name/Host stay private to probes.
type PublicUdpSettings struct {
	Enabled         bool                      `yaml:"enabled"`
	ExpectedTargets int                       `yaml:"expected_targets"`
	Targets         []PublicUdpTargetSettings `yaml:"targets"`
	Invalid         bool                      `yaml:"-"`
}

// Each logical path names a nonsecret stable label and an existing enabled
// inventory host. Families are explicit even when an address is missing.
// Alt's explicitly published direct UDP ports are not Connect-private leaks.
type PublicUdpTargetSettings struct {
	Name        string   `yaml:"name"`
	Host        string   `yaml:"host"`
	Interface   string   `yaml:"interface"`
	Service     string   `yaml:"service"`
	Alias       string   `yaml:"alias"`
	Front       string   `yaml:"front"`
	Carrier     string   `yaml:"carrier"`
	Families    []string `yaml:"families"`
	IPv4Address string   `yaml:"ipv4_address"`
	IPv6Address string   `yaml:"ipv6_address"`
	Port        int      `yaml:"port"`
	ServerName  string   `yaml:"server_name"`
	DnsTld      string   `yaml:"dns_tld"`
}

// A fresh attempt's exact pin and protocol authority. AttemptId is private
// correlation data, never an alert identity or evidence fingerprint.
type PublicUdpRequest struct {
	AttemptId  string
	Host       string
	Target     string
	Family     string
	Service    string
	Front      string
	Carrier    string
	Address    string
	Port       int
	ServerName string
	DnsTld     string
}

// Only fixed failure domains cross the adapter. They describe the bounded
// observation, not an inferred firewall, SNAT or application root cause.
type PublicUdpFailure string

const (
	PublicUdpFailureNone          PublicUdpFailure = ""
	PublicUdpFailureSource        PublicUdpFailure = "source-unavailable"
	PublicUdpFailureSocket        PublicUdpFailure = "socket-unavailable"
	PublicUdpFailureRoute         PublicUdpFailure = "observer-route-unavailable"
	PublicUdpFailureTimeout       PublicUdpFailure = "handshake-timeout"
	PublicUdpFailureHandshake     PublicUdpFailure = "handshake-failed"
	PublicUdpFailureTls           PublicUdpFailure = "tls-authentication-unverified"
	PublicUdpFailureBudget        PublicUdpFailure = "wire-budget-exhausted"
	PublicUdpFailureConfiguration PublicUdpFailure = "configuration-invalid"
)

// Raw addresses remain private. Verified health requires positive bounded
// wire evidence and a completed authenticated handshake from the exact pin.
// Unexpected packets are discarded before translation/QUIC; their presence
// alone cannot attribute a response, let alone prove an SNAT defect.
type PublicUdpObservation struct {
	AttemptId          string
	RequestedTuple     string
	PeerTuple          string
	FreshSocket        bool
	HandshakeComplete  bool
	TlsVerified        bool
	NegotiatedProtocol string
	SentPackets        int
	SentBytes          int
	ReceivedPackets    int
	ReceivedBytes      int
	UnexpectedPackets  int
	Failure            PublicUdpFailure
}

// Synthetic/alternate sources must implement this explicitly. There is no
// native network fallback when SignalSettings.Source is supplied.
type PublicUdpSignalSource interface {
	PublicUdp(context.Context, PublicUdpRequest) (PublicUdpObservation, error)
}

// Optional internal seam leaves unrelated transports unchanged.
type publicUdpRunner interface {
	publicUdp(context.Context, PublicUdpRequest) (PublicUdpObservation, error)
}

// Clone all nested slices so startup generation and observation scope stay
// independent of a caller's later mutation.
func clonePublicUdpSettings(settings PublicUdpSettings) PublicUdpSettings {
	settings.Targets = append([]PublicUdpTargetSettings(nil), settings.Targets...)
	for index := range settings.Targets {
		settings.Targets[index].Families = append([]string(nil), settings.Targets[index].Families...)
	}
	return settings
}

// Validate the complete declared matrix before any socket is opened. The
// returned reason is a fixed domain, never an input value.
func publicUdpRequests(settings PublicUdpSettings, hosts []*host) ([]PublicUdpRequest, string) {
	if settings.Invalid || settings.ExpectedTargets < 1 || settings.ExpectedTargets > publicUdpMaxTargets || len(settings.Targets) != settings.ExpectedTargets {
		return nil, "target-denominator"
	}
	hostCounts := map[string]int{}
	for _, configured := range hosts {
		hostCounts[configured.name]++
	}
	names := map[string]bool{}
	paths := map[string]bool{}
	requests := []PublicUdpRequest{}
	for _, target := range settings.Targets {
		if !validDNSLabel(target.Name) || names[target.Name] || hostCounts[target.Host] != 1 {
			return nil, "target-identity"
		}
		names[target.Name] = true
		if target.Interface == "" || target.Alias == "" || strings.TrimSpace(target.Interface) != target.Interface || strings.TrimSpace(target.Alias) != target.Alias {
			return nil, "path-identity"
		}
		if target.Service != "connect" && target.Service != "alt" ||
			target.Front != "connect" && target.Front != "api" ||
			target.Service == "connect" && target.Front != "connect" ||
			!slices.Contains([]string{"quic", "dns", "dns-pump"}, target.Carrier) ||
			target.Port < 1 || target.Port > 65535 || !publicUdpServerName(target.ServerName) {
			return nil, "protocol-authority"
		}
		if target.Carrier != "quic" && (!strings.HasSuffix(target.DnsTld, ".") || !publicUdpServerName(strings.TrimSuffix(target.DnsTld, "."))) ||
			target.Carrier == "quic" && target.DnsTld != "" {
			return nil, "codec-authority"
		}
		if len(target.Families) < 1 || len(target.Families) > 2 {
			return nil, "family-denominator"
		}
		families := map[string]bool{}
		for _, family := range target.Families {
			if families[family] || family != "ipv4" && family != "ipv6" {
				return nil, "family-denominator"
			}
			families[family] = true
			addressText := target.IPv4Address
			if family == "ipv6" {
				addressText = target.IPv6Address
			}
			address, err := netip.ParseAddr(addressText)
			if err != nil || address.Zone() != "" || address.Is4In6() || !address.IsGlobalUnicast() || address.IsPrivate() ||
				(family == "ipv4") != address.Is4() {
				return nil, "family-address"
			}
			key := strings.Join([]string{target.Host, target.Interface, target.Service, target.Alias, target.Front, target.Carrier, family}, "/")
			if paths[key] {
				return nil, "duplicate-path"
			}
			paths[key] = true
			requests = append(requests, PublicUdpRequest{
				Host: target.Host, Target: target.Name, Family: family,
				Service: target.Service, Front: target.Front, Carrier: target.Carrier,
				Address: address.String(), Port: target.Port, ServerName: target.ServerName, DnsTld: target.DnsTld,
			})
		}
		if target.IPv4Address != "" && !families["ipv4"] || target.IPv6Address != "" && !families["ipv6"] {
			return nil, "unprobed-family"
		}
	}
	return requests, ""
}

// SNI is a literal DNS name, never an IP, URL, wildcard or fallback lookup.
func publicUdpServerName(name string) bool {
	if name == "" || len(name) > 253 || name != strings.ToLower(name) || strings.HasSuffix(name, ".") || !strings.Contains(name, ".") {
		return false
	}
	if _, err := netip.ParseAddr(name); err == nil {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if !validDNSLabel(label) {
			return false
		}
	}
	return true
}

// The request has already been validated; no name lookup is permitted here.
func publicUdpTuple(request PublicUdpRequest) string {
	address, err := netip.ParseAddr(request.Address)
	if err != nil || request.Port < 1 || request.Port > 65535 {
		return ""
	}
	return netip.AddrPortFrom(address, uint16(request.Port)).String()
}

// Source capability absence is a bounded observation error, not permission
// for a synthetic test or embedding process to make an unrequested live dial.
func (self *sourceRunner) publicUdp(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
	if err := ctx.Err(); err != nil {
		return PublicUdpObservation{}, err
	}
	source, ok := self.source.(PublicUdpSignalSource)
	if !ok {
		return PublicUdpObservation{Failure: PublicUdpFailureSource}, fmt.Errorf("monitor: public UDP source capability unavailable")
	}
	return source.PublicUdp(ctx, request)
}

// Enforce both logical ownership and shared-endpoint exclusions before the
// underlying real or synthetic adapter is contacted.
func (self *hostScopeRunner) publicUdp(ctx context.Context, request PublicUdpRequest) (PublicUdpObservation, error) {
	var configured *host
	for _, candidate := range self.cfg.hosts {
		if candidate.name == request.Host {
			if configured != nil {
				return PublicUdpObservation{}, fmt.Errorf("monitor: public UDP host authority is ambiguous")
			}
			configured = candidate
		}
	}
	if configured == nil {
		return PublicUdpObservation{}, fmt.Errorf("monitor: public UDP host authority is unavailable")
	}
	if err := self.guardHost(ctx, configured); err != nil {
		return PublicUdpObservation{}, err
	}
	if err := self.guardEndpoint(ctx, request.Address); err != nil {
		return PublicUdpObservation{}, err
	}
	transport, ok := self.probeRunner.(publicUdpRunner)
	if !ok {
		return PublicUdpObservation{Failure: PublicUdpFailureSource}, fmt.Errorf("monitor: public UDP source capability unavailable")
	}
	return transport.publicUdp(ctx, request)
}

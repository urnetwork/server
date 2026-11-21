//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// DNSSEC above the world's DNS (world_test.go), for the send half of mail: the world's dns edge is authoritative under
// its own root, zone.e2e.example., signed under the KSK the world made for it, and nothing signs a path from the DNS
// root down to it, so no validator could reach a network's MX from IANA's anchors. The world signs that path itself:
//
//	parents    ".", "example." and "e2e.example.", each a zone of one P-256 key the world draws, each holding its
//	           child's DS, the last the DS of the world root's KSK, so a chain from the world's own root anchor
//	           reaches every record the dns edge signs
//	forwarder  sidecar.DnsWatchDnssecForwarder as a DNSSEC-OK recursive resolver with checking disabled answers: a
//	           question at or under the world's root, but the root's own DS, goes to the dns edge over TCP with DO
//	           set, its answer as the edge signed it; the parents' keys and DS are answered here, each RRset with its
//	           RRSIG
//	resolver   smtpedge's sidecar.SmtpMxResolver: sidecar.NewSmtpMxDnsResolver over the forwarder under the world's
//	           anchor for names under the world's root, so the edge validates a network's MX, its mail host's
//	           address and its TLSA as the dns edge serves them; and sidecartest's MxWorld for every other name, the
//	           sending address's HELO name among them, which no zone of the world serves
//
// What stands in, named in the package header: the parents, which are the internet's DNS root and the zones above
// the world's root in production, and the anchor a sending device validates under, theirs in place of IANA's.

package sidecare2e

import (
	"context"
	"crypto"
	"errors"
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/sidecar"
)

// What the world's DNSSEC parents sign with (QD-702).
type worldDnssecSettings struct {
	// The zones above the world's root, deepest last, each signing its child's DS.
	ParentZones []string
	// Each RRset's TTL, and the signatures' window on either side of now.
	Ttl               uint32
	SignatureValidity time.Duration
	// One question's bound at the dns edge, and the EDNS0 buffer it asks with.
	ExchangeTimeout    time.Duration
	UdpBufferByteCount uint16
}

func defaultWorldDnssecSettings() *worldDnssecSettings {
	return &worldDnssecSettings{
		ParentZones:        []string{".", "example.", "e2e.example."},
		Ttl:                3600,
		SignatureValidity:  24 * time.Hour,
		ExchangeTimeout:    10 * time.Second,
		UdpBufferByteCount: 4096,
	}
}

// One parent zone: its key and what it answers.
type worldDnssecZone struct {
	name   string
	key    *dns.DNSKEY
	signer crypto.Signer
	// its child's DS RRset
	childDs []dns.RR
}

// The world's DNSSEC parents and the forwarder over them and the dns edge. Immutable once made, so safe for
// concurrent use.
type worldDnssec struct {
	world    *World
	settings *worldDnssecSettings
	// the world's root, canonical with its final dot
	root string
	// by name, canonical with its final dot
	zones map[string]*worldDnssecZone
	// the DNS root's DS: what a validator anchors at
	anchor *dns.DS
}

var _ sidecar.DnsWatchDnssecForwarder = (*worldDnssec)(nil)
var _ sidecar.SmtpDohForwarder = (*worldDnssec)(nil)

// The parents over the world's root and its KSK, each zone keyed afresh.
func newWorldDnssec(world *World, settings *worldDnssecSettings) (*worldDnssec, error) {
	if settings == nil {
		settings = defaultWorldDnssecSettings()
	}
	root := dns.CanonicalName(world.HostsSettings().DnsRoot)
	self := &worldDnssec{world: world, settings: settings, root: root, zones: map[string]*worldDnssecZone{}}
	for _, name := range settings.ParentZones {
		name = dns.CanonicalName(name)
		key := &dns.DNSKEY{
			Hdr:       dns.RR_Header{Name: name, Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: settings.Ttl},
			Flags:     dns.ZONE | dns.SEP,
			Protocol:  3,
			Algorithm: dns.ECDSAP256SHA256,
		}
		private, err := key.Generate(256)
		if err != nil {
			return nil, err
		}
		signer, ok := private.(crypto.Signer)
		if !ok {
			return nil, errors.New("a zone key that does not sign")
		}
		self.zones[name] = &worldDnssecZone{name: name, key: key, signer: signer}
	}
	// each parent holds its child's DS: the next parent's, and the deepest the world root's
	children := append(append([]string{}, settings.ParentZones[1:]...), root)
	for index, parentName := range settings.ParentZones {
		parent := self.zones[dns.CanonicalName(parentName)]
		childName := dns.CanonicalName(children[index])
		var childKey *dns.DNSKEY
		if child := self.zones[childName]; child != nil {
			childKey = child.key
		} else {
			childKey = world.credentials.dnsKskRecord
		}
		if !dns.IsSubDomain(parent.name, childName) || parent.name == childName {
			return nil, fmt.Errorf("%s is no child of %s", childName, parent.name)
		}
		ds := childKey.ToDS(dns.SHA256)
		ds.Hdr = dns.RR_Header{Name: childName, Rrtype: dns.TypeDS, Class: dns.ClassINET, Ttl: settings.Ttl}
		parent.childDs = []dns.RR{ds}
	}
	rootZone := self.zones["."]
	if rootZone == nil {
		return nil, errors.New("the parents hold no DNS root")
	}
	self.anchor = rootZone.key.ToDS(dns.SHA256)
	self.anchor.Hdr = dns.RR_Header{Name: ".", Rrtype: dns.TypeDS, Class: dns.ClassINET, Ttl: settings.Ttl}
	return self, nil
}

// The anchor a validator holds: the DNS root's DS as one uncompressed wire RR.
func (self *worldDnssec) Anchors() (*sidecar.SmtpDnssecAnchors, error) {
	wire := make([]byte, dns.Len(self.anchor)+64)
	length, err := dns.PackRR(self.anchor, wire, 0, nil, false)
	if err != nil {
		return nil, err
	}
	return sidecar.NewSmtpDnssecAnchors(wire[:length])
}

// rrset and its RRSIG by zone, valid around now.
func (self *worldDnssec) signed(zone *worldDnssecZone, rrset []dns.RR) ([]dns.RR, error) {
	now := time.Now()
	signature := &dns.RRSIG{
		Hdr:        dns.RR_Header{Name: rrset[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: self.settings.Ttl},
		Algorithm:  zone.key.Algorithm,
		KeyTag:     zone.key.KeyTag(),
		SignerName: zone.name,
		Inception:  uint32(now.Add(-self.settings.SignatureValidity).Unix()),
		Expiration: uint32(now.Add(self.settings.SignatureValidity).Unix()),
	}
	if err := signature.Sign(zone.signer, rrset); err != nil {
		return nil, err
	}
	return append(append([]dns.RR{}, rrset...), signature), nil
}

// Whether name is the world's to answer at the dns edge: at or under its root, but the root's own DS, which its
// parent holds.
func (self *worldDnssec) atEdge(qType uint16, name string) bool {
	if !dns.IsSubDomain(self.root, name) {
		return false
	}
	return !(name == self.root && qType == dns.TypeDS)
}

// See sidecar.DnsWatchDnssecForwarder: the answer to qType at domain, given without its final dot and the root as
// ".", as packed DNS: the dns edge's for the world's names, the parents' keys and DS here, and NXDOMAIN for anything
// else above the world's root, which no question of a validator walking to a network's records asks.
func (self *worldDnssec) ForwardDnssec(ctx context.Context, qType dnsmessage.Type, domain string) ([]byte, bool) {
	name := dns.CanonicalName(dns.Fqdn(domain))
	if self.atEdge(uint16(qType), name) {
		answer, err := self.exchange(ctx, name, uint16(qType), true)
		if err != nil {
			return nil, false
		}
		packed, err := answer.Pack()
		return packed, err == nil
	}
	message := &dns.Msg{}
	message.SetQuestion(name, uint16(qType))
	message.Response = true
	message.RecursionAvailable = true
	message.CheckingDisabled = true
	message.SetEdns0(self.settings.UdpBufferByteCount, true)
	var rrset []dns.RR
	var zone *worldDnssecZone
	switch uint16(qType) {
	case dns.TypeDNSKEY:
		if apex := self.zones[name]; apex != nil {
			zone, rrset = apex, []dns.RR{apex.key}
		}
	case dns.TypeDS:
		for _, parent := range self.zones {
			if 0 < len(parent.childDs) && parent.childDs[0].Header().Name == name {
				zone, rrset = parent, parent.childDs
			}
		}
	}
	if zone == nil {
		message.Rcode = dns.RcodeNameError
	} else {
		signed, err := self.signed(zone, rrset)
		if err != nil {
			return nil, false
		}
		message.Answer = signed
	}
	packed, err := message.Pack()
	return packed, err == nil
}

// See sidecar.SmtpDohForwarder: the device's own DoH, which TL2's _mta-sts lookup and TL3's MX lookup take, as the
// world's dns edge answers them; outside the world's root nothing answers, as public DoH knows no .example name.
func (self *worldDnssec) Forward(ctx context.Context, qType dnsmessage.Type, domain string) ([]byte, bool) {
	name := dns.CanonicalName(dns.Fqdn(domain))
	if !dns.IsSubDomain(self.root, name) {
		return nil, false
	}
	answer, err := self.exchange(ctx, name, uint16(qType), false)
	if err != nil {
		return nil, false
	}
	packed, err := answer.Pack()
	return packed, err == nil
}

// One question to the dns edge over TCP, DO set when dnssec, as a recursive resolver with checking disabled relays
// it: the answer marked a response with recursion available.
func (self *worldDnssec) exchange(ctx context.Context, name string, qType uint16, dnssec bool) (*dns.Msg, error) {
	edge := self.world.Block(WorldBlockDnsedge)
	if edge == nil {
		return nil, errors.New("the world runs no dns edge")
	}
	address := edge.DirectAddress(worldDnsPort)
	if address == "" {
		return nil, errors.New("the dns edge is stopped")
	}
	question := &dns.Msg{}
	question.SetQuestion(name, qType)
	question.RecursionDesired = true
	question.CheckingDisabled = true
	question.SetEdns0(self.settings.UdpBufferByteCount, dnssec)
	client := &dns.Client{Net: "tcp", Timeout: self.settings.ExchangeTimeout}
	answer, _, err := client.ExchangeContext(ctx, question, address)
	if err != nil {
		return nil, err
	}
	answer.RecursionAvailable = true
	answer.CheckingDisabled = true
	return answer, nil
}

// smtpedge's resolver (the file's header): names under the world's root validated through the world's DNSSEC from its
// anchor, every other name the MX world's.
type worldSmtpMxResolver struct {
	root    string
	world   sidecar.SmtpMxResolver
	outside sidecar.SmtpMxResolver
}

var _ sidecar.SmtpMxResolver = (*worldSmtpMxResolver)(nil)

// The resolver over dnssec under its anchor, outside for every other name.
func newWorldSmtpMxResolver(dnssec *worldDnssec, outside sidecar.SmtpMxResolver) (*worldSmtpMxResolver, error) {
	anchors, err := dnssec.Anchors()
	if err != nil {
		return nil, err
	}
	return &worldSmtpMxResolver{
		root:    dnssec.root,
		world:   sidecar.NewSmtpMxDnsResolver(dnssec, anchors, nil, sidecar.RealClock{}),
		outside: outside,
	}, nil
}

func (self *worldSmtpMxResolver) pick(name string) sidecar.SmtpMxResolver {
	if dns.IsSubDomain(self.root, dns.CanonicalName(dns.Fqdn(strings.TrimSpace(name)))) {
		return self.world
	}
	return self.outside
}

func (self *worldSmtpMxResolver) LookupMx(ctx context.Context, domain string) (*sidecar.SmtpMxAnswer, error) {
	return self.pick(domain).LookupMx(ctx, domain)
}

func (self *worldSmtpMxResolver) LookupTlsa(ctx context.Context, host string) (*sidecar.SmtpTlsaAnswer, error) {
	return self.pick(host).LookupTlsa(ctx, host)
}

func (self *worldSmtpMxResolver) LookupAddresses(ctx context.Context, host string) ([]netip.Addr, error) {
	return self.pick(host).LookupAddresses(ctx, host)
}

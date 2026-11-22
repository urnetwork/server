//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// DNSSEC above the world's DNS (world_test.go), for both halves of mail: the world's dns edge is authoritative under
// its own root, zone.e2e.example., signed under the KSK the world made for it, and nothing signs a path from the DNS
// root down to it, so no validator could reach a network's MX from IANA's anchors. The world signs that path itself,
// and beside it the internet sender's own zones, so a receiver's checks of outside mail read real verdicts:
//
//	parents    ".", "example." and "e2e.example.", each a zone of one P-256 key the world draws, each holding its
//	           children's DS, "e2e.example." the DS of the world root's KSK, so a chain from the world's own root
//	           anchor reaches every record the dns edge signs; and "arpa." and "in-addr.arpa." above the sender's
//	           reverse zone
//	sender     the internet sender's domain, sender.example. (worldInternetSender), a zone of its own under
//	           "example.": SPF passing its MTA's address alone, its DKIM selector's ed25519 key, DMARC at p=reject,
//	           its MTA's address, which its EHLO names and its PTR confirms, its MX at mx.sender.example. and that
//	           host's TLSA 3 1 1; and its address's reverse zone, 100.51.198.in-addr.arpa., holding the MTA's PTR
//	           and no other. Each RRset is signed and each denial proven by the zone's NSEC chain (RFC 4035), so
//	           smtpedge's validating resolver and a sending device's DANE reach the MX as they reach a network's
//	forwarder  sidecar.DnsWatchDnssecForwarder as a DNSSEC-OK recursive resolver with checking disabled answers: a
//	           question at or under the world's root, but the root's own DS, goes to the dns edge with DO set, over
//	           UDP and over TCP where that answer is truncated, its answer as the edge signed it; the parents' keys
//	           and DS and the sender's zones are answered here, each RRset with its RRSIG. Forward, the plain DoH a
//	           device and a receiver ask, answers the same names unsigned, a denial with its zone's SOA so a stub
//	           caches it as RFC 2308 says
//	resolver   smtpedge's sidecar.SmtpMxResolver: sidecar.NewSmtpMxDnsResolver over the forwarder under the world's
//	           anchor for names under the world's root and in the sender's zones, so the edge validates a network's
//	           MX, its mail host's address and its TLSA as the dns edge serves them, and the sender's as the world
//	           signs them; and sidecartest's MxWorld for every other name, the sending address's HELO name among
//	           them, which no zone of the world serves
//	dialer     smtpedge's sidecar.SmtpMxDialer: the sender's MX address reaches the sender's MX, a sidecartest
//	           ScriptedMx on the TLSA's key, over an in-process pipe, which records each transaction it takes; every
//	           other address the MX world's
//
// The sender's mail (worldInternetSender.Message) is signed as an MTA of the domain signs it, with the selector's
// key over relaxed headers and body, or not, or with its body changed after signing, and it comes from the MTA's
// address or from another address of its /24 that SPF fails: §15.5's verdicts, pass and fail, each from what the
// world's DNS answers.
//
// What stands in, named in the package header: the parents, which are the internet's DNS root and the zones above
// the world's root in production, the sender's zones and MX, which are a mail domain's and its provider's, and the
// anchor a sending device and smtpedge validate under, theirs in place of IANA's.

package sidecare2e

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"

	moxdkim "github.com/mjl-/mox/dkim"
	moxdns "github.com/mjl-/mox/dns"
	moxsmtp "github.com/mjl-/mox/smtp"

	"github.com/miekg/dns"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"
)

// What the world's DNSSEC parents and the internet sender's zones sign with (QD-702).
type worldDnssecSettings struct {
	// The zones above the world's root, the root first, each a parent of the next and the last the world root's.
	ParentZones []string
	// The internet sender's zones and MTA; nil serves none, every name outside the world's root unanswered.
	Sender *worldInternetSenderSettings
	// Each RRset's TTL, and the signatures' window on either side of now.
	Ttl               uint32
	SignatureValidity time.Duration
	// One question's bound at the dns edge, and the EDNS0 buffer it asks with.
	ExchangeTimeout    time.Duration
	UdpBufferByteCount uint16
	// How many questions the forwarder asks the dns edge over TCP at once, which it asks only where UDP's answer was
	// truncated or never came, as a recursive resolver does: the edge admits at most DnsTcpConnectionsPerSource TCP
	// connections from one source (e2eHostsSettings', 2), and everything of the world asks it from loopback, so a
	// forwarder that opened one a question would be refused past them (dnsservice's edge admit), and a receiver's
	// checks, which ask eight at once, would read temperror. 1, leaving the other to the dns API's SOA reads
	// (worldSoaReader), which ask the edge over TCP; never past the edge's cap.
	TcpExchangeCount int
}

func defaultWorldDnssecSettings() *worldDnssecSettings {
	return &worldDnssecSettings{
		ParentZones:        []string{".", "example.", "e2e.example."},
		Sender:             defaultWorldInternetSenderSettings(),
		Ttl:                3600,
		SignatureValidity:  24 * time.Hour,
		ExchangeTimeout:    10 * time.Second,
		UdpBufferByteCount: 4096,
		TcpExchangeCount:   1,
	}
}

// The internet sender's zones, MTA and MX (the file's header), all documentation names and addresses.
type worldInternetSenderSettings struct {
	// Its domain, a zone of its own under one of the world's parents.
	Domain string
	// The zones above its addresses' reverse zone that are not the world's parents, the highest first: each a child
	// of the one before, the first of ".".
	ReverseParentZones []string
	// The reverse zone of its MTA's /24, which holds the MTA's PTR alone.
	ReverseZone string
	// Its MTA: the name its EHLO gives and its address's PTR names, and the address SPF passes and the name confirms.
	MtaHost    string
	MtaAddress netip.Addr
	// Another address of the MTA's /24, which SPF fails and no PTR names: a forger's.
	ForgedAddress netip.Addr
	// The port each of its connections comes from, as the lb's PROXY header names it.
	SourcePort uint16
	// The DKIM selector its key is published under, and the header fields each signature covers.
	DkimSelector string
	DkimHeaders  []string
	// DMARC's p=: reject, so a message failing both SPF and DKIM files as spam (§15.9).
	DmarcPolicy string
	// The line a kind that fails DKIM adds to a message's body after signing it.
	ChangedBodyLine string
	// Its MX, which a network's mail to its domain reaches through smtpedge: the host, and its address, on the lb's
	// /24, the one documentation prefix the world's smtpedge dials (runSmtpedge's MxRefusedPrefixes).
	MxHost    string
	MxAddress netip.Addr
	// The SOA's serial and its minimum, the TTL of the zones' denials (RFC 2308), which a receiver's stub caches.
	SoaSerial   uint32
	NegativeTtl uint32
	// How long the world's close waits for the MX's connections to end, each closed by smtpedge's stop.
	ServeTimeout time.Duration
}

func defaultWorldInternetSenderSettings() *worldInternetSenderSettings {
	return &worldInternetSenderSettings{
		Domain:             "sender.example.",
		ReverseParentZones: []string{"arpa.", "in-addr.arpa."},
		ReverseZone:        "100.51.198.in-addr.arpa.",
		MtaHost:            "mta.sender.example",
		MtaAddress:         netip.MustParseAddr("198.51.100.25"),
		ForgedAddress:      netip.MustParseAddr("198.51.100.26"),
		SourcePort:         40025,
		DkimSelector:       "s1",
		DkimHeaders:        []string{"From", "To", "Subject", "Date", "Message-ID", "MIME-Version", "Content-Type"},
		DmarcPolicy:        "reject",
		ChangedBodyLine:    "Changed after its signature.",
		MxHost:             "mx.sender.example",
		MxAddress:          netip.MustParseAddr("192.0.2.25"),
		SoaSerial:          1,
		NegativeTtl:        60,
		ServeTimeout:       90 * time.Second,
	}
}

// One zone the world signs: its key, its children's DS and, for the sender's zones, what it serves.
type worldDnssecZone struct {
	name   string
	key    *dns.DNSKEY
	signer crypto.Signer
	// each child zone's DS RRset, by the child's canonical name
	childDs map[string][]dns.RR
	// what the zone serves, by owner and type, its SOA and DNSKEY at its apex among them; nil in a parent, which
	// answers only its key and its children's DS
	records map[string]map[uint16][]dns.RR
	soa     *dns.SOA
	// the owners of records in canonical order (RFC 4034 §6.1), the apex first: its NSEC chain
	owners []string
}

// The world's DNSSEC parents, the sender's zones and the forwarder over them and the dns edge. Immutable once made, so
// safe for concurrent use.
type worldDnssec struct {
	world    *World
	settings *worldDnssecSettings
	// the world's root, canonical with its final dot
	root string
	// by name, canonical with its final dot
	zones map[string]*worldDnssecZone
	// the DNS root's DS: what a validator anchors at
	anchor *dns.DS
	// nil where the settings name no sender
	sender *worldInternetSender
	// one token a TCP question to the dns edge holds (TcpExchangeCount)
	tcpExchanges chan struct{}
}

var _ sidecar.DnsWatchDnssecForwarder = (*worldDnssec)(nil)
var _ sidecar.SmtpDohForwarder = (*worldDnssec)(nil)
var _ sidecar.MailReceiverDoh = (*worldDnssec)(nil)

// The parents over the world's root and its KSK, and the sender's zones under them, each zone keyed afresh.
func newWorldDnssec(world *World, settings *worldDnssecSettings) (*worldDnssec, error) {
	if settings == nil {
		settings = defaultWorldDnssecSettings()
	}
	root := dns.CanonicalName(world.HostsSettings().DnsRoot)
	tcpExchangeCount := max(1, min(settings.TcpExchangeCount, world.HostsSettings().DnsTcpConnectionsPerSource))
	self := &worldDnssec{
		world:        world,
		settings:     settings,
		root:         root,
		zones:        map[string]*worldDnssecZone{},
		tcpExchanges: make(chan struct{}, tcpExchangeCount),
	}
	addZone := func(name string) (*worldDnssecZone, error) {
		name = dns.CanonicalName(name)
		if _, ok := self.zones[name]; ok || name == root {
			return nil, fmt.Errorf("the zone %s is the world's twice", name)
		}
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
		zone := &worldDnssecZone{name: name, key: key, signer: signer, childDs: map[string][]dns.RR{}}
		self.zones[name] = zone
		return zone, nil
	}
	for _, name := range settings.ParentZones {
		if _, err := addZone(name); err != nil {
			return nil, err
		}
	}
	if settings.Sender != nil {
		for _, name := range settings.Sender.ReverseParentZones {
			if _, err := addZone(name); err != nil {
				return nil, err
			}
		}
		sender, err := newWorldInternetSender(settings.Sender)
		if err != nil {
			return nil, err
		}
		self.sender = sender
		for _, name := range []string{settings.Sender.Domain, settings.Sender.ReverseZone} {
			zone, err := addZone(name)
			if err != nil {
				return nil, err
			}
			if err := self.serve(zone, sender.zoneRecords(zone.name, settings.Ttl)); err != nil {
				return nil, err
			}
		}
	}
	// each zone's DS at the deepest zone above it, and the world root's KSK's at the deepest above the root
	for name, zone := range self.zones {
		if name == "." {
			continue
		}
		if err := self.addChildDs(name, zone.key); err != nil {
			return nil, err
		}
	}
	if err := self.addChildDs(root, world.credentials.dnsKskRecord); err != nil {
		return nil, err
	}
	rootZone := self.zones["."]
	if rootZone == nil {
		return nil, errors.New("the parents hold no DNS root")
	}
	self.anchor = rootZone.key.ToDS(dns.SHA256)
	self.anchor.Hdr = dns.RR_Header{Name: ".", Rrtype: dns.TypeDS, Class: dns.ClassINET, Ttl: settings.Ttl}
	if self.sender != nil {
		world.closes = append(world.closes, func() {
			if err := self.sender.wait(settings.Sender.ServeTimeout); err != nil {
				world.t.Errorf("%v", err)
			}
		})
	}
	return self, nil
}

// The deepest of the world's zones strictly above name, nil above the DNS root.
func (self *worldDnssec) zoneAbove(name string) *worldDnssecZone {
	for name != "." {
		labels := dns.SplitDomainName(name)
		name = dns.Fqdn(strings.Join(labels[1:], "."))
		if zone := self.zones[name]; zone != nil {
			return zone
		}
	}
	return nil
}

// The deepest of the world's zones at or above name, nil where none is.
func (self *worldDnssec) zoneAt(name string) *worldDnssecZone {
	if zone := self.zones[name]; zone != nil {
		return zone
	}
	return self.zoneAbove(name)
}

// key's DS, as the parent of child's zone holds it: refused where no zone of the world's is above child.
func (self *worldDnssec) addChildDs(child string, key *dns.DNSKEY) error {
	parent := self.zoneAbove(child)
	if parent == nil {
		return fmt.Errorf("no zone of the world's is above %s", child)
	}
	ds := key.ToDS(dns.SHA256)
	ds.Hdr = dns.RR_Header{Name: child, Rrtype: dns.TypeDS, Class: dns.ClassINET, Ttl: self.settings.Ttl}
	parent.childDs[child] = []dns.RR{ds}
	return nil
}

// Makes zone serve rrs beside its SOA and its key, and chains its owners for NSEC.
func (self *worldDnssec) serve(zone *worldDnssecZone, rrs []dns.RR) error {
	zone.soa = &dns.SOA{
		Hdr:     dns.RR_Header{Name: zone.name, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: self.settings.Ttl},
		Ns:      "ns." + zone.name,
		Mbox:    "hostmaster." + zone.name,
		Serial:  self.settings.Sender.SoaSerial,
		Refresh: self.settings.Ttl,
		Retry:   self.settings.Ttl,
		Expire:  self.settings.Ttl * 24,
		Minttl:  self.settings.Sender.NegativeTtl,
	}
	zone.records = map[string]map[uint16][]dns.RR{}
	for _, rr := range append([]dns.RR{zone.soa, zone.key}, rrs...) {
		owner := dns.CanonicalName(rr.Header().Name)
		if !dns.IsSubDomain(zone.name, owner) {
			return fmt.Errorf("%s is no name of %s", owner, zone.name)
		}
		rr.Header().Name = owner
		if zone.records[owner] == nil {
			zone.records[owner] = map[uint16][]dns.RR{}
		}
		zone.records[owner][rr.Header().Rrtype] = append(zone.records[owner][rr.Header().Rrtype], rr)
	}
	for owner := range zone.records {
		zone.owners = append(zone.owners, owner)
	}
	slices.SortFunc(zone.owners, worldDnsCanonicalCompare)
	return nil
}

// RFC 4034 §6.1's canonical order of two canonical names: by their labels from the rightmost, each compared as
// lowercase octets.
func worldDnsCanonicalCompare(a string, b string) int {
	left, right := dns.SplitDomainName(a), dns.SplitDomainName(b)
	slices.Reverse(left)
	slices.Reverse(right)
	for index := range min(len(left), len(right)) {
		if order := bytes.Compare([]byte(strings.ToLower(left[index])), []byte(strings.ToLower(right[index]))); order != 0 {
			return order
		}
	}
	return len(left) - len(right)
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
		Hdr:        dns.RR_Header{Name: rrset[0].Header().Name, Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: rrset[0].Header().Ttl},
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

// Whether name is in one of the sender's zones, which answer every question at or under their apex.
func (self *worldDnssec) atSender(name string) bool {
	zone := self.zoneAt(name)
	return zone != nil && zone.records != nil
}

// See sidecar.DnsWatchDnssecForwarder: the answer to qType at domain, given without its final dot and the root as
// ".", as packed DNS: the dns edge's for the world's names, and the parents' and the sender's here, signed.
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
	answer, err := self.answer(name, uint16(qType), true)
	if err != nil {
		return nil, false
	}
	packed, err := answer.Pack()
	return packed, err == nil
}

// See sidecar.SmtpDohForwarder and sidecar.MailDohForwarder: the plain DoH a device's TL2, TL3 and DKIM lookups and a
// receiver's checks take, as the world's dns edge answers its names and the sender's zones answer theirs, unsigned;
// any other name is unanswered, as public DoH knows no other .example name.
func (self *worldDnssec) Forward(ctx context.Context, qType dnsmessage.Type, domain string) ([]byte, bool) {
	name := dns.CanonicalName(dns.Fqdn(domain))
	var answer *dns.Msg
	var err error
	switch {
	case dns.IsSubDomain(self.root, name):
		answer, err = self.exchange(ctx, name, uint16(qType), false)
	case self.atSender(name):
		answer, err = self.answer(name, uint16(qType), false)
	default:
		return nil, false
	}
	if err != nil {
		return nil, false
	}
	packed, err := answer.Pack()
	return packed, err == nil
}

// The answer of the world's own zones to qType at name, signed when dnssec: a parent's key at its apex or a child's DS
// it holds, NXDOMAIN for anything else a parent is asked, which no validator walking to a network's or the sender's
// records asks; a sender zone's RRset, or its denial with its SOA and, when dnssec, the NSEC that proves it. A DS at a
// zone's apex is its parent's to answer.
func (self *worldDnssec) answer(name string, qType uint16, dnssec bool) (*dns.Msg, error) {
	message := &dns.Msg{}
	message.SetQuestion(name, qType)
	message.Response = true
	message.RecursionAvailable = true
	message.CheckingDisabled = true
	message.SetEdns0(self.settings.UdpBufferByteCount, dnssec)
	zone := self.zoneAt(name)
	if qType == dns.TypeDS && (self.zones[name] != nil || name == self.root) {
		zone = self.zoneAbove(name)
	}
	if zone == nil {
		message.Rcode = dns.RcodeNameError
		return message, nil
	}
	sign := func(rrset []dns.RR) ([]dns.RR, error) {
		if !dnssec {
			return rrset, nil
		}
		return self.signed(zone, rrset)
	}
	if zone.records == nil {
		var rrset []dns.RR
		switch {
		case qType == dns.TypeDNSKEY && name == zone.name:
			rrset = []dns.RR{zone.key}
		case qType == dns.TypeDS:
			rrset = zone.childDs[name]
		}
		if len(rrset) == 0 {
			message.Rcode = dns.RcodeNameError
			return message, nil
		}
		answer, err := sign(rrset)
		if err != nil {
			return nil, err
		}
		message.Answer = answer
		return message, nil
	}
	if rrset := zone.records[name][qType]; 0 < len(rrset) {
		answer, err := sign(rrset)
		if err != nil {
			return nil, err
		}
		message.Answer = answer
		return message, nil
	}
	authority, err := sign([]dns.RR{zone.soa})
	if err != nil {
		return nil, err
	}
	exists := zone.exists(name)
	if !exists {
		message.Rcode = dns.RcodeNameError
	}
	if dnssec {
		// NODATA: the NSEC at name, or for an empty non-terminal the one covering it; NXDOMAIN: the NSECs covering
		// name and the wildcard at its closest encloser
		owners := []string{}
		if exists {
			owners = append(owners, zone.nsecOwnerOf(name))
		} else {
			owners = append(owners, zone.nsecOwnerOf(name), zone.nsecOwnerOf("*."+zone.closestEncloser(name)))
		}
		slices.Sort(owners)
		for _, owner := range slices.Compact(owners) {
			nsec, err := sign([]dns.RR{zone.nsec(owner, self.settings.Sender.NegativeTtl)})
			if err != nil {
				return nil, err
			}
			authority = append(authority, nsec...)
		}
	}
	message.Ns = authority
	return message, nil
}

// Whether name exists in the zone: an owner of records, or an empty non-terminal above one.
func (self *worldDnssecZone) exists(name string) bool {
	for _, owner := range self.owners {
		if dns.IsSubDomain(name, owner) {
			return true
		}
	}
	return false
}

// The longest name at or above name that exists in the zone, the apex at the least.
func (self *worldDnssecZone) closestEncloser(name string) string {
	for name != self.name {
		if self.exists(name) {
			return name
		}
		labels := dns.SplitDomainName(name)
		name = dns.Fqdn(strings.Join(labels[1:], "."))
	}
	return self.name
}

// The owner whose NSEC proves name's absence or holds its types: name where it owns records, else the last owner
// before name in canonical order, the chain wrapping to its last.
func (self *worldDnssecZone) nsecOwnerOf(name string) string {
	index, found := slices.BinarySearchFunc(self.owners, name, worldDnsCanonicalCompare)
	if found {
		return self.owners[index]
	}
	if index == 0 {
		return self.owners[len(self.owners)-1]
	}
	return self.owners[index-1]
}

// owner's NSEC: the next owner in the chain, and the types owner holds beside RRSIG and NSEC.
func (self *worldDnssecZone) nsec(owner string, ttl uint32) *dns.NSEC {
	index, _ := slices.BinarySearchFunc(self.owners, owner, worldDnsCanonicalCompare)
	next := self.owners[(index+1)%len(self.owners)]
	types := []uint16{dns.TypeRRSIG, dns.TypeNSEC}
	for rrtype := range self.records[owner] {
		types = append(types, rrtype)
	}
	slices.Sort(types)
	return &dns.NSEC{
		Hdr:        dns.RR_Header{Name: owner, Rrtype: dns.TypeNSEC, Class: dns.ClassINET, Ttl: ttl},
		NextDomain: next,
		TypeBitMap: types,
	}
}

// One question to the dns edge, DO set when dnssec, as a recursive resolver with checking disabled relays it: over
// UDP, then over TCP where that answer was truncated or none came, at most TcpExchangeCount at once (the setting's
// row); the answer marked a response with recursion available.
func (self *worldDnssec) exchange(ctx context.Context, name string, qType uint16, dnssec bool) (*dns.Msg, error) {
	edge := self.world.Block(WorldBlockDnsedge)
	if edge == nil {
		return nil, errors.New("the world runs no dns edge")
	}
	question := &dns.Msg{}
	question.SetQuestion(name, qType)
	question.RecursionDesired = true
	question.CheckingDisabled = true
	question.SetEdns0(self.settings.UdpBufferByteCount, dnssec)
	answer, err := func() (*dns.Msg, error) {
		address := edge.UdpAddress()
		if address == "" {
			return nil, errors.New("the dns edge is stopped")
		}
		client := &dns.Client{Net: "udp", Timeout: self.settings.ExchangeTimeout, UDPSize: self.settings.UdpBufferByteCount}
		answer, _, err := client.ExchangeContext(ctx, question, address)
		return answer, err
	}()
	if err != nil || answer.Truncated {
		select {
		case self.tcpExchanges <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		answer, err = func() (*dns.Msg, error) {
			defer func() { <-self.tcpExchanges }()
			address := edge.DirectAddress(worldDnsPort)
			if address == "" {
				return nil, errors.New("the dns edge is stopped")
			}
			client := &dns.Client{Net: "tcp", Timeout: self.settings.ExchangeTimeout}
			answer, _, err := client.ExchangeContext(ctx, question, address)
			return answer, err
		}()
		if err != nil {
			return nil, err
		}
	}
	answer.RecursionAvailable = true
	answer.CheckingDisabled = true
	return answer, nil
}

// smtpedge's resolver (the file's header): names under the world's root and in the sender's zones validated through
// the world's DNSSEC from its anchor, every other name the MX world's.
type worldSmtpMxResolver struct {
	dnssec  *worldDnssec
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
		dnssec:  dnssec,
		world:   sidecar.NewSmtpMxDnsResolver(dnssec, anchors, nil, sidecar.RealClock{}),
		outside: outside,
	}, nil
}

func (self *worldSmtpMxResolver) pick(name string) sidecar.SmtpMxResolver {
	canonical := dns.CanonicalName(dns.Fqdn(strings.TrimSpace(name)))
	if dns.IsSubDomain(self.dnssec.root, canonical) || self.dnssec.atSender(canonical) {
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

// smtpedge's dialer (runSmtpedge): the internet sender's MX address to its MX, any other the MX world's.
type worldSmtpMxDialer struct {
	sender  *worldInternetSender
	outside sidecar.SmtpMxDialer
}

var _ sidecar.SmtpMxDialer = (*worldSmtpMxDialer)(nil)

// The dialer a run of smtpedge sends through: the sender's MX where the world serves one, outside for the rest.
func (self *World) smtpMxDialer(outside sidecar.SmtpMxDialer) sidecar.SmtpMxDialer {
	if self.dnssec == nil || self.dnssec.sender == nil {
		return outside
	}
	return &worldSmtpMxDialer{sender: self.dnssec.sender, outside: outside}
}

func (self *worldSmtpMxDialer) DialMx(ctx context.Context, sendingAddress netip.Addr, mxAddress netip.Addr) (net.Conn, error) {
	if mxAddress != self.sender.settings.MxAddress {
		return self.outside.DialMx(ctx, sendingAddress, mxAddress)
	}
	return self.sender.dialMx(ctx, sendingAddress)
}

// How one internet sender's message is signed and where it comes from (the file's header).
type worldSenderKind int

const (
	// From the MTA's address, signed: SPF, DKIM and DMARC pass.
	worldSenderAligned worldSenderKind = iota
	// From the forged address, signed: SPF fails, DKIM passes, and DMARC passes on DKIM's alignment.
	worldSenderSpfFail
	// From the MTA's address, its body changed after signing: DKIM fails, and DMARC passes on SPF's alignment.
	worldSenderDkimFail
	// From the forged address, its body changed after signing: SPF and DKIM fail, so DMARC fails under p=reject and
	// the message files as spam (§15.9), still deposited and answered 250 (§15.5).
	worldSenderDmarcFail
)

// Every kind, in order.
func worldSenderKinds() []worldSenderKind {
	return []worldSenderKind{worldSenderAligned, worldSenderSpfFail, worldSenderDkimFail, worldSenderDmarcFail}
}

func (self worldSenderKind) String() string {
	switch self {
	case worldSenderAligned:
		return "aligned"
	case worldSenderSpfFail:
		return "spf-fail"
	case worldSenderDkimFail:
		return "dkim-fail"
	case worldSenderDmarcFail:
		return "dmarc-fail"
	}
	return fmt.Sprintf("kind-%d", int(self))
}

// The internet sender (the file's header): its DKIM key and its MX, which records every transaction it takes. Safe
// for concurrent use.
type worldInternetSender struct {
	settings *worldInternetSenderSettings
	dkimKey  ed25519.PrivateKey
	mx       *sidecartest.ScriptedMx
	// every connection the MX serves
	serving sync.WaitGroup
}

// The sender with a fresh DKIM key, and its MX on a fresh certificate for its host.
func newWorldInternetSender(settings *worldInternetSenderSettings) (*worldInternetSender, error) {
	_, dkimKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	mx, err := sidecartest.NewScriptedMx(settings.MxHost, nil, nil)
	if err != nil {
		return nil, err
	}
	return &worldInternetSender{settings: settings, dkimKey: dkimKey, mx: mx}, nil
}

// The domain, without its final dot.
func (self *worldInternetSender) Domain() string {
	return strings.TrimSuffix(dns.CanonicalName(self.settings.Domain), ".")
}

// An address of the domain's.
func (self *worldInternetSender) Address(localPart string) string {
	return localPart + "@" + self.Domain()
}

// The MX, whose Transactions are what a network's mail to the domain delivered.
func (self *worldInternetSender) Mx() *sidecartest.ScriptedMx {
	return self.mx
}

// The records zone serves (the file's header): the sender's domain's, or its reverse zone's.
func (self *worldInternetSender) zoneRecords(zone string, ttl uint32) []dns.RR {
	header := func(name string, rrtype uint16) dns.RR_Header {
		return dns.RR_Header{Name: dns.CanonicalName(dns.Fqdn(name)), Rrtype: rrtype, Class: dns.ClassINET, Ttl: ttl}
	}
	settings := self.settings
	if dns.CanonicalName(zone) == dns.CanonicalName(settings.ReverseZone) {
		reverse, _ := dns.ReverseAddr(settings.MtaAddress.String())
		return []dns.RR{&dns.PTR{Hdr: header(reverse, dns.TypePTR), Ptr: dns.CanonicalName(dns.Fqdn(settings.MtaHost))}}
	}
	domain := dns.CanonicalName(settings.Domain)
	dkimRecord := &moxdkim.Record{Version: "DKIM1", Key: "ed25519", Pubkey: []byte(self.dkimKey.Public().(ed25519.PublicKey))}
	dkimText, _ := dkimRecord.Record()
	spki := sha256.Sum256(self.mx.Certificate().RawSubjectPublicKeyInfo)
	return []dns.RR{
		&dns.TXT{Hdr: header(domain, dns.TypeTXT), Txt: []string{"v=spf1 ip4:" + settings.MtaAddress.String() + " -all"}},
		&dns.MX{Hdr: header(domain, dns.TypeMX), Preference: 10, Mx: dns.CanonicalName(dns.Fqdn(settings.MxHost))},
		&dns.TXT{Hdr: header("_dmarc."+domain, dns.TypeTXT), Txt: []string{"v=DMARC1; p=" + settings.DmarcPolicy}},
		&dns.TXT{Hdr: header(settings.DkimSelector+"._domainkey."+domain, dns.TypeTXT), Txt: []string{dkimText}},
		&dns.A{Hdr: header(settings.MtaHost, dns.TypeA), A: net.IP(settings.MtaAddress.AsSlice())},
		&dns.A{Hdr: header(settings.MxHost, dns.TypeA), A: net.IP(settings.MxAddress.AsSlice())},
		&dns.TLSA{Hdr: header("_25._tcp."+settings.MxHost, dns.TypeTLSA), Usage: 3, Selector: 1, MatchingType: 1, Certificate: hex.EncodeToString(spki[:])},
	}
}

// The address one of kind's connections comes from, as the lb's PROXY header names it.
func (self *worldInternetSender) Source(kind worldSenderKind) netip.AddrPort {
	switch kind {
	case worldSenderSpfFail, worldSenderDmarcFail:
		return netip.AddrPortFrom(self.settings.ForgedAddress, self.settings.SourcePort)
	}
	return netip.AddrPortFrom(self.settings.MtaAddress, self.settings.SourcePort)
}

// One message of the sender's as kind sends it: the plain message from From's mailbox, a DKIM-Signature under the
// selector's key above it, and for the kinds that fail DKIM a line added to its body after signing.
func (self *worldInternetSender) Message(ctx context.Context, kind worldSenderKind, from string, to string, subject string, messageId string, body string) ([]byte, error) {
	message := worldMailMessageBytes(from, to, subject, messageId, body)
	localPart, domainText, ok := strings.Cut(from, "@")
	if !ok || !strings.EqualFold(domainText, self.Domain()) {
		return nil, fmt.Errorf("%s is no address of %s", from, self.Domain())
	}
	domain, err := moxdns.ParseDomain(domainText)
	if err != nil {
		return nil, err
	}
	selector, err := moxdns.ParseDomain(self.settings.DkimSelector)
	if err != nil {
		return nil, err
	}
	headers, err := moxdkim.Sign(ctx, slog.New(slog.DiscardHandler), moxsmtp.Localpart(localPart), domain, []moxdkim.Selector{{
		Hash:          "sha256",
		HeaderRelaxed: true,
		BodyRelaxed:   true,
		Headers:       self.settings.DkimHeaders,
		PrivateKey:    self.dkimKey,
		Domain:        selector,
	}}, false, bytes.NewReader(message))
	if err != nil {
		return nil, err
	}
	signed := append([]byte(headers), message...)
	if self.changesBody(kind) {
		signed = append(signed, []byte(self.settings.ChangedBodyLine+"\r\n")...)
	}
	return signed, nil
}

// Whether kind changes a message's body after signing it, so its DKIM fails.
func (self *worldInternetSender) changesBody(kind worldSenderKind) bool {
	return kind == worldSenderDkimFail || kind == worldSenderDmarcFail
}

// The text a reader reads of kind's Message of body: body, and the line kind added after signing, if it adds one.
func (self *worldInternetSender) Text(kind worldSenderKind, body string) string {
	if self.changesBody(kind) {
		return body + "\n" + self.settings.ChangedBodyLine
	}
	return body
}

// One connection from smtpedge's sending address to the MX, served to its end on a goroutine the world's close
// waits for.
func (self *worldInternetSender) dialMx(ctx context.Context, sendingAddress netip.Addr) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	client, server := sidecartest.NewMxPipe(
		net.TCPAddrFromAddrPort(netip.AddrPortFrom(sendingAddress, self.settings.SourcePort)),
		net.TCPAddrFromAddrPort(netip.AddrPortFrom(self.settings.MxAddress, worldSmtpPort)),
	)
	self.serving.Go(func() {
		self.mx.ServeMx(server)
	})
	return client, nil
}

// Waits until every connection the MX took has been served to its end, an error past timeout.
func (self *worldInternetSender) wait(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		defer close(done)
		self.serving.Wait()
	}()
	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("the internet sender's MX still served a connection %s after the world's blocks stopped", timeout)
	}
}

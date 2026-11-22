//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The harness of the dns and certificates case (dns_certs_concurrency_test.go): each device's writes through dns's
// api and what it learned of each, the readers at the dns edge, the DNSSEC checks of what the edge answers against the
// world's KSK, and every read of server state the case makes outside the hosts and api's routes, in one place (the e2e
// conventions): the root's journal, which says what each write committed and at which serial, and each challenge
// name's writes in dns_challenges_per_name_hour's window; the network's dns.records admin events, which §12.7's
// dns_changes_per_hour counts; the certificate rows, the custom domains' receipts and spends; certs' budget in Redis;
// and the objects api's process wrote on the first metadata cluster, which its write budget reports.
//
// A write's outcome is what its answer said. An unavailable answer, the dns API's own or the lb's while the block is
// down, wrote nothing. A 5xx past the API, or no answer at all, leaves the outcome unknown, as a client that lost its
// answer has it: a device's write to its own RRset, which no other device writes, is sent again, and takes no new
// serial where the first landed (the record API answers a write that changes nothing with the zone's version); a
// compare-and-swap on the shared RRset is not sent again, since the zone may have moved past it either way, and the
// journal settles at the end whether it landed, once at most.

package sidecare2e

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/certsservice"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// One RRset a case's devices write.
type dnsCertsSetKey struct {
	// Canonical, with its final dot.
	Owner string
	Type  uint16
}

func (self dnsCertsSetKey) String() string {
	return self.Owner + " " + dns.TypeToString[self.Type]
}

// One write of one device through dns's api, as its writer saw it.
type dnsCertsWrite struct {
	Writer string
	Set    dnsCertsSetKey
	// The one value the write put in the set, which no other write puts anywhere.
	Value string
	// Compare-and-swapped on the zone's version its writer read: the shared set's writes.
	Cas bool
	// The version the compare-and-swap that was answered expected.
	Expect uint64
	// The version its answer named, 0 while it has none.
	Version uint64
	// An attempt's outcome was unknown (see the file header).
	Unknown bool
	// It was answered: Version is its own serial, or for a plain write sent again after an unknown outcome, the
	// zone's version the answer found.
	Answered bool
	// The version conflicts its compare-and-swap met before its answer.
	Conflicts int
}

// One device of the case as a writer of its network's zone: its own dns client and the writes it made, in order.
// Safe for concurrent use.
type dnsCertsWriter struct {
	Device  *sidecart3.T3Device
	Network *dnsCertsNetwork
	// Its place among the case's devices, which its sets and values are named by.
	Index int
	// A dns client of its own over the device's service tokens, as Sidecar.Dns builds one, whose Zone reads the zone
	// afresh at every call (ResyncInterval 0), so a compare-and-swap expects the version the API holds.
	Client *sidecar.DnsClient

	stateLock sync.Mutex
	writes    []*dnsCertsWrite
}

// The writer's writes so far, in order.
func (self *dnsCertsWriter) Writes() []*dnsCertsWrite {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.writes)
}

func (self *dnsCertsWriter) keep(write *dnsCertsWrite) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.writes = append(self.writes, write)
}

// One network of the case: its sidecars as writers and its zone.
type dnsCertsNetwork struct {
	*WorldNetwork
	// The zone's root and apex, canonical with their final dots.
	Root string
	Apex string
	// Each sidecar's writer, in enrollment order.
	Writers []*dnsCertsWriter
}

// The owner name of host in the network's zone, canonical.
func (self *dnsCertsNetwork) OwnerName(host string) string {
	return host + "." + self.Apex
}

// A writer for device, its dns client on the device's own service tokens at the world's dns origin.
func newDnsCertsWriter(world *World, network *dnsCertsNetwork, device *sidecart3.T3Device, index int) (*dnsCertsWriter, error) {
	settings := sidecar.DefaultDnsClientSettings()
	settings.ResyncInterval = 0
	client, err := sidecar.NewDnsClient(&sidecar.DnsClientArgs{
		DnsUrl:       device.Settings.DnsUrl,
		Transport:    world.T3().Api().Transport(),
		ServiceToken: device.Sidecar().ServiceToken,
		NetworkId:    sidecar.Id(network.NetworkId),
		Settings:     settings,
		Clock:        sidecar.RealClock{},
	})
	if err != nil {
		return nil, err
	}
	return &dnsCertsWriter{Device: device, Network: network, Index: index, Client: client}, nil
}

// How an attempt of a write ended.
type dnsCertsAttemptEnd int

const (
	// answered with a version
	dnsCertsAttemptAnswered dnsCertsAttemptEnd = iota
	// nothing was written: the API or the lb answered unavailable
	dnsCertsAttemptUnavailable
	// compare-and-swap refused: the zone is at a newer version
	dnsCertsAttemptConflict
	// a refusal that is the write's answer
	dnsCertsAttemptRefused
	// no answer that says, a 5xx past the API or none at all
	dnsCertsAttemptUnknown
)

// How err ends an attempt.
func dnsCertsAttemptEndOf(err error) dnsCertsAttemptEnd {
	if err == nil {
		return dnsCertsAttemptAnswered
	}
	var apiErr *sidecar.DnsApiError
	if !errors.As(err, &apiErr) {
		return dnsCertsAttemptUnknown
	}
	switch {
	case apiErr.Code == sidecar.DnsApiCodeUnavailable:
		return dnsCertsAttemptUnavailable
	case apiErr.Code == sidecar.DnsApiCodeVersionConflict:
		return dnsCertsAttemptConflict
	case apiErr.Status/100 == 4:
		return dnsCertsAttemptRefused
	}
	return dnsCertsAttemptUnknown
}

// The zone as the API holds it now, read through the writer's client and tried again while the API or the lb
// answers unavailable or nothing: a read changes nothing.
func (self *dnsCertsWriter) zone(ctx context.Context, settings *WorldDnsSettings) (*sidecar.DnsZone, error) {
	for {
		zone, err := self.Client.Zone(ctx)
		end := dnsCertsAttemptEndOf(err)
		if end == dnsCertsAttemptAnswered || (end != dnsCertsAttemptUnavailable && end != dnsCertsAttemptUnknown) {
			return zone, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s's zone read: %w (%w)", self.Device.Name, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// Writes value as the whole of set through the writer's client, compare-and-swapped on the zone's version read just
// before when cas, as the file header says: the write as its writer saw it, kept in the writer's list whatever its
// end, and the refusal that answered it, if one did. casTryCount bounds the conflicts a compare-and-swap rides.
func (self *dnsCertsWriter) Write(ctx context.Context, set dnsCertsSetKey, value string, cas bool, casTryCount int, settings *WorldDnsSettings) (*dnsCertsWrite, error) {
	write := &dnsCertsWrite{Writer: self.Device.Name, Set: set, Value: value, Cas: cas}
	defer self.keep(write)
	upsert := &sidecar.DnsRecordUpsert{Name: set.Owner, Type: dns.TypeToString[set.Type], Values: []string{value}}
	for {
		var expect *uint64
		if cas {
			zone, err := self.zone(ctx, settings)
			if err != nil {
				return write, err
			}
			version := zone.Version
			expect = &version
		}
		answer, err := self.Client.SetRecord(ctx, upsert, expect)
		switch dnsCertsAttemptEndOf(err) {
		case dnsCertsAttemptAnswered:
			write.Answered = true
			write.Version = answer.Version
			if expect != nil {
				write.Expect = *expect
			}
			return write, nil
		case dnsCertsAttemptConflict:
			write.Conflicts += 1
			if casTryCount <= write.Conflicts {
				return write, fmt.Errorf("%s's compare-and-swap of %s met %d conflicts: %w", self.Device.Name, set, write.Conflicts, err)
			}
		case dnsCertsAttemptRefused:
			return write, err
		case dnsCertsAttemptUnknown:
			write.Unknown = true
			if cas {
				// the journal says whether it landed; it is never sent again
				return write, nil
			}
		}
		select {
		case <-ctx.Done():
			return write, fmt.Errorf("%s's write of %s: %v (%w)", self.Device.Name, set, err, ctx.Err())
		case <-time.After(settings.RetryInterval):
		}
	}
}

// The root's zone keys, as its DNSKEY set gives them once that set's signature verifies under the KSK the world's
// dns.yml anchors: what every signature the edge makes must verify under.
type dnsCertsZoneKeys struct {
	root string
	// by key tag
	keys map[uint16]*dns.DNSKEY
}

// The verified zone keys of the world's root, read from the dns edge.
func newDnsCertsZoneKeys(ctx context.Context, world *World, settings *WorldDnsSettings) (*dnsCertsZoneKeys, error) {
	root := dns.CanonicalName(world.HostsSettings().DnsRoot)
	ksk := world.credentials.dnsKskRecord
	answer, err := world.AwaitDnsAnswer(ctx, root, dns.TypeDNSKEY, func(answer *dns.Msg) bool {
		return answer != nil && answer.Rcode == dns.RcodeSuccess && 0 < len(answer.Answer)
	}, settings)
	if err != nil {
		return nil, fmt.Errorf("the root's DNSKEY set: %w", err)
	}
	keys := map[uint16]*dns.DNSKEY{}
	rrset := []dns.RR{}
	for _, record := range answer.Answer {
		if key, ok := record.(*dns.DNSKEY); ok && dns.CanonicalName(key.Hdr.Name) == root {
			keys[key.KeyTag()] = key
			rrset = append(rrset, key)
		}
	}
	verified := false
	for _, record := range answer.Answer {
		signature, ok := record.(*dns.RRSIG)
		if !ok || signature.TypeCovered != dns.TypeDNSKEY || signature.KeyTag != ksk.KeyTag() {
			continue
		}
		if signature.Verify(ksk, rrset) == nil && signature.ValidityPeriod(time.Now()) {
			verified = true
		}
	}
	if !verified {
		return nil, fmt.Errorf("the root's DNSKEY set carries no signature of the KSK's that verifies: %v", answer)
	}
	if _, ok := keys[ksk.KeyTag()]; !ok {
		return nil, errors.New("the root's DNSKEY set does not hold the KSK")
	}
	return &dnsCertsZoneKeys{root: root, keys: keys}, nil
}

// Whether answer's records of name and rrType, of which it holds at least one, carry a signature that verifies under
// one of the zone's keys, signed by the root and valid at now: §12.6's online signing, as a validator holding the KSK
// as its anchor reads it.
func (self *dnsCertsZoneKeys) Verify(answer *dns.Msg, name string, rrType uint16, now time.Time) error {
	if answer == nil {
		return errors.New("no answer")
	}
	name = dns.CanonicalName(name)
	rrset := []dns.RR{}
	for _, record := range answer.Answer {
		if record.Header().Rrtype == rrType && dns.CanonicalName(record.Header().Name) == name {
			rrset = append(rrset, record)
		}
	}
	if len(rrset) == 0 {
		return fmt.Errorf("no %s record of %s to verify", dns.TypeToString[rrType], name)
	}
	reasons := []string{}
	for _, record := range answer.Answer {
		signature, ok := record.(*dns.RRSIG)
		if !ok || signature.TypeCovered != rrType || dns.CanonicalName(signature.Hdr.Name) != name {
			continue
		}
		key, ok := self.keys[signature.KeyTag]
		switch {
		case !ok:
			reasons = append(reasons, fmt.Sprintf("a signature by key %d, which the DNSKEY set does not hold", signature.KeyTag))
		case dns.CanonicalName(signature.SignerName) != self.root:
			reasons = append(reasons, "a signature by "+signature.SignerName)
		case !signature.ValidityPeriod(now):
			reasons = append(reasons, "a signature outside its validity")
		default:
			if err := signature.Verify(key, rrset); err != nil {
				reasons = append(reasons, fmt.Sprintf("a signature that does not verify: %v", err))
				continue
			}
			return nil
		}
	}
	if len(reasons) == 0 {
		return fmt.Errorf("%s's %s set carries no signature", name, dns.TypeToString[rrType])
	}
	return fmt.Errorf("%s's %s set: %s", name, dns.TypeToString[rrType], strings.Join(reasons, "; "))
}

// The values answer holds for set, in the form a write names them: a TXT record's text, a CNAME's canonical target.
// None for an empty answer.
func dnsCertsAnswerValues(answer *dns.Msg, set dnsCertsSetKey) []string {
	values := []string{}
	if answer == nil {
		return values
	}
	for _, record := range answer.Answer {
		if dns.CanonicalName(record.Header().Name) != set.Owner || record.Header().Rrtype != set.Type {
			continue
		}
		switch typed := record.(type) {
		case *dns.TXT:
			values = append(values, strings.Join(typed.Txt, ""))
		case *dns.CNAME:
			values = append(values, dns.CanonicalName(typed.Target))
		}
	}
	slices.Sort(values)
	return values
}

// The addresses answer holds at name, canonical, of rrType, A or AAAA, sorted; none for an empty answer.
func dnsCertsAnswerAddresses(answer *dns.Msg, name string, rrType uint16) []netip.Addr {
	addresses := []netip.Addr{}
	if answer == nil {
		return addresses
	}
	for _, record := range answer.Answer {
		if dns.CanonicalName(record.Header().Name) != name || record.Header().Rrtype != rrType {
			continue
		}
		var ip net.IP
		switch typed := record.(type) {
		case *dns.A:
			ip = typed.A
		case *dns.AAAA:
			ip = typed.AAAA
		}
		if address, ok := netip.AddrFromSlice(ip); ok {
			if rrType == dns.TypeA {
				address = address.Unmap()
			}
			addresses = append(addresses, address)
		}
	}
	slices.SortFunc(addresses, netip.Addr.Compare)
	return addresses
}

// The world's lb addresses a published name's rrType, A or AAAA, answers while every probe passes
// (World.LbEdgeAddresses).
func dnsCertsLbAddresses(world *World, rrType uint16) []netip.Addr {
	ipv4s, ipv6s := world.LbEdgeAddresses()
	if rrType == dns.TypeA {
		return ipv4s
	}
	return ipv6s
}

// The values of a journal row's RRset, from their presentation form, in the form a write names them.
func dnsCertsRdataValues(set dnsservice.DnsRecordSet) ([]string, error) {
	values := []string{}
	for _, rdata := range set.Rdata {
		record, err := dns.NewRR(fmt.Sprintf("%s 60 IN %s %s", set.Owner, dns.TypeToString[set.Type], rdata))
		if err != nil {
			return nil, fmt.Errorf("%s's rdata %q: %w", set.Owner, rdata, err)
		}
		switch typed := record.(type) {
		case *dns.TXT:
			values = append(values, strings.Join(typed.Txt, ""))
		case *dns.CNAME:
			values = append(values, dns.CanonicalName(typed.Target))
		default:
			values = append(values, rdata)
		}
	}
	slices.Sort(values)
	return values, nil
}

// The root's SOA serial as the dns edge answers it now, its signature verified: an error while the edge is stopped
// or its answer does not verify.
func dnsCertsSoaSerial(ctx context.Context, world *World, keys *dnsCertsZoneKeys, settings *WorldDnsSettings) (uint64, error) {
	answer, err := world.DnsQuery(ctx, keys.root, dns.TypeSOA, settings)
	if err != nil {
		return 0, err
	}
	for _, record := range answer.Answer {
		if soa, ok := record.(*dns.SOA); ok && dns.CanonicalName(soa.Hdr.Name) == keys.root {
			if err := keys.Verify(answer, keys.root, dns.TypeSOA, time.Now()); err != nil {
				return 0, err
			}
			return uint64(soa.Serial), nil
		}
	}
	return 0, fmt.Errorf("the edge answered the root's SOA with none: %v", answer)
}

// One read of one RRset at the dns edge between two reads of the root's SOA, whose serial is the root's (§12.6): the
// RRset was served from the zone at a serial between the two.
type dnsCertsRead struct {
	Reader string
	Set    dnsCertsSetKey
	Before uint64
	After  uint64
	// What the edge answered, sorted; none for an empty answer.
	Values []string
}

// One reader of the dns edge: each read keeps the serials it saw, which must never go back. Safe for concurrent use.
type dnsCertsReader struct {
	Name string

	stateLock  sync.Mutex
	lastSerial uint64
	reads      []*dnsCertsRead
	// reads skipped while the edge was stopped
	skipped int
}

// The reader's reads so far.
func (self *dnsCertsReader) Reads() []*dnsCertsRead {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return slices.Clone(self.reads)
}

// Reads skipped while the edge was stopped.
func (self *dnsCertsReader) Skipped() int {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	return self.skipped
}

// One read of a set random picks from sets: the SOA, the set, the SOA again, every answer's signature verified. A read
// that finds the edge stopped, or that loses an exchange, is skipped and counted; one that sees the serial go back,
// or an answer that does not verify, is the error.
func (self *dnsCertsReader) Read(ctx context.Context, world *World, keys *dnsCertsZoneKeys, sets []dnsCertsSetKey, random *mathrand.Rand, settings *WorldDnsSettings) error {
	set := sets[random.IntN(len(sets))]
	skip := func() error {
		self.stateLock.Lock()
		defer self.stateLock.Unlock()
		self.skipped += 1
		return nil
	}
	before, err := dnsCertsSoaSerial(ctx, world, keys, settings)
	if err != nil {
		if dnsCertsEdgeUnanswered(err) {
			return skip()
		}
		return err
	}
	answer, err := world.DnsQuery(ctx, set.Owner, set.Type, settings)
	if err != nil {
		if dnsCertsEdgeUnanswered(err) {
			return skip()
		}
		return err
	}
	if answer.Rcode != dns.RcodeSuccess {
		return fmt.Errorf("%s: the edge answered %s for %s", self.Name, dns.RcodeToString[answer.Rcode], set)
	}
	values := dnsCertsAnswerValues(answer, set)
	if 0 < len(values) {
		if err := keys.Verify(answer, set.Owner, set.Type, time.Now()); err != nil {
			return fmt.Errorf("%s: %w", self.Name, err)
		}
	}
	after, err := dnsCertsSoaSerial(ctx, world, keys, settings)
	if err != nil {
		if dnsCertsEdgeUnanswered(err) {
			return skip()
		}
		return err
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if before < self.lastSerial || after < before {
		return fmt.Errorf("%s saw the root's serial go back: %d, then %d, then %d", self.Name, self.lastSerial, before, after)
	}
	self.lastSerial = after
	self.reads = append(self.reads, &dnsCertsRead{Reader: self.Name, Set: set, Before: before, After: after, Values: values})
	return nil
}

// Whether err is the edge not answering: stopped, restarting, or an exchange lost.
func dnsCertsEdgeUnanswered(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr) || strings.Contains(err.Error(), "the dns edge is stopped") || errors.Is(err, context.DeadlineExceeded)
}

// Every journal row of root above after, in serial order, read until the root's serial as it stood at the first
// read (model.GetDnsJournal): what the zone's writes committed, one row a serial.
func dnsCertsJournal(ctx context.Context, root string, after uint64, pageCount int) (rows []dnsservice.DnsJournalRow, returnErr error) {
	server.HandleError(func() {
		var end uint64
		for {
			read, err := model.GetDnsJournal(ctx, root, after, pageCount)
			if err != nil {
				returnErr = err
				return
			}
			if end == 0 {
				end = read.Serial
			}
			if read.Gap {
				returnErr = fmt.Errorf("the journal of %s has a gap after %d", root, after)
				return
			}
			rows = append(rows, read.Rows...)
			if len(read.Rows) == 0 || end <= read.Rows[len(read.Rows)-1].Serial {
				return
			}
			after = read.Rows[len(read.Rows)-1].Serial
		}
	}, func(err error) {
		returnErr = fmt.Errorf("the journal: %w", err)
	})
	return
}

// The root's serial as Postgres holds it now (model.GetDnsRootState).
func dnsCertsRootSerial(ctx context.Context, root string) (serial uint64, returnErr error) {
	server.HandleError(func() {
		state, err := model.GetDnsRootState(ctx, root)
		if err != nil {
			returnErr = err
			return
		}
		serial = state.Serial
	}, func(err error) {
		returnErr = fmt.Errorf("the root's state: %w", err)
	})
	return
}

// The network's dns.records admin events at or after since, which §12.7's dns_changes_per_hour counts over its window
// (model's WriteDnsRecords, step 7), and the times of each.
func dnsCertsChangeEvents(ctx context.Context, networkId server.Id, since time.Time) (eventTimes []time.Time, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT event_time
					FROM network_admin_event
					WHERE network_id = $1 AND action = $2 AND $3 <= event_time
					ORDER BY event_time
				`,
				networkId,
				model.AdminEventActionDnsRecords,
				since.UTC(),
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var eventTime time.Time
					server.Raise(result.Scan(&eventTime))
					eventTimes = append(eventTimes, eventTime)
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the admin events: %w", err)
	})
	return
}

// A refusal of §12.7's dns_changes_per_hour, nil for any other answer.
func dnsCertsChangesRefusal(err error) *sidecar.QuotaRefusal {
	var apiErr *sidecar.DnsApiError
	if errors.As(err, &apiErr) && apiErr.Code == sidecar.DnsApiCodeQuota && apiErr.Refusal != nil &&
		apiErr.Refusal.LimitKey == model.DnsChangesPerHourLimitKey && apiErr.Status == http.StatusTooManyRequests {
		return apiErr.Refusal
	}
	return nil
}

// The network's certificate rows as network_tls_cert holds them (model.NewCertsStore's ListNetworkCerts), oldest
// first.
func dnsCertsNetworkCerts(ctx context.Context, networkId server.Id) (rows []*sidecar.TlsCertRecord, returnErr error) {
	server.HandleError(func() {
		rows, returnErr = model.NewCertsStore(nil).ListNetworkCerts(ctx, sidecar.Id(networkId))
	}, func(err error) {
		returnErr = fmt.Errorf("the certificate rows: %w", err)
	})
	return
}

// publisher's own certificate rows, oldest first.
func dnsCertsSidecarCerts(ctx context.Context, publisher *dnsCertsPublisher) ([]*sidecar.TlsCertRecord, error) {
	rows, err := dnsCertsNetworkCerts(ctx, publisher.Network.NetworkId)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(rows, func(row *sidecar.TlsCertRecord) bool { return row.SidecarId != publisher.SidecarId }), nil
}

// rows as one line each for a failure's message: id, state, CA, serial, successor and chain length.
func dnsCertsDescribeRows(rows []*sidecar.TlsCertRecord) string {
	lines := []string{}
	for _, row := range rows {
		lines = append(lines, fmt.Sprintf("%s %s ca %q serial %q successor %s chain %d", row.CertId, row.State, row.Ca, row.Serial, row.SuccessorCertId, len(row.Chain)))
	}
	if len(lines) == 0 {
		return "none"
	}
	return strings.Join(lines, "; ")
}

// Waits until want holds for each publisher's rows, read every CertPollInterval: a row moves at a pass, with no hook
// a test reads.
func dnsCertsAwaitRows(ctx context.Context, publishers []*dnsCertsPublisher, settings *dnsCertsSettings, want func(publisher *dnsCertsPublisher, rows []*sidecar.TlsCertRecord) bool) error {
	for {
		waiting := []string{}
		for _, publisher := range publishers {
			rows, err := dnsCertsSidecarCerts(ctx, publisher)
			if err != nil {
				return err
			}
			if !want(publisher, rows) {
				waiting = append(waiting, fmt.Sprintf("%s (%s)", publisher.Device.Name, dnsCertsDescribeRows(rows)))
			}
		}
		if len(waiting) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the certificate rows of %s never were as wanted: %w", strings.Join(waiting, ", "), ctx.Err())
		case <-time.After(settings.CertPollInterval):
		}
	}
}

// Reads a sorted set's members in full, each followed by its score.
var dnsCertsBudgetMembersScript = &sidecar.RedisScript{
	Name:   "e2e_certs_budget_members",
	Source: `return redis.call('ZRANGE', KEYS[1], 0, -1, 'WITHSCORES')`,
}

// The members of root's budget at ca, {tcb_<ca>_<root>} (certsservice's budget.go), each scored by its admission in
// Unix ms: each order that spent a unit, by its cert_id, and the marker of the key's rebuild, scored 0.
func dnsCertsBudgetMembers(ctx context.Context, world *World, ca certsservice.CertsCa, root string) (map[string]int64, error) {
	reply, err := world.T3().Host().Redis.RunScript(ctx, dnsCertsBudgetMembersScript, []string{fmt.Sprintf("{tcb_%s_%s}", ca, root)}, nil)
	if err != nil {
		return nil, err
	}
	if len(reply.Replies)%2 != 0 {
		return nil, fmt.Errorf("the certs budget answered %d members and scores", len(reply.Replies))
	}
	members := map[string]int64{}
	for index := 0; index < len(reply.Replies); index += 2 {
		score, err := strconv.ParseFloat(string(reply.Replies[index+1].Bulk), 64)
		if err != nil {
			return nil, fmt.Errorf("the certs budget scores %q with %q: %w", reply.Replies[index].Bulk, reply.Replies[index+1].Bulk, err)
		}
		members[string(reply.Replies[index].Bulk)] = int64(score)
	}
	return members, nil
}

// How many journal rows of root committed at or after since upsert the TXT at owner, canonical: what the dns API counts
// against dns_challenges_per_name_hour for a challenge name (model's countDnsChallengeWritesInTx, over the network's
// rows or root's own alike), read from dns_zone_journal as it does, by containment.
func dnsCertsChallengeWindowCount(ctx context.Context, root string, owner string, since time.Time) (count int, returnErr error) {
	match := fmt.Sprintf(`{"upserts":[{"owner":%q,"type":%d}]}`, dns.CanonicalName(owner), dns.TypeTXT)
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM dns_zone_journal WHERE root = $1 AND $2 <= commit_time AND diff @> $3::jsonb`,
				dns.CanonicalName(root),
				since.UTC(),
				match,
			).Scan(&count))
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the challenge window of %s: %w", owner, err)
	})
	return
}

// A custom domain's challenge target's writes in the cap's window ending at now, as certs reads them before it
// charges a write (model.GetDnsAcmeChallengeWindowInTx; QD-1165).
func dnsCertsAcmeChallengeWindow(ctx context.Context, root string, name string, now time.Time) (count int, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			count, _ = model.GetDnsAcmeChallengeWindowInTx(ctx, conn, root, name, now)
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the acme window of %s: %w", name, err)
	})
	return
}

// The key prefixes of the objects api's process writes on the first metadata cluster, and the P0 kind it issues each
// one's PUT to its write budget as: §7.9's log entries and a base's rows (model.ConfigLogSequencer), and §7.10's
// revocation records (model.SidecarRevStore).
var dnsCertsApiObjectKinds = map[string]sidecar.WriteP0Kind{
	"proj/":     sidecar.WriteP0KindLog,
	"projrows/": sidecar.WriteP0KindLog,
	"rev/":      sidecar.WriteP0KindRevocation,
}

// Every object under dnsCertsApiObjectKinds' prefixes on the world's first metadata cluster, its byte count by key,
// read a page of pageCount at a time.
func dnsCertsApiObjects(ctx context.Context, world *World, pageCount int) (map[string]int64, error) {
	cluster, err := world.T3().Host().Blobs.FirstCluster()
	if err != nil {
		return nil, err
	}
	objects := map[string]int64{}
	for prefix := range dnsCertsApiObjectKinds {
		startAfter := ""
		for {
			page, more, err := cluster.List(ctx, prefix, startAfter, pageCount)
			if err != nil {
				return nil, fmt.Errorf("the objects under %s: %w", prefix, err)
			}
			for _, object := range page {
				objects[object.Key] = object.ByteCount
				startAfter = object.Key
			}
			if !more || len(page) == 0 {
				break
			}
		}
	}
	return objects, nil
}

// A verified custom domain of networkId's with host mapped to owner (§7.6, §12.8 steps 1 and 2), written as POST
// /network/domain-verify's first verification and POST /network/dns/custom-host-add leave the rows: the domain
// verified with its acme_token, 16 random bytes in lowercase base32 with no padding as verification mints it, and the
// host active in weak mode, which needs no CAA walk. Both routes are a Team's (model.RequireTeamTierInTx) and the case's
// networks are Pro, so the case writes the rows those routes would, a stand-in its header names. root and owner are
// canonical, with their final dots. The token.
func dnsCertsAddCustomDomain(ctx context.Context, networkId server.Id, domain string, host string, root string, owner string) (acmeToken string, returnErr error) {
	tokenBytes := make([]byte, 16)
	if _, err := rand.Read(tokenBytes); err != nil {
		return "", err
	}
	acmeToken = strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(tokenBytes))
	challengeBytes := make([]byte, 32)
	if _, err := rand.Read(challengeBytes); err != nil {
		return "", err
	}
	server.HandleError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			now := server.NowUtc()
			server.RaisePgResult(tx.Exec(
				ctx,
				`INSERT INTO network_domain (network_id, domain, challenge, verify_time, acme_token) VALUES ($1, $2, $3, $4, $5)`,
				networkId,
				domain,
				base64.RawURLEncoding.EncodeToString(challengeBytes),
				now,
				acmeToken,
			))
			server.RaisePgResult(tx.Exec(
				ctx,
				`
					INSERT INTO network_dns_custom_host (host, root, network_id, owner, state, caa_mode, create_time)
					VALUES ($1, $2, $3, $4, $5, $6, $7)
				`,
				host,
				root,
				networkId,
				owner,
				sidecar.DnsCustomHostStateActive,
				sidecar.DnsCustomHostModeWeak,
				now,
			))
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the custom domain's rows: %w", err)
	})
	return
}

// One delegated challenge's receipt as network_tls_custom_challenge holds it (model's tls_custom_challenge_model.go).
type dnsCertsChallengeReceipt struct {
	ChallengeId server.Id
	Domain      string
	ClaimSeq    int64
	// the challenge's owner, <acme_token>.acme.<root>, and the digest it holds
	Name          string
	Value         string
	State         string
	Serial        uint64
	DispatchCount int
	Refusal       string
}

// The network's delegated challenge receipts, by capture order.
func dnsCertsChallengeReceipts(ctx context.Context, networkId server.Id) (receipts []*dnsCertsChallengeReceipt, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT challenge_id, domain, claim_seq, name, value, state, COALESCE(serial, 0), dispatch_count, COALESCE(refusal, '')
					FROM network_tls_custom_challenge
					WHERE network_id = $1
					ORDER BY challenge_id
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					receipt := &dnsCertsChallengeReceipt{}
					var serial int64
					server.Raise(result.Scan(&receipt.ChallengeId, &receipt.Domain, &receipt.ClaimSeq, &receipt.Name, &receipt.Value, &receipt.State, &serial, &receipt.DispatchCount, &receipt.Refusal))
					receipt.Serial = uint64(serial)
					receipts = append(receipts, receipt)
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the challenge receipts: %w", err)
	})
	return
}

// The network's spends of kind in network_tls_custom_spend: each one a custom account's budget took (model's
// tls_account_model.go).
func dnsCertsCustomSpends(ctx context.Context, networkId server.Id, kind string) (count int, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			server.Raise(conn.QueryRow(
				ctx,
				`SELECT count(*) FROM network_tls_custom_spend WHERE network_id = $1 AND kind = $2`,
				networkId,
				kind,
			).Scan(&count))
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the custom spends: %w", err)
	})
	return
}

// The network's custom rows of network_tls_cert with their hosts (model's tls_account_model.go moves them; certs'
// store reads no custom column, so model.NewCertsStore's rows name no host), oldest first: id, sidecar, key, state,
// host, reason and chain.
func dnsCertsCustomCerts(ctx context.Context, networkId server.Id) (rows []*sidecar.TlsCertRecord, returnErr error) {
	server.HandleError(func() {
		server.Db(ctx, func(conn server.PgConn) {
			result, err := conn.Query(
				ctx,
				`
					SELECT cert_id, sidecar_id, spki_sha256, state, custom_host, COALESCE(custom_reason, ''), chain
					FROM network_tls_cert
					WHERE network_id = $1 AND custom_host IS NOT NULL
					ORDER BY cert_id
				`,
				networkId,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var certId, sidecarId server.Id
					row := &sidecar.TlsCertRecord{NetworkId: sidecar.Id(networkId)}
					var state string
					server.Raise(result.Scan(&certId, &sidecarId, &row.SpkiSha256, &state, &row.CustomHost, &row.CustomReason, &row.Chain))
					row.CertId = sidecar.Id(certId)
					row.SidecarId = sidecar.Id(sidecarId)
					row.State = sidecar.TlsCertState(state)
					rows = append(rows, row)
				}
			})
		})
	}, func(err error) {
		returnErr = fmt.Errorf("the custom rows: %w", err)
	})
	return
}

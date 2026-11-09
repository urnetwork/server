package model

// §12.7's system records (DNS-M5-11; R-DNS-1, R-DNS-2, R-DNS-9, D-DNS-6,
// D-SMTP-5, D-SMTP-19): each zone's set is one pure function of its state, so
// every toggle recomputes the whole set rather than patching it, and M7's and
// M8's mail rows join this one place.
//
//   - <net>.<root> at M5, mail off: @ and * MX 0 . (RFC 7505), @ TXT
//     v=spf1 -all, and _dmarc p=reject with strict alignment, never ruf or fo;
//     * A and AAAA, kind edge, only with an edge grant, and @'s only while the
//     apex is published (plan-DNS-edge-record-trigger). * is never a CNAME.
//     _dmarc carries no rua until network_dns_zone.report_token has §2.8's v9
//     row (plan-DNS-rua-token's interim), and p=reject stands without it.
//   - <root> (reading 19): SOA, NS and their glue, CAA per §13.8 with iodef,
//     @ TXT v=spf1 -all, @ MX 10 to the report host, @ A and AAAA kind edge
//     when the apex redirects, _dmarc strict with psd=y once company mail has
//     left, _psl and _spf (reading 16), each from dns.yml's root_records, so
//     no code names a platform host.
//
// A reconcile writes only what differs from the stored rows of the owners and
// types its set manages, through §12.6's one write path, and writes no
// journal row when nothing changed. A user row at a managed key is left, since
// keeping users off system keys is the record API's (DNS-M5-12). Nothing in
// either set counts against dns_entries: system and edge kinds are free (§12.7).

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// A zone whose mail is on, whose records are M7's (DNS-M7-01) and not built
// here, so nothing publishes a mail-off set for a zone that expects mail.
var ErrDnsMailRecordsUnbuilt = errors.New("dns mail records are not built")

// _dmarc's policy for every zone (§12.7, §14.6): strict, never ruf or fo.
const dnsDmarcPolicy = "v=DMARC1; p=reject; sp=reject; np=reject; adkim=s; aspf=s"

// SPF that sends from nowhere (§14.6).
const dnsSpfNone = "v=spf1 -all"

// The local part <root>'s _dmarc reports to, which no 26-character report
// token can equal (plan-DNS-rua-token).
const dnsRootReportLocalPart = "root"

// What a network zone's system set is a function of: network_dns_zone's state.
type DnsZoneState struct {
	// The zone's name: <label>.<root>, fully qualified.
	Apex string
	// * A and AAAA: an edge grant in the zone, or the system publish
	// (plan-DNS-edge-record-trigger).
	EdgeWildcard bool
	// @ A and AAAA: the apex itself is published.
	EdgeApex bool
}

// A TXT row of one string.
func dnsSystemTxt(owner string, text string, ttl uint32) dnsservice.DnsRecordSet {
	return dnsservice.DnsRecordSet{Owner: owner, Type: dns.TypeTXT, Ttl: ttl, Kind: dnsservice.DnsRecordKindSystem, Rdata: []string{`"` + text + `"`}}
}

// A and AAAA rows of kind edge at owner, answered from the healthy interfaces.
func dnsSystemEdgeRows(owner string, ttl uint32) []dnsservice.DnsRecordSet {
	return []dnsservice.DnsRecordSet{
		{Owner: owner, Type: dns.TypeA, Ttl: ttl, Kind: dnsservice.DnsRecordKindEdge},
		{Owner: owner, Type: dns.TypeAAAA, Ttl: ttl, Kind: dnsservice.DnsRecordKindEdge},
	}
}

// §12.7's <net>.<root> set for state, mail off, in owner then type order.
func DnsSystemRecords(state *DnsZoneState, ttl *dnsservice.DnsTtlSettings) []dnsservice.DnsRecordSet {
	apex := dns.CanonicalName(state.Apex)
	wildcard := "*." + apex
	nullMx := []string{"0 ."}
	sets := []dnsservice.DnsRecordSet{
		{Owner: apex, Type: dns.TypeMX, Ttl: ttl.Mx, Kind: dnsservice.DnsRecordKindSystem, Rdata: nullMx},
		dnsSystemTxt(apex, dnsSpfNone, ttl.MailPolicy),
		{Owner: wildcard, Type: dns.TypeMX, Ttl: ttl.Mx, Kind: dnsservice.DnsRecordKindSystem, Rdata: nullMx},
		dnsSystemTxt("_dmarc."+apex, dnsDmarcPolicy, ttl.MailPolicy),
	}
	if state.EdgeApex {
		sets = append(sets, dnsSystemEdgeRows(apex, ttl.Edge)...)
	}
	if state.EdgeWildcard {
		sets = append(sets, dnsSystemEdgeRows(wildcard, ttl.Edge)...)
	}
	sortDnsRecordSets(sets)
	return sets
}

// The keys a network zone's system set manages: what DnsSystemRecords can
// write at @, * and _dmarc.
func dnsZoneSystemKeys(apex string) []dnsservice.DnsRecordKey {
	apex = dns.CanonicalName(apex)
	wildcard := "*." + apex
	return []dnsservice.DnsRecordKey{
		{Owner: apex, Type: dns.TypeMX},
		{Owner: apex, Type: dns.TypeTXT},
		{Owner: apex, Type: dns.TypeA},
		{Owner: apex, Type: dns.TypeAAAA},
		{Owner: wildcard, Type: dns.TypeMX},
		{Owner: wildcard, Type: dns.TypeA},
		{Owner: wildcard, Type: dns.TypeAAAA},
		{Owner: "_dmarc." + apex, Type: dns.TypeTXT},
	}
}

// §12.7's <root> set (reading 19) from dns.yml's root_records, in owner then
// type order. A root with no root_records has none: its edges serve nothing
// until it has an SOA.
func DnsRootRecords(root string, settings *dnsservice.DnsSettings) ([]dnsservice.DnsRecordSet, error) {
	root = dns.CanonicalName(root)
	records := settings.RootRecords[root]
	if records == nil {
		return nil, nil
	}
	ttl := &settings.Ttl
	rname := records.SoaRname
	if rname == "" {
		rname = "hostmaster." + root
	}
	rname = dns.CanonicalName(rname)
	nsNames := []string{}
	for _, ns := range settings.Ns {
		nsNames = append(nsNames, ns+"."+root)
	}
	sets := []dnsservice.DnsRecordSet{
		{
			Owner: root, Type: dns.TypeSOA, Ttl: ttl.Ns, Kind: dnsservice.DnsRecordKindSystem,
			// the edge answers the root's serial in place of the 1, and no secondary reads the timers
			Rdata: []string{fmt.Sprintf("%s %s 1 7200 3600 1209600 %d", nsNames[0], rname, ttl.SoaMinimum)},
		},
		{Owner: root, Type: dns.TypeNS, Ttl: ttl.Ns, Kind: dnsservice.DnsRecordKindSystem, Rdata: nsNames},
		dnsSystemTxt(root, dnsSpfNone, ttl.MailPolicy),
		dnsSystemTxt("_spf."+root, dnsSpfNone, ttl.MailPolicy),
	}
	caa := slices.Clone(records.Caa)
	if len(caa) == 0 {
		caa = []string{`0 issue ";"`, `0 issuewild ";"`}
	}
	if records.Iodef != "" {
		caa = append(caa, fmt.Sprintf(`0 iodef "%s"`, records.Iodef))
	}
	sets = append(sets, dnsservice.DnsRecordSet{Owner: root, Type: dns.TypeCAA, Ttl: ttl.Caa, Kind: dnsservice.DnsRecordKindSystem, Rdata: caa})
	mx := []string{"0 ."}
	dmarc := dnsDmarcPolicy
	if records.ReportMx != "" {
		reportMx := dns.CanonicalName(records.ReportMx)
		mx = []string{"10 " + reportMx}
		dmarc += "; rua=mailto:" + dnsRootReportLocalPart + "@" + strings.TrimSuffix(reportMx, ".")
	}
	if records.CompanyMailLeft {
		dmarc += "; psd=y"
	}
	if err := dnsservice.ValidateDmarcRecord(root, dmarc); err != nil {
		return nil, err
	}
	sets = append(sets,
		dnsservice.DnsRecordSet{Owner: root, Type: dns.TypeMX, Ttl: ttl.Mx, Kind: dnsservice.DnsRecordKindSystem, Rdata: mx},
		dnsSystemTxt("_dmarc."+root, dmarc, ttl.MailPolicy),
	)
	if records.ApexEdge {
		sets = append(sets, dnsSystemEdgeRows(root, ttl.Edge)...)
	}
	if records.Psl != "" {
		sets = append(sets, dnsSystemTxt("_psl."+root, records.Psl, ttl.MailPolicy))
	}
	for _, ns := range settings.Ns {
		ipv4s := []string{}
		ipv6s := []string{}
		for _, text := range records.Glue[ns] {
			address, err := netip.ParseAddr(text)
			if err != nil {
				return nil, fmt.Errorf("glue %q for %s: %w", text, ns, err)
			}
			if address.Unmap().Is4() {
				ipv4s = append(ipv4s, address.Unmap().String())
			} else {
				ipv6s = append(ipv6s, address.String())
			}
		}
		if 0 < len(ipv4s) {
			sets = append(sets, dnsservice.DnsRecordSet{Owner: ns + "." + root, Type: dns.TypeA, Ttl: ttl.Ns, Kind: dnsservice.DnsRecordKindSystem, Rdata: ipv4s})
		}
		if 0 < len(ipv6s) {
			sets = append(sets, dnsservice.DnsRecordSet{Owner: ns + "." + root, Type: dns.TypeAAAA, Ttl: ttl.Ns, Kind: dnsservice.DnsRecordKindSystem, Rdata: ipv6s})
		}
	}
	for index := range sets {
		if err := sets[index].Validate(root); err != nil {
			return nil, err
		}
	}
	sortDnsRecordSets(sets)
	return sets, nil
}

// The keys <root>'s set manages: what DnsRootRecords can write at @, _dmarc,
// _psl, _spf and each NS.
func dnsRootSystemKeys(root string, settings *dnsservice.DnsSettings) []dnsservice.DnsRecordKey {
	root = dns.CanonicalName(root)
	keys := []dnsservice.DnsRecordKey{}
	for _, rrType := range []uint16{dns.TypeSOA, dns.TypeNS, dns.TypeCAA, dns.TypeTXT, dns.TypeMX, dns.TypeA, dns.TypeAAAA} {
		keys = append(keys, dnsservice.DnsRecordKey{Owner: root, Type: rrType})
	}
	for _, owner := range []string{"_dmarc." + root, "_psl." + root, "_spf." + root} {
		keys = append(keys, dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT})
	}
	for _, ns := range settings.Ns {
		keys = append(keys,
			dnsservice.DnsRecordKey{Owner: ns + "." + root, Type: dns.TypeA},
			dnsservice.DnsRecordKey{Owner: ns + "." + root, Type: dns.TypeAAAA},
		)
	}
	return keys
}

// Owner, then type.
func sortDnsRecordSets(sets []dnsservice.DnsRecordSet) {
	slices.SortFunc(sets, func(a dnsservice.DnsRecordSet, b dnsservice.DnsRecordSet) int {
		if order := strings.Compare(a.Owner, b.Owner); order != 0 {
			return order
		}
		return int(a.Type) - int(b.Type)
	})
}

// Whether two rows answer alike.
func dnsRecordSetsEqual(a *dnsservice.DnsRecordSet, b *dnsservice.DnsRecordSet) bool {
	return a.Key() == b.Key() && a.Ttl == b.Ttl && a.Kind == b.Kind && slices.Equal(a.Rdata, b.Rdata)
}

// Writes what desired changes of the zone's managed keys through §12.6's one
// path, or nothing when the stored rows already equal it. A stored user row at
// a managed key is left. Returns nil when nothing was written.
func reconcileDnsSystemRowsInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id, managed []dnsservice.DnsRecordKey, desired []dnsservice.DnsRecordSet) (*DnsWriteResult, error) {
	lockDnsZoneInTx(ctx, tx, root, networkId)
	managedKeys := map[dnsservice.DnsRecordKey]bool{}
	for _, key := range managed {
		managedKeys[key] = true
	}
	stored := map[dnsservice.DnsRecordKey]*dnsStoredRow{}
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		if managedKeys[row.set.Key()] {
			stored[row.set.Key()] = row
		}
	}
	upserts := []dnsservice.DnsRecordSet{}
	wanted := map[dnsservice.DnsRecordKey]bool{}
	for index := range desired {
		set := &desired[index]
		wanted[set.Key()] = true
		row, held := stored[set.Key()]
		switch {
		case held && row.set.Kind == dnsservice.DnsRecordKindUser:
			// the record API's to keep users off system keys
		case held && dnsRecordSetsEqual(&row.set, set):
		default:
			upserts = append(upserts, *set)
		}
	}
	deletes := []dnsservice.DnsRecordKey{}
	for _, key := range managed {
		if row, held := stored[key]; held && !wanted[key] && row.set.Kind != dnsservice.DnsRecordKindUser {
			deletes = append(deletes, key)
		}
	}
	if len(upserts) == 0 && len(deletes) == 0 {
		return nil, nil
	}
	return DnsWriteInTx(ctx, tx, &DnsWriteArgs{Root: root, NetworkId: networkId, Upserts: upserts, Deletes: deletes})
}

// Writes <root>'s own records from settings for every root it lists, each in
// its own transaction, adding each dns_root row first (reading 19). The dns
// API block runs it at start; a changed dns.yml reaches it with the block's
// next start. Returns the first refusal of each root, joined: a restoring root
// waits for the next start.
func DnsReconcileRootRecords(ctx context.Context, settings *dnsservice.DnsSettings) error {
	if !server.SidecarMg5Linked() {
		return ErrDnsUnlinked
	}
	errs := []error{}
	for _, root := range settings.Roots {
		if err := EnsureDnsRoot(ctx, root); err != nil {
			errs = append(errs, err)
			continue
		}
		desired, err := DnsRootRecords(root, settings)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
			continue
		}
		var returnErr error
		err = dnsCatchRefusal(func() {
			server.Tx(ctx, func(tx server.PgTx) {
				_, returnErr = reconcileDnsSystemRowsInTx(ctx, tx, root, server.Id{}, dnsRootSystemKeys(root, settings), desired)
				if returnErr != nil {
					panic(dnsWriteRefusal{err: returnErr})
				}
			}, server.TxReadCommitted)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", root, err))
		}
	}
	return errors.Join(errs...)
}

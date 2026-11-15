package model

// §14.6's DKIM records (DNS-M9-01; R-DNS-9, D-DNS-6, D-DNS-11, I-15, FS-42, FI-12): the record half that
// POST /smtp/dkim-keys, its 7-day rotation and §7.10's removal effect call inside their own transaction
// (SMTP-M9-19, which holds dns_dkim_selectors, rotation and model.DeleteSmtpDkimKeysForClient).
//
//   - A sending sidecar's selector is <sel>._domainkey.<net>.<root> TXT, kind key, free of dns_entries
//     (§12.7), with an admin event for the key write (§12.8).
//   - A replying burner's selector, until the PSL listing (owner decision 79), is <sel>._domainkey.<blabel>
//     under a word-number label. A child makes <blabel> an empty non-terminal the wildcard no longer answers
//     (RFC 4592 §2.2.1), which would end the burner's inbound mail, so <blabel> carries a copy of *'s MX, A
//     and AAAA, written in the same journal write as its first selector and deleted with its last: the copy
//     row §12.6's one write path plans (dnsservice.PlanCopyRows), which dnsedge answers from *'s current
//     records, health drops included, never copied addresses (D-DNS-11). _domainkey.<blabel>, an empty
//     non-terminal too, takes one beside it by the same rule.
//   - These are the only writers allowed a word-number owner: the record API refuses one (§12.7), and the
//     burner writers take nothing else.
//
// A burn changes DNS only when a replying burner's selector retires, and a receive-only burner creates no
// child. §14.6's cut-over after the PSL listing is Later, and needs only DnsDeleteBurnerSelectorInTx. Each
// writer is one journal write in the caller's transaction, which must be read committed, and needs MG2 and
// MG5.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin events of DKIM key writes (§7.11, §12.8): the sending sidecar is the target, or the zone when
// none is named.
const (
	AdminEventActionDnsDkimKey       = "dns.dkim_key"
	AdminEventActionDnsDkimKeyDelete = "dns.dkim_key_delete"
)

// The label every DKIM selector sits under (RFC 6376 §3.6.2.1).
const dnsDkimLabel = "_domainkey"

// A selector, burner label or key a DKIM writer refuses.
var ErrDnsDkimRefused = errors.New("dns dkim record refused")

// Tunables of the DKIM writers.
type DnsDkimSettings struct {
	// The most bytes a key record's text holds: RSA-2048's v=DKIM1; k=rsa; p= is about 410, so twice that
	// with room bounds the parse and never a real key.
	ValueMaxByteCount int
	// The most bytes one TXT character-string holds (RFC 1035 §3.3.14): a longer key is cut into several in
	// one record, which a verifier joins (RFC 6376 §3.6.2.2).
	TxtStringMaxByteCount int
}

func DefaultDnsDkimSettings() *DnsDkimSettings {
	return &DnsDkimSettings{
		ValueMaxByteCount:     2048,
		TxtStringMaxByteCount: 255,
	}
}

// One selector write.
type DnsDkimSelector struct {
	Root      string
	NetworkId server.Id
	// Opaque, one lowercase LDH label (§14.6).
	Selector string
	// A replying burner's word-number label, for DnsPutBurnerSelectorInTx and DnsDeleteBurnerSelectorInTx;
	// empty for a sending sidecar's selector.
	BurnerLabel string
	// The key record's text, v=DKIM1; k=rsa; p=<base64 key> (RFC 6376 §3.6.1). Put only.
	Value string
	// The sending sidecar, the admin event's target; nil names the zone.
	SidecarId *server.Id
	// The admin event's actor (§7.11). Empty is the system's, as a removal effect is.
	ActorKind string
	ActorId   *server.Id
	RequestId *server.Id
	// Seconds; zero takes dns.yml's tlsa, the key records' TTL.
	Ttl uint32
}

// The selector's owner in the network's zone, <sel>._domainkey.<apex> or <sel>._domainkey.<blabel>.<apex>,
// refusing a selector or burner label of any other shape.
func dnsDkimOwner(selector *DnsDkimSelector, apex string, burner bool) (string, error) {
	if !isDnsCtLabel(selector.Selector) {
		return "", fmt.Errorf("%w: selector %q is not one lowercase LDH label", ErrDnsDkimRefused, selector.Selector)
	}
	if !burner {
		if selector.BurnerLabel != "" {
			return "", fmt.Errorf("%w: a sending sidecar's selector names no burner", ErrDnsDkimRefused)
		}
		return selector.Selector + "." + dnsDkimLabel + "." + apex, nil
	}
	if !isDnsCtLabel(selector.BurnerLabel) || !sidecar.IsWordNumberDnsLabel(selector.BurnerLabel) {
		return "", fmt.Errorf("%w: %q is no burner's word-number label", ErrDnsDkimRefused, selector.BurnerLabel)
	}
	return selector.Selector + "." + dnsDkimLabel + "." + selector.BurnerLabel + "." + apex, nil
}

// The key record's text as one TXT record's rdata: printable ASCII with no quote or backslash, v=DKIM1 first
// when it names a version, a p= tag, cut into character-strings of at most the setting's bytes.
func dnsDkimRdata(value string, settings *DnsDkimSettings) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || settings.ValueMaxByteCount < len(value) {
		return "", fmt.Errorf("%w: a key record of %d bytes", ErrDnsDkimRefused, len(value))
	}
	for index := range len(value) {
		if value[index] < 0x20 || 0x7e < value[index] || value[index] == '"' || value[index] == '\\' {
			return "", fmt.Errorf("%w: a key record byte that is no printable text", ErrDnsDkimRefused)
		}
	}
	hasKey := false
	for index, part := range strings.Split(value, ";") {
		name, tagValue, _ := strings.Cut(strings.TrimSpace(part), "=")
		name = strings.TrimSpace(name)
		if name == "v" && (index != 0 || strings.TrimSpace(tagValue) != "DKIM1") {
			return "", fmt.Errorf("%w: v= is not DKIM1 first", ErrDnsDkimRefused)
		}
		if name == "p" && strings.TrimSpace(tagValue) != "" {
			hasKey = true
		}
	}
	if !hasKey {
		return "", fmt.Errorf("%w: no p= key", ErrDnsDkimRefused)
	}
	parts := []string{}
	for 0 < len(value) {
		cut := min(len(value), settings.TxtStringMaxByteCount)
		parts = append(parts, `"`+value[:cut]+`"`)
		value = value[cut:]
	}
	return strings.Join(parts, " "), nil
}

// The write's admin event, or nil for none.
func dnsDkimAdminEvent(selector *DnsDkimSelector, action string) *AdminEvent {
	actorKind := selector.ActorKind
	if actorKind == "" {
		actorKind = AdminEventActorSystem
	}
	event := &AdminEvent{
		NetworkId:  selector.NetworkId,
		ActorKind:  actorKind,
		ActorId:    selector.ActorId,
		Action:     action,
		TargetKind: AdminEventTargetDnsZone,
		Result:     AdminEventResultOk,
		RequestId:  selector.RequestId,
	}
	if selector.SidecarId != nil {
		targetId := *selector.SidecarId
		event.TargetKind = AdminEventTargetSidecar
		event.TargetId = &targetId
	}
	return event
}

// One put or delete through §12.6's path in the network's zone.
func dnsWriteDkimSelectorInTx(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector, burner bool, put bool) (*DnsWriteResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, ErrDnsUnlinked
	}
	if selector == nil || selector.NetworkId == (server.Id{}) {
		return nil, fmt.Errorf("%w: no network", ErrDnsDkimRefused)
	}
	root := dnsLabelRoot(selector.Root)
	label, err := EnsureNetworkLabelInTx(ctx, tx, selector.NetworkId, root)
	if err != nil {
		return nil, err
	}
	owner, err := dnsDkimOwner(selector, label+"."+root, burner)
	if err != nil {
		return nil, err
	}
	key := dnsservice.DnsRecordKey{Owner: owner, Type: dns.TypeTXT}
	args := &DnsWriteArgs{Root: root, NetworkId: selector.NetworkId}
	if put {
		rdata, err := dnsDkimRdata(selector.Value, DefaultDnsDkimSettings())
		if err != nil {
			return nil, err
		}
		ttl := selector.Ttl
		if ttl == 0 {
			ttl = dnsZoneTtl().Tlsa
		}
		args.Upserts = []dnsservice.DnsRecordSet{{Owner: owner, Type: dns.TypeTXT, Ttl: ttl, Kind: dnsservice.DnsRecordKindKey, Rdata: []string{rdata}}}
		args.AdminEvent = dnsDkimAdminEvent(selector, AdminEventActionDnsDkimKey)
	} else {
		args.Deletes = []dnsservice.DnsRecordKey{key}
		args.AdminEvent = dnsDkimAdminEvent(selector, AdminEventActionDnsDkimKeyDelete)
	}
	// the zone's lock, as the zone's writers take it after the label's, before its rows are read
	lockDnsZoneInTx(ctx, tx, root, selector.NetworkId)
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, selector.NetworkId) {
		if row.set.Key() != key {
			continue
		}
		if row.set.Kind != dnsservice.DnsRecordKindKey {
			return nil, fmt.Errorf("%w: %s holds a record of another kind", ErrDnsDkimRefused, owner)
		}
		if put && !row.withheld && slices.Equal(row.set.Rdata, args.Upserts[0].Rdata) && row.set.Ttl == args.Upserts[0].Ttl {
			return nil, nil
		}
	}
	result, err := DnsWriteInTx(ctx, tx, args)
	if errors.Is(err, dnsservice.ErrDnsWriteEmpty) {
		return nil, nil
	}
	return result, err
}

// Publishes a sending sidecar's selector, <sel>._domainkey.<net>.<root>, with its key, in the caller's
// transaction: one journal write with its admin event, none when the selector already holds the key.
// Returns the write, nil when nothing changed.
func DnsPutDkimSelectorInTx(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector) (*DnsWriteResult, error) {
	return dnsWriteDkimSelectorInTx(ctx, tx, selector, false, true)
}

// Withdraws a sending sidecar's selector in the caller's transaction, as a retired selector's 7 days
// ending and §7.10's removal do (I-15). Returns the write, nil when no such selector stood.
func DnsDeleteDkimSelectorInTx(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector) (*DnsWriteResult, error) {
	return dnsWriteDkimSelectorInTx(ctx, tx, selector, false, false)
}

// Publishes a replying burner's selector, <sel>._domainkey.<blabel>.<net>.<root>, in the caller's
// transaction: the burner's first selector writes <blabel>'s copy of *'s records in the same journal write
// (D-DNS-11). Returns the write, nil when the selector already holds the key.
func DnsPutBurnerSelectorInTx(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector) (*DnsWriteResult, error) {
	return dnsWriteDkimSelectorInTx(ctx, tx, selector, true, true)
}

// Withdraws a replying burner's selector in the caller's transaction: its last selector's removal deletes
// <blabel>'s copy in the same journal write, so the label answers from * again. Returns the write, nil when
// no such selector stood.
func DnsDeleteBurnerSelectorInTx(ctx context.Context, tx server.PgTx, selector *DnsDkimSelector) (*DnsWriteResult, error) {
	return dnsWriteDkimSelectorInTx(ctx, tx, selector, true, false)
}

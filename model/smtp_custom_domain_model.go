package model

// §14.6's Team custom domains, their model half (SMTP-M9-34; R-SMTP-12, D-SMTP-15, D-SMTP-5, §20.7, owner decision
// 420, QD-1080, QD-1129): a Team network's verified network_domain (§7.6) as the header From of its mail.
//
//	gate         what smtpedge's send gate reads of a network's custom domains, loaded off every preamble's path with
//	             the rest of its facts (GetSmtpGateFacts): its verified domains; when a network that left the team
//	             tier stops sending From them, NetworkDowngradeTime plus §20.7's 14 days, the downgrade job's own
//	             clock, so mail From is withheld beside the domain's web hosts (QD-840); and the domains in strict
//	             mode (GetSmtpStrictCustomDomains)
//	strict       a verified domain at or above an active strict-mode MX host the network maps (§12.8 step 4; QD-1129),
//	             whose envelope is b.<domain>. The gate's feed and GET /mail/status must name the same set, since an
//	             envelope the edge does not expect fails its part and cuts the connection, so both read it here
//	             (GetSmtpStrictCustomDomains, under DefaultSmtpCustomDomainSettings in both), and MAIL's bounce address
//	             follows it (sidecar mail_bounce.go). Mapping the MX host moves the envelope whether or not mail.strict
//	             is signed yet, since api never reads the config repo (§7.9)
//	selectors    POST /smtp/dkim-keys takes a selector with d=<domain> only for a verified domain outside the root
//	             the gate would send From now (GetSmtpDkimCustomDomains, smtpedgeservice.SmtpCustomDomainSendable),
//	             its record written at <sel>._domainkey.<net>.<root> as the network's own are
//	delegation   one row of kind delegated at the network's label apex names _domainkey.<domain> for each of its
//	             verified domains (DNS-M9-02's DnsSetDelegatedZonesInTx), so dns answers a selector under a
//	             delegated _domainkey.<domain> from the network's own key row and from no other network's. Each
//	             publish naming a custom domain writes it in the publish's transaction, and the transaction that
//	             removes or drops a verified domain withdraws the domain from it
//	             (SmtpWithdrawCustomDomainDelegationInTx), so no delegation outlives its domain's row (§2.8 dns-1's
//	             ttl, QD-1080), and another network that verifies the domain next is not left answering for neither
//
// A per-selector CNAME needs nothing here: it names the ordinary <sel>._domainkey.<net>.<root> row. Nothing here
// reads or writes a table its group does not link, and no domain reaches a log or an admin event (I-13).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// Tunables of custom-domain mail.
type SmtpCustomDomainSettings struct {
	// How long after its downgrade a network that left the team tier still sends From its verified domains: §20.7's
	// 14 days, the downgrade job's own grace (NetworkDowngradeSettings.DowngradeGrace), so its mail From and its
	// web hosts are withheld together (owner decision 420).
	DowngradeGrace time.Duration
	// Whether strict mode's envelope is on: the gate's feed and GET /mail/status name the strict domains
	// (QD-1129). On, since MAIL's bounce address follows the set GET /mail/status names. Both read it under the
	// same settings, so turning it off empties the set at the edge and at the sender together, and every custom
	// domain keeps b.<net>.<root>.
	StrictEnvelope bool
}

func DefaultSmtpCustomDomainSettings() *SmtpCustomDomainSettings {
	return &SmtpCustomDomainSettings{
		DowngradeGrace: DefaultNetworkDowngradeSettings().DowngradeGrace,
		StrictEnvelope: true,
	}
}

// The tier and the downgrade clock the custom-domain rows read, which tests replace.
var (
	smtpCustomDomainTierOf          = NetworkTierOf
	smtpCustomDomainDowngradeTimeOf = NetworkDowngradeTime
)

// When the network stops sending From its verified domains off the team tier: its downgrade time plus the grace.
// Zero while it has left no tier, which off the team tier sends From none (smtpedgeservice.SmtpCustomDomainSendable).
func GetSmtpCustomDomainWithholdTime(ctx context.Context, networkId server.Id, settings *SmtpCustomDomainSettings) time.Time {
	downgradeTime, _, left := smtpCustomDomainDowngradeTimeOf(ctx, networkId)
	if !left {
		return time.Time{}
	}
	return downgradeTime.Add(settings.DowngradeGrace)
}

// Whether domain, lowercase with no final dot, is at or below root, which a custom domain never is.
func smtpCustomDomainUnderRoot(domain string, root string) bool {
	root = strings.ToLower(strings.TrimSuffix(root, "."))
	return root != "" && (domain == root || strings.HasSuffix(domain, "."+root))
}

// The verified domains a strict-mode MX host stands at or below (QD-1129), in domains' order: hosts are the hosts of
// the network's active strict-mode MX rows.
func smtpStrictCustomDomains(domains []string, hosts []string) []string {
	strict := []string{}
	for _, domain := range domains {
		if slices.ContainsFunc(hosts, func(host string) bool { return host == domain || strings.HasSuffix(host, "."+domain) }) {
			strict = append(strict, domain)
		}
	}
	return strict
}

// The network's verified domains in strict mode (see the file header), the set the gate's feed and GET /mail/status
// both name. Empty while settings.StrictEnvelope is off, and in a binary that does not link MG6.
func GetSmtpStrictCustomDomains(ctx context.Context, networkId server.Id, settings *SmtpCustomDomainSettings) []string {
	if !settings.StrictEnvelope {
		return []string{}
	}
	hosts := []string{}
	for _, row := range DnsStrictMxHostRows(ctx, networkId) {
		if row.State == sidecar.DnsCustomHostStateActive {
			hosts = append(hosts, strings.ToLower(strings.TrimSuffix(row.Host, ".")))
		}
	}
	return smtpStrictCustomDomains(GetNetworkVerifiedDomains(ctx, networkId), hosts)
}

// Fills facts' custom-domain fields for the network: its verified domains, their withhold time and the strict ones.
func fillSmtpGateCustomDomainFacts(ctx context.Context, networkId server.Id, facts *smtpedgeservice.SmtpGateFacts, settings *SmtpCustomDomainSettings) {
	facts.VerifiedDomains = GetNetworkVerifiedDomains(ctx, networkId)
	facts.StrictDomains = GetSmtpStrictCustomDomains(ctx, networkId, settings)
	facts.CustomDomainWithholdTime = GetSmtpCustomDomainWithholdTime(ctx, networkId, settings)
}

// The custom domains POST /smtp/dkim-keys takes a selector under for the network at now: its verified domains
// outside root that the gate would send From (smtpedgeservice.SmtpCustomDomainSendable), in name order. Read
// before the route's transaction, on connections of its own.
func GetSmtpDkimCustomDomains(ctx context.Context, networkId server.Id, root string, now time.Time, settings *SmtpCustomDomainSettings) []string {
	domains := []string{}
	verifiedDomains := GetNetworkVerifiedDomains(ctx, networkId)
	if len(verifiedDomains) == 0 {
		return domains
	}
	tier := string(smtpCustomDomainTierOf(ctx, networkId))
	withholdTime := GetSmtpCustomDomainWithholdTime(ctx, networkId, settings)
	if !smtpedgeservice.SmtpCustomDomainSendable(tier, withholdTime, now) {
		return domains
	}
	for _, domain := range verifiedDomains {
		if !smtpCustomDomainUnderRoot(domain, root) {
			domains = append(domains, domain)
		}
	}
	return domains
}

// The network's verified domains whose _domainkey apex its delegated row in root can name, read in tx: those outside
// root and every root and reverse zone dns.yml names (dnsservice.DnsDelegatedApex), in name order.
func getSmtpDelegatedCustomDomainsInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) []string {
	domains := []string{}
	for _, domain := range getNetworkVerifiedDomainsInTx(ctx, tx, networkId) {
		if smtpCustomDomainUnderRoot(domain, root) {
			continue
		}
		if _, err := dnsservice.DnsDelegatedApex(dnsZoneSettings(), domain); err == nil {
			domains = append(domains, domain)
		}
	}
	return domains
}

// Writes the network's delegated row in root to its verified domains, in tx, the caller's, which must be read
// committed (see the file header's delegation row). Returns the write, nil when nothing changed, or the dns write
// path's refusal, nothing written.
func setSmtpCustomDomainDelegationsInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) (*DnsWriteResult, error) {
	return DnsSetDelegatedZonesInTx(ctx, tx, root, networkId, getSmtpDelegatedCustomDomainsInTx(ctx, tx, root, networkId))
}

// Withdraws from the network's delegated row any domain no longer among its verified ones, in tx, the transaction
// that removed or dropped one, ahead of any change there to the custom hosts' map, since a zone write takes its
// zone's lock before dns_root's. A network with no label in the user root holds no row, and nothing is written for
// it. A zone that takes no write now, suspended or its root restoring, keeps its row until the network's next
// publish or removal writes it, and the removal still commits; a suspended zone's rows are served by no edge
// meanwhile. Answers whether the row changed.
func SmtpWithdrawCustomDomainDelegationInTx(ctx context.Context, tx server.PgTx, networkId server.Id) bool {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg6Linked() {
		return false
	}
	root := DnsUserRoot()
	if root == "" {
		return false
	}
	root = dnsLabelRoot(root)
	lockDnsLabelInTx(ctx, tx, networkId)
	if getNetworkLabelInTx(ctx, tx, networkId, root) == "" {
		return false
	}
	result, err := setSmtpCustomDomainDelegationsInTx(ctx, tx, root, networkId)
	if err != nil {
		if !errors.Is(err, ErrDnsZoneSuspended) && !errors.Is(err, dnsservice.ErrDnsRestoring) {
			glog.Errorf("[smtp]%s delegated _domainkey zones not withdrawn: %v\n", networkId, err)
		}
		return false
	}
	return result != nil
}

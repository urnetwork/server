package model

// §21.8's third signal row and §19.8's notice class (ABUSE-M5-10; D-ABUSE-3,
// D-ABUSE-5, owner decisions 134, 143, 530): which notices page the rota at
// any hour, who handles each within the hour, and how a role mail is told to
// be one. Pure: nothing here reads a store, a clock or a message.
//
// Only these classes page. A notice reaches one of them two ways and no other
// (owner decision 530): a person files it through the staff route, having read
// it in the company mailbox off the platform, or a mail to abuse@ or
// postmaster@ carries a DKIM signature that verifies under a domain abuse.yml's
// notice_senders lists (ClassifyAbuseNotice). The public form offers no such
// class, so a forged notice pages nothing, and no class here kills anything: a
// host a registrar's or registry's URL names is the rota's to kill within the
// same hour (D-ABUSE-3). A Search Console flag is no notice and waits for
// business hours (§21.17).

import (
	"github.com/urnetwork/server"
)

// A kind of notice that pages, as the case keeps it in its join key.
type AbuseNoticeClass string

const (
	// no notice: nothing pages
	AbuseNoticeClassNone AbuseNoticeClass = ""
	// any registrar or registry notice about <root> or bringyour.com
	AbuseNoticeClassRegistrar AbuseNoticeClass = "registrar"
	AbuseNoticeClassRegistry  AbuseNoticeClass = "registry"
	// a law-enforcement emergency notice that is neither of the two below
	AbuseNoticeClassLawEnforcementEmergency AbuseNoticeClass = "law_enforcement_emergency"
	AbuseNoticeClassEuRemovalOrder          AbuseNoticeClass = "eu_removal_order"
	AbuseNoticeClassProductionOrder         AbuseNoticeClass = "production_order"
)

// Who handles a paged notice within the hour (§21.8).
type AbuseNoticeRoute string

const (
	// removal orders and registrar or registry notices stay with the rota
	AbuseNoticeRouteRota AbuseNoticeRoute = "rota"
	// production orders and other data requests go to abuse.yml's
	// counsel_contact
	AbuseNoticeRouteCounsel AbuseNoticeRoute = "counsel"
)

// Whether the class pages. Every class but none does, and nothing else does.
func (self AbuseNoticeClass) Pages() bool {
	switch self {
	case AbuseNoticeClassRegistrar, AbuseNoticeClassRegistry,
		AbuseNoticeClassLawEnforcementEmergency, AbuseNoticeClassEuRemovalOrder, AbuseNoticeClassProductionOrder:
		return true
	}
	return false
}

// The §21.8 row the class sits on, whose clock is the paged hour. A class that
// does not page sits on the report row.
func (self AbuseNoticeClass) Signal() AbuseSignal {
	switch self {
	case AbuseNoticeClassRegistrar:
		return AbuseSignalRegistrarNotice
	case AbuseNoticeClassRegistry:
		return AbuseSignalRegistryNotice
	case AbuseNoticeClassLawEnforcementEmergency:
		return AbuseSignalLawEnforcementEmergency
	case AbuseNoticeClassEuRemovalOrder:
		return AbuseSignalEuRemovalOrder
	case AbuseNoticeClassProductionOrder:
		return AbuseSignalProductionOrder
	}
	return AbuseSignalReport
}

// Who the notice came from, as its case's reporter class.
func (self AbuseNoticeClass) ReporterClass() AbuseReporterClass {
	switch self {
	case AbuseNoticeClassRegistrar:
		return AbuseReporterClassRegistrar
	case AbuseNoticeClassRegistry:
		return AbuseReporterClassRegistry
	}
	return AbuseReporterClassLawEnforcement
}

// Who handles the class. A law-enforcement emergency notice whose kind the
// filer could not tell goes to counsel, as a data request does: the rota still
// reads it first, since both people are paged for every class.
func (self AbuseNoticeClass) Route() AbuseNoticeRoute {
	switch self {
	case AbuseNoticeClassLawEnforcementEmergency, AbuseNoticeClassProductionOrder:
		return AbuseNoticeRouteCounsel
	}
	return AbuseNoticeRouteRota
}

// The role addresses whose mail may be a notice, by local part (RFC 2142).
const (
	abuseNoticeRoleAbuse      = "abuse"
	abuseNoticeRolePostmaster = "postmaster"
)

// One DKIM signature of a role mail, as the listener's verifier judged it.
type AbuseNoticeSignature struct {
	// the signature's d=
	Domain string
	// the signature verified against the key its d= and s= name
	Verified bool
}

// What the classifier reads of a role mail. There is no field for a body, a
// subject, a From or a sender address, so none can decide a page: a From is
// whatever the sender typed, and a body is never read (owner decision 78).
type AbuseNoticeMail struct {
	// the recipient's local part, as the listener accepted it
	RecipientLocalPart string
	Signatures         []AbuseNoticeSignature
}

// abuse.yml's notice_senders, as server.AbuseConfig answers it: a listed
// domain's class word, or "" for an unlisted one.
type AbuseNoticeSenders interface {
	NoticeSenderClass(domain string) string
}

// The class of a role mail, or none (owner decision 530): a mail to abuse@ or
// postmaster@ with a signature that verified under a listed domain takes that
// domain's class, and anything else is none. A signature's d= must equal the
// listed domain, never merely sit under it, so listing a registrar does not
// list every name the registrar's customers can sign for. A listed
// law-enforcement sender is an emergency notice: which order it is, if any, is
// a person's to read. Several listed signers answer in the order law
// enforcement, registry, registrar, so the answer does not depend on header
// order.
func ClassifyAbuseNotice(mail *AbuseNoticeMail, senders AbuseNoticeSenders) AbuseNoticeClass {
	if mail == nil || senders == nil {
		return AbuseNoticeClassNone
	}
	switch abuseName(mail.RecipientLocalPart) {
	case abuseNoticeRoleAbuse, abuseNoticeRolePostmaster:
	default:
		return AbuseNoticeClassNone
	}
	found := map[AbuseNoticeClass]bool{}
	for _, signature := range mail.Signatures {
		if !signature.Verified {
			continue
		}
		domain := abuseName(signature.Domain)
		if domain == "" {
			continue
		}
		switch senders.NoticeSenderClass(domain) {
		case server.AbuseNoticeSenderLawEnforcement:
			found[AbuseNoticeClassLawEnforcementEmergency] = true
		case server.AbuseNoticeSenderRegistry:
			found[AbuseNoticeClassRegistry] = true
		case server.AbuseNoticeSenderRegistrar:
			found[AbuseNoticeClassRegistrar] = true
		}
	}
	for _, class := range []AbuseNoticeClass{
		AbuseNoticeClassLawEnforcementEmergency, AbuseNoticeClassRegistry, AbuseNoticeClassRegistrar,
	} {
		if found[class] {
			return class
		}
	}
	return AbuseNoticeClassNone
}

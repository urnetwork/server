package model

// What §21.5's breakers read, gathered into one sample (ABUSE-M9-03; D-ABUSE-2, R-ABUSE-4, I-13, I-18): <root>'s
// Postmaster spam rate by day, the feedback-loop complaint rate from the store, and which of URnetwork's own sending
// addresses a listing or SNDS holds against it. model.NextMailBreakerState reads the sample; this file decides
// nothing.
//
// Every figure is known or unknown, never zero for want of an answer, since an unknown measure holds a breaker and
// a false zero would clear one:
//
//	Postmaster rate   the latest day inside PostmasterMaxLagDays that Postmaster Tools reports, then each day before
//	                  it for the run §21.5 asks; no reported day inside the lag is no rate at all, and a day inside
//	                  the run it does not report is unknown. It reports a day late and may leave a subdomain's mail
//	                  out (§14.15), which is why the complaint rate stands beside it.
//	complaint rate    MailComplaintRate, unknown when the binary lacks either table, when nothing was sent in the
//	                  window, and when the store did not answer
//	listed addresses  Spamhaus SBL and CSS from the local mirror, and SNDS red. Known only when the sending
//	                  addresses are known and both sources answered; an address a source that did answer holds
//	                  counts either way.
//
// The figures are aggregate: no value here names a network, a client, a recipient or a message, and nothing here
// writes a row (I-13). The sources' credentials are external, and with none a source answers unknown.

import (
	"context"
	"net/netip"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// One day's spam rate of a domain: server.PostmasterToolsClient.
type MailPostmasterSource interface {
	SpamRate(ctx context.Context, domain string, day time.Time) (partsPerMillion int64, known bool)
}

// The sending addresses SNDS colors red: server.SndsClient.
type MailSndsSource interface {
	RedAddresses(ctx context.Context, addresses []netip.Addr) (red []netip.Addr, known bool)
}

// The sending addresses a listing holds: server.SpamhausListingClient.
type MailListingSource interface {
	ListedAddresses(ctx context.Context, addresses []netip.Addr, now time.Time) (listed []netip.Addr, known bool)
}

// Where one sample comes from. A nil source answers unknown.
type MailBreakerSources struct {
	// <root>, the one domain asked of Postmaster Tools. Empty asks nothing.
	Root string
	// URnetwork's own shared sending addresses, from smtpedge.yml: the only addresses asked about anywhere. Empty
	// leaves the listing unknown.
	SendingAddresses []netip.Addr
	Postmaster       MailPostmasterSource
	Snds             MailSndsSource
	Listing          MailListingSource
}

// One sample at now (see the file header). sources nil gathers the complaint rate alone.
func GatherMailBreakerMeasures(ctx context.Context, sources *MailBreakerSources, now time.Time, settings *MailBreakerSettings) *MailBreakerMeasures {
	measures := gatherMailSourceMeasures(ctx, sources, now, settings)
	measures.ComplaintRate = MailComplaintRate(ctx, now, settings)
	return measures
}

// The part of a sample the outside sources give: everything but the complaint rate, which is the store's.
func gatherMailSourceMeasures(ctx context.Context, sources *MailBreakerSources, now time.Time, settings *MailBreakerSettings) *MailBreakerMeasures {
	if sources == nil {
		sources = &MailBreakerSources{}
	}
	measures := &MailBreakerMeasures{
		PostmasterRates: mailPostmasterRates(ctx, sources, now, settings),
		ListedAddresses: []netip.Addr{},
	}

	addresses := mailBreakerAddresses(sources.SendingAddresses)
	listingKnown := 0 < len(addresses)
	if sources.Listing != nil && 0 < len(addresses) {
		listed, known := sources.Listing.ListedAddresses(ctx, addresses, now)
		listingKnown = listingKnown && known
		measures.ListedAddresses = append(measures.ListedAddresses, mailOwnAddresses(listed, addresses)...)
	} else {
		listingKnown = false
	}
	if sources.Snds != nil && 0 < len(addresses) {
		red, known := sources.Snds.RedAddresses(ctx, addresses)
		listingKnown = listingKnown && known
		measures.ListedAddresses = append(measures.ListedAddresses, mailOwnAddresses(red, addresses)...)
	} else {
		listingKnown = false
	}
	measures.ListingKnown = listingKnown
	measures.ListedAddresses = mailBreakerAddresses(measures.ListedAddresses)
	return measures
}

// Of answered, the addresses that are the platform's own: a source can list only what it was asked about.
func mailOwnAddresses(answered []netip.Addr, own []netip.Addr) []netip.Addr {
	kept := []netip.Addr{}
	for _, address := range mailBreakerAddresses(answered) {
		for _, ownAddress := range own {
			if address == ownAddress {
				kept = append(kept, address)
			}
		}
	}
	return kept
}

// <root>'s Postmaster rates, newest first: the latest reported day inside the lag, then the days before it up to
// the run. Empty when no day inside the lag is reported.
func mailPostmasterRates(ctx context.Context, sources *MailBreakerSources, now time.Time, settings *MailBreakerSettings) []MailRate {
	rates := []MailRate{}
	if sources.Postmaster == nil || sources.Root == "" {
		return rates
	}
	today := now.UTC().Truncate(24 * time.Hour)
	for lag := 0; lag <= settings.PostmasterMaxLagDays; lag += 1 {
		latestDay := today.AddDate(0, 0, -lag)
		partsPerMillion, known := sources.Postmaster.SpamRate(ctx, sources.Root, latestDay)
		if !known {
			continue
		}
		rates = append(rates, KnownMailRate(partsPerMillion))
		for before := 1; before < settings.PostmasterRunDays; before += 1 {
			partsPerMillion, known := sources.Postmaster.SpamRate(ctx, sources.Root, latestDay.AddDate(0, 0, -before))
			if known {
				rates = append(rates, KnownMailRate(partsPerMillion))
			} else {
				rates = append(rates, MailRate{})
			}
		}
		return rates
	}
	return rates
}

// §21.5's feedback-loop complaint rate over the ComplaintWindow ending at now: complaints over recipients, in parts
// per million, across every network together.
//
//	complaints   smtp_complaint rows that arrived in the window with a join at high or medium confidence and a type
//	             §14.8 counts (abuse, fraud, virus, other), each (network, Message-ID hash) once, as §14.8 counts
//	             them (smtpedgeservice's smtpCountedComplaints), so two reporters of one message are one complaint,
//	             and none whose hash a not-spam in the window names. A low-confidence join, a not-spam, an
//	             authentication failure and an aggregate report count nothing; a complaint with no hash counts
//	             each.
//	recipients   smtp_send's recipient_count over sends that started in the window, each message once: a retry and
//	             the send it retries are one message, counted at the most recipients either reached. Recipients,
//	             not connections.
//
// Unknown when the binary links no MG5 or no MG9, when no recipient was sent to in the window, since nothing over
// nothing is no rate, and when the store did not answer.
func MailComplaintRate(ctx context.Context, now time.Time, settings *MailBreakerSettings) (rate MailRate) {
	if !server.SidecarMg5Linked() || !server.SidecarMg9Linked() || settings.ComplaintWindow <= 0 {
		return MailRate{}
	}
	defer func() {
		// the store raises a failed read
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			rate = MailRate{}
			if glog.V(1) {
				glog.Infof("[abuse]the complaint rate did not read: %v\n", r)
			}
		}
	}()
	until := now.UTC()
	since := until.Add(-settings.ComplaintWindow)
	var complaintCount int64
	var recipientCount int64
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COUNT(*)
				FROM (
					SELECT DISTINCT
						smtp_complaint.network_id,
						COALESCE(smtp_complaint.message_id_hash, decode(replace(smtp_complaint.complaint_id::text, '-', ''), 'hex'))
					FROM smtp_complaint
					WHERE
						$1 <= smtp_complaint.arrival_time AND
						smtp_complaint.arrival_time < $2 AND
						smtp_complaint.join_result IN ($3, $4) AND
						smtp_complaint.complaint_type IN ($5, $6, $7, $8) AND
						NOT EXISTS (
							SELECT 1
							FROM smtp_complaint not_spam
							WHERE
								not_spam.network_id = smtp_complaint.network_id AND
								not_spam.message_id_hash = smtp_complaint.message_id_hash AND
								not_spam.complaint_type = $9 AND
								$1 <= not_spam.arrival_time AND
								not_spam.arrival_time < $2
						)
				) counted
			`,
			since,
			until,
			string(smtpedgeservice.SmtpConfidenceHigh),
			string(smtpedgeservice.SmtpConfidenceMedium),
			smtpedgeservice.SmtpComplaintTypeAbuse,
			smtpedgeservice.SmtpComplaintTypeFraud,
			smtpedgeservice.SmtpComplaintTypeVirus,
			smtpedgeservice.SmtpComplaintTypeOther,
			smtpedgeservice.SmtpComplaintTypeNotSpam,
		).Scan(&complaintCount))
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT COALESCE(SUM(message.recipient_count), 0)::bigint
				FROM (
					SELECT MAX(smtp_send.recipient_count) AS recipient_count
					FROM smtp_send
					WHERE
						$1 <= smtp_send.start_time AND
						smtp_send.start_time < $2 AND
						$3 <= smtp_send.send_day AND
						smtp_send.send_day <= $4
					GROUP BY COALESCE(smtp_send.retry_of_send_id, smtp_send.send_id)
				) message
			`,
			since,
			until,
			smtpUtcDay(since),
			smtpUtcDay(until),
		).Scan(&recipientCount))
	})
	return mailRateOf(complaintCount, recipientCount)
}

// count over total in parts per million, rounded up so that a rate at a threshold never reads under it; unknown
// with no total.
func mailRateOf(count int64, total int64) MailRate {
	if total <= 0 || count < 0 {
		return MailRate{}
	}
	const partsPerMillion = 1000 * 1000
	if total < count {
		// more complaints than recipients in the window, which a window's edge allows
		return KnownMailRate(partsPerMillion)
	}
	return KnownMailRate((count*partsPerMillion + total - 1) / total)
}

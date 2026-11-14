package model

// The gift rails (mmm/GIFTOFPRIVACY.md §9.2, §10.2): a gift's note and
// signature carry no link, and one buyer has at most
// GiftFriendEmailDailyLimit friend emails sent in any 24 hours.

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

// GiftTextHasLink reports whether a gift's note or signature holds anything an
// email client would turn into a link. The site mirrors this rule exactly
// (GIFTOFPRIVACY.md §10.2, "Links in the note"). On the text with ASCII A-Z
// lowered (nothing else is case-folded), it is a link when:
//
//  1. a URL scheme: a letter, then letters, digits, "+", "." or "-", then
//     "://" (https://, ftp://, any scheme://);
//  2. "mailto:" anywhere;
//  3. "www." at the start of the text or after a character that is not a
//     letter, mark or digit, followed by a letter, mark or digit;
//  4. a domain: a run of two or more labels, each one or more letters, marks,
//     digits or "-", joined by single dots, in which a label after the first,
//     its leading and trailing "-" trimmed, is a TLD of giftLinkTlds; or whose
//     last label so trimmed is a TLD of giftLinkWordTlds and the run is
//     followed directly by "/" or preceded directly by "@" (t.me/x, amzn.to/x,
//     sam@web.de, but not "you.me" or "mucho.es" in a sentence missing its
//     space). The run takes as many labels as it can, so a sentence's last
//     dot, and any other punctuation, ends it.
//
// Letters, marks and digits are Unicode's (\p{L}, \p{M}, \p{N}), so a domain
// whose labels are in another script (例子.com) is a link; a TLD in another
// script (.рф) is in no list and is not. Only "." separates labels.
func GiftTextHasLink(text string) bool {
	lowered := giftLowerAscii(text)
	if giftLinkSchemePattern.MatchString(lowered) {
		return true
	}
	if strings.Contains(lowered, "mailto:") {
		return true
	}
	if giftLinkWwwPattern.MatchString(lowered) {
		return true
	}
	for _, span := range giftLinkDomainPattern.FindAllStringIndex(lowered, -1) {
		start, end := span[0], span[1]
		labels := strings.Split(lowered[start:end], ".")
		for i := 1; i < len(labels); i += 1 {
			label := strings.Trim(labels[i], "-")
			if giftLinkTlds[label] {
				return true
			}
			if i == len(labels)-1 && giftLinkWordTlds[label] {
				followedBySlash := end < len(lowered) && lowered[end] == '/'
				precededByAt := 0 < start && lowered[start-1] == '@'
				if followedBySlash || precededByAt {
					return true
				}
			}
		}
	}
	return false
}

// The rule's patterns, on text with ASCII A-Z lowered (GiftTextHasLink).
var (
	giftLinkSchemePattern = regexp.MustCompile(`[a-z][a-z0-9+.\-]*://`)
	giftLinkWwwPattern    = regexp.MustCompile(`(?:^|[^\p{L}\p{M}\p{N}])www\.[\p{L}\p{M}\p{N}]`)
	giftLinkDomainPattern = regexp.MustCompile(`[\p{L}\p{M}\p{N}\-]+(?:\.[\p{L}\p{M}\p{N}\-]+)+`)
)

// The TLDs that make a domain a link wherever they end one of its labels after
// the first: common generic ones, the ones shorteners and spam favor, and the
// common country codes that are not words in the site's languages.
var giftLinkTlds = giftLinkTldSet(
	// generic
	"com", "net", "org", "info", "biz", "edu", "gov", "app", "dev", "io", "ai",
	"co", "xyz", "tv", "cc", "eu", "asia",
	// shorteners and favored by spam
	"ly", "gg", "gl", "gd", "sh", "ws", "icu", "vip", "cfd", "sbs", "cyou",
	"tk", "ml", "ga", "cf", "gq",
	// country codes
	"uk", "ca", "au", "nz", "ie", "fr", "nl", "ch", "dk", "fi", "pl", "cz",
	"sk", "hu", "ro", "bg", "gr", "pt", "ru", "ua", "kz", "cn", "hk", "tw",
	"jp", "kr", "sg", "ph", "vn", "pk", "bd", "ae", "sa", "il", "tr", "eg",
	"za", "ng", "ke", "br", "ar", "cl", "pe", "mx",
)

// The TLDs that are also words (country codes that are words in English,
// Spanish or German, and generic TLDs that are English words): they make a
// domain a link only directly before "/" or directly after "@", so a sentence
// that lost the space after its period is not one.
var giftLinkWordTlds = giftLinkTldSet(
	"al", "am", "as", "at", "be", "by", "de", "do", "er", "es", "im", "in",
	"ir", "is", "it", "la", "me", "my", "no", "se", "si", "so", "su", "th",
	"to", "us", "ve",
	"link", "site", "online", "store", "shop", "top", "club", "live", "click",
	"tech", "cloud", "page", "blog", "buzz", "bond", "fun", "one", "win",
	"world", "life", "love", "gift",
)

func giftLinkTldSet(tlds ...string) map[string]bool {
	set := make(map[string]bool, len(tlds))
	for _, tld := range tlds {
		set[tld] = true
	}
	return set
}

// The text with ASCII A-Z lowered and every other character as it is, so the
// rule reads the same in any language's case folding.
func giftLowerAscii(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for i := 0; i < len(text); i += 1 {
		c := text[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// GiftFriendEmailDailyLimit is the most friend emails one buyer (their email,
// case-insensitive) has sent in any 24 hours: the recipients of their
// delivery "them" orders made in the last 24 hours that are pending or paid.
// Cards delivered to the buyer ("me") are not counted.
const GiftFriendEmailDailyLimit = 20

// GiftFriendEmailWindow is the window GiftFriendEmailDailyLimit counts over.
const GiftFriendEmailWindow = 24 * time.Hour

// GetGiftFriendEmailCount is the number of friend emails the buyer's orders
// owe: the recipients of their delivery "them" orders made after since that
// are pending or paid, the buyer's email compared case-insensitively.
func GetGiftFriendEmailCount(ctx context.Context, buyerEmail string, since time.Time) (count int) {
	server.Db(ctx, func(conn server.PgConn) {
		server.Raise(conn.QueryRow(
			ctx,
			`
				SELECT count(*)
				FROM gift_order
				INNER JOIN gift_order_recipient
					ON gift_order_recipient.gift_order_id = gift_order.gift_order_id
				WHERE
					lower(gift_order.buyer_email) = lower($1) AND
					gift_order.delivery = $2 AND
					gift_order.status = ANY($3) AND
					$4 < gift_order.create_time
			`,
			strings.TrimSpace(buyerEmail),
			GiftDeliveryThem,
			[]string{GiftOrderStatusPending, GiftOrderStatusPaid},
			since,
		).Scan(&count))
	})
	return
}

// RemoveUnstartedGiftOrder removes an order that was just stored and never
// started: still pending, with no Checkout session, invoice or payment, as a
// refusal does after storing it. false when the order is not such a one.
func RemoveUnstartedGiftOrder(ctx context.Context, giftOrderId server.Id) (removed bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		removed = false
		var unstarted bool
		result, err := tx.Query(
			ctx,
			`
				SELECT
					status = $2 AND
					stripe_session_id IS NULL AND
					crypto_intent_id IS NULL AND
					purchase_event_base IS NULL
				FROM gift_order
				WHERE gift_order_id = $1
				FOR UPDATE
			`,
			giftOrderId,
			GiftOrderStatusPending,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&unstarted))
			}
		})
		if !unstarted {
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM gift_order_recipient WHERE gift_order_id = $1`,
			giftOrderId,
		))
		server.RaisePgResult(tx.Exec(
			ctx,
			`DELETE FROM gift_order WHERE gift_order_id = $1`,
			giftOrderId,
		))
		removed = true
	})
	return
}

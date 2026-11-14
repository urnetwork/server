package controller

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/model"
)

// A paid order of the language, delivery and size, with its codes.
func giftEmailTestOrder(language string, delivery string, quantity int, signature string, bittensor bool) (*model.GiftOrder, []*model.GiftCode) {
	paidTime := time.Date(2026, 11, 20, 18, 0, 0, 0, time.UTC)
	order := &model.GiftOrder{
		GiftOrderId:     server.NewId(),
		Status:          model.GiftOrderStatusPaid,
		PaidTime:        &paidTime,
		Quantity:        quantity,
		Delivery:        delivery,
		BuyerEmail:      "giver@example.invalid",
		SendTimezone:    "America/Chicago",
		Message:         "Thinking of you\nalways",
		Signature:       signature,
		Language:        language,
		Season:          "winter-2026",
		BittensorFooter: bittensor,
		PdfReady:        true,
	}
	codes := []*model.GiftCode{}
	for i := range quantity {
		code := &model.GiftCode{
			BalanceCodeId: server.NewId(),
			Secret:        "K7QX3M2PNB4DLZ8R9YWC5AGHT" + string(rune('A'+i)),
			RedeemBy:      model.GiftRedeemBy(paidTime, 8760*time.Hour, model.GiftOrderLocation("America/Chicago")),
		}
		if delivery == model.GiftDeliveryThem {
			recipientIndex := i
			code.RecipientIndex = &recipientIndex
			order.Recipients = append(order.Recipients, "friend"+string(rune('a'+i))+"@example.invalid")
		}
		codes = append(codes, code)
	}
	if delivery == model.GiftDeliveryThem {
		sendOn := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
		order.SendOn = &sendOn
	}
	return order, codes
}

var giftEmailTestPdf = []byte("%PDF-1.7\nsynthetic gift card\n%%EOF\n")

// The buyer's email of the order, as loaded when sent.
func giftBuyerTestTemplate(order *model.GiftOrder, codes []*model.GiftCode) *GiftCardsBuyerTemplate {
	return &GiftCardsBuyerTemplate{
		GiftOrderId: order.GiftOrderId,
		giftEmailOrder: giftEmailOrder{
			order: order,
			codes: giftOrderCards(order, codes),
			pdf:   giftEmailTestPdf,
		},
	}
}

// Friend recipientIndex's email of the order, as loaded when sent.
func giftFriendTestTemplate(order *model.GiftOrder, codes []*model.GiftCode, recipientIndex int) *GiftCardFriendTemplate {
	return &GiftCardFriendTemplate{
		GiftOrderId:    order.GiftOrderId,
		RecipientIndex: recipientIndex,
		giftEmailOrder: giftEmailOrder{
			order: order,
			codes: giftOrderCards(order, codes),
			pdf:   giftEmailTestPdf,
		},
		code: codes[recipientIndex],
	}
}

var giftCardTestPlaceholder = regexp.MustCompile(`\{[a-z]+\}`)

// Every string has the six languages, none empty, each with the same
// placeholders as the English; every count form has its pair.
func TestGiftCardStringsHaveEveryLanguage(t *testing.T) {
	for key, translations := range giftCardStrings {
		if len(translations) != len(giftCardLanguages) {
			t.Errorf("%s has %d languages, want %d", key, len(translations), len(giftCardLanguages))
		}
		wantPlaceholders := giftCardTestPlaceholder.FindAllString(translations["en"], -1)
		slices.Sort(wantPlaceholders)
		for _, language := range giftCardLanguages {
			text, ok := translations[language]
			if !ok || strings.TrimSpace(text) == "" {
				t.Errorf("%s has no %s", key, language)
				continue
			}
			placeholders := giftCardTestPlaceholder.FindAllString(text, -1)
			slices.Sort(placeholders)
			if !slices.Equal(placeholders, wantPlaceholders) {
				t.Errorf("%s (%s) has placeholders %v, want %v", key, language, placeholders, wantPlaceholders)
			}
		}
		if base, ok := strings.CutSuffix(key, ".one"); ok {
			if _, ok := giftCardStrings[base+".other"]; !ok {
				t.Errorf("%s has no .other", key)
			}
		}
		if base, ok := strings.CutSuffix(key, ".other"); ok {
			if _, ok := giftCardStrings[base+".one"]; !ok {
				t.Errorf("%s has no .one", key)
			}
		}
	}
	if giftCardLanguage("zh-CN") != "zh" || giftCardLanguage("AR") != "ar" || giftCardLanguage("fr") != "en" || giftCardLanguage("") != "en" {
		t.Fatal("language normalization")
	}
}

// Both gift emails render in all six languages through the shared layout: a
// one-line subject in the language, lang and dir on the page (Arabic right to
// left), the codes, the redeem link, the note as written, the Bittensor line
// only when chosen, a text part without markup, and no link off ur.io.
func TestGiftEmailTemplatesRenderEveryLanguage(t *testing.T) {
	linkHref := regexp.MustCompile(`href="([^"]+)"`)
	imageSrc := regexp.MustCompile(`src="([^"]+)"`)
	for _, language := range giftCardLanguages {
		dir := giftCardDir(language)
		check := func(name string, template Template, wantSubject string, wantHtml []string, wantText []string, absent []string) {
			t.Helper()
			subject, bodyHtml, bodyText, err := RenderEmailTemplate(template)
			if err != nil {
				t.Fatalf("%s %s: %v", language, name, err)
			}
			if subject != wantSubject {
				t.Errorf("%s %s: subject %q, want %q", language, name, subject, wantSubject)
			}
			if !strings.Contains(bodyHtml, `<html lang="`+language+`" dir="`+dir+`"`) || !strings.Contains(bodyHtml, `lang="`+language+`" dir="`+dir+`" style=`) {
				t.Errorf("%s %s: the page is not lang %s dir %s", language, name, language, dir)
			}
			if strings.Contains(bodyHtml, "{{") || strings.Contains(bodyText, "{{") {
				t.Errorf("%s %s: unresolved template action", language, name)
			}
			if strings.Contains(bodyText, "<") || strings.Contains(bodyText, "&amp;") {
				t.Errorf("%s %s: the text part has markup", language, name)
			}
			for _, match := range linkHref.FindAllStringSubmatch(bodyHtml, -1) {
				if match[1] != "https://ur.io" && !strings.HasPrefix(match[1], "https://ur.io/") {
					t.Errorf("%s %s: link %q leaves ur.io", language, name, match[1])
				}
			}
			for _, match := range imageSrc.FindAllStringSubmatch(bodyHtml, -1) {
				if !strings.HasPrefix(match[1], "https://ur.io/images/emails/ur-wordmark-") {
					t.Errorf("%s %s: image %q", language, name, match[1])
				}
			}
			for _, want := range wantHtml {
				escaped := htmlEscapedText(want)
				if strings.HasPrefix(want, "href=") {
					// markup, as written
					escaped = want
				}
				if !strings.Contains(bodyHtml, escaped) {
					t.Errorf("%s %s: the html part is missing %q", language, name, want)
				}
			}
			for _, want := range wantText {
				if !strings.Contains(bodyText, want) {
					t.Errorf("%s %s: the text part is missing %q", language, name, want)
				}
			}
			for _, unwanted := range absent {
				if strings.Contains(bodyHtml, htmlEscapedText(unwanted)) || strings.Contains(bodyText, unwanted) {
					t.Errorf("%s %s: has %q", language, name, unwanted)
				}
			}
		}

		// one card, emailed to the buyer
		order, codes := giftEmailTestOrder(language, model.GiftDeliveryMe, 1, "Sam", false)
		code := giftCardCodeGrouped(codes[0].Secret)
		redeemBy := giftCardText(language, "card.redeem_by", "date", giftCardDate(language, time.Date(2027, 11, 20, 0, 0, 0, 0, time.UTC)))
		check("buyer me 1", giftBuyerTestTemplate(order, codes),
			giftCardText(language, "buyer.subject.one"),
			[]string{giftCardText(language, "buyer.headline.one"), giftCardText(language, "buyer.me.one"), code, redeemBy, giftCardText(language, "buyer.how")},
			[]string{giftCardText(language, "buyer.me.one"), code, redeemBy},
			[]string{giftCardText(language, "buyer.them_copy.one")},
		)

		// three cards for three friends on Dec 24
		order, codes = giftEmailTestOrder(language, model.GiftDeliveryThem, 3, "Sam", true)
		date := giftCardDate(language, time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC))
		wantThem := []string{
			giftCardText(language, "buyer.them_on.other", "date", date),
			giftCardText(language, "buyer.them_copy.other"),
			giftCardText(language, "buyer.code_for", "email", "friendc@example.invalid"),
			giftCardCodeGrouped(codes[2].Secret),
		}
		headline := giftCardText(language, "buyer.headline.other")
		check("buyer them 3", giftBuyerTestTemplate(order, codes), giftCardText(language, "buyer.subject.other"),
			append([]string{headline}, wantThem...),
			// the text part's headline is in capitals, as every account email's
			append([]string{strings.ToUpper(headline)}, wantThem...),
			nil,
		)

		// friend 1's card, signed, with the Bittensor line
		friend := giftFriendTestTemplate(order, codes, 1)
		check("friend signed", friend,
			giftCardText(language, "friend.subject.signed", "signature", "Sam"),
			[]string{
				giftCardText(language, "card.kicker"),
				giftCardText(language, "friend.intro.signed", "signature", "Sam"),
				giftCardCodeGrouped(codes[1].Secret),
				`href="https://ur.io/redeem#code=` + codes[1].Secret + `"`,
				"Thinking of you",
				"always",
				giftCardText(language, "card.bittensor"),
				giftCardText(language, "card.step2"),
				giftCardText(language, "friend.reply"),
			},
			[]string{
				giftCardRedeemLink(codes[1].Secret),
				"Thinking of you\nalways",
				"– Sam",
				giftCardText(language, "card.bittensor"),
			},
			[]string{giftCardCodeGrouped(codes[0].Secret), "friendb@example.invalid"},
		)

		// unsigned, no Bittensor line, no note
		order, codes = giftEmailTestOrder(language, model.GiftDeliveryThem, 2, "", false)
		order.Message = ""
		check("friend anonymous", giftFriendTestTemplate(order, codes, 0),
			giftCardText(language, "friend.subject.anonymous"),
			[]string{giftCardText(language, "friend.intro.anonymous")},
			[]string{giftCardText(language, "friend.intro.anonymous")},
			[]string{giftCardText(language, "card.bittensor"), "– "},
		)
	}
}

// The buyer's email of an order of delivery "them" whose addresses were
// blanked (paid after the retention) reads as delivery "me": no friend is
// emailed, so it promises none.
func TestGiftBuyerEmailWithoutFriendAddresses(t *testing.T) {
	order, codes := giftEmailTestOrder("en", model.GiftDeliveryThem, 2, "", false)
	order.Recipients = []string{"", ""}
	buyer := giftBuyerTestTemplate(order, codes)
	if buyer.Them() {
		t.Fatal("an order without addresses reads as delivery them")
	}
	_, _, bodyText, err := RenderEmailTemplate(buyer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bodyText, giftCardText("en", "buyer.me.other")) || strings.Contains(bodyText, "each friend") || strings.Contains(bodyText, "For ") {
		t.Fatalf("buyer email %q", bodyText)
	}
}

// The friend's subject names the buyer's signature on one line, or someone who
// cares, and a line break in a signature never reaches a header.
func TestGiftFriendEmailSubject(t *testing.T) {
	order, codes := giftEmailTestOrder("en", model.GiftDeliveryThem, 1, "Sam", false)
	subject, _, _, err := RenderEmailTemplate(giftFriendTestTemplate(order, codes, 0))
	if err != nil || subject != "Sam sent you a gift of privacy" {
		t.Fatalf("subject = %q, %v", subject, err)
	}
	order.Signature = ""
	subject, _, _, err = RenderEmailTemplate(giftFriendTestTemplate(order, codes, 0))
	if err != nil || subject != "Someone who cares about you sent you a gift of privacy" {
		t.Fatalf("anonymous subject = %q, %v", subject, err)
	}
	order.Signature = "Sam\r\nBcc: victim@example.invalid"
	subject, _, _, err = RenderEmailTemplate(giftFriendTestTemplate(order, codes, 0))
	if err != nil || subject != "Sam Bcc: victim@example.invalid sent you a gift of privacy" {
		t.Fatalf("a signature with a break: subject = %q, %v", subject, err)
	}
}

// The gift emails carry their language, their card and (the friend's) the
// buyer as reply-to; the outbox stores only the order's keys.
func TestGiftEmailOptionalInterfaces(t *testing.T) {
	order, codes := giftEmailTestOrder("ar", model.GiftDeliveryThem, 2, "Sam", false)
	friend := giftFriendTestTemplate(order, codes, 1)
	buyer := giftBuyerTestTemplate(order, codes)

	var template Template = friend
	if replyTo, ok := template.(ReplyToTemplate); !ok || replyTo.ReplyTo() != "giver@example.invalid" {
		t.Fatal("the friend's email does not reply to the buyer")
	}
	if _, ok := Template(buyer).(ReplyToTemplate); ok {
		t.Fatal("the buyer's email replies to the buyer")
	}
	for _, template := range []Template{friend, buyer} {
		if language, ok := template.(LanguageTemplate); !ok || language.Language() != "ar" {
			t.Fatalf("%s does not carry its language", template.Name())
		}
		if _, ok := template.(accountMessageLoader); !ok {
			t.Fatalf("%s does not load when sent", template.Name())
		}
		attachments := template.(AttachmentTemplate).EmailAttachments()
		if len(attachments) != 1 || attachments[0].ContentType != "application/pdf" || string(attachments[0].Data) != string(giftEmailTestPdf) {
			t.Fatalf("%s attachments %+v", template.Name(), attachments)
		}
	}
	if friend.EmailAttachments()[0].Filename != "URnetwork-gift-card.pdf" || buyer.EmailAttachments()[0].Filename != "URnetwork-gift-cards.pdf" {
		t.Fatal("attachment names")
	}

	// the outbox json: the keys, never the note, an address or a card
	for _, template := range []Template{friend, buyer} {
		templateJson, err := json.Marshal(template)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(templateJson, &fields); err != nil {
			t.Fatal(err)
		}
		for field := range fields {
			if field != "gift_order_id" && field != "recipient_index" {
				t.Fatalf("%s stores %s in the outbox: %s", template.Name(), field, templateJson)
			}
		}
		decoded, err := decodeAccountMessageTemplate(template.Name(), string(templateJson))
		if err != nil {
			t.Fatal(err)
		}
		if loaded, ok := decoded.(AttachmentTemplate); !ok || 0 < len(loaded.EmailAttachments()) {
			t.Fatalf("%s decoded with a card", template.Name())
		}
	}
	decoded, _ := decodeAccountMessageTemplate(giftCardFriendTemplateName, `{"gift_order_id":"`+order.GiftOrderId.String()+`","recipient_index":1}`)
	if decoded.(*GiftCardFriendTemplate).RecipientIndex != 1 || decoded.(*GiftCardFriendTemplate).GiftOrderId != order.GiftOrderId {
		t.Fatal("the friend's email does not decode its keys")
	}
}

// A friend's email is due at 9:00 on the buyer's date in the buyer's zone, with
// the zone's offset on that date: daylight saving time on either side of a
// change, south of the equator, a half-hour zone. No date, a past time or an
// unknown zone send now (or at 9:00 UTC).
func TestGiftFriendDeliverTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		zone   string
		sendOn time.Time
		want   time.Time
	}{
		// Chicago: CST (-6) the day before daylight saving time starts, CDT (-5) on it
		{"America/Chicago", time.Date(2026, 3, 7, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 7, 15, 0, 0, 0, time.UTC)},
		{"America/Chicago", time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 8, 14, 0, 0, 0, time.UTC)},
		// and back on the day it ends
		{"America/Chicago", time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 31, 14, 0, 0, 0, time.UTC)},
		{"America/Chicago", time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC)},
		{"America/Chicago", time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 24, 15, 0, 0, 0, time.UTC)},
		// Sydney: AEDT (+11) from the first Sunday of October, so 9:00 is the day before in UTC
		{"Australia/Sydney", time.Date(2026, 10, 3, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 2, 23, 0, 0, 0, time.UTC)},
		{"Australia/Sydney", time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 3, 22, 0, 0, 0, time.UTC)},
		{"Australia/Sydney", time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 24, 22, 0, 0, 0, time.UTC)},
		{"Asia/Kolkata", time.Date(2026, 12, 25, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 25, 3, 30, 0, 0, time.UTC)},
		{"Europe/Berlin", time.Date(2026, 3, 29, 0, 0, 0, 0, time.UTC), time.Date(2026, 3, 29, 7, 0, 0, 0, time.UTC)},
		{"Pacific/Kiritimati", time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 23, 19, 0, 0, 0, time.UTC)},
		// unknown or empty zones read as UTC
		{"Not/AZone", time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 24, 9, 0, 0, 0, time.UTC)},
		{"", time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC), time.Date(2026, 12, 24, 9, 0, 0, 0, time.UTC)},
	}
	for _, c := range cases {
		sendOn := c.sendOn
		order := &model.GiftOrder{SendOn: &sendOn, SendTimezone: c.zone, Delivery: model.GiftDeliveryThem}
		if got := giftFriendDeliverTime(order, now); !got.Equal(c.want) || got.Location() != time.UTC {
			t.Errorf("%s %s: deliver at %s, want %s", c.zone, c.sendOn.Format(time.DateOnly), got, c.want)
		}
	}
	// a coin payment is never held: no date, or a past one, means now
	order := &model.GiftOrder{SendTimezone: "America/Chicago", Delivery: model.GiftDeliveryThem, Rail: model.GiftRailCrypto}
	if got := giftFriendDeliverTime(order, now); !got.Equal(now) {
		t.Errorf("coin, no date: deliver at %s, want now", got)
	}
	sendOn := time.Date(2025, 12, 24, 0, 0, 0, 0, time.UTC)
	order.SendOn = &sendOn
	if got := giftFriendDeliverTime(order, now); !got.Equal(now) {
		t.Errorf("coin, a past date: deliver at %s, want now", got)
	}
	// a card payment waits out the hold (owner, 2026-11-14), and an unknown rail is held like a card
	for _, rail := range []string{model.GiftRailStripe, ""} {
		card := &model.GiftOrder{SendTimezone: "America/Chicago", Delivery: model.GiftDeliveryThem, Rail: rail}
		if got := giftFriendDeliverTime(card, now); !got.Equal(now.Add(GiftFriendsCardHold)) {
			t.Errorf("rail %q, no date: deliver at %s, want the payment plus the hold", rail, got)
		}
		card.SendOn = &sendOn
		if got := giftFriendDeliverTime(card, now); !got.Equal(now.Add(GiftFriendsCardHold)) {
			t.Errorf("rail %q, a past date: deliver at %s, want the payment plus the hold", rail, got)
		}
	}
}

// The raw MIME of an email with a file: multipart/mixed of the alternative body
// (text, then html) and the file, base64; the reply-to and the encoded subject
// in the headers; nothing a template writes can add a header.
func TestRawEmailWithAttachment(t *testing.T) {
	pdf := []byte("%PDF-1.7\n" + strings.Repeat("binary \x00\x01\xff data ", 200))
	subject := "هدية الخصوصية من Sam"
	raw, err := buildRawEmail(&rawEmail{
		from:        "URnetwork <no-reply@ur.io>",
		to:          "friend@example.invalid",
		replyTo:     "giver@example.invalid",
		subject:     subject,
		bodyText:    "Hello\nthere = a long line " + strings.Repeat("x", 200),
		bodyHtml:    `<!DOCTYPE html><html lang="ar" dir="rtl"><body><p>مرحبا</p></body></html>`,
		attachments: []*EmailAttachment{{Filename: "URnetwork-gift-card.pdf", ContentType: "application/pdf", Data: pdf}},
		date:        time.Date(2026, 12, 24, 15, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\r\n") {
		if 998 < len(line) {
			t.Fatalf("a line of %d characters", len(line))
		}
	}
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	decoder := &mime.WordDecoder{}
	decodedSubject, err := decoder.DecodeHeader(message.Header.Get("Subject"))
	if err != nil || decodedSubject != subject {
		t.Fatalf("subject %q (%v), want %q", decodedSubject, err, subject)
	}
	if message.Header.Get("Reply-To") != "giver@example.invalid" || message.Header.Get("To") != "friend@example.invalid" ||
		message.Header.Get("From") != `"URnetwork" <no-reply@ur.io>` || message.Header.Get("MIME-Version") != "1.0" ||
		message.Header.Get("Date") != "Thu, 24 Dec 2026 15:00:00 +0000" {
		t.Fatalf("headers %v", message.Header)
	}
	mediaType, params, err := mime.ParseMediaType(message.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/mixed" {
		t.Fatalf("content type %s, %v", mediaType, err)
	}
	mixed := multipart.NewReader(message.Body, params["boundary"])

	body, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	bodyType, bodyParams, _ := mime.ParseMediaType(body.Header.Get("Content-Type"))
	if bodyType != "multipart/alternative" {
		t.Fatalf("the first part is %s", bodyType)
	}
	alternative := multipart.NewReader(body, bodyParams["boundary"])
	wantTypes := []string{"text/plain", "text/html"}
	for _, wantType := range wantTypes {
		part, err := alternative.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		partType, partParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if partType != wantType || partParams["charset"] != "UTF-8" {
			t.Fatalf("alternative part %s %v, want %s", partType, partParams, wantType)
		}
		// multipart.Reader decodes quoted-printable itself
		content, _ := io.ReadAll(part)
		if wantType == "text/html" && !strings.Contains(string(content), "مرحبا") {
			t.Fatalf("html part %q", content)
		}
		if wantType == "text/plain" && !strings.Contains(string(content), "there = a long line "+strings.Repeat("x", 200)) {
			t.Fatalf("text part %q", content)
		}
	}
	if _, err := alternative.NextPart(); err != io.EOF {
		t.Fatalf("a third alternative part: %v", err)
	}

	attachment, err := mixed.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	if attachment.FileName() != "URnetwork-gift-card.pdf" || attachment.Header.Get("Content-Transfer-Encoding") != "base64" {
		t.Fatalf("attachment headers %v", attachment.Header)
	}
	if partType, _, _ := mime.ParseMediaType(attachment.Header.Get("Content-Type")); partType != "application/pdf" {
		t.Fatalf("attachment type %s", partType)
	}
	encoded, _ := io.ReadAll(attachment)
	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(encoded), "\r\n", ""))
	if err != nil || string(decoded) != string(pdf) {
		t.Fatalf("the attachment does not decode to its bytes: %v", err)
	}
	if _, err := mixed.NextPart(); err != io.EOF {
		t.Fatalf("a third part: %v", err)
	}

	// injections and bad names are refused
	for _, bad := range []*rawEmail{
		{from: "a@ur.io", to: "b@example.invalid", replyTo: "c@example.invalid\r\nBcc: d@example.invalid", subject: "s"},
		{from: "a@ur.io", to: "b@example.invalid\nBcc: d@example.invalid", subject: "s"},
		{from: "a@ur.io", to: "b@example.invalid", subject: "s\r\nBcc: d@example.invalid"},
		{from: "a@ur.io", to: "b@example.invalid", subject: "s", attachments: []*EmailAttachment{{Filename: "a\".pdf", Data: pdf}}},
		{from: "a@ur.io", to: "b@example.invalid", subject: "s", attachments: []*EmailAttachment{{Filename: "../a.pdf", Data: pdf}}},
	} {
		if _, err := buildRawEmail(bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

// A template with files goes to SES as SendRawEmail, to the one recipient, with
// the template's reply-to; a template with a reply-to and no files keeps
// SendEmail and its ReplyToAddresses.
func TestAWSMessageSenderSendsAttachmentsRaw(t *testing.T) {
	endpoint := newAwsEndpoint(t, "text/xml", `<SendRawEmailResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <SendRawEmailResult>
    <MessageId>synthetic-message-id</MessageId>
  </SendRawEmailResult>
  <ResponseMetadata>
    <RequestId>synthetic-request-id</RequestId>
  </ResponseMetadata>
</SendRawEmailResponse>`)
	sender := endpoint.sender(5 * time.Second)

	order, codes := giftEmailTestOrder("es", model.GiftDeliveryThem, 2, "Sam", true)
	err := sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate(
			"frienda@example.invalid",
			giftFriendTestTemplate(order, codes, 0),
			SenderEmail("sender@synthetic.example"),
		)
	})
	if err != nil {
		t.Fatalf("send error = %v", err)
	}
	request := endpoint.receivedRequest()
	if request == nil {
		t.Fatal("the endpoint saw no SES call")
	}
	if err := request.ParseForm(); err != nil {
		t.Fatal(err)
	}
	form := request.PostForm
	if form.Get("Action") != "SendRawEmail" || form.Get("Source") != "sender@synthetic.example" ||
		form.Get("Destinations.member.1") != "frienda@example.invalid" || form.Get("Destinations.member.2") != "" {
		t.Fatalf("SES call = %v, want SendRawEmail to the friend from the sender", url.Values(form))
	}
	raw, err := base64.StdEncoding.DecodeString(form.Get("RawMessage.Data"))
	if err != nil {
		t.Fatal(err)
	}
	message, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	subject, _ := (&mime.WordDecoder{}).DecodeHeader(message.Header.Get("Subject"))
	if message.Header.Get("Reply-To") != "giver@example.invalid" || subject != "Sam te ha enviado un regalo de privacidad" {
		t.Fatalf("raw headers %v (subject %q)", message.Header, subject)
	}
	// a token, so unquoted (mime.FormatMediaType)
	if !strings.Contains(string(raw), "Content-Disposition: attachment; filename=URnetwork-gift-card.pdf\r\n") {
		t.Fatal("the raw message has no card")
	}

	// a reply-to without files: SendEmail
	endpoint = newAwsEndpoint(t, "text/xml", `<SendEmailResponse xmlns="http://ses.amazonaws.com/doc/2010-12-01/">
  <SendEmailResult>
    <MessageId>synthetic-message-id</MessageId>
  </SendEmailResult>
  <ResponseMetadata>
    <RequestId>synthetic-request-id</RequestId>
  </ResponseMetadata>
</SendEmailResponse>`)
	sender = endpoint.sender(5 * time.Second)
	friend := giftFriendTestTemplate(order, codes, 1)
	friend.pdf = nil
	err = sendWithGuard(t, func() error {
		return sender.SendAccountMessageTemplate("friendb@example.invalid", friend, SenderEmail("sender@synthetic.example"))
	})
	if err != nil {
		t.Fatalf("send error = %v", err)
	}
	request = endpoint.receivedRequest()
	if request == nil || request.ParseForm() != nil {
		t.Fatal("the endpoint saw no SES call")
	}
	if request.PostForm.Get("Action") != "SendEmail" || request.PostForm.Get("ReplyToAddresses.member.1") != "giver@example.invalid" {
		t.Fatalf("SES call = %v, want SendEmail replying to the buyer", url.Values(request.PostForm))
	}
}

// The friends' emails go out at the order's time and never before its
// payment: a card payment holds them two hours, a coin payment not at all, a
// date at 9:00 in the buyer's zone wins when it is later, and an order that
// emails no friend has no time.
func TestGiftFriendsSendAt(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	// 08:30 in Chicago
	paidTime := time.Date(2026, 11, 20, 14, 30, 0, 0, time.UTC)
	order := func(rail string, sendOn *time.Time, mutate func(order *model.GiftOrder)) *model.GiftOrder {
		order, _ := giftEmailTestOrder("en", model.GiftDeliveryThem, 2, "Sam", false)
		order.Rail = rail
		order.PaidTime = &paidTime
		order.SendOn = sendOn
		if mutate != nil {
			mutate(order)
		}
		return order
	}
	date := func(year int, month time.Month, day int) *time.Time {
		date := time.Date(year, month, day, 0, 0, 0, 0, time.UTC)
		return &date
	}
	for _, c := range []struct {
		name  string
		order *model.GiftOrder
		want  *time.Time
	}{
		{"card, right after payment", order(model.GiftRailStripe, nil, nil), func() *time.Time {
			at := paidTime.Add(GiftFriendsCardHold)
			return &at
		}()},
		{"coin, right after payment", order(model.GiftRailCrypto, nil, nil), &paidTime},
		{"card, a later date", order(model.GiftRailStripe, date(2026, 12, 24), nil), func() *time.Time {
			at := time.Date(2026, 12, 24, 9, 0, 0, 0, chicago).UTC()
			return &at
		}()},
		// 9:00 that day is within the hold: the hold wins
		{"card, a date inside the hold", order(model.GiftRailStripe, date(2026, 11, 20), nil), func() *time.Time {
			at := paidTime.Add(GiftFriendsCardHold)
			return &at
		}()},
		// paid after its date's 9:00: right after payment
		{"coin, a date already passed", order(model.GiftRailCrypto, date(2026, 11, 19), nil), &paidTime},
		{"coin, a date inside what would be a hold", order(model.GiftRailCrypto, date(2026, 11, 20), nil), func() *time.Time {
			at := time.Date(2026, 11, 20, 9, 0, 0, 0, chicago).UTC()
			return &at
		}()},
		// a rail it does not know holds, as a card does
		{"no rail", order("", nil, nil), func() *time.Time {
			at := paidTime.Add(GiftFriendsCardHold)
			return &at
		}()},
		{"delivery me", order(model.GiftRailStripe, nil, func(order *model.GiftOrder) { order.Delivery = model.GiftDeliveryMe }), nil},
		{"pending", order(model.GiftRailStripe, nil, func(order *model.GiftOrder) {
			order.Status = model.GiftOrderStatusPending
			order.PaidTime = nil
		}), nil},
		{"refunded", order(model.GiftRailStripe, nil, func(order *model.GiftOrder) { order.Status = model.GiftOrderStatusRefunded }), nil},
		{"no friend's address left", order(model.GiftRailCrypto, nil, func(order *model.GiftOrder) { order.Recipients = []string{"", " "} }), nil},
	} {
		got := GiftFriendsSendAt(c.order)
		if (got == nil) != (c.want == nil) || (got != nil && !got.Equal(*c.want)) {
			t.Errorf("%s: friends send at %v, want %v", c.name, got, c.want)
		}
	}
	if GiftFriendsCardHold != 2*time.Hour {
		t.Fatalf("the card hold is %s", GiftFriendsCardHold)
	}
}

// The buyer's email says when the friends get theirs, from the same time:
// now for a coin payment, the date for a date, and the time, in the buyer's
// zone, for a card payment's hold; now once the time has come when it goes.
func TestGiftBuyerLeadSaysWhenFriendsGetTheirs(t *testing.T) {
	paidTime := time.Date(2026, 11, 20, 14, 30, 0, 0, time.UTC)
	lead := func(language string, rail string, timezone string, sendOn *time.Time, loadTime time.Time) string {
		order, codes := giftEmailTestOrder(language, model.GiftDeliveryThem, 2, "Sam", false)
		order.Rail = rail
		order.PaidTime = &paidTime
		order.SendOn = sendOn
		order.SendTimezone = timezone
		template := giftBuyerTestTemplate(order, codes)
		template.loadTime = loadTime
		return template.Lead()
	}
	for _, language := range giftCardLanguages {
		if got, want := lead(language, model.GiftRailCrypto, "America/Chicago", nil, time.Time{}), giftCardText(language, "buyer.them_now.other"); got != want {
			t.Errorf("%s coin: %q, want %q", language, got, want)
		}
		holdTime := giftCardTime(language, paidTime.Add(GiftFriendsCardHold).In(model.GiftOrderLocation("America/Chicago")))
		if got, want := lead(language, model.GiftRailStripe, "America/Chicago", nil, time.Time{}), giftCardText(language, "buyer.them_hold.other", "time", holdTime); got != want {
			t.Errorf("%s card: %q, want %q", language, got, want)
		}
		sendOn := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
		date := giftCardDate(language, sendOn)
		if got, want := lead(language, model.GiftRailStripe, "America/Chicago", &sendOn, paidTime.Add(time.Minute)), giftCardText(language, "buyer.them_on.other", "date", date); got != want {
			t.Errorf("%s dated: %q, want %q", language, got, want)
		}
		// an email that goes out after the hold has passed
		if got, want := lead(language, model.GiftRailStripe, "America/Chicago", nil, paidTime.Add(3*time.Hour)), giftCardText(language, "buyer.them_now.other"); got != want {
			t.Errorf("%s late: %q, want %q", language, got, want)
		}
	}
	// a buyer whose zone is unknown reads the time in UTC
	if got, want := lead("en", model.GiftRailStripe, "", nil, time.Time{}), "We'll email each friend their own gift card and your note at 4:30 PM UTC, about two hours after your payment."; got != want {
		t.Errorf("unknown zone: %q, want %q", got, want)
	}
	if got, want := lead("en", model.GiftRailStripe, "America/Chicago", nil, time.Time{}), "We'll email each friend their own gift card and your note at 10:30 AM, about two hours after your payment."; got != want {
		t.Errorf("chicago: %q, want %q", got, want)
	}
}

// Each language writes the time of day its way; UTC says so.
func TestGiftCardTime(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Fatal(err)
	}
	afternoon := time.Date(2026, 11, 20, 15, 4, 0, 0, chicago)
	night := time.Date(2026, 11, 20, 1, 5, 0, 0, chicago)
	midnight := time.Date(2026, 11, 20, 0, 30, 0, 0, time.UTC)
	for _, c := range []struct {
		language string
		at       time.Time
		want     string
	}{
		{"en", afternoon, "3:04 PM"},
		{"en", night, "1:05 AM"},
		{"en", midnight, "12:30 AM UTC"},
		{"es", afternoon, "las 15:04"},
		{"es", night, "la 1:05"},
		{"es", midnight, "las 0:30 UTC"},
		{"de", afternoon, "15:04 Uhr"},
		{"de", midnight, "0:30 UTC"},
		{"zh", afternoon, "15:04"},
		{"ru", night, "1:05"},
		{"ar", midnight, "0:30 UTC"},
	} {
		if got := giftCardTime(c.language, c.at); got != c.want {
			t.Errorf("%s %s: %q, want %q", c.language, c.at, got, c.want)
		}
	}
}

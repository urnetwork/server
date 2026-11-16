package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image/png"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/urnetwork/server/v2026"
	"github.com/urnetwork/server/v2026/controller/giftcardassets"
	"github.com/urnetwork/server/v2026/model"
)

// A 240-character note, the longest the dialog takes, per language.
var giftCardTestLongNotes = map[string]string{
	"en": giftCardTestRunes("I'm thinking about you this holiday. Here's an app to help control your online identity. ", 240),
	"es": giftCardTestRunes("En estas fiestas pienso en ti. Te regalo una app para tu identidad digital. ", 240),
	"de": giftCardTestRunes("In dieser festlichen Zeit denke ich an dich und an deine Privatsphäre. ", 240),
	"zh": giftCardTestRunes("这个节日，我一直惦记着您。送您一款应用，帮您掌控自己的网络身份。", 240),
	"ru": giftCardTestRunes("В эти праздники я думаю о тебе. Дарю тебе приложение для приватности. ", 240),
	"ar": giftCardTestRunes("في هذه الأعياد، أفكر فيك. إليك تطبيقًا يساعدك على التحكم في هويتك الرقمية. ", 240),
}

// The text repeated to n runes.
func giftCardTestRunes(text string, n int) string {
	runes := []rune(strings.Repeat(text, n/len([]rune(text))+1))
	return strings.TrimSpace(string(runes[:n]))
}

func giftCardTestDocument(language string, count int, message string) *giftCardsDocument {
	document := &giftCardsDocument{
		language:  language,
		season:    "winter-2026",
		message:   message,
		signature: "Sam",
		bittensor: true,
		showCount: true,
	}
	for i := range count {
		document.cards = append(document.cards, &giftCard{
			secret: fmt.Sprintf("K7QX3M2PNB4DLZ8R9YWC5AGH%02d", i),
			// a window that ends with the whole of 13 November everywhere
			redeemBy: time.Date(2027, 11, 14, 12, 0, 0, 0, time.UTC),
			n:        i + 1,
			count:    count,
		})
	}
	return document
}

// The code prints in groups of four, the remainder last; the QR code and the
// links carry it plain.
func TestGiftCardCodeGroups(t *testing.T) {
	secret := "K7QX3M2PNB4DLZ8R9YWC5AGHT6"
	groups := giftCardCodeGroups(secret)
	if strings.Join(groups, "") != secret || len(groups) != 7 || groups[6] != "T6" {
		t.Fatalf("groups = %q", groups)
	}
	if giftCardCodeGrouped(secret) != "K7QX 3M2P NB4D LZ8R 9YWC 5AGH T6" {
		t.Fatalf("grouped = %q", giftCardCodeGrouped(secret))
	}
	// in the fragment, which never reaches a server
	if giftCardRedeemLink(secret) != "https://ur.io/redeem#code="+secret {
		t.Fatalf("redeem link = %q", giftCardRedeemLink(secret))
	}
	if groups := giftCardCodeGroups("ABCDEFGH"); !slices.Equal(groups, []string{"ABCD", "EFGH"}) {
		t.Fatalf("groups of 8 = %q", groups)
	}
}

// The QR code is a PNG of whole 10 px modules, black on white.
func TestGiftCardQrPng(t *testing.T) {
	pngBytes, err := giftCardQrPng("K7QX3M2PNB4DLZ8R9YWC5AGHT6")
	if err != nil {
		t.Fatal(err)
	}
	image, err := png.Decode(bytes.NewReader(pngBytes))
	if err != nil {
		t.Fatal(err)
	}
	bounds := image.Bounds()
	if bounds.Dx() != bounds.Dy() || bounds.Dx()%10 != 0 || bounds.Dx() < 210 {
		t.Fatalf("qr image is %v", bounds)
	}
	r, g, b, _ := image.At(0, 0).RGBA()
	// the top left module is the finder pattern's dark corner
	if 0x2000 < r || 0x2000 < g || 0x2000 < b {
		t.Fatalf("the corner module is not dark: %d %d %d", r, g, b)
	}
}

// A note keeps its words and at most a few line breaks, never two blank lines.
func TestGiftCardNote(t *testing.T) {
	if note := giftCardNote("  Hello\n\n\n\nthere  \n"); note != "Hello\n\nthere" {
		t.Fatalf("note = %q", note)
	}
	note := giftCardNote(strings.Repeat("a\n", 40))
	if strings.Count(note, "\n") != giftCardNoteMaxLineBreaks || strings.Count(note, "a") != 40 {
		t.Fatalf("note kept %d breaks and %d words", strings.Count(note, "\n"), strings.Count(note, "a"))
	}
}

// The card page renders in every language with the language's words, its date,
// its direction (Arabic right to left), every code in groups, the escaped note,
// and the Bittensor line only when chosen.
func TestGiftCardsHtmlEveryLanguage(t *testing.T) {
	wantDates := map[string]string{
		"en": "November 13, 2027",
		"es": "13 de noviembre de 2027",
		"de": "13. November 2027",
		"zh": "2027 年 11 月 13 日",
		"ru": "13 ноября 2027 г.",
		"ar": "13 نوفمبر 2027",
	}
	for _, language := range giftCardLanguages {
		document := giftCardTestDocument(language, 2, "Thinking of you <b>always</b>\nand forever")
		pageHtml, err := renderGiftCardsHtml(giftCardsPageFor(document, "qr-0", "scene-0.png"))
		if err != nil {
			t.Fatalf("%s: %v", language, err)
		}
		wantDir := "ltr"
		if language == "ar" {
			wantDir = "rtl"
		}
		if !strings.Contains(pageHtml, fmt.Sprintf(`<html lang="%s" dir="%s">`, language, wantDir)) {
			t.Errorf("%s: the page does not declare lang %s dir %s", language, language, wantDir)
		}
		for _, key := range []string{"card.kicker", "card.gift_label", "card.gift_value", "card.code_label", "card.tagline", "card.bittensor"} {
			if want := htmlEscapedText(giftCardText(language, key)); !strings.Contains(pageHtml, want) {
				t.Errorf("%s: missing %s %q", language, key, want)
			}
		}
		for _, key := range []string{"card.step1", "card.step2", "card.step3"} {
			if want := htmlEscapedText(giftCardText(language, key)); !strings.Contains(pageHtml, "<li>"+want+"</li>") {
				t.Errorf("%s: missing step %s %q", language, key, want)
			}
		}
		// the window's last whole day
		if want := htmlEscapedText(giftCardText(language, "card.redeem_by", "date", wantDates[language])); !strings.Contains(pageHtml, want) {
			t.Errorf("%s: missing the redeem-by %q", language, want)
		}
		if want := htmlEscapedText(giftCardText(language, "card.gift_n", "n", "2", "count", "2")); !strings.Contains(pageHtml, want) {
			t.Errorf("%s: missing %q", language, want)
		}
		if strings.Count(pageHtml, `<article class="gc">`) != 2 {
			t.Errorf("%s: want 2 cards", language)
		}
		if !strings.Contains(pageHtml, `<span class="g">K7QX</span><span class="g">3M2P</span>`) || !strings.Contains(pageHtml, `<span class="g">01</span>`) {
			t.Errorf("%s: the code is not in groups", language)
		}
		if !strings.Contains(pageHtml, `src="qr-0-1.png"`) || !strings.Contains(pageHtml, `src="scene-0.png"`) {
			t.Errorf("%s: missing the qr code or the scene", language)
		}
		if strings.Contains(pageHtml, `class="gc-sky"`) {
			t.Errorf("%s: the night sky prints behind the scene", language)
		}
		if strings.Contains(pageHtml, "<b>always</b>") || !strings.Contains(pageHtml, "&lt;b&gt;always&lt;/b&gt;\nand forever") {
			t.Errorf("%s: the note is not escaped as written", language)
		}
		if !strings.Contains(pageHtml, "– Sam") {
			t.Errorf("%s: missing the signature", language)
		}
	}

	// without the Bittensor line, a scene, a count or a note
	document := giftCardTestDocument("en", 1, "")
	document.bittensor = false
	document.signature = ""
	pageHtml, err := renderGiftCardsHtml(giftCardsPageFor(document, "qr-0", ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"Made with Bittensor", `class="taoline"`, "<img src=", "Gift 1 of 1", `class="gc-note"`, `class="gc-msg"`, `class="gc-from"`} {
		if strings.Contains(pageHtml, absent) {
			t.Errorf("the plain card has %q", absent)
		}
	}
	if !strings.Contains(pageHtml, `class="gc-sky"`) {
		t.Error("a season without a scene prints no night sky")
	}
}

func htmlEscapedText(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;").Replace(text)
}

// A paid order of delivery "them" makes the combined PDF of every card, in the
// recipients' order with "Gift n of N", and one PDF of each friend's card
// without the count. Delivery "me" makes only the combined PDF.
func TestGiftOrderCardDocuments(t *testing.T) {
	sendOn := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
	order := &model.GiftOrder{
		GiftOrderId:  server.NewId(),
		Status:       model.GiftOrderStatusPaid,
		Quantity:     3,
		Delivery:     model.GiftDeliveryThem,
		Recipients:   []string{"a@example.invalid", "b@example.invalid", "c@example.invalid"},
		SendOn:       &sendOn,
		SendTimezone: "Asia/Tokyo",
		Message:      "Note",
		Signature:    "Sam",
		Language:     "de",
		Season:       "winter-2026",
	}
	index := func(i int) *int { return &i }
	codes := []*model.GiftCode{
		{Secret: "CCCCCCCCCCCCCCCCCCCCCCCCCC", RecipientIndex: index(2)},
		{Secret: "AAAAAAAAAAAAAAAAAAAAAAAAAA", RecipientIndex: index(0)},
		{Secret: "BBBBBBBBBBBBBBBBBBBBBBBBBB", RecipientIndex: index(1)},
	}
	documents, parts := giftOrderCardDocuments(order, codes)
	if !slices.Equal(parts, []string{"all", "r0", "r1", "r2"}) {
		t.Fatalf("parts = %q", parts)
	}
	all := documents[0]
	if !all.showCount || len(all.cards) != 3 || all.cards[0].secret[0] != 'A' || all.cards[2].secret[0] != 'C' || all.cards[2].n != 3 {
		t.Fatalf("the combined document is out of order: %+v", all)
	}
	if all.language != "de" {
		t.Fatalf("the combined document reads in %s", all.language)
	}
	for i, document := range documents[1:] {
		if document.showCount || len(document.cards) != 1 || document.cards[0].secret[0] != byte('A'+i) {
			t.Fatalf("friend %d's document: %+v", i, document)
		}
	}

	// an order whose addresses the retention blanked prints no friend's card
	order.Recipients = []string{"a@example.invalid", "", " "}
	if _, parts = giftOrderCardDocuments(order, codes); !slices.Equal(parts, []string{"all", "r0"}) {
		t.Fatalf("blank addresses: parts %q", parts)
	}

	order.Delivery = model.GiftDeliveryMe
	order.Recipients = nil
	for _, code := range codes {
		code.RecipientIndex = nil
	}
	documents, parts = giftOrderCardDocuments(order, codes)
	if !slices.Equal(parts, []string{"all"}) || len(documents[0].cards) != 3 || documents[0].cards[0].secret[0] != 'C' {
		t.Fatalf("delivery me: parts %q", parts)
	}
}

// The headless Chromium to print with in tests: WARP_GIFT_CHROMIUM, else a
// Playwright headless shell, else none.
func giftCardTestChromium(t *testing.T) string {
	if chromium := os.Getenv(GiftCardChromiumEnv); chromium != "" {
		return chromium
	}
	home, _ := os.UserHomeDir()
	for _, pattern := range []string{
		filepath.Join(home, "Library/Caches/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell"),
		filepath.Join(home, ".cache/ms-playwright/chromium_headless_shell-*/chrome-headless-shell-*/chrome-headless-shell"),
	} {
		matches, _ := filepath.Glob(pattern)
		slices.Sort(matches)
		if 0 < len(matches) {
			return matches[len(matches)-1]
		}
	}
	return ""
}

var (
	giftCardPdfPagePattern     = regexp.MustCompile(`/Type\s*/Page[^s]`)
	giftCardPdfMediaBoxPattern = regexp.MustCompile(`/MediaBox\s*\[\s*0\s+0\s+([0-9.]+)\s+([0-9.]+)\s*\]`)
)

// The most a PDF of 20 cards may weigh: SES takes a raw message of 10 MB after
// base64, so the buyer's email must stay far below.
const giftCardTestMaxPdfByteCount = 4 * 1024 * 1024

// A real print with a local headless shell (skipped without one): every card
// is one 5 x 7 in page, the longest note in each language fits (or is cut) on
// its card, the text is selectable (fonts with a ToUnicode map; Chromium writes
// a variable face as a Type 3 font), and an order of 20 cards stays far under
// SES's size limit. With GIFT_CARD_SAMPLE_DIR set, the PDFs are kept there.
func TestGiftCardsPrintPdf(t *testing.T) {
	chromium := giftCardTestChromium(t)
	if chromium == "" {
		t.Skip("no headless chromium: set " + GiftCardChromiumEnv)
	}
	t.Setenv(GiftCardChromiumEnv, chromium)
	InstallGiftCardAssets(giftcardassets.FS)

	documents := []*giftCardsDocument{}
	languages := []string{}
	for _, language := range giftCardLanguages {
		documents = append(documents, giftCardTestDocument(language, 2, giftCardTestLongNotes[language]))
		languages = append(languages, language)
	}
	// a lone friend's card, a short note, no signature and no Bittensor line
	friend := giftCardTestDocument("en", 1, "Happy holidays!")
	friend.signature = ""
	friend.bittensor = false
	friend.showCount = false
	documents = append(documents, friend)
	languages = append(languages, "en-friend")
	// the largest order
	documents = append(documents, giftCardTestDocument("en", 20, giftCardTestLongNotes["en"]))
	languages = append(languages, "en-20")

	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	startTime := time.Now()
	pdfs, err := printGiftCardDocuments(ctx, documents)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("printed %d pdfs in %s", len(pdfs), time.Since(startTime))
	sampleDir := os.Getenv("GIFT_CARD_SAMPLE_DIR")
	for i, pdf := range pdfs {
		language := languages[i]
		if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
			t.Fatalf("%s: not a pdf", language)
		}
		if pageCount := len(giftCardPdfPagePattern.FindAll(pdf, -1)); pageCount != len(documents[i].cards) {
			t.Errorf("%s: %d pages for %d cards", language, pageCount, len(documents[i].cards))
		}
		for _, match := range giftCardPdfMediaBoxPattern.FindAllSubmatch(pdf, -1) {
			if string(match[1]) != "360" || string(match[2]) != "504" {
				t.Errorf("%s: a page is %s x %s pt, want 360 x 504 (5 x 7 in)", language, match[1], match[2])
			}
		}
		if !bytes.Contains(pdf, []byte("/ToUnicode")) || !(bytes.Contains(pdf, []byte("/Type3")) || bytes.Contains(pdf, []byte("/FontFile2"))) {
			t.Errorf("%s: the pdf has no selectable text", language)
		}
		if giftCardTestMaxPdfByteCount < len(pdf) {
			t.Errorf("%s: the pdf of %d cards is %d bytes", language, len(documents[i].cards), len(pdf))
		}
		t.Logf("%s: %d cards, %d bytes", language, len(documents[i].cards), len(pdf))
		if sampleDir != "" {
			if err := os.MkdirAll(sampleDir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sampleDir, fmt.Sprintf("gift-cards-%s.pdf", language)), pdf, 0600); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// Without installed assets, or without a browser, printing fails at once and
// says why.
func TestGiftCardsPrintNeedsAssetsAndChromium(t *testing.T) {
	InstallGiftCardAssets(fstest.MapFS{
		"fonts/Synthetic.ttf": &fstest.MapFile{Data: []byte("not a font")},
	})
	defer InstallGiftCardAssets(giftcardassets.FS)
	t.Setenv(GiftCardChromiumEnv, filepath.Join(t.TempDir(), "no-chromium"))
	_, err := printGiftCardDocuments(context.Background(), []*giftCardsDocument{giftCardTestDocument("en", 1, "")})
	if err == nil || !strings.Contains(err.Error(), "no headless chromium") {
		t.Fatalf("print without chromium: %v", err)
	}
}

// Text a buyer types that tries to be markup.
const (
	giftCardTestHostileNote      = "<script>window.pwned = 1</script><img src=x onerror=\"window.pwned = 2\"></style><a href=\"javascript:window.pwned = 3\">open</a> &amp; done"
	giftCardTestHostileSignature = "</p><script>window.pwned = 4</script><svg onload=\"window.pwned = 5\">"
)

// The buyer's note and signature reach the page only as escaped text: no tag,
// attribute or URL of theirs survives into the html.
func TestGiftCardsHtmlEscapesBuyerText(t *testing.T) {
	document := giftCardTestDocument("en", 1, giftCardTestHostileNote)
	document.signature = giftCardTestHostileSignature
	pageHtml, err := renderGiftCardsHtml(giftCardsPageFor(document, "qr-0", ""))
	if err != nil {
		t.Fatal(err)
	}
	// no tag of theirs opens (their words, "onerror=" or "javascript:", stay
	// as text)
	for _, raw := range []string{"<script", "<img src=x", "</style><a", "<a href", "<svg onload", "</p><script"} {
		if strings.Contains(pageHtml, raw) {
			t.Errorf("the page has the buyer's raw %q", raw)
		}
	}
	for _, escaped := range []string{
		"&lt;script&gt;window.pwned = 1&lt;/script&gt;&lt;img src=x onerror=&#34;window.pwned = 2&#34;&gt;&lt;/style&gt;&lt;a href=&#34;javascript:window.pwned = 3&#34;&gt;open&lt;/a&gt; &amp;amp; done",
		"– &lt;/p&gt;&lt;script&gt;window.pwned = 4&lt;/script&gt;&lt;svg onload=&#34;window.pwned = 5&#34;&gt;",
	} {
		if !strings.Contains(pageHtml, escaped) {
			t.Errorf("the page does not carry the buyer's text as text: want %q", escaped)
		}
	}
	// the page carries no script of its own, and a policy that allows none
	if strings.Contains(pageHtml, "<script") || !strings.Contains(pageHtml, `<meta http-equiv="Content-Security-Policy" content="default-src 'none'; img-src file:; font-src file:; style-src 'unsafe-inline'; script-src 'none'; base-uri 'none'; form-action 'none'">`) {
		t.Error("the page has a script or lacks its policy")
	}
}

// The browser's environment is its own: none of the taskworker's variables
// (which hold the server's secrets) but fontconfig's, and its home and
// temporary directories inside the print directory.
func TestGiftCardChromiumEnvironment(t *testing.T) {
	t.Setenv("WARP_SYNTHETIC_SECRET", "synthetic")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "synthetic")
	t.Setenv("FONTCONFIG_PATH", "/etc/fonts")
	dir := t.TempDir()
	env, err := giftCardChromiumEnvironment(dir)
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, entry := range env {
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	want := []string{"FONTCONFIG_PATH", "HOME", "LANG", "PATH", "TMPDIR", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"}
	names := slices.Sorted(maps.Keys(values))
	if !slices.Equal(names, want) {
		t.Fatalf("the browser's environment is %v, want %v", names, want)
	}
	for _, name := range []string{"HOME", "TMPDIR", "XDG_CACHE_HOME"} {
		if !strings.HasPrefix(values[name], dir+string(filepath.Separator)) {
			t.Errorf("%s=%s is outside the print directory", name, values[name])
		}
	}
	for _, name := range []string{"HOME", "TMPDIR"} {
		if info, err := os.Stat(values[name]); err != nil || !info.IsDir() {
			t.Errorf("%s does not exist: %v", name, err)
		}
	}
	if values["FONTCONFIG_PATH"] != "/etc/fonts" {
		t.Error("the browser lost fontconfig's path")
	}
}

// In the real browser (skipped without one): the buyer's hostile note and
// signature are text nodes only, no page script can run (the page's policy and
// the session's disabled scripts), nothing is fetched, and the card still
// prints.
func TestGiftCardsBrowserRunsNoBuyerMarkup(t *testing.T) {
	chromium := giftCardTestChromium(t)
	if chromium == "" {
		t.Skip("no headless chromium: set " + GiftCardChromiumEnv)
	}
	document := giftCardTestDocument("en", 1, giftCardTestHostileNote)
	document.signature = giftCardTestHostileSignature

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	// in the system temporary directory, as a print's: the printer's user
	// cannot traverse t.TempDir's private parent
	dir := giftCardTestPrintDir(t)
	pageUrls, err := writeGiftCardPages(giftcardassets.FS, dir, []*giftCardsDocument{document})
	if err != nil {
		t.Fatal(err)
	}
	browser, err := startGiftCardBrowser(ctx, chromium, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.close()
	if err := browser.loadPage(ctx, pageUrls[0]); err != nil {
		t.Fatal(err)
	}
	// the page as the buyer's text made it
	value, err := browser.evaluate(ctx, `JSON.stringify({
		noteChildren: document.querySelector(".gc-msg").children.length,
		noteText: document.querySelector(".gc-msg").textContent,
		fromChildren: document.querySelector(".gc-from").children.length,
		fromText: document.querySelector(".gc-from").textContent,
		pageScripts: document.querySelectorAll("script").length,
		handlers: document.querySelectorAll("[onload], [onerror]").length,
		anchors: document.querySelectorAll("a").length,
		images: Array.from(document.images).map((image) => image.getAttribute("src")),
	})`)
	if err != nil {
		t.Fatal(err)
	}
	var dom struct {
		NoteChildren int      `json:"noteChildren"`
		NoteText     string   `json:"noteText"`
		FromChildren int      `json:"fromChildren"`
		FromText     string   `json:"fromText"`
		PageScripts  int      `json:"pageScripts"`
		Handlers     int      `json:"handlers"`
		Anchors      int      `json:"anchors"`
		Images       []string `json:"images"`
	}
	if err := json.Unmarshal([]byte(value.(string)), &dom); err != nil {
		t.Fatal(err)
	}
	if dom.NoteChildren != 0 || dom.NoteText != giftCardNote(giftCardTestHostileNote) ||
		dom.FromChildren != 0 || dom.FromText != "– "+giftCardTestHostileSignature {
		t.Fatalf("the buyer's text became markup: %+v", dom)
	}
	if dom.PageScripts != 0 || dom.Handlers != 0 || dom.Anchors != 0 || !slices.Equal(dom.Images, []string{"scene-0.png", "qr-0-0.png"}) {
		t.Fatalf("the page gained elements from the buyer's text: %+v", dom)
	}

	// a script and a handler added to the page never run
	if _, err := browser.evaluate(ctx, `(() => {
		const injected = document.createElement("script");
		injected.textContent = "window.pwned = 6";
		document.body.appendChild(injected);
		const handler = document.createElement("img");
		handler.setAttribute("onerror", "window.pwned = 7");
		handler.src = "missing.png";
		document.body.appendChild(handler);
		return true;
	})()`); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Second)
	if pwned, err := browser.evaluate(ctx, `window.pwned === undefined ? 0 : window.pwned`); err != nil || pwned != float64(0) {
		t.Fatalf("a page script ran: window.pwned = %v, %v", pwned, err)
	}

	// and the page's policy refuses a fetch
	fetchCtx, fetchCancel := context.WithTimeout(ctx, 20*time.Second)
	defer fetchCancel()
	if fetched, err := browser.evaluate(fetchCtx, `fetch("file:///etc/hosts").then(() => "fetched", () => "blocked")`); err != nil || fetched != "blocked" {
		t.Fatalf("the page fetched a file: %v, %v", fetched, err)
	}

	pdf, fit, err := browser.printPage(ctx, pageUrls[0])
	if err != nil || !bytes.HasPrefix(pdf, []byte("%PDF-")) || (fit != "fit" && fit != "cut") {
		t.Fatalf("the hostile card did not print: %s, %v", fit, err)
	}
}

// A print directory as printGiftCardDocuments makes one, removed after the test.
func giftCardTestPrintDir(t *testing.T) string {
	dir, err := os.MkdirTemp("", "urnetwork-gift-cards-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// The printer's credential on Linux as root (the taskworker image), else the
// test is skipped.
func giftCardTestPrinterCredential(t *testing.T) *syscall.Credential {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("the printer's user applies to a root taskworker on linux")
	}
	credential, err := giftCardChromiumCredential()
	if err != nil {
		t.Skipf("no printer user here: %v", err)
	}
	if credential == nil || credential.Uid == 0 || credential.Gid == 0 || len(credential.Groups) != 0 {
		t.Fatalf("the printer's credential is %+v", credential)
	}
	return credential
}

// Chromium never runs as root: as root (the taskworker image), the browser and
// every process it starts run as the printer's user, every uid and gid of
// theirs with no supplementary group, in a directory the printer owns.
func TestGiftCardChromiumRunsAsThePrinter(t *testing.T) {
	credential := giftCardTestPrinterCredential(t)
	chromium := giftCardTestChromium(t)
	if chromium == "" {
		t.Skip("no headless chromium: set " + GiftCardChromiumEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := giftCardTestPrintDir(t)
	browser, err := startGiftCardBrowser(ctx, chromium, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.close()

	info, err := os.Stat(dir)
	if err != nil || info.Sys().(*syscall.Stat_t).Uid != credential.Uid {
		t.Fatalf("the print directory is not the printer's: %v", err)
	}
	pids := []int{browser.cmd.Process.Pid}
	// and its helper processes, in its process group
	entries, _ := os.ReadDir("/proc")
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == browser.cmd.Process.Pid {
			continue
		}
		if pgid, err := syscall.Getpgid(pid); err == nil && pgid == browser.cmd.Process.Pid {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(status), "\n") {
			name, value, _ := strings.Cut(line, ":")
			fields := strings.Fields(value)
			switch name {
			case "Uid":
				for _, field := range fields {
					if field != strconv.FormatUint(uint64(credential.Uid), 10) {
						t.Fatalf("process %d runs with uid %s: %q", pid, field, line)
					}
				}
			case "Gid":
				for _, field := range fields {
					if field != strconv.FormatUint(uint64(credential.Gid), 10) {
						t.Fatalf("process %d runs with gid %s: %q", pid, field, line)
					}
				}
			case "Groups":
				if len(fields) != 0 {
					t.Fatalf("process %d keeps supplementary groups: %q", pid, line)
				}
			}
		}
	}
	t.Logf("%d chromium processes run as uid %d", len(pids), credential.Uid)
}

// The printer's user cannot read the taskworker's secrets: the vault laid out
// as warpctl mounts it (a 0700 root directory of 0644 root files, bind-mounted
// at /srv/warp/vault), /root (where .aws links into the vault), or the
// taskworker's /proc entries (this test process stands in for it). It can read
// its own directory. Linux as root only (the taskworker image).
func TestGiftCardPrinterCannotReadSecrets(t *testing.T) {
	credential := giftCardTestPrinterCredential(t)
	run := func(name string, args ...string) error {
		command := exec.Command(name, args...)
		command.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
		command.Env = []string{"PATH=/usr/bin:/bin"}
		command.Dir = "/"
		return command.Run()
	}

	// a vault as deployed: the directory 0700 root, its files 0644 root
	parent := giftCardTestPrintDir(t)
	if err := os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	vault := filepath.Join(parent, "vault")
	if err := os.Mkdir(vault, 0700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(vault, "pg.yml")
	if err := os.WriteFile(secret, []byte("password: synthetic\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("/bin/cat", secret).Run(); err != nil {
		t.Fatalf("root cannot read the synthetic vault: %v", err)
	}
	unreadable := []string{secret, "/root", fmt.Sprintf("/proc/%d/environ", os.Getpid()), fmt.Sprintf("/proc/%d/fd", os.Getpid()), fmt.Sprintf("/proc/%d/mem", os.Getpid())}
	// the deployment's own mount, when this runs where it is
	if entries, err := os.ReadDir("/srv/warp/vault"); err == nil {
		for _, entry := range entries {
			unreadable = append(unreadable, filepath.Join("/srv/warp/vault", entry.Name()))
		}
		t.Logf("checking %d files in /srv/warp/vault", len(entries))
	}
	for _, path := range unreadable {
		info, statErr := os.Stat(path)
		if statErr != nil {
			continue
		}
		var err error
		if info.IsDir() {
			err = run("/bin/ls", path)
		} else {
			err = run("/bin/cat", path)
		}
		if err == nil {
			t.Errorf("the printer's user reads %s", path)
		}
	}

	// its own directory it can
	own := giftCardTestPrintDir(t)
	page := filepath.Join(own, "cards-0.html")
	if err := os.WriteFile(page, []byte("<p>card</p>"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := giftCardChownTree(own, int(credential.Uid), int(credential.Gid)); err != nil {
		t.Fatal(err)
	}
	if err := run("/bin/cat", page); err != nil {
		t.Fatalf("the printer's user cannot read its own page: %v", err)
	}
}

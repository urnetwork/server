package controller

// The gift cards as PDFs (GIFTOFPRIVACY.md §2.5, §10.4): mockup 4 of
// mmm/giftofprivacy/design.html as an html/template page (gift_cards/card.html),
// one 5 x 7 in card per page, printed by headless Chromium. A paid order gets
// one PDF of every card (part "all": the download and the buyer's email) and,
// for delivery "them", one PDF of each friend's card (part "r<index>": that
// friend's email).
//
// Chromium is driven over the DevTools protocol on a pipe
// (--remote-debugging-pipe), not with --print-to-pdf: the command line prints
// before the web fonts load (a note set in Caveat prints blank) and the
// headless shell then does not exit. Over the protocol the printer loads each
// page, waits for its fonts and images, fits every note to its card, prints
// with the page's own size, and closes the browser.
//
// The browser runs without its sandbox (the taskworker's container has no user
// namespaces) and prints text the buyer typed, so the page is held tight. The
// buyer's note and signature reach the page only through html/template's
// contextual escaping. The page carries no script, page scripts are disabled
// for the whole session (the fitting runs as a DevTools evaluation), and its
// Content-Security-Policy allows nothing but its own fonts and images from
// file: and its inline style. The browser resolves no host, starts with an
// environment of its own (none of the taskworker's, which holds its secrets),
// works in a directory of its own, and is killed with every process it started
// when the print ends.
//
// The binary is WARP_GIFT_CHROMIUM, else the taskworker image's
// chrome-headless-shell (cli/taskworker/Dockerfile). The fonts and scenes come
// from giftcardassets, which the taskworker installs (InstallGiftCardAssets),
// so the API and the other services do not carry their bytes.

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"image/color"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/skip2/go-qrcode"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server/model"
)

// The env var that names the headless Chromium binary.
const GiftCardChromiumEnv = "WARP_GIFT_CHROMIUM"

// The env var that names the user Chromium runs as when this process is root,
// and that user in the taskworker image (cli/taskworker/Dockerfile). Chromium
// never runs as root.
const (
	GiftCardChromiumUserEnv     = "WARP_GIFT_CHROMIUM_USER"
	giftCardChromiumUserDefault = "giftcard"
)

// The taskworker image's Chrome for Testing headless shell
// (cli/taskworker/Dockerfile), used when the env var is unset.
const giftCardChromiumDefault = "/opt/chrome-headless-shell/chrome-headless-shell"

// The page the QR code opens, with the code filled in.
const giftCardRedeemUrl = "https://ur.io/redeem"

// The bounds on one print: the browser's start, and one page from navigation to
// PDF.
const (
	giftCardBrowserStartTimeout = 30 * time.Second
	giftCardPagePrintTimeout    = 90 * time.Second
	giftCardBrowserCloseTimeout = 10 * time.Second
)

// The most browsers one process runs at once. Each takes a few hundred MB, and
// a burst of paid orders must not run the taskworker out of memory.
const giftCardMaxConcurrentBrowsers = 2

var giftCardBrowserSlots = make(chan struct{}, giftCardMaxConcurrentBrowsers)

// The longest note kept in lines: further line breaks become spaces, so a note
// of blank lines cannot push the code off the card.
const giftCardNoteMaxLineBreaks = 8

//go:embed gift_cards/card.html
var giftCardsPageSource string

var giftCardsPageTemplate = sync.OnceValues(func() (*htmltemplate.Template, error) {
	return htmltemplate.New("card.html").Parse(giftCardsPageSource)
})

// The installed assets (giftcardassets.FS): fonts/ and scenes/.
type giftCardAssetsHolder struct {
	fsys fs.FS
}

var giftCardAssetsInstalled atomic.Pointer[giftCardAssetsHolder]

// Installs the fonts and scenes the card pages load. The taskworker installs
// giftcardassets.FS before it runs tasks; a process without them cannot print
// cards.
func InstallGiftCardAssets(fsys fs.FS) {
	giftCardAssetsInstalled.Store(&giftCardAssetsHolder{fsys: fsys})
}

// Whether this process can print cards (the taskworker pins it).
func GiftCardAssetsInstalled() bool {
	return giftCardAssetsInstalled.Load() != nil
}

// The headless Chromium binary.
func giftCardChromiumPath() (string, error) {
	chromium := strings.TrimSpace(os.Getenv(GiftCardChromiumEnv))
	if chromium == "" {
		chromium = giftCardChromiumDefault
	}
	info, err := os.Stat(chromium)
	if err != nil {
		return "", fmt.Errorf("gift cards: no headless chromium at %s (set %s): %w", chromium, GiftCardChromiumEnv, err)
	}
	if info.IsDir() || info.Mode()&0111 == 0 {
		return "", fmt.Errorf("gift cards: %s is not an executable", chromium)
	}
	return chromium, nil
}

// One card: a code and its window.
type giftCard struct {
	secret   string
	redeemBy time.Time
	// 1-based, of count gifts in the order
	n     int
	count int
}

// One PDF: cards with the order's words.
type giftCardsDocument struct {
	language  string
	season    string
	message   string
	signature string
	bittensor bool
	// print "Gift n of count" (the buyer's PDF of several gifts)
	showCount bool
	cards     []*giftCard
}

// What card.html renders.
type giftCardsPage struct {
	Lang      string
	Dir       string
	Title     string
	Kicker    string
	GiftLabel string
	GiftValue string
	CodeLabel string
	QrAlt     string
	Steps     []string
	Tagline   string
	Bittensor string
	SceneSrc  string
	Cards     []*giftCardsPageCard
}

type giftCardsPageCard struct {
	Message    string
	Signature  string
	CodeGroups []string
	QrSrc      string
	RedeemBy   string
	GiftN      string
}

// The code as a card prints it: groups of four, the remainder last
// ("K7QX 3M2P NB4D LZ8R 9YWC 5AGH T6"). The QR code carries it plain.
func giftCardCodeGroups(secret string) []string {
	groups := []string{}
	for 4 < len(secret) {
		groups = append(groups, secret[:4])
		secret = secret[4:]
	}
	if secret != "" {
		groups = append(groups, secret)
	}
	return groups
}

// The code in groups on one line, for an email.
func giftCardCodeGrouped(secret string) string {
	return strings.Join(giftCardCodeGroups(secret), " ")
}

// The link the QR code and the friend's email open: ur.io's redeem page with
// the code filled in. The code rides in the fragment, which a browser never
// sends, so no server, load balancer or log sees it. A code is base32 (A-Z,
// 2-7) and needs no escaping.
func giftCardRedeemLink(secret string) string {
	return giftCardRedeemUrl + "#code=" + secret
}

// The QR code of the redeem link, black on white, 10 px a module with no quiet
// zone (the code box's white padding is the quiet zone).
func giftCardQrPng(secret string) ([]byte, error) {
	q, err := qrcode.New(giftCardRedeemLink(secret), qrcode.Medium)
	if err != nil {
		return nil, err
	}
	q.DisableBorder = true
	q.ForegroundColor = color.RGBA{R: 0x10, G: 0x10, B: 0x10, A: 0xff}
	q.BackgroundColor = color.RGBA{R: 0xff, G: 0xff, B: 0xff, A: 0xff}
	return q.PNG(-10)
}

// The note as a card prints it: trimmed, with at most one blank line in a row
// and at most giftCardNoteMaxLineBreaks breaks.
func giftCardNote(message string) string {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(message), "\r\n", "\n"), "\n")
	out := &strings.Builder{}
	breakCount := 0
	blank := false
	for i, line := range lines {
		line = strings.TrimRight(line, " \t")
		if i == 0 {
			out.WriteString(line)
			continue
		}
		if line == "" {
			if blank {
				continue
			}
			blank = true
		} else {
			blank = false
		}
		if breakCount < giftCardNoteMaxLineBreaks {
			out.WriteString("\n")
			breakCount += 1
		} else {
			out.WriteString(" ")
		}
		out.WriteString(line)
	}
	return strings.TrimSpace(out.String())
}

// The scene of the season in the assets, or "".
func giftCardScenePath(assets fs.FS, season string) string {
	if season == "" || strings.ContainsAny(season, "/\\.") {
		return ""
	}
	scenePath := path.Join("scenes", season+".png")
	if _, err := fs.Stat(assets, scenePath); err != nil {
		return ""
	}
	return scenePath
}

// The page of a document, its QR codes named with `qrPrefix`.
func giftCardsPageFor(document *giftCardsDocument, qrPrefix string, sceneSrc string) *giftCardsPage {
	language := giftCardLanguage(document.language)
	page := &giftCardsPage{
		Lang:      language,
		Dir:       giftCardDir(language),
		Title:     giftCardText(language, "card.title"),
		Kicker:    giftCardText(language, "card.kicker"),
		GiftLabel: giftCardText(language, "card.gift_label"),
		GiftValue: giftCardText(language, "card.gift_value"),
		CodeLabel: giftCardText(language, "card.code_label"),
		QrAlt:     giftCardText(language, "card.qr_alt"),
		Steps: []string{
			giftCardText(language, "card.step1"),
			giftCardText(language, "card.step2"),
			giftCardText(language, "card.step3"),
		},
		Tagline:  giftCardText(language, "card.tagline"),
		SceneSrc: sceneSrc,
	}
	if document.bittensor {
		page.Bittensor = giftCardText(language, "card.bittensor")
	}
	message := giftCardNote(document.message)
	signature := strings.Join(strings.Fields(document.signature), " ")
	for i, card := range document.cards {
		pageCard := &giftCardsPageCard{
			Message:    message,
			Signature:  signature,
			CodeGroups: giftCardCodeGroups(card.secret),
			QrSrc:      fmt.Sprintf("%s-%d.png", qrPrefix, i),
			RedeemBy:   giftCardText(language, "card.redeem_by", "date", giftCardDate(language, model.GiftRedeemByDate(card.redeemBy))),
		}
		if document.showCount && 1 < card.count {
			pageCard.GiftN = giftCardText(language, "card.gift_n", "n", fmt.Sprint(card.n), "count", fmt.Sprint(card.count))
		}
		page.Cards = append(page.Cards, pageCard)
	}
	return page
}

// The document's page as html.
func renderGiftCardsHtml(page *giftCardsPage) (string, error) {
	pageTemplate, err := giftCardsPageTemplate()
	if err != nil {
		return "", err
	}
	out := &strings.Builder{}
	if err := pageTemplate.Execute(out, page); err != nil {
		return "", err
	}
	return out.String(), nil
}

// Prints each document to a PDF, a card per page, in one browser.
func printGiftCardDocuments(ctx context.Context, documents []*giftCardsDocument) (pdfs [][]byte, returnErr error) {
	holder := giftCardAssetsInstalled.Load()
	if holder == nil {
		return nil, errors.New("gift cards: no assets installed (controller.InstallGiftCardAssets)")
	}
	chromium, err := giftCardChromiumPath()
	if err != nil {
		return nil, err
	}

	dir, err := os.MkdirTemp("", "urnetwork-gift-cards-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)

	pageUrls, err := writeGiftCardPages(holder.fsys, dir, documents)
	if err != nil {
		return nil, err
	}

	select {
	case giftCardBrowserSlots <- struct{}{}:
		defer func() { <-giftCardBrowserSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	browser, err := startGiftCardBrowser(ctx, chromium, dir)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := browser.close(); err != nil && returnErr == nil {
			glog.Infof("[gift]card printer did not close cleanly: %s\n", err)
		}
	}()
	for i, pageUrl := range pageUrls {
		pdf, fit, err := browser.printPage(ctx, pageUrl)
		if err != nil {
			return nil, fmt.Errorf("gift cards: print %d of %d: %w", i+1, len(pageUrls), err)
		}
		if fit != "fit" {
			glog.Infof("[gift]a card's note was cut to fit its page (%s)\n", fit)
		}
		pdfs = append(pdfs, pdf)
	}
	return pdfs, nil
}

// Writes each document's page into dir, with the fonts, the seasons' scenes and
// the QR codes beside it, and returns the pages' file urls.
func writeGiftCardPages(fsys fs.FS, dir string, documents []*giftCardsDocument) ([]string, error) {
	// the fonts, and each season's scene, beside the pages
	fontNames, err := fs.Glob(fsys, "fonts/*.ttf")
	if err != nil {
		return nil, err
	}
	if len(fontNames) == 0 {
		return nil, errors.New("gift cards: the assets hold no fonts")
	}
	if err := os.Mkdir(filepath.Join(dir, "fonts"), 0700); err != nil {
		return nil, err
	}
	for _, fontName := range fontNames {
		if err := giftCardCopyAsset(fsys, fontName, filepath.Join(dir, filepath.FromSlash(fontName))); err != nil {
			return nil, err
		}
	}
	pageUrls := []string{}
	scenes := map[string]string{}
	for i, document := range documents {
		sceneSrc, ok := scenes[document.season]
		if !ok {
			if scenePath := giftCardScenePath(fsys, document.season); scenePath != "" {
				sceneSrc = fmt.Sprintf("scene-%d.png", len(scenes))
				if err := giftCardCopyAsset(fsys, scenePath, filepath.Join(dir, sceneSrc)); err != nil {
					return nil, err
				}
			}
			scenes[document.season] = sceneSrc
		}
		qrPrefix := fmt.Sprintf("qr-%d", i)
		for j, card := range document.cards {
			qrPng, err := giftCardQrPng(card.secret)
			if err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s-%d.png", qrPrefix, j)), qrPng, 0600); err != nil {
				return nil, err
			}
		}
		pageHtml, err := renderGiftCardsHtml(giftCardsPageFor(document, qrPrefix, sceneSrc))
		if err != nil {
			return nil, err
		}
		pageName := fmt.Sprintf("cards-%d.html", i)
		if err := os.WriteFile(filepath.Join(dir, pageName), []byte(pageHtml), 0600); err != nil {
			return nil, err
		}
		pageUrls = append(pageUrls, (&url.URL{Scheme: "file", Path: filepath.ToSlash(filepath.Join(dir, pageName))}).String())
	}
	return pageUrls, nil
}

func giftCardCopyAsset(fsys fs.FS, name string, destination string) error {
	source, err := fsys.Open(name)
	if err != nil {
		return err
	}
	defer source.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// The cards of a paid order, in order: for delivery "them", by recipient.
func giftOrderCards(order *model.GiftOrder, codes []*model.GiftCode) []*model.GiftCode {
	ordered := slices.Clone(codes)
	if order.Delivery == model.GiftDeliveryThem {
		slices.SortStableFunc(ordered, func(a *model.GiftCode, b *model.GiftCode) int {
			switch {
			case a.RecipientIndex == nil && b.RecipientIndex == nil:
				return 0
			case a.RecipientIndex == nil:
				return 1
			case b.RecipientIndex == nil:
				return -1
			default:
				return *a.RecipientIndex - *b.RecipientIndex
			}
		})
	}
	return ordered
}

// The documents of a paid order with the part each is stored as: "all", then
// for delivery "them" the "r<index>" of each friend with an address.
func giftOrderCardDocuments(order *model.GiftOrder, codes []*model.GiftCode) (documents []*giftCardsDocument, parts []string) {
	newDocument := func(showCount bool) *giftCardsDocument {
		return &giftCardsDocument{
			language:  order.Language,
			season:    order.Season,
			message:   order.Message,
			signature: order.Signature,
			bittensor: order.BittensorFooter,
			showCount: showCount,
		}
	}
	ordered := giftOrderCards(order, codes)
	all := newDocument(true)
	for i, code := range ordered {
		all.cards = append(all.cards, &giftCard{
			secret:   code.Secret,
			redeemBy: code.RedeemBy,
			n:        i + 1,
			count:    len(ordered),
		})
	}
	documents = append(documents, all)
	parts = append(parts, model.GiftOrderPdfPartAll)
	if order.Delivery == model.GiftDeliveryThem {
		for i, code := range ordered {
			// a friend's card only for a friend to email (the retention blanks
			// the addresses of an order left unpaid)
			if code.RecipientIndex == nil || len(order.Recipients) <= *code.RecipientIndex ||
				strings.TrimSpace(order.Recipients[*code.RecipientIndex]) == "" {
				continue
			}
			friend := newDocument(false)
			friend.cards = []*giftCard{{
				secret:   code.Secret,
				redeemBy: code.RedeemBy,
				n:        i + 1,
				count:    len(ordered),
			}}
			documents = append(documents, friend)
			parts = append(parts, model.GiftOrderPdfRecipientPart(*code.RecipientIndex))
		}
	}
	return
}

// Prints a paid order's PDFs, by part.
func renderGiftOrderPdfs(ctx context.Context, order *model.GiftOrder, codes []*model.GiftCode) (map[string][]byte, error) {
	if len(codes) == 0 {
		return nil, fmt.Errorf("gift order %s has no codes", order.GiftOrderId)
	}
	documents, parts := giftOrderCardDocuments(order, codes)
	pdfs, err := printGiftCardDocuments(ctx, documents)
	if err != nil {
		return nil, err
	}
	byPart := map[string][]byte{}
	for i, part := range parts {
		byPart[part] = pdfs[i]
	}
	return byPart, nil
}

// The credential Chromium starts with: none when this process is not root
// (Chromium runs as this process's user, as in development), else the printer's
// user (GiftCardChromiumUserEnv, by default the image's "giftcard") with no
// supplementary groups. Chromium never runs as root: without that user there
// is no print.
func giftCardChromiumCredential() (*syscall.Credential, error) {
	if os.Geteuid() != 0 {
		return nil, nil
	}
	name := strings.TrimSpace(os.Getenv(GiftCardChromiumUserEnv))
	if name == "" {
		name = giftCardChromiumUserDefault
	}
	printer, err := user.Lookup(name)
	if err != nil {
		return nil, fmt.Errorf("gift cards: chromium does not run as root, and its user %q does not exist: %w", name, err)
	}
	uid, uidErr := strconv.ParseUint(printer.Uid, 10, 32)
	gid, gidErr := strconv.ParseUint(printer.Gid, 10, 32)
	if uidErr != nil || gidErr != nil || uid == 0 || gid == 0 {
		return nil, fmt.Errorf("gift cards: chromium's user %q is root or unreadable (%s:%s)", name, printer.Uid, printer.Gid)
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}, nil
}

// Gives the print directory and everything in it to the user.
func giftCardChownTree(dir string, uid int, gid int) error {
	return filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return os.Lchown(path, uid, gid)
	})
}

// The fontconfig settings the browser keeps from the taskworker's environment.
var giftCardChromiumFontconfigEnv = []string{"FONTCONFIG_FILE", "FONTCONFIG_PATH", "FONTCONFIG_SYSROOT"}

// The browser's whole environment: a fixed PATH, a home, temporary and XDG
// directories inside its print directory, a UTF-8 locale, and fontconfig's
// settings. Nothing else of the taskworker's environment, which holds the
// server's secrets, reaches it.
func giftCardChromiumEnvironment(dir string) ([]string, error) {
	home := filepath.Join(dir, "home")
	tmp := filepath.Join(dir, "tmp")
	for _, subdir := range []string{home, tmp} {
		if err := os.MkdirAll(subdir, 0700); err != nil {
			return nil, err
		}
	}
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + home,
		"TMPDIR=" + tmp,
		"XDG_CONFIG_HOME=" + filepath.Join(home, ".config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, ".cache"),
		"XDG_DATA_HOME=" + filepath.Join(home, ".local", "share"),
		"LANG=C.UTF-8",
	}
	for _, name := range giftCardChromiumFontconfigEnv {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env, nil
}

// One headless Chromium, driven over the DevTools protocol on a pipe: the
// browser reads commands on fd 3 and writes replies and events on fd 4, each
// message JSON ended by a NUL.
type giftCardBrowser struct {
	cmd        *exec.Cmd
	toChrome   *os.File
	fromChrome *os.File
	// the browser's stderr, a file so that waiting for the browser does not
	// wait for helper processes that inherited it
	stderrPath string
	exited     chan struct{}

	writeMutex sync.Mutex
	nextId     atomic.Int64

	stateMutex sync.Mutex
	pending    map[int64]chan *giftCardCdpMessage
	readErr    error
	readDone   chan struct{}

	loads chan string

	sessionId string
}

type giftCardCdpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type giftCardCdpMessage struct {
	Id        int64             `json:"id,omitempty"`
	Method    string            `json:"method,omitempty"`
	Params    json.RawMessage   `json:"params,omitempty"`
	SessionId string            `json:"sessionId,omitempty"`
	Result    json.RawMessage   `json:"result,omitempty"`
	Error     *giftCardCdpError `json:"error,omitempty"`
}

// The most of the browser's stderr an error message carries.
const giftCardStderrTailByteCount = 2048

// The end of what the browser wrote to stderr.
func (self *giftCardBrowser) stderrTail() string {
	data, err := os.ReadFile(self.stderrPath)
	if err != nil {
		return ""
	}
	if giftCardStderrTailByteCount < len(data) {
		data = data[len(data)-giftCardStderrTailByteCount:]
	}
	return strings.TrimSpace(string(data))
}

// Starts the browser in its own process group with a fresh profile under dir,
// and opens one page to print with.
func startGiftCardBrowser(ctx context.Context, chromium string, dir string) (*giftCardBrowser, error) {
	stderrPath := filepath.Join(dir, "chromium-stderr.log")
	stderr, err := os.OpenFile(stderrPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	defer stderr.Close()
	toChromeRead, toChromeWrite, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	fromChromeRead, fromChromeWrite, err := os.Pipe()
	if err != nil {
		toChromeRead.Close()
		toChromeWrite.Close()
		return nil, err
	}
	cmd := exec.Command(
		chromium,
		"--headless",
		"--no-sandbox",
		"--disable-gpu",
		"--disable-dev-shm-usage",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-extensions",
		"--disable-background-networking",
		"--disable-component-update",
		"--disable-default-apps",
		"--disable-sync",
		"--hide-scrollbars",
		"--mute-audio",
		// no host resolves: the pages load only the files beside them
		"--host-resolver-rules=MAP * ~NOTFOUND",
		"--user-data-dir="+filepath.Join(dir, "profile"),
		"--remote-debugging-pipe",
		"about:blank",
	)
	cmd.Stdout = nil
	cmd.Stderr = stderr
	// an environment and a directory of its own: nothing of the taskworker's
	env, err := giftCardChromiumEnvironment(dir)
	if err != nil {
		toChromeRead.Close()
		toChromeWrite.Close()
		fromChromeRead.Close()
		fromChromeWrite.Close()
		return nil, err
	}
	cmd.Env = env
	cmd.Dir = dir
	cmd.ExtraFiles = []*os.File{toChromeRead, fromChromeWrite}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// never as root: as the printer's own user, which owns its directory and
	// nothing else of the taskworker's
	credential, err := giftCardChromiumCredential()
	if err == nil && credential != nil {
		err = giftCardChownTree(dir, int(credential.Uid), int(credential.Gid))
	}
	if err != nil {
		toChromeRead.Close()
		toChromeWrite.Close()
		fromChromeRead.Close()
		fromChromeWrite.Close()
		return nil, err
	}
	cmd.SysProcAttr.Credential = credential
	if err := cmd.Start(); err != nil {
		toChromeRead.Close()
		toChromeWrite.Close()
		fromChromeRead.Close()
		fromChromeWrite.Close()
		return nil, fmt.Errorf("gift cards: start %s: %w", chromium, err)
	}
	// the browser's ends
	toChromeRead.Close()
	fromChromeWrite.Close()

	browser := &giftCardBrowser{
		cmd:        cmd,
		toChrome:   toChromeWrite,
		fromChrome: fromChromeRead,
		stderrPath: stderrPath,
		exited:     make(chan struct{}),
		pending:    map[int64]chan *giftCardCdpMessage{},
		readDone:   make(chan struct{}),
		loads:      make(chan string, 16),
	}
	go func() {
		defer close(browser.exited)
		cmd.Wait()
	}()
	go browser.read()

	startCtx, startCancel := context.WithTimeout(ctx, giftCardBrowserStartTimeout)
	defer startCancel()
	if err := browser.openPage(startCtx); err != nil {
		browser.close()
		return nil, fmt.Errorf("%w: %s", err, browser.stderrTail())
	}
	return browser, nil
}

// Reads replies and events until the pipe ends.
func (self *giftCardBrowser) read() {
	reader := bufio.NewReaderSize(self.fromChrome, 1024*1024)
	var readErr error
	for {
		frame, err := reader.ReadBytes(0)
		if err != nil {
			readErr = err
			break
		}
		message := &giftCardCdpMessage{}
		if err := json.Unmarshal(frame[:len(frame)-1], message); err != nil {
			readErr = fmt.Errorf("unreadable devtools message: %w", err)
			break
		}
		if message.Id != 0 {
			self.stateMutex.Lock()
			reply, ok := self.pending[message.Id]
			delete(self.pending, message.Id)
			self.stateMutex.Unlock()
			if ok {
				reply <- message
			}
			continue
		}
		if message.Method == "Page.loadEventFired" {
			select {
			case self.loads <- message.SessionId:
			default:
			}
		}
	}
	self.stateMutex.Lock()
	self.readErr = readErr
	self.stateMutex.Unlock()
	close(self.readDone)
}

// Sends one command and returns its result.
func (self *giftCardBrowser) call(ctx context.Context, sessionId string, method string, params any, result any) error {
	id := self.nextId.Add(1)
	reply := make(chan *giftCardCdpMessage, 1)
	self.stateMutex.Lock()
	if self.readErr != nil {
		err := self.readErr
		self.stateMutex.Unlock()
		return fmt.Errorf("gift cards: the browser closed its pipe (%v): %s", err, self.stderrTail())
	}
	self.pending[id] = reply
	self.stateMutex.Unlock()
	defer func() {
		self.stateMutex.Lock()
		delete(self.pending, id)
		self.stateMutex.Unlock()
	}()

	command := map[string]any{
		"id":     id,
		"method": method,
	}
	if params != nil {
		command["params"] = params
	}
	if sessionId != "" {
		command["sessionId"] = sessionId
	}
	frame, err := json.Marshal(command)
	if err != nil {
		return err
	}
	self.writeMutex.Lock()
	_, err = self.toChrome.Write(append(frame, 0))
	self.writeMutex.Unlock()
	if err != nil {
		return fmt.Errorf("gift cards: write %s: %w", method, err)
	}

	select {
	case message := <-reply:
		if message.Error != nil {
			return fmt.Errorf("gift cards: %s: %s (%d)", method, message.Error.Message, message.Error.Code)
		}
		if result != nil && message.Result != nil {
			return json.Unmarshal(message.Result, result)
		}
		return nil
	case <-self.readDone:
		return fmt.Errorf("gift cards: the browser exited during %s: %s", method, self.stderrTail())
	case <-ctx.Done():
		return fmt.Errorf("gift cards: %s: %w", method, ctx.Err())
	}
}

// Opens the page the documents print in, and attaches to it.
func (self *giftCardBrowser) openPage(ctx context.Context) error {
	var target struct {
		TargetId string `json:"targetId"`
	}
	if err := self.call(ctx, "", "Target.createTarget", map[string]any{"url": "about:blank"}, &target); err != nil {
		return err
	}
	var session struct {
		SessionId string `json:"sessionId"`
	}
	if err := self.call(ctx, "", "Target.attachToTarget", map[string]any{"targetId": target.TargetId, "flatten": true}, &session); err != nil {
		return err
	}
	self.sessionId = session.SessionId
	if err := self.call(ctx, self.sessionId, "Page.enable", nil, nil); err != nil {
		return err
	}
	// no page script runs, whatever a page carries; the fitting is a DevTools
	// evaluation
	return self.call(ctx, self.sessionId, "Emulation.setScriptExecutionDisabled", map[string]any{"value": true}, nil)
}

// The DevTools evaluation run before each print, the page's only script: it
// waits for the fonts and images, then fits each card to its page. The code
// stays on one line, smaller if its column is narrow. The note (the buyer's own
// text, up to 240 characters in any script) shrinks from 27 px to 12 px until
// the card fits; a note that does not fit even then is cut, so the code, the
// steps and the footer always print. Each fit starts over. Returns "fit", "cut"
// when a note was cut, or the faces that failed to load.
const giftCardPrepareScript = `(async () => {
	const fit = () => {
		const maxSize = 27;
		const minSize = 12;
		let result = "fit";
		for (const card of document.querySelectorAll(".gc")) {
			const overflow = () => card.scrollHeight - card.clientHeight;
			const code = card.querySelector(".gc-code code");
			if (code) {
				code.style.fontSize = "";
				let codeSize = parseFloat(getComputedStyle(code).fontSize);
				while (code.clientWidth + 0.5 < code.scrollWidth && 10 < codeSize) {
					codeSize -= 0.5;
					code.style.fontSize = codeSize + "px";
				}
			}
			const note = card.querySelector(".gc-msg");
			if (note) {
				note.style.maxHeight = "";
				note.style.overflow = "";
			}
			let size = maxSize;
			card.style.setProperty("--hand-size", size + "px");
			while (0.5 < overflow() && minSize < size) {
				size -= 1;
				card.style.setProperty("--hand-size", size + "px");
			}
			if (0.5 < overflow() && note) {
				note.style.overflow = "hidden";
				note.style.maxHeight = Math.max(0, note.clientHeight - Math.ceil(overflow())) + "px";
				result = "cut";
			}
		}
		return result;
	};
	await document.fonts.ready;
	await Promise.all(Array.from(document.images).map((image) => image.decode().catch(() => null)));
	fit();
	await document.fonts.ready;
	const result = fit();
	const failed = Array.from(document.fonts).filter((face) => face.status === "error").map((face) => face.family);
	return failed.length ? "fonts failed: " + failed.join(", ") : result;
})()`

// Loads the page, prepares it and prints it. Returns the PDF and the fit
// ("fit", or "cut" when a note was cut to fit its card).
func (self *giftCardBrowser) printPage(ctx context.Context, pageUrl string) (pdf []byte, fit string, returnErr error) {
	printCtx, printCancel := context.WithTimeout(ctx, giftCardPagePrintTimeout)
	defer printCancel()

	if err := self.loadPage(printCtx, pageUrl); err != nil {
		return nil, "", err
	}
	value, err := self.evaluate(printCtx, giftCardPrepareScript)
	if err != nil {
		return nil, "", err
	}
	fit, _ = value.(string)
	if fit != "fit" && fit != "cut" {
		return nil, "", fmt.Errorf("gift cards: prepare: %v", value)
	}

	var printed struct {
		Data string `json:"data"`
	}
	err = self.call(printCtx, self.sessionId, "Page.printToPDF", map[string]any{
		"printBackground":     true,
		"preferCSSPageSize":   true,
		"displayHeaderFooter": false,
		"marginTop":           0,
		"marginBottom":        0,
		"marginLeft":          0,
		"marginRight":         0,
	}, &printed)
	if err != nil {
		return nil, "", err
	}
	pdf, err = base64.StdEncoding.DecodeString(printed.Data)
	if err != nil {
		return nil, "", err
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF-")) {
		return nil, "", errors.New("gift cards: the browser printed no pdf")
	}
	return pdf, fit, nil
}

// Navigates the page to the url and waits for its load event.
func (self *giftCardBrowser) loadPage(ctx context.Context, pageUrl string) error {
	// a load of an earlier page must not count for this one
	for drained := false; !drained; {
		select {
		case <-self.loads:
		default:
			drained = true
		}
	}
	var navigation struct {
		ErrorText string `json:"errorText"`
	}
	if err := self.call(ctx, self.sessionId, "Page.navigate", map[string]any{"url": pageUrl}, &navigation); err != nil {
		return err
	}
	if navigation.ErrorText != "" {
		return fmt.Errorf("gift cards: navigate: %s", navigation.ErrorText)
	}
	for loaded := false; !loaded; {
		select {
		case sessionId := <-self.loads:
			loaded = sessionId == self.sessionId
		case <-self.readDone:
			return fmt.Errorf("gift cards: the browser exited loading the page: %s", self.stderrTail())
		case <-ctx.Done():
			return fmt.Errorf("gift cards: load: %w", ctx.Err())
		}
	}
	return nil
}

// Evaluates the expression in the page as DevTools does, unaffected by the
// page's disabled scripts and policy, and returns its value.
func (self *giftCardBrowser) evaluate(ctx context.Context, expression string) (any, error) {
	var evaluation struct {
		Result struct {
			Value any `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text      string `json:"text"`
			Exception *struct {
				Description string `json:"description"`
			} `json:"exception"`
		} `json:"exceptionDetails"`
	}
	err := self.call(ctx, self.sessionId, "Runtime.evaluate", map[string]any{
		"expression":    expression,
		"awaitPromise":  true,
		"returnByValue": true,
	}, &evaluation)
	if err != nil {
		return nil, err
	}
	if evaluation.ExceptionDetails != nil {
		text := evaluation.ExceptionDetails.Text
		if evaluation.ExceptionDetails.Exception != nil {
			text += " " + evaluation.ExceptionDetails.Exception.Description
		}
		return nil, fmt.Errorf("gift cards: evaluate: %s", text)
	}
	return evaluation.Result.Value, nil
}

// Asks the browser to close and waits for it, then kills whatever is left of
// its process group. A browser that does not close in time is killed too, and
// that is the error.
func (self *giftCardBrowser) close() (returnErr error) {
	closeCtx, closeCancel := context.WithTimeout(context.Background(), giftCardBrowserCloseTimeout)
	defer closeCancel()
	// the browser may exit before it answers
	self.call(closeCtx, "", "Browser.close", nil, nil)
	self.toChrome.Close()
	select {
	case <-self.exited:
	case <-closeCtx.Done():
		returnErr = errors.New("gift cards: the browser did not close in time and was killed")
	}
	// the browser if it still runs, and any helper process it left
	syscall.Kill(-self.cmd.Process.Pid, syscall.SIGKILL)
	<-self.exited
	self.fromChrome.Close()
	<-self.readDone
	return
}

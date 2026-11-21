//go:build sidecar_pending && sidecar_testhooks

package main

// FS-31 at T3 (SMTP-M9-30; §22.5's FS-31 row, D-SMTP-16's adversarial corpus, DESIGN-DETAILS 3.6): the rows
// sidecar/smtpproof's fs31_test.go marks T3, each in every cell a crypto/tls-based ScriptedMx serves, rerun on the
// block's engine as main composes it (newSmtpEdgeHost) over the private stack's Postgres, the test's own database, and
// its Redis, through .stack/server-test.sh with -tags sidecar_pending,sidecar_testhooks, as s6_test.go runs S6:
//
//   - the store is model.NewSmtpEdgeStore, whose smtp_send gets each connection's record through the send log;
//   - the quota is the {smq_} scripts on the stack's Redis, and the gate the block's own over model.GetSmtpGateFacts
//     for a root, network and zone made as the dns block makes them, mail_send answered for the test's device;
//   - the verifier is smtpproof's Verifier holding a relation key set (smtpproof.RelationKeySet, the
//     sidecar_testhooks build's), since no setup has run (SMTP-M9-27), so every session is smtpproof's edgeSession
//     and every part is held to the reference relation;
//   - the sender is smtpproof's Prover under that set, each stream reaching the engine as the ur/smtpedge listener
//     hands one over and its parts through the root's own channel (sidecar.NewSmtpProofTestChannel);
//   - the world is sidecartest's MxWorld with one ScriptedMx at the recipient domain, and the sending address's FCrDNS.
//
// A malicious sender's rows here are those its wire alone expresses, since the witness is smtpproof's own: a part's
// reply_count one fewer or one more than its records hold, and a message part covering none of the envelope's replies.
// The rest of FS-31's adversarial rows, which change a witness, run at T2 (smtpproof/fs31_test.go).
//
// The assertion is the MX's own record: a delivered row leaves exactly the proven transaction, and any other leaves
// none and no command accepted inside TLS but the honest dialog's. After each cell the block closes, which writes its
// send log, and smtp_send holds one record for the connection, the network's, naming no more recipients than its
// preamble declared.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"
	"github.com/urnetwork/sidecar/smtpedgeservice"
	"github.com/urnetwork/sidecar/smtpproof"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The recipient domain and its MX, which the block's world serves, and the MX's address.
const (
	t3Fs31RecipientDomain = "recipient.example"
	t3Fs31MxHost          = "mx.recipient.example"
	// 26 lowercase base32 characters, the engine's selector form (plan-SMTP-small-readings #5)
	t3Fs31Selector = "fsaaaaaaaaaaaaaaaaaaaaaaaa"
	// §14.6's second signature's constant prefix, which smtpproof's Z5 holds the header block to
	t3Fs31DkimPrefix = "DKIM-Signature: v=1; a=rsa-sha256; c=relaxed/relaxed; h=from:date:message-id:x-ur-ref:feedback-id; "
)

var t3Fs31MxAddress = netip.MustParseAddr("192.0.2.25")

// One cell: a TLS leg a ScriptedMx serves, and a mode.
type t3Fs31Cell struct {
	name    string
	version uint16
	suite   uint16
	rsa     bool
	data    bool
}

func (self *t3Fs31Cell) String() string {
	if self.data {
		return self.name + " DATA"
	}
	return self.name + " BDAT"
}

func t3Fs31Cells() []*t3Fs31Cell {
	var cells []*t3Fs31Cell
	for _, leg := range []*t3Fs31Cell{
		{name: "tls13 aes", version: tls.VersionTLS13, suite: tls.TLS_AES_128_GCM_SHA256},
		{name: "tls13 chacha", version: tls.VersionTLS13, suite: tls.TLS_CHACHA20_POLY1305_SHA256},
		{name: "tls12 ecdsa", version: tls.VersionTLS12, suite: tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		{name: "tls12 rsa", version: tls.VersionTLS12, suite: tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256, rsa: true},
	} {
		for _, data := range []bool{false, true} {
			cell := *leg
			cell.data = data
			cells = append(cells, &cell)
		}
	}
	return cells
}

// What a T3 row holds each cell to: delivered, or ended by the MX's reply, or the edge's refusal of a part.
type t3Fs31Expect struct {
	delivered   bool
	refusedCode string
	refusedPart uint32
	// The relation's check, where the verifier refused the part; empty where the gate refused it before verifying.
	refusedCheck string
}

// One row smtpproof's fs31_test.go marks T3.
type t3Fs31Row struct {
	name    string
	applies func(cell *t3Fs31Cell) bool
	script  func(script *sidecartest.MxScript)
	// The recipients and the header block's lines after its single fields and signature, and the body.
	recipients     []string
	extraHeaders   func(cell *t3Fs31Cell, networkDomain string) string
	body           string
	groupMaxCount  int
	recipientLimit int
	// Lets the MX's EHLO reply choose the mode, where the DATA column would force DATA.
	unforced bool
	// A malicious sender's change to a part on its way, by its wire fields alone.
	part   func(part *sidecar.SmtpProofPart, parts []*sidecar.SmtpProofPart)
	expect t3Fs31Expect
	check  func(t testing.TB, name string, mx *sidecartest.ScriptedMx, result *sidecar.SmtpProofResult)
}

func t3Fs31Every(cell *t3Fs31Cell) bool { return true }
func t3Fs31Data(cell *t3Fs31Cell) bool  { return cell.data }
func t3Fs31Bdat(cell *t3Fs31Cell) bool  { return !cell.data }

func t3Fs31Without(keywords []string, keyword string) []string {
	return slices.DeleteFunc(slices.Clone(keywords), func(offered string) bool {
		return strings.EqualFold(strings.Fields(offered + " ")[0], keyword)
	})
}

// A 16 KiB header block's lines after the single fields and the signature, which the rest of the block, from the
// network's domain, brings to 16,384 octets: fillers of 800 octets a line.
func t3Fs31LongHeaders(cell *t3Fs31Cell, networkDomain string) string {
	base := len(t3Fs31HeaderBlock("1."+strings.Repeat("a", 34), "n:p:"+strings.Repeat("a", 26)+":urnetwork", networkDomain, ""))
	remaining := 16*1024 - base
	var extra strings.Builder
	line := func(count int) {
		prefix := fmt.Sprintf("X-Filler-%02d: ", strings.Count(extra.String(), "\r\n"))
		extra.WriteString(prefix + strings.Repeat("f", count-len(prefix)-len("\r\n")) + "\r\n")
	}
	for 1600 < remaining {
		line(800)
		remaining -= 800
	}
	if 800 < remaining {
		line(remaining / 2)
		remaining -= remaining / 2
	}
	line(remaining)
	return extra.String()
}

// The rows smtpproof's fs31_test.go marks T3.
func t3Fs31Rows() []*t3Fs31Row {
	smuggled := "MAIL FROM:<mallory@other.example>\r\nRCPT TO:<victim@other.example>\r\nDATA\r\n"
	hundred := make([]string, 0, 100)
	for i := range 100 {
		hundred = append(hundred, fmt.Sprintf("r%03d", i))
	}
	return []*t3Fs31Row{
		{name: "an honest send", applies: t3Fs31Every, expect: t3Fs31Expect{delivered: true}},
		{name: "an honest send to 100 recipients", applies: t3Fs31Every, recipients: hundred, recipientLimit: 100, expect: t3Fs31Expect{delivered: true}},
		{name: "an honest send with a 16 KiB header block", applies: t3Fs31Every, extraHeaders: t3Fs31LongHeaders, expect: t3Fs31Expect{delivered: true}},
		{
			name: "an MX without CHUNKING", applies: t3Fs31Data, unforced: true,
			script: func(script *sidecartest.MxScript) { script.Keywords = t3Fs31Without(script.Keywords, "CHUNKING") },
			expect: t3Fs31Expect{delivered: true},
		},
		{
			name: "an MX offering CHUNKING only before TLS", applies: t3Fs31Data, unforced: true,
			script: func(script *sidecartest.MxScript) {
				script.PreTlsKeywords = append(slices.Clone(script.PreTlsKeywords), "CHUNKING")
				script.Keywords = t3Fs31Without(script.Keywords, "CHUNKING")
			},
			expect: t3Fs31Expect{delivered: true},
		},
		{
			name: "an MX without PIPELINING, refusing a command sent ahead of a reply", applies: t3Fs31Every,
			script: func(script *sidecartest.MxScript) {
				script.Keywords = t3Fs31Without(script.Keywords, "PIPELINING")
				script.RefusePipelining = true
			},
			expect: t3Fs31Expect{delivered: true},
			check: func(t testing.TB, name string, mx *sidecartest.ScriptedMx, result *sidecar.SmtpProofResult) {
				if mx.PipeliningViolated() {
					t.Errorf("%s: a command reached the MX ahead of a reply", name)
				}
			},
		},
		{
			name: "an MX refusing MAIL", applies: t3Fs31Every,
			script: func(script *sidecartest.MxScript) { script.Replies["MAIL"] = "550 5.7.1 Sender refused" },
			expect: t3Fs31Expect{refusedCode: "550"},
		},
		{
			name: "an MX refusing every RCPT", applies: t3Fs31Every,
			script: func(script *sidecartest.MxScript) {
				script.RcptReplies["bob"] = "550 5.1.1 Recipient refused"
				script.RcptReplies["carol"] = "550 5.1.1 Recipient refused"
			},
			expect: t3Fs31Expect{refusedCode: "550"},
		},
		{
			name: "an MX refusing one RCPT of two", applies: t3Fs31Every,
			script: func(script *sidecartest.MxScript) { script.RcptReplies["carol"] = "550 5.1.1 Recipient refused" },
			expect: t3Fs31Expect{refusedCode: "550"},
		},
		{
			name: "an MX answering DATA with no 354", applies: t3Fs31Data,
			script: func(script *sidecartest.MxScript) { script.Replies["DATA"] = "554 5.5.1 Transaction failed" },
			expect: t3Fs31Expect{refusedCode: "554"},
		},
		{
			name: "an MX answering the message 451", applies: t3Fs31Every,
			script: func(script *sidecartest.MxScript) { script.MessageReply = "451 4.3.0 Try again later" },
			expect: t3Fs31Expect{refusedCode: "451"},
		},
		{
			name: "a second transaction in the BDAT body, the MX offering CHUNKING", applies: t3Fs31Bdat,
			body:   "a line\r\n.\r\n" + smuggled,
			expect: t3Fs31Expect{delivered: true},
			check: func(t testing.TB, name string, mx *sidecartest.ScriptedMx, result *sidecar.SmtpProofResult) {
				mails := 0
				for _, command := range mx.Accepted() {
					if strings.HasPrefix(command, "MAIL FROM:") {
						mails += 1
					}
				}
				if mails != 1 {
					t.Errorf("%s: the MX accepted %d MAIL commands", name, mails)
				}
			},
		},
		{
			name: "reply_count one fewer than the records hold, the envelope one command a group", applies: t3Fs31Every, groupMaxCount: 1,
			part: func(part *sidecar.SmtpProofPart, parts []*sidecar.SmtpProofPart) {
				if part.Index == 3 {
					part.ReplyCount -= 1
				}
			},
			expect: t3Fs31Expect{refusedPart: 3, refusedCheck: "Z8"},
		},
		{
			name: "reply_count one more than the records hold", applies: t3Fs31Every,
			part: func(part *sidecar.SmtpProofPart, parts []*sidecar.SmtpProofPart) {
				if part.Index == 3 {
					part.ReplyCount += 1
				}
			},
			expect: t3Fs31Expect{refusedPart: 3},
		},
		{
			name: "a message record before the envelope's replies are proven", applies: t3Fs31Every,
			part: func(part *sidecar.SmtpProofPart, parts []*sidecar.SmtpProofPart) {
				if part.Index == 3 {
					part.ServerRecordCount = parts[1].ServerRecordCount
					part.ReplyCount = 0
				}
			},
			expect: t3Fs31Expect{refusedPart: 3},
		},
	}
}

// A header block in SMTP-M9-04's order: the single fields and the fixed signature first, then extra, then the final
// CRLF.
func t3Fs31HeaderBlock(xUrRef string, feedbackId string, networkDomain string, extra string) string {
	return "X-UR-Ref: " + xUrRef + "\r\n" +
		"Feedback-ID: " + feedbackId + "\r\n" +
		"From: Alice Example <alice@" + networkDomain + ">\r\n" +
		"Message-ID: <abcdefghijklmnopqrstuvwxyz@" + networkDomain + ">\r\n" +
		t3Fs31DkimPrefix + "d=" + networkDomain + "; s=" + t3Fs31Selector + "; t=1; x=2; bh=AAAA; b=BBBB\r\n" +
		"To: <bob@" + t3Fs31RecipientDomain + ">\r\n" +
		"Subject: A synthetic message\r\n" +
		"Date: Fri, 06 Nov 2026 12:00:00 +0000\r\n" +
		extra +
		"\r\n"
}

// A message as DATA carries it, a leading dot doubled on each line.
func t3Fs31DotStuffed(message []byte) []byte {
	stuffed := make([]byte, 0, len(message))
	lineStart := true
	for _, octet := range message {
		if lineStart && octet == '.' {
			stuffed = append(stuffed, '.')
		}
		stuffed = append(stuffed, octet)
		lineStart = octet == '\n'
	}
	return stuffed
}

// The block one cell runs on: the engine as main composes it over the stack's stores, in a world whose recipient
// domain's MX is the cell's ScriptedMx.
type t3Fs31Block struct {
	t        testing.TB
	ctx      context.Context
	cancel   context.CancelFunc
	clock    *sidecartest.FakeClock
	start    time.Time
	world    *sidecartest.MxWorld
	host     *smtpEdgeHost
	gate     *t3SendGate
	store    *model.SmtpEdgeStore
	listener *smtpedgeservice.SmtpEdgeTestListener
	keySet   *smtpproof.RelationKeySet

	networkId     sidecar.Id
	networkDomain string
	edgeClientId  sidecar.Id
	clientId      sidecar.Id
	sidecarId     sidecar.Id
	serving       sync.WaitGroup
	closeOnce     sync.Once
}

// A block for one cell, the engine started and its gate holding the network's facts.
func newT3Fs31Block(t testing.TB, mx *sidecartest.ScriptedMx, recipientLimit int) *t3Fs31Block {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now().UTC().Truncate(time.Second)
	clock := sidecartest.NewFakeClock(start)
	root, networkId, label := newT3SendNetwork(t, ctx, start)
	keySet, err := smtpproof.NewRelationKeySet()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	self := &t3Fs31Block{
		t:             t,
		ctx:           ctx,
		cancel:        cancel,
		clock:         clock,
		start:         start,
		keySet:        keySet,
		networkId:     networkId,
		networkDomain: label + "." + root,
		edgeClientId:  sidecar.NewId(),
		clientId:      sidecar.NewId(),
		sidecarId:     sidecar.NewId(),
	}
	t.Cleanup(self.close)

	worldSettings := sidecartest.DefaultMxWorldSettings()
	worldSettings.Clock = clock
	if self.world, err = sidecartest.NewMxWorld(worldSettings); err != nil {
		t.Fatal(err)
	}
	heloName := "o1.out." + root
	for _, err := range []error{
		self.world.SetMx(t3Fs31RecipientDomain, &sidecar.SmtpMxHost{Preference: 10, Host: t3Fs31MxHost}),
		self.world.AddMx(t3Fs31MxHost, t3Fs31MxAddress, mx),
		self.world.AddMx(heloName, t3SendSendingAddress, nil),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	self.world.SetPtr(t3SendSendingAddress, heloName)

	config, err := smtpedgeservice.LoadSmtpEdgeConfig([]byte(fmt.Sprintf(`
root: %s
report_host: rpt.%s
helo_domain: out.%s
sending_addresses:
  - {address: %s, helo: %s}
`, root, root, root, t3SendSendingAddress, heloName)), 0)
	if err != nil {
		t.Fatal(err)
	}
	keyFile := "issue_key_id: 1\nkeys:\n  - {id: 1, key: " + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32)) + "}\n"
	if err := model.ApplySmtpXUrRefKeyFile(config.Settings, []byte(keyFile)); err != nil {
		t.Fatal(err)
	}
	config.Settings.MxRefusedPrefixes = slices.DeleteFunc(slices.Clone(config.Settings.MxRefusedPrefixes), func(prefix netip.Prefix) bool {
		return slices.Contains(t3SendDocumentationPrefixes, prefix)
	})

	fakes := sidecartest.NewFakeHost(ctx, clock, nil, nil, nil)
	limits := t3SendLimits()
	if 0 < recipientLimit {
		limits.Limits[smtpedgeservice.SmtpLimitRecipientsPerMessage] = int64(recipientLimit)
	}
	fakes.Quota.SetLimits(connect.Id(networkId), limits)
	fakes.Quota.SetBalance(connect.Id(networkId), 1024*1024*1024)
	fakes.Gates.OpenService(sidecar.ServiceGateSmtpedge, connect.Id(networkId))
	fakes.Abuse.Set(connect.Id(networkId), sidecar.AbuseStandingStandard, true)

	hostSettings := defaultHostSettings(config)
	self.gate = &t3SendGate{
		hostGate:  newHostGate(ctx, &modelGateSource{root: root, quota: fakes.Quota}, clock, hostSettings.Gate),
		networkId: networkId,
		member:    func(sidecarId sidecar.Id) bool { return sidecarId == self.sidecarId },
	}
	self.gate.load(networkId, clock.Now())
	self.gate.loadBreaker()
	if facts, ok := self.gate.Facts(networkId); !ok || facts.Label != label {
		t.Fatalf("the gate loaded %+v, %t", facts, ok)
	}
	self.store = model.NewSmtpEdgeStore(nil)
	self.host, err = newSmtpEdgeHost(ctx, hostSettings, &hostDeps{
		Quota:    fakes.Quota,
		Abuse:    fakes.Abuse,
		Store:    self.store,
		Gate:     self.gate,
		Resolver: self.world,
		Ptr:      self.world,
		Dialer:   self.world,
		Verifier: keySet.NewVerifier(nil),
		Gates:    fakes.Gates,
		Redis:    sidecarhost.NewRedis(nil),
		Metrics:  fakes.Metrics,
		Clock:    clock,
		Log:      &testLog{},
	})
	if err != nil {
		t.Fatal(err)
	}
	self.listener = smtpedgeservice.NewSmtpEdgeTestListener(self.host.engine)
	guard, done := context.WithTimeout(ctx, t3SendGuardTimeout)
	defer done()
	if err := self.listener.AwaitStarted(guard); err != nil {
		t.Fatalf("the engine did not start: %v", err)
	}
	return self
}

// Ends the block, which writes its send log, and every stream it served. Safe more than once.
func (self *t3Fs31Block) close() {
	self.closeOnce.Do(func() {
		if self.host != nil {
			self.host.Close()
		}
		self.cancel()
		if self.gate != nil {
			self.gate.wait()
		}
		self.serving.Wait()
		if self.world != nil {
			self.world.Wait()
		}
	})
}

// One stream to the engine, as the listener hands one over on a ticket naming the network, the device's client and its
// sidecar; the sidecar's end is returned.
func (self *t3Fs31Block) stream() net.Conn {
	ticketId := make([]byte, 16)
	if _, err := rand.Read(ticketId); err != nil {
		self.t.Fatal(err)
	}
	now := self.clock.Now()
	ticket := &protocol.SocketTicket{
		TicketId:              ticketId,
		NetworkId:             sidecar.NewId().Bytes(),
		Name:                  smtpedgeservice.SmtpEdgeSocketName,
		ListenerClientId:      self.edgeClientId.Bytes(),
		DialerClientId:        self.clientId.Bytes(),
		DialerNetworkId:       self.networkId.Bytes(),
		DialerSidecarId:       self.sidecarId.Bytes(),
		IssueTimeMillis:       uint64(now.UnixMilli()),
		ExpireTimeMillis:      uint64(now.Add(30 * time.Second).UnixMilli()),
		GrantExpireTimeMillis: uint64(now.Add(time.Hour).UnixMilli()),
	}
	sidecarEnd, edgeEnd := sidecartest.NewMxPipe(&net.TCPAddr{}, &net.TCPAddr{})
	self.serving.Add(1)
	go func() {
		defer self.serving.Done()
		self.listener.Deliver(edgeEnd, ticket)
	}()
	return sidecarEnd
}

// See sidecar.SmtpProofTestCaller: a call to the ticket's listener lands on the engine's handler from the device's
// client.
func (self *t3Fs31Block) Call(ctx context.Context, clientId sidecar.Id, messageType protocol.MessageType, body []byte, options *sidecar.CallOptions) ([]byte, error) {
	if clientId != self.edgeClientId {
		return nil, errT3SendCallClient
	}
	return self.listener.Call(ctx, self.clientId, messageType, body)
}

// The channel as a sender submits its parts, through rewrite where the row has one, the parts the edge took and the
// first it refused kept.
type t3Fs31Channel struct {
	inner   sidecar.SmtpProofChannel
	rewrite func(part *sidecar.SmtpProofPart, parts []*sidecar.SmtpProofPart)

	stateLock    sync.Mutex
	parts        []*sidecar.SmtpProofPart
	refusedIndex uint32
}

func (self *t3Fs31Channel) DiscloseShts(ctx context.Context, shts []byte) error {
	return self.inner.DiscloseShts(ctx, shts)
}

func (self *t3Fs31Channel) SubmitPart(ctx context.Context, part *sidecar.SmtpProofPart) error {
	clone := *part
	self.stateLock.Lock()
	parts := slices.Clone(self.parts)
	self.stateLock.Unlock()
	if self.rewrite != nil {
		self.rewrite(&clone, parts)
	}
	err := self.inner.SubmitPart(ctx, &clone)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	switch {
	case err == nil && self.refusedIndex == 0:
		self.parts = append(self.parts, &clone)
	case err != nil && self.refusedIndex == 0:
		self.refusedIndex = clone.Index
	}
	return err
}

// Runs one row in one cell on a block of its own, and holds the MX's record and the block's send log to the row.
func t3Fs31RunCell(t testing.TB, row *t3Fs31Row, cell *t3Fs31Cell) {
	t.Helper()
	name := row.name + ", " + cell.String()
	script := sidecartest.DefaultMxScript()
	if row.script != nil {
		row.script(script)
	}
	mx, err := sidecartest.NewScriptedMx(t3Fs31MxHost, script, &sidecartest.ScriptedMxSettings{
		MinVersion:   cell.version,
		MaxVersion:   cell.version,
		CipherSuites: []uint16{cell.suite},
		RsaKey:       cell.rsa,
	})
	if err != nil {
		t.Fatal(err)
	}
	block := newT3Fs31Block(t, mx, row.recipientLimit)
	defer block.close()

	recipients := row.recipients
	if recipients == nil {
		recipients = []string{"bob", "carol"}
	}
	body := row.body
	if body == "" {
		body = "one line\r\nanother line\r\nand a third\r\n"
	}
	extra := ""
	if row.extraHeaders != nil {
		extra = row.extraHeaders(cell, block.networkDomain)
	}
	message := func(xUrRef string, feedbackId string) []byte {
		return []byte(t3Fs31HeaderBlock(xUrRef, feedbackId, block.networkDomain, extra) + body)
	}
	// the issued values' widths are fixed, so the count is known before they are (plan-SMTP-preamble-byte-count)
	sized := message("1."+strings.Repeat("a", 34), "n:p:"+strings.Repeat("a", 26)+":urnetwork")
	byteCount := uint64(len(sized))
	if cell.data {
		byteCount = uint64(len(t3Fs31DotStuffed(sized)))
	}
	preamble := &protocol.SmtpPreamble{
		Version:          sidecar.SmtpPreambleVersion,
		RecipientDomain:  t3Fs31RecipientDomain,
		RecipientCount:   uint32(len(recipients)),
		MessageId:        "abcdefghijklmnopqrstuvwxyz@" + block.networkDomain,
		HeaderFromDomain: block.networkDomain,
		DkimSelector:     t3Fs31Selector,
		MessageByteCount: byteCount,
		ProofKeySet:      block.keySet.Id(),
	}
	conn := block.stream()
	if err := sidecar.WriteSmtpPreamble(conn, preamble, nil); err != nil {
		t.Fatal(err)
	}
	answer, err := sidecar.ReadSmtpPreambleResult(conn, nil)
	if err != nil || answer.GetCode() != uint32(protocol.SmtpPreambleCode_SMTP_PREAMBLE_OK) {
		t.Fatalf("%s: the preamble was answered %d: %v", name, answer.GetCode(), err)
	}
	job := &sidecar.SmtpProofJob{
		SendId: answer.GetSendId(),
		Preamble: &sidecar.SmtpProofPreamble{
			RecipientDomain:  preamble.RecipientDomain,
			RecipientCount:   preamble.RecipientCount,
			MessageId:        preamble.MessageId,
			HeaderFromDomain: preamble.HeaderFromDomain,
			DkimSelector:     preamble.DkimSelector,
			MessageByteCount: preamble.MessageByteCount,
			HeloName:         answer.GetHeloName(),
			XUrRef:           answer.GetXUrRef(),
			FeedbackId:       answer.GetFeedbackId(),
			EnvelopeDomain:   "b." + block.networkDomain,
			MxHost:           answer.GetMxHost(),
			ProofKeySet:      preamble.ProofKeySet,
		},
		SenderLocal:           "bounce-1",
		RecipientLocals:       recipients,
		Message:               message(answer.GetXUrRef(), answer.GetFeedbackId()),
		VerifyPeer:            func(chain []*x509.Certificate) error { return nil },
		KeySet:                block.keySet.Id(),
		EnvelopeGroupMaxCount: row.groupMaxCount,
	}
	if uint64(len(job.Message)) != uint64(len(sized)) {
		t.Fatalf("%s: the issued values changed the message's length", name)
	}
	settings := smtpproof.DefaultProverSettings()
	settings.SuiteOrder = []uint16{cell.suite}
	settings.ForceData = cell.data && !row.unforced
	channel := &t3Fs31Channel{inner: sidecar.NewSmtpProofTestChannel(block, block.edgeClientId, answer.GetSendId(), sidecar.DefaultSmtpSenderSettings()), rewrite: row.part}
	ctx, cancel := context.WithTimeout(context.Background(), t3SendGuardTimeout)
	defer cancel()
	result, sendErr := block.keySet.NewProver(settings).SendSmtp(ctx, conn, job, channel)
	accepted := sendErr == nil && result != nil && result.Accepted
	if accepted {
		// an accepted send's connection ends at the MX's close after QUIT
		done := make(chan struct{})
		go func() {
			block.serving.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(t3SendGuardTimeout):
			t.Errorf("%s: the connection did not end after QUIT", name)
		}
	}
	conn.Close()
	block.close()

	transactions := mx.Transactions()
	honest := []string{"EHLO " + job.Preamble.HeloName, "MAIL FROM:<bounce-1@" + job.Preamble.EnvelopeDomain + ">"}
	for _, local := range recipients {
		honest = append(honest, "RCPT TO:<"+local+"@"+t3Fs31RecipientDomain+">")
	}
	if cell.data {
		honest = append(honest, "DATA")
	} else {
		honest = append(honest, "BDAT "+strconv.FormatUint(byteCount, 10)+" LAST")
	}
	next := 0
	for _, event := range mx.Events() {
		if !event.Secure || !event.Accepted || event.Command == "QUIT" || strings.HasPrefix(event.Command, "<data ") {
			continue
		}
		at := slices.Index(honest[next:], event.Command)
		if at < 0 {
			t.Errorf("%s: the MX accepted %q, outside the honest dialog", name, event.Command)
			continue
		}
		next += at + 1
	}
	switch {
	case row.expect.delivered:
		if !accepted || len(transactions) != 1 || !bytes.Equal(transactions[0].Message, job.Message) || len(transactions[0].Recipients) != len(recipients) {
			t.Errorf("%s: not delivered: %+v, %v, %d transactions", name, result, sendErr, len(transactions))
		}
	case row.expect.refusedCode != "":
		if sendErr != nil || result == nil || result.RefusedCode != row.expect.refusedCode || len(transactions) != 0 {
			t.Errorf("%s: %+v, %v, %d transactions, want the MX's %s", name, result, sendErr, len(transactions), row.expect.refusedCode)
		}
	default:
		refusals := block.keySet.Refusals()
		if accepted || len(transactions) != 0 || channel.refusedIndex != row.expect.refusedPart || !errors.Is(sendErr, sidecar.ErrSmtpProofRefused) {
			t.Errorf("%s: the edge refused part %d (%v), %d transactions, want part %d", name, channel.refusedIndex, sendErr, len(transactions), row.expect.refusedPart)
		}
		if row.expect.refusedCheck != "" && (len(refusals) != 1 || refusals[0].Index != row.expect.refusedPart || refusals[0].Check != row.expect.refusedCheck) {
			t.Errorf("%s: the verifier refused %+v, want part %d by %s", name, refusals, row.expect.refusedPart, row.expect.refusedCheck)
		}
	}
	if row.check != nil {
		row.check(t, name, mx, result)
	}

	// the send log: one record, the network's, naming no more recipients than the preamble declared
	sends, err := block.store.FindSends(context.Background(), &smtpedgeservice.SmtpJoinKey{
		NetworkId: block.networkId,
		Since:     block.start.Add(-time.Hour),
		Until:     block.clock.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sends) != 1 || sends[0].RecipientDomain != t3Fs31RecipientDomain || uint64(len(recipients)) < uint64(sends[0].RecipientCount) {
		t.Errorf("%s: smtp_send holds %d records: %+v", name, len(sends), sends)
	}
}

// Runs one row in every cell it applies to, in a TestEnv of its own.
func t3Fs31Run(t *testing.T, name string) {
	var row *t3Fs31Row
	for _, candidate := range t3Fs31Rows() {
		if candidate.name == name {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("no row %q", name)
	}
	env := server.DefaultTestEnv()
	env.RerunCount = 0
	env.Run(t, func(t testing.TB) {
		ran := 0
		for _, cell := range t3Fs31Cells() {
			if row.applies(cell) {
				t3Fs31RunCell(t, row, cell)
				ran += 1
			}
		}
		if ran == 0 {
			t.Fatalf("%s ran in no cell", row.name)
		}
	})
}

func TestT3Fs31AnHonestSend(t *testing.T) {
	t3Fs31Run(t, "an honest send")
}

func TestT3Fs31AnHonestSendTo100Recipients(t *testing.T) {
	t3Fs31Run(t, "an honest send to 100 recipients")
}

func TestT3Fs31AnHonestSendWithA16KibHeaderBlock(t *testing.T) {
	t3Fs31Run(t, "an honest send with a 16 KiB header block")
}

func TestT3Fs31NoChunking(t *testing.T) {
	t3Fs31Run(t, "an MX without CHUNKING")
}

func TestT3Fs31ChunkingOnlyBeforeTls(t *testing.T) {
	t3Fs31Run(t, "an MX offering CHUNKING only before TLS")
}

func TestT3Fs31NoPipelining(t *testing.T) {
	t3Fs31Run(t, "an MX without PIPELINING, refusing a command sent ahead of a reply")
}

func TestT3Fs31MailRefused(t *testing.T) {
	t3Fs31Run(t, "an MX refusing MAIL")
}

func TestT3Fs31RcptRefused(t *testing.T) {
	t3Fs31Run(t, "an MX refusing every RCPT")
}

func TestT3Fs31MixedRecipients(t *testing.T) {
	t3Fs31Run(t, "an MX refusing one RCPT of two")
}

func TestT3Fs31No354(t *testing.T) {
	t3Fs31Run(t, "an MX answering DATA with no 354")
}

func TestT3Fs31A451(t *testing.T) {
	t3Fs31Run(t, "an MX answering the message 451")
}

func TestT3Fs31ASecondTransactionInTheBdatBody(t *testing.T) {
	t3Fs31Run(t, "a second transaction in the BDAT body, the MX offering CHUNKING")
}

func TestT3Fs31ReplyCountOneFewer(t *testing.T) {
	t3Fs31Run(t, "reply_count one fewer than the records hold, the envelope one command a group")
}

func TestT3Fs31ReplyCountOneMore(t *testing.T) {
	t3Fs31Run(t, "reply_count one more than the records hold")
}

func TestT3Fs31AMessageRecordBeforeTheEnvelopesReplies(t *testing.T) {
	t3Fs31Run(t, "a message record before the envelope's replies are proven")
}

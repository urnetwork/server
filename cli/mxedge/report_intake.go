package main

// The report intake mxedge's platform block hands its listener (SMTP-M5-06; §14.8, §15.4's platform mail,
// D-SMTP-12, D-SMTP-19, D-SMTP-23, plan-SMTP-report-intake-home): sidecar's smtpedgeservice.SmtpReportIntake
// in this process, on model's Postgres store, with the listener's mxedgeservice.PlatformIntake in front of it,
// since neither engine may import the other (§4.4). The listener calls it once for each recipient it took,
// before it answers the DATA or BDAT, and the intake keeps the envelope and the message in memory (mx-4).
//
// It reads config/<env>/smtpedge.yml for the names, the sending addresses, the loop allowlist and the report
// bounds, as the smtpedge block will (smtpedgeservice.LoadSmtpEdgeSettings). Each seam the block cannot give the
// intake stays empty, says so at start, and fails closed as smtpedgeservice.SmtpReportIntakeDeps says.
//
// The Tokens tie a report at <token>@<report host> to the network whose zone holds the token, by network_dns_zone's
// unique index on report_token over the block's own Postgres (report_token_source.go; plan-SMTP-net-token, §14.6),
// which main gives the intake through resolveTokensThrough where the binary links MG6, the column's group; in one that
// does not, every <token>@ report is dropped unread, as no zone there draws a token for its rua.
//
// The Keys, each network's mail key, come from api's report-key route on the platform listener's gate bearer
// (newPlatformReportKeySource, plan-SMTP-report-seal-key), which main gives the intake through sealThrough: api
// answers the key only once it verified it against the network's rows, so a report is sealed to that key, or to
// none, its raw bytes then dropped unread and its fields kept, and a key api cannot tell now is the intake's error,
// 451. Without api's url or the bearer the block seals nothing, says so at start, and keeps every report's fields.
//
// The Join and the halt ladder, SMTP-M9-15's attribution and SMTP-M9-16's ladder, are the block's to give
// (attributeThrough, report_join.go), where its binary links MG9's send log, halts and keys and its vault holds the
// X-UR-Ref keys; an intake given none attributes no ARF report or role mail, and the listener answers role mail
// plan-ABUSE-auto-reply's 550. The Sink is §21's real one (model.NewAbuseSmtpSink, ABUSE-M5-09), which an
// attributed role mail's case and a fraud complaint's review reach.
//
// <root>'s rows resolve through §12's label map (model.ResolveNetworkLabel), and reporters' DKIM keys come from
// DNS through the process's resolver. The intake reads MG2's dns_label and writes MG5's report tables, so the
// block is ready only in a binary that links both (reportIntakeGroups, HostDeps.IntakeGroups), and every store
// call answers model.ErrSmtpReportUnlinked in one that does not.
//
// Role mail is also checked for a notice (owner decisions 530, 612; QD-1070). For mail to abuse@ or postmaster@
// on <root>, each DKIM signature of the mail's own header block is verified, its body hash over the raw bytes and
// its key from DNS within smtpedge.yml's report bounds, and api's notice route (server.AbuseNoticeGatePath) is sent
// the recipient's local part, <root> and each signature's d= and result, on the platform listener's own gate
// bearer, and never a byte of the message. api files the notice when a signature passed under a domain abuse.yml's
// notice_senders lists, and pages the rota. A mail filed as a notice is taken, 250, as attributed role mail is,
// since a case was made of it; one api could not judge or file now is the intake's error, 451, so the sender sends
// it again and the notice is not lost to a moment's outage. Mail with no signature that passed or met a key that
// did not answer cannot page under any listing and costs api no call. One message to both role addresses files one
// notice: the answer is kept by the message's hash and arrival for the listener's next recipient. Without api's url
// or the bearer the block files no notice, says so at start, and answers role mail as it did.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/glog"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/mxedgeservice"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecarhost"
)

// The intake's own file in config/<env>/, which the smtpedge block reads too.
const reportIntakeSettingsName = "smtpedge.yml"

// The pending groups the intake reads: MG2's dns_label through the label map, and MG5's report tables.
func reportIntakeGroups() []*sidecarhost.RunGroup {
	return []*sidecarhost.RunGroup{
		{Name: "MG2", Linked: server.SidecarMg2Linked},
		{Name: "MG5", Linked: server.SidecarMg5Linked},
	}
}

// smtpedge.yml from config.
func loadReportIntakeSettings() (*smtpedgeservice.SmtpEdgeSettings, error) {
	resource, err := server.Config.SimpleResource(reportIntakeSettingsName)
	if err != nil {
		return nil, err
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, err
	}
	return smtpedgeservice.LoadSmtpEdgeSettings(data)
}

// The intake on store with settings, reporters' keys from txt and report tokens from tokens, either of which
// may be nil to fail closed, logging through glog. Its metrics go to the process's registry, and it reads real
// time.
func newPlatformReportIntake(store smtpedgeservice.SmtpEdgeStore, settings *smtpedgeservice.SmtpEdgeSettings, txt smtpedgeservice.SmtpDnsTxtSource, tokens smtpedgeservice.SmtpNetTokenSource) (*platformReportIntake, error) {
	if settings == nil {
		return nil, errors.New("report intake: no smtpedge.yml")
	}
	host := &sidecar.ServiceHost{
		Metrics: sidecarhost.NewMetrics(),
		Clock:   sidecar.RealClock{},
	}
	deps := &smtpedgeservice.SmtpReportIntakeDeps{
		Tokens: tokens,
		Labels: &reportLabelSource{root: settings.Root},
		Txt:    txt,
		Sink:   model.NewAbuseSmtpSink(nil),
		Log:    reportIntakeLogger{},
	}
	intake, err := smtpedgeservice.NewSmtpReportIntake(host, store, deps, settings)
	if err != nil {
		return nil, err
	}
	return &platformReportIntake{
		intake:   intake,
		host:     host,
		store:    store,
		deps:     deps,
		root:     reportIntakeName(settings.Root),
		settings: settings,
		txt:      txt,
		clock:    host.Clock,
	}, nil
}

// A name as both files hold it: lowercase, with no final dot.
func reportIntakeName(name string) string {
	return strings.ToLower(strings.TrimSuffix(name, "."))
}

// The platform block's intake from config: smtpedge.yml's settings on model's store and reporters' keys from the
// process's resolver, its report tokens given after it (resolveBlockReportTokens). smtpedge.yml's root and report
// host must be mxedge.yml's platform_root and platform_report_host, since the listener takes recipients by the one
// and the intake routes them by the other: mail one took and the other could not route would be answered 250 and
// dropped. An error leaves the block without an intake, so every recipient the listener answers for gets 451 4.3.0.
func newConfiguredReportIntake(mxEdgeSettings *mxedgeservice.MxEdgeSettings) (*platformReportIntake, error) {
	settings, err := loadReportIntakeSettings()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", reportIntakeSettingsName, err)
	}
	if mxEdgeSettings == nil {
		return nil, errors.New("report intake: no mxedge.yml")
	}
	if reportIntakeName(settings.Root) != reportIntakeName(mxEdgeSettings.PlatformRootDomainName) {
		return nil, fmt.Errorf("%s's root is not mxedge.yml's platform_root", reportIntakeSettingsName)
	}
	if reportIntakeName(settings.ReportHostName) != reportIntakeName(mxEdgeSettings.PlatformReportHostName) {
		return nil, fmt.Errorf("%s's report_host is not mxedge.yml's platform_report_host", reportIntakeSettingsName)
	}
	return newPlatformReportIntake(model.NewSmtpEdgeStore(nil), settings, &smtpedgeservice.SmtpResolverTxtSource{}, nil)
}

// The intake as the platform listener reaches it. Safe for concurrent use, as the intake is, once
// fileNoticesThrough, sealThrough, resolveTokensThrough and attributeThrough have run, which they do before the
// listener has it.
type platformReportIntake struct {
	intake *smtpedgeservice.SmtpReportIntake
	// what the intake was made with, which attributeThrough makes it again with
	host  *sidecar.ServiceHost
	store smtpedgeservice.SmtpEdgeStore
	deps  *smtpedgeservice.SmtpReportIntakeDeps
	// <root>, lowercased, whose abuse@ and postmaster@ are role mail
	root string
	// smtpedge.yml's bounds and the reporters' key source, which role mail's notice check reads too
	settings *smtpedgeservice.SmtpEdgeSettings
	txt      smtpedgeservice.SmtpDnsTxtSource
	clock    sidecar.Clock
	// role mail's notice check; nil files no notice
	notices *platformNoticeFiler
}

var _ mxedgeservice.PlatformIntake = (*platformReportIntake)(nil)

// Files role mail's notices through gate from now on, under settings, nil taking the defaults.
func (self *platformReportIntake) fileNoticesThrough(gate platformNoticeGate, settings *platformNoticeSettings) {
	if settings == nil {
		settings = defaultPlatformNoticeSettings()
	}
	format := gate.Format()
	if format == nil {
		format = server.DefaultAbuseNoticeGateFormatSettings()
	}
	self.notices = &platformNoticeFiler{
		gate:     gate,
		verifier: smtpedgeservice.NewSmtpDkimVerifier(self.settings, self.txt, nil, self.clock),
		settings: settings,
		format:   format,
		intake:   self.settings,
		root:     self.root,
		recent:   map[platformNoticeMessageKey]bool{},
	}
}

// The local part when recipient is abuse@ or postmaster@ on <root>, as the listener lowercased it.
func (self *platformReportIntake) roleRecipient(recipient string) (string, bool) {
	local, domain, ok := strings.Cut(strings.ToLower(recipient), "@")
	if !ok {
		return "", false
	}
	if (local == "abuse" || local == "postmaster") && strings.TrimSuffix(domain, ".") == self.root {
		return local, true
	}
	return "", false
}

// See mxedgeservice.PlatformIntake. A role mail the intake could not make a case of, and that api did not file as
// a notice, is unread, which the listener answers with plan-ABUSE-auto-reply's 550 when every recipient's ends
// so; everything else the intake finished with is taken, counted, stored or dropped, which tells the sender
// nothing. An error is the intake's, a store or DNS that did not answer, or a notice api could not judge or file
// now, which carries no report text, and the listener answers it 451. The notice is checked after the intake, so
// an intake error leaves no notice filed for the sender's retry to file again.
func (self *platformReportIntake) Accept(ctx context.Context, message *mxedgeservice.PlatformMessage) (mxedgeservice.PlatformOutcome, error) {
	outcome, err := self.intake.Accept(ctx, &smtpedgeservice.SmtpReportMessage{
		Recipient:   message.Recipient,
		ArrivalTime: message.ArrivalTime,
		Raw:         message.Raw,
	})
	if err != nil {
		return 0, err
	}
	localPart, ok := self.roleRecipient(message.Recipient)
	if !ok {
		return mxedgeservice.PlatformOutcomeTaken, nil
	}
	notice, err := self.notices.file(ctx, localPart, message)
	if err != nil {
		return 0, err
	}
	if outcome != smtpedgeservice.SmtpReportOutcomeCase && !notice {
		return mxedgeservice.PlatformOutcomeUnread, nil
	}
	return mxedgeservice.PlatformOutcomeTaken, nil
}

// api's notice route as role mail's check reaches it: server.AbuseNoticeGateClient, and a fake in tests. Safe for
// concurrent use.
type platformNoticeGate interface {
	// See server.AbuseNoticeGateClient.FileNotice.
	FileNotice(ctx context.Context, args *server.AbuseNoticeGateArgs) (*server.AbuseNoticeGateResult, error)
	// The route's format the gate sends in.
	Format() *server.AbuseNoticeGateFormatSettings
}

// Tunables of role mail's notice check.
type platformNoticeSettings struct {
	// How many messages' answers are kept for the listener's next recipient of the same message, so one message
	// to abuse@ and postmaster@ files one notice: 1,024, past the role mail the listener's buffers hold at once.
	// The oldest goes first. Not positive keeps none, and each recipient's check stands alone.
	RecentMessageCount int
}

func defaultPlatformNoticeSettings() *platformNoticeSettings {
	return &platformNoticeSettings{
		RecentMessageCount: 1024,
	}
}

// One message, by its bytes' hash and its arrival, which the listener gives each of its recipients alike.
type platformNoticeMessageKey struct {
	rawSha256   [sha256.Size]byte
	arrivalTime time.Time
}

// Role mail's notice check, the file header's. Safe for concurrent use.
type platformNoticeFiler struct {
	gate     platformNoticeGate
	verifier *smtpedgeservice.SmtpDkimVerifier
	settings *platformNoticeSettings
	format   *server.AbuseNoticeGateFormatSettings
	// smtpedge.yml's bounds, its header block's among them
	intake *smtpedgeservice.SmtpEdgeSettings
	// <root>, lowercased
	root string

	stateLock sync.Mutex
	// recent messages' answers, whether each was filed as a notice, and their keys oldest first
	recent     map[platformNoticeMessageKey]bool
	recentKeys []platformNoticeMessageKey
}

// Whether api filed the message as a notice for localPart's role address, checking it at the message's first role
// recipient and answering that for the rest. An error is a check that could not finish or api not filing now.
func (self *platformNoticeFiler) file(ctx context.Context, localPart string, message *mxedgeservice.PlatformMessage) (bool, error) {
	if self == nil {
		return false, nil
	}
	key := platformNoticeMessageKey{rawSha256: sha256.Sum256(message.Raw), arrivalTime: message.ArrivalTime}
	if filed, ok := self.recalled(key); ok {
		return filed, nil
	}
	signatures, err := self.verdicts(ctx, message.Raw)
	if err != nil {
		return false, err
	}
	mayPage := false
	for _, signature := range signatures {
		if signature.Result == server.AbuseNoticeDkimResultPass || signature.Result == server.AbuseNoticeDkimResultTempError {
			mayPage = true
		}
	}
	if !mayPage {
		self.remember(key, false)
		return false, nil
	}
	result, err := self.gate.FileNotice(ctx, &server.AbuseNoticeGateArgs{
		RecipientLocalPart: localPart,
		Root:               self.root,
		Signatures:         signatures,
	})
	if err != nil {
		// the error names api's answer or the part refused, never a domain or the bearer
		return false, fmt.Errorf("report intake: a role mail's notice was not judged: %w", err)
	}
	self.remember(key, result.Notice)
	return result.Notice, nil
}

// The answer kept for key, if any.
func (self *platformNoticeFiler) recalled(key platformNoticeMessageKey) (bool, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	filed, ok := self.recent[key]
	return filed, ok
}

// Keeps key's answer, dropping the oldest past RecentMessageCount.
func (self *platformNoticeFiler) remember(key platformNoticeMessageKey, filed bool) {
	if self.settings.RecentMessageCount <= 0 {
		return
	}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if _, ok := self.recent[key]; !ok {
		self.recentKeys = append(self.recentKeys, key)
	}
	self.recent[key] = filed
	for self.settings.RecentMessageCount < len(self.recentKeys) {
		delete(self.recent, self.recentKeys[0])
		self.recentKeys = self.recentKeys[1:]
	}
}

// Each DKIM signature of the message's own header block, d= and result, in header order and at most the format's
// count, a d= the format cannot carry sent as none, since no listing can name it. None for a message whose header
// block does not end within smtpedge.yml's bound or does not parse. The error is only ctx's.
func (self *platformNoticeFiler) verdicts(ctx context.Context, raw []byte) ([]*server.AbuseNoticeGateSignature, error) {
	headerBlock, ok := platformNoticeHeaderBlock(raw, self.intake.ReportHeaderByteCount)
	if !ok {
		return nil, nil
	}
	bodyHash, err := self.verifier.NewBodyHash(headerBlock)
	if err != nil {
		return nil, nil
	}
	// the body hash skips the header block itself and never keeps a byte
	bodyHash.Write(raw)
	results, err := self.verifier.Verify(ctx, bodyHash)
	if err != nil {
		return nil, err
	}
	signatures := []*server.AbuseNoticeGateSignature{}
	for _, result := range results {
		if self.format.SignatureMaxCount <= len(signatures) {
			break
		}
		domain := result.Domain
		if !self.format.DomainValid(domain) {
			domain = ""
		}
		signatures = append(signatures, &server.AbuseNoticeGateSignature{Domain: domain, Result: result.Result})
	}
	return signatures, nil
}

// The message's own header block through the CRLF CRLF that ends it, as smtpedgeservice's walk cuts it, or false
// when it does not end within maxByteCount.
func platformNoticeHeaderBlock(raw []byte, maxByteCount int) ([]byte, bool) {
	if bytes.HasPrefix(raw, []byte("\r\n")) {
		return raw[:2], true
	}
	end := bytes.Index(raw[:min(len(raw), maxByteCount)], []byte("\r\n\r\n"))
	if end < 0 {
		return nil, false
	}
	return raw[:end+4], true
}

// A notice gate on api at apiUrl with the platform listener's bearer from the block's secret files, or the part
// that is missing, never the bearer.
func newPlatformNoticeGate(apiUrl string) (platformNoticeGate, error) {
	if apiUrl == "" {
		return nil, errors.New("no api url")
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerMxedgePlatform)
	if err != nil {
		return nil, err
	}
	client, err := server.NewAbuseNoticeGateClient(&server.AbuseNoticeGateClientArgs{ApiUrl: apiUrl, Bearer: bearer})
	if err != nil {
		return nil, err
	}
	return client, nil
}

// Seals each report to its network's mail key from now on, the key read from keys (newPlatformReportKeySource): the
// intake is made again with it, its other seams as they were. An error leaves the intake as it was. Called before the
// listener has the intake, as attributeThrough is.
func (self *platformReportIntake) sealThrough(keys smtpedgeservice.SmtpReportKeySource) error {
	if keys == nil {
		return errors.New("report intake: no key source")
	}
	deps := *self.deps
	deps.Keys = keys
	intake, err := smtpedgeservice.NewSmtpReportIntake(self.host, self.store, &deps, self.settings)
	if err != nil {
		return err
	}
	self.intake = intake
	self.deps = &deps
	return nil
}

// The network mail keys on api at apiUrl, its report-key route on the platform listener's bearer from the block's
// secret files, or the part that is missing, never the bearer.
func newPlatformReportKeySource(apiUrl string) (smtpedgeservice.SmtpReportKeySource, error) {
	if apiUrl == "" {
		return nil, errors.New("no api url")
	}
	bearer, err := server.LoadInternalGateCredential(server.InternalGateCallerMxedgePlatform)
	if err != nil {
		return nil, err
	}
	source, err := smtpedgeservice.NewSmtpReportKeyGateSource(&smtpedgeservice.SmtpReportKeyGateSourceArgs{ApiUrl: apiUrl, Bearer: bearer})
	if err != nil {
		return nil, err
	}
	return source, nil
}

// §12's label map as the intake reads <root>'s rows: the network a <net> label or alias names in root, a
// retiring alias's included, since a network's mail still names it (plan-SMTP-net-token). A name nothing holds
// is no network; a read that failed is an error, so the reporter retries.
type reportLabelSource struct {
	root string
}

// See smtpedgeservice.SmtpLabelSource.
func (self *reportLabelSource) NetworkForLabel(ctx context.Context, label string) (networkId sidecar.Id, ok bool, returnErr error) {
	defer func() {
		// the model raises a failed read, which reaches the intake as an error
		if r := recover(); r != nil {
			if server.IsDoneError(r) {
				panic(r)
			}
			returnErr = fmt.Errorf("report intake: the label map: %v", r)
		}
	}()
	resolvedId, _, err := model.ResolveNetworkLabel(ctx, self.root, label, false)
	if errors.Is(err, sidecar.ErrNetworkLabelUnknown) {
		return sidecar.Id{}, false, nil
	}
	if err != nil {
		return sidecar.Id{}, false, err
	}
	return sidecar.Id(resolvedId), true, nil
}

// glog as the intake's sidecar.Logger. The intake logs a reason as a word and never report text, an address
// or a recipient (I-13), and nothing here adds one.
type reportIntakeLogger struct{}

func (self reportIntakeLogger) Info(args ...any) {
	glog.Info(args...)
}

func (self reportIntakeLogger) Infof(format string, args ...any) {
	glog.Infof(format, args...)
}

func (self reportIntakeLogger) Warningf(format string, args ...any) {
	glog.Warningf(format, args...)
}

func (self reportIntakeLogger) Errorf(format string, args ...any) {
	glog.Errorf(format, args...)
}

func (self reportIntakeLogger) V(level int32) sidecar.Verbose {
	return reportIntakeVerbose(glog.V(glog.Level(level)))
}

// glog's verbose level as the intake's sidecar.Verbose.
type reportIntakeVerbose glog.Verbose

func (self reportIntakeVerbose) Enabled() bool {
	return bool(self)
}

func (self reportIntakeVerbose) Info(args ...any) {
	glog.Verbose(self).Info(args...)
}

func (self reportIntakeVerbose) Infof(format string, args ...any) {
	glog.Verbose(self).Infof(format, args...)
}

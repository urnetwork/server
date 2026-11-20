package controller

// §21.9's preservation holds as api and the abuse worker run them (ABUSE-M5-14; R-ABUSE-5, D-ABUSE-6, D-ABUSE-8,
// D-TRUST-7, FS-48). model/abuse_hold_model.go holds the rows, the hold/ writer and the deadlines; this file holds
// what reaches past Postgres and the object:
//
//	PlaceLegalHold   model.PlaceLegalHold (object, then row), owing the legal-request mailbox its record in the
//	                 row's transaction, then the post-placement hook, which starts the hold's copy (ABUSE-M5-15)
//	RenewLegalHold   model.RenewLegalHold, owing the renewed record the same way
//	staff kinds      place_hold and renew_hold (legal), and report_csam (reviewer, legal): a CSAM report recorded
//	                 through POST /abuse/action bars its case, suspends its network through §21's one path
//	                 (ABUSE-M5-06's effects) and places report_2258a; the filing with NCMEC is a person's
//	RunAbuseHoldPass each hour: past its expiry a hold's sweep pass is queued (evsservice's hold end, QD-635),
//	                 then its object deleted, then its row and copies, owing the ended record; a live row's missing
//	                 object is put back; a hold/ object with no row stays until its own expiry and alarms, since
//	                 after a restore that is a hold placed after the dump (§19.10)
//
// The legal-request mailbox (plan-ABUSE-small-readings #12, QD-088) gets one record per hold placed, renewed or
// ended, MACed and sealed as the abuse mailbox's records are (QD-078, QD-726; abuse_mailbox_seal.go), to abuse.yml's
// legal_request_mailbox:
//
//	{"event":"placed"|"renewed"|"ended","case_id":"<id>","network_id":"<id>","kind":"preserve_2703f"|"report_2258a",
//	 "scope":"network"|["<repo_id>",...],"place_time":"<time>","expire_time":"<time>","mac":"<hex>"}
//
// mac is HMAC-SHA-256 in lowercase hex over the other keys as canonical JSON in this order, no whitespace, times in
// RFC 3339 UTC to the millisecond, under abuse-mailbox.yml's abuse_mailbox_mac_key. The record is owed as a
// server/task run (MailLegalHoldRecordTask) scheduled in the transaction that commits its change, so a crash
// between commit and mail leaves it owed and its task mails it; a mailbox that is down retries each minute. The
// task's args carry the record's fields and no more: the network, case, kind, scope and times api-14 lists for a
// hold.
//
// Silence (owner decisions 93, 184): placing, renewing and ending a hold write no hint, no admin event, no notice,
// no status field and no log line naming a network or case, sealed or not; only counsel notifies. A barred case's
// suspension writes no notice, admin event or hint either (abuse_action_controller.go). A site loss voids what a
// hold kept on the file cluster: the pass puts objects back from rows, and §19.10's step tells counsel.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
	"github.com/urnetwork/server/task"
)

// A legal-request record's event (plan-ABUSE-small-readings #12).
type LegalHoldEvent string

const (
	LegalHoldEventPlaced  LegalHoldEvent = "placed"
	LegalHoldEventRenewed LegalHoldEvent = "renewed"
	LegalHoldEventEnded   LegalHoldEvent = "ended"
)

func (self LegalHoldEvent) known() bool {
	switch self {
	case LegalHoldEventPlaced, LegalHoldEventRenewed, LegalHoldEventEnded:
		return true
	}
	return false
}

// Every key of a legal-request record, its seven and the MAC.
var legalHoldRecordKeys = []string{"event", "case_id", "network_id", "kind", "scope", "place_time", "expire_time", "mac"}

// A legal-request record that does not parse, holds a key it should not, or lacks one.
var ErrLegalHoldRecordInvalid = errors.New("legal hold record invalid")

// The mailer cannot send any record now: no mailer installed, no legal_request_mailbox, no key file or a refused
// one, or no SES sender. The record stays owed, and its task retries.
var ErrLegalHoldRecordMailerUnavailable = errors.New("legal hold record mailer unavailable")

// One record of the legal-request mailbox.
type LegalHoldRecord struct {
	Event      LegalHoldEvent
	CaseId     server.Id
	NetworkId  server.Id
	Kind       sidecar.HoldKind
	Scope      sidecar.HoldScope
	PlaceTime  time.Time
	ExpireTime time.Time
}

// The record of a hold's event.
func LegalHoldRecordOf(event LegalHoldEvent, hold *model.LegalHold) *LegalHoldRecord {
	return &LegalHoldRecord{
		Event:      event,
		CaseId:     hold.CaseId,
		NetworkId:  hold.NetworkId,
		Kind:       hold.Kind,
		Scope:      sidecar.HoldScopeRepos(hold.Scope.RepoIds...),
		PlaceTime:  hold.PlaceTime,
		ExpireTime: hold.ExpireTime,
	}
}

// The record's fields as JSON, in the canonical order. Pointers tell an absent key from a null one when parsing.
type legalHoldRecordJson struct {
	Event      *string         `json:"event"`
	CaseId     *server.Id      `json:"case_id"`
	NetworkId  *server.Id      `json:"network_id"`
	Kind       *string         `json:"kind"`
	Scope      json.RawMessage `json:"scope"`
	PlaceTime  *string         `json:"place_time"`
	ExpireTime *string         `json:"expire_time"`
}

// The scope's JSON: "network", or the repo ids in order.
func legalHoldRecordScopeJson(scope sidecar.HoldScope) json.RawMessage {
	if scope.Network() {
		return json.RawMessage(`"network"`)
	}
	repoTexts := []string{}
	for _, repoId := range scope.RepoIds {
		repoTexts = append(repoTexts, repoId.String())
	}
	encoded, err := json.Marshal(repoTexts)
	if err != nil {
		panic(err)
	}
	return encoded
}

// The canonical JSON of the record's fields, without its MAC.
func (self *LegalHoldRecord) canonical() ([]byte, error) {
	event := string(self.Event)
	kind := string(self.Kind)
	placeTime := self.PlaceTime.UTC().Format(abuseRecordTimeLayout)
	expireTime := self.ExpireTime.UTC().Format(abuseRecordTimeLayout)
	y := &legalHoldRecordJson{
		Event:      &event,
		CaseId:     &self.CaseId,
		NetworkId:  &self.NetworkId,
		Kind:       &kind,
		Scope:      legalHoldRecordScopeJson(self.Scope),
		PlaceTime:  &placeTime,
		ExpireTime: &expireTime,
	}
	b := &bytes.Buffer{}
	encoder := json.NewEncoder(b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(y); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// The record's bytes as mailed before the seal: its canonical JSON with mac as the last key.
func EncodeLegalHoldRecord(record *LegalHoldRecord, macKey []byte) ([]byte, error) {
	if len(bytes.TrimSpace(macKey)) == 0 {
		return nil, ErrAbuseRecordMacKey
	}
	if !record.Event.known() {
		return nil, fmt.Errorf("%w: event %q", ErrLegalHoldRecordInvalid, record.Event)
	}
	canonical, err := record.canonical()
	if err != nil {
		return nil, err
	}
	mac := hex.EncodeToString(abuseRecordMac(canonical, macKey))
	encoded := bytes.Clone(canonical[:len(canonical)-1])
	encoded = append(encoded, []byte(`,"mac":"`+mac+`"}`)...)
	return encoded, nil
}

// Reads a legal-request record back and verifies its MAC under macKey: every key present, no other, the MAC over
// their canonical form. A record that is not exactly what EncodeLegalHoldRecord writes for its fields does not
// verify (ErrAbuseRecordMac), and §19.10's step re-places nothing from it.
func ParseLegalHoldRecord(body []byte, macKey []byte) (*LegalHoldRecord, error) {
	if len(bytes.TrimSpace(macKey)) == 0 {
		return nil, ErrAbuseRecordMacKey
	}
	var y struct {
		legalHoldRecordJson
		Mac *string `json:"mac"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&y); err != nil {
		return nil, fmt.Errorf("%w: the body does not parse", ErrLegalHoldRecordInvalid)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: bytes after the record", ErrLegalHoldRecordInvalid)
	}
	keyValues := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &keyValues); err != nil {
		return nil, fmt.Errorf("%w: the body does not parse", ErrLegalHoldRecordInvalid)
	}
	for _, key := range legalHoldRecordKeys {
		if _, ok := keyValues[key]; !ok {
			if key == "mac" {
				return nil, ErrAbuseRecordMac
			}
			return nil, fmt.Errorf("%w: a key is missing", ErrLegalHoldRecordInvalid)
		}
	}
	if len(keyValues) != len(legalHoldRecordKeys) {
		return nil, fmt.Errorf("%w: a key it should not hold", ErrLegalHoldRecordInvalid)
	}
	if y.Event == nil || y.CaseId == nil || y.NetworkId == nil || y.Kind == nil || y.PlaceTime == nil || y.ExpireTime == nil || y.Mac == nil {
		return nil, fmt.Errorf("%w: a key is null", ErrLegalHoldRecordInvalid)
	}
	placeTime, err := time.Parse(abuseRecordTimeLayout, *y.PlaceTime)
	if err != nil {
		return nil, fmt.Errorf("%w: place_time", ErrLegalHoldRecordInvalid)
	}
	expireTime, err := time.Parse(abuseRecordTimeLayout, *y.ExpireTime)
	if err != nil {
		return nil, fmt.Errorf("%w: expire_time", ErrLegalHoldRecordInvalid)
	}
	scope, err := parseLegalHoldRecordScope(y.Scope)
	if err != nil {
		return nil, err
	}
	record := &LegalHoldRecord{
		Event:      LegalHoldEvent(*y.Event),
		CaseId:     *y.CaseId,
		NetworkId:  *y.NetworkId,
		Kind:       sidecar.HoldKind(*y.Kind),
		Scope:      scope,
		PlaceTime:  placeTime.UTC(),
		ExpireTime: expireTime.UTC(),
	}
	canonical, err := record.canonical()
	if err != nil {
		return nil, err
	}
	mac, err := hex.DecodeString(*y.Mac)
	if err != nil || !hmac.Equal(mac, abuseRecordMac(canonical, macKey)) {
		return nil, ErrAbuseRecordMac
	}
	if !record.Event.known() || (record.Kind != sidecar.HoldKindPreserve2703f && record.Kind != sidecar.HoldKindReport2258a) {
		return nil, fmt.Errorf("%w: an unknown event or kind", ErrLegalHoldRecordInvalid)
	}
	return record, nil
}

// "network", or 1 to sidecar.HoldScopeMaxRepoCount repo ids.
func parseLegalHoldRecordScope(raw json.RawMessage) (sidecar.HoldScope, error) {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		if value == "network" {
			return sidecar.HoldScopeNetwork(), nil
		}
		return sidecar.HoldScope{}, fmt.Errorf("%w: scope", ErrLegalHoldRecordInvalid)
	}
	var repoIds []sidecar.Id
	if err := json.Unmarshal(raw, &repoIds); err != nil || len(repoIds) == 0 || sidecar.HoldScopeMaxRepoCount < len(repoIds) {
		return sidecar.HoldScope{}, fmt.Errorf("%w: scope", ErrLegalHoldRecordInvalid)
	}
	return sidecar.HoldScopeRepos(repoIds...), nil
}

// Where the legal-request mailbox's records go. Safe for concurrent use. MailLegalHoldRecord returns once the
// record has left the process for its mailbox. A failure leaves it owed, and its task sends it again, so a mailbox
// may hold one record twice; §19.10's step re-places by network and case and takes the copy once.
type LegalHoldRecordMailer interface {
	MailLegalHoldRecord(ctx context.Context, record *LegalHoldRecord) error
}

// The production mailer: MACed under abuse-mailbox.yml's MAC key, sealed to its seal key and mailed to abuse.yml's
// legal_request_mailbox through api's and taskworker's SES path. It sends nothing, answering
// ErrLegalHoldRecordMailerUnavailable, while abuse.yml names no legal_request_mailbox, the key file is missing or
// refused, or the process has no SES sender. The zero value is the production mailer.
type sesLegalHoldRecordMailer struct {
	// nil is DefaultAbuseMailboxSettings
	settings *AbuseMailboxSettings
	// nil is SES (sesAbuseMailboxSender)
	sender abuseMailboxSender
}

// Mails one record: see the type.
func (self *sesLegalHoldRecordMailer) MailLegalHoldRecord(ctx context.Context, record *LegalHoldRecord) error {
	settings := self.settings
	if settings == nil {
		settings = DefaultAbuseMailboxSettings()
	}
	sender := self.sender
	if sender == nil {
		sender = &sesAbuseMailboxSender{settings: settings}
	}
	mailbox := server.GetAbuseConfig().LegalRequestMailbox()
	if mailbox == "" {
		return fmt.Errorf("%w: abuse.yml names no legal_request_mailbox", ErrLegalHoldRecordMailerUnavailable)
	}
	keys, err := LoadAbuseMailboxKeys(settings)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrLegalHoldRecordMailerUnavailable, err)
	}
	plaintext, err := EncodeLegalHoldRecord(record, keys.MacKey)
	if err != nil {
		return err
	}
	if err := mailSealedAbuseMailboxRecord(ctx, sender, mailbox, plaintext, keys, settings); err != nil {
		if errors.Is(err, ErrAbuseMailboxUnavailable) {
			return fmt.Errorf("%w: %w", ErrLegalHoldRecordMailerUnavailable, err)
		}
		return err
	}
	return nil
}

// What a process has before it installs the production mailer: it refuses every record, which stays owed.
type unavailableLegalHoldRecordMailer struct{}

func (self *unavailableLegalHoldRecordMailer) MailLegalHoldRecord(ctx context.Context, record *LegalHoldRecord) error {
	return fmt.Errorf("%w: this process mails no records; api and taskworker do", ErrLegalHoldRecordMailerUnavailable)
}

// The process's legal-request mailer, the post-placement hook and the clock, set at startup or by a test.
var abuseHoldDeps = struct {
	stateLock sync.Mutex
	mailer    LegalHoldRecordMailer
	// runs after a hold stands: ABUSE-M5-15's copy
	placed func(ctx context.Context, hold *model.LegalHold)
	now    func() time.Time
}{
	mailer: &unavailableLegalHoldRecordMailer{},
	// the hold's copy starts at once (ABUSE-M5-15): the copy pass, pulled to now, finds it pending
	placed: func(ctx context.Context, hold *model.LegalHold) {
		model.KickAbuseHoldCopy(ctx)
	},
	now: server.NowUtc,
}

func currentAbuseHoldDeps() (LegalHoldRecordMailer, func(ctx context.Context, hold *model.LegalHold), func() time.Time) {
	abuseHoldDeps.stateLock.Lock()
	defer abuseHoldDeps.stateLock.Unlock()
	return abuseHoldDeps.mailer, abuseHoldDeps.placed, abuseHoldDeps.now
}

// Installs the process's legal-request mailer, returning what puts the previous one back.
func SetLegalHoldRecordMailer(mailer LegalHoldRecordMailer) (restore func()) {
	abuseHoldDeps.stateLock.Lock()
	defer abuseHoldDeps.stateLock.Unlock()
	previous := abuseHoldDeps.mailer
	abuseHoldDeps.mailer = mailer
	return func() {
		abuseHoldDeps.stateLock.Lock()
		defer abuseHoldDeps.stateLock.Unlock()
		abuseHoldDeps.mailer = previous
	}
}

// Installs the production legal-request mailer in this process, in a binary that links MG5: api's and taskworker's
// startup, the two that mount abuse-mailbox.yml (QD-726).
func InstallLegalHoldRecordMailer() (restore func()) {
	if !server.SidecarMg5Linked() {
		return func() {}
	}
	return SetLegalHoldRecordMailer(&sesLegalHoldRecordMailer{})
}

// Replaces the post-placement hook until the returned restore.
func Testing_SetLegalHoldPlacedHook(placed func(ctx context.Context, hold *model.LegalHold)) (restore func()) {
	abuseHoldDeps.stateLock.Lock()
	defer abuseHoldDeps.stateLock.Unlock()
	previous := abuseHoldDeps.placed
	abuseHoldDeps.placed = placed
	return func() {
		abuseHoldDeps.stateLock.Lock()
		defer abuseHoldDeps.stateLock.Unlock()
		abuseHoldDeps.placed = previous
	}
}

// Replaces the clock placement and renewal read until the returned restore.
func Testing_SetLegalHoldNow(now func() time.Time) (restore func()) {
	abuseHoldDeps.stateLock.Lock()
	defer abuseHoldDeps.stateLock.Unlock()
	previous := abuseHoldDeps.now
	abuseHoldDeps.now = now
	return func() {
		abuseHoldDeps.stateLock.Lock()
		defer abuseHoldDeps.stateLock.Unlock()
		abuseHoldDeps.now = previous
	}
}

// The default-level reports of failed record mails.
var legalHoldMailFailures = newBoundedReport(abuseActionReportInterval)

// MailLegalHoldRecordTask's args: the record's fields, ids and times only (api-14's hold fields).
type MailLegalHoldRecordArgs struct {
	Event     LegalHoldEvent `json:"event"`
	NetworkId server.Id      `json:"network_id"`
	CaseId    server.Id      `json:"case_id"`
	Kind      string         `json:"kind"`
	// empty for the network
	ScopeRepoIds []server.Id `json:"scope_repo_ids,omitempty"`
	PlaceTimeMs  int64       `json:"place_time_ms"`
	ExpireTimeMs int64       `json:"expire_time_ms"`
}

type MailLegalHoldRecordResult struct {
}

// The task's args for a record.
func mailLegalHoldRecordArgsOf(record *LegalHoldRecord) *MailLegalHoldRecordArgs {
	args := &MailLegalHoldRecordArgs{
		Event:        record.Event,
		NetworkId:    record.NetworkId,
		CaseId:       record.CaseId,
		Kind:         string(record.Kind),
		PlaceTimeMs:  record.PlaceTime.UnixMilli(),
		ExpireTimeMs: record.ExpireTime.UnixMilli(),
	}
	for _, repoId := range record.Scope.RepoIds {
		args.ScopeRepoIds = append(args.ScopeRepoIds, server.Id(repoId))
	}
	return args
}

// The record the args carry.
func (self *MailLegalHoldRecordArgs) record() *LegalHoldRecord {
	repoIds := []sidecar.Id{}
	for _, repoId := range self.ScopeRepoIds {
		repoIds = append(repoIds, sidecar.Id(repoId))
	}
	return &LegalHoldRecord{
		Event:      self.Event,
		CaseId:     self.CaseId,
		NetworkId:  self.NetworkId,
		Kind:       sidecar.HoldKind(self.Kind),
		Scope:      sidecar.HoldScopeRepos(repoIds...),
		PlaceTime:  time.UnixMilli(self.PlaceTimeMs).UTC(),
		ExpireTime: time.UnixMilli(self.ExpireTimeMs).UTC(),
	}
}

// Owes the legal-request mailbox the record of a hold's event, in the transaction that commits the event: one
// task run, once-keyed by the hold, event and expiry, so a rerun owes one record.
func oweLegalHoldRecordInTx(ctx context.Context, tx server.PgTx, event LegalHoldEvent, hold *model.LegalHold) {
	settings := model.DefaultAbuseHoldSettings()
	record := LegalHoldRecordOf(event, hold)
	owner := session.NewLocalClientSession(ctx, "", nil)
	defer owner.Cancel()
	task.ScheduleTaskInTx(
		tx,
		MailLegalHoldRecordTask,
		mailLegalHoldRecordArgsOf(record),
		owner,
		task.RunOnce("abuse_hold_record", hold.NetworkId, hold.CaseId, string(event), record.ExpireTime.UnixMilli()),
		task.Priority(task.TaskPriorityFastest),
		task.MaxTime(settings.RecordMailTaskMaxTime),
	)
}

// Mails one owed record to the legal-request mailbox. A mailer that refuses or fails leaves it owed: the run
// retries after RecordMailRetryDelay.
func MailLegalHoldRecordTask(args *MailLegalHoldRecordArgs, clientSession *session.ClientSession) (*MailLegalHoldRecordResult, error) {
	settings := model.DefaultAbuseHoldSettings()
	mailer, _, _ := currentAbuseHoldDeps()
	err := mailer.MailLegalHoldRecord(clientSession.Ctx, args.record())
	countAbuseActionStep("hold_mail", err, ErrLegalHoldRecordMailerUnavailable, legalHoldMailFailures)
	if err != nil {
		return nil, task.WithRetryDelay(err, settings.RecordMailRetryDelay)
	}
	return &MailLegalHoldRecordResult{}, nil
}

func MailLegalHoldRecordTaskPost(
	args *MailLegalHoldRecordArgs,
	result *MailLegalHoldRecordResult,
	clientSession *session.ClientSession,
	tx server.PgTx,
) error {
	return nil
}

// What a placement or renewal did.
type LegalHoldResult struct {
	Hold *model.LegalHold
	// false when the hold stood already: a retry
	Inserted bool
	// From when the hold's terms bind every deleter, evs's purge and drop job among them: BindTimeout after the call
	// returned, by when api had seen its object acknowledged (owner decision 629). Before it, a deleter may still act
	// on a read of hold/ from before the write.
	BindTime time.Time
}

// When terms api saw acknowledged by now bind every deleter (owner decision 629).
func legalHoldBindTime(now time.Time) time.Time {
	return now.Add(model.DefaultAbuseHoldSettings().BindTimeout)
}

// Places a hold (model.PlaceLegalHold): the object, then the row with its record owed, then the post-placement
// hook, which runs for a hold that stood already too, since a retry after a crash finishes what the first call
// began. Refuses unless holds_enabled. The result says from when the hold binds every deleter.
func PlaceLegalHold(ctx context.Context, args *model.LegalHoldArgs) (*LegalHoldResult, error) {
	_, placed, now := currentAbuseHoldDeps()
	hold, inserted, err := model.PlaceLegalHold(ctx, args, now(), func(ctx context.Context, tx server.PgTx, hold *model.LegalHold) {
		oweLegalHoldRecordInTx(ctx, tx, LegalHoldEventPlaced, hold)
	})
	if err != nil {
		return nil, err
	}
	// the object was acknowledged before the row committed, so a time read now is at or after its acknowledgement
	bindTime := legalHoldBindTime(now())
	placed(ctx, hold)
	return &LegalHoldResult{Hold: hold, Inserted: inserted, BindTime: bindTime}, nil
}

// Renews a preserve_2703f hold once (model.RenewLegalHold), owing the renewed record. The result says from when the
// renewed terms bind every deleter.
func RenewLegalHold(ctx context.Context, networkId server.Id, caseId server.Id) (*LegalHoldResult, error) {
	_, _, now := currentAbuseHoldDeps()
	hold, err := model.RenewLegalHold(ctx, networkId, caseId, now(), func(ctx context.Context, tx server.PgTx, hold *model.LegalHold) {
		oweLegalHoldRecordInTx(ctx, tx, LegalHoldEventRenewed, hold)
	})
	if err != nil {
		return nil, err
	}
	return &LegalHoldResult{Hold: hold, Inserted: true, BindTime: legalHoldBindTime(now())}, nil
}

// ----- the staff kinds -----

// A hold as the abuse role reads it.
type AbuseStaffHoldView struct {
	NetworkId server.Id `json:"network_id"`
	CaseId    server.Id `json:"case_id"`
	Kind      string    `json:"kind"`
	// "network", or the repo ids
	Scope        json.RawMessage `json:"scope"`
	PlaceTimeMs  int64           `json:"place_time_ms"`
	ReportTimeMs int64           `json:"report_time_ms"`
	RenewTimeMs  int64           `json:"renew_time_ms"`
	ExpireTimeMs int64           `json:"expire_time_ms"`
	CopyState    string          `json:"copy_state"`
}

func abuseStaffHoldView(hold *model.LegalHold) *AbuseStaffHoldView {
	if hold == nil {
		return nil
	}
	return &AbuseStaffHoldView{
		NetworkId:    hold.NetworkId,
		CaseId:       hold.CaseId,
		Kind:         string(hold.Kind),
		Scope:        legalHoldRecordScopeJson(hold.Scope),
		PlaceTimeMs:  hold.PlaceTime.UnixMilli(),
		ReportTimeMs: abuseStaffTimeMs(hold.ReportTime),
		RenewTimeMs:  abuseStaffTimeMs(hold.RenewTime),
		ExpireTimeMs: hold.ExpireTime.UnixMilli(),
		CopyState:    string(hold.CopyState),
	}
}

// place_hold's and renew_hold's answer.
type AbuseStaffHoldResult struct {
	Hold     *AbuseStaffHoldView `json:"hold"`
	Inserted bool                `json:"inserted"`
	// From when the hold's terms bind every deleter (LegalHoldResult.BindTime; owner decision 629): what api's
	// placement says, so the person placing it knows a deleter may act on an older read of hold/ until then.
	BindTimeMs int64 `json:"bind_time_ms"`
}

// place_hold's body.
type abuseStaffPlaceHoldArgs struct {
	Kind      string    `json:"kind"`
	CaseId    server.Id `json:"case_id"`
	NetworkId server.Id `json:"network_id"`
	// preserve_2703f or report_2258a
	HoldKind string `json:"hold_kind"`
	// "network" (the default when absent), or a list of repo ids
	Scope json.RawMessage `json:"scope"`
	// report_2258a: when the report reached NCMEC; 0 for now
	ReportTimeMs int64 `json:"report_time_ms"`
}

// renew_hold's body.
type abuseStaffRenewHoldArgs struct {
	Kind      string    `json:"kind"`
	CaseId    server.Id `json:"case_id"`
	NetworkId server.Id `json:"network_id"`
}

// report_csam's body.
type abuseStaffReportCsamArgs struct {
	Kind   string    `json:"kind"`
	CaseId server.Id `json:"case_id"`
	// the suspension's id, so a retry records it once; absent draws one
	ActionId *server.Id `json:"action_id"`
	// when the report reached NCMEC; 0 for now
	ReportTimeMs int64  `json:"report_time_ms"`
	Reason       string `json:"reason"`
}

// report_csam's answer: the bar, the suspension, and the hold or why it was not placed. The bar and the suspension
// stand whatever the hold does; a hold refused while holds_enabled is false is placed later with place_hold.
type AbuseStaffReportCsamResult struct {
	Case        *AbuseStaffCaseView     `json:"case"`
	Suspension  *AbuseStaffActionResult `json:"suspension"`
	Hold        *AbuseStaffHoldView     `json:"hold,omitempty"`
	HoldPlaced  bool                    `json:"hold_placed"`
	HoldRefusal string                  `json:"hold_refusal,omitempty"`
	// From when a placed hold binds every deleter (owner decision 629), 0 with no hold placed.
	HoldBindTimeMs int64 `json:"hold_bind_time_ms,omitempty"`
}

// A staff body's scope: absent or "network" for the network, else a list of repo ids.
func abuseStaffHoldScope(raw json.RawMessage) (sidecar.HoldScope, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return sidecar.HoldScopeNetwork(), nil
	}
	scope, err := parseLegalHoldRecordScope(raw)
	if err != nil {
		return sidecar.HoldScope{}, fmt.Errorf("%w: a scope that is neither \"network\" nor 1 to %d repo ids", ErrAbuseStaffRequestInvalid, sidecar.HoldScopeMaxRepoCount)
	}
	return scope, nil
}

// A report time in ms, or now for 0.
func abuseStaffReportTime(reportTimeMs int64, now time.Time) *time.Time {
	reportTime := now
	if 0 < reportTimeMs {
		reportTime = time.UnixMilli(reportTimeMs).UTC()
	}
	return &reportTime
}

// Records a CSAM report for staff (§21.9): bars the case, suspends its network through §21's one path, and places
// report_2258a from the report's time.
func abuseStaffReportCsam(ctx context.Context, staff *AbuseStaff, args *abuseStaffReportCsamArgs) (*AbuseStaffReportCsamResult, error) {
	settings := abuseStaffDeps.settings()
	if err := validateAbuseStaffReason(args.Reason, settings); err != nil {
		return nil, err
	}
	// the abuse role's read of the case, logged first (§21.10)
	reader, err := model.NewAbuseReader(staff.ReviewerId, settings)
	if err != nil {
		return nil, err
	}
	caseFile, err := reader.LookupAbuseCase(ctx, args.CaseId)
	if err != nil {
		return nil, err
	}
	abuseCase := caseFile.Case
	if abuseCase.NetworkId == nil {
		return nil, fmt.Errorf("%w: a case that names no network", ErrAbuseStaffRequestInvalid)
	}
	networkId := *abuseCase.NetworkId
	// never the root or a platform name (§21.4), as every staff action is checked
	target := model.AbuseTarget{Kind: model.AbuseTargetKindNetwork, Name: networkId.String()}
	if _, err := model.NextAbuseStep(model.AbuseCategoryCsam, target, nil, abuseStaffDeps.now(), &settings.AbusePolicySettings); err != nil {
		return nil, err
	}
	// barred before the suspension, so its path writes no notice, admin event or hint
	if err := model.BarAbuseCase(ctx, args.CaseId); err != nil {
		return nil, err
	}
	actionId := server.NewId()
	if args.ActionId != nil {
		actionId = *args.ActionId
	}
	reviewerId := staff.ReviewerId
	suspension, err := ApplyAbuseAction(ctx, &model.AbuseActionArgs{
		ActionId:     actionId,
		CaseId:       args.CaseId,
		Kind:         model.AbuseActionKindNetworkSuspend,
		Target:       networkId.String(),
		ReviewerId:   &reviewerId,
		ReviewReason: args.Reason,
	})
	if err != nil {
		return nil, err
	}
	caseView := abuseStaffCaseView(abuseCase)
	caseView.Barred = true
	result := &AbuseStaffReportCsamResult{
		Case:       caseView,
		Suspension: abuseStaffActionResultOf(suspension),
	}
	_, _, now := currentAbuseHoldDeps()
	placed, err := PlaceLegalHold(ctx, &model.LegalHoldArgs{
		NetworkId:  networkId,
		CaseId:     args.CaseId,
		Kind:       sidecar.HoldKindReport2258a,
		Scope:      sidecar.HoldScopeNetwork(),
		ReportTime: abuseStaffReportTime(args.ReportTimeMs, now()),
	})
	if err != nil {
		result.HoldRefusal = err.Error()
		return result, nil
	}
	result.Hold = abuseStaffHoldView(placed.Hold)
	result.HoldPlaced = true
	result.HoldBindTimeMs = placed.BindTime.UnixMilli()
	return result, nil
}

// The hold verbs.
func init() {
	legalRoles := []AbuseStaffRole{AbuseStaffRoleLegal}

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "place_hold",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffPlaceHoldArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			scope, err := abuseStaffHoldScope(args.Scope)
			if err != nil {
				return nil, err
			}
			holdArgs := &model.LegalHoldArgs{
				NetworkId: args.NetworkId,
				CaseId:    args.CaseId,
				Kind:      sidecar.HoldKind(args.HoldKind),
				Scope:     scope,
			}
			if holdArgs.Kind == sidecar.HoldKindReport2258a {
				_, _, now := currentAbuseHoldDeps()
				holdArgs.ReportTime = abuseStaffReportTime(args.ReportTimeMs, now())
			} else if args.ReportTimeMs != 0 {
				return nil, fmt.Errorf("%w: a report time for a hold that takes none", ErrAbuseStaffRequestInvalid)
			}
			result, err := PlaceLegalHold(ctx, holdArgs)
			if err != nil {
				return nil, err
			}
			return &AbuseStaffHoldResult{Hold: abuseStaffHoldView(result.Hold), Inserted: result.Inserted, BindTimeMs: result.BindTime.UnixMilli()}, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "renew_hold",
		Roles: legalRoles,
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffRenewHoldArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			result, err := RenewLegalHold(ctx, args.NetworkId, args.CaseId)
			if err != nil {
				return nil, err
			}
			return &AbuseStaffHoldResult{Hold: abuseStaffHoldView(result.Hold), Inserted: result.Inserted, BindTimeMs: result.BindTime.UnixMilli()}, nil
		},
	})

	RegisterAbuseStaffKind(&AbuseStaffKind{
		Name:  "report_csam",
		Roles: []AbuseStaffRole{AbuseStaffRoleReviewer, AbuseStaffRoleLegal},
		Run: func(ctx context.Context, staff *AbuseStaff, body []byte) (any, error) {
			args := &abuseStaffReportCsamArgs{}
			if err := decodeAbuseStaffArgs(body, args); err != nil {
				return nil, err
			}
			return abuseStaffReportCsam(ctx, staff, args)
		},
	})
}

// ----- the pass -----

// The steps of a hold's end, which AbuseHoldPassDeps.Step sees with the case id, in this order.
const (
	AbuseHoldPassStepScheduled     = "abuse.hold.pass.scheduled"
	AbuseHoldPassStepObjectDeleted = "abuse.hold.pass.object_deleted"
)

// What one pass runs on.
type AbuseHoldPassDeps struct {
	// the hold/ writer; nil ends and puts back nothing, since the object goes before the row
	Store *model.AbuseHoldStore
	// where each ended hold's sweep pass is queued; nil ends nothing
	Tasks sidecar.Tasks
	// the pass's time
	Now func() time.Time
	// nil for the defaults
	Settings *model.AbuseHoldSettings
	// sees each step of a hold's end with its case; nil in production
	Step func(step string, caseId server.Id)
}

// What one pass did. Counts only, never a network or case.
type AbuseHoldPassResult struct {
	// holds ended past their expiry: sweep queued, object and rows gone
	EndedCount int
	// objects with no row ended past their own expiry
	UnrecordedEndedCount int
	// live rows whose missing object was put back
	RestoredCount int
	// live rows whose object held other terms, rewritten
	RewrittenCount int
	// live holds, every part 0 not past its expiry
	LiveCount int
	// objects with no row, still before their expiry: the alarm
	UnrecordedCount int
	// objects no reader can read as a hold: kept, and alarmed
	UnreadableCount int
	// holds the pass could not finish this time, left to the next
	FailedCount int
}

// The default-level reports of the pass's alarms and failures.
var (
	abuseHoldPassAlarms   = newBoundedReport(abuseActionReportInterval)
	abuseHoldPassFailures = newBoundedReport(abuseActionReportInterval)
)

// One pass over holds (see the file header): ends each hold past its expiry, puts back each live row's object, and
// looks for objects with no row, until done, PassMaxTime has passed or ctx ends, then sets urnetwork_abuse_holds.
// Idempotent: a pass killed between queueing a hold's sweep and deleting its object queues it again. A binary
// that does not link MG5 does nothing.
func RunAbuseHoldPass(ctx context.Context, deps *AbuseHoldPassDeps) *AbuseHoldPassResult {
	result := &AbuseHoldPassResult{}
	if !server.SidecarMg5Linked() {
		return result
	}
	settings := deps.Settings
	if settings == nil {
		settings = model.DefaultAbuseHoldSettings()
	}
	now := deps.Now()
	startTime := time.Now()
	outOfTime := func() bool {
		return ctx.Err() != nil || settings.PassMaxTime <= time.Since(startTime)
	}
	step := func(name string, caseId server.Id) {
		if deps.Step != nil {
			deps.Step(name, caseId)
		}
	}
	counts := model.AbuseHoldCounts{}
	complete := true

	// holds past their expiry
	cursor := model.LegalHoldCursor{}
	for !outOfTime() {
		holds, next := model.ListExpiredLegalHolds(ctx, now, cursor, settings.PassBatchSize)
		if len(holds) == 0 {
			break
		}
		cursor = next
		for _, hold := range holds {
			if outOfTime() {
				complete = false
				break
			}
			if err := endAbuseHold(ctx, deps, hold, now, step); err != nil {
				result.FailedCount += 1
				reportAbuseHoldPassFailure("end", err)
				continue
			}
			result.EndedCount += 1
		}
	}

	// live holds and their objects
	liveCursor := model.LegalHoldCursor{}
	for !outOfTime() {
		holds, next := model.ListLiveLegalHolds(ctx, now, liveCursor, settings.PassBatchSize)
		if len(holds) == 0 {
			break
		}
		liveCursor = next
		for _, hold := range holds {
			result.LiveCount += 1
			counts.Add(hold.Kind, model.AbuseHoldStateLive)
			if deps.Store == nil || outOfTime() {
				continue
			}
			restored, rewritten, err := reconcileAbuseHoldObject(ctx, deps.Store, hold)
			if err != nil {
				result.FailedCount += 1
				reportAbuseHoldPassFailure("reconcile", err)
				continue
			}
			if restored {
				result.RestoredCount += 1
			}
			if rewritten {
				result.RewrittenCount += 1
			}
		}
	}
	if outOfTime() {
		complete = false
	}

	// objects with no row
	if deps.Store != nil && !outOfTime() {
		if err := scanUnrecordedAbuseHolds(ctx, deps, settings, now, counts, result, outOfTime, step); err != nil {
			result.FailedCount += 1
			reportAbuseHoldPassFailure("scan", err)
			complete = false
		}
	} else if deps.Store == nil {
		complete = false
	}

	if complete {
		// a partial pass would set a count it did not finish making
		model.SetAbuseHoldsGauge(counts)
	}
	if 0 < result.UnrecordedCount || 0 < result.UnreadableCount {
		if suppressedCount, ok := abuseHoldPassAlarms.Allow(time.Now()); ok {
			glog.Warningf(
				"[abuse]%d hold/ objects have no row and %d do not read as holds; each keeps its network's data, and the abuse role compares them with the legal-request mailbox; %d more reports since the last\n",
				result.UnrecordedCount,
				result.UnreadableCount,
				suppressedCount,
			)
		}
	}
	return result
}

// Says a failed step of the pass at most once per interval, naming no network or case.
func reportAbuseHoldPassFailure(what string, err error) {
	if suppressedCount, ok := abuseHoldPassFailures.Allow(time.Now()); ok {
		glog.Infof("[abuse]the hold pass could not %s a hold, and the next pass retries; %d more since the last report\n", what, suppressedCount)
	}
	if glog.V(1) {
		glog.Infof("[abuse]the hold pass's %s failed: %s\n", what, err)
	}
}

// Ends one hold past its expiry: queues its sweep pass with its record, then deletes its object, then its rows,
// owing the ended record. A hold that reads live again in the last step, as a restore could make it, gets its
// object back.
func endAbuseHold(ctx context.Context, deps *AbuseHoldPassDeps, hold *model.LegalHold, now time.Time, step func(string, server.Id)) error {
	if deps.Store == nil || deps.Tasks == nil {
		return fmt.Errorf("%w: no hold/ writer or task queue", model.ErrLegalHoldStoreUnavailable)
	}
	record := hold.Record()
	request, err := evsservice.EvsSweepHoldEndTaskRequest(record, hold.ExpireTime)
	if err != nil {
		return err
	}
	if err := deps.Tasks.Schedule(ctx, request); err != nil {
		return err
	}
	step(AbuseHoldPassStepScheduled, hold.CaseId)
	if err := deps.Store.Delete(ctx, hold.NetworkId, hold.CaseId); err != nil {
		return err
	}
	step(AbuseHoldPassStepObjectDeleted, hold.CaseId)
	var current *model.LegalHold
	var ended bool
	server.Tx(ctx, func(tx server.PgTx) {
		current, ended = model.EndLegalHoldInTx(ctx, tx, hold.NetworkId, hold.CaseId, now)
		if ended {
			oweLegalHoldRecordInTx(ctx, tx, LegalHoldEventEnded, current)
		}
	})
	if !ended && current != nil && current.Live(now) {
		return deps.Store.Rewrite(ctx, current.Record())
	}
	return nil
}

// Puts back a live row's object when it is gone, and rewrites one that holds other terms unless those terms keep
// at least what the row's do, such as a renewal whose row has not committed: every doubt keeps data.
func reconcileAbuseHoldObject(ctx context.Context, store *model.AbuseHoldStore, hold *model.LegalHold) (restored bool, rewritten bool, err error) {
	record := hold.Record()
	body, err := sidecar.EncodeHoldRecord(record)
	if err != nil {
		// a row no object can say, such as one with no placement: its readers already keep everything for an
		// unreadable object, and nothing better can be written
		return false, false, err
	}
	object, found, err := store.Read(ctx, hold.NetworkId, hold.CaseId)
	if err != nil {
		return false, false, err
	}
	if !found {
		if err := store.Rewrite(ctx, record); err != nil {
			return false, false, err
		}
		return true, false, nil
	}
	if bytes.Equal(object.Body, body) {
		return false, false, nil
	}
	if object.Record != nil && abuseHoldKeepsAtLeast(object.Record, record) {
		return false, false, nil
	}
	if err := store.Rewrite(ctx, record); err != nil {
		return false, false, err
	}
	return false, true, nil
}

// Whether held keeps at least what want keeps: as late an expiry, as late a placement (which keeps more of what
// existed), and a scope that contains want's.
func abuseHoldKeepsAtLeast(held *sidecar.HoldRecord, want *sidecar.HoldRecord) bool {
	if held.ExpireTime.Before(want.ExpireTime) || held.PlaceTime.Before(want.PlaceTime) {
		return false
	}
	if held.Scope.Network() {
		return true
	}
	if want.Scope.Network() {
		return false
	}
	for _, repoId := range want.Scope.RepoIds {
		if !slices.Contains(held.Scope.RepoIds, repoId) {
			return false
		}
	}
	return true
}

// Lists hold/ for objects with no part 0: one past its own expiry is ended as a row's would be, its sweep queued
// from its own terms first; one before it stays and alarms (UnrecordedCount); one no reader can read stays and
// alarms (UnreadableCount).
func scanUnrecordedAbuseHolds(
	ctx context.Context,
	deps *AbuseHoldPassDeps,
	settings *model.AbuseHoldSettings,
	now time.Time,
	counts model.AbuseHoldCounts,
	result *AbuseHoldPassResult,
	outOfTime func() bool,
	step func(string, server.Id),
) error {
	startAfter := ""
	for !outOfTime() {
		infos, more, err := deps.Store.List(ctx, startAfter, settings.ListPageCount)
		if err != nil {
			return err
		}
		if len(infos) == 0 {
			return nil
		}
		startAfter = infos[len(infos)-1].Key
		networkIds := []server.Id{}
		for _, info := range infos {
			if networkId, _, ok := model.ParseAbuseHoldKey(info.Key); ok {
				networkIds = append(networkIds, networkId)
			}
		}
		recorded := model.LegalHoldKeysOf(ctx, networkIds)
		for _, info := range infos {
			networkId, caseId, object, found, ok, err := deps.Store.ReadListed(ctx, info.Key)
			if err != nil {
				return err
			}
			if !ok {
				result.UnreadableCount += 1
				counts.Add("", model.AbuseHoldStateUnreadable)
				continue
			}
			if recorded[model.LegalHoldKey{NetworkId: networkId, CaseId: caseId}] || !found {
				continue
			}
			if object.Record == nil {
				result.UnreadableCount += 1
				counts.Add("", model.AbuseHoldStateUnreadable)
				continue
			}
			if object.Record.Live(now) {
				result.UnrecordedCount += 1
				counts.Add(object.Record.Kind, model.AbuseHoldStateUnrecorded)
				continue
			}
			if deps.Tasks == nil {
				result.FailedCount += 1
				continue
			}
			request, err := evsservice.EvsSweepHoldEndTaskRequest(object.Record, object.Record.ExpireTime)
			if err != nil {
				result.FailedCount += 1
				continue
			}
			if err := deps.Tasks.Schedule(ctx, request); err != nil {
				return err
			}
			step(AbuseHoldPassStepScheduled, caseId)
			if err := deps.Store.Delete(ctx, networkId, caseId); err != nil {
				return err
			}
			step(AbuseHoldPassStepObjectDeleted, caseId)
			result.UnrecordedEndedCount += 1
		}
		if !more {
			return nil
		}
	}
	return nil
}

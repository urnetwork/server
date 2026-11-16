package controller

// §21.10's restore and §19.10's abuse step (ABUSE-M5-18; D-ABUSE-7, D-OPS-14, FS-42, FI-12, I-14): the mechanism
// OPS-M5-08's step runs after the dumps load and before any block reads the restored rows, the dns step's snapshot
// or the token exchange, so it needs Postgres and Redis and no api. A restore lifts no action.
//
//	ParseAbuseRecords     reads an exported mailbox file (mbox: each message a "From " line, headers and a
//	                      text/plain body of the sealed record's base64), not the live account; opens each with the
//	                      offline private key (ParseAbuseMailboxSealPrivateKey, QD-832) and verifies its MAC under
//	                      abuse-mailbox.yml's key (QD-078); a record that does not open, parse or verify is refused
//	                      and printed, and nothing is applied from it
//	PlanAbuseRestore      keeps, in mail order, every action not past its until and every lift committed after the
//	                      sidecar dump; mail order is the records' commit times, the mailbox's order breaking ties, so
//	                      a lift always follows the action it lifts even when the action's mail was retried later
//	ApplyAbuseRestorePlan prints the plan, ids and kinds only, and applies only when told to (reading 16): each item
//	                      under its own id, idempotently, through ApplyAbuseAction's effects (barred, so no hint),
//	                      with no new record mail, no notice and no hint, or history would gain a false second action
//	ReplaceLegalHolds     re-places the holds the legal-request mailbox holds from after the dump with their
//	                      original placement and lists each for counsel; after a site loss a hold's kept objects are
//	                      void, and the list says so
//
// A lift mailed after the dump also removes what the restored row holds: its effect returns what the restored action
// withheld. A suspension's zone suspension while the root is restoring sets rows only, so the dns step's snapshot
// carries it (DNS-M5-14). The gap is real: an action committed in the seconds before the loss, its mail not yet
// sent, is gone; tlsedge still keeps a kill Redis holds; and feeds recur (§21.10). §12.6's restore flags are cleared
// through POST /abuse/action's clear_restore_flag, by the abuse role.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hpke"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// One message of an exported mailbox, opened, or why it could not be.
type AbuseMailboxMessage struct {
	// its place in the export, from 0
	Index int
	// the sealed record's plaintext, nil when Err is set
	Plaintext []byte
	Err       error
}

// The messages of an mbox export: each starts at a line beginning "From " at the file's start or after a blank
// line, and holds headers, a blank line and a body.
func splitAbuseMailboxExport(data []byte) [][]byte {
	messages := [][]byte{}
	var current *bytes.Buffer
	previousBlank := true
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), len(data)+1)
	for scanner.Scan() {
		line := scanner.Text()
		if previousBlank && strings.HasPrefix(line, "From ") {
			if current != nil {
				messages = append(messages, current.Bytes())
			}
			current = &bytes.Buffer{}
			previousBlank = false
			continue
		}
		if current != nil {
			current.WriteString(line)
			current.WriteString("\n")
		}
		previousBlank = strings.TrimSpace(line) == ""
	}
	if current != nil {
		messages = append(messages, current.Bytes())
	}
	return messages
}

// A message's text body, its transfer encoding undone.
func abuseMailboxMessageBody(message []byte) (string, error) {
	parsed, err := mail.ReadMessage(bytes.NewReader(message))
	if err != nil {
		return "", fmt.Errorf("the message does not parse")
	}
	var body io.Reader = parsed.Body
	switch strings.ToLower(strings.TrimSpace(parsed.Header.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit", "binary":
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, &abuseMailboxBase64Lines{reader: bufio.NewReader(body)})
	default:
		return "", fmt.Errorf("an unknown transfer encoding")
	}
	text, err := io.ReadAll(body)
	if err != nil {
		return "", fmt.Errorf("the body does not decode")
	}
	return string(text), nil
}

// A base64 transfer encoding's lines, their line breaks dropped.
type abuseMailboxBase64Lines struct {
	reader *bufio.Reader
}

func (self *abuseMailboxBase64Lines) Read(p []byte) (int, error) {
	n := 0
	for n < len(p) {
		b, err := self.reader.ReadByte()
		if err != nil {
			if n == 0 {
				return 0, err
			}
			return n, nil
		}
		if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
			continue
		}
		p[n] = b
		n += 1
	}
	return n, nil
}

// Opens every message of an mbox export with the seal's offline private key. A message that does not parse, decode
// or open keeps its error and its place, so the plan prints it.
func OpenAbuseMailboxExport(data []byte, privateKey hpke.PrivateKey, settings *AbuseMailboxSettings) []*AbuseMailboxMessage {
	opened := []*AbuseMailboxMessage{}
	for index, message := range splitAbuseMailboxExport(data) {
		entry := &AbuseMailboxMessage{Index: index}
		body, err := abuseMailboxMessageBody(message)
		if err == nil {
			var sealed []byte
			if sealed, err = DecodeAbuseMailboxBody(body, settings); err == nil {
				entry.Plaintext, err = OpenAbuseMailboxRecord(privateKey, sealed, settings)
			}
		}
		entry.Err = err
		opened = append(opened, entry)
	}
	return opened
}

// A record of the export the step refuses, and why: it did not open, parse or verify.
type AbuseRestoreRefusal struct {
	Index int
	Err   error
}

// One record of the abuse mailbox, verified, at its place in the export.
type AbuseRestoreRecord struct {
	Index  int
	Record *AbuseRecord
}

// An export's verified records and its refusals.
type AbuseMailboxRecords struct {
	Records []*AbuseRestoreRecord
	Refused []*AbuseRestoreRefusal
}

// Reads an exported abuse mailbox (see the file header): each record opened with the offline private key and its MAC
// verified under macKey. Refusals keep their place; a missing or failing MAC is ErrAbuseRecordMac.
func ParseAbuseRecords(data []byte, privateKey hpke.PrivateKey, macKey []byte, settings *AbuseMailboxSettings) *AbuseMailboxRecords {
	records := &AbuseMailboxRecords{}
	for _, message := range OpenAbuseMailboxExport(data, privateKey, settings) {
		if message.Err != nil {
			records.Refused = append(records.Refused, &AbuseRestoreRefusal{Index: message.Index, Err: message.Err})
			continue
		}
		record, err := ParseAbuseRecord(message.Plaintext, macKey)
		if err != nil {
			records.Refused = append(records.Refused, &AbuseRestoreRefusal{Index: message.Index, Err: err})
			continue
		}
		records.Records = append(records.Records, &AbuseRestoreRecord{Index: message.Index, Record: record})
	}
	return records
}

// An exported legal-request mailbox's verified records and its refusals.
type LegalHoldMailboxRecords struct {
	Records []*LegalHoldRecord
	Refused []*AbuseRestoreRefusal
}

// Reads an exported legal-request mailbox as ParseAbuseRecords reads the abuse mailbox, with
// ParseLegalHoldRecord's MAC check.
func ParseLegalHoldRecords(data []byte, privateKey hpke.PrivateKey, macKey []byte, settings *AbuseMailboxSettings) *LegalHoldMailboxRecords {
	records := &LegalHoldMailboxRecords{}
	for _, message := range OpenAbuseMailboxExport(data, privateKey, settings) {
		if message.Err != nil {
			records.Refused = append(records.Refused, &AbuseRestoreRefusal{Index: message.Index, Err: message.Err})
			continue
		}
		record, err := ParseLegalHoldRecord(message.Plaintext, macKey)
		if err != nil {
			records.Refused = append(records.Refused, &AbuseRestoreRefusal{Index: message.Index, Err: err})
			continue
		}
		records.Records = append(records.Records, record)
	}
	return records
}

// What §19.10's abuse step re-applies, in order.
type AbuseRestorePlan struct {
	DumpTime time.Time
	Now      time.Time
	// every action and lift committed after the dump, an action not past its until, in mail order
	Items []*AbuseRestoreRecord
	// records committed at or before the dump, which the restored rows hold
	BeforeDumpCount int
	// actions committed after the dump whose until had passed
	ExpiredCount int
	// the export's refusals, printed and never applied
	Refused []*AbuseRestoreRefusal
}

// Plans the step at now for a sidecar dump taken at dumpTime (see the file header).
func PlanAbuseRestore(records *AbuseMailboxRecords, dumpTime time.Time, now time.Time) *AbuseRestorePlan {
	plan := &AbuseRestorePlan{DumpTime: dumpTime, Now: now, Refused: slices.Clone(records.Refused)}
	seen := map[server.Id]bool{}
	for _, entry := range records.Records {
		record := entry.Record
		if !record.Time.After(dumpTime) {
			plan.BeforeDumpCount += 1
			continue
		}
		if seen[record.ActionId] {
			// a record mailed twice, as a retried mail may be: once is enough
			continue
		}
		seen[record.ActionId] = true
		if record.Kind != model.AbuseActionKindLift && record.Until != nil && !now.Before(*record.Until) {
			plan.ExpiredCount += 1
			continue
		}
		plan.Items = append(plan.Items, entry)
	}
	slices.SortStableFunc(plan.Items, func(a *AbuseRestoreRecord, b *AbuseRestoreRecord) int {
		if c := a.Record.Time.Compare(b.Record.Time); c != 0 {
			return c
		}
		return a.Index - b.Index
	})
	return plan
}

// What applying one item did.
type AbuseRestoreOutcome string

const (
	// recorded now, its effect applied
	AbuseRestoreOutcomeApplied AbuseRestoreOutcome = "applied"
	// recorded and applied by an earlier run, or by the restored rows
	AbuseRestoreOutcomeAlready AbuseRestoreOutcome = "already"
	// recorded, and its effect left owed for the action pass
	AbuseRestoreOutcomeOwed AbuseRestoreOutcome = "owed"
	// not recorded: its case or lifted action is unknown, or its id names another row
	AbuseRestoreOutcomeRefused AbuseRestoreOutcome = "refused"
)

// What one run of the step did, by outcome.
type AbuseRestoreResult struct {
	Applied bool
	// each item's outcome, in the plan's order
	Outcomes []AbuseRestoreOutcome
}

// Prints the plan, ids and kinds only.
func printAbuseRestorePlan(out io.Writer, plan *AbuseRestorePlan) {
	fmt.Fprintf(out, "abuse restore: dump %s, now %s: %d to re-apply, %d before the dump, %d past their until, %d refused\n",
		plan.DumpTime.UTC().Format(time.RFC3339), plan.Now.UTC().Format(time.RFC3339), len(plan.Items), plan.BeforeDumpCount, plan.ExpiredCount, len(plan.Refused))
	for _, refusal := range plan.Refused {
		fmt.Fprintf(out, "  refused  message %d: %v\n", refusal.Index, refusal.Err)
	}
	for _, item := range plan.Items {
		record := item.Record
		networkText := "-"
		if record.NetworkId != nil {
			networkText = record.NetworkId.String()
		}
		untilText := "-"
		if record.Until != nil {
			untilText = record.Until.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "  %s  action %s  case %s  network %s  until %s  at %s\n",
			record.Kind, record.ActionId, record.CaseId, networkText, untilText, record.Time.UTC().Format(time.RFC3339))
	}
}

// Prints the plan, then, only when apply is set, applies each item in order (see the file header). A second run
// changes nothing. The error is a run that could not go on, such as a binary that does not link MG5; an item that
// cannot be applied is its outcome.
func ApplyAbuseRestorePlan(ctx context.Context, plan *AbuseRestorePlan, apply bool, out io.Writer) (*AbuseRestoreResult, error) {
	printAbuseRestorePlan(out, plan)
	result := &AbuseRestoreResult{}
	if !apply {
		fmt.Fprintf(out, "abuse restore: nothing applied; run again with the apply flag once the plan is read\n")
		return result, nil
	}
	if !server.SidecarMg5Linked() {
		return nil, model.ErrAbuseUnlinked
	}
	result.Applied = true
	settings := model.DefaultAbuseSettings()
	for _, item := range plan.Items {
		outcome := applyAbuseRestoreItem(ctx, item.Record, plan.Now, settings)
		result.Outcomes = append(result.Outcomes, outcome)
		fmt.Fprintf(out, "  %s  action %s\n", outcome, item.Record.ActionId)
	}
	return result, nil
}

// One item: its row under its id, then its effect, barred so it hints nothing, never a mail or a notice.
func applyAbuseRestoreItem(ctx context.Context, record *AbuseRecord, now time.Time, settings *model.AbuseSettings) AbuseRestoreOutcome {
	restored := &model.AbuseRestoredAction{
		ActionId:  record.ActionId,
		CaseId:    record.CaseId,
		Category:  record.Category,
		NetworkId: record.NetworkId,
		Kind:      record.Kind,
		Target:    record.Target,
		UntilTime: record.Until,
		Time:      record.Time,
	}
	var action *model.AbuseAction
	var lifted *model.AbuseAction
	var err error
	server.Tx(ctx, func(tx server.PgTx) {
		if record.Kind == model.AbuseActionKindLift {
			action, lifted, _, err = model.RestoreAbuseLiftInTx(ctx, tx, restored, now, settings)
		} else {
			action, _, err = model.RestoreAbuseActionInTx(ctx, tx, restored, now)
		}
		if err == nil {
			reevaluateAbuseStandingInTx(ctx, tx, action, now, settings)
		}
	})
	if errors.Is(err, model.ErrAbuseActionLifted) {
		return AbuseRestoreOutcomeAlready
	}
	if err != nil {
		return AbuseRestoreOutcomeRefused
	}
	if action.AppliedTime != nil {
		return AbuseRestoreOutcomeAlready
	}
	applied, _, _, _ := completeAbuseAction(ctx, true, action, lifted, true, false)
	if !applied {
		return AbuseRestoreOutcomeOwed
	}
	return AbuseRestoreOutcomeApplied
}

// What re-placing one hold did.
type LegalHoldRestoreOutcome string

const (
	// re-placed, or its expiry raised to a renewal's
	LegalHoldRestoreOutcomePlaced LegalHoldRestoreOutcome = "placed"
	// the restored rows held it already
	LegalHoldRestoreOutcomeAlready LegalHoldRestoreOutcome = "already"
	// past its expiry, or ended by a record after the dump
	LegalHoldRestoreOutcomeEnded LegalHoldRestoreOutcome = "ended"
	// its object could not be written; the next run writes it
	LegalHoldRestoreOutcomeObjectOwed LegalHoldRestoreOutcome = "object-owed"
)

// One hold as ReplaceLegalHolds lists it for counsel.
type LegalHoldRestoreLine struct {
	NetworkId server.Id
	CaseId    server.Id
	Kind      sidecar.HoldKind
	PlaceTime time.Time
	// its expiry, renewal included
	ExpireTime time.Time
	Outcome    LegalHoldRestoreOutcome
	// a site loss took what it kept on the file cluster, which has no off-site copy (owner decision 94)
	Void bool
}

// Re-places the holds the legal-request mailbox's records hold from after the sidecar dump (see the file header):
// each hold's latest record governs, a hold ended by its record or past its expiry is listed as ended, and every
// other is re-placed with its original placement, its object written through this process's hold/ writer, and
// listed for counsel; after a site loss (siteLost) each is void, and the list says so. Prints the list, ids only,
// and writes only when apply is set. Idempotent.
func ReplaceLegalHolds(ctx context.Context, records []*LegalHoldRecord, dumpTime time.Time, now time.Time, siteLost bool, apply bool, out io.Writer) ([]*LegalHoldRestoreLine, error) {
	latest := map[model.LegalHoldKey]*LegalHoldRecord{}
	order := []model.LegalHoldKey{}
	for _, record := range records {
		key := model.LegalHoldKey{NetworkId: record.NetworkId, CaseId: record.CaseId}
		previous, seen := latest[key]
		if !seen {
			order = append(order, key)
		}
		// a hold's records come placed, renewed, ended: the later expiry or the end governs
		if !seen || record.Event == LegalHoldEventEnded || (previous.Event != LegalHoldEventEnded && !record.ExpireTime.Before(previous.ExpireTime)) {
			latest[key] = record
		}
	}
	var store *model.AbuseHoldStore
	if apply {
		if !server.SidecarMg5Linked() {
			return nil, model.ErrAbuseUnlinked
		}
		var err error
		if store, err = model.CurrentAbuseHoldStore(); err != nil {
			return nil, err
		}
	}
	lines := []*LegalHoldRestoreLine{}
	for _, key := range order {
		record := latest[key]
		// the latest record before the dump: the restored rows hold this hold as it then stood
		if !record.PlaceTime.After(dumpTime) && record.Event == LegalHoldEventPlaced {
			continue
		}
		line := &LegalHoldRestoreLine{
			NetworkId:  record.NetworkId,
			CaseId:     record.CaseId,
			Kind:       record.Kind,
			PlaceTime:  record.PlaceTime,
			ExpireTime: record.ExpireTime,
			Void:       siteLost,
		}
		lines = append(lines, line)
		if record.Event == LegalHoldEventEnded || !now.Before(record.ExpireTime) {
			line.Outcome = LegalHoldRestoreOutcomeEnded
			continue
		}
		if !apply {
			line.Outcome = LegalHoldRestoreOutcomePlaced
			continue
		}
		restored := &model.RestoredLegalHold{
			NetworkId:  record.NetworkId,
			CaseId:     record.CaseId,
			Kind:       record.Kind,
			Scope:      record.Scope,
			PlaceTime:  record.PlaceTime,
			ExpireTime: record.ExpireTime,
			Renewed:    record.Event == LegalHoldEventRenewed,
		}
		var hold *model.LegalHold
		var placed bool
		server.Tx(ctx, func(tx server.PgTx) {
			hold, placed = model.ReplaceLegalHoldInTx(ctx, tx, restored)
		})
		line.Outcome = LegalHoldRestoreOutcomeAlready
		if placed {
			line.Outcome = LegalHoldRestoreOutcomePlaced
		}
		if err := store.Rewrite(ctx, hold.Record()); err != nil {
			line.Outcome = LegalHoldRestoreOutcomeObjectOwed
		}
	}
	fmt.Fprintf(out, "legal holds: %d after the dump%s\n", len(lines), map[bool]string{true: ", each void: the site took what it kept", false: ""}[siteLost])
	for _, line := range lines {
		fmt.Fprintf(out, "  %s  network %s  case %s  %s  placed %s  expires %s\n",
			line.Outcome, line.NetworkId, line.CaseId, line.Kind, line.PlaceTime.UTC().Format(time.RFC3339), line.ExpireTime.UTC().Format(time.RFC3339))
	}
	return lines, nil
}

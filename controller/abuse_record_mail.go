package controller

// §21.10's off-site record: one mail per action or lift, to abuse.yml's
// abuse_mailbox, sent straight after the action commits (plan-ABUSE-small-readings
// #10), so a Postgres restore to the sidecar dump's age can re-apply every
// action and lift committed since, in mail order and idempotently by action_id
// (§19.10's abuse step). It does not ride account_message_outbox, whose
// one-minute cycle sits in the same Postgres the record exists to survive.
//
// The record is JSON with exactly §21.10's fields and a MAC (QD-078):
//
//	{"action_id":"<id>","case_id":"<id>","category":"<category>","network_id":"<id>"|null,
//	 "kind":"<kind>","target":"<target>","until":"<time>"|null,"time":"<time>","mac":"<hex>"}
//
// mac is HMAC-SHA-256, in lowercase hex, over the other fields as canonical JSON:
// these keys in this order, no whitespace, no HTML escaping, ids in their 8-4-4-4-12
// form and times in RFC 3339 UTC with milliseconds. The key is vault's
// abuse_mailbox_mac_key, so a record no one at URnetwork MACed does not
// re-apply: a seal alone would not do, since anyone can seal to a public key.
// A record holds no reporter data, no reason and no reviewer: what a restore
// re-applies, and nothing more.
//
// Before a record leaves the process it is sealed to a vault-held HPKE key, so
// the mailbox provider and SES see sender, time and size only (QD-030). Neither
// the sealed bytes nor the keys' vault homes are settled
// (abuse-m5-03-record-seal.md), so the production mailer refuses every record:
// each stays owed, mailed_time NULL, and the action pass retries it, and nothing
// unsealed ever leaves.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The record's time form: RFC 3339 in UTC, to the millisecond.
const abuseRecordTimeLayout = "2006-01-02T15:04:05.000Z"

// Every key of a mailed record, §21.10's eight and the MAC.
var abuseRecordKeys = []string{"action_id", "case_id", "category", "network_id", "kind", "target", "until", "time", "mac"}

// A record that does not parse, holds a key it should not, or lacks one.
var ErrAbuseRecordInvalid = errors.New("abuse record invalid")

// A record whose MAC is missing or does not verify. §19.10's abuse step refuses
// and prints it, and applies nothing from it.
var ErrAbuseRecordMac = errors.New("abuse record mac does not verify")

// No MAC key, or a blank one: anyone can MAC under an absent key, so nothing is
// MACed or verified without one.
var ErrAbuseRecordMacKey = errors.New("abuse record mac key unavailable")

// The mailer cannot send any record now: no mailbox, no seal or no key. The
// record stays owed, and a pass that meets this mails nothing more until its
// next run.
var ErrAbuseRecordMailerUnavailable = errors.New("abuse record mailer unavailable")

// §21.10's record of one action or lift.
type AbuseRecord struct {
	ActionId server.Id
	CaseId   server.Id
	Category model.AbuseCategory
	// nil for a platform-wide breaker level (plan-ABUSE-metadata-rows #5)
	NetworkId *server.Id
	Kind      model.AbuseActionKind
	// what it acted on; a lift's is the lifted action's id
	Target string
	// when it ends by itself; nil when only a lift ends it
	Until *time.Time
	// when it committed
	Time time.Time
}

// The record of an action as committed.
func AbuseRecordOf(action *model.AbuseAction) *AbuseRecord {
	return &AbuseRecord{
		ActionId:  action.ActionId,
		CaseId:    action.CaseId,
		Category:  action.Category,
		NetworkId: action.NetworkId,
		Kind:      action.Kind,
		Target:    action.Target,
		Until:     action.UntilTime,
		Time:      action.CreateTime,
	}
}

// The record's fields as JSON, in the canonical order. Pointers tell an absent
// key from a null one when parsing.
type abuseRecordJson struct {
	ActionId  *server.Id `json:"action_id"`
	CaseId    *server.Id `json:"case_id"`
	Category  *string    `json:"category"`
	NetworkId *server.Id `json:"network_id"`
	Kind      *string    `json:"kind"`
	Target    *string    `json:"target"`
	Until     *string    `json:"until"`
	Time      *string    `json:"time"`
}

// The canonical JSON of the record's fields, without its MAC.
func (self *AbuseRecord) canonical() ([]byte, error) {
	category := string(self.Category)
	kind := string(self.Kind)
	recordTime := self.Time.UTC().Format(abuseRecordTimeLayout)
	y := &abuseRecordJson{
		ActionId:  &self.ActionId,
		CaseId:    &self.CaseId,
		Category:  &category,
		NetworkId: self.NetworkId,
		Kind:      &kind,
		Target:    &self.Target,
		Time:      &recordTime,
	}
	if self.Until != nil {
		until := self.Until.UTC().Format(abuseRecordTimeLayout)
		y.Until = &until
	}
	b := &bytes.Buffer{}
	encoder := json.NewEncoder(b)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(y); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

// HMAC-SHA-256 of canonical under macKey.
func abuseRecordMac(canonical []byte, macKey []byte) []byte {
	mac := hmac.New(sha256.New, macKey)
	mac.Write(canonical)
	return mac.Sum(nil)
}

// The record's bytes as mailed before the seal: its canonical JSON with mac as
// the last key.
func EncodeAbuseRecord(record *AbuseRecord, macKey []byte) ([]byte, error) {
	if len(bytes.TrimSpace(macKey)) == 0 {
		return nil, ErrAbuseRecordMacKey
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

// Reads a record back and verifies its MAC under macKey: every field present,
// no other key, the MAC over their canonical form. A record that is not exactly
// what EncodeAbuseRecord writes for its fields does not verify.
func ParseAbuseRecord(body []byte, macKey []byte) (*AbuseRecord, error) {
	if len(bytes.TrimSpace(macKey)) == 0 {
		return nil, ErrAbuseRecordMacKey
	}
	var y struct {
		abuseRecordJson
		Mac *string `json:"mac"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&y); err != nil {
		return nil, fmt.Errorf("%w: the body does not parse", ErrAbuseRecordInvalid)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w: bytes after the record", ErrAbuseRecordInvalid)
	}
	// every key present, network_id and until even when null, and no other
	keyValues := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &keyValues); err != nil {
		return nil, fmt.Errorf("%w: the body does not parse", ErrAbuseRecordInvalid)
	}
	for _, key := range abuseRecordKeys {
		if _, ok := keyValues[key]; !ok {
			if key == "mac" {
				return nil, ErrAbuseRecordMac
			}
			return nil, fmt.Errorf("%w: a key is missing", ErrAbuseRecordInvalid)
		}
	}
	if len(keyValues) != len(abuseRecordKeys) {
		return nil, fmt.Errorf("%w: a key it should not hold", ErrAbuseRecordInvalid)
	}
	if y.ActionId == nil || y.CaseId == nil || y.Category == nil || y.Kind == nil || y.Target == nil || y.Time == nil || y.Mac == nil {
		return nil, fmt.Errorf("%w: a key is null", ErrAbuseRecordInvalid)
	}
	recordTime, err := time.Parse(abuseRecordTimeLayout, *y.Time)
	if err != nil {
		return nil, fmt.Errorf("%w: time", ErrAbuseRecordInvalid)
	}
	record := &AbuseRecord{
		ActionId:  *y.ActionId,
		CaseId:    *y.CaseId,
		Category:  model.AbuseCategory(*y.Category),
		NetworkId: y.NetworkId,
		Kind:      model.AbuseActionKind(*y.Kind),
		Target:    *y.Target,
		Time:      recordTime.UTC(),
	}
	if y.Until != nil {
		until, err := time.Parse(abuseRecordTimeLayout, *y.Until)
		if err != nil {
			return nil, fmt.Errorf("%w: until", ErrAbuseRecordInvalid)
		}
		until = until.UTC()
		record.Until = &until
	}
	canonical, err := record.canonical()
	if err != nil {
		return nil, err
	}
	mac, err := hex.DecodeString(*y.Mac)
	if err != nil || !hmac.Equal(mac, abuseRecordMac(canonical, macKey)) {
		return nil, ErrAbuseRecordMac
	}
	return record, nil
}

// Where §21.10's records go. Safe for concurrent use. MailAbuseRecord returns
// once the record has left the process for its mailbox, and only then may the
// caller mark it mailed. A record that fails stays owed, and the action pass
// sends it again, so a mailbox may hold one record twice; a restore applies by
// action_id and takes the copy once.
type AbuseRecordMailer interface {
	MailAbuseRecord(ctx context.Context, record *AbuseRecord) error
}

// The production mailer, through api's and taskworker's SES path. It refuses
// every record until the seal and its keys are settled
// (abuse-m5-03-record-seal.md), as it must not send a record unsealed, and
// before that while abuse.yml names no mailbox.
type sesAbuseRecordMailer struct{}

// Refuses: see the type.
func (self *sesAbuseRecordMailer) MailAbuseRecord(ctx context.Context, record *AbuseRecord) error {
	if server.GetAbuseConfig().AbuseMailbox() == "" {
		return fmt.Errorf("%w: abuse.yml names no abuse_mailbox", ErrAbuseRecordMailerUnavailable)
	}
	return fmt.Errorf("%w: records are sealed before they leave, and the seal is not built (abuse-m5-03-record-seal.md)", ErrAbuseRecordMailerUnavailable)
}

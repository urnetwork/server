package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// §21.10's record (abuse_record_mail.go). Pure: a synthetic MAC key and an
// override of abuse.yml.

// A synthetic MAC key, as vault's abuse_mailbox_mac_key would be.
var testAbuseRecordMacKey = []byte("synthetic abuse mailbox mac key, test only")

func testAbuseRecord() *AbuseRecord {
	networkId := server.NewId()
	until := time.Date(2026, 11, 9, 12, 30, 0, 250*int(time.Millisecond), time.UTC)
	return &AbuseRecord{
		ActionId:  server.NewId(),
		CaseId:    server.NewId(),
		Category:  model.AbuseCategoryPhishing,
		NetworkId: &networkId,
		Kind:      model.AbuseActionKindHostKill,
		Target:    "shop.root.example",
		Until:     &until,
		Time:      time.Date(2026, 11, 8, 12, 30, 0, 125*int(time.Millisecond), time.UTC),
	}
}

// A record parses back to its fields, with exactly §21.10's keys and the MAC,
// in that order, and nothing of a report.
func TestAbuseRecordRoundTrips(t *testing.T) {
	record := testAbuseRecord()
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseAbuseRecord(body, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.ActionId != record.ActionId || parsed.CaseId != record.CaseId || parsed.Category != record.Category ||
		*parsed.NetworkId != *record.NetworkId || parsed.Kind != record.Kind || parsed.Target != record.Target ||
		!parsed.Until.Equal(*record.Until) || !parsed.Time.Equal(record.Time) {
		t.Fatalf("parsed %+v, want %+v", parsed, record)
	}

	keyValues := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &keyValues); err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for key := range keyValues {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	want := slices.Clone(abuseRecordKeys)
	slices.Sort(want)
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
	// the canonical order, MAC last
	order := []int{}
	for _, key := range abuseRecordKeys {
		order = append(order, bytes.Index(body, []byte(`"`+key+`":`)))
	}
	if !slices.IsSorted(order) {
		t.Fatalf("keys out of order in %s", body)
	}
	if !bytes.Contains(body, []byte(`"time":"2026-11-08T12:30:00.125Z"`)) || !bytes.Contains(body, []byte(`"until":"2026-11-09T12:30:00.250Z"`)) {
		t.Fatalf("times are not RFC 3339 UTC to the millisecond: %s", body)
	}
	for _, reporterWord := range []string{"reporter", "reason", "reviewer", "url", "address", "text"} {
		if bytes.Contains(body, []byte(reporterWord)) {
			t.Errorf("the record holds %q: %s", reporterWord, body)
		}
	}
}

// A breaker level has no network, and an action that only a lift ends has no
// until: both keys stay, as null.
func TestAbuseRecordKeepsNullKeys(t *testing.T) {
	record := testAbuseRecord()
	record.NetworkId = nil
	record.Until = nil
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(`"network_id":null`)) || !bytes.Contains(body, []byte(`"until":null`)) {
		t.Fatalf("null keys dropped: %s", body)
	}
	parsed, err := ParseAbuseRecord(body, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.NetworkId != nil || parsed.Until != nil {
		t.Fatalf("parsed %+v", parsed)
	}
}

// A record no one at URnetwork MACed does not parse: a field changed, the MAC
// gone, another key's MAC, or a form that is not canonical. A blank key MACs and
// verifies nothing.
func TestAbuseRecordMacRefusesForgeries(t *testing.T) {
	record := testAbuseRecord()
	body, err := EncodeAbuseRecord(record, testAbuseRecordMacKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		body []byte
		key  []byte
		want error
	}{
		{name: "a changed kind", body: bytes.Replace(body, []byte(`"host_kill"`), []byte(`"lift"`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "a changed target", body: bytes.Replace(body, []byte(`shop.root`), []byte(`shoq.root`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "another key", body: body, key: []byte("another synthetic key"), want: ErrAbuseRecordMac},
		{name: "no mac", body: []byte(strings.SplitN(string(body), `,"mac"`, 2)[0] + `}`), key: testAbuseRecordMacKey, want: ErrAbuseRecordMac},
		{name: "a time to the second", body: bytes.Replace(body, []byte(`12:30:00.125Z`), []byte(`12:30:00Z`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a time with a zone", body: bytes.Replace(body, []byte(`12:30:00.125Z`), []byte(`13:30:00.125+01:00`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "an extra key", body: bytes.Replace(body, []byte(`{"action_id"`), []byte(`{"reporter":"x","action_id"`), 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a missing key", body: bytes.Replace(body, []byte(`"until":"2026-11-09T12:30:00.250Z",`), nil, 1), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "trailing bytes", body: append(bytes.Clone(body), []byte(` {}`)...), key: testAbuseRecordMacKey, want: ErrAbuseRecordInvalid},
		{name: "a blank key", body: body, key: []byte(" \n"), want: ErrAbuseRecordMacKey},
		{name: "no key", body: body, key: nil, want: ErrAbuseRecordMacKey},
	} {
		if parsed, err := ParseAbuseRecord(c.body, c.key); !errors.Is(err, c.want) {
			t.Errorf("%s parses as %+v: %v", c.name, parsed, err)
		}
	}
	if _, err := EncodeAbuseRecord(record, nil); !errors.Is(err, ErrAbuseRecordMacKey) {
		t.Errorf("a record MACs under no key: %v", err)
	}
}

// The production mailer sends nothing unsealed: with no mailbox, and with one,
// it refuses as unavailable, so the record stays owed.
func TestProductionAbuseRecordMailerRefuses(t *testing.T) {
	mailer := &sesAbuseRecordMailer{}
	for _, yaml := range []string{"", "abuse_mailbox: records@mailbox.example\n"} {
		func() {
			defer server.Testing_PushAbuseConfig(yaml)()
			if err := mailer.MailAbuseRecord(context.Background(), testAbuseRecord()); !errors.Is(err, ErrAbuseRecordMailerUnavailable) {
				t.Errorf("abuse.yml %q: the mailer answered %v", yaml, err)
			}
		}()
	}
}

// The record of an action is its row's fields, a lift's target the lifted id.
func TestAbuseRecordOfAnAction(t *testing.T) {
	networkId := server.NewId()
	liftedId := server.NewId()
	action := &model.AbuseAction{
		ActionId:   server.NewId(),
		CaseId:     server.NewId(),
		NetworkId:  &networkId,
		Category:   model.AbuseCategoryCopyright,
		Kind:       model.AbuseActionKindLift,
		Target:     liftedId.String(),
		CreateTime: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC),
	}
	record := AbuseRecordOf(action)
	if record.ActionId != action.ActionId || record.CaseId != action.CaseId || *record.NetworkId != networkId ||
		record.Kind != model.AbuseActionKindLift || record.Target != liftedId.String() || record.Until != nil ||
		!record.Time.Equal(action.CreateTime) {
		t.Fatalf("record %+v", record)
	}
}

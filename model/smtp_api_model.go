package model

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base32"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/smtpedgeservice"

	"github.com/urnetwork/server"
)

// §14.14's api routes, their model half (SMTP-M9-19; R-SMTP-10, D-SMTP-5, D-SMTP-9, I-6, I-15, FS-42, C11):
//
//	POST /smtp/dkim-keys   a sending sidecar's selectors, network and burner ones and a verified Team domain's,
//	                       public keys only, each RSA-2048 (owner decision 36): PublishSmtpDkimKeys writes
//	                       smtp_dkim_key, §12's records through DNS-M9-01's writers and their admin events in one
//	                       transaction, past dns_dkim_selectors (64 device selectors, burners' excluded) refusing
//	                       the whole call; a retirement keeps the selector in DNS 7 days, after which SMTP-M9-17's
//	                       key expiry takes it out
//	GET /smtp/status       the network's smtp limits, today's counts as the edges' {smq_} keeps them, and the halts
//	                       holding now with their scope (GetSmtpStatus)
//	GET /smtp/reports      the network's attributed reports with their sealed copies and its aggregate counts, a
//	                       page at a time on a cursor (GetSmtpReportsPage)
//	POST /smtp/appeal      an admin's recheck of the network's halts, which the handler runs on SMTP-M9-16's ladder
//
// and §7.10's removal (I-15): the "smtp_dkim" effect takes a removed sidecar's selectors out of DNS in the
// removal's own write (DeleteSmtpDkimKeysForClientInTx), and §19.10's replay applies it again.
//
// The two §8 checks, mail_send for a publish and read on the repo that holds the network mail key for the reports
// (plan-SMTP-small-readings#16), are api's gate cache's (C-09): SmtpApiGateOf over that cache
// (smtp_api_gate_model.go), which api's process installs as SmtpApiGate where its projection publishes the cache
// (controller.installApiProjection). With none installed both routes answer 503, so no publish or report read passes
// an unmade check.
//
// Every DKIM write locks the network's label, then its zones in root order (lockSmtpDkimNetworkZonesInTx), then
// its quota, then its selector rows, as key expiry and the removal effect do. No recipient, client IP or key
// reaches a log; selectors are opaque and random, so a log may name one (I-13). Nothing here reads or writes a
// table its group does not link.

// Tunables of the routes' model half.
type SmtpApiSettings struct {
	// The most selectors one POST /smtp/dkim-keys names, published and retired together: a rotation names two
	// per domain, and a sidecar signs for its network and its replying burners, so 32. Positive.
	DkimKeysMaxCount int
	// Complaints, and aggregate reports, one GET /smtp/reports page holds at most. Positive.
	ReportsPageCount int
	// The sealed copies' bytes one page carries at most: a copy that would pass it starts the next page, a
	// page's first copy always taken. Positive.
	ReportsPageSealedByteCount int
	// Aggregate rows one page reads at most, its reports kept whole. Positive.
	ReportsPageAggregateRowCount int
	// The most bytes a ?cursor= decodes to: 2048, several times what a cursor this route gives holds, so a forged one is
	// refused before its JSON is parsed. Non-positive refuses every cursor.
	ReportsCursorMaxByteCount int
	// The longest label a DKIM domain may hold, RFC 1035's 63 octets: a protocol limit, so its default is the
	// protocol's. Non-positive refuses every domain.
	DkimDomainLabelMaxByteCount int
}

func DefaultSmtpApiSettings() *SmtpApiSettings {
	return &SmtpApiSettings{
		DkimKeysMaxCount:             32,
		ReportsPageCount:             100,
		ReportsPageSealedByteCount:   8 * 1024 * 1024,
		ReportsPageAggregateRowCount: 1000,
		ReportsCursorMaxByteCount:    2048,
		DkimDomainLabelMaxByteCount:  63,
	}
}

// The selector cap's key in pro.yml's sidecar_abuse: (§12.7; owner decision 36).
const SmtpDkimSelectorsLimitKey = "dns_dkim_selectors"

// Format values of a selector (plan-SMTP-small-readings#5) and of the keys published (§14.6).
const (
	smtpDkimSelectorLength          = 26
	smtpDkimSelectorRandomByteCount = 16
	smtpDkimRsaKeyBitCount          = 2048
)

// A selector's alphabet, lowercase base32 with no padding.
var smtpDkimSelectorEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// The routes' coded refusals: a 4xx status with a code the sidecar reads (SidecarApiError.Code), and a message
// that names no value of the caller's.
type SmtpApiError struct {
	Status  int
	Code    string
	Message string
}

func (self *SmtpApiError) Error() string {
	return fmt.Sprintf("%d %s", self.Status, self.Message)
}

// The body RaiseHttpError answers a 4xx with.
func (self *SmtpApiError) HttpErrorResultBody() any {
	return &struct {
		Error string `json:"error"`
		Code  string `json:"code"`
	}{Error: self.Message, Code: self.Code}
}

// The refusals' codes.
const (
	SmtpApiCodeArgs          = "smtp_dkim_args"
	SmtpApiCodeDomain        = "smtp_dkim_domain"
	SmtpApiCodeSelectorTaken = "smtp_dkim_selector_taken"
	SmtpApiCodeMailSend      = "mail_send"
	SmtpApiCodeReportsRead   = "smtp_reports_read"
	SmtpApiCodeCursor        = "smtp_reports_cursor"
	SmtpApiCodeNoSidecar     = "smtp_no_sidecar"
)

func smtpApiRefusal(status int, code string, message string) *SmtpApiError {
	return &SmtpApiError{Status: status, Code: code, Message: message}
}

// What a route answers when its state cannot be read or written now: its groups unlinked, its §8 check not
// installed or not answering, or its zone not taking writes. The handler answers 503.
var ErrSmtpApiUnavailable = errors.New("smtp api unavailable")

// §14.14's two §8 checks, through api's one gate cache (C-09). Safe for concurrent use.
type SmtpApiGate interface {
	// Whether §8 grants the sidecar mail_send (AclGateView.MaySendMail).
	MaySendMail(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error)
	// Whether §8 grants the sidecar read on the repo that holds the network mail key, the audience the sealed
	// copies are sealed to (plan-SMTP-small-readings#16).
	MayReadReports(ctx context.Context, networkId server.Id, sidecarId server.Id, now time.Time) (bool, error)
}

var smtpApiGateState = struct {
	stateLock sync.Mutex
	gate      SmtpApiGate
}{}

// Installs the process's §8 checks for the routes, returning what puts the previous ones back. With none, both
// checked routes answer 503.
func SetSmtpApiGate(gate SmtpApiGate) (restore func()) {
	smtpApiGateState.stateLock.Lock()
	defer smtpApiGateState.stateLock.Unlock()
	previous := smtpApiGateState.gate
	smtpApiGateState.gate = gate
	return func() {
		smtpApiGateState.stateLock.Lock()
		defer smtpApiGateState.stateLock.Unlock()
		smtpApiGateState.gate = previous
	}
}

// The installed checks, nil for none.
func CurrentSmtpApiGate() SmtpApiGate {
	smtpApiGateState.stateLock.Lock()
	defer smtpApiGateState.stateLock.Unlock()
	return smtpApiGateState.gate
}

// Whether the selector is the 26 characters of 16 octets in its one spelling.
func smtpDkimSelectorForm(selector string) bool {
	if len(selector) != smtpDkimSelectorLength {
		return false
	}
	decoded, err := smtpDkimSelectorEncoding.DecodeString(selector)
	return err == nil && len(decoded) == smtpDkimSelectorRandomByteCount && smtpDkimSelectorEncoding.EncodeToString(decoded) == selector
}

// Whether the name is a lowercase LDH domain of at least two labels that a selector's record name can hold.
func smtpDkimDomainForm(domain string, settings *SmtpApiSettings) bool {
	if domain == "" || smtpDomainMaxByteCount-smtpDkimSelectorLength-len("._domainkey.") < len(domain) || strings.ToLower(domain) != domain {
		return false
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return false
	}
	for _, label := range labels {
		if label == "" || settings.DkimDomainLabelMaxByteCount < len(label) || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Whether the key is DER SubjectPublicKeyInfo of an RSA key of 2048 bits.
func smtpDkimRsaPublicKey(publicKey []byte) bool {
	parsed, err := x509.ParsePKIXPublicKey(publicKey)
	if err != nil {
		return false
	}
	key, ok := parsed.(*rsa.PublicKey)
	return ok && key.N.BitLen() == smtpDkimRsaKeyBitCount
}

// The key record's text for an RSA key (RFC 6376 §3.6.1).
func smtpDkimRecordValue(publicKey []byte) string {
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(publicKey)
}

// Who calls POST /smtp/dkim-keys: a sidecar of the network through its current client.
type SmtpDkimKeysCaller struct {
	NetworkId server.Id
	ClientId  server.Id
	SidecarId server.Id
	// The admin events' request id, when the call carries one.
	RequestId *server.Id
}

// Why args cannot be applied, before anything is read.
func smtpDkimKeysArgsRefusal(args *sidecar.SmtpDkimKeysArgs, settings *SmtpApiSettings) error {
	if args == nil || len(args.Publish)+len(args.Retire) == 0 {
		return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "The call publishes and retires nothing.")
	}
	if settings.DkimKeysMaxCount < len(args.Publish)+len(args.Retire) {
		return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "The call names too many selectors.")
	}
	selectors := map[string]bool{}
	for _, publish := range args.Publish {
		if publish == nil || !smtpDkimSelectorForm(publish.Selector) || !smtpDkimDomainForm(publish.Domain, settings) || !smtpDkimRsaPublicKey(publish.PublicKey) {
			return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "A selector to publish needs its 26 characters, a domain and an RSA-2048 public key.")
		}
		if selectors[publish.Selector] {
			return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "A selector is named twice.")
		}
		selectors[publish.Selector] = true
	}
	for _, retire := range args.Retire {
		if retire == nil || !smtpDkimSelectorForm(retire.Selector) || !smtpDkimDomainForm(retire.Domain, settings) {
			return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "A selector to retire needs its 26 characters and a domain.")
		}
		if selectors[retire.Selector] {
			return smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeArgs, "A selector is named twice.")
		}
		selectors[retire.Selector] = true
	}
	return nil
}

// Where a published selector's record goes in root, whose apex is the network's label there: the burner writer's
// label for a replying burner's domain, "" for the network's own name and for a Team custom domain the network may
// send From now (GetSmtpDkimCustomDomains), whose delegated _domainkey or per-selector CNAMEs answer from the apex
// (§14.6). ok false for any other domain; custom whether it is a custom domain's.
func smtpDkimPublishTarget(domain string, apex string, customDomains []string) (burnerLabel string, custom bool, ok bool) {
	if burnerLabel, ok := smtpDkimBurnerLabelOf(domain, apex); ok {
		return burnerLabel, false, true
	}
	if slices.Contains(customDomains, domain) {
		return "", true, true
	}
	return "", false, false
}

// A row of the network's selectors the call reads.
type smtpDkimKeyRow struct {
	selector  string
	domain    string
	sidecarId server.Id
	publicKey []byte
	state     string
}

// The network's selectors still in DNS, published or retired, locked for the call.
func getSmtpDkimLiveKeysInTx(ctx context.Context, tx server.PgTx, networkId server.Id) []*smtpDkimKeyRow {
	rows := []*smtpDkimKeyRow{}
	result, err := tx.Query(
		ctx,
		`
		SELECT selector, domain, sidecar_id, public_key, state
		FROM smtp_dkim_key
		WHERE network_id = $1 AND state IN ($2, $3)
		ORDER BY selector, domain
		FOR UPDATE
		`,
		networkId,
		SmtpDkimKeyStatePublished,
		SmtpDkimKeyStateRetired,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			row := &smtpDkimKeyRow{}
			server.Raise(result.Scan(&row.selector, &row.domain, &row.sidecarId, &row.publicKey, &row.state))
			rows = append(rows, row)
		}
	})
	return rows
}

// The row stored under selector at domain, whatever its state and network, locked; nil for none.
func getSmtpDkimKeyRowInTx(ctx context.Context, tx server.PgTx, selector string, domain string) (row *smtpDkimKeyRow, networkId server.Id) {
	result, err := tx.Query(
		ctx,
		`SELECT selector, domain, sidecar_id, public_key, state, network_id FROM smtp_dkim_key WHERE selector = $1 AND domain = $2 FOR UPDATE`,
		selector,
		domain,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			row = &smtpDkimKeyRow{}
			server.Raise(result.Scan(&row.selector, &row.domain, &row.sidecarId, &row.publicKey, &row.state, &networkId))
		}
	})
	return
}

// POST /smtp/dkim-keys's write (see the file header), at now. In one transaction: each retirement of a selector
// the caller holds marks it retired, kept in DNS 7 days, and one it does not hold, or already retired, is no
// error; each publish of a selector the caller published with the same key is no error, and any other stored
// selector is refused; each new one past the network's dns_dkim_selectors device selectors, burners' never
// counted, refuses the whole call with the quota's refusal; each new one is a smtp_dkim_key row and its record,
// with its admin event, through §12's writers. The answer echoes every selector published and retired.
// mailSend is asked, once, only when the call publishes. A refusal changes nothing.
func PublishSmtpDkimKeys(
	ctx context.Context,
	caller *SmtpDkimKeysCaller,
	args *sidecar.SmtpDkimKeysArgs,
	mailSend func() (bool, error),
	now time.Time,
	settings *SmtpApiSettings,
) (*sidecar.SmtpDkimKeysResult, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		return nil, fmt.Errorf("%w: DKIM keys need MG2, MG5 and MG9", ErrSmtpApiUnavailable)
	}
	if caller == nil || caller.NetworkId == (server.Id{}) || caller.SidecarId == (server.Id{}) {
		return nil, smtpApiRefusal(http.StatusForbidden, SmtpApiCodeNoSidecar, "The caller is no sidecar of the network.")
	}
	if err := smtpDkimKeysArgsRefusal(args, settings); err != nil {
		return nil, err
	}
	root := DnsUserRoot()
	if root == "" {
		return nil, fmt.Errorf("%w: no dns root", ErrSmtpApiUnavailable)
	}
	if 0 < len(args.Publish) {
		if mailSend == nil {
			return nil, fmt.Errorf("%w: no mail_send check", ErrSmtpApiUnavailable)
		}
		allowed, err := mailSend()
		if err != nil {
			return nil, fmt.Errorf("%w: the mail_send check: %w", ErrSmtpApiUnavailable, err)
		}
		if !allowed {
			return nil, smtpApiRefusal(http.StatusForbidden, SmtpApiCodeMailSend, "The sidecar holds no mail_send grant.")
		}
	}
	// read before the transaction, which holds the one connection: the Team custom domains the gate would send From
	customDomains := GetSmtpDkimCustomDomains(ctx, caller.NetworkId, root, now, DefaultSmtpCustomDomainSettings())

	result := &sidecar.SmtpDkimKeysResult{Published: []string{}, Retired: []string{}}
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			refuse := func(err error) {
				panic(dnsWriteRefusal{err: err})
			}
			label, err := EnsureNetworkLabelInTx(ctx, tx, caller.NetworkId, root)
			if err != nil {
				refuse(err)
			}
			lockSmtpDkimNetworkZonesInTx(ctx, tx, caller.NetworkId)
			LockNetworkQuotaInTx(ctx, tx, caller.NetworkId)
			apex := label + "." + strings.TrimSuffix(dnsLabelRoot(root), ".")

			// each new selector's place, refusing a domain before anything is written
			targets := map[string]string{}
			customPublished := false
			for _, publish := range args.Publish {
				burnerLabel, custom, ok := smtpDkimPublishTarget(publish.Domain, apex, customDomains)
				if !ok {
					refuse(smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeDomain, "A domain is not the network's, a replying burner's or a verified Team domain the network may send from."))
				}
				targets[publish.Selector] = burnerLabel
				customPublished = customPublished || custom
			}

			// the device selectors published now, burners' never counted (§12.7)
			publishedDeviceCount := int64(0)
			for _, row := range getSmtpDkimLiveKeysInTx(ctx, tx, caller.NetworkId) {
				if row.state != SmtpDkimKeyStatePublished {
					continue
				}
				if burnerLabel, ok := smtpDkimBurnerLabelOf(row.domain, apex); ok && burnerLabel != "" {
					continue
				}
				publishedDeviceCount += 1
			}

			for _, retire := range args.Retire {
				row, networkId := getSmtpDkimKeyRowInTx(ctx, tx, retire.Selector, retire.Domain)
				if row != nil && networkId == caller.NetworkId && row.sidecarId == caller.SidecarId && row.state == SmtpDkimKeyStatePublished {
					server.RaisePgResult(tx.Exec(
						ctx,
						`UPDATE smtp_dkim_key SET state = $3, retire_time = $4 WHERE selector = $1 AND domain = $2`,
						retire.Selector,
						retire.Domain,
						SmtpDkimKeyStateRetired,
						now.UTC(),
					))
					if burnerLabel, ok := smtpDkimBurnerLabelOf(row.domain, apex); !ok || burnerLabel == "" {
						publishedDeviceCount -= 1
					}
				}
				// a selector the caller does not hold, or holds retired, is no error
				result.Retired = append(result.Retired, retire.Selector)
			}

			sidecarId := caller.SidecarId
			for _, publish := range args.Publish {
				row, networkId := getSmtpDkimKeyRowInTx(ctx, tx, publish.Selector, publish.Domain)
				if row != nil {
					if networkId != caller.NetworkId || row.sidecarId != caller.SidecarId || row.state != SmtpDkimKeyStatePublished || string(row.publicKey) != string(publish.PublicKey) {
						refuse(smtpApiRefusal(http.StatusConflict, SmtpApiCodeSelectorTaken, "A selector is stored under another key, another sidecar, or retired."))
					}
					// the same key published again: answered as before
					result.Published = append(result.Published, publish.Selector)
					continue
				}
				burnerLabel := targets[publish.Selector]
				if burnerLabel == "" {
					if err := CheckCapacityInTx(ctx, tx, caller.NetworkId, SmtpDkimSelectorsLimitKey, publishedDeviceCount); err != nil {
						refuse(err)
					}
					publishedDeviceCount += 1
				}
				key := &smtpedgeservice.SmtpDkimKey{
					Selector:  publish.Selector,
					Domain:    publish.Domain,
					NetworkId: sidecar.Id(caller.NetworkId),
					SidecarId: sidecar.Id(caller.SidecarId),
					Algorithm: SmtpDkimAlgorithmRsaSha256,
					PublicKey: publish.PublicKey,
				}
				if inserted, err := InsertSmtpDkimKeyInTx(ctx, tx, key, now); err != nil || !inserted {
					if err == nil {
						err = smtpApiRefusal(http.StatusConflict, SmtpApiCodeSelectorTaken, "A selector is stored under another key, another sidecar, or retired.")
					}
					refuse(err)
				}
				selector := &DnsDkimSelector{
					Root:        root,
					NetworkId:   caller.NetworkId,
					Selector:    publish.Selector,
					BurnerLabel: burnerLabel,
					Value:       smtpDkimRecordValue(publish.PublicKey),
					SidecarId:   &sidecarId,
					ActorKind:   AdminEventActorSidecar,
					ActorId:     &sidecarId,
					RequestId:   caller.RequestId,
				}
				write := DnsPutDkimSelectorInTx
				if burnerLabel != "" {
					write = DnsPutBurnerSelectorInTx
				}
				if _, err := write(ctx, tx, selector); err != nil {
					refuse(err)
				}
				result.Published = append(result.Published, publish.Selector)
			}

			// a custom domain's selectors answer under its delegated _domainkey from the apex's rows: the network's
			// delegated row names each of its verified domains (smtp_custom_domain_model.go)
			if customPublished {
				if _, err := setSmtpCustomDomainDelegationsInTx(ctx, tx, root, caller.NetworkId); err != nil {
					refuse(err)
				}
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		// a zone restoring or suspended, and the quota's refusal, reach the handler as they are
		return nil, err
	}
	return result, nil
}

// The name §7.10's chain runs the DKIM effect under.
const RemovalEffectSmtpDkim = "smtp_dkim"

func init() {
	RegisterRemovalEffect(RemovalEffectSmtpDkim, applySmtpDkimRemovalEffectInTx)
}

// The removal effect (I-15; §14.6: removal deletes the sidecar's selectors in the same write): each removed
// sidecar's selectors, in network then sidecar order so two removals lock alike. Idempotent: a removed key's row
// is never published again, so a second run, §19.10's replay among them, writes nothing.
func applySmtpDkimRemovalEffectInTx(ctx context.Context, tx server.PgTx, removal *Removal) error {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		return nil
	}
	subjects := slices.Clone(removal.SubjectsOf(sidecar.RevocationKindSidecar))
	slices.SortFunc(subjects, func(a *RemovalSubject, b *RemovalSubject) int {
		if order := a.NetworkId.Cmp(b.NetworkId); order != 0 {
			return order
		}
		return a.SubjectId.Cmp(b.SubjectId)
	})
	at := removal.Time
	if at.IsZero() {
		at = server.NowUtc()
	}
	for _, subject := range subjects {
		if _, err := DeleteSmtpDkimKeysForClientInTx(ctx, tx, subject.NetworkId, subject.SubjectId, at); err != nil {
			return err
		}
	}
	return nil
}

// Takes every selector of the sending sidecar sidecarId of networkId, its client's to the routes, out of DNS in
// tx, the removal's own, and marks its rows removed at removeTime, kept 30 days for late reports (§14.14).
// Returns how many left DNS. A zone that takes no write now, restoring or suspended, keeps that selector's record:
// its row is marked retired as of DkimRetiredKeepTimeout before removeTime, so key expiry takes it out at its next
// run once the zone takes writes, and the removal still commits (I-15 never waits on a zone).
func DeleteSmtpDkimKeysForClientInTx(ctx context.Context, tx server.PgTx, networkId server.Id, sidecarId server.Id, removeTime time.Time) (removedCount int, returnErr error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		return 0, nil
	}
	lockSmtpDkimNetworkZonesInTx(ctx, tx, networkId)
	refs := []*smtpDkimKeyRef{}
	result, err := tx.Query(
		ctx,
		`
		SELECT selector, domain
		FROM smtp_dkim_key
		WHERE sidecar_id = $1 AND network_id = $2 AND state IN ($3, $4)
		ORDER BY selector, domain
		FOR UPDATE
		`,
		sidecarId,
		networkId,
		SmtpDkimKeyStatePublished,
		SmtpDkimKeyStateRetired,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			ref := &smtpDkimKeyRef{networkId: networkId, sidecarId: sidecarId}
			server.Raise(result.Scan(&ref.selector, &ref.domain))
			refs = append(refs, ref)
		}
	})
	expiryRetireTime := removeTime.UTC().Add(-DefaultSmtpRetentionSettings().DkimRetiredKeepTimeout)
	for _, ref := range refs {
		err := deleteSmtpDkimSelectorRecordsInTx(ctx, tx, ref, smtpDkimActor{})
		switch {
		case err == nil:
			server.RaisePgResult(tx.Exec(
				ctx,
				`
				UPDATE smtp_dkim_key
				SET state = $3, retire_time = COALESCE(retire_time, $4), remove_time = $4
				WHERE selector = $1 AND domain = $2
				`,
				ref.selector,
				ref.domain,
				SmtpDkimKeyStateRemoved,
				removeTime.UTC(),
			))
			removedCount += 1
		case IsDnsZoneWaiting(err):
			server.RaisePgResult(tx.Exec(
				ctx,
				`UPDATE smtp_dkim_key SET state = $3, retire_time = $4 WHERE selector = $1 AND domain = $2`,
				ref.selector,
				ref.domain,
				SmtpDkimKeyStateRetired,
				expiryRetireTime,
			))
		default:
			return removedCount, err
		}
	}
	return removedCount, nil
}

// DeleteSmtpDkimKeysForClientInTx in a transaction of its own.
func DeleteSmtpDkimKeysForClient(ctx context.Context, networkId server.Id, sidecarId server.Id, removeTime time.Time) (removedCount int, returnErr error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() || !server.SidecarMg9Linked() {
		return 0, nil
	}
	refusal := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			var err error
			removedCount, err = DeleteSmtpDkimKeysForClientInTx(ctx, tx, networkId, sidecarId, removeTime)
			if err != nil {
				panic(dnsWriteRefusal{err: err})
			}
		}, server.TxReadCommitted)
	})
	if refusal != nil {
		return 0, refusal
	}
	return removedCount, nil
}

// GET /smtp/status's answer: what the sender's queue reads of its network's mail standing.
type SmtpStatusResult struct {
	// The network's tier, and whether its limits are enforced (§20.7).
	Tier     string `json:"tier"`
	Enforced bool   `json:"enforced"`
	// §20.4's smtp_* keys and §14.7's sidecar_abuse: ones, by key, as §20.7 gives them: -1 none, 0 not included.
	Limits map[string]int64 `json:"limits"`
	// Today's counts, the UTC day's, as the edges' {smq_} keeps them; nil when Redis did not answer.
	Today *SmtpStatusDay `json:"today,omitempty"`
	// The halts holding now, oldest first: the network's, or one burner label's.
	Halts []*SmtpStatusHalt `json:"halts"`
}

// One UTC day's counts.
type SmtpStatusDay struct {
	ConnectionCount int64 `json:"connection_count"`
	RecipientCount  int64 `json:"recipient_count"`
	ByteCount       int64 `json:"byte_count"`
}

// One halt as its network sees it (§14.8).
type SmtpStatusHalt struct {
	HaltId server.Id `json:"halt_id"`
	// A burner label, or empty for the whole network.
	Label   string `json:"label,omitempty"`
	Trigger string `json:"trigger"`
	// Unix milliseconds: the report's arrival, and 24 h after it.
	StartTime int64 `json:"start_time"`
	EndTime   int64 `json:"end_time"`
}

// The {smq_} day counts the edges' script keeps for networkId on day (quota_scripts.go's c:, r: and b: fields).
type SmtpQuotaDayReader func(ctx context.Context, networkId server.Id, day int64) (*SmtpStatusDay, error)

// {smq_<networkId>}'s day fields from this process's Redis.
func readSmtpQuotaDay(ctx context.Context, networkId server.Id, day int64) (today *SmtpStatusDay, returnErr error) {
	if r := server.HandleError(func() {
		server.Redis(ctx, func(client server.RedisClient) {
			values, err := client.HMGet(
				ctx,
				"{smq_"+networkId.String()+"}",
				fmt.Sprintf("c:%d", day),
				fmt.Sprintf("r:%d", day),
				fmt.Sprintf("b:%d", day),
			).Result()
			if err != nil {
				returnErr = err
				return
			}
			counts := [3]int64{}
			for i, value := range values {
				text, ok := value.(string)
				if !ok {
					continue
				}
				if count, err := strconv.ParseInt(text, 10, 64); err == nil && 0 <= count {
					counts[i] = count
				}
			}
			today = &SmtpStatusDay{ConnectionCount: counts[0], RecipientCount: counts[1], ByteCount: counts[2]}
		})
	}); r != nil {
		return nil, fmt.Errorf("smtp status redis: %v", r)
	}
	return
}

// GET /smtp/status for networkId at now: its limits, today's counts read through readDay (nil reads this
// process's Redis), and its halts holding now. Needs MG9.
func GetSmtpStatus(ctx context.Context, networkId server.Id, now time.Time, readDay SmtpQuotaDayReader) (*SmtpStatusResult, error) {
	if !server.SidecarMg9Linked() {
		return nil, fmt.Errorf("%w: the status needs MG9", ErrSmtpApiUnavailable)
	}
	limits := NetworkQuotaLimits(ctx, networkId)
	status := &SmtpStatusResult{
		Tier:     limits.Tier,
		Enforced: limits.Enforced,
		Limits:   map[string]int64{},
		Halts:    []*SmtpStatusHalt{},
	}
	for key, value := range limits.Limits {
		if strings.HasPrefix(key, "smtp_") || key == SmtpDkimSelectorsLimitKey {
			status.Limits[key] = value
		}
	}
	halts, err := FindSmtpHalts(ctx, networkId, now.Add(-smtpedgeservice.SmtpHaltDuration))
	if err != nil {
		return nil, err
	}
	for _, halt := range halts {
		if !halt.ActiveAt(now) {
			continue
		}
		status.Halts = append(status.Halts, &SmtpStatusHalt{
			HaltId:    server.Id(halt.HaltId),
			Label:     halt.Label,
			Trigger:   halt.Trigger,
			StartTime: halt.StartTime.UnixMilli(),
			EndTime:   halt.StartTime.Add(smtpedgeservice.SmtpHaltDuration).UnixMilli(),
		})
	}
	if readDay == nil {
		readDay = readSmtpQuotaDay
	}
	if today, err := readDay(ctx, networkId, smtpedgeservice.SmtpSendDay(now)); err == nil {
		status.Today = today
	}
	return status, nil
}

// One attributed report as GET /smtp/reports answers it: §2.8 smtp-5's fields and the sealed copy.
type SmtpReportsComplaint struct {
	ComplaintId server.Id `json:"complaint_id"`
	Reporter    string    `json:"reporter,omitempty"`
	Type        string    `json:"type"`
	// Unix milliseconds.
	ArrivalTime   int64  `json:"arrival_time"`
	Domain        string `json:"domain,omitempty"`
	DkimDomain    string `json:"dkim_domain,omitempty"`
	DkimSelector  string `json:"dkim_selector,omitempty"`
	DkimResult    string `json:"dkim_result,omitempty"`
	MessageIdHash []byte `json:"message_id_hash,omitempty"`
	XUrRef        string `json:"x_ur_ref,omitempty"`
	FeedbackId    string `json:"feedback_id,omitempty"`
	JoinResult    string `json:"join_result,omitempty"`
	// HPKE-XWing to the network mail key (§14.8), cleared at 30 days; empty when none was kept.
	SealedReport []byte `json:"sealed_report,omitempty"`
}

// One aggregate report's counts for one header_from of the network (smtp_dmarc_daily's row): a report's rows share
// its reporter and report id, which the reader groups by.
type SmtpReportsAggregate struct {
	Reporter string `json:"reporter"`
	ReportId string `json:"report_id"`
	Kind     string `json:"kind"`
	// Unix milliseconds: the UTC day it counts, and its arrival.
	Day         int64  `json:"day"`
	ArrivalTime int64  `json:"arrival_time"`
	Domain      string `json:"domain"`
	// TLS-RPT's policy type; empty for DMARC.
	PolicyType      string `json:"policy_type,omitempty"`
	MessageCount    int64  `json:"message_count"`
	DkimPassCount   int64  `json:"dkim_pass_count"`
	SpfPassCount    int64  `json:"spf_pass_count"`
	QuarantineCount int64  `json:"quarantine_count"`
	RejectCount     int64  `json:"reject_count"`
	SuccessCount    int64  `json:"success_count"`
	FailureCount    int64  `json:"failure_count"`
}

// GET /smtp/reports's answer.
type SmtpReportsResult struct {
	Complaints []*SmtpReportsComplaint `json:"complaints"`
	Aggregates []*SmtpReportsAggregate `json:"aggregates"`
	// Where the next page starts, passed back as ?cursor=; absent when both lists are done.
	Cursor string `json:"cursor,omitempty"`
}

// Where a page resumes, opaque to the caller: each list's last row returned, by its key, and whether the list is
// done. Every read filters by the caller's network, so a cursor reaches no other network's rows whatever it holds.
type smtpReportsCursor struct {
	ComplaintTimeUs int64     `json:"ct"`
	ComplaintId     server.Id `json:"ci"`
	ComplaintsDone  bool      `json:"cd,omitempty"`
	// smtp_dmarc_daily's primary key after its network: day, reporter, header_from, report_id, policy_type
	AggregateDayUs      int64  `json:"ad"`
	AggregateReporter   string `json:"ar"`
	AggregateHeaderFrom string `json:"ah"`
	AggregateReportId   string `json:"ai"`
	AggregatePolicyType string `json:"ap"`
	// the key names a row already returned, past which the page starts; false only before the first page
	AggregateAfter bool `json:"aa,omitempty"`
	AggregatesDone bool `json:"dd,omitempty"`
}

func encodeSmtpReportsCursor(cursor *smtpReportsCursor) string {
	value, err := json.Marshal(cursor)
	if err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(value)
}

func decodeSmtpReportsCursor(text string, settings *SmtpApiSettings) (*smtpReportsCursor, error) {
	value, err := base64.RawURLEncoding.DecodeString(text)
	if err != nil || settings.ReportsCursorMaxByteCount < len(value) {
		return nil, smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeCursor, "The cursor is not one this route gave.")
	}
	cursor := &smtpReportsCursor{}
	if err := json.Unmarshal(value, cursor); err != nil {
		return nil, smtpApiRefusal(http.StatusBadRequest, SmtpApiCodeCursor, "The cursor is not one this route gave.")
	}
	return cursor, nil
}

// One GET /smtp/reports page of networkId's reports: with no cursor, the complaints that arrived at or after since
// and the aggregate rows of the UTC days from since's on; with one, the rest from where the last page stopped. Each
// list is in its key's order, complaints by arrival then id and aggregate rows by smtp_dmarc_daily's primary key,
// each read on its index from the cursor's key, at most ReportsPageCount complaints, their sealed copies at most
// ReportsPageSealedByteCount together (the page's first always taken), and at most ReportsPageAggregateRowCount
// rows. Needs MG5.
func GetSmtpReportsPage(ctx context.Context, networkId server.Id, since time.Time, cursorText string, settings *SmtpApiSettings) (*SmtpReportsResult, error) {
	if !server.SidecarMg5Linked() {
		return nil, fmt.Errorf("%w: the reports need MG5", ErrSmtpApiUnavailable)
	}
	// the zero id orders before every complaint that arrived at since
	cursor := &smtpReportsCursor{
		ComplaintTimeUs: since.UTC().UnixMicro(),
		AggregateDayUs:  smtpUtcDay(since).UnixMicro(),
	}
	if cursorText != "" {
		decoded, err := decodeSmtpReportsCursor(cursorText, settings)
		if err != nil {
			return nil, err
		}
		cursor = decoded
	}
	page := &SmtpReportsResult{Complaints: []*SmtpReportsComplaint{}, Aggregates: []*SmtpReportsAggregate{}}
	next := *cursor
	server.Db(ctx, func(conn server.PgConn) {
		if !cursor.ComplaintsDone {
			result, err := conn.Query(
				ctx,
				`
				SELECT
					complaint_id, reporter, complaint_type, arrival_time, domain, dkim_domain, dkim_selector,
					dkim_result, message_id_hash, x_ur_ref, feedback_id, join_result, sealed_report
				FROM smtp_complaint
				WHERE network_id = $1 AND $2 <= arrival_time AND (arrival_time, complaint_id) > ($2, $3)
				ORDER BY arrival_time, complaint_id
				LIMIT $4
				`,
				networkId,
				time.UnixMicro(cursor.ComplaintTimeUs).UTC(),
				cursor.ComplaintId,
				settings.ReportsPageCount+1,
			)
			sealedByteCount := 0
			cut := false
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var reporter, domain, dkimDomain, dkimSelector, dkimResult, xUrRef, feedbackId, joinResult *string
					var arrivalTime time.Time
					complaint := &SmtpReportsComplaint{}
					server.Raise(result.Scan(
						&complaint.ComplaintId, &reporter, &complaint.Type, &arrivalTime, &domain, &dkimDomain, &dkimSelector,
						&dkimResult, &complaint.MessageIdHash, &xUrRef, &feedbackId, &joinResult, &complaint.SealedReport,
					))
					if cut {
						continue
					}
					if len(page.Complaints) == settings.ReportsPageCount ||
						(0 < len(page.Complaints) && settings.ReportsPageSealedByteCount < sealedByteCount+len(complaint.SealedReport)) {
						cut = true
						continue
					}
					sealedByteCount += len(complaint.SealedReport)
					complaint.ArrivalTime = arrivalTime.UnixMilli()
					complaint.Reporter = smtpTextOf(reporter)
					complaint.Domain = smtpTextOf(domain)
					complaint.DkimDomain = smtpTextOf(dkimDomain)
					complaint.DkimSelector = smtpTextOf(dkimSelector)
					complaint.DkimResult = smtpTextOf(dkimResult)
					complaint.XUrRef = smtpTextOf(xUrRef)
					complaint.FeedbackId = smtpTextOf(feedbackId)
					complaint.JoinResult = smtpTextOf(joinResult)
					page.Complaints = append(page.Complaints, complaint)
					next.ComplaintTimeUs = arrivalTime.UTC().UnixMicro()
					next.ComplaintId = complaint.ComplaintId
				}
			})
			next.ComplaintsDone = !cut
		}
		if !cursor.AggregatesDone {
			comparison := ">="
			if cursor.AggregateAfter {
				comparison = ">"
			}
			result, err := conn.Query(
				ctx,
				`
				SELECT
					day, reporter, header_from, report_id, policy_type, kind, arrival_time, message_count, dkim_pass_count,
					spf_pass_count, quarantine_count, reject_count, success_count, failure_count
				FROM smtp_dmarc_daily
				WHERE
					network_id = $1 AND $2 <= day AND
					(day, reporter, header_from, report_id, policy_type) `+comparison+` ($2, $3, $4, $5, $6)
				ORDER BY day, reporter, header_from, report_id, policy_type
				LIMIT $7
				`,
				networkId,
				time.UnixMicro(cursor.AggregateDayUs).UTC(),
				cursor.AggregateReporter,
				cursor.AggregateHeaderFrom,
				cursor.AggregateReportId,
				cursor.AggregatePolicyType,
				settings.ReportsPageAggregateRowCount+1,
			)
			cut := false
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var day, arrivalTime time.Time
					row := &SmtpReportsAggregate{}
					server.Raise(result.Scan(
						&day, &row.Reporter, &row.Domain, &row.ReportId, &row.PolicyType, &row.Kind, &arrivalTime, &row.MessageCount,
						&row.DkimPassCount, &row.SpfPassCount, &row.QuarantineCount, &row.RejectCount, &row.SuccessCount, &row.FailureCount,
					))
					if len(page.Aggregates) == settings.ReportsPageAggregateRowCount {
						cut = true
						continue
					}
					row.Day = day.UnixMilli()
					row.ArrivalTime = arrivalTime.UnixMilli()
					page.Aggregates = append(page.Aggregates, row)
					next.AggregateDayUs = day.UTC().UnixMicro()
					next.AggregateReporter = row.Reporter
					next.AggregateHeaderFrom = row.Domain
					next.AggregateReportId = row.ReportId
					next.AggregatePolicyType = row.PolicyType
					next.AggregateAfter = true
				}
			})
			next.AggregatesDone = !cut
		}
	})
	if !next.ComplaintsDone || !next.AggregatesDone {
		page.Cursor = encodeSmtpReportsCursor(&next)
	}
	return page, nil
}

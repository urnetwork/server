package model

// Removing the sign-up credentials from the network-create audit rows written
// before the audit stored an allowlist of the request
// (auditNetworkCreateRequest). Until then a row held the whole request, and so
// the password, the sign-in token, the balance code, and the wallet's signature
// and signed message of the sign-up. `bringyourctl db
// scrub-network-create-secrets` runs the census and the rewrite below. It runs
// after the allowlist is deployed: a release that still stores the whole
// request writes new rows with credentials, after the scope of the run.
//
// # What a run removes
//
// From the event details of each network_created row in its scope, the keys
// NetworkCreateAuditScrubKeyPaths: `password`, `auth_jwt` and `balance_code`
// under `network_create`, and `wallet_signature` and `wallet_message` under its
// `wallet_auth`, whatever their values. Every other key keeps its place and
// its value, byte for byte. A row the run cannot read as those json objects is
// counted as unreadable and left as it is; every writer of these rows wrote them
// with encoding/json, so a run expects none.
//
// # The census and the digest
//
// A census walks the scope, the event ids after `After` (the zero id precedes
// every event id) through `Through`, in ranges of at most BatchSize rows of
// any event type, each read by one short statement, with a pause after each
// range. A range reads only the network_created rows whose details contain one
// of the keys as text, and decodes them. The census counts the rows that hold
// each key and the rows to rewrite, and hashes the ids of the rows to rewrite
// in id order. The digest binds the scope, the key paths, those counts and that
// hash. A dry run prints it, and an apply runs the census again and refuses to
// write unless its digest is the one it was given. Retention removes rows older
// than AuditNetworkEventExpiration every hour, so a census taken across one of
// its runs can count fewer rows: the apply then refuses, and a new dry run
// gives the digest of the rows as they are.
//
// # The rewrite
//
// An apply rewrites the census's rows in batches of at most BatchSize, in id
// order. Each batch is one short read committed transaction, under the
// settings' lock and statement timeouts: it locks the batch's rows, reads their
// details again, removes the keys and writes the rows that still hold one,
// with one statement. A pause follows each batch. A failed batch (a lock or
// statement timeout) is tried again after the pause, up to MaxBatchAttempts
// attempts; then the run stops and reports the last event id it committed.
// Each batch commits on its own and a scrubbed row has nothing left to remove,
// so a stopped run resumes with a new dry run and apply, from the start or
// after the reported id.
//
// A run never prints, logs or returns event details, only ids and counts.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/server/v2026"
)

const networkCreateAuditScrubDigestVersion = "urnetwork scrub-network-create-secrets v1"

var ErrNetworkCreateAuditScrubDigestMismatch = errors.New("db scrub-network-create-secrets: --expect does not match the digest of the rows as they are now; nothing was changed; run the dry run again")

// The request keys that hold a credential, under `network_create`.
var networkCreateAuditScrubRequestKeys = []string{
	"password",
	"auth_jwt",
	"balance_code",
}

// The wallet keys that hold a credential, under `network_create.wallet_auth`.
var networkCreateAuditScrubWalletKeys = []string{
	"wallet_signature",
	"wallet_message",
}

// The paths of the keys a run removes, in the order a census reports them.
func NetworkCreateAuditScrubKeyPaths() []string {
	keyPaths := []string{}
	for _, key := range networkCreateAuditScrubRequestKeys {
		keyPaths = append(keyPaths, "network_create."+key)
	}
	for _, key := range networkCreateAuditScrubWalletKeys {
		keyPaths = append(keyPaths, "network_create.wallet_auth."+key)
	}
	return keyPaths
}

// Tunables of a run's census and rewrite.
type NetworkCreateAuditScrubSettings struct {
	// Rows of any event type a census range reads at most, and rows a rewrite
	// batch writes at most. A few thousand rows keep each statement and
	// transaction short on a busy primary. Must be positive.
	BatchSize int
	// The wait after each census range and each rewrite batch, so a run never
	// holds the primary's IO or WAL for long. Non-positive does not wait.
	BatchPause time.Duration
	// The bound on each statement (`statement_timeout`). Non-positive leaves
	// the session's.
	StatementTimeout time.Duration
	// The bound on each lock wait (`lock_timeout`), so a batch never waits
	// behind the retention's delete or any other writer for long.
	LockTimeout time.Duration
	// Attempts of one rewrite batch before the run stops. Less than one is
	// one.
	MaxBatchAttempts int
}

// Batches of 5,000 rows with a 2 s pause: under 2,500 rows a second, with
// each statement and transaction well under a second on the primary.
func DefaultNetworkCreateAuditScrubSettings() *NetworkCreateAuditScrubSettings {
	return &NetworkCreateAuditScrubSettings{
		BatchSize:        5000,
		BatchPause:       2 * time.Second,
		StatementTimeout: 30 * time.Second,
		LockTimeout:      2 * time.Second,
		MaxBatchAttempts: 5,
	}
}

// What a run would remove in its scope, and the digest that binds it.
type NetworkCreateAuditScrubCensus struct {
	// the scope: the event ids after After (nil: from the first) through Through
	After   *server.Id
	Through server.Id
	// network_created rows whose details contain a key as text: the rows the
	// census decoded
	CandidateCount int
	// rows holding each key, by key path
	KeyCounts map[string]int
	// the rows that hold at least one key, in id order
	EventIds []server.Id
	// candidate rows that are not the expected json objects, in id order; a
	// run leaves them as they are
	UnreadableEventIds []server.Id
	// binds the scope, the key paths, the counts and the ids of the rows to
	// rewrite
	Digest string
}

// Reported after each census range and each committed rewrite batch.
type NetworkCreateAuditScrubProgress struct {
	// "census" or "rewrite"
	Stage string
	// the last event id the census read through, or the last the rewrite
	// committed
	EventId server.Id
	// census: the candidate rows read so far; rewrite: the rows rewritten so
	// far, and the census's rows to rewrite
	Count int
	Total int
}

// What an apply did, as far as it went.
type NetworkCreateAuditScrubResult struct {
	Census *NetworkCreateAuditScrubCensus
	// rows the rewrite wrote; a row the census counted that has nothing left to
	// remove (or is gone) is not written
	RewrittenCount int
	// the last event id of the last batch the rewrite committed; nil before the
	// first
	LastEventId *server.Id
}

// The newest event id of the audit, the scope a dry run takes when it is not
// given one. False when the audit is empty.
func NewestAuditNetworkEventId(ctx context.Context) (eventId server.Id, found bool) {
	server.Tx(ctx, func(tx server.PgTx) {
		found = false
		result, err := tx.Query(
			ctx,
			`SELECT event_id FROM audit_network_event ORDER BY event_id DESC LIMIT 1`,
		)
		server.WithPgResult(result, err, func() {
			if result.Next() {
				server.Raise(result.Scan(&eventId))
				found = true
			}
		})
	}, server.TxReadCommitted)
	return
}

// Counts the rows in the scope that hold each key, and digests them. Reads
// only; see the file comment.
func CensusNetworkCreateAuditSecrets(
	ctx context.Context,
	after *server.Id,
	through server.Id,
	settings *NetworkCreateAuditScrubSettings,
	progress func(NetworkCreateAuditScrubProgress),
) (*NetworkCreateAuditScrubCensus, error) {
	if settings.BatchSize <= 0 {
		return nil, fmt.Errorf("db scrub-network-create-secrets: the batch size must be positive, not %d", settings.BatchSize)
	}
	census := &NetworkCreateAuditScrubCensus{
		After:     after,
		Through:   through,
		KeyCounts: map[string]int{},
	}
	for _, keyPath := range NetworkCreateAuditScrubKeyPaths() {
		census.KeyCounts[keyPath] = 0
	}
	eventIdsHash := sha256.New()

	// The rows that may hold a key: their details contain the quoted key.
	// Every writer of these rows wrote keys unescaped, so a row without the
	// text holds no key; one with it (in a value, say) is decoded and may
	// hold none.
	candidateConditions := []string{}
	for _, key := range slices.Concat(networkCreateAuditScrubRequestKeys, networkCreateAuditScrubWalletKeys) {
		candidateConditions = append(candidateConditions, `0 < strpos(event_details, '"`+key+`"')`)
	}
	candidateSql := strings.Join(candidateConditions, " OR ")

	// the zero id precedes every event id
	cursor := server.Id{}
	if after != nil {
		cursor = *after
	}
	for cursor.Less(through) {
		type candidate struct {
			eventId      server.Id
			eventDetails string
		}
		rangeEnd := through
		candidates := []candidate{}
		err := runNetworkCreateAuditScrubTx(ctx, settings, func(tx server.PgTx) {
			// a rerun starts over
			rangeEnd = through
			candidates = candidates[:0]
			result, err := tx.Query(
				ctx,
				`
					SELECT event_id FROM audit_network_event
					WHERE $1 < event_id AND event_id <= $2
					ORDER BY event_id
					OFFSET $3 LIMIT 1
				`,
				cursor,
				through,
				settings.BatchSize-1,
			)
			server.WithPgResult(result, err, func() {
				if result.Next() {
					server.Raise(result.Scan(&rangeEnd))
				}
			})
			result, err = tx.Query(
				ctx,
				`
					SELECT event_id, event_details FROM audit_network_event
					WHERE
						$1 < event_id AND event_id <= $2 AND
						event_type = $3 AND
						(`+candidateSql+`)
					ORDER BY event_id
				`,
				cursor,
				rangeEnd,
				AuditEventTypeNetworkCreated,
			)
			server.WithPgResult(result, err, func() {
				for result.Next() {
					var c candidate
					server.Raise(result.Scan(&c.eventId, &c.eventDetails))
					candidates = append(candidates, c)
				}
			})
		})
		if err != nil {
			return census, err
		}
		for _, c := range candidates {
			census.CandidateCount += 1
			_, removedKeyPaths, readable := scrubNetworkCreateAuditDetails(c.eventDetails)
			if !readable {
				census.UnreadableEventIds = append(census.UnreadableEventIds, c.eventId)
				continue
			}
			if len(removedKeyPaths) == 0 {
				continue
			}
			for _, keyPath := range removedKeyPaths {
				census.KeyCounts[keyPath] += 1
			}
			census.EventIds = append(census.EventIds, c.eventId)
			io.WriteString(eventIdsHash, c.eventId.String()+"\n")
		}
		cursor = rangeEnd
		if progress != nil {
			progress(NetworkCreateAuditScrubProgress{
				Stage:   "census",
				EventId: cursor,
				Count:   census.CandidateCount,
			})
		}
		if err := networkCreateAuditScrubPause(ctx, settings); err != nil {
			return census, err
		}
	}
	census.Digest = networkCreateAuditScrubDigest(census, eventIdsHash)
	return census, nil
}

// Takes the census again and, when its digest is expect, rewrites its rows.
// See the file comment.
func ApplyNetworkCreateAuditScrub(
	ctx context.Context,
	after *server.Id,
	through server.Id,
	expect string,
	settings *NetworkCreateAuditScrubSettings,
	progress func(NetworkCreateAuditScrubProgress),
) (*NetworkCreateAuditScrubResult, error) {
	census, err := CensusNetworkCreateAuditSecrets(ctx, after, through, settings, progress)
	result := &NetworkCreateAuditScrubResult{
		Census: census,
	}
	if err != nil {
		return result, err
	}
	if census.Digest != expect {
		return result, ErrNetworkCreateAuditScrubDigestMismatch
	}

	maxBatchAttempts := max(1, settings.MaxBatchAttempts)
	for start := 0; start < len(census.EventIds); start += settings.BatchSize {
		batchEventIds := census.EventIds[start:min(start+settings.BatchSize, len(census.EventIds))]
		rewrittenCount := 0
		for attempt := 1; ; attempt += 1 {
			err := runNetworkCreateAuditScrubTx(ctx, settings, func(tx server.PgTx) {
				// a rerun starts over
				rewrittenCount = 0
				eventIds := []server.Id{}
				eventDetailses := []string{}
				result, err := tx.Query(
					ctx,
					`
						SELECT event_id, event_details FROM audit_network_event
						WHERE event_id = ANY($1::uuid[]) AND event_type = $2
						ORDER BY event_id
						FOR UPDATE
					`,
					batchEventIds,
					AuditEventTypeNetworkCreated,
				)
				server.WithPgResult(result, err, func() {
					for result.Next() {
						var eventId server.Id
						var eventDetails *string
						server.Raise(result.Scan(&eventId, &eventDetails))
						if eventDetails == nil {
							continue
						}
						scrubbed, removedKeyPaths, readable := scrubNetworkCreateAuditDetails(*eventDetails)
						if !readable || len(removedKeyPaths) == 0 {
							continue
						}
						eventIds = append(eventIds, eventId)
						eventDetailses = append(eventDetailses, scrubbed)
					}
				})
				if len(eventIds) == 0 {
					return
				}
				tag, err := tx.Exec(
					ctx,
					`
						UPDATE audit_network_event
						SET event_details = scrubbed.event_details
						FROM unnest($1::uuid[], $2::text[]) AS scrubbed(event_id, event_details)
						WHERE audit_network_event.event_id = scrubbed.event_id
					`,
					eventIds,
					eventDetailses,
				)
				server.Raise(err)
				rewrittenCount = int(tag.RowsAffected())
			}, server.TxReadCommitted)
			if err == nil {
				break
			}
			if maxBatchAttempts <= attempt {
				return result, fmt.Errorf("db scrub-network-create-secrets: stopped after %d attempts of the batch after %s: %w", attempt, networkCreateAuditScrubEventIdString(result.LastEventId), err)
			}
			if err := networkCreateAuditScrubPause(ctx, settings); err != nil {
				return result, err
			}
		}
		result.RewrittenCount += rewrittenCount
		lastEventId := batchEventIds[len(batchEventIds)-1]
		result.LastEventId = &lastEventId
		if progress != nil {
			progress(NetworkCreateAuditScrubProgress{
				Stage:   "rewrite",
				EventId: lastEventId,
				Count:   result.RewrittenCount,
				Total:   len(census.EventIds),
			})
		}
		if err := networkCreateAuditScrubPause(ctx, settings); err != nil {
			return result, err
		}
	}
	return result, nil
}

// Runs callback in one transaction under the settings' bounds, and returns
// what it raised as an error. The error is the database's own text (a
// PgError's message and code, never its detail), so it holds no row value.
func runNetworkCreateAuditScrubTx(
	ctx context.Context,
	settings *NetworkCreateAuditScrubSettings,
	callback func(server.PgTx),
	options ...any,
) (returnErr error) {
	defer func() {
		if r := recover(); r != nil {
			if err, ok := r.(error); ok {
				returnErr = err
			} else {
				returnErr = fmt.Errorf("%v", r)
			}
		}
	}()
	options = append(options, server.TxLockTimeout(settings.LockTimeout))
	server.Tx(ctx, func(tx server.PgTx) {
		if 0 < settings.StatementTimeout {
			server.RaisePgResult(tx.Exec(ctx, fmt.Sprintf("SET LOCAL statement_timeout = %d", max(int64(1), settings.StatementTimeout.Milliseconds()))))
		}
		callback(tx)
	}, options...)
	return nil
}

// Waits the batch pause; an error when the context ends first.
func networkCreateAuditScrubPause(ctx context.Context, settings *NetworkCreateAuditScrubSettings) error {
	if settings.BatchPause <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(settings.BatchPause):
		return nil
	}
}

// Binds the scope, the key paths, the counts, the hash of the ids to rewrite
// and the unreadable ids.
func networkCreateAuditScrubDigest(census *NetworkCreateAuditScrubCensus, eventIdsHash hash.Hash) string {
	lines := []string{
		networkCreateAuditScrubDigestVersion,
		"keys " + strings.Join(NetworkCreateAuditScrubKeyPaths(), " "),
		"after " + networkCreateAuditScrubEventIdString(census.After),
		"through " + census.Through.String(),
	}
	for _, keyPath := range NetworkCreateAuditScrubKeyPaths() {
		lines = append(lines, "key "+keyPath+" "+strconv.Itoa(census.KeyCounts[keyPath]))
	}
	lines = append(lines, "rewrite "+strconv.Itoa(len(census.EventIds)))
	lines = append(lines, "rewrite ids "+hex.EncodeToString(eventIdsHash.Sum(nil)))
	unreadable := []string{}
	for _, eventId := range census.UnreadableEventIds {
		unreadable = append(unreadable, eventId.String())
	}
	lines = append(lines, "unreadable "+strconv.Itoa(len(unreadable))+" "+strings.Join(unreadable, " "))
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n") + "\n"))
	return hex.EncodeToString(sum[:])
}

// The id, or "start" for none.
func networkCreateAuditScrubEventIdString(eventId *server.Id) string {
	if eventId == nil {
		return "start"
	}
	return eventId.String()
}

// Removes the credential keys from one row's event details. Returns the
// details with the keys removed (the details themselves when none was), the
// paths of the keys removed, and false for details that are not the expected
// json objects, which it leaves as they are. Members keep their order and
// their values byte for byte.
func scrubNetworkCreateAuditDetails(eventDetails string) (scrubbed string, removedKeyPaths []string, readable bool) {
	details, ok := parseJsonObjectMembers([]byte(eventDetails))
	if !ok {
		return eventDetails, nil, false
	}
	removed := map[string]bool{}
	for i, detailsMember := range details {
		if detailsMember.key != "network_create" || string(detailsMember.value) == "null" {
			continue
		}
		networkCreate, ok := parseJsonObjectMembers(detailsMember.value)
		if !ok {
			return eventDetails, nil, false
		}
		networkCreateChanged := false
		keptMembers := []jsonObjectMember{}
		for _, member := range networkCreate {
			switch {
			case slices.Contains(networkCreateAuditScrubRequestKeys, member.key):
				removed["network_create."+member.key] = true
				networkCreateChanged = true
				continue
			case member.key == "wallet_auth" && string(member.value) != "null":
				walletAuth, ok := parseJsonObjectMembers(member.value)
				if !ok {
					return eventDetails, nil, false
				}
				keptWalletMembers := []jsonObjectMember{}
				for _, walletMember := range walletAuth {
					if slices.Contains(networkCreateAuditScrubWalletKeys, walletMember.key) {
						removed["network_create.wallet_auth."+walletMember.key] = true
						continue
					}
					keptWalletMembers = append(keptWalletMembers, walletMember)
				}
				if len(keptWalletMembers) < len(walletAuth) {
					member.value = appendJsonObjectMembers(nil, keptWalletMembers)
					networkCreateChanged = true
				}
			}
			keptMembers = append(keptMembers, member)
		}
		if networkCreateChanged {
			details[i].value = appendJsonObjectMembers(nil, keptMembers)
		}
	}
	if len(removed) == 0 {
		return eventDetails, nil, true
	}
	for _, keyPath := range NetworkCreateAuditScrubKeyPaths() {
		if removed[keyPath] {
			removedKeyPaths = append(removedKeyPaths, keyPath)
		}
	}
	return string(appendJsonObjectMembers(nil, details)), removedKeyPaths, true
}

// One member of a json object: its key, and its value as written.
type jsonObjectMember struct {
	key   string
	value json.RawMessage
}

// The members of a json object in their order, each value as written. False
// when the text is not exactly one json object.
func parseJsonObjectMembers(text []byte) (members []jsonObjectMember, ok bool) {
	decoder := json.NewDecoder(bytes.NewReader(text))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, false
	}
	members = []jsonObjectMember{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := token.(string)
		if !ok {
			return nil, false
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, false
		}
		members = append(members, jsonObjectMember{key: key, value: value})
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, false
	}
	// nothing follows the object
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	return members, true
}

// Appends the object of the members, each value as it is.
func appendJsonObjectMembers(out []byte, members []jsonObjectMember) []byte {
	out = append(out, '{')
	for i, member := range members {
		if 0 < i {
			out = append(out, ',')
		}
		var key bytes.Buffer
		encoder := json.NewEncoder(&key)
		encoder.SetEscapeHTML(false)
		// a string always encodes
		encoder.Encode(member.key)
		out = append(out, bytes.TrimSuffix(key.Bytes(), []byte("\n"))...)
		out = append(out, ':')
		out = append(out, member.value...)
	}
	return append(out, '}')
}

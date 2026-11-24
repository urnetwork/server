package model

// §21.9's preservation holds on api's side (ABUSE-M5-14; R-ABUSE-5, D-ABUSE-6, D-ABUSE-8, D-TRUST-7, FS-48): the
// abuse_legal_hold rows, the hold/<network_id>/<case_id>/_ objects api mirrors them to on the first metadata
// cluster (owner decision 145; sidecar/host_hold.go's codec, which every reader parses), each kind's statutory
// deadline, and HeldNetworkIds, the small set every server sweep reads before it deletes an in-scope row.
//
// A hold's part 0 is the hold itself (plan-ABUSE-hold-format #2): kind, scope, placement, the NCMEC report's time
// for report_2258a, the renewal and the expiry. Part 1 reserves these terms until the worker commits the
// placement manifest. The remaining parts carry the captured records (ABUSE-M5-15). The deadlines are statute, so constants, not settings:
//
//	preserve_2703f  90 days from placement (18 U.S.C. §2703(f)(1)), renewed once for 90 days more by a renewed
//	                request (§2703(f)(2)), so 180 days from placement at most; a second renewal is refused
//	report_2258a    1 year from the report to NCMEC (§2258A(h)(1)), which may precede placement; never renewed
//
// Placement writes the object before the row (PlaceLegalHold): a crash between the two leaves a hold that keeps
// data until its own expiry and that AbuseHoldPass alarms on, where the other order would leave a window in which
// evs deletes held bytes. place_time is chosen once: a retry that finds its own earlier object takes that object's
// placement. A renewal rewrites the object If-Match before the row, for the same reason.
//
// A hold keeps what exists at placement and collects nothing new. It never blocks --now, the 7-day timer or
// per-entry erasure, which run on evs's side and read nothing here. Nothing here writes a hint, an admin event, a
// notice or a status field, and no log line or metric names a network or case (owner decisions 93, 184; I-13):
// only counsel notifies.
//
// Placement and renewal refuse unless abuse.yml's holds_enabled reads true (§23.7, plan-ABUSE-small-readings #20),
// which stays false until counsel confirms owner decision 133's condition and each kind. Readers of hold/ need no
// gate: no object, no effect. Nothing here reads or writes the table unless the binary links MG5.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"

	"github.com/urnetwork/server"
)

// The statutes' periods (see the file header). Statutes, not settings: no deployment chooses another.
const (
	// 18 U.S.C. §2703(f)(1): a preservation request keeps records 90 days.
	LegalHoldPreserveDays = 90
	// §2703(f)(2): extended once for an additional 90 days upon a renewed request.
	LegalHoldPreserveRenewDays = 90
	// §2258A(h)(1): a provider preserves a report's contents for 1 year after it reaches NCMEC.
	LegalHoldReportYears = 1
)

// The part that is the hold itself; parts above it hold copied rows (plan-ABUSE-metadata-rows #3).
const legalHoldPartHold = 0

// A hold's copy of what drops or is overwritten on its own (ABUSE-M5-15), as part 0's copy_state holds it.
type LegalHoldCopyState string

const (
	// placed, its copy not yet complete: every range it may need stays (AbuseHoldCopyPending)
	LegalHoldCopyStatePending LegalHoldCopyState = "pending"
	// every registered table and the Redis soft state copied
	LegalHoldCopyStateCopied LegalHoldCopyState = "copied"
	// Older "copied" rows did not certify contracts or Redis. Only a new
	// placement with a complete snapshot may release a retention guard.
	legalHoldVerifiedCopy = server.LegalHoldVerifiedCopy
)

// scope's one string value (plan-ABUSE-hold-format #1), as the column and the object hold it.
const legalHoldScopeNetworkValue = "network"

var (
	// abuse.yml's holds_enabled is false, absent or unreadable (§23.7): nothing is placed or renewed.
	ErrLegalHoldsDisabled = errors.New("legal holds are not enabled")
	// Arguments no hold may hold. The message names no id (I-13).
	ErrLegalHoldInvalid = errors.New("legal hold invalid")
	// The network and case hold another hold: another kind, scope or report time, or an object other bytes.
	ErrLegalHoldConflict = errors.New("legal hold conflict")
	// No hold of the network has the case.
	ErrLegalHoldNotFound = errors.New("legal hold not found")
	// A report_2258a hold, or one renewed already: §2703(f)(2) renews once.
	ErrLegalHoldNotRenewable = errors.New("legal hold not renewable")
	// The hold is past its expiry, waiting for AbuseHoldPass to end it.
	ErrLegalHoldExpired = errors.New("legal hold expired")
	// This process holds no first metadata cluster for hold/, so nothing is placed or renewed: the object comes first.
	ErrLegalHoldStoreUnavailable = errors.New("legal hold store unavailable")
)

// Tunables of holds' placement, their hold/ writer and AbuseHoldPass.
type AbuseHoldSettings struct {
	// How often AbuseHoldPass runs: an hour (§21.9's pass). It ends holds past their expiry within this, and
	// re-creates an object a live row lacks within this, so a lost object is gone at most this long.
	PassInterval time.Duration
	// The most rows one read of the pass takes, by a cursor.
	PassBatchSize int
	// One pass's bound; a pass that runs out leaves the rest to the next.
	PassMaxTime time.Duration
	// The task runner's bound on one pass, past PassMaxTime, so a pass ends itself before the runner ends it.
	PassTaskMaxTime time.Duration
	// Interval at which the API checks the worker's placement receipt.
	PlacementPollInterval time.Duration
	// Keys one LIST page of hold/ asks for, while the pass looks for objects with no row. Must be positive.
	ListPageCount int
	// How long a record mail that failed waits before its task tries again: a minute, as the action pass retries
	// owed records.
	RecordMailRetryDelay time.Duration
	// The task runner's bound on one record mail.
	RecordMailTaskMaxTime time.Duration
	// How long after api saw a placement's or renewal's object acknowledged every deleter is bound by it: a deleter
	// reads hold/ through §11.13's cache, which answers at most this old (owner decision 629), so this is that cache's
	// bound, the readers' HoldSetSettings.RereadInterval. Placement and renewal report the time.
	BindTimeout time.Duration
	// How many hold/ writes whose outcome stayed unknown, the read after them failing too, the writer remembers by key:
	// each is spent as issued, once, and the write of its key that settles it spends only what its own cost row prices
	// past that (api_write_spend.go). Past it such a write is spent and not remembered, so the write that settles it
	// spends again, one write over. 1,024: each needs the store to fail both a write and the read after it.
	// Non-positive remembers none.
	UnsettledWriteCount int
}

func DefaultAbuseHoldSettings() *AbuseHoldSettings {
	return &AbuseHoldSettings{
		PassInterval:          time.Hour,
		PassBatchSize:         100,
		PassMaxTime:           5 * time.Minute,
		PassTaskMaxTime:       10 * time.Minute,
		PlacementPollInterval: time.Second,
		ListPageCount:         1000,
		RecordMailRetryDelay:  time.Minute,
		RecordMailTaskMaxTime: 5 * time.Minute,
		BindTimeout:           sidecar.DefaultHoldSetSettings().RereadInterval,
		UnsettledWriteCount:   1024,
	}
}

// One hold as part 0 holds it.
type LegalHold struct {
	NetworkId server.Id
	CaseId    server.Id
	Kind      sidecar.HoldKind
	Scope     sidecar.HoldScope
	// when the hold was placed, kept across a renewal: what existed then is what it keeps
	PlaceTime time.Time
	// report_2258a: when the report reached NCMEC, from which its year runs; nil for preserve_2703f
	ReportTime *time.Time
	// when the one renewal §2703(f)(2) allows was taken; nil before it
	RenewTime *time.Time
	// the kind's deadline, renewal included: past it the hold is no hold
	ExpireTime time.Time
	CopyState  LegalHoldCopyState
}

// Whether the hold stands at now: before its own expiry.
func (self *LegalHold) Live(now time.Time) bool {
	return now.Before(self.ExpireTime)
}

// The hold/ object's terms (sidecar.EncodeHoldRecord's input).
func (self *LegalHold) Record() *sidecar.HoldRecord {
	return &sidecar.HoldRecord{
		NetworkId:  sidecar.Id(self.NetworkId),
		CaseId:     sidecar.Id(self.CaseId),
		Kind:       self.Kind,
		Scope:      sidecar.HoldScopeRepos(self.Scope.RepoIds...),
		PlaceTime:  self.PlaceTime,
		ExpireTime: self.ExpireTime,
	}
}

// A hold to place.
type LegalHoldArgs struct {
	NetworkId server.Id
	// the abuse case the order or report is filed under, which keys the hold with the network
	CaseId server.Id
	Kind   sidecar.HoldKind
	// counsel's scope from the order, the network by default (plan-ABUSE-hold-format #1)
	Scope sidecar.HoldScope
	// report_2258a: when the report reached NCMEC, at or before now; nil for preserve_2703f
	ReportTime *time.Time
}

// The kind's deadline for a hold placed at placeTime (see the file header), before any renewal.
func LegalHoldDeadline(kind sidecar.HoldKind, placeTime time.Time, reportTime *time.Time) (time.Time, error) {
	switch kind {
	case sidecar.HoldKindPreserve2703f:
		return placeTime.UTC().AddDate(0, 0, LegalHoldPreserveDays), nil
	case sidecar.HoldKindReport2258a:
		if reportTime == nil {
			return time.Time{}, fmt.Errorf("%w: a §2258A hold names no report time", ErrLegalHoldInvalid)
		}
		return reportTime.UTC().AddDate(LegalHoldReportYears, 0, 0), nil
	}
	return time.Time{}, fmt.Errorf("%w: an unknown kind", ErrLegalHoldInvalid)
}

// The deadline after §2703(f)(2)'s one renewal: 90 days past the first 90, from placement.
func LegalHoldRenewedDeadline(placeTime time.Time) time.Time {
	return placeTime.UTC().AddDate(0, 0, LegalHoldPreserveDays+LegalHoldPreserveRenewDays)
}

// Times as the object holds them, to the millisecond, so the row and the object agree.
func legalHoldTime(t time.Time) time.Time {
	return t.UTC().Truncate(time.Millisecond)
}

// Refuses what no hold may hold, at now.
func (self *LegalHoldArgs) validate(now time.Time) error {
	if self.NetworkId == (server.Id{}) || self.CaseId == (server.Id{}) {
		return fmt.Errorf("%w: a zero network or case id", ErrLegalHoldInvalid)
	}
	switch self.Kind {
	case sidecar.HoldKindPreserve2703f:
		if self.ReportTime != nil {
			return fmt.Errorf("%w: a §2703(f) hold names a report time", ErrLegalHoldInvalid)
		}
	case sidecar.HoldKindReport2258a:
		if self.ReportTime == nil || self.ReportTime.IsZero() {
			return fmt.Errorf("%w: a §2258A hold names no report time", ErrLegalHoldInvalid)
		}
		if self.ReportTime.After(now) {
			return fmt.Errorf("%w: a report time after now", ErrLegalHoldInvalid)
		}
		if !now.Before(self.ReportTime.UTC().AddDate(LegalHoldReportYears, 0, 0)) {
			return fmt.Errorf("%w: a report whose year has passed", ErrLegalHoldInvalid)
		}
	default:
		return fmt.Errorf("%w: an unknown kind", ErrLegalHoldInvalid)
	}
	if sidecar.HoldScopeMaxRepoCount < len(self.Scope.RepoIds) {
		return fmt.Errorf("%w: a scope of %d repos", ErrLegalHoldInvalid, len(self.Scope.RepoIds))
	}
	for i, repoId := range self.Scope.RepoIds {
		if repoId == (sidecar.Id{}) {
			return fmt.Errorf("%w: a zero repo id in the scope", ErrLegalHoldInvalid)
		}
		if slices.Contains(self.Scope.RepoIds[:i], repoId) {
			return fmt.Errorf("%w: a repo named twice in the scope", ErrLegalHoldInvalid)
		}
	}
	return nil
}

// The scope column's JSON: "network", or the repo ids in the scope's order.
func legalHoldScopeJson(scope sidecar.HoldScope) string {
	if scope.Network() {
		return `"` + legalHoldScopeNetworkValue + `"`
	}
	repoTexts := []string{}
	for _, repoId := range scope.RepoIds {
		repoTexts = append(repoTexts, repoId.String())
	}
	encoded, err := json.Marshal(repoTexts)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// The scope a column holds. Anything that does not read as "network" or a list of repo ids reads as the network,
// the wider scope, since every doubt keeps data (plan-ABUSE-hold-format #1).
func parseLegalHoldScope(raw []byte) sidecar.HoldScope {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return sidecar.HoldScopeNetwork()
	}
	var repoTexts []string
	if err := json.Unmarshal(raw, &repoTexts); err != nil || len(repoTexts) == 0 {
		return sidecar.HoldScopeNetwork()
	}
	repoIds := []sidecar.Id{}
	for _, repoText := range repoTexts {
		repoId, err := sidecar.ParseId(repoText)
		if err != nil || repoId == (sidecar.Id{}) {
			return sidecar.HoldScopeNetwork()
		}
		repoIds = append(repoIds, repoId)
	}
	return sidecar.HoldScopeRepos(repoIds...)
}

// Whether two scopes name the same repos in the same order, or are both the network.
func legalHoldScopesEqual(a sidecar.HoldScope, b sidecar.HoldScope) bool {
	return slices.Equal(a.RepoIds, b.RepoIds)
}

// Whether two optional times are both nil or equal.
func legalHoldTimesEqual(a *time.Time, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Equal(*b)
}

// ----- the hold/ writer -----

// api's and taskworker's hold/ writer on the first metadata cluster: the hold/<network_id>/<case_id>/_ objects,
// written through sidecar.EncodeHoldRecord so a reader parses exactly what api writes. Every write is P0 of kind
// lifecycle (plan-EVSB-write-classes, QD-084) once the process installs its budget (SetWriteBudget): each PUT and
// DELETE carries P0, and each is spent once the store shows it took effect, at its own one of §11.13's cost rows
// (api_write_spend.go): a create, a rewrite's If-Match PUT as the overwrite it is, a missing object's put-back as a
// create, and a DELETE as a delete. A 412, a DELETE of a key already gone, which is never sent, and a write a read
// shows never landed spend nothing, and one whose outcome stays unknown is spent as issued, once. A renewal and the
// pass's put-back keep a hold's place_time, so after a create's unknown outcome a read that finds no object, or one
// of another placement, shows the create never landed; after a rewrite's or a DELETE's, the object it was
// conditioned on, or meant, standing shows it never landed. Safe for concurrent use.
type AbuseHoldStore struct {
	cluster sidecar.BlobCluster
	// the process's write budget's spend, P0 of kind lifecycle
	spend *apiWriteSpend
}

// A writer on the first metadata cluster, with the default settings.
func NewAbuseHoldStore(cluster sidecar.BlobCluster) *AbuseHoldStore {
	return NewAbuseHoldStoreWithSettings(cluster, DefaultAbuseHoldSettings())
}

// A writer on the first metadata cluster.
func NewAbuseHoldStoreWithSettings(cluster sidecar.BlobCluster, settings *AbuseHoldSettings) *AbuseHoldStore {
	return &AbuseHoldStore{
		cluster: cluster,
		spend:   newApiWriteSpend(cluster, sidecar.WriteP0KindLifecycle, settings.UnsettledWriteCount),
	}
}

// Spends every hold/ write that takes effect from budget, as P0 of kind lifecycle at its row of prices, §11.13's cost
// rows, and passes every PUT and DELETE through it as P0 (see the type's doc). nil prices is the table a budget built
// with evsservice's default settings prices with; nil budget removes it.
func (self *AbuseHoldStore) SetWriteBudget(budget sidecar.WriteBudget, prices *evsservice.PriceTable) {
	self.spend.setBudget(budget, prices)
}

// What shows a create of record never landed, after its outcome was unknown: no object, or one of another placement,
// since a renewal and the pass's put-back keep place_time.
func abuseHoldCreateNeverLanded(record *sidecar.HoldRecord) func(found bool, body []byte, etag string) bool {
	return func(found bool, body []byte, etag string) bool {
		if !found {
			return true
		}
		held, err := sidecar.ParseHoldRecord(body)
		return err == nil && held.PlaceTime.UnixMilli() != record.PlaceTime.UnixMilli()
	}
}

// What a read of a hold's key gave, as settleRead takes it.
func abuseHoldReadBody(object *AbuseHoldObject, found bool) (body []byte, etag string) {
	if !found {
		return nil, ""
	}
	return object.Body, object.Etag
}

// The object one hold/ key holds: its terms when it reads as a known hold of the key's network and case, else nil
// with found true, and its bytes and ETag.
type AbuseHoldObject struct {
	Record *sidecar.HoldRecord
	Body   []byte
	Etag   string
}

// Reads one hold's object. found is false for no object.
func (self *AbuseHoldStore) Read(ctx context.Context, networkId server.Id, caseId server.Id) (object *AbuseHoldObject, found bool, err error) {
	return self.readKey(ctx, sidecar.HoldObjectKey(sidecar.Id(networkId), sidecar.Id(caseId)), networkId, caseId)
}

func (self *AbuseHoldStore) readKey(ctx context.Context, key string, networkId server.Id, caseId server.Id) (*AbuseHoldObject, bool, error) {
	blob, err := self.cluster.Get(ctx, key)
	if errors.Is(err, sidecar.ErrBlobNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer blob.Body.Close()
	// one byte past the bound, so an oversized body shows as one
	body, err := io.ReadAll(io.LimitReader(blob.Body, sidecar.HoldRecordMaxByteCount+1))
	if err != nil {
		return nil, false, err
	}
	object := &AbuseHoldObject{Body: body, Etag: blob.Etag}
	if record, err := sidecar.ParseHoldRecord(body); err == nil &&
		record.NetworkId == sidecar.Id(networkId) && record.CaseId == sidecar.Id(caseId) {
		object.Record = record
	}
	return object, true, nil
}

// Creates record's object If-None-Match, or finds one there: the record the object holds is returned, so a retry
// takes its earlier attempt's placement. An object of other terms, or one that does not read as a hold of this
// network and case, is ErrLegalHoldConflict, and nothing is written. A create whose outcome was unknown reads its key:
// its own bytes there are the create landed, its answer lost. Any other error is an outcome the caller cannot know,
// which a retry reads again. The create is spent once the store shows it took effect (see the type's doc).
func (self *AbuseHoldStore) Create(ctx context.Context, record *sidecar.HoldRecord) (*sidecar.HoldRecord, error) {
	body, err := sidecar.EncodeHoldRecord(record)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrLegalHoldInvalid, err)
	}
	key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
	_, putErr := self.cluster.PutIfNoneMatch(self.spend.p0Context(ctx), key, body, nil)
	put := newApiWrite(evsservice.WriteOperationCreate, body, "", abuseHoldCreateNeverLanded(record))
	var unknown *sidecar.BlobUnknownError
	var unknownPut *apiWrite
	switch {
	case putErr == nil:
		self.spend.spendWritten(ctx, key, put)
		return record, nil
	case errors.As(putErr, &unknown):
		// it may have landed: the read decides
		unknownPut = put
	case !errors.Is(putErr, sidecar.ErrBlobPrecondition):
		return nil, putErr
	}
	// a 412 wrote nothing and spends nothing, an object standing; the read tells which
	object, found, err := self.readKey(ctx, key, server.Id(record.NetworkId), server.Id(record.CaseId))
	if err != nil {
		if unknownPut != nil {
			// no read told: spent as issued, once, for the write that settles it
			self.spend.spendUnsettled(ctx, key, unknownPut)
			return nil, putErr
		}
		return nil, err
	}
	heldBody, heldEtag := abuseHoldReadBody(object, found)
	if writeLanded, _ := self.spend.settleRead(ctx, key, unknownPut, found, heldBody, heldEtag); writeLanded {
		// this create landed and its answer was lost
		return record, nil
	}
	if unknownPut != nil {
		// another's object, or none: an outcome a retry reads again
		return nil, putErr
	}
	if !found {
		// deleted between the PUT and the read: a hold that ended, which this placement does not revive
		return nil, fmt.Errorf("%w: the object went between its create and its read", ErrLegalHoldConflict)
	}
	if bytes.Equal(object.Body, body) {
		return record, nil
	}
	stored := object.Record
	if stored == nil || stored.Kind != record.Kind || !slices.Equal(stored.Scope.RepoIds, record.Scope.RepoIds) {
		return nil, fmt.Errorf("%w: the object holds another hold", ErrLegalHoldConflict)
	}
	return stored, nil
}

// Rewrites record's object If-Match, or creates it when it is gone: a renewal, or the pass putting back what a
// live row says. Equal bytes write nothing. A write whose outcome was unknown reads its key: its own bytes there are
// the write landed, its answer lost, and nil; any other error, a 412's included, leaves the outcome to the caller's
// retry, which reads again. The write is spent once the store shows it took effect, an If-Match PUT at the overwrite
// row and a missing object's at the create row (see the type's doc).
func (self *AbuseHoldStore) Rewrite(ctx context.Context, record *sidecar.HoldRecord) error {
	body, err := sidecar.EncodeHoldRecord(record)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrLegalHoldInvalid, err)
	}
	key := sidecar.HoldObjectKey(record.NetworkId, record.CaseId)
	read := func() (*AbuseHoldObject, bool, []byte, string, error) {
		object, found, err := self.readKey(ctx, key, server.Id(record.NetworkId), server.Id(record.CaseId))
		if err != nil {
			return nil, false, nil, "", err
		}
		heldBody, heldEtag := abuseHoldReadBody(object, found)
		return object, found, heldBody, heldEtag, nil
	}
	object, found, heldBody, heldEtag, err := read()
	if err != nil {
		return err
	}
	// an earlier write of the key, spent with its outcome unknown, may settle here
	self.spend.settleRead(ctx, key, nil, found, heldBody, heldEtag)
	if found && bytes.Equal(object.Body, body) {
		return nil
	}
	var put *apiWrite
	if !found {
		_, err = self.cluster.PutIfNoneMatch(self.spend.p0Context(ctx), key, body, nil)
		put = newApiWrite(evsservice.WriteOperationCreate, body, "", abuseHoldCreateNeverLanded(record))
	} else {
		conditionEtag := object.Etag
		_, err = self.cluster.PutIfMatch(self.spend.p0Context(ctx), key, conditionEtag, body, nil)
		// the object it was conditioned on still stands: it replaced nothing
		put = newApiWrite(evsservice.WriteOperationOverwrite, body, "", func(found bool, body []byte, etag string) bool {
			return found && etag == conditionEtag
		})
	}
	var unknown *sidecar.BlobUnknownError
	switch {
	case err == nil:
		self.spend.spendWritten(ctx, key, put)
		return nil
	case errors.As(err, &unknown):
		// it may have landed: the read decides
		_, found, heldBody, heldEtag, readErr := read()
		if readErr != nil {
			// no read told: spent as issued, once, for the write that settles it
			self.spend.spendUnsettled(ctx, key, put)
			return err
		}
		if writeLanded, _ := self.spend.settleRead(ctx, key, put, found, heldBody, heldEtag); writeLanded {
			// landed, its answer lost
			return nil
		}
		return err
	default:
		// a 412 wrote nothing and spends nothing, as does a refusal
		return err
	}
}

// Deletes one hold's object; nil when it is gone already. The key is read first, so a key already gone, which the
// store's DELETE answers nil as it answers one that deleted, sends nothing and spends nothing; the DELETE is spent at
// §11.13's delete row once the store shows it took effect, and one whose outcome was unknown reads the key again: gone,
// it landed, its answer lost, and nil; the object it meant still standing, it never landed (see the type's doc).
func (self *AbuseHoldStore) Delete(ctx context.Context, networkId server.Id, caseId server.Id) error {
	key := sidecar.HoldObjectKey(sidecar.Id(networkId), sidecar.Id(caseId))
	read := func() (found bool, body []byte, etag string, err error) {
		object, found, err := self.readKey(ctx, key, networkId, caseId)
		if err != nil {
			return false, nil, "", err
		}
		body, etag = abuseHoldReadBody(object, found)
		return found, body, etag, nil
	}
	found, heldBody, heldEtag, err := read()
	if err != nil {
		return err
	}
	// an earlier write of the key, spent with its outcome unknown, may settle here
	self.spend.settleRead(ctx, key, nil, found, heldBody, heldEtag)
	if !found {
		return nil
	}
	return self.spend.deleteObject(ctx, key, heldEtag, read)
}

// hold/ keys after startAfter, at most limit, and whether more follow.
func (self *AbuseHoldStore) List(ctx context.Context, startAfter string, limit int) ([]*sidecar.BlobInfo, bool, error) {
	return self.cluster.List(ctx, abuseHoldKeyPrefix, startAfter, limit)
}

// Reads one listed key: the network and case it names, and its object. ok is false for a key that is not
// hold/<network_id>/<case_id>/_, whose object no reader can tie to a hold of its own.
func (self *AbuseHoldStore) ReadListed(ctx context.Context, key string) (networkId server.Id, caseId server.Id, object *AbuseHoldObject, found bool, ok bool, err error) {
	networkId, caseId, ok = ParseAbuseHoldKey(key)
	if !ok {
		return server.Id{}, server.Id{}, nil, false, false, nil
	}
	object, found, err = self.readKey(ctx, key, networkId, caseId)
	return networkId, caseId, object, found, true, err
}

// The object family's prefix (§11.8's keys row).
const abuseHoldKeyPrefix = "hold/"

// The network and case a hold key names: hold/<network_id>/<case_id>/_ exactly.
func ParseAbuseHoldKey(key string) (server.Id, server.Id, bool) {
	rest, found := bytes.CutPrefix([]byte(key), []byte(abuseHoldKeyPrefix))
	if !found {
		return server.Id{}, server.Id{}, false
	}
	parts := bytes.Split(rest, []byte("/"))
	if len(parts) != 3 || string(parts[2]) != "_" {
		return server.Id{}, server.Id{}, false
	}
	networkId, err := sidecar.ParseId(string(parts[0]))
	if err != nil || networkId == (sidecar.Id{}) {
		return server.Id{}, server.Id{}, false
	}
	caseId, err := sidecar.ParseId(string(parts[1]))
	if err != nil || caseId == (sidecar.Id{}) {
		return server.Id{}, server.Id{}, false
	}
	if sidecar.HoldObjectKey(networkId, caseId) != key {
		return server.Id{}, server.Id{}, false
	}
	return server.Id(networkId), server.Id(caseId), true
}

// This process's hold/ writer, which api and taskworker install once their first metadata cluster is wired (OPS's
// ServiceHost.Blobs). Until then placement and renewal refuse, since the object comes first, and AbuseHoldPass
// ends nothing, since it deletes the object before the row.
var abuseHoldStore atomic.Pointer[AbuseHoldStore]

// Installs this process's writer; nil removes it.
func SetAbuseHoldStore(store *AbuseHoldStore) {
	abuseHoldStore.Store(store)
}

// This process's writer, or ErrLegalHoldStoreUnavailable.
func CurrentAbuseHoldStore() (*AbuseHoldStore, error) {
	store := abuseHoldStore.Load()
	if store == nil {
		return nil, fmt.Errorf("%w: this process holds no first metadata cluster for hold/", ErrLegalHoldStoreUnavailable)
	}
	return store, nil
}

// ----- the rows -----

// Every column of part 0, in scanLegalHold's order.
const legalHoldColumns = `
	abuse_legal_hold.network_id,
	abuse_legal_hold.case_id,
	abuse_legal_hold.kind,
	abuse_legal_hold.scope,
	abuse_legal_hold.place_time,
	abuse_legal_hold.report_time,
	abuse_legal_hold.renew_time,
	abuse_legal_hold.expire_time,
	abuse_legal_hold.copy_state
`

// One part-0 row of legalHoldColumns. A part 0 missing its kind, scope or times reads as covering the network with
// no end, since a hold that cannot say what it spares spares nothing.
func scanLegalHold(result server.PgResult) *LegalHold {
	hold := &LegalHold{}
	var kind, copyState *string
	var scope []byte
	var placeTime, expireTime *time.Time
	server.Raise(result.Scan(
		&hold.NetworkId,
		&hold.CaseId,
		&kind,
		&scope,
		&placeTime,
		&hold.ReportTime,
		&hold.RenewTime,
		&expireTime,
		&copyState,
	))
	if kind != nil {
		hold.Kind = sidecar.HoldKind(*kind)
	}
	hold.Scope = parseLegalHoldScope(scope)
	if placeTime != nil {
		hold.PlaceTime = placeTime.UTC()
	}
	if expireTime != nil {
		hold.ExpireTime = expireTime.UTC()
	} else {
		hold.ExpireTime = legalHoldUnboundedTime
	}
	if hold.ReportTime != nil {
		reportTime := hold.ReportTime.UTC()
		hold.ReportTime = &reportTime
	}
	if hold.RenewTime != nil {
		renewTime := hold.RenewTime.UTC()
		hold.RenewTime = &renewTime
	}
	if copyState != nil {
		switch *copyState {
		case legalHoldVerifiedCopy:
			hold.CopyState = LegalHoldCopyStateCopied
		case string(LegalHoldCopyStateCopied), server.LegalHoldPlacementSnapshot:
			hold.CopyState = LegalHoldCopyStatePending
		default:
			hold.CopyState = LegalHoldCopyState(*copyState)
		}
	}
	return hold
}

// What a part 0 with no expiry reads as: a hold that never ends.
var legalHoldUnboundedTime = time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC)

// The network's hold of the case, read in the caller's transaction and locked to its end when forUpdate, or nil.
func getLegalHoldInTx(ctx context.Context, tx server.PgTx, networkId server.Id, caseId server.Id, forUpdate bool) *LegalHold {
	sql := `
		SELECT ` + legalHoldColumns + `
		FROM abuse_legal_hold
		WHERE
			abuse_legal_hold.network_id = $1 AND
			abuse_legal_hold.case_id = $2 AND
			abuse_legal_hold.part = 0
	`
	if forUpdate {
		sql += ` FOR UPDATE`
	}
	var hold *LegalHold
	result, err := tx.Query(ctx, sql, networkId, caseId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			hold = scanLegalHold(result)
		}
	})
	return hold
}

// The network's hold of the case, or nil, including nil in a binary that does not link MG5.
func GetLegalHold(ctx context.Context, networkId server.Id, caseId server.Id) (hold *LegalHold) {
	if !server.SidecarMg5Linked() {
		return nil
	}
	server.Tx(ctx, func(tx server.PgTx) {
		hold = getLegalHoldInTx(ctx, tx, networkId, caseId, false)
	})
	return
}

// The network's holds, live or awaiting their end, oldest placement first. For the abuse role and counsel, never
// for anything a network sees.
func GetLegalHolds(ctx context.Context, networkId server.Id) (holds []*LegalHold) {
	holds = []*LegalHold{}
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+legalHoldColumns+`
				FROM abuse_legal_hold
				WHERE abuse_legal_hold.network_id = $1 AND abuse_legal_hold.part = 0
				ORDER BY abuse_legal_hold.place_time, abuse_legal_hold.case_id
			`,
			networkId,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				holds = append(holds, scanLegalHold(result))
			}
		})
	})
	return
}

// The networks some hold covers at now: every network with a part 0 not past its expiry, or with none recorded.
// The small set every server sweep and drop of an in-scope row reads (§21.9: TTL sweeps skip a held network's
// in-scope rows). A hold covers a network's server rows whatever its scope (plan-ABUSE-hold-format #1). A read
// that fails raises, so a sweep that cannot tell deletes nothing. Empty in a binary that does not link MG5.
func HeldNetworkIds(ctx context.Context, now time.Time) (networkIds map[server.Id]bool) {
	networkIds = map[server.Id]bool{}
	if !server.SidecarMg5Linked() {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT DISTINCT abuse_legal_hold.network_id
				FROM abuse_legal_hold
				WHERE
					abuse_legal_hold.part = 0 AND
					(abuse_legal_hold.expire_time IS NULL OR $1 < abuse_legal_hold.expire_time)
			`,
			now.UTC(),
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				var networkId server.Id
				server.Raise(result.Scan(&networkId))
				networkIds[networkId] = true
			}
		})
	})
	return
}

// A hold's key in the table and in hold/.
type LegalHoldKey struct {
	NetworkId server.Id
	CaseId    server.Id
}

// The keys of every part 0 of the networks, live or not: what AbuseHoldPass's scan of hold/ compares objects with.
func LegalHoldKeysOf(ctx context.Context, networkIds []server.Id) (keys map[LegalHoldKey]bool) {
	keys = map[LegalHoldKey]bool{}
	if !server.SidecarMg5Linked() || len(networkIds) == 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT abuse_legal_hold.network_id, abuse_legal_hold.case_id
				FROM abuse_legal_hold
				WHERE abuse_legal_hold.part = 0 AND abuse_legal_hold.network_id = ANY($1)
			`,
			networkIds,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				key := LegalHoldKey{}
				server.Raise(result.Scan(&key.NetworkId, &key.CaseId))
				keys[key] = true
			}
		})
	})
	return
}

// Bars a case: a CSAM report or a sealing order (§21.8), so the network is told nothing of it, and no notice,
// admin event or hint follows any action under it. A case stays barred; nothing here unbars one.
func BarAbuseCase(ctx context.Context, caseId server.Id) (returnErr error) {
	if !server.SidecarMg5Linked() {
		return ErrAbuseUnlinked
	}
	server.Tx(ctx, func(tx server.PgTx) {
		tag := server.RaisePgResult(tx.Exec(
			ctx,
			`UPDATE abuse_case SET barred = true WHERE abuse_case.case_id = $1`,
			caseId,
		))
		if tag.RowsAffected() == 0 {
			returnErr = ErrAbuseCaseNotFound
		}
	})
	return
}

// Inserts part 0 unless the network and case have one, and reads back what is there. inserted is false when a row
// was there already.
func insertLegalHoldInTx(ctx context.Context, tx server.PgTx, hold *LegalHold) (stored *LegalHold, inserted bool) {
	tag := server.RaisePgResult(tx.Exec(
		ctx,
		`
			INSERT INTO abuse_legal_hold (
				network_id,
				case_id,
				part,
				kind,
				scope,
				place_time,
				report_time,
				renew_time,
				expire_time,
				copy_state
			) VALUES ($1, $2, 0, $3, $4::jsonb, $5, $6, NULL, $7, $8)
			ON CONFLICT (network_id, case_id, part) DO NOTHING
		`,
		hold.NetworkId,
		hold.CaseId,
		string(hold.Kind),
		legalHoldScopeJson(hold.Scope),
		hold.PlaceTime.UTC(),
		hold.ReportTime,
		hold.ExpireTime.UTC(),
		string(hold.CopyState),
	))
	return getLegalHoldInTx(ctx, tx, hold.NetworkId, hold.CaseId, false), tag.RowsAffected() == 1
}

// Whether a stored hold is the one args would place.
func (self *LegalHoldArgs) sameAs(hold *LegalHold) bool {
	return hold.Kind == self.Kind && legalHoldScopesEqual(hold.Scope, self.Scope) && legalHoldTimesEqual(hold.ReportTime, self.reportTime())
}

// The report time as the row keeps it, to the millisecond.
func (self *LegalHoldArgs) reportTime() *time.Time {
	if self.ReportTime == nil {
		return nil
	}
	reportTime := legalHoldTime(*self.ReportTime)
	return &reportTime
}

// Called around placement's and renewal's linearization points with the step's name: how a test holds a placement
// between its object and its row, or ends it there as a crash would. nil in production.
var Testing_LegalHoldStep func(step string)

// The steps Testing_LegalHoldStep sees.
const (
	LegalHoldStepObjectAfter   = "abuse.hold.object.after"
	LegalHoldStepRowAfter      = "abuse.hold.row.after"
	LegalHoldStepRenewObjAfter = "abuse.hold.renew.object.after"
)

func legalHoldStep(step string) {
	if hook := Testing_LegalHoldStep; hook != nil {
		hook(step)
	}
}

// Places a hold (see the file header): refuses unless holds_enabled, a case of the id and the network exist, and
// this process holds the hold/ writer; chooses place_time once, writes the object If-None-Match, then commits part
// 0 with copy_state pending, calling owe in that transaction, which is where the caller owes the legal-request
// mailbox its record. A hold already standing under the network and case is returned with inserted false when it
// is the one args would place, and refused with ErrLegalHoldConflict otherwise; one past its expiry, waiting for
// AbuseHoldPass, is ErrLegalHoldExpired.
func PlaceLegalHold(
	ctx context.Context,
	args *LegalHoldArgs,
	now time.Time,
	owe func(ctx context.Context, tx server.PgTx, hold *LegalHold),
) (hold *LegalHold, inserted bool, returnErr error) {
	return PlaceLegalHoldWithClock(ctx, args, func() time.Time { return now }, owe)
}

// The worker samples its clock at the PostgreSQL snapshot, between two
// matching reads of the scoped Redis families. A fixed clock supports
// deterministic placement and recovery.
func PlaceLegalHoldWithClock(ctx context.Context, args *LegalHoldArgs, clock func() time.Time, owe func(context.Context, server.PgTx, *LegalHold)) (hold *LegalHold, inserted bool, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, false, ErrAbuseUnlinked
	}
	if !server.GetAbuseConfig().HoldsEnabled() {
		return nil, false, ErrLegalHoldsDisabled
	}
	now := legalHoldTime(clock())
	if err := args.validate(now); err != nil {
		return nil, false, err
	}

	var existing *LegalHold
	var caseFound, networkFound bool
	server.Tx(ctx, func(tx server.PgTx) {
		existing = getLegalHoldInTx(ctx, tx, args.NetworkId, args.CaseId, false)
		caseFound = getAbuseCaseInTx(ctx, tx, args.CaseId, false) != nil
		networkFound = networkExistsInTx(ctx, tx, args.NetworkId)
	})
	if existing != nil {
		if !existing.Live(now) {
			return nil, false, ErrLegalHoldExpired
		}
		if !args.sameAs(existing) {
			return nil, false, ErrLegalHoldConflict
		}
		return existing, false, nil
	}
	if !caseFound {
		return nil, false, ErrAbuseCaseNotFound
	}
	if !networkFound {
		return nil, false, ErrNetworkNotFound
	}
	store, err := CurrentAbuseHoldStore()
	if err != nil {
		return nil, false, err
	}

	expireTime, err := LegalHoldDeadline(args.Kind, now, args.reportTime())
	if err != nil {
		return nil, false, err
	}
	placing := &LegalHold{
		NetworkId:  args.NetworkId,
		CaseId:     args.CaseId,
		Kind:       args.Kind,
		Scope:      sidecar.HoldScopeRepos(args.Scope.RepoIds...),
		PlaceTime:  now,
		ReportTime: args.reportTime(),
		ExpireTime: legalHoldTime(expireTime),
		CopyState:  LegalHoldCopyStatePending,
	}
	previous, found, err := store.Read(ctx, args.NetworkId, args.CaseId)
	if err != nil {
		return nil, false, err
	}
	if found {
		// Recover an orphaned mirror's original terms. An older placement
		// may have no durable snapshot; recreating it now would collect new
		// data.
		if previous.Record == nil || previous.Record.Kind != args.Kind ||
			!slices.Equal(previous.Record.Scope.RepoIds, placing.Scope.RepoIds) {
			return nil, false, ErrLegalHoldConflict
		}
		placing.PlaceTime = legalHoldTime(previous.Record.PlaceTime)
		expireTime, err := LegalHoldDeadline(args.Kind, placing.PlaceTime, args.reportTime())
		if err != nil || !previous.Record.ExpireTime.Equal(expireTime) {
			return nil, false, ErrLegalHoldConflict
		}
		placing.ExpireTime = legalHoldTime(expireTime)
		if !placing.Live(now) {
			return nil, false, ErrLegalHoldExpired
		}
	}
	snapshot, err := prepareLegalHoldSnapshot(ctx, placing, found, clock)
	if err != nil {
		return nil, false, err
	}
	if snapshot != nil {
		if !args.sameAs(snapshot) || (found && !placing.PlaceTime.Equal(snapshot.PlaceTime)) {
			return nil, false, ErrLegalHoldConflict
		}
		placing = snapshot
		placing.CopyState = LegalHoldCopyState(server.LegalHoldPlacementSnapshot)
	}
	if !placing.Live(legalHoldTime(clock())) {
		return nil, false, ErrLegalHoldExpired
	}
	stored, err := store.Create(ctx, placing.Record())
	if err != nil {
		return nil, false, err
	}
	if !stored.PlaceTime.Equal(placing.PlaceTime) {
		return nil, false, ErrLegalHoldConflict
	}
	legalHoldStep(LegalHoldStepObjectAfter)
	server.Tx(ctx, func(tx server.PgTx) {
		now = legalHoldTime(clock())
		if !placing.Live(now) {
			returnErr = ErrLegalHoldExpired
			return
		}
		hold, inserted = insertLegalHoldInTx(ctx, tx, placing)
		if !inserted {
			if !args.sameAs(hold) || !hold.Live(now) {
				returnErr = ErrLegalHoldConflict
			}
			return
		}
		if owe != nil {
			owe(ctx, tx, hold)
		}
	}, server.OptNoRetry())

	if returnErr != nil {
		return nil, false, returnErr
	}
	legalHoldStep(LegalHoldStepRowAfter)
	return hold, inserted, nil
}

// Renews a preserve_2703f hold once, by a renewed request (§2703(f)(2)): refuses unless holds_enabled and the
// writer is installed, a report_2258a hold and a second renewal with ErrLegalHoldNotRenewable, and a hold past its
// expiry with ErrLegalHoldExpired. Rewrites the object If-Match with the renewed expiry, keeping place_time, then
// commits renew_time and expire_time, calling owe in that transaction.
func RenewLegalHold(
	ctx context.Context,
	networkId server.Id,
	caseId server.Id,
	now time.Time,
	owe func(ctx context.Context, tx server.PgTx, hold *LegalHold),
) (hold *LegalHold, returnErr error) {
	if !server.SidecarMg5Linked() {
		return nil, ErrAbuseUnlinked
	}
	if !server.GetAbuseConfig().HoldsEnabled() {
		return nil, ErrLegalHoldsDisabled
	}
	now = legalHoldTime(now)
	existing := GetLegalHold(ctx, networkId, caseId)
	if existing == nil {
		return nil, ErrLegalHoldNotFound
	}
	if err := legalHoldRenewable(existing, now); err != nil {
		return nil, err
	}
	store, err := CurrentAbuseHoldStore()
	if err != nil {
		return nil, err
	}
	renewed := *existing
	renewed.RenewTime = &now
	renewed.ExpireTime = legalHoldTime(LegalHoldRenewedDeadline(existing.PlaceTime))
	if err := store.Rewrite(ctx, renewed.Record()); err != nil {
		return nil, err
	}
	legalHoldStep(LegalHoldStepRenewObjAfter)

	server.Tx(ctx, func(tx server.PgTx) {
		current := getLegalHoldInTx(ctx, tx, networkId, caseId, true)
		if current == nil {
			returnErr = ErrLegalHoldNotFound
			return
		}
		if err := legalHoldRenewable(current, now); err != nil {
			returnErr = err
			return
		}
		server.RaisePgResult(tx.Exec(
			ctx,
			`
				UPDATE abuse_legal_hold
				SET renew_time = $3, expire_time = $4
				WHERE
					abuse_legal_hold.network_id = $1 AND
					abuse_legal_hold.case_id = $2 AND
					abuse_legal_hold.part = 0
			`,
			networkId,
			caseId,
			now,
			renewed.ExpireTime,
		))
		hold = getLegalHoldInTx(ctx, tx, networkId, caseId, false)
		if owe != nil {
			owe(ctx, tx, hold)
		}
	})
	if returnErr != nil {
		return nil, returnErr
	}
	return hold, nil
}

// Whether the hold may take §2703(f)(2)'s renewal at now.
func legalHoldRenewable(hold *LegalHold, now time.Time) error {
	if hold.Kind != sidecar.HoldKindPreserve2703f {
		return fmt.Errorf("%w: a %s hold is not renewed", ErrLegalHoldNotRenewable, hold.Kind)
	}
	if hold.RenewTime != nil {
		return fmt.Errorf("%w: renewed once already", ErrLegalHoldNotRenewable)
	}
	if !hold.Live(now) {
		return ErrLegalHoldExpired
	}
	return nil
}

// Where a pass over holds stands: the last part 0 it took, in its read's order. The zero value starts at the first.
type LegalHoldCursor struct {
	ExpireTime time.Time
	NetworkId  server.Id
	CaseId     server.Id
}

// Up to limit holds past their expiry at now, oldest expiry first, after cursor, and the cursor after them. A part
// 0 with no expiry recorded never ends here.
func ListExpiredLegalHolds(ctx context.Context, now time.Time, cursor LegalHoldCursor, limit int) (holds []*LegalHold, next LegalHoldCursor) {
	holds = []*LegalHold{}
	next = cursor
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+legalHoldColumns+`
				FROM abuse_legal_hold
				WHERE
					abuse_legal_hold.part = 0 AND
					abuse_legal_hold.expire_time <= $1 AND
					(abuse_legal_hold.expire_time, abuse_legal_hold.network_id, abuse_legal_hold.case_id) > ($2, $3, $4)
				ORDER BY abuse_legal_hold.expire_time, abuse_legal_hold.network_id, abuse_legal_hold.case_id
				LIMIT $5
			`,
			now.UTC(),
			cursor.ExpireTime.UTC(),
			cursor.NetworkId,
			cursor.CaseId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				holds = append(holds, scanLegalHold(result))
			}
		})
	})
	if 0 < len(holds) {
		last := holds[len(holds)-1]
		next = LegalHoldCursor{ExpireTime: last.ExpireTime, NetworkId: last.NetworkId, CaseId: last.CaseId}
	}
	return
}

// Up to limit holds live at now, or with no expiry recorded, by network and case after cursor, and the cursor after
// them. ExpireTime in the cursor is unused.
func ListLiveLegalHolds(ctx context.Context, now time.Time, cursor LegalHoldCursor, limit int) (holds []*LegalHold, next LegalHoldCursor) {
	holds = []*LegalHold{}
	next = cursor
	if !server.SidecarMg5Linked() || limit <= 0 {
		return
	}
	server.Db(ctx, func(conn server.PgConn) {
		result, err := conn.Query(
			ctx,
			`
				SELECT `+legalHoldColumns+`
				FROM abuse_legal_hold
				WHERE
					abuse_legal_hold.part = 0 AND
					(abuse_legal_hold.expire_time IS NULL OR $1 < abuse_legal_hold.expire_time) AND
					(abuse_legal_hold.network_id, abuse_legal_hold.case_id) > ($2, $3)
				ORDER BY abuse_legal_hold.network_id, abuse_legal_hold.case_id
				LIMIT $4
			`,
			now.UTC(),
			cursor.NetworkId,
			cursor.CaseId,
			limit,
		)
		server.WithPgResult(result, err, func() {
			for result.Next() {
				holds = append(holds, scanLegalHold(result))
			}
		})
	})
	if 0 < len(holds) {
		last := holds[len(holds)-1]
		next = LegalHoldCursor{NetworkId: last.NetworkId, CaseId: last.CaseId}
	}
	return
}

// Ends a hold past its expiry in the caller's transaction: its part 0 and every part of its copied rows go. Its
// object must be gone first (AbuseHoldPass). ended is false when the hold is not there or not past its expiry at
// now, as when a renewal or a restore moved it, so a pass that read it stale deletes nothing.
func EndLegalHoldInTx(ctx context.Context, tx server.PgTx, networkId server.Id, caseId server.Id, now time.Time) (hold *LegalHold, ended bool) {
	hold = getLegalHoldInTx(ctx, tx, networkId, caseId, true)
	if hold == nil || hold.Live(now) {
		return hold, false
	}
	endLegalHoldCopiesInTx(ctx, tx, hold, now)
	server.RaisePgResult(tx.Exec(
		ctx,
		`
			DELETE FROM abuse_legal_hold
			WHERE abuse_legal_hold.network_id = $1 AND abuse_legal_hold.case_id = $2
		`,
		networkId,
		caseId,
	))
	return hold, true
}

// What ending a hold does to its copied rows before its parts go. ABUSE-M5-15 moves them to another hold that
// covers them; until then a hold has no parts above 0 and this does nothing.
var endLegalHoldCopiesInTx = func(ctx context.Context, tx server.PgTx, hold *LegalHold, now time.Time) {}

// ----- the gauge -----

// Holds by kind and state, as AbuseHoldPass last counted them: live rows, objects with no row (state unrecorded,
// which alarms: after a restore, a hold placed after the dump, §19.10), and objects no reader can read as a hold
// (state unreadable, which keeps its network whole until a person removes it). Counts only, never a network or a
// case (owner decisions 93, 184).
var abuseHoldsGauge = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Namespace: "urnetwork",
		Subsystem: "abuse",
		Name:      "holds",
		Help:      "Preservation holds by kind and state (live, unrecorded, unreadable), as the hold pass last counted them",
	},
	[]string{"kind", "state"},
)

// The gauge's states.
const (
	AbuseHoldStateLive       = "live"
	AbuseHoldStateUnrecorded = "unrecorded"
	AbuseHoldStateUnreadable = "unreadable"
)

// The gauge's kind for a hold whose terms are unknown.
const abuseHoldKindUnknown = "unknown"

func init() {
	prometheus.MustRegister(abuseHoldsGauge)
}

// A pass's counts, by kind and state.
type AbuseHoldCounts map[string]map[string]int

// Adds one to kind's state.
func (self AbuseHoldCounts) Add(kind sidecar.HoldKind, state string) {
	kindText := string(kind)
	if kindText == "" {
		kindText = abuseHoldKindUnknown
	}
	if self[kindText] == nil {
		self[kindText] = map[string]int{}
	}
	self[kindText][state] += 1
}

// Sets the gauge to counts, every known kind and state included, so a state that emptied reads zero.
func SetAbuseHoldsGauge(counts AbuseHoldCounts) {
	for _, kind := range []string{string(sidecar.HoldKindPreserve2703f), string(sidecar.HoldKindReport2258a), abuseHoldKindUnknown} {
		for _, state := range []string{AbuseHoldStateLive, AbuseHoldStateUnrecorded, AbuseHoldStateUnreadable} {
			abuseHoldsGauge.WithLabelValues(kind, state).Set(float64(counts[kind][state]))
		}
	}
}

// The gauge's series for kind and state, for tests to read.
func Testing_AbuseHoldsGauge(kind string, state string) prometheus.Gauge {
	return abuseHoldsGauge.WithLabelValues(kind, state)
}

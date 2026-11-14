package model

// §12.8's record API (DNS-M5-12; R-DNS-2, R-DNS-3, D-DNS-7, D-MCP-11, D-MCP-12, I-8, Q-DNS-6, owner decisions 70,
// 96, 97): one write, WriteDnsRecords, which api's POST /network/dns/records takes for a network credential and the
// dns block's POST /dns/records for a sidecar, and the zone's reads behind GET /network/dns and GET /dns/zone. The
// bodies are the sidecar root's (dns_records.go).
//
// A write is one read committed transaction, in §12.8's order:
//
//  1. under a decision, §16.6's gate (ConsumeApprovalUseInTx): a use committed under the request's effect key answers
//     with its stored bytes and no write, even past not_after or with a stale expect_version; else the spend check,
//     which needs the request's one RRset to equal the decision's CanonicalAction (plan reading 12), then the claim.
//     Steps 2 to 9 are the use's effect, whose answer the use's row stores. Under a standing grant the dns block has
//     already checked §8's admin on dns:<host> for every owner, and api's role gate §7.5's DNS permission;
//  2. the network's per-network lock (dns_records:<network_id>), which every user writer takes first, and its label,
//     allocated on first need;
//  3. the standing (§21.7, ServiceHost.Abuse's shape): suspended refuses, not loaded answers unavailable;
//  4. network_verified for an upsert creating a public name, an A, AAAA, CNAME, MX or SRV owner, else
//     QuotaRefusal{limit_key: verification} (owner decision 96);
//  5. probation, or restricted, which takes its limits: no A, AAAA or CNAME, and at most probation_dns_entries
//     user RRsets;
//  6. dns_entries under the lock (§20.4): one user RRset is one unit, and system, DKIM, challenge, alias and edge
//     records are free;
//  7. dns_changes_per_hour (§12.7's abuse cap), counted by the user writes' admin events in the window;
//  8. dnsservice.ValidateUserRecordSet on each upsert, then §21.6's ScreenName on each label of a new owner;
//  9. the compare-and-swap on the version the caller read, under the zone's lock, which step 2 took after the
//     label's, the zone's system set on first need (EnsureNetworkZoneInTx), then DnsWriteInTx with its admin event
//     (§7.11), all in this transaction.
//
// A refusal at any step rolls the transaction back, so it writes no journal row and, under a decision, takes no use.
// Every refusal is a *sidecar.DnsApiError, whose status api's router and the dns block answer alike.
//
// The standing and network_verified come from the AbuseGate the process installs (SetDnsRecordsAbuseGate), memory
// ABUSE-M5-07's model.NewAbuseGate fills; until a process installs one every write answers unavailable, the closed
// direction. The limits come from QUOTA's NetworkSidecarLimits through DnsLimits, which a test replaces.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/miekg/dns"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// The admin event a user write adds (§7.11, §12.8): the zone is its target, and nothing else is said.
const (
	AdminEventActionDnsRecords = "dns.records"
	AdminEventTargetDnsZone    = "dns_zone"
)

// The limit keys the record API reads (§20.4, §21.7), and the one §21.7's verification refusal names.
const (
	DnsEntriesLimitKey          = "dns_entries"
	DnsProbationEntriesLimitKey = "probation_dns_entries"
	DnsChangesPerHourLimitKey   = "dns_changes_per_hour"
	DnsVerificationLimitKey     = "verification"
)

// Tunables of the record API.
type DnsRecordsSettings struct {
	// The window dns_changes_per_hour counts a network's user writes over: an hour, as the key names.
	ChangesWindow time.Duration
	// The most RRsets one write names, upserts and deletes together. Far above what a zone's dns_entries let it
	// hold, so a bound on work and never a limit a caller meets.
	WriteMaxRecordSetCount int
	// The most values one upsert names. §12.7's own caps per type are lower; this bounds the parse.
	UpsertMaxValueCount int
}

func DefaultDnsRecordsSettings() *DnsRecordsSettings {
	return &DnsRecordsSettings{
		ChangesWindow:          time.Hour,
		WriteMaxRecordSetCount: 256,
		UpsertMaxValueCount:    64,
	}
}

// §20.5's limits as the record API reads them: the seam over QUOTA's NetworkSidecarLimits, which a test replaces.
// Safe for concurrent use.
type DnsLimits interface {
	// The network's effective limit for limitKey, read in tx, -1 for none; enforced false while sidecar limits are
	// dark (R-QUOTA-5), when nothing is refused.
	LimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string) (limit int64, enforced bool)
	// Whether a purchase or a higher tier lifts limitKey past usedCount: QuotaRefusal.Upgrade.
	UpgradeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string, usedCount int64) bool
}

// DnsLimits over QUOTA's model.
type dnsQuotaLimits struct{}

func (dnsQuotaLimits) LimitInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string) (int64, bool) {
	if !Pro().EnforceSidecarLimits {
		return proSidecarLimitNone, false
	}
	_, limits := networkSidecarLimitsWithTier(ctx, tx, networkId)
	return limits.Limit(limitKey), true
}

func (dnsQuotaLimits) UpgradeInTx(ctx context.Context, tx server.PgTx, networkId server.Id, limitKey string, usedCount int64) bool {
	tier, _ := networkSidecarLimitsWithTier(ctx, tx, networkId)
	return quotaRefusalUpgrade(tier, limitKey, usedCount, networkSidecarLimitHasRow(ctx, tx, networkId, limitKey))
}

// What the record API reads that a process installs: the abuse facts and the limits.
var dnsRecordsDeps = struct {
	stateLock sync.Mutex
	abuse     sidecar.AbuseGate
	limits    DnsLimits
}{
	limits: dnsQuotaLimits{},
}

func currentDnsRecordsDeps() (sidecar.AbuseGate, DnsLimits) {
	dnsRecordsDeps.stateLock.Lock()
	defer dnsRecordsDeps.stateLock.Unlock()
	abuse := dnsRecordsDeps.abuse
	if abuse == nil {
		abuse = sidecar.UnloadedAbuseGate{}
	}
	return abuse, dnsRecordsDeps.limits
}

// Installs where this process's record writes read §21.7's standing and network_verified: api's and the dns block's
// AbuseGate. nil removes it, and every write answers unavailable. Answers what puts the previous one back.
func SetDnsRecordsAbuseGate(abuse sidecar.AbuseGate) (restore func()) {
	dnsRecordsDeps.stateLock.Lock()
	defer dnsRecordsDeps.stateLock.Unlock()
	previous := dnsRecordsDeps.abuse
	dnsRecordsDeps.abuse = abuse
	return func() {
		dnsRecordsDeps.stateLock.Lock()
		defer dnsRecordsDeps.stateLock.Unlock()
		dnsRecordsDeps.abuse = previous
	}
}

// Reads limits in place of QUOTA's model, nil restoring it. Answers what puts the previous one back.
func Testing_SetDnsLimits(limits DnsLimits) (restore func()) {
	if limits == nil {
		limits = dnsQuotaLimits{}
	}
	dnsRecordsDeps.stateLock.Lock()
	defer dnsRecordsDeps.stateLock.Unlock()
	previous := dnsRecordsDeps.limits
	dnsRecordsDeps.limits = limits
	return func() {
		dnsRecordsDeps.stateLock.Lock()
		defer dnsRecordsDeps.stateLock.Unlock()
		dnsRecordsDeps.limits = previous
	}
}

// The process's tunables, which a test replaces.
var dnsRecordsSettings atomic.Pointer[DnsRecordsSettings]

func currentDnsRecordsSettings() *DnsRecordsSettings {
	if settings := dnsRecordsSettings.Load(); settings != nil {
		return settings
	}
	return DefaultDnsRecordsSettings()
}

// Replaces the record API's tunables, nil restoring the defaults. Answers what puts the previous ones back.
func Testing_SetDnsRecordsSettings(settings *DnsRecordsSettings) (restore func()) {
	previous := dnsRecordsSettings.Swap(settings)
	return func() {
		dnsRecordsSettings.Store(previous)
	}
}

// The record write's barriers (§22.8), which a T2 or T3 schedule holds: before the per-network lock and once it
// is held. Called with the network; nil in production.
var Testing_DnsRecordsBarrier func(name string, networkId server.Id)

// The barrier names.
const (
	DnsRecordsBarrierBefore = "model.dns_records.before"
	DnsRecordsBarrierLocked = "model.dns_records.locked"
)

func dnsRecordsBarrier(name string, networkId server.Id) {
	if barrier := Testing_DnsRecordsBarrier; barrier != nil {
		barrier(name, networkId)
	}
}

// Who writes: a network credential on api, or a sidecar on the dns block.
type DnsRecordsCaller struct {
	NetworkId server.Id
	// The sidecar whose service token the dns block took, which a decision's spend check names as its spender; nil
	// for api's network credential, whose route's role gate has checked §7.5's DNS permission.
	SidecarId *server.Id
	// The admin event's actor (§7.11).
	ActorKind string
	ActorId   *server.Id
	RequestId *server.Id
}

// One write.
type DnsRecordsWrite struct {
	// The root, canonical.
	Root    string
	Caller  *DnsRecordsCaller
	Request *sidecar.DnsRecordsRequest
	// The Idempotency-Key's bytes, set with Request.Approval.
	EffectKey []byte
	// The server's time, which the spend check and the hourly cap read.
	Now time.Time
}

// A refusal of the record routes.
func dnsRecordsError(status int, code string, format string, args ...any) *sidecar.DnsApiError {
	return &sidecar.DnsApiError{Status: status, Code: code, Message: fmt.Sprintf(format, args...)}
}

func dnsRecordsInvalid(format string, args ...any) *sidecar.DnsApiError {
	return dnsRecordsError(http.StatusBadRequest, sidecar.DnsApiCodeInvalid, format, args...)
}

func dnsRecordsUnavailable(format string, args ...any) *sidecar.DnsApiError {
	return dnsRecordsError(http.StatusServiceUnavailable, sidecar.DnsApiCodeUnavailable, format, args...)
}

func dnsRecordsRefused(format string, args ...any) *sidecar.DnsApiError {
	return dnsRecordsError(http.StatusForbidden, sidecar.DnsApiCodeRefused, format, args...)
}

// err as the record routes answer it: the write path's refusals mapped to their status, an approval_use refusal to
// its own, and anything else as it is, a failure.
func dnsRecordsApiError(err error) error {
	var apiError *sidecar.DnsApiError
	var approvalError *ApprovalUseError
	var quotaError *QuotaRefusalError
	switch {
	case err == nil:
		return nil
	case errors.As(err, &apiError):
		return apiError
	case errors.As(err, &quotaError):
		return sidecar.NewDnsQuotaError(&quotaError.Refusal)
	case errors.As(err, &approvalError):
		code := approvalError.Code
		switch code {
		case sidecar.DnsApiCodeInvalid, sidecar.DnsApiCodeRefused, sidecar.DnsApiCodeSpent, sidecar.DnsApiCodeUnavailable:
		default:
			code = sidecar.DnsApiCodeRefused
		}
		return &sidecar.DnsApiError{Status: approvalError.Status, Code: code, Message: approvalError.Err.Error()}
	case errors.Is(err, dnsservice.ErrDnsRestoring), errors.Is(err, ErrDnsUnlinked), errors.Is(err, ErrDnsLabelUnlinked):
		return dnsRecordsUnavailable("the zone takes no write now")
	case errors.Is(err, ErrDnsZoneSuspended), errors.Is(err, ErrDnsHostSuspended):
		return dnsRecordsError(http.StatusForbidden, sidecar.DnsApiCodeSuspended, "%v", err)
	case errors.Is(err, dnsservice.ErrDnsVersionConflict):
		return dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeVersionConflict, "the zone moved since that version")
	case errors.Is(err, dnsservice.ErrDnsZoneLint), errors.Is(err, dnsservice.ErrDnsUserRecordRefused),
		errors.Is(err, dnsservice.ErrDnsDmarcRefused), errors.Is(err, dnsservice.ErrDnsRowMalformed),
		errors.Is(err, ErrDnsOwnerOutsideZone), errors.Is(err, ErrDnsZoneUnlabeled):
		return dnsRecordsInvalid("%v", err)
	}
	return err
}

// A request's RRsets as the write path holds them.
type dnsRecordsParsed struct {
	upserts []dnsservice.DnsRecordSet
	deletes []dnsservice.DnsRecordKey
	// under a decision: what the gate's own request shows of the one RRset (§16.6)
	fields *sidecar.SpendGateFields
}

// The rdata of record as it is kept: miekg/dns's presentation form, with no owner, TTL, class or type.
func dnsRecordRdata(record dns.RR) string {
	return strings.TrimPrefix(record.String(), record.Header().String())
}

// The request's shape, with no I/O: canonical names at or below root, known types, each key once, values that parse,
// and under a decision the one RRset with one value or the one delete reading 12 allows. userTtl is dns.yml's
// ttl.user, which an upsert naming none takes.
func parseDnsRecordsRequest(root string, request *sidecar.DnsRecordsRequest, userTtl uint32, settings *DnsRecordsSettings) (*dnsRecordsParsed, error) {
	if request == nil {
		return nil, dnsRecordsInvalid("no body")
	}
	if settings.WriteMaxRecordSetCount < len(request.Upserts)+len(request.Deletes) {
		return nil, dnsRecordsInvalid("more than %d RRsets in one write", settings.WriteMaxRecordSetCount)
	}
	if len(request.Upserts) == 0 && len(request.Deletes) == 0 {
		return nil, dnsRecordsInvalid("a write of nothing")
	}
	name := func(text string) (string, error) {
		if _, ok := dns.IsDomainName(text); !ok || !dns.IsFqdn(text) || text != dns.CanonicalName(text) || strings.IndexByte(text, '\\') != -1 {
			return "", dnsRecordsInvalid("%q is not a canonical name with its final dot", text)
		}
		if text == root || !dns.IsSubDomain(root, text) {
			return "", dnsRecordsInvalid("%q is not in the root", text)
		}
		return text, nil
	}
	recordType := func(text string) (uint16, error) {
		rrType, ok := dns.StringToType[text]
		if !ok || rrType == dns.TypeNone || rrType == dns.TypeReserved {
			return 0, dnsRecordsInvalid("%q is not a type's mnemonic", text)
		}
		return rrType, nil
	}
	parsed := &dnsRecordsParsed{}
	keys := map[dnsservice.DnsRecordKey]bool{}
	for _, upsert := range request.Upserts {
		owner, err := name(upsert.Name)
		if err != nil {
			return nil, err
		}
		rrType, err := recordType(upsert.Type)
		if err != nil {
			return nil, err
		}
		if len(upsert.Values) == 0 {
			return nil, dnsRecordsInvalid("an upsert of %s %s with no value; a delete removes an RRset", owner, upsert.Type)
		}
		if settings.UpsertMaxValueCount < len(upsert.Values) {
			return nil, dnsRecordsInvalid("more than %d values in one RRset", settings.UpsertMaxValueCount)
		}
		ttl := upsert.Ttl
		if ttl == 0 {
			ttl = userTtl
		}
		set := dnsservice.DnsRecordSet{Owner: owner, Type: rrType, Ttl: ttl, Kind: dnsservice.DnsRecordKindUser, Rdata: slices.Clone(upsert.Values)}
		records, err := set.Records()
		if err != nil {
			return nil, dnsRecordsInvalid("%v", err)
		}
		set.Rdata = set.Rdata[:0]
		for _, record := range records {
			rdata := dnsRecordRdata(record)
			if slices.Contains(set.Rdata, rdata) {
				return nil, dnsRecordsInvalid("one value twice in %s %s", owner, upsert.Type)
			}
			set.Rdata = append(set.Rdata, rdata)
		}
		// an RRset is a set: kept in one order, so two writes of it compare equal
		slices.Sort(set.Rdata)
		if keys[set.Key()] {
			return nil, dnsRecordsInvalid("%s %s twice in one write", owner, upsert.Type)
		}
		keys[set.Key()] = true
		parsed.upserts = append(parsed.upserts, set)
	}
	for _, recordDelete := range request.Deletes {
		owner, err := name(recordDelete.Name)
		if err != nil {
			return nil, err
		}
		rrType, err := recordType(recordDelete.Type)
		if err != nil {
			return nil, err
		}
		key := dnsservice.DnsRecordKey{Owner: owner, Type: rrType}
		if keys[key] {
			return nil, dnsRecordsInvalid("%s %s twice in one write", owner, recordDelete.Type)
		}
		keys[key] = true
		parsed.deletes = append(parsed.deletes, key)
	}
	if request.Approval != nil {
		// reading 12: one decision, one RRset, one value
		switch {
		case len(request.Upserts) == 1 && len(request.Deletes) == 0 && len(request.Upserts[0].Values) == 1:
			upsert := request.Upserts[0]
			parsed.fields = &sidecar.SpendGateFields{
				Kind:     sidecar.SidecarActionDnsSet,
				DnsName:  upsert.Name,
				DnsType:  upsert.Type,
				DnsValue: upsert.Values[0],
				DnsTtl:   upsert.Ttl,
			}
		case len(request.Upserts) == 0 && len(request.Deletes) == 1:
			recordDelete := request.Deletes[0]
			parsed.fields = &sidecar.SpendGateFields{
				Kind:    sidecar.SidecarActionDnsDelete,
				DnsName: recordDelete.Name,
				DnsType: recordDelete.Type,
			}
		default:
			return nil, dnsRecordsInvalid("a write under a decision names one RRset with one value, or one delete")
		}
	}
	return parsed, nil
}

// §12.8's write (see the file header): the answer's body, which a use's row stores for a retry under a decision, or
// a *sidecar.DnsApiError. A binary that does not link MG2 and MG5 answers unavailable.
func WriteDnsRecords(ctx context.Context, write *DnsRecordsWrite) ([]byte, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, dnsRecordsUnavailable("the zone tables are not linked")
	}
	if write == nil || write.Caller == nil || write.Caller.NetworkId == (server.Id{}) {
		return nil, dnsRecordsInvalid("no caller")
	}
	root := dnsLabelRoot(write.Root)
	settings := currentDnsRecordsSettings()
	ttl := dnsZoneTtl()
	parsed, err := parseDnsRecordsRequest(root, write.Request, ttl.User, settings)
	if err != nil {
		return nil, err
	}
	approval := write.Request.Approval
	if approval != nil && (write.Caller.SidecarId == nil || len(write.EffectKey) == 0) {
		return nil, dnsRecordsInvalid("a decision needs a sidecar's token and an Idempotency-Key")
	}
	now := write.Now
	if now.IsZero() {
		now = server.NowUtc()
	}
	var body []byte
	var returnErr error
	err = dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			apply := func(ctx context.Context, tx server.PgTx) ([]byte, error) {
				return applyDnsRecordsInTx(ctx, tx, root, write, parsed, ttl, settings, now)
			}
			if approval == nil {
				body, returnErr = apply(ctx, tx)
			} else {
				var outcome *ApprovalUseOutcome
				outcome, returnErr = ConsumeApprovalUseInTx(ctx, tx, &ApprovalUseRequest{
					NetworkId:        write.Caller.NetworkId,
					SpenderSidecarId: *write.Caller.SidecarId,
					Decisions:        approval.Decisions,
					Action:           approval.Action,
					Template:         approval.Template,
					EffectKey:        write.EffectKey,
					Fields:           parsed.fields,
				}, now, func(ctx context.Context, tx server.PgTx, useIndex int) ([]byte, error) {
					return apply(ctx, tx)
				})
				if returnErr == nil {
					body = outcome.Result
				}
			}
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return body, nil
}

// One user row the zone holds, or a row of another kind at a key a write names.
type dnsRecordsHeld struct {
	set      dnsservice.DnsRecordSet
	withheld bool
}

// Steps 2 to 9 of the file header in tx: the answer's body.
func applyDnsRecordsInTx(
	ctx context.Context,
	tx server.PgTx,
	root string,
	write *DnsRecordsWrite,
	parsed *dnsRecordsParsed,
	ttl *dnsservice.DnsTtlSettings,
	settings *DnsRecordsSettings,
	now time.Time,
) ([]byte, error) {
	networkId := write.Caller.NetworkId
	abuse, limits := currentDnsRecordsDeps()

	// 2: the per-network lock, first of every lock a user write takes, then the label
	dnsRecordsBarrier(DnsRecordsBarrierBefore, networkId)
	server.RaisePgResult(tx.Exec(
		ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('dns_records:' || $1::uuid::text, 0))`,
		networkId,
	))
	dnsRecordsBarrier(DnsRecordsBarrierLocked, networkId)
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	// the zone's lock after the label's, as every zone writer takes them, before the zone's rows are read, so no
	// system writer changes a key this write judges between its read and its commit
	lockDnsZoneInTx(ctx, tx, root, networkId)
	apex := label + "." + root
	for _, key := range dnsWriteKeys(parsed.upserts, parsed.deletes) {
		if !dns.IsSubDomain(apex, key.Owner) {
			return nil, dnsRecordsInvalid("%s is outside the zone %s", key.Owner, apex)
		}
	}

	// 3: the standing
	standing, ok := abuse.Standing(connect.Id(networkId))
	switch {
	case !ok || !standing.Known():
		return nil, dnsRecordsUnavailable("the network's standing is not loaded")
	case standing == sidecar.AbuseStandingSuspended:
		return nil, dnsRecordsRefused("the network is suspended (§21.7)")
	}
	probation := standing == sidecar.AbuseStandingProbation || standing == sidecar.AbuseStandingRestricted

	// 4: network_verified for a public name
	publicName := slices.ContainsFunc(parsed.upserts, func(set dnsservice.DnsRecordSet) bool {
		switch set.Type {
		case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME, dns.TypeMX, dns.TypeSRV:
			return true
		}
		return false
	})
	if publicName {
		verified, ok := abuse.Verified(connect.Id(networkId))
		if !ok {
			return nil, dnsRecordsUnavailable("the network's verification is not loaded")
		}
		if !verified {
			// a payment method verifies, so a purchase lifts it
			return nil, sidecar.NewDnsQuotaError(&sidecar.QuotaRefusal{LimitKey: DnsVerificationLimitKey, Upgrade: true})
		}
	}

	// 5: probation's types
	if probation {
		for _, set := range parsed.upserts {
			switch set.Type {
			case dns.TypeA, dns.TypeAAAA, dns.TypeCNAME:
				return nil, dnsRecordsRefused("a network on probation publishes no A, AAAA or CNAME (§21.7)")
			}
		}
	}

	// 6: dns_entries, and probation's own count, under the lock
	held := map[dnsservice.DnsRecordKey]*dnsRecordsHeld{}
	heldOwners := map[string]bool{}
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		held[row.set.Key()] = &dnsRecordsHeld{set: row.set, withheld: row.withheld}
		heldOwners[row.set.Owner] = true
	}
	addCount := int64(0)
	for _, set := range parsed.upserts {
		row, ok := held[set.Key()]
		switch {
		case !ok:
			addCount += 1
		case row.set.Kind != dnsservice.DnsRecordKindUser:
			return nil, dnsRecordsInvalid("%s %s is the system's", set.Owner, dns.TypeToString[set.Type])
		}
	}
	for _, key := range parsed.deletes {
		if row, ok := held[key]; ok {
			if row.set.Kind != dnsservice.DnsRecordKindUser {
				return nil, dnsRecordsInvalid("%s %s is the system's", key.Owner, dns.TypeToString[key.Type])
			}
			addCount -= 1
		}
	}
	if 0 < addCount {
		usedCount := DnsEntryCountInTx(ctx, tx, networkId)
		limitKeys := []string{DnsEntriesLimitKey}
		if probation {
			limitKeys = append(limitKeys, DnsProbationEntriesLimitKey)
		}
		for _, limitKey := range limitKeys {
			limit, enforced := limits.LimitInTx(ctx, tx, networkId, limitKey)
			if !enforced || limit == proSidecarLimitNone || usedCount+addCount <= limit {
				continue
			}
			return nil, sidecar.NewDnsQuotaError(&sidecar.QuotaRefusal{
				LimitKey: limitKey,
				Limit:    limit,
				Used:     usedCount,
				Upgrade:  limitKey == DnsEntriesLimitKey && limits.UpgradeInTx(ctx, tx, networkId, limitKey, usedCount+addCount-1),
			})
		}
	}

	// 7: dns_changes_per_hour
	if limit, enforced := limits.LimitInTx(ctx, tx, networkId, DnsChangesPerHourLimitKey); enforced && limit != proSidecarLimitNone {
		windowStart := now.Add(-settings.ChangesWindow)
		var changeCount int64
		var oldestTime *time.Time
		server.Raise(tx.QueryRow(
			ctx,
			`
				SELECT count(*), min(event_time)
				FROM network_admin_event
				WHERE network_id = $1 AND action = $2 AND $3 <= event_time
			`,
			networkId,
			AdminEventActionDnsRecords,
			windowStart.UTC(),
		).Scan(&changeCount, &oldestTime))
		if limit <= changeCount {
			retryAfter := settings.ChangesWindow
			if oldestTime != nil {
				retryAfter = max(oldestTime.Add(settings.ChangesWindow).Sub(now), time.Millisecond)
			}
			return nil, sidecar.NewDnsQuotaError(&sidecar.QuotaRefusal{
				LimitKey:     DnsChangesPerHourLimitKey,
				Limit:        limit,
				Used:         changeCount,
				RetryAfterMs: uint32(min(retryAfter.Milliseconds(), int64(^uint32(0)))),
			})
		}
	}

	// 8: §12.7's user table, then the screen on each new owner's labels
	recordContext := &dnsservice.DnsUserRecordContext{
		Root:     root,
		Apex:     apex,
		TtlRange: ttl.UserRange,
		Tombstoned: func(label string) bool {
			return isDnsLabelTombstonedInTx(ctx, tx, root, label)
		},
	}
	for index := range parsed.upserts {
		if err := dnsservice.ValidateUserRecordSet(recordContext, &parsed.upserts[index]); err != nil {
			return nil, dnsRecordsInvalid("%v", err)
		}
	}
	var screenFacts *sidecar.ScreenFacts
	for _, set := range parsed.upserts {
		if heldOwners[set.Owner] {
			continue
		}
		if screenFacts == nil {
			screenFacts = &sidecar.ScreenFacts{
				VerifiedDomains: getNetworkVerifiedDomainsInTx(ctx, tx, networkId),
				Probation:       probation,
			}
		}
		for _, ownerLabel := range dns.SplitDomainName(strings.TrimSuffix(set.Owner, apex)) {
			if ownerLabel == "*" || strings.HasPrefix(ownerLabel, "_") {
				continue
			}
			if verdict := sidecar.ScreenName(ownerLabel, screenFacts); verdict != sidecar.ScreenVerdictAllow {
				return nil, dnsRecordsRefused("the name %s is refused (§21.6)", ownerLabel)
			}
		}
	}

	// 9: the compare-and-swap on the version the caller read, under the zone's lock, the zone on first need, the write
	version, err := getDnsZoneVersionOrZeroInTx(ctx, tx, root, networkId)
	if err != nil {
		return nil, err
	}
	if expect := write.Request.ExpectVersion; expect != nil && *expect != version {
		return nil, dnsRecordsError(http.StatusConflict, sidecar.DnsApiCodeVersionConflict, "the zone is at a newer version")
	}
	if _, err := EnsureNetworkZoneInTx(ctx, tx, networkId, root, ttl); err != nil {
		return nil, err
	}
	version, err = getDnsZoneVersionOrZeroInTx(ctx, tx, root, networkId)
	if err != nil {
		return nil, err
	}
	upserts := []dnsservice.DnsRecordSet{}
	for _, set := range parsed.upserts {
		row, ok := held[set.Key()]
		if ok && !row.withheld && row.set.Ttl == set.Ttl && slices.Equal(slices.Sorted(slices.Values(row.set.Rdata)), set.Rdata) {
			// already what the zone holds
			continue
		}
		upserts = append(upserts, set)
	}
	deletes := slices.DeleteFunc(slices.Clone(parsed.deletes), func(key dnsservice.DnsRecordKey) bool {
		_, ok := held[key]
		return !ok
	})
	if len(upserts) == 0 && len(deletes) == 0 {
		return json.Marshal(&sidecar.DnsRecordsAnswer{Version: version})
	}
	result, err := DnsWriteInTx(ctx, tx, &DnsWriteArgs{
		Root:          root,
		NetworkId:     networkId,
		ExpectVersion: &version,
		Upserts:       upserts,
		Deletes:       deletes,
		AdminEvent: &AdminEvent{
			NetworkId:  networkId,
			ActorKind:  write.Caller.ActorKind,
			ActorId:    write.Caller.ActorId,
			Action:     AdminEventActionDnsRecords,
			TargetKind: AdminEventTargetDnsZone,
			Result:     AdminEventResultOk,
			RequestId:  write.Caller.RequestId,
		},
	})
	if err != nil {
		return nil, err
	}
	return json.Marshal(&sidecar.DnsRecordsAnswer{Version: result.Serial})
}

// The zone's version, zero before its row.
func getDnsZoneVersionOrZeroInTx(ctx context.Context, query server.PgCanQuery, root string, networkId server.Id) (uint64, error) {
	var version int64
	result, err := query.Query(ctx, `SELECT version FROM network_dns_zone WHERE root = $1 AND network_id = $2`, root, networkId)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&version))
		}
	})
	return dnsSerialFromColumn(version), nil
}

// Whether label, directly below root, is tombstoned (§12.5): what a CNAME into the root may not point under.
func isDnsLabelTombstonedInTx(ctx context.Context, query server.PgCanQuery, root string, label string) bool {
	tombstoned := false
	result, err := query.Query(
		ctx,
		`SELECT EXISTS (SELECT 1 FROM dns_label WHERE root = $1 AND label = $2 AND state = $3)`,
		root,
		label,
		DnsLabelStateTombstoned,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&tombstoned))
		}
	})
	return tombstoned
}

// The network's verified domains read in the caller's transaction, as GetNetworkVerifiedDomains reads them. Empty in
// a binary without MG6.
func getNetworkVerifiedDomainsInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) []string {
	domains := []string{}
	if !server.SidecarMg6Linked() {
		return domains
	}
	result, err := query.Query(
		ctx,
		`SELECT domain FROM network_domain WHERE network_id = $1 AND verify_time IS NOT NULL ORDER BY domain`,
		networkId,
	)
	server.WithPgResult(result, err, func() {
		for result.Next() {
			var domain string
			server.Raise(result.Scan(&domain))
			domains = append(domains, domain)
		}
	})
	return domains
}

// §20's dns_entries count: the network's user RRsets in every root, one unit each (§12.7). System, DKIM, challenge,
// alias and edge records are free. Zero in a binary that does not link MG5.
func DnsEntryCount(ctx context.Context, networkId server.Id) (count int64) {
	if !server.SidecarMg5Linked() {
		return 0
	}
	server.Db(ctx, func(conn server.PgConn) {
		count = DnsEntryCountInTx(ctx, conn, networkId)
	})
	return
}

// DnsEntryCount in the caller's transaction.
func DnsEntryCountInTx(ctx context.Context, query server.PgCanQuery, networkId server.Id) (count int64) {
	if !server.SidecarMg5Linked() {
		return 0
	}
	result, err := query.Query(
		ctx,
		`SELECT count(*) FROM network_dns_record WHERE network_id = $1 AND kind = $2`,
		networkId,
		string(dnsservice.DnsRecordKindUser),
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&count))
		}
	})
	return
}

// The network's zone in root, its label allocated on first need: the label, its live alias, its version and every
// RRset it holds, withheld ones left out, in owner then type order. What GET /network/dns shows the console and GET
// /dns/zone filters by the caller's grants.
func GetDnsZoneView(ctx context.Context, root string, networkId server.Id) (*sidecar.DnsZone, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, dnsRecordsUnavailable("the zone tables are not linked")
	}
	root = dnsLabelRoot(root)
	var zone *sidecar.DnsZone
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			zone, returnErr = getDnsZoneViewInTx(ctx, tx, root, networkId)
			if returnErr != nil {
				panic(dnsWriteRefusal{err: returnErr})
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return zone, nil
}

func getDnsZoneViewInTx(ctx context.Context, tx server.PgTx, root string, networkId server.Id) (*sidecar.DnsZone, error) {
	label, err := EnsureNetworkLabelInTx(ctx, tx, networkId, root)
	if err != nil {
		return nil, err
	}
	zone := &sidecar.DnsZone{
		NetworkId:  sidecar.Id(networkId),
		Root:       root,
		Label:      label,
		RecordSets: []sidecar.DnsZoneRecordSet{},
	}
	result, err := tx.Query(
		ctx,
		`
			SELECT label FROM dns_label
			WHERE network_id = $1 AND root = $2 AND kind = $3 AND state = $4
			ORDER BY label
			LIMIT 1
		`,
		networkId,
		root,
		DnsLabelKindAlias,
		DnsLabelStateLive,
	)
	server.WithPgResult(result, err, func() {
		if result.Next() {
			server.Raise(result.Scan(&zone.Alias))
		}
	})
	if zone.Version, err = getDnsZoneVersionOrZeroInTx(ctx, tx, root, networkId); err != nil {
		return nil, err
	}
	for _, row := range getDnsZoneRowsInTx(ctx, tx, root, networkId) {
		if row.withheld {
			continue
		}
		typeText, ok := dns.TypeToString[row.set.Type]
		if !ok {
			typeText = fmt.Sprintf("TYPE%d", row.set.Type)
		}
		zone.RecordSets = append(zone.RecordSets, sidecar.DnsZoneRecordSet{
			Name:   row.set.Owner,
			Type:   typeText,
			Ttl:    row.set.Ttl,
			Kind:   string(row.set.Kind),
			Values: slices.Clone(row.set.Rdata),
		})
	}
	return zone, nil
}

// GET /network/dns's body: the zone as GetDnsZoneView reads it, with dns_entries' count and the network's limit, -1
// for none or while sidecar limits are dark.
func GetNetworkDns(ctx context.Context, root string, networkId server.Id) (*sidecar.NetworkDns, error) {
	if !server.SidecarMg2Linked() || !server.SidecarMg5Linked() {
		return nil, dnsRecordsUnavailable("the zone tables are not linked")
	}
	root = dnsLabelRoot(root)
	_, limits := currentDnsRecordsDeps()
	var networkDns *sidecar.NetworkDns
	var returnErr error
	err := dnsCatchRefusal(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			zone, err := getDnsZoneViewInTx(ctx, tx, root, networkId)
			if err != nil {
				returnErr = err
				panic(dnsWriteRefusal{err: err})
			}
			networkDns = &sidecar.NetworkDns{
				DnsZone:    *zone,
				EntryCount: DnsEntryCountInTx(ctx, tx, networkId),
				EntryLimit: proSidecarLimitNone,
			}
			if limit, enforced := limits.LimitInTx(ctx, tx, networkId, DnsEntriesLimitKey); enforced {
				networkDns.EntryLimit = limit
			}
		}, server.TxReadCommitted)
	})
	if err != nil {
		return nil, dnsRecordsApiError(err)
	}
	return networkDns, returnErr
}

// The user root api's /network/dns routes serve: dns.yml's one root, read once a process, as dnsZoneTtl reads its
// TTLs. "" while the process holds no dns.yml, or one naming no root or several, and the routes answer unavailable.
var dnsUserRootOnce = sync.OnceValue(func() string {
	resource, err := server.Config.SimpleResource(dnsSettingsResourceName)
	if err != nil {
		return ""
	}
	data, err := resource.BytesE()
	if err != nil {
		return ""
	}
	settings, err := dnsservice.LoadDnsSettings(data)
	if err != nil || len(settings.Roots) != 1 {
		return ""
	}
	return settings.Roots[0]
})

// A root a test serves in place of dns.yml's.
var dnsUserRootOverride atomic.Pointer[string]

// See dnsUserRootOnce.
func DnsUserRoot() string {
	if root := dnsUserRootOverride.Load(); root != nil {
		return *root
	}
	return dnsUserRootOnce()
}

// Serves root in place of dns.yml's. Answers what puts the previous one back.
func Testing_SetDnsUserRoot(root string) (restore func()) {
	previous := dnsUserRootOverride.Swap(&root)
	return func() {
		dnsUserRootOverride.Store(previous)
	}
}

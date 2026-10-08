package server

// Current-cause reads are explicit diagnostic work. They reuse the serving
// reader and current owner registry; they do not open a candidate, hash whole
// resources, refresh stored lookup times, or change subscriber eligibility.
import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/netip"
	"slices"
	"time"

	mmdb "github.com/oschwald/maxminddb-golang/v2"
)

const ArinCurrentCauseLimit = 64
const ArinCurrentCauseMethod = "current_cause_v1"
const ArinCurrentCauseInventoryMethod = "current_cause_inventory_v1"

type ArinCurrentCause struct {
	DatabaseBuildEpoch       int64                  `json:"database_build_epoch"`
	State                    string                 `json:"state"`
	Risk                     bool                   `json:"risk"`
	NonQuality               bool                   `json:"non_quality"`
	Verified                 bool                   `json:"verified"`
	ProxyRisk                bool                   `json:"proxy_risk"`
	NetworkRiskCategories    []string               `json:"network_risk_categories"`
	OtherNetworkRiskEvidence uint32                 `json:"other_network_risk_evidence"`
	HostingPrefixEvidence    bool                   `json:"hosting_prefix_evidence"`
	OriginRPKIValidity       string                 `json:"origin_rpki_validity"`
	OriginVisibilityPeers    uint32                 `json:"origin_visibility_peers"`
	OriginVisibilityKnown    bool                   `json:"origin_visibility_known"`
	Registration             ArinShadowRegistration `json:"registration"`
	RegistrationAttribution  string                 `json:"registration_attribution"`
	RuleSHA256               string                 `json:"rule_sha256"`
	Origin                   ArinShadowOrigin       `json:"origin"`
	OriginAttribution        string                 `json:"origin_attribution"`
	OriginWithheldReason     string                 `json:"origin_withheld_reason"`
}

// These keyed rows are private authenticated IPC, never an operator report.
// In particular, no address, prefix, address hash, or free-form MMDB text is
// serialized. A refused row keeps its requested key and no cause evidence.
type ArinCurrentCauseRow struct {
	ConnectionId Id                `json:"connection_id"`
	ClientId     Id                `json:"client_id"`
	HandlerId    Id                `json:"handler_id"`
	ActualAt     time.Time         `json:"actual_at"`
	ObservedAt   time.Time         `json:"observed_at"`
	CapturedAt   time.Time         `json:"captured_at"`
	Reason       string            `json:"reason"`
	Cause        *ArinCurrentCause `json:"cause"`
}

type ArinCurrentCauseRequest struct {
	ExpectedEpoch   int64     `json:"expected_epoch"`
	LookupNotBefore time.Time `json:"lookup_not_before"`
	Connections     []Id      `json:"connections"`
}

type ArinCurrentCauseReply struct {
	ExpectedEpoch   int64                 `json:"expected_epoch"`
	LookupNotBefore time.Time             `json:"lookup_not_before"`
	Rows            []ArinCurrentCauseRow `json:"rows"`
}

// Zero means the serving reader has not been initialized. This metadata seam
// never calls the initializer or opens/hashes any resource.
func CurrentArinCauseReaderEpoch() int64 {
	db := arinServingReader.Load()
	if db == nil || schemaType(db.Metadata.DatabaseType) != schemaTypeArinDb {
		return 0
	}
	return db.Metadata.BuildTime().Unix()
}

type ArinCurrentCauseCount struct {
	Cause       ArinCurrentCause `json:"cause"`
	Connections int              `json:"connections"`
}

type ArinCurrentCauseAggregate struct {
	RequestedConnections int                     `json:"requested_connections"`
	QualifiedConnections int                     `json:"qualified_connections"`
	Reasons              map[string]int          `json:"reasons"`
	Causes               []ArinCurrentCauseCount `json:"causes"`
}

// Entries come from the private, generation-bound selector, one selected live
// connection per sampled provider. The operator never expands a destination
// from SQL: handlers resolve only through separately attested RPC clients.
type ArinCurrentCauseSampleEntry struct {
	ClientId     Id
	ConnectionId Id
	HandlerId    Id
}

func CollectArinCurrentCauseSample(ctx context.Context, entries []ArinCurrentCauseSampleEntry, handlers map[Id]*ArinShadowRPCClient,
	epoch int64, lookupNotBefore time.Time,
) (ArinCurrentCauseAggregate, error) {
	if ctx == nil || ctx.Err() != nil || len(entries) == 0 || len(entries) > ArinCurrentCauseLimit || len(handlers) > 256 {
		return ArinCurrentCauseAggregate{}, ErrArinShadowInput
	}
	request := ArinCurrentCauseRequest{ExpectedEpoch: epoch, LookupNotBefore: lookupNotBefore}
	seenClients := map[Id]bool{}
	for _, entry := range entries {
		if entry.ClientId == (Id{}) || entry.HandlerId == (Id{}) || seenClients[entry.ClientId] {
			return ArinCurrentCauseAggregate{}, ErrArinShadowInput
		}
		seenClients[entry.ClientId] = true
		request.Connections = append(request.Connections, entry.ConnectionId)
	}
	if !validArinCurrentCauseRequest(request, NowUtc()) {
		return ArinCurrentCauseAggregate{}, ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, ArinShadowCaptureMaxAge)
	defer cancel()
	reply := ArinCurrentCauseReply{ExpectedEpoch: epoch, LookupNotBefore: lookupNotBefore, Rows: make([]ArinCurrentCauseRow, len(entries))}
	groups, clients := map[*ArinShadowRPCClient][]int{}, []*ArinShadowRPCClient{}
	for i, entry := range entries {
		reply.Rows[i] = ArinCurrentCauseRow{ConnectionId: entry.ConnectionId, CapturedAt: NowUtc(), Reason: "owner_unavailable"}
		if client := handlers[entry.HandlerId]; client != nil {
			if client.identity.Role != "connect" {
				return ArinCurrentCauseAggregate{}, ErrArinShadowInput
			}
			if groups[client] == nil {
				clients = append(clients, client)
			}
			groups[client] = append(groups[client], i)
		}
	}
	// One call per exact owner, with no retry or parallel fan-out. A closed
	// deadline leaves all remaining requested keys explicitly unavailable.
	for _, client := range clients {
		if bounded.Err() != nil {
			break
		}
		indexes := groups[client]
		batch := ArinCurrentCauseRequest{ExpectedEpoch: epoch, LookupNotBefore: lookupNotBefore}
		for _, i := range indexes {
			batch.Connections = append(batch.Connections, entries[i].ConnectionId)
		}
		observed, err := CallArinCurrentCauses(bounded, client, batch)
		if err != nil {
			continue
		}
		for j, i := range indexes {
			row, expected := observed.Rows[j], entries[i]
			if row.Reason == "qualified" && (row.ClientId != expected.ClientId || row.HandlerId != expected.HandlerId) {
				row = ArinCurrentCauseRow{ConnectionId: expected.ConnectionId, CapturedAt: NowUtc(), Reason: "binding_mismatch"}
			}
			reply.Rows[i] = row
		}
	}
	return AggregateArinCurrentCauses(reply)
}

// The only public projection removes every private key. These are connection
// counts, not a provider census, operator rank, or a serving-policy decision.
func AggregateArinCurrentCauses(reply ArinCurrentCauseReply) (ArinCurrentCauseAggregate, error) {
	if len(reply.Rows) == 0 || len(reply.Rows) > ArinCurrentCauseLimit || reply.ExpectedEpoch <= 0 {
		return ArinCurrentCauseAggregate{}, ErrArinShadowInput
	}
	out := ArinCurrentCauseAggregate{RequestedConnections: len(reply.Rows), Reasons: map[string]int{}, Causes: []ArinCurrentCauseCount{}}
	seen, counts := map[Id]bool{}, map[string]*ArinCurrentCauseCount{}
	for _, row := range reply.Rows {
		if row.ConnectionId == (Id{}) || seen[row.ConnectionId] || !slices.Contains(arinShadowCaptureReasons[:], row.Reason) {
			return ArinCurrentCauseAggregate{}, ErrArinShadowInput
		}
		seen[row.ConnectionId] = true
		out.Reasons[row.Reason]++
		if row.Reason != "qualified" {
			if row.Cause != nil {
				return ArinCurrentCauseAggregate{}, ErrArinShadowInput
			}
			continue
		}
		if row.Cause == nil || !validArinCurrentCause(*row.Cause) || row.Cause.DatabaseBuildEpoch != reply.ExpectedEpoch {
			return ArinCurrentCauseAggregate{}, ErrArinShadowInput
		}
		encoded, err := json.Marshal(row.Cause)
		if err != nil {
			return ArinCurrentCauseAggregate{}, ErrArinShadowInput
		}
		key := string(encoded)
		if counts[key] == nil {
			counts[key] = &ArinCurrentCauseCount{Cause: *row.Cause}
		}
		counts[key].Connections++
		out.QualifiedConnections++
	}
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		out.Causes = append(out.Causes, *counts[key])
	}
	return out, nil
}

func validArinCurrentCauseRequest(request ArinCurrentCauseRequest, now time.Time) bool {
	if request.ExpectedEpoch <= 0 || request.LookupNotBefore.Before(time.Unix(request.ExpectedEpoch, 0)) ||
		request.LookupNotBefore.After(now.Add(arinShadowCaptureClockSkew)) || len(request.Connections) == 0 || len(request.Connections) > ArinCurrentCauseLimit {
		return false
	}
	seen := make(map[Id]bool, len(request.Connections))
	for _, id := range request.Connections {
		if id == (Id{}) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

func validArinCurrentCause(c ArinCurrentCause) bool {
	if len(c.NetworkRiskCategories) > 5 || c.OtherNetworkRiskEvidence > 64 ||
		c.ProxyRisk != (len(c.NetworkRiskCategories) > 0) ||
		c.OriginVisibilityKnown != (c.OriginVisibilityPeers > 0) ||
		!slices.Contains([]string{"", "valid", "valid-aggregate", "invalid", "not-found"}, c.OriginRPKIValidity) ||
		(c.OriginRPKIValidity != "" || c.OriginVisibilityKnown) && c.OriginAttribution != "present" {
		return false
	}
	for i, category := range c.NetworkRiskCategories {
		if !slices.Contains([]string{"proxy", "residential_proxy", "virtual_isp", "vpn", "tor"}, category) ||
			i > 0 && c.NetworkRiskCategories[i-1] >= category {
			return false
		}
	}
	if c.DatabaseBuildEpoch <= 0 || !slices.Contains([]string{"subscriber", "excluded", "unknown", "ambiguous"}, c.State) ||
		c.NonQuality != (c.State != "subscriber") || c.Verified != (c.State == "subscriber" && !c.Risk) || c.ProxyRisk && !c.Risk ||
		!validArinShadowRegistration(c.Registration) || !validArinShadowOrigin(c.Origin) ||
		!slices.Contains([]string{"single", "multiple", "unavailable"}, c.RegistrationAttribution) ||
		!slices.Contains([]string{"present", "absent", "overflow"}, c.OriginAttribution) ||
		(c.RuleSHA256 != "" && (len(c.RuleSHA256) != 64 || !arinShadowHex(c.RuleSHA256, 32))) {
		return false
	}
	if (c.RegistrationAttribution == "single") != (c.Registration.OrgHandle != "") ||
		(c.OriginAttribution == "present") != (len(c.Origin.ASNs) > 0) {
		return false
	}
	if c.OriginWithheldReason != "" && (c.OriginAttribution != "present" || c.Origin.UseState != "withheld" || !slices.Contains([]string{
		"insufficient-origin-visibility", "rpki-invalid-origin", "outside-reviewed-countries", "registry-hosting-assignment",
	}, c.OriginWithheldReason)) {
		return false
	}
	return true
}

// Only the serving reader is consulted. Connect warms this reader before
// serving; the request neither maps a second resource nor infers a file hash
// from an epoch. The exact queried epoch must match the durable selected row.
func lookupCurrentArinCause(address netip.Addr) (*ArinCurrentCause, error) {
	db := arinServingReader.Load()
	if db == nil || schemaType(db.Metadata.DatabaseType) != schemaTypeArinDb {
		return nil, ErrArinShadowInput
	}
	return currentArinCauseFromDatabase(db, address)
}

func currentArinCauseFromDatabase(db *mmdb.Reader, address netip.Addr) (*ArinCurrentCause, error) {
	if db == nil || !address.IsValid() || address.IsUnspecified() || address.Zone() != "" {
		return nil, ErrArinShadowInput
	}
	result := db.Lookup(address.Unmap())
	info := ArinInfo{schemaType: schemaTypeArinDb, DatabaseBuildEpoch: db.Metadata.BuildTime().Unix()}
	if result.Decode(&info) != nil || info.ClassifierVersion != 1 || info.QualityPolicyVersion != 2 {
		return nil, ErrArinShadowInput
	}
	var extra struct {
		OrgHandle               string   `maxminddb:"org_handle"`
		NetHandle               string   `maxminddb:"net_handle"`
		ClassificationOrgHandle string   `maxminddb:"classification_org_handle"`
		ClassificationRule      string   `maxminddb:"classification_rule"`
		MultipleOwners          bool     `maxminddb:"multiple_registration_owners"`
		OriginASNs              []uint32 `maxminddb:"origin_asns"`
		OriginUseState          string   `maxminddb:"origin_use_state"`
		OriginWithheldReason    string   `maxminddb:"origin_withheld_reason"`
		OriginRPKIValidity      string   `maxminddb:"origin_rpki_validity"`
		OriginVisibilityPeers   uint32   `maxminddb:"origin_peers"`
		HostingPrefixSourceIDs  []string `maxminddb:"hosting_prefix_source_ids"`
		Evidence                []struct {
			Category string `maxminddb:"category"`
		} `maxminddb:"network_risk_evidence"`
	}
	if result.Decode(&extra) != nil || len(extra.Evidence) > 64 || len(extra.HostingPrefixSourceIDs) > 64 {
		return nil, ErrArinShadowInput
	}
	for _, source := range extra.HostingPrefixSourceIDs {
		if len(source) == 0 || len(source) > 128 {
			return nil, ErrArinShadowInput
		}
	}
	cause := &ArinCurrentCause{DatabaseBuildEpoch: info.DatabaseBuildEpoch, State: info.QualityState,
		Risk: info.Risk, NonQuality: info.NonQuality, Verified: info.QualityVerified(),
		RegistrationAttribution: "unavailable", OriginAttribution: "absent",
		NetworkRiskCategories: []string{}, HostingPrefixEvidence: len(extra.HostingPrefixSourceIDs) > 0}
	if extra.MultipleOwners {
		cause.RegistrationAttribution = "multiple"
	} else {
		cause.Registration = newArinShadowRegistration(extra.OrgHandle, extra.NetHandle, extra.ClassificationOrgHandle, extra.ClassificationRule)
		if cause.Registration.OrgHandle != "" {
			cause.RegistrationAttribution = "single"
		}
	}
	if extra.ClassificationRule != "" {
		hash := sha256.Sum256([]byte(extra.ClassificationRule))
		cause.RuleSHA256 = hex.EncodeToString(hash[:])
	}
	if len(extra.OriginASNs) > arinShadowOriginASNLimit {
		cause.OriginAttribution = "overflow"
	} else if len(extra.OriginASNs) > 0 {
		cause.Origin = ArinShadowOrigin{ASNs: slices.Clone(extra.OriginASNs), UseState: extra.OriginUseState}
		cause.OriginAttribution = "present"
		cause.OriginWithheldReason = extra.OriginWithheldReason
		cause.OriginRPKIValidity = extra.OriginRPKIValidity
		cause.OriginVisibilityPeers = extra.OriginVisibilityPeers
		cause.OriginVisibilityKnown = extra.OriginVisibilityPeers > 0
	} else if extra.OriginUseState != "" || extra.OriginWithheldReason != "" || extra.OriginRPKIValidity != "" || extra.OriginVisibilityPeers != 0 {
		return nil, ErrArinShadowInput
	}
	for _, evidence := range extra.Evidence {
		if slices.Contains([]string{"proxy", "residential_proxy", "virtual_isp", "vpn", "tor"}, evidence.Category) {
			if !slices.Contains(cause.NetworkRiskCategories, evidence.Category) {
				cause.NetworkRiskCategories = append(cause.NetworkRiskCategories, evidence.Category)
			}
		} else {
			cause.OtherNetworkRiskEvidence++
		}
	}
	slices.Sort(cause.NetworkRiskCategories)
	cause.ProxyRisk = len(cause.NetworkRiskCategories) > 0
	if !validArinCurrentCause(*cause) {
		return nil, ErrArinShadowInput
	}
	return cause, nil
}

// The only production caller is the existing authenticated, default-disabled
// Connect endpoint. Authentication and process nonce checks precede this seam.
func ArinCurrentCauseRPC(ctx context.Context, input json.RawMessage, owners ArinShadowCaptureOwnerReader, read ArinShadowCaptureFactReader) (any, error) {
	var request ArinCurrentCauseRequest
	if DecodeArinShadowRPC(input, &request) != nil {
		return nil, ErrArinShadowInput
	}
	return readCurrentArinCauses(ctx, request, owners, read, lookupCurrentArinCause, NowUtc)
}

func readCurrentArinCauses(ctx context.Context, request ArinCurrentCauseRequest, owners ArinShadowCaptureOwnerReader, read ArinShadowCaptureFactReader,
	lookup func(netip.Addr) (*ArinCurrentCause, error), clock func() time.Time,
) (ArinCurrentCauseReply, error) {
	if ctx == nil || ctx.Err() != nil || owners == nil || read == nil || lookup == nil || clock == nil || !validArinCurrentCauseRequest(request, clock()) {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, ArinShadowRPCCallTimeout)
	defer cancel()
	started := clock()
	targets, err := owners(bounded, request.Connections)
	if err != nil || bounded.Err() != nil || len(targets) != len(request.Connections) {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	before := make([]ArinShadowOwnerSnapshot, len(targets))
	valid := make([]bool, len(targets))
	for i, target := range targets {
		if target.ConnectionId != request.Connections[i] {
			return ArinCurrentCauseReply{}, ErrArinShadowInput
		}
		before[i], valid[i] = currentArinShadowOwner(target.Owner, target.ConnectionId, started)
	}
	facts, err := read(bounded, request.Connections)
	if err != nil || bounded.Err() != nil || len(facts) != len(targets) {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	reply := ArinCurrentCauseReply{ExpectedEpoch: request.ExpectedEpoch, LookupNotBefore: request.LookupNotBefore, Rows: make([]ArinCurrentCauseRow, len(targets))}
	for i, target := range targets {
		fact, now := facts[i], clock()
		if bounded.Err() != nil || now.Before(started) || now.Sub(started) > ArinShadowRPCCallTimeout || fact.ConnectionId != target.ConnectionId {
			return ArinCurrentCauseReply{}, ErrArinShadowInput
		}
		row := ArinCurrentCauseRow{ConnectionId: target.ConnectionId, Reason: "owner_unavailable", CapturedAt: now}
		if valid[i] {
			row.Reason = "facts_unavailable"
			if fact.Present && fact.Connected {
				row.Reason = "binding_mismatch"
				if fact.ClientId == before[i].ClientId && fact.HandlerId == before[i].HandlerId {
					row.Reason = "facts_stale"
					if !fact.ObservedAt.Before(started.Add(-arinShadowCaptureClockSkew)) && !fact.ObservedAt.After(now.Add(arinShadowCaptureClockSkew)) &&
						!fact.Actual.At.Before(request.LookupNotBefore) && !fact.Actual.At.After(fact.ObservedAt.Add(arinShadowCaptureClockSkew)) {
						row.Reason = "active_mismatch"
						if fact.Actual.Epoch == request.ExpectedEpoch {
							row.Reason = "lookup_unavailable"
							cause, lookupErr := lookup(before[i].Address.Unmap())
							if lookupErr == nil && cause != nil && validArinCurrentCause(*cause) {
								row.Reason = "active_mismatch"
								if cause.DatabaseBuildEpoch == request.ExpectedEpoch && cause.Risk == fact.Actual.Risk && cause.NonQuality == fact.Actual.NonQuality && cause.Verified == fact.Actual.Verified {
									row.ClientId, row.HandlerId = fact.ClientId, fact.HandlerId
									row.ActualAt, row.ObservedAt = fact.Actual.At, fact.ObservedAt
									row.Reason, row.Cause = "qualified", cause
								}
							}
						}
					}
				}
			}
			after, current := currentArinShadowOwner(target.Owner, target.ConnectionId, clock())
			if !current || !sameArinShadowOwner(before[i], after) {
				row = ArinCurrentCauseRow{ConnectionId: target.ConnectionId, Reason: "owner_changed"}
			}
		}
		row.CapturedAt = clock()
		reply.Rows[i] = row
	}
	if bounded.Err() != nil || clock().Before(started) || clock().Sub(started) > ArinShadowRPCCallTimeout {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	return reply, nil
}

// Client-side validation is separate from public aggregation. Missing owners
// and changed rows remain explicit; callers must not count them as clean.
func CallArinCurrentCauses(ctx context.Context, client *ArinShadowRPCClient, request ArinCurrentCauseRequest) (ArinCurrentCauseReply, error) {
	started := NowUtc()
	if client == nil || client.identity.Role != "connect" || !validArinCurrentCauseRequest(request, started) {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	var reply ArinCurrentCauseReply
	if client.Call(ctx, ArinCurrentCauseMethod, request, &reply) != nil || reply.ExpectedEpoch != request.ExpectedEpoch || !reply.LookupNotBefore.Equal(request.LookupNotBefore) || len(reply.Rows) != len(request.Connections) {
		return ArinCurrentCauseReply{}, ErrArinShadowInput
	}
	for i, row := range reply.Rows {
		if row.ConnectionId != request.Connections[i] || !slices.Contains(arinShadowCaptureReasons[:], row.Reason) || row.CapturedAt.Before(started.Add(-arinShadowCaptureClockSkew)) || row.CapturedAt.After(NowUtc().Add(arinShadowCaptureClockSkew)) {
			return ArinCurrentCauseReply{}, ErrArinShadowInput
		}
		if row.Reason == "qualified" {
			if row.ClientId == (Id{}) || row.HandlerId == (Id{}) || row.Cause == nil || !validArinCurrentCause(*row.Cause) || row.Cause.DatabaseBuildEpoch != request.ExpectedEpoch ||
				row.ActualAt.Before(request.LookupNotBefore) || row.ActualAt.After(row.ObservedAt.Add(arinShadowCaptureClockSkew)) || row.ObservedAt.Before(started.Add(-arinShadowCaptureClockSkew)) || row.ObservedAt.After(row.CapturedAt.Add(arinShadowCaptureClockSkew)) {
				return ArinCurrentCauseReply{}, ErrArinShadowInput
			}
		} else if row.Cause != nil || row.ClientId != (Id{}) || row.HandlerId != (Id{}) || !row.ActualAt.IsZero() || !row.ObservedAt.IsZero() {
			return ArinCurrentCauseReply{}, ErrArinShadowInput
		}
	}
	return reply, nil
}

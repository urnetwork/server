package controller

// Capture the flag's census (sn CAPTURETHEFLAG.md §4.2, §4.3): right after an
// epoch's end boundary is final, as the users count is captured, the census
// step reads the end-boundary facts of every miner the epoch's samples found,
// applies the rules (ctf_census_rules.go), encodes the census with the sn ctf
// package, stores it once and puts its objects in the ledger object store,
// header last. The driver then commits its hash with commitCtfCensus and
// records the commitment; this step never sends a transaction, and never
// decides whether an epoch is live or a shadow run.
//
// The facts are live state read at the capture, so the capture is bounded:
// past ctfCensusCaptureWindow after the epoch's end an epoch gets no census,
// and every slot of it carries (§4.7). Within it a failed read retries.

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/urfoundation/sn/ctf"
	"github.com/urfoundation/sn/ledger"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The capture the flag steps, for the task layer.
const (
	StCtfStepSample = "ctf_sample"
	StCtfStepCensus = "ctf_census"
)

// StCtfAlertCensus is raised once per epoch when its census cannot be built for
// a reason that retrying will not change, and StCtfAlertSamples when its census
// is built from fewer samples than the epoch had.
const (
	StCtfAlertCensus  = "ctf_census"
	StCtfAlertSamples = "ctf_samples"
)

// How long after an epoch's end its census may still be read: within it a
// late census still reads the boundary's state, and the driver keeps more than
// half of the 600-block commitment window to commit it.
const ctfCensusCaptureWindow = 50 * time.Minute

// Spaces a census's retries while its boundary is not final or a read fails.
const ctfCensusRetryDelay = time.Minute

// How long past an epoch's capture window, by the block clock's estimate of
// its end, the sync still plans its census, so the step itself reads the end's
// authenticated time and alerts.
const ctfCensusPlanGrace = 10 * time.Minute

// StCtfStep is one planned capture the flag step.
type StCtfStep struct {
	Kind  string
	Epoch uint64
	// the sample's index and the block and time it was planned at (samples)
	Index       int
	HeadBlock   uint64
	PlannedTime time.Time
	RunAt       time.Time
}

// The samples planned by this process, the last per deployment, so a sync turn
// plans each sample once; the task's run-once key keeps workers to one.
var ctfPlannedSample sync.Map

// The epochs whose census this process saw built or given up.
var ctfCensusConcluded sync.Map

// StCtfSync plans one sync turn's capture the flag steps: the current
// quarter hour's sample while presence is on, and the census of the epoch that
// has just ended while the census is on, from its first epoch. Nothing is due
// while ctf.yml leaves both off.
func StCtfSync(ctx context.Context, state *StEpochState) ([]*StCtfStep, error) {
	settings := model.CtfDataSettings()
	if !settings.Presence.Enabled && !settings.Census.Enabled {
		return nil, nil
	}
	cfg, _, err := stRequire()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled || state == nil || state.Epoch == 0 {
		return nil, nil
	}
	deploymentKey := cfg.DeploymentKey()
	now := server.NowUtc()
	steps := []*StCtfStep{}
	if settings.Presence.Enabled {
		if index, ok := model.CtfSampleIndex(state.EpochStartBlock, state.TEpochBlocks, state.HeadBlock); ok {
			planned := fmt.Sprintf("%d|%d", state.Epoch, index)
			if previous, loaded := ctfPlannedSample.Swap(fmt.Sprintf("%s|%d", deploymentKey, cfg.NoId), planned); !loaded || previous != planned {
				steps = append(steps, &StCtfStep{Kind: StCtfStepSample, Epoch: state.Epoch, Index: index, HeadBlock: state.HeadBlock, PlannedTime: now, RunAt: now})
			}
		}
	}
	if settings.Census.Enabled && 1 < state.Epoch {
		epoch := state.Epoch - 1
		concludedKey := fmt.Sprintf("%s|%d|%d", deploymentKey, cfg.NoId, epoch)
		if _, concluded := ctfCensusConcluded.Load(concludedKey); !concluded && settings.Census.FirstEpoch <= epoch {
			// the epoch ended when the current one started; past its capture
			// window an epoch without a census gets none, so a step that keeps
			// failing is not planned again (a built census is still published)
			ended := StEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, state.EpochStartBlock)
			pastWindow := !state.HeadBlockTime.IsZero() && ctfCensusCaptureWindow+ctfCensusPlanGrace < now.Sub(ended)
			switch census := model.GetCtfCensus(ctx, deploymentKey, cfg.NoId, epoch); {
			case census != nil && census.PublishedTime != nil:
				ctfCensusConcluded.Store(concludedKey, true)
			case census == nil && pastWindow:
				ctfCensusConcluded.Store(concludedKey, true)
			default:
				steps = append(steps, &StCtfStep{Kind: StCtfStepCensus, Epoch: epoch, RunAt: now})
			}
		}
	}
	return steps, nil
}

// StCtfRunStep runs one planned step. Each is idempotent.
func StCtfRunStep(ctx context.Context, step *StCtfStep) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errStNotConfigured
	}
	settings := model.CtfDataSettings()
	switch step.Kind {
	case StCtfStepSample:
		if !settings.Presence.Enabled {
			return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "presence is off"}, nil
		}
		return stCtfTakeSample(ctx, cfg, step), nil
	case StCtfStepCensus:
		if !settings.Census.Enabled {
			return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "the census is off"}, nil
		}
		if err := stPayoutAdmission(ctx, cfg); err != nil {
			return nil, err
		}
		return stCtfBuildCensus(ctx, cfg, client, settings, step.Epoch), nil
	}
	return nil, fmt.Errorf("st ctf: unknown step %q", step.Kind)
}

func stCtfCensusRetry(reason string) *StPublishOutcome {
	ctfMetrics.censuses.WithLabelValues(ctfCensusResultRetry).Inc()
	return &StPublishOutcome{Status: model.StPublishStatusFailed, Reason: stScrubEndpoints(reason), Retry: true, RetryAt: server.NowUtc().Add(ctfCensusRetryDelay)}
}

// stCtfBuildCensus builds, stores and publishes an epoch's census, once.
func stCtfBuildCensus(ctx context.Context, cfg *StConfig, client StClient, settings *model.CtfDataConfig, epoch uint64) *StPublishOutcome {
	deploymentKey := cfg.DeploymentKey()
	concludedKey := fmt.Sprintf("%s|%d|%d", deploymentKey, cfg.NoId, epoch)
	if census := model.GetCtfCensus(ctx, deploymentKey, cfg.NoId, epoch); census != nil {
		return stCtfPublishCensus(ctx, cfg, census)
	}
	authority, err := client.PayoutEpochAuthority(ctx, epoch)
	if err != nil {
		return stCtfCensusRetry(err.Error())
	}
	if authority == nil || authority.Epoch != epoch {
		return stCtfCensusRetry(fmt.Sprintf("the payout authority of epoch %d is absent or differs", epoch))
	}
	captureTime := server.NowUtc()
	if lag := captureTime.Sub(authority.EndTime); ctfCensusCaptureWindow < lag {
		ctfCensusConcluded.Store(concludedKey, true)
		ctfMetrics.censuses.WithLabelValues(ctfCensusResultGivenUp).Inc()
		stLedgerV2Alert(cfg, StCtfAlertCensus, fmt.Sprintf("epoch %d", epoch), "epoch %d has no census: its end passed %s ago, past the %s its boundary can still be read in, so every slot of it carries", epoch, lag.Round(time.Second), ctfCensusCaptureWindow)
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d's capture window has passed", epoch)}
	}

	providers, err := model.ReadCtfPresenceProviders(ctx)
	if err != nil {
		return stCtfCensusRetry(fmt.Sprintf("the export's online set: %s", err))
	}
	if providers == nil || model.CtfPresenceProvidersMaxAge < captureTime.Sub(providers.MeasuredAt) {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d: the score export's online set is stale or absent, so P1 at the end cannot be read", epoch))
	}

	samples := model.GetCtfPresenceSamples(ctx, deploymentKey, cfg.NoId, epoch)
	providerSamples := 0
	for _, sample := range samples {
		if sample.ProviderSourceTime != nil {
			providerSamples += 1
		}
	}
	outages := model.GetCtfPresenceOutages(ctx, deploymentKey, cfg.NoId, authority.StartTime, authority.EndTime)
	excused := model.CtfExcusedSamples(authority.StartTime, authority.EndTime, outages)
	facts := &ctfCensusFacts{
		Excused:     excused,
		Required:    model.CtfRequiredSamples(excused.Count()),
		Presences:   model.GetCtfPresences(ctx, deploymentKey, cfg.NoId, epoch),
		Providers:   map[server.Id]*ctfProviderEnd{},
		Extenders:   map[server.Id]*ctfExtenderEnd{},
		OwnNetworks: map[server.Id]bool{},
		HeadBound:   map[server.Id]bool{},
		Coldkeys:    map[server.Id][32]byte{},
	}
	providerIds, extenderIds, clientIds := ctfMemberClientIds(facts.Presences)

	// P1, P3–P6
	online := map[server.Id]bool{}
	for _, provider := range providers.Providers {
		online[provider.ClientId] = true
	}
	boundaries := model.GetCtfProviderBoundaries(ctx, providerIds)
	// read only: a pin set after the epoch would be no prefix of it
	egresses := model.GetCtfProviderEgress(ctx, providerIds, VerifySettings(), epoch, false)
	sampledUnits := ctfSampledUnits(facts.Presences)
	for _, clientId := range providerIds {
		end := &ctfProviderEnd{Online: online[clientId], Egress: egresses[clientId]}
		if boundary := boundaries[clientId]; boundary != nil {
			networkId := boundary.NetworkId
			end.NetworkId = &networkId
			end.Active, end.TopLevel = boundary.Active, boundary.TopLevel
			end.Connected, end.ValidLocation = boundary.Connected, boundary.ValidLocation
			end.Public, end.CountryCode = boundary.Public, boundary.CountryCode
		}
		end.Egress = ctfResolveProviderUnit(end.Egress, sampledUnits[clientId], end.CountryCode)
		facts.Providers[clientId] = end
	}

	// X1, X3–X6
	for clientId, records := range model.GetCtfExtenderBoundaries(ctx, extenderIds, authority.StartTime, authority.EndTime) {
		if record := ctfExtenderRecord(records); record != nil {
			networkId := record.NetworkId
			facts.Extenders[clientId] = &ctfExtenderEnd{
				ExtenderId:         record.ExtenderId,
				NetworkId:          &networkId,
				Active:             record.Active,
				Canary:             record.Canary,
				CountryCode:        record.CountryCode,
				ActivatedElsewhere: slices.ContainsFunc(record.ActivationCountryCodes, func(code string) bool { return code != record.CountryCode }),
				Egress:             model.CtfExtenderEgress(record.Ipv4, record.Ipv6, VerifySettings()),
			}
		}
	}

	// P6, X6: the networks the operator declares, its gated durable fleet and
	// every canary's network
	for _, networkId := range settings.OwnNetworkIds() {
		facts.OwnNetworks[networkId] = true
	}
	if extenderConfig, err := EnvExtenderConfig(); err == nil {
		for _, networkId := range extenderConfig.DurableNetworkIds() {
			facts.OwnNetworks[networkId] = true
		}
	}
	for _, networkId := range model.GetCtfCanaryNetworkIds(ctx) {
		facts.OwnNetworks[networkId] = true
	}

	// P5, X5: a fleet binding at either boundary, and the provider pool's
	// wallet selection
	ids16 := make([][16]byte, len(clientIds))
	for i, clientId := range clientIds {
		ids16[i] = stId16(clientId)
	}
	bound, err := stHeadExcludedClients(ctx, cfg, client, epoch, authority.Start.Block, authority.End.Block, ids16)
	if err != nil {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d fleet bindings: %s", epoch, err))
	}
	for clientId := range bound {
		facts.HeadBound[server.Id(clientId)] = true
	}
	coldkeys, err := stCtfColdkeys(ctx, cfg, authority, clientIds)
	if err != nil {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d wallets: %s", epoch, err))
	}
	facts.Coldkeys = coldkeys

	decision := decideCtfCensus(facts)
	encoded, err := encodeCtfCensus(cfg.NoId, epoch, &excused, decision)
	if err != nil {
		// the rules give a canonical census; one that is not is a fault here
		ctfCensusConcluded.Store(concludedKey, true)
		ctfMetrics.censuses.WithLabelValues(ctfCensusResultGivenUp).Inc()
		stLedgerV2Alert(cfg, StCtfAlertCensus, fmt.Sprintf("epoch %d", epoch), "epoch %d's census does not encode, so it has none and every slot of it carries: %s", epoch, err)
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d's census does not encode: %s", epoch, err)}
	}

	census := &model.CtfCensus{
		DeploymentKey:        deploymentKey,
		NoId:                 cfg.NoId,
		Epoch:                epoch,
		CensusHash:           encoded.Hash,
		Header:               encoded.Header,
		ExcusedSamples:       excused.Indexes(),
		SamplesTaken:         len(samples),
		ProviderSamplesTaken: providerSamples,
		RequiredSamples:      facts.Required,
		StartBlock:           authority.Start.Block,
		EndBlock:             authority.End.Block,
		StartTime:            authority.StartTime,
		EndTime:              authority.EndTime,
		CaptureTime:          captureTime,
		ProviderSourceTime:   providers.MeasuredAt,
	}
	units := map[int]map[[32]byte]bool{model.CtfRoleProvider: {}, model.CtfRoleExtender: {}}
	for _, member := range decision.Members {
		if member.Role == model.CtfRoleProvider {
			census.ProviderMembers += 1
		} else {
			census.ExtenderMembers += 1
		}
		member.Epoch = epoch
		if !member.Candidate {
			continue
		}
		units[member.Role][*member.Unit] = true
		if member.Role == model.CtfRoleProvider {
			census.ProviderCandidates += 1
		} else {
			census.ExtenderCandidates += 1
		}
	}
	census.Candidates = census.ProviderCandidates + census.ExtenderCandidates
	census.ProviderUnits = int64(len(units[model.CtfRoleProvider]))
	census.ExtenderUnits = int64(len(units[model.CtfRoleExtender]))
	objects := make([]*model.CtfCensusObject, 0, len(encoded.Objects))
	for _, ref := range slices.SortedFunc(maps.Keys(encoded.Objects), func(a, b ledger.Ref) int { return slices.Compare(a[:], b[:]) }) {
		objects = append(objects, &model.CtfCensusObject{Ref: ref, Data: encoded.Objects[ref]})
	}
	if !model.CreateCtfCensus(ctx, census, objects, decision.Members, decision.Pots, decision.Exclusions) {
		// another worker built it first; publish what it stored
		return stCtfPublishCensus(ctx, cfg, model.GetCtfCensus(ctx, deploymentKey, cfg.NoId, epoch))
	}
	ctfMetrics.censuses.WithLabelValues(ctfCensusResultBuilt).Inc()
	stCtfCensusMetrics(census, decision, excused.Count(), captureTime.Sub(authority.EndTime))
	if len(samples) < model.CtfSamplesPerEpoch {
		stLedgerV2Alert(cfg, StCtfAlertSamples, fmt.Sprintf("epoch %d", epoch), "epoch %d's census counts %d of its %d samples taken (%d with the providers' half) and excuses %d: a missing sample found nobody; declare the outage before the census if the platform was down", epoch, len(samples), model.CtfSamplesPerEpoch, providerSamples, excused.Count())
	}
	glog.Infof("[ctf]epoch %d census %s: %d candidates (%d providers in %d units, %d extenders in %d units) of %d members, %d samples taken, %d excused, required %d\n", epoch, ledger.Ref(census.CensusHash), census.Candidates, census.ProviderCandidates, census.ProviderUnits, census.ExtenderCandidates, census.ExtenderUnits, census.ProviderMembers+census.ExtenderMembers, len(samples), excused.Count(), facts.Required)
	stCtfCensusRetention(ctx, cfg, settings, epoch)
	return stCtfPublishCensus(ctx, cfg, census)
}

// encodeCtfCensus encodes a decided census in the sn ctf census format (§4.3):
// one row per candidate in its own country, the excused samples and the
// exclusion counts.
func encodeCtfCensus(noId uint64, epoch uint64, excused *model.CtfSampleSet, decision *ctfCensusDecision) (*ctf.CensusObjects, error) {
	census := &ctf.Census{NoID: noId, Epoch: epoch}
	for _, member := range decision.candidates() {
		country, ok := ctf.ParseCountry(member.CountryCode)
		if !ok {
			return nil, fmt.Errorf("candidate %s has country %q outside the list", member.ClientId, member.CountryCode)
		}
		census.Candidates = append(census.Candidates, ctf.Candidate{
			Role:     ctf.Role(member.Role),
			Country:  country,
			Unit:     *member.Unit,
			Family:   uint8(member.UnitFamily),
			ClientID: [16]byte(member.ClientId),
		})
	}
	for _, index := range excused.Indexes() {
		census.ExcusedSamples = append(census.ExcusedSamples, uint16(index))
	}
	for _, exclusion := range decision.Exclusions {
		country := ctf.CountryUnknown
		if exclusion.CountryCode != model.CtfCountryUnknown {
			parsed, ok := ctf.ParseCountry(exclusion.CountryCode)
			if !ok {
				return nil, fmt.Errorf("an exclusion has country %q outside the list", exclusion.CountryCode)
			}
			country = parsed
		}
		census.Excluded = append(census.Excluded, ctf.Exclusion{Country: country, Role: ctf.Role(exclusion.Role), Reason: ctf.Reason(exclusion.Reason), Count: uint32(exclusion.Count)})
	}
	return ctf.EncodeCensus(census)
}

// stCtfPublishCensus puts a stored census's objects in the ledger object store,
// put-if-absent and header last, so the census hash names an object only once
// everything it names is there, then records it published.
func stCtfPublishCensus(ctx context.Context, cfg *StConfig, census *model.CtfCensus) *StPublishOutcome {
	if census == nil {
		return stCtfCensusRetry("the census is not stored")
	}
	concludedKey := fmt.Sprintf("%s|%d|%d", cfg.DeploymentKey(), cfg.NoId, census.Epoch)
	if census.PublishedTime != nil {
		ctfCensusConcluded.Store(concludedKey, true)
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d census %s already published", census.Epoch, ledger.Ref(census.CensusHash))}
	}
	objects := model.GetCtfCensusObjects(ctx, cfg.DeploymentKey(), cfg.NoId, census.Epoch)
	if _, ok := objects[census.CensusHash]; !ok {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d census header is not stored", census.Epoch))
	}
	for _, ref := range slices.SortedFunc(maps.Keys(objects), func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) }) {
		if ref == census.CensusHash {
			continue
		}
		if _, err := PutStLedgerV2Object(ctx, objects[ref]); err != nil {
			return stCtfCensusRetry(fmt.Sprintf("epoch %d census object %s: %s", census.Epoch, ledger.Ref(ref), err))
		}
	}
	if _, err := PutStLedgerV2Object(ctx, objects[census.CensusHash]); err != nil {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d census header: %s", census.Epoch, err))
	}
	model.SetCtfCensusPublished(ctx, cfg.DeploymentKey(), cfg.NoId, census.Epoch, server.NowUtc())
	ctfCensusConcluded.Store(concludedKey, true)
	ctfMetrics.censuses.WithLabelValues(ctfCensusResultPublished).Inc()
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d census %s published: %d candidates", census.Epoch, ledger.Ref(census.CensusHash), census.Candidates)}
}

// The members' client ids, each role's by client id, and every member client
// once.
func ctfMemberClientIds(presences []*model.CtfPresence) (providerIds []server.Id, extenderIds []server.Id, clientIds []server.Id) {
	providers, extenders, clients := map[server.Id]bool{}, map[server.Id]bool{}, map[server.Id]bool{}
	for _, presence := range presences {
		if presence.Role == model.CtfRoleProvider {
			providers[presence.ClientId] = true
		} else {
			extenders[presence.ClientId] = true
		}
		clients[presence.ClientId] = true
	}
	sorted := func(set map[server.Id]bool) []server.Id {
		ids := make([]server.Id, 0, len(set))
		for id := range set {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b server.Id) int { return a.Cmp(b) })
		return ids
	}
	return sorted(providers), sorted(extenders), sorted(clients)
}

// A provider's units the samples saw, per country: the latest sample's unit
// and family in each country it was present in.
type ctfSampledUnit struct {
	countryCode     string
	unit            [32]byte
	family          int
	lastSampleIndex int
}

func ctfSampledUnits(presences []*model.CtfPresence) map[server.Id][]*ctfSampledUnit {
	units := map[server.Id][]*ctfSampledUnit{}
	for _, presence := range presences {
		if presence.Role != model.CtfRoleProvider || presence.Unit == nil || (presence.UnitFamily != 4 && presence.UnitFamily != 6) {
			continue
		}
		units[presence.ClientId] = append(units[presence.ClientId], &ctfSampledUnit{
			countryCode: presence.CountryCode, unit: *presence.Unit, family: presence.UnitFamily, lastSampleIndex: presence.LastSampleIndex,
		})
	}
	return units
}

// ctfResolveProviderUnit settles a provider's unit for the census. The unit is
// the prefix pinned in the epoch's window. When its family cannot be read from
// the provider's live addresses, as after the address changed, it is the
// family the samples recorded for that prefix. Without a pin, the unit is the
// latest one the samples recorded in the provider's end country, else in any
// country, as the sampler read it from the same pins.
func ctfResolveProviderUnit(egress *model.CtfEgress, sampled []*ctfSampledUnit, countryCode string) *model.CtfEgress {
	resolved := &model.CtfEgress{}
	if egress != nil {
		resolved.Addresses = egress.Addresses
		if egress.Unit != nil {
			unit := *egress.Unit
			resolved.Unit = &unit
			resolved.UnitFamily = egress.UnitFamily
		}
	}
	if resolved.Unit != nil && resolved.UnitFamily == 0 {
		for _, unit := range sampled {
			if unit.unit == *resolved.Unit {
				resolved.UnitFamily = unit.family
				break
			}
		}
		return resolved
	}
	if resolved.Unit != nil || len(sampled) == 0 {
		return resolved
	}
	latest := slices.MaxFunc(sampled, func(a, b *ctfSampledUnit) int {
		aHome, bHome := a.countryCode == countryCode, b.countryCode == countryCode
		switch {
		case aHome != bHome:
			if aHome {
				return 1
			}
			return -1
		case a.lastSampleIndex != b.lastSampleIndex:
			return a.lastSampleIndex - b.lastSampleIndex
		}
		return slices.Compare(b.unit[:], a.unit[:])
	})
	unit := latest.unit
	resolved.Unit = &unit
	resolved.UnitFamily = latest.family
	return resolved
}

// The record an extender member is judged by: its provider client's record
// that is active at the end, the lowest extender id, else its lowest record.
func ctfExtenderRecord(records []*model.CtfExtenderBoundary) *model.CtfExtenderBoundary {
	var chosen *model.CtfExtenderBoundary
	for _, record := range records {
		switch {
		case chosen == nil,
			record.Active && !chosen.Active,
			record.Active == chosen.Active && record.ExtenderId.Less(chosen.ExtenderId):
			chosen = record
		}
	}
	return chosen
}

// stCtfColdkeys selects the payout coldkey of each member client as the
// provider pool's rows select it (E4): the client's own consent, else its
// network's, else its network's hotkey delegation, from the signed roster
// when one is configured, else the retained consents.
func stCtfColdkeys(ctx context.Context, cfg *StConfig, authority *StPayoutEpochAuthority, clientIds []server.Id) (map[server.Id][32]byte, error) {
	coldkeys := map[server.Id][32]byte{}
	if len(clientIds) == 0 {
		return coldkeys, nil
	}
	if authority.PolicyHash != cfg.PolicyHash {
		return nil, fmt.Errorf("the payout authority of epoch %d is under another policy", authority.Epoch)
	}
	networks := model.GetCtfClientNetworks(ctx, clientIds)
	usages := []*model.StProviderUsage{}
	for _, clientId := range clientIds {
		if networkId, ok := networks[clientId]; ok {
			usages = append(usages, &model.StProviderUsage{ClientId: clientId, NetworkId: networkId})
		}
	}
	work, workConfigured, err := stLoadProviderWorkAuthority(ctx, cfg, authority)
	if err != nil {
		return nil, err
	}
	resolutions, err := stLedgerV2Wallets(ctx, cfg, work, workConfigured, authority, usages)
	if err != nil {
		return nil, err
	}
	for clientId, resolution := range resolutions {
		if resolution != nil && resolution.Coldkey != ([32]byte{}) {
			coldkeys[clientId] = resolution.Coldkey
		}
	}
	return coldkeys, nil
}

func stCtfCensusMetrics(census *model.CtfCensus, decision *ctfCensusDecision, excused int, lag time.Duration) {
	ctfMetrics.lastCensusEpoch.Set(float64(census.Epoch))
	ctfMetrics.censusMembers.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderMembers))
	ctfMetrics.censusMembers.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderMembers))
	ctfMetrics.censusCandidate.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderCandidates))
	ctfMetrics.censusCandidate.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderCandidates))
	ctfMetrics.censusUnits.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderUnits))
	ctfMetrics.censusUnits.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderUnits))
	excluded := map[int]map[int]int{model.CtfRoleProvider: {}, model.CtfRoleExtender: {}}
	for _, exclusion := range decision.Exclusions {
		excluded[exclusion.Role][exclusion.Reason] += exclusion.Count
	}
	for role, reasons := range excluded {
		for reason, label := range ctfReasonLabels {
			ctfMetrics.censusExcluded.WithLabelValues(ctfRoleLabel(role), label).Set(float64(reasons[reason]))
		}
	}
	ctfMetrics.censusTaken.Set(float64(census.SamplesTaken))
	ctfMetrics.censusExcused.Set(float64(excused))
	ctfMetrics.censusLag.Set(lag.Seconds())
}

// stCtfCensusRetention prunes, once an epoch's census is built, the samples,
// published census objects and census members older than ctf.yml keeps them.
func stCtfCensusRetention(ctx context.Context, cfg *StConfig, settings *model.CtfDataConfig, epoch uint64) {
	defer func() {
		if r := recover(); r != nil {
			glog.Errorf("[ctf]retention after epoch %d's census failed: %v\n", epoch, r)
		}
	}()
	deploymentKey := cfg.DeploymentKey()
	before := func(keep uint64) uint64 {
		if epoch+1 <= keep {
			return 0
		}
		return epoch + 1 - keep
	}
	if keepFrom := before(settings.PresenceRetentionEpochs()); 0 < keepFrom {
		model.RemoveCtfPresenceBefore(ctx, deploymentKey, cfg.NoId, keepFrom)
	}
	if keepFrom := before(settings.ObjectRetentionEpochs()); 0 < keepFrom {
		model.RemoveCtfCensusObjectsBefore(ctx, deploymentKey, cfg.NoId, keepFrom)
	}
	if keepFrom := before(settings.MemberRetentionEpochs()); 0 < keepFrom {
		model.RemoveCtfCensusMembersBefore(ctx, deploymentKey, cfg.NoId, keepFrom)
	}
}

// The longest outage one declaration may excuse: a day. A longer gap is a
// failure of capture the flag itself, which leaves the epoch's slots to carry,
// not one to excuse.
const ctfOutageMaxSpan = 24 * time.Hour

// StCtfDeclareOutage is the operator's explicit command declaring a
// platform-wide outage (§4.2): every presence sample whose quarter hour
// overlaps [startTime, endTime) is excused, in each census built after the
// declaration. A census already built keeps its excused samples: it is fixed.
// It records who declared it and why, and returns the epochs whose census it
// came too late for.
func StCtfDeclareOutage(ctx context.Context, startTime time.Time, endTime time.Time, reason string, declaredBy string) (server.Id, []uint64, error) {
	cfg, _, err := stRequire()
	if err != nil {
		return server.Id{}, nil, err
	}
	if !startTime.Before(endTime) {
		return server.Id{}, nil, fmt.Errorf("st ctf: an outage ends after it starts")
	}
	if ctfOutageMaxSpan < endTime.Sub(startTime) {
		return server.Id{}, nil, fmt.Errorf("st ctf: an outage of %s is longer than the %s one declaration may excuse", endTime.Sub(startTime), ctfOutageMaxSpan)
	}
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(declaredBy) == "" {
		return server.Id{}, nil, fmt.Errorf("st ctf: an outage declaration names its reason and who makes it")
	}
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" {
		return server.Id{}, nil, errStNotConfigured
	}
	outageId := model.DeclareCtfPresenceOutage(ctx, &model.CtfPresenceOutage{
		DeploymentKey: deploymentKey,
		NoId:          cfg.NoId,
		StartTime:     startTime,
		EndTime:       endTime,
		Reason:        reason,
		DeclaredBy:    declaredBy,
	})
	glog.Infof("[ctf]outage %s from %s to %s declared by %s: %s\n", outageId, startTime.UTC().Format(time.RFC3339), endTime.UTC().Format(time.RFC3339), declaredBy, reason)
	late := []uint64{}
	for _, census := range model.GetCtfCensuses(ctx, deploymentKey, cfg.NoId, 1, ^uint64(0)>>1) {
		if census.StartTime.Before(endTime) && startTime.Before(census.EndTime) {
			late = append(late, census.Epoch)
		}
	}
	return outageId, late, nil
}

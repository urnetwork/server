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
	"errors"
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

// The steps of capture the flag's data, for the task layer.
const (
	StCtfDataStepSample = "ctf_sample"
	StCtfDataStepCensus = "ctf_census"
)

// The alerts, each raised once per epoch and process: StCtfDataAlertCensus when
// an epoch's census cannot be built or published for a reason retrying will not
// change; StCtfDataAlertSamples when a census counts samples that were not
// taken, or taken without the providers' half, that no declared outage
// excuses; StCtfDataAlertPresence during the epoch, as soon as a sample is
// missed or finds no provider for a stale score export, while an outage can
// still be declared for it.
const (
	StCtfDataAlertCensus   = "ctf_census_build"
	StCtfDataAlertSamples  = "ctf_census_samples"
	StCtfDataAlertPresence = "ctf_presence_gap"
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

// StCtfDataStep is one planned capture the flag step.
type StCtfDataStep struct {
	Kind  string
	Epoch uint64
	// samples: the sample's index, the epoch's length in blocks, the block
	// and time it was planned at, and the estimated times of the first blocks
	// of the sample and of the next one: its quarter hour
	Index        int
	EpochBlocks  uint64
	HeadBlock    uint64
	PlannedTime  time.Time
	QuarterStart time.Time
	QuarterEnd   time.Time
	RunAt        time.Time
}

// The samples scheduled by this process, the last per deployment, so a sync
// turn plans each sample once; the task's run-once key keeps workers to one.
var ctfDataPlannedSample sync.Map

// StCtfDataStepScheduled records that a step's task is scheduled, so the next
// sync turn does not plan its sample again. A sample whose scheduling failed
// is planned again.
func StCtfDataStepScheduled(step *StCtfDataStep) {
	if step.Kind != StCtfDataStepSample {
		return
	}
	cfg := stConfig()
	if cfg == nil {
		return
	}
	ctfDataPlannedSample.Store(fmt.Sprintf("%s|%d", cfg.DeploymentKey(), cfg.NoId), fmt.Sprintf("%d|%d", step.Epoch, step.Index))
}

// The block the chain is at now, estimated from the finalized head the sync
// read and the time since it, so samples keep their quarter hours while
// finality lags or stalls.
func ctfEstimatedHead(state *StEpochState, now time.Time) uint64 {
	if state.HeadBlockTime.IsZero() || !now.After(state.HeadBlockTime) {
		return state.HeadBlock
	}
	blockSeconds := int64(stDefaultBlockSeconds)
	if cfg := stConfig(); cfg != nil && 0 < cfg.BlockSeconds {
		blockSeconds = cfg.BlockSeconds
	}
	return state.HeadBlock + uint64(now.Sub(state.HeadBlockTime)/(time.Duration(blockSeconds)*time.Second))
}

// The epochs whose census this process saw built or given up.
var ctfDataCensusConcluded sync.Map

// StCtfDataSync plans one sync turn's capture the flag steps: the current
// quarter hour's sample while presence is on, and the census of the epoch that
// has just ended while the census is on, from its first epoch. Nothing is due
// while ctf.yml leaves both off.
func StCtfDataSync(ctx context.Context, state *StEpochState) ([]*StCtfDataStep, error) {
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
	steps := []*StCtfDataStep{}
	if settings.Presence.Enabled {
		// A sample is taken in the second half of its quarter hour, so the
		// extender challenges of the quarter hour have run (X2).
		head := ctfEstimatedHead(state, now)
		if index, ok := model.CtfSampleIndex(state.EpochStartBlock, state.TEpochBlocks, head); ok {
			first := model.CtfSampleFirstBlock(state.EpochStartBlock, state.TEpochBlocks, index)
			next := model.CtfSampleFirstBlock(state.EpochStartBlock, state.TEpochBlocks, index+1)
			planned := fmt.Sprintf("%d|%d", state.Epoch, index)
			if previous, _ := ctfDataPlannedSample.Load(fmt.Sprintf("%s|%d", deploymentKey, cfg.NoId)); previous != planned && first+(next-first)/2 <= head {
				steps = append(steps, &StCtfDataStep{
					Kind:         StCtfDataStepSample,
					Epoch:        state.Epoch,
					Index:        index,
					EpochBlocks:  state.TEpochBlocks,
					HeadBlock:    head,
					PlannedTime:  now,
					QuarterStart: StEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, first),
					QuarterEnd:   StEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, next),
					RunAt:        now,
				})
			}
		}
	}
	if settings.Census.Enabled && 1 < state.Epoch {
		epoch := state.Epoch - 1
		concludedKey := fmt.Sprintf("%s|%d|%d", deploymentKey, cfg.NoId, epoch)
		if _, concluded := ctfDataCensusConcluded.Load(concludedKey); !concluded && settings.Census.FirstEpoch <= epoch {
			// the epoch ended when the current one started; past its capture
			// window an epoch without a census gets none, so a step that keeps
			// failing is not planned again (a built census is still published)
			ended := StEstimateBlockTime(state.HeadBlock, state.HeadBlockTime, state.EpochStartBlock)
			pastWindow := !state.HeadBlockTime.IsZero() && ctfCensusCaptureWindow+ctfCensusPlanGrace < now.Sub(ended)
			switch census := model.GetCtfCensus(ctx, deploymentKey, cfg.NoId, epoch); {
			case census != nil && census.PublishedTime != nil:
				ctfDataCensusConcluded.Store(concludedKey, true)
			case census == nil && pastWindow:
				ctfDataCensusConcluded.Store(concludedKey, true)
				stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d", epoch), "epoch %d has no census: its capture window passed at about %s without one, so every slot of it carries", epoch, ended.Add(ctfCensusCaptureWindow).UTC().Format(time.RFC3339))
			default:
				steps = append(steps, &StCtfDataStep{Kind: StCtfDataStepCensus, Epoch: epoch, RunAt: now})
			}
		}
	}
	return steps, nil
}

// StCtfDataRunStep runs one planned step. Each is idempotent.
func StCtfDataRunStep(ctx context.Context, step *StCtfDataStep) (*StPublishOutcome, error) {
	cfg, client, err := stRequire()
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errStNotConfigured
	}
	settings := model.CtfDataSettings()
	switch step.Kind {
	case StCtfDataStepSample:
		if !settings.Presence.Enabled {
			return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: "presence is off"}, nil
		}
		return stCtfTakeSample(ctx, cfg, step), nil
	case StCtfDataStepCensus:
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
	ctfDataMetrics.censuses.WithLabelValues(ctfCensusResultRetry).Inc()
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
		ctfDataCensusConcluded.Store(concludedKey, true)
		ctfDataMetrics.censuses.WithLabelValues(ctfCensusResultGivenUp).Inc()
		stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d", epoch), "epoch %d has no census: its end passed %s ago, past the %s its boundary can still be read in, so every slot of it carries", epoch, lag.Round(time.Second), ctfCensusCaptureWindow)
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
	// a sample that covers no block of a short epoch is never taken
	if authority.Start.Block < authority.End.Block {
		uncovered := model.CtfUncoveredSamples(authority.End.Block - authority.Start.Block)
		for i := range excused {
			excused[i] |= uncovered[i]
		}
	}
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

	// X1, X4–X6: the shared eligibility rules, the extender pool's E1 and
	// E3–E5, over every record of the epoch, so both pools judge an extender
	// alike; X3 from the judged record's activations
	records := model.GetStExtenderCensusRecords(ctx, authority.StartTime, authority.EndTime)
	rules, err := stExtenderEligibilityRules(ctx, cfg, client, authority, records)
	if err != nil {
		stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d extender eligibility", epoch), "epoch %d's census waits: %s", epoch, err)
		return stCtfCensusRetry(fmt.Sprintf("epoch %d: %s", epoch, err))
	}
	eligibilities, err := model.DecideExtenderEligibility(records, rules)
	if err != nil {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d: %s", epoch, err))
	}
	facts.Extenders = ctfExtenderEnds(eligibilities, model.GetCtfExtenderBoundaries(ctx, extenderIds, authority.StartTime, authority.EndTime), extenderIds)

	// P6: the operator's own networks, the one list E5 reads
	ownNetworks, err := readStExtenderOwnNetworks(ctx, cfg)
	if err != nil {
		stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d own networks", epoch), "epoch %d's census waits: %s", epoch, err)
		return stCtfCensusRetry(fmt.Sprintf("epoch %d: %s", epoch, err))
	}
	facts.OwnNetworks = ownNetworks.AllNetworkIds()

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
		ctfDataCensusConcluded.Store(concludedKey, true)
		ctfDataMetrics.censuses.WithLabelValues(ctfCensusResultGivenUp).Inc()
		stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d", epoch), "epoch %d's census does not encode, so it has none and every slot of it carries: %s", epoch, err)
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
	ctfDataMetrics.censuses.WithLabelValues(ctfCensusResultBuilt).Inc()
	stCtfCensusMetrics(census, decision, excused.Count(), captureTime.Sub(authority.EndTime))
	if missing, providersMissing := ctfMissingSamples(samples, &excused); 0 < missing || 0 < providersMissing {
		stLedgerV2Alert(cfg, StCtfDataAlertSamples, fmt.Sprintf("epoch %d", epoch), "epoch %d's census found nobody in %d samples that were not taken and found no provider in %d more taken while the score export was stale; no declared outage excuses them (%d excused), so they count against every miner", epoch, missing, providersMissing, excused.Count())
	}
	glog.Infof("[ctf]epoch %d census %s: %d candidates (%d providers in %d units, %d extenders in %d units) of %d members, %d samples taken, %d excused, required %d\n", epoch, ledger.Ref(census.CensusHash), census.Candidates, census.ProviderCandidates, census.ProviderUnits, census.ExtenderCandidates, census.ExtenderUnits, census.ProviderMembers+census.ExtenderMembers, len(samples), excused.Count(), facts.Required)
	outcome := stCtfPublishCensus(ctx, cfg, census)
	// after the publication, which the driver's commitment waits on
	stCtfCensusRetention(ctx, cfg, settings, epoch)
	return outcome
}

// The samples of an epoch that count against every miner: those not taken,
// and those taken with no providers' half, that are not excused.
func ctfMissingSamples(samples []*model.CtfPresenceSample, excused *model.CtfSampleSet) (missing int, providersMissing int) {
	var taken, providers model.CtfSampleSet
	for _, sample := range samples {
		taken.Set(sample.SampleIndex)
		if sample.ProviderSourceTime != nil {
			providers.Set(sample.SampleIndex)
		}
	}
	for i := range model.CtfSamplesPerEpoch {
		switch {
		case excused.Has(i):
		case !taken.Has(i):
			missing += 1
		case !providers.Has(i):
			providersMissing += 1
		}
	}
	return
}

// ctfExtenderMoved is X3's test that an extender stayed in one country through
// the epoch: its activations in the epoch were all in one country, and, when
// the record's country is its activations' own (it has no derived location),
// in the record's country. A derived location comes from the extender's
// co-signed pings, not GeoLite2, so it is not compared with the activations,
// which every extender sends at least daily; a change between them is the
// move the rule catches.
func ctfExtenderMoved(record *model.CtfExtenderBoundary) bool {
	countries := map[string]bool{}
	for _, countryCode := range record.ActivationCountryCodes {
		countries[countryCode] = true
	}
	if 1 < len(countries) {
		return true
	}
	if record.Derived {
		return false
	}
	for countryCode := range countries {
		if countryCode != record.CountryCode {
			return true
		}
	}
	return false
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
		ctfDataCensusConcluded.Store(concludedKey, true)
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d census %s already published", census.Epoch, ledger.Ref(census.CensusHash))}
	}
	objects := model.GetCtfCensusObjects(ctx, cfg.DeploymentKey(), cfg.NoId, census.Epoch)
	if _, ok := objects[census.CensusHash]; !ok {
		return stCtfCensusRetry(fmt.Sprintf("epoch %d census header is not stored", census.Epoch))
	}
	failed := func(reason string) *StPublishOutcome {
		if late := server.NowUtc().Sub(census.EndTime); ctfCensusCaptureWindow < late {
			stLedgerV2Alert(cfg, StCtfDataAlertCensus, fmt.Sprintf("epoch %d publication", census.Epoch), "epoch %d's census %s is built but not in the ledger object store %s after its end, so it cannot be committed: %s", census.Epoch, ledger.Ref(census.CensusHash), late.Round(time.Second), reason)
		}
		return stCtfCensusRetry(reason)
	}
	for _, ref := range slices.SortedFunc(maps.Keys(objects), func(a, b [32]byte) int { return slices.Compare(a[:], b[:]) }) {
		if ref == census.CensusHash {
			continue
		}
		if _, err := PutStLedgerV2Object(ctx, objects[ref]); err != nil {
			return failed(fmt.Sprintf("epoch %d census object %s: %s", census.Epoch, ledger.Ref(ref), err))
		}
	}
	if _, err := PutStLedgerV2Object(ctx, objects[census.CensusHash]); err != nil {
		return failed(fmt.Sprintf("epoch %d census header: %s", census.Epoch, err))
	}
	model.SetCtfCensusPublished(ctx, cfg.DeploymentKey(), cfg.NoId, census.Epoch, server.NowUtc())
	ctfDataCensusConcluded.Store(concludedKey, true)
	ctfDataMetrics.censuses.WithLabelValues(ctfCensusResultPublished).Inc()
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

// ctfResolveProviderUnit settles a provider's unit for the census: the prefix
// the verify head model pinned in the epoch's window, its family read from the
// provider's live addresses or, after the address changed, from the samples
// that saw the pin. Without a pin, the latest unit the samples recorded, in
// the provider's end country first, as the sampler read it from the same pins
// or, for a provider the model cannot pin, from its live address; else the
// prefix of its live address at the capture.
func ctfResolveProviderUnit(egress *model.CtfEgress, sampled []*ctfSampledUnit, countryCode string) *model.CtfEgress {
	resolved := &model.CtfEgress{}
	if egress != nil {
		resolved.Addresses = egress.Addresses
	}
	if egress != nil && egress.Pinned && egress.Unit != nil {
		unit := *egress.Unit
		resolved.Unit, resolved.UnitFamily, resolved.Pinned = &unit, egress.UnitFamily, true
		if resolved.UnitFamily == 0 {
			for _, sampledUnit := range sampled {
				if sampledUnit.unit == unit {
					resolved.UnitFamily = sampledUnit.family
					break
				}
			}
		}
		return resolved
	}
	if 0 < len(sampled) {
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
		resolved.Unit, resolved.UnitFamily = &unit, latest.family
		return resolved
	}
	if egress != nil && egress.Unit != nil {
		unit := *egress.Unit
		resolved.Unit, resolved.UnitFamily = &unit, egress.UnitFamily
	}
	return resolved
}

// The extender members' facts: each member's provider client judged by its
// judged record under the shared eligibility rules (X1, X4–X6), with the
// country of that record and its activations from the boundary read (X3). A
// member whose client has no record of the epoch has no facts, so it is
// inactive.
func ctfExtenderEnds(eligibilities model.ExtenderEligibilities, clientIdBoundaries map[server.Id][]*model.CtfExtenderBoundary, extenderClientIds []server.Id) map[server.Id]*ctfExtenderEnd {
	clientIdExtenderEnds := map[server.Id]*ctfExtenderEnd{}
	for _, clientId := range extenderClientIds {
		eligibility := eligibilities.Judged(clientId)
		if eligibility == nil {
			continue
		}
		networkId := eligibility.Record.NetworkId
		extenderEnd := &ctfExtenderEnd{
			ExtenderId: eligibility.Record.ExtenderId,
			NetworkId:  &networkId,
			Active:     eligibility.Active(),
			Duplicate:  eligibility.Duplicate(),
			HeadBound:  eligibility.HeadBound(),
			Coldkey:    eligibility.Coldkey,
			OwnNetwork: eligibility.OwnNetwork(),
		}
		if eligibility.Active() {
			unit := eligibility.Unit
			extenderEnd.Egress = &model.CtfEgress{Unit: &unit, UnitFamily: int(eligibility.Family)}
		}
		for _, boundary := range clientIdBoundaries[clientId] {
			if boundary.ExtenderId == eligibility.Record.ExtenderId {
				extenderEnd.CountryCode = boundary.CountryCode
				extenderEnd.ActivatedElsewhere = ctfExtenderMoved(boundary)
			}
		}
		clientIdExtenderEnds[clientId] = extenderEnd
	}
	return clientIdExtenderEnds
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
	ctfDataMetrics.lastCensusEpoch.Set(float64(census.Epoch))
	ctfDataMetrics.censusMembers.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderMembers))
	ctfDataMetrics.censusMembers.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderMembers))
	ctfDataMetrics.censusCandidate.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderCandidates))
	ctfDataMetrics.censusCandidate.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderCandidates))
	ctfDataMetrics.censusUnits.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(census.ProviderUnits))
	ctfDataMetrics.censusUnits.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(census.ExtenderUnits))
	excluded := map[int]map[int]int{model.CtfRoleProvider: {}, model.CtfRoleExtender: {}}
	for _, exclusion := range decision.Exclusions {
		excluded[exclusion.Role][exclusion.Reason] += exclusion.Count
	}
	for role, reasons := range excluded {
		for reason, label := range ctfReasonLabels {
			ctfDataMetrics.censusExcluded.WithLabelValues(ctfRoleLabel(role), label).Set(float64(reasons[reason]))
		}
	}
	ctfDataMetrics.censusTaken.Set(float64(census.SamplesTaken))
	ctfDataMetrics.censusExcused.Set(float64(excused))
	ctfDataMetrics.censusLag.Set(lag.Seconds())
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

// StCtfCensus is an epoch's census as the driver commits it and builds the
// epoch's ledger from it (sn ctf.CensusInput, ctf.EpochInput.Coldkeys).
type StCtfCensus struct {
	Epoch uint64
	// sha256 of the header, what commitCtfCensus commits
	Hash ledger.Ref
	// the header, the row chunks, the exclusion object and the countries
	// object
	Objects ledger.Objects
	// every candidate's payout coldkey (P5); never published
	Coldkeys map[[16]byte][32]byte
	// every object is in the ledger object store
	Published bool
}

// ErrStCtfCensusObjectsGone is a census whose stored objects retention has
// removed, once a finalized root covered its epoch. The ledger object store
// keeps them under the census hash.
var ErrStCtfCensusObjectsGone = errors.New("st ctf: the census's stored objects are gone; read them from the ledger object store")

// GetStCtfCensus returns the census of an epoch for the configured operator,
// nil while it is not built.
func GetStCtfCensus(ctx context.Context, epoch uint64) (*StCtfCensus, error) {
	cfg, _, err := stRequire()
	if err != nil {
		return nil, err
	}
	deploymentKey := cfg.DeploymentKey()
	if deploymentKey == "" {
		return nil, errStNotConfigured
	}
	census := model.GetCtfCensus(ctx, deploymentKey, cfg.NoId, epoch)
	if census == nil {
		return nil, nil
	}
	stored := model.GetCtfCensusObjects(ctx, deploymentKey, cfg.NoId, epoch)
	if _, ok := stored[census.CensusHash]; !ok {
		return nil, fmt.Errorf("%w: epoch %d census %s", ErrStCtfCensusObjectsGone, epoch, ledger.Ref(census.CensusHash))
	}
	objects := ledger.Objects{}
	for ref, data := range stored {
		objects[ledger.Ref(ref)] = data
	}
	coldkeys := map[[16]byte][32]byte{}
	for clientId, coldkey := range model.GetCtfCensusColdkeys(ctx, deploymentKey, cfg.NoId, epoch) {
		coldkeys[[16]byte(clientId)] = coldkey
	}
	return &StCtfCensus{Epoch: epoch, Hash: ledger.Ref(census.CensusHash), Objects: objects, Coldkeys: coldkeys, Published: census.PublishedTime != nil}, nil
}

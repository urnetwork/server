package controller

// Capture the flag's presence sampler (sn CAPTURETHEFLAG.md §4.2,
// model/ctf_presence_model.go): each quarter hour of an epoch, one sample of
// every provider and extender of the operator's pool.
//
//   - Providers (P1): the score export's Online set, which the export publishes
//     when presence is on. A provider in it flagged Public is an active,
//     top-level, connected provider with a Public provide key and a valid
//     location that passes ClientScore.Online, in the country it is published
//     under. A set older than model.CtfPresenceProvidersMaxAge is a stalled
//     export: the sample then finds no provider, and the presence alert says
//     so while an outage can still be declared.
//   - Extenders (X2): every record that is active and unrevoked with an active
//     address whose challenge passed in the sample's quarter hour, in the
//     record's country, counted for its provider client.
//
// Each present provider's unit is read, and pinned where nothing pinned it,
// in the epoch's window of the verify head model, as the SN Top board pins
// it, so the census finds every provider's prefix of the epoch. The sample is
// written once per index: a retry, or a second worker, changes nothing.

import (
	"context"
	"fmt"
	"time"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The window a challenge must have passed in for an extender to be present in a
// sample whose quarter hour is not known: the quarter hour before it.
const ctfExtenderChallengeWindow = 15 * time.Minute

// A sample whose quarter hour is not known runs at most this late after it was
// planned. Later, its quarter hour has passed and the next sample covers the
// miners instead.
const ctfSampleMaxDelay = 15 * time.Minute

// stCtfTakeSample takes one sample of the epoch, unless it is taken. Its
// quarter hour runs from the estimated time of its first block to the next
// sample's; the sync plans it in the second half, so the quarter hour's
// extender challenges have run.
func stCtfTakeSample(ctx context.Context, cfg *StConfig, step *StCtfDataStep) *StPublishOutcome {
	deploymentKey := cfg.DeploymentKey()
	if model.HasCtfPresenceSample(ctx, deploymentKey, cfg.NoId, step.Epoch, step.Index) {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d sample %d already taken", step.Epoch, step.Index)}
	}
	now := server.NowUtc()
	quarterStart, quarterEnd := step.QuarterStart, step.QuarterEnd
	if quarterStart.IsZero() || quarterEnd.IsZero() || !quarterStart.Before(quarterEnd) {
		quarterStart, quarterEnd = now.Add(-ctfExtenderChallengeWindow), step.PlannedTime.Add(ctfSampleMaxDelay)
	}
	if quarterEnd.Before(now) {
		ctfDataMetrics.samples.WithLabelValues(ctfSampleResultLate).Inc()
		stLedgerV2Alert(cfg, StCtfDataAlertPresence, fmt.Sprintf("epoch %d", step.Epoch), "epoch %d sample %d was not taken: its quarter hour ended before it ran, so it counts against every miner unless an outage is declared for it (bringyourctl st ctf-outage) before the epoch's census", step.Epoch, step.Index)
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d sample %d is late; its quarter hour ended at %s", step.Epoch, step.Index, quarterEnd.Format(time.RFC3339))}
	}

	sample := &model.CtfPresenceSample{
		DeploymentKey: deploymentKey,
		NoId:          cfg.NoId,
		Epoch:         step.Epoch,
		SampleIndex:   step.Index,
		SampleTime:    now,
		HeadBlock:     step.HeadBlock,
	}
	observations := []*model.CtfPresenceObservation{}

	providers, err := model.ReadCtfPresenceProviders(ctx)
	if err != nil {
		glog.Infof("[ctf]epoch %d sample %d: the export's online providers are unreadable: %s\n", step.Epoch, step.Index, err)
	}
	if providers != nil && now.Sub(providers.MeasuredAt) <= model.CtfPresenceProvidersMaxAge {
		measuredAt := providers.MeasuredAt
		sample.ProviderSourceTime = &measuredAt
		// P1: online, with a Public provide key
		public := []*model.CtfPresenceProvider{}
		clientIds := []server.Id{}
		for _, provider := range providers.Providers {
			if provider.Public {
				public = append(public, provider)
				clientIds = append(clientIds, provider.ClientId)
			}
		}
		egresses := model.GetCtfProviderEgress(ctx, clientIds, VerifySettings(), step.Epoch, true)
		for _, provider := range public {
			observation := &model.CtfPresenceObservation{Role: model.CtfRoleProvider, ClientId: provider.ClientId, CountryCode: provider.CountryCode}
			if egress := egresses[provider.ClientId]; egress != nil && egress.Unit != nil {
				observation.Unit = egress.Unit
				observation.UnitFamily = egress.UnitFamily
			}
			observations = append(observations, observation)
		}
		sample.Providers = len(public)
	} else {
		age := "absent"
		if providers != nil {
			age = now.Sub(providers.MeasuredAt).Round(time.Second).String() + " old"
		}
		glog.Infof("[ctf]epoch %d sample %d finds no provider: the export's online set is %s\n", step.Epoch, step.Index, age)
		stLedgerV2Alert(cfg, StCtfDataAlertPresence, fmt.Sprintf("epoch %d", step.Epoch), "epoch %d sample %d found no provider: the score export's online set is %s. If providers were online, declare the outage (bringyourctl st ctf-outage) before the epoch's census, or the sample counts against every provider", step.Epoch, step.Index, age)
	}

	// X2: active and unrevoked, with an address whose challenge passed in the
	// sample's quarter hour
	extenders := 0
	for _, presence := range model.GetCtfExtenderPresence(ctx, quarterStart) {
		egress := model.CtfExtenderEgress(presence.Ipv4, presence.Ipv6, VerifySettings())
		observations = append(observations, &model.CtfPresenceObservation{
			Role:        model.CtfRoleExtender,
			ClientId:    presence.ClientId,
			CountryCode: presence.CountryCode,
			Unit:        egress.Unit,
			UnitFamily:  egress.UnitFamily,
		})
		extenders += 1
	}
	sample.Extenders = extenders

	if !model.RecordCtfPresenceSample(ctx, sample, observations) {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d sample %d already taken", step.Epoch, step.Index)}
	}
	// the sample before, unless it covers no block of a short epoch
	var uncovered model.CtfSampleSet
	if 0 < step.EpochBlocks {
		uncovered = model.CtfUncoveredSamples(step.EpochBlocks)
	}
	if previous := step.Index - 1; 0 <= previous && !uncovered.Has(previous) &&
		!model.HasCtfPresenceSample(ctx, deploymentKey, cfg.NoId, step.Epoch, previous) {
		stLedgerV2Alert(cfg, StCtfDataAlertPresence, fmt.Sprintf("epoch %d", step.Epoch), "epoch %d sample %d was never taken: it counts against every miner unless an outage is declared for it (bringyourctl st ctf-outage) before the epoch's census", step.Epoch, previous)
	}
	result := ctfSampleResultTaken
	if sample.ProviderSourceTime == nil {
		result = ctfSampleResultNoProviders
	}
	ctfDataMetrics.samples.WithLabelValues(result).Inc()
	ctfDataMetrics.lastSample.Set(float64(now.Unix()))
	ctfDataMetrics.samplePresent.WithLabelValues(ctfRoleLabel(model.CtfRoleProvider)).Set(float64(sample.Providers))
	ctfDataMetrics.samplePresent.WithLabelValues(ctfRoleLabel(model.CtfRoleExtender)).Set(float64(sample.Extenders))
	if sample.ProviderSourceTime != nil {
		ctfDataMetrics.sampleSourceAge.Set(now.Sub(*sample.ProviderSourceTime).Seconds())
	}
	return &StPublishOutcome{Status: model.StPublishStatusConfirmed, Reason: fmt.Sprintf("epoch %d sample %d: %d providers, %d extender records present", step.Epoch, step.Index, sample.Providers, sample.Extenders)}
}

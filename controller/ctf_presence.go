package controller

// Capture the flag's presence sampler (sn CAPTURETHEFLAG.md §4.2,
// model/ctf_presence_model.go): each quarter hour of an epoch, one sample of
// every provider and extender of the operator's pool.
//
//   - Providers (P1): the score export's Online set, which the export publishes
//     when presence is on. A provider in it is an active, top-level, connected
//     provider with a Public provide key and a valid location that passes
//     ClientScore.Online, in the country it is published under. A set older
//     than model.CtfPresenceProvidersMaxAge is a stalled export: the sample
//     then finds no provider, and the census alert says so.
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
// sample: the quarter hour before it.
const ctfExtenderChallengeWindow = 15 * time.Minute

// A sample runs at most this late after it was planned. Later, its quarter
// hour has passed and the next sample covers the miners instead.
const ctfSampleMaxDelay = 15 * time.Minute

// stCtfTakeSample takes one sample of the epoch, unless it is taken.
func stCtfTakeSample(ctx context.Context, cfg *StConfig, step *StCtfDataStep) *StPublishOutcome {
	deploymentKey := cfg.DeploymentKey()
	if model.HasCtfPresenceSample(ctx, deploymentKey, cfg.NoId, step.Epoch, step.Index) {
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d sample %d already taken", step.Epoch, step.Index)}
	}
	now := server.NowUtc()
	if ctfSampleMaxDelay < now.Sub(step.PlannedTime) {
		ctfDataMetrics.samples.WithLabelValues(ctfSampleResultLate).Inc()
		return &StPublishOutcome{Status: model.StPublishStatusSkipped, Reason: fmt.Sprintf("epoch %d sample %d is %s late; its quarter hour has passed", step.Epoch, step.Index, now.Sub(step.PlannedTime).Round(time.Second))}
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
		clientIds := make([]server.Id, len(providers.Providers))
		for i, provider := range providers.Providers {
			clientIds[i] = provider.ClientId
		}
		egresses := model.GetCtfProviderEgress(ctx, clientIds, VerifySettings(), step.Epoch, true)
		for _, provider := range providers.Providers {
			observation := &model.CtfPresenceObservation{Role: model.CtfRoleProvider, ClientId: provider.ClientId, CountryCode: provider.CountryCode}
			if egress := egresses[provider.ClientId]; egress != nil && egress.Unit != nil {
				observation.Unit = egress.Unit
				observation.UnitFamily = egress.UnitFamily
			}
			observations = append(observations, observation)
		}
		sample.Providers = len(providers.Providers)
	} else {
		age := "none"
		if providers != nil {
			age = now.Sub(providers.MeasuredAt).Round(time.Second).String()
		}
		glog.Infof("[ctf]epoch %d sample %d finds no provider: the export's online set is %s old\n", step.Epoch, step.Index, age)
	}

	extenders := 0
	for _, presence := range model.GetCtfExtenderPresence(ctx, now.Add(-ctfExtenderChallengeWindow)) {
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

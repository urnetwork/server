package model

import (
	"context"

	"github.com/urnetwork/server"
)

// CollectArinShadowCurrentPublic joins the entire read-only public cohort to
// one committed score generation and exact live owners. The supplied resolver
// must cover the independently attested running handler population; missing
// owners remain unknown rows. A local-process resolver alone is not a fleet
// authority. This function never installs itself, opens a transport endpoint,
// enables policy, writes locations or reconnects clients.
func CollectArinShadowCurrentPublic(ctx context.Context, recorder *server.ArinShadowRecorder,
	snapshot *ArinShadowScoreSnapshot, required []string, owners server.ArinShadowCaptureOwnerReader,
) (server.ArinShadowCaptureReport, error) {
	if ctx == nil || ctx.Err() != nil || recorder == nil || snapshot == nil || owners == nil || !snapshot.Validate(ctx) {
		return server.ArinShadowCaptureReport{}, server.ErrArinShadowInput
	}
	bounded, cancel := context.WithTimeout(ctx, server.ArinShadowCaptureMaxAge)
	defer cancel()
	var collector *server.ArinShadowCaptureCollector
	var source server.ArinShadowCaptureCohort
	var lastProvider server.Id
	var sequence uint64
	cohort, err := StreamArinShadowPublicCohort(bounded, func(public ArinShadowPublicCohort) error {
		source = server.ArinShadowCaptureCohort{GenerationSHA256: snapshot.generation, ObservedAt: public.ObservedAt, Providers: public.Providers, Connections: public.Connections}
		var openErr error
		collector, openErr = recorder.NewCurrentCaptureCollector(bounded, source, required)
		return openErr
	}, func(rows []ArinShadowPublicConnection) error {
		page := server.ArinShadowCapturePage{GenerationSHA256: snapshot.generation, Sequence: sequence, Records: make([]server.ArinShadowCaptureRecord, len(rows))}
		for i, row := range rows {
			record := server.ArinShadowCaptureRecord{ConnectionId: row.ConnectionId, HandlerId: row.HandlerId}
			if row.ClientId != lastProvider {
				member := snapshot.member(row.ClientId, row.ProviderConnections)
				record.Provider = &member
				lastProvider = row.ClientId
			}
			page.Records[i] = record
		}
		if consumeErr := collector.Consume(page, owners, ReadArinShadowCaptureFacts); consumeErr != nil {
			return consumeErr
		}
		sequence++
		return nil
	})
	if collector == nil {
		return server.ArinShadowCaptureReport{}, server.ErrArinShadowInput
	}
	complete := err == nil && cohort.Complete && bounded.Err() == nil && snapshot.Validate(bounded)
	report, finishErr := collector.Finish(source, complete)
	report.NativeSourceStartedAt = snapshot.sourceStartedAt
	report.NativeSourceCompletedAt = snapshot.sourceCompletedAt
	report.NativePublishedAt = snapshot.publishedAt
	if err != nil || finishErr != nil || !complete {
		return report, server.ErrArinShadowInput
	}
	return report, nil
}

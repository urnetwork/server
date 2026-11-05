// A reachable, previously qualified host can still contain an unknown old run.
// Only its authenticated current physical observation permits a new job clock.
package controller

import (
	"context"
	"errors"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/competitionagent"
)

// Internal capability leaves historical evaluators and their Store contract
// unchanged. Availability never means that an old run can be executed again.
type competitionExecutorAvailability interface {
	Available(context.Context) (bool, error)
}

// This observation does not alter any run fence, execution identity, deadline or host
// qualification. Occupied is healthy control transport but not fresh admission.
func (self *competitionAgentEvaluator) Available(ctx context.Context) (bool, error) {
	metric := competitionExecutorPhysicalAvailable.WithLabelValues(self.host.HostId)
	metric.Set(-1)
	competitionExecutorPhysicalObservedAt.WithLabelValues(self.host.HostId).Set(float64(server.NowUtc().Unix()))
	response, err := self.transport.Exchange(ctx, competitionagent.Request{Operation: "availability"}, "", "")
	if err != nil {
		return false, err
	}
	if response.Identity != nil || response.ManifestSha256 != "" || response.RequestSha256 != "" || len(response.Files) != 0 || len(response.SelfCheck) != 0 || response.ExitCode != 0 || response.ErrorCode != "" {
		return false, errors.New("executor physical availability observation was incomplete")
	}
	switch response.State {
	case "available":
		metric.Set(1)
		return true, nil
	case "occupied":
		metric.Set(0)
		return false, nil
	default:
		return false, errors.New("executor physical availability is unavailable")
	}
}

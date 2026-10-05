package model

import (
	"context"
	"fmt"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/urnetwork/server"
)

// Ordinary discovery can borrow Speed/Online when current subscriber facts are
// unavailable. One budget covers every Quality batch and refill in the request;
// a slow primary must not restart that budget once per candidate or page.
const providerQualityValidationTimeout = 250 * time.Millisecond

var providerQualityValidationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
	Name:    "urnetwork_findproviders2_subscriber_validation_seconds",
	Help:    "Current subscriber validation within one provider discovery request, including database acquisition and query waits",
	Buckets: prometheus.DefBuckets,
}, []string{"outcome"})

var providerQualityValidationInflight = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "urnetwork_findproviders2_subscriber_validation_inflight",
	Help: "Provider discovery requests currently validating subscriber facts on the primary",
})

func init() {
	prometheus.MustRegister(providerQualityValidationSeconds, providerQualityValidationInflight)
}

type providerQualityValidation struct {
	parent      context.Context
	ctx         context.Context
	cancel      context.CancelFunc
	unavailable error
}

func (v *providerQualityValidation) close() {
	if v.cancel != nil {
		v.cancel()
	}
}

func (v *providerQualityValidation) read(ids []server.Id) (excluded, risky map[server.Id]bool, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}
	if v.parent.Err() != nil {
		return nil, nil, v.parent.Err()
	}
	if v.unavailable != nil {
		return nil, nil, v.unavailable
	}
	if v.ctx == nil {
		v.ctx, v.cancel = context.WithTimeout(v.parent, providerQualityValidationTimeout)
	}
	started := time.Now()
	providerQualityValidationInflight.Inc()
	defer func() {
		// Db may raise an acquisition/ping failure instead of returning it.
		// Those failures also make this native source unavailable, never empty.
		if failure := recover(); failure != nil {
			if failureErr, ok := failure.(error); ok {
				err = failureErr
			} else {
				err = fmt.Errorf("subscriber validation: %v", failure)
			}
		}
		outcome := "ok"
		if err != nil {
			v.unavailable = err
			outcome = "unavailable"
			if v.parent.Err() != nil {
				err, outcome = v.parent.Err(), "canceled"
			}
		}
		providerQualityValidationInflight.Dec()
		providerQualityValidationSeconds.WithLabelValues(outcome).Observe(time.Since(started).Seconds())
	}()
	return getProviderSubscriberExclusions(v.ctx, ids)
}

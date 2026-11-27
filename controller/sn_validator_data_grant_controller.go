package controller

// The SN25 validator data grant, asked for by each admitted rate class mint
// (POST /sn/validator-class; model.GrantValidatorData): one counter by outcome,
// and one log line per grant created. The mint is answered whatever the
// outcome. A grant that fails is asked for again by the validator's next mint,
// a few minutes later.

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The outcomes of GrantSnValidatorData.
const (
	// a new grant for the validator's network
	SnValidatorDataGrantCreated = "created"
	// the validator's hotkey holds an active grant
	SnValidatorDataGrantSkipped = "skipped"
	// pro.yml validator_grant grants nothing
	SnValidatorDataGrantDisabled = "disabled"
	// nothing was granted; the next mint asks again
	SnValidatorDataGrantFailed = "failed"
)

var snValidatorDataGrants = prometheus.NewCounterVec(prometheus.CounterOpts{
	Namespace: "urnetwork",
	Subsystem: "sn",
	Name:      "validator_data_grants_total",
	Help:      "SN25 validator data grants asked for by admitted rate class mints at POST /sn/validator-class, by outcome: created, a new grant for the validator's network; skipped, the validator's hotkey holds an active grant; disabled, pro.yml validator_grant grants nothing; failed, nothing was granted and the next mint asks again",
}, []string{"outcome"})

func init() {
	prometheus.MustRegister(snValidatorDataGrants)
	// each outcome's series starts at 0, so an increase sees its first count:
	// a validator's grant is created about once a month
	for _, outcome := range []string{SnValidatorDataGrantCreated, SnValidatorDataGrantSkipped, SnValidatorDataGrantDisabled, SnValidatorDataGrantFailed} {
		snValidatorDataGrants.WithLabelValues(outcome)
	}
}

// Asks for the data grant of the admitted validator with the hotkey, for the
// minting client's network, at the mint's time, and returns the outcome. Never
// raises. The log names the validator by a prefix of its receipt id, never by
// its hotkey.
func GrantSnValidatorData(ctx context.Context, networkId server.Id, hotkey [32]byte, now time.Time) (outcome string) {
	settings := model.Pro().ValidatorDataGrant
	outcome = SnValidatorDataGrantFailed
	defer func() {
		snValidatorDataGrants.WithLabelValues(outcome).Inc()
	}()
	if !settings.Enabled() {
		outcome = SnValidatorDataGrantDisabled
		return
	}
	if 0 < settings.Timeout {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, settings.Timeout)
		defer cancel()
	}
	validator := model.ValidatorDataGrantId(hotkey).String()[:8]
	server.HandleError(func() {
		grant, err := model.GrantValidatorData(ctx, networkId, hotkey, &settings, now)
		switch {
		case err != nil:
			glog.Infof("[sn]validator %s data grant to network %s failed: %s\n", validator, networkId, err)
		case grant == nil:
			outcome = SnValidatorDataGrantSkipped
		default:
			outcome = SnValidatorDataGrantCreated
			glog.Infof(
				"[sn]validator %s data grant created: network %s, %s until %s\n",
				validator,
				networkId,
				model.ByteCountHumanReadable(grant.BalanceByteCount),
				grant.EndTime.Format(time.RFC3339),
			)
		}
	}, func(err error) {
		glog.Infof("[sn]validator %s data grant to network %s failed: %s\n", validator, networkId, err)
	})
	return
}

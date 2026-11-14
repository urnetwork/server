package sidecart3

// §7.9's authorization log on T3 (TEAM-M2-19): server's real sequencer, model.ConfigLogSequencer, as the admission
// suite's AdmissionApi, which every T3 Net's config-log rows run on (NetSettings.ConfigLogApi) and the admission suite's
// T3 runs take (server/api/sidecar_admission_t3_test.go). Its ids are sidecar's, and a model.ConfigLogError is the
// suite's ConfigLogRefusal, as the store suite's factory maps it (model/config_log_model_test.go).
//
// One thing server's sequencer does not do, which ConfigLogSuiteSettings says so a row skips it here and logs it: it
// appends nothing its own check refuses, so a forged clause runs on T2 alone (plan-TEST-small-readings reading 6). A
// tick that activates a recovery window it judges as every reader does (ConfigLog.CheckTick; TEAM-M2-14, QD-745): one
// the window holds due lands, and an early one is refused where T2's FakeApi lands it for readers to refuse.

import (
	"context"
	"errors"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// Makes server's sequencer over a cluster on a clock, with the default settings: no append limits, admission cap,
// effects or hints, which need Postgres and Redis and no admission row reads.
func ConfigLogApi() sidecartest.AdmissionApiFactory {
	return ConfigLogApiWith(nil, nil)
}

// ConfigLogApi with what a test adds to the sequencer: edit changes its settings once the clock is set, as api's
// process sets the read cache's script (model.NewConfigLogRedisEntryCache), and advanceHead raises network_acl_head
// after each entry (model.AdvanceNetworkAclHead); both need the private stores. nil for neither.
func ConfigLogApiWith(
	edit func(settings *model.ConfigLogSettings),
	advanceHead func(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error),
) sidecartest.AdmissionApiFactory {
	return func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.AdmissionApi {
		settings := model.DefaultConfigLogSettings()
		settings.Clock = clock
		if edit != nil {
			edit(settings)
		}
		return &configLogApi{sequencer: model.NewConfigLogSequencer(cluster, settings, advanceHead)}
	}
}

// The admission suite's settings on server's sequencer, as the package comment says.
func ConfigLogSuiteSettings() *sidecartest.AdmissionSuiteSettings {
	settings := sidecartest.DefaultAdmissionSuiteSettings()
	settings.ForgedAppends = false
	return settings
}

type configLogApi struct {
	sequencer *model.ConfigLogSequencer
}

// A ConfigLogError as the suite's refusal.
func configLogApiError(err error) error {
	configLogError := &model.ConfigLogError{}
	if errors.As(err, &configLogError) {
		return &sidecartest.ConfigLogRefusal{Status: configLogError.Status, Tip: configLogError.Tip, Err: configLogError.Err}
	}
	return err
}

func (self *configLogApi) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
	return pin, configLogApiError(err)
}

func (self *configLogApi) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendBase(ctx, server.Id(networkId), &model.ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	return pin, configLogApiError(err)
}

func (self *configLogApi) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
	return pin, configLogApiError(err)
}

func (self *configLogApi) ReadConfigProjection(ctx context.Context, networkId sidecar.Id, after uint64) (*sidecar.ConfigProjection, error) {
	read, err := self.sequencer.Read(ctx, server.Id(networkId), after)
	if err != nil {
		return nil, configLogApiError(err)
	}
	return &sidecar.ConfigProjection{SignedGenesis: read.SignedGenesis, Entries: read.Entries, Rows: read.Rows, Tip: read.Tip, More: read.More}, nil
}

// Refused: server's sequencer appends only what its check takes.
func (self *configLogApi) SequenceAnyway(ctx context.Context, networkId sidecar.Id, kind uint32, body []byte) (*bridge.AuthPin, error) {
	return nil, errors.New("server's sequencer appends nothing its check refuses: a forged clause runs on T2 alone")
}

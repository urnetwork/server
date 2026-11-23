package connect

import (
	"errors"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/glog"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The connect service's config, in the config release. Optional: a missing
// file keeps every default.
const connectConfigResourceName = "connect.yml"

// The formats of the records connect writes (CPUOPT7.md §12). Readers from
// CPUOPT7 §12 on read both formats of each; a reader from before reads only
// gob. Each flag waits until every reader of its record runs a build that
// reads the typed format, and goes off before any reader rolls back below
// it. A connect run reads them from connect.yml once, at its start
// (exchangeSettingsForRun), so a change is a config release and a connect
// roll, and turning a flag off is its rollback.
type ExchangeRecordFormatSettings struct {
	// Whether nominations write the resident record (model.NetworkClientResident,
	// `ncr_<client id>`) in the typed format instead of gob. Connect, the api
	// and the taskworker read it. False, the default, writes gob.
	// `typed_resident_records` in connect.yml.
	TypedResidentRecords bool
	// Whether exchange connections write their header in the typed format
	// instead of gob: a dialer's header and an acceptor's echo. Only connect
	// reads it. False, the default, writes gob. `typed_exchange_headers` in
	// connect.yml.
	TypedExchangeHeaders bool
}

func DefaultExchangeRecordFormatSettings() *ExchangeRecordFormatSettings {
	return &ExchangeRecordFormatSettings{
		// gob until every reader runs CPUOPT7 §12 (owner, config release)
		TypedResidentRecords: false,
		TypedExchangeHeaders: false,
	}
}

// The format nominations write the resident record in.
func (self *ExchangeRecordFormatSettings) residentRecordFormat() model.NetworkClientResidentFormat {
	if self.TypedResidentRecords {
		return model.NetworkClientResidentFormatTyped
	}
	return model.NetworkClientResidentFormatGob
}

// The keys of connect.yml this file reads. A key it does not know is
// ignored, so a newer file is harmless to an older build.
type exchangeRecordFormatConfig struct {
	TypedResidentRecords *bool `yaml:"typed_resident_records"`
	TypedExchangeHeaders *bool `yaml:"typed_exchange_headers"`
}

// The defaults with connect.yml's record format keys over them, read once at
// the start of a run. A warp container sees only the config version it was
// created with, so a config release reaches connect with its next roll. A
// missing file or key keeps the default, gob. A file that cannot be read, or
// a key that is not a bool or is given twice, writes gob too and says so: gob
// is what every reader reads.
func exchangeRecordFormatSettingsFromConfig() *ExchangeRecordFormatSettings {
	settings := DefaultExchangeRecordFormatSettings()
	config, err := func() (*exchangeRecordFormatConfig, error) {
		resource, err := server.Config.SimpleResource(connectConfigResourceName)
		if errors.Is(err, server.ErrResourceNotFound) && !errors.Is(err, server.ErrResourceUnavailable) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		value, err := resource.BytesE()
		if err != nil {
			return nil, err
		}
		var config exchangeRecordFormatConfig
		if err := yaml.Unmarshal(value, &config); err != nil {
			return nil, err
		}
		return &config, nil
	}()
	if err != nil {
		glog.Errorf("[connect]%s is unreadable (%s); resident records and exchange headers are written as gob\n", connectConfigResourceName, err)
		return settings
	}
	if config != nil {
		if config.TypedResidentRecords != nil {
			settings.TypedResidentRecords = *config.TypedResidentRecords
		}
		if config.TypedExchangeHeaders != nil {
			settings.TypedExchangeHeaders = *config.TypedExchangeHeaders
		}
	}
	glog.Infof("[connect]resident records are %s and exchange headers are %s\n", settings.residentRecordFormat(), exchangeHeaderFormatName(settings.TypedExchangeHeaders))
	return settings
}

// The format label of exchange headers, as the counters name it.
func exchangeHeaderFormatName(typed bool) string {
	if typed {
		return "typed"
	}
	return "gob"
}

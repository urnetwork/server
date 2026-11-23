package connect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/urnetwork/server/model"
)

func exchangeRecordFormatConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("WARP_CONFIG_HOME", home)
	t.Setenv("WARP_ENV", "test")
	return home
}

// Both records are gob unless connect.yml turns each on. A file or key that
// cannot be read writes gob, what every reader reads; a key it does not know
// leaves the others read.
func TestExchangeRecordFormatSettingsFromConfig(t *testing.T) {
	home := exchangeRecordFormatConfigHome(t)
	path := filepath.Join(home, connectConfigResourceName)
	if settings := exchangeRecordFormatSettingsFromConfig(); settings.TypedResidentRecords || settings.TypedExchangeHeaders {
		t.Fatalf("without connect.yml: %+v", *settings)
	}
	for _, c := range []struct {
		config             string
		wantTypedResidents bool
		wantTypedHeaders   bool
	}{
		{config: "", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "# nothing set\n", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "typed_resident_records: true\n", wantTypedResidents: true, wantTypedHeaders: false},
		{config: "typed_exchange_headers: true\n", wantTypedResidents: false, wantTypedHeaders: true},
		{config: "typed_resident_records: true\ntyped_exchange_headers: true\n", wantTypedResidents: true, wantTypedHeaders: true},
		{config: "typed_resident_records: false\ntyped_exchange_headers: false\n", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "typed_resident_records: true\na_later_key: 7\n", wantTypedResidents: true, wantTypedHeaders: false},
		{config: "typed_resident_records:\ntyped_exchange_headers: true\n", wantTypedResidents: false, wantTypedHeaders: true},
		// unreadable: every record is gob
		{config: "typed_resident_records: true\ntyped_exchange_headers: maybe\n", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "typed_exchange_headers: true\ntyped_exchange_headers: true\n", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "- typed_exchange_headers\n", wantTypedResidents: false, wantTypedHeaders: false},
		{config: "typed_resident_records: [true\n", wantTypedResidents: false, wantTypedHeaders: false},
	} {
		if err := os.WriteFile(path, []byte(c.config), 0600); err != nil {
			t.Fatal(err)
		}
		settings := exchangeRecordFormatSettingsFromConfig()
		if settings.TypedResidentRecords != c.wantTypedResidents || settings.TypedExchangeHeaders != c.wantTypedHeaders {
			t.Errorf("%q: %+v, want typed resident records %t, typed exchange headers %t", c.config, *settings, c.wantTypedResidents, c.wantTypedHeaders)
		}
	}
}

// The connect run takes its record formats from connect.yml; the defaults,
// which embedders and tests start from, write gob.
func TestExchangeSettingsForRunReadsRecordFormats(t *testing.T) {
	defaults := DefaultExchangeSettings()
	if defaults.TypedResidentRecords || defaults.TypedExchangeHeaders || defaults.residentRecordFormat() != model.NetworkClientResidentFormatGob {
		t.Fatalf("the default settings write a typed record: %+v", defaults.ExchangeRecordFormatSettings)
	}
	home := exchangeRecordFormatConfigHome(t)
	if settings := exchangeSettingsForRun(RunOptions{Port: 443}); settings.TypedResidentRecords || settings.TypedExchangeHeaders {
		t.Fatalf("a run without connect.yml writes a typed record: %+v", settings.ExchangeRecordFormatSettings)
	}
	config := "typed_resident_records: true\ntyped_exchange_headers: true\n"
	if err := os.WriteFile(filepath.Join(home, connectConfigResourceName), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	settings := exchangeSettingsForRun(RunOptions{Port: 443})
	if !settings.TypedResidentRecords || !settings.TypedExchangeHeaders || settings.residentRecordFormat() != model.NetworkClientResidentFormatTyped {
		t.Fatalf("a run did not take connect.yml's record formats: %+v", settings.ExchangeRecordFormatSettings)
	}
}

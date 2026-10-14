// The serving ARIN reader's identity is observable in pushed metrics, so fleet
// adoption of a selected resource is measurable without host process access.
package server

import (
	"bytes"
	"net"
	"testing"

	"github.com/maxmind/mmdbwriter"
	"github.com/maxmind/mmdbwriter/mmdbtype"
	mmdb "github.com/oschwald/maxminddb-golang/v2"
	"github.com/prometheus/client_golang/prometheus"
)

const arinDatabaseBuildEpochMetricName = "urnetwork_arin_database_build_epoch_seconds"

// A synthetic policy-two ARIN database with a fixed build epoch over
// documentation space only.
func testArinEpochDatabase(t *testing.T, buildEpoch int64) *mmdb.Reader {
	t.Helper()
	w, err := mmdbwriter.New(mmdbwriter.Options{
		DatabaseType:            string(schemaTypeArinDb),
		BuildEpoch:              buildEpoch,
		IncludeReservedNetworks: true,
		RecordSize:              32,
		Description:             map[string]string{"en": "synthetic ARIN database"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, network, err := net.ParseCIDR("192.0.2.0/24")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Insert(network, mmdbtype.Map{
		"classifier_version": mmdbtype.Uint32(1), "quality_policy_version": mmdbtype.Uint32(2),
		"quality_state": mmdbtype.String("unknown"), "non_quality": mmdbtype.Bool(true), "risk": mmdbtype.Bool(false),
	}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := w.WriteTo(&out); err != nil {
		t.Fatal(err)
	}
	db, err := mmdb.OpenBytes(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	return db
}

// The gathered value of an unlabeled gauge in the pushed registry. Absence is
// reported separately from a zero value.
func gatheredArinEpochGauge(t *testing.T) (float64, bool) {
	t.Helper()
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != arinDatabaseBuildEpochMetricName {
			continue
		}
		metrics := family.GetMetric()
		if len(metrics) != 1 || metrics[0].GetGauge() == nil {
			t.Fatalf("%s has %d samples, want one unlabeled gauge", arinDatabaseBuildEpochMetricName, len(metrics))
		}
		return metrics[0].GetGauge().GetValue(), true
	}
	return 0, false
}

// Before the serving reader opens the gauge is present and zero, so a process
// that never classifies an address is distinguishable from a missing push.
func TestArinDatabaseBuildEpochGaugeIsZeroBeforeTheReaderOpens(t *testing.T) {
	previous := arinServingReader.Load()
	t.Cleanup(func() { arinServingReader.Store(previous) })

	arinServingReader.Store(nil)
	value, ok := gatheredArinEpochGauge(t)
	if !ok {
		t.Fatalf("%s is not registered in the pushed registry", arinDatabaseBuildEpochMetricName)
	}
	if value != 0 {
		t.Fatalf("unopened reader reports build epoch %v, want 0", value)
	}
}

// The gauge reports exactly the build epoch of the reader the lookups use, and
// follows the published reader rather than any selected or newer resource.
func TestArinDatabaseBuildEpochGaugeReportsTheServingReader(t *testing.T) {
	const firstBuildEpoch = 1700000000
	const secondBuildEpoch = 1700086400
	first := testArinEpochDatabase(t, firstBuildEpoch)
	second := testArinEpochDatabase(t, secondBuildEpoch)
	previous := arinServingReader.Load()
	t.Cleanup(func() {
		arinServingReader.Store(previous)
		first.Close()
		second.Close()
	})

	for _, c := range []struct {
		reader     *mmdb.Reader
		buildEpoch float64
	}{
		{reader: first, buildEpoch: firstBuildEpoch},
		{reader: second, buildEpoch: secondBuildEpoch},
	} {
		arinServingReader.Store(c.reader)
		value, ok := gatheredArinEpochGauge(t)
		if !ok {
			t.Fatalf("%s is not registered in the pushed registry", arinDatabaseBuildEpochMetricName)
		}
		if value != c.buildEpoch {
			t.Fatalf("serving reader built at %v reports build epoch %v", c.buildEpoch, value)
		}
		if lookupEpoch := float64(CurrentArinCauseReaderEpoch()); lookupEpoch != value {
			t.Fatalf("gauge %v disagrees with the serving reader epoch %v", value, lookupEpoch)
		}
	}
}

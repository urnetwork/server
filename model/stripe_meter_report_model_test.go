//go:build sidecar_pending

package model

// QUOTA-M6-03: stripe_meter_report's identifier and its versions, under -tags sidecar_pending, since
// the table is MG6's.

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

// §20.9's identifier, SHA-256(network_id || meter || usage_day || version), against vectors computed
// outside this code.
func TestStripeMeterIdentifierVectors(t *testing.T) {
	networkId, err := server.ParseId("00112233-4455-6677-8899-aabbccddeeff")
	connect.AssertEqual(t, err, nil)
	day := func(year int, month time.Month, dayOfMonth int) time.Time {
		return time.Date(year, month, dayOfMonth, 0, 0, 0, 0, time.UTC)
	}
	for _, vector := range []struct {
		meter    string
		day      time.Time
		version  int
		expected string
	}{
		{TeamMeterSeats, day(2026, time.November, 13), 1, "cce959463ac399a2aa9acffe994c2523fa2915a6783ec42304de616b2bceb701"},
		{TeamMeterTransferOverage, day(2026, time.November, 13), 1, "403af620d571758cc366020e8c3128cffaa92dac1ac386d0d7ddd229e14ff440"},
		{TeamMeterTransferOverage, day(2026, time.November, 13), 2, "d33716b42c9a81a4d7b040962ad6abaa2cd8f9e4e8d7e737d568387abf8090ed"},
		{TeamMeterStorageOverage, day(2027, time.January, 1), 7, "d1486b4954110e6fd838d067c2d5fa5b8d09b338ac72515a35c2ad66f2175193"},
	} {
		connect.AssertEqual(t, StripeMeterIdentifier(networkId, vector.meter, vector.day, vector.version), vector.expected)
		// any time inside the day is the day
		connect.AssertEqual(t, StripeMeterIdentifier(networkId, vector.meter, vector.day.Add(23*time.Hour), vector.version), vector.expected)
	}
}

// A day's first value is version 1, queued; the same value again writes nothing; a changed value is
// the next version, with its own identifier, the earlier kept.
func TestWriteStripeMeterRowsVersionsAChangedDay(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		networkId := server.NewId()
		day := time.Date(2026, time.November, 13, 0, 0, 0, 0, time.UTC)
		now := day.AddDate(0, 0, 1).Add(10 * time.Minute)
		write := func(values map[string]int64) (written []*StripeMeterReport) {
			t.Helper()
			server.Tx(ctx, func(tx server.PgTx) {
				var err error
				written, err = WriteStripeMeterRowsInTx(ctx, tx, networkId, day, values, now)
				connect.AssertEqual(t, err, nil)
			})
			return
		}
		written := write(map[string]int64{TeamMeterSeats: 7, TeamMeterTransferOverage: 3})
		connect.AssertEqual(t, len(written), 2)
		connect.AssertEqual(t, len(write(map[string]int64{TeamMeterSeats: 7, TeamMeterTransferOverage: 3})), 0)
		written = write(map[string]int64{TeamMeterSeats: 7, TeamMeterTransferOverage: 4})
		connect.AssertEqual(t, len(written), 1)
		connect.AssertEqual(t, written[0].Meter, TeamMeterTransferOverage)
		connect.AssertEqual(t, written[0].Version, 2)

		reports := GetStripeMeterReports(ctx, networkId, day)
		connect.AssertEqual(t, len(reports), 3)
		identifiers := map[string]bool{}
		for _, report := range reports {
			connect.AssertEqual(t, report.State, StripeMeterReportStateQueued)
			connect.AssertEqual(t, report.Attempts, 0)
			connect.AssertEqual(t, report.NextTime, now)
			connect.AssertEqual(t, report.Identifier, StripeMeterIdentifier(networkId, report.Meter, day, report.Version))
			identifiers[report.Identifier] = true
		}
		connect.AssertEqual(t, len(identifiers), 3)
		// the earlier version stays as it was
		connect.AssertEqual(t, reports[1].Meter, TeamMeterTransferOverage)
		connect.AssertEqual(t, reports[1].Version, 1)
		connect.AssertEqual(t, reports[1].Value, int64(3))
		connect.AssertEqual(t, reports[2].Value, int64(4))

		server.Tx(ctx, func(tx server.PgTx) {
			_, err := WriteStripeMeterRowsInTx(ctx, tx, networkId, day, map[string]int64{TeamMeterSeats: -1}, now)
			connect.AssertEqual(t, err != nil, true)
		})
	})
}

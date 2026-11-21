// The export's admission set for the SN Top board (sn_top_admission_model.go)
// goes through Redis: what the export publishes is what the fold reads, and
// with nothing published the fold reads no set. In the fixture's isolated
// Redis database.
package model

import (
	"context"
	"testing"
	"time"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/server"
)

func TestSnTopAdmissionPublishedSetReadsBack(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		none, err := ReadSnTopAdmission(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, none == nil, true)

		admitted, online, neither := server.NewId(), server.NewId(), server.NewId()
		ru := "ru"
		measuredAt := server.NowUtc().Truncate(time.Millisecond)
		publishSnTopAdmission(
			ctx,
			measuredAt,
			map[server.Id]map[server.Id]*ClientScore{
				server.NewId(): {
					admitted: {ClientId: admitted, PassesMinimums: map[string]bool{RankModeSpeed: true}, Online: true},
					online:   {ClientId: online, PassesMinimums: map[string]bool{}, Online: true},
					neither:  {ClientId: neither},
				},
			},
			map[server.Id]*clientScoreEgress{online: {publishedCountryCode: &ru}},
		)

		read, err := ReadSnTopAdmission(ctx)
		connect.AssertEqual(t, err, nil)
		connect.AssertEqual(t, read.MeasuredAt, measuredAt)
		connect.AssertEqual(t, read.Fresh(measuredAt.Add(time.Minute)), true)
		connect.AssertEqual(t, len(read.Providers), 3)
		connect.AssertEqual(t, read.Providers[admitted].Admitted, true)
		connect.AssertEqual(t, *read.Providers[online], SnTopAdmissionProvider{ClientId: online, CountryCode: "ru", Online: true})
		connect.AssertEqual(t, read.Providers[neither].Admitted || read.Providers[neither].Online, false)
	})
}

//go:build sidecar_pending

package dnshost

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/miekg/dns"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The site monitor's canary on Postgres: one serial and one journal row at canary.<root>, of kind canary;
// a value no canary may hold refused before the path; while restoring, ErrDnsRestoring and no serial.
func TestCanariesWriteThroughTheJournal(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		id := server.NewId().String()
		root := "c" + strings.ReplaceAll(id[len(id)-12:], "-", "") + ".example."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		canaries := NewCanaries()
		value := strconv.FormatInt(server.NowUtc().UnixMilli(), 10) + " site"
		serial, err := canaries.PutCanary(ctx, root, value, 60)
		if err != nil || serial == 0 {
			t.Fatalf("serial %d %v", serial, err)
		}
		read, err := model.GetDnsJournal(ctx, root, serial-1, 10)
		if err != nil || len(read.Rows) != 1 {
			t.Fatalf("journal %+v %v", read, err)
		}
		row := read.Rows[0].Upserts[0]
		if row.Owner != dnsservice.DnsCanaryOwner(root) || row.Type != dns.TypeTXT || row.Kind != dnsservice.DnsRecordKindCanary || row.Rdata[0] != `"`+value+`"` {
			t.Errorf("the row %+v", row)
		}
		if _, err := canaries.PutCanary(ctx, root, "no milliseconds", 60); !errors.Is(err, dnsservice.ErrDnsCanaryRefused) {
			t.Errorf("a bad value: %v", err)
		}
		if err := model.SetDnsRestoring(ctx, root, true); err != nil {
			t.Fatal(err)
		}
		if _, err := canaries.PutCanary(ctx, root, value, 60); !errors.Is(err, dnsservice.ErrDnsRestoring) {
			t.Errorf("while restoring: %v", err)
		}
		if state, _ := model.GetDnsRootState(ctx, root); state.Serial != serial {
			t.Errorf("a refused canary took serial %d", state.Serial)
		}
	})
}

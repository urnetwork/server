//go:build sidecar_pending

package work

// §19.10's labels step (dns_ct_replay_work.go; DNS-M5-16): a replay with no CT log installed, or with a log that
// fails, does not finish and asks to run again, and one whose logs answer replays their union from the dump's time
// less 7 days.

import (
	"context"
	"crypto/sha256"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// A CT log in memory: its names, or its failure, and the since it was asked from.
type testDnsCtLog struct {
	names []*model.DnsCtName
	err   error
	since time.Time
}

func (self *testDnsCtLog) Name() string {
	return "synthetic log"
}

func (self *testDnsCtLog) Names(ctx context.Context, root string, since time.Time) ([]*model.DnsCtName, error) {
	self.since = since
	return self.names, self.err
}

func TestDnsCtReplayNeedsEveryLog(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		root := "t" + strings.ReplaceAll(server.NewId().String()[24:], "-", "") + ".example."
		if err := model.EnsureDnsRoot(ctx, root); err != nil {
			t.Fatal(err)
		}
		dumpTime := server.NowUtc().Add(-time.Hour)
		args := &DnsCtReplayArgs{Root: root, IncidentId: server.NewId(), DumpTime: dumpTime}
		now := server.NowUtc()

		result, err := runDnsCtReplay(ctx, args, nil, now)
		if err != nil || !result.Failed {
			t.Fatalf("no log: %+v %v", result, err)
		}
		first := &testDnsCtLog{names: []*model.DnsCtName{{Name: "lost00005." + strings.TrimSuffix(root, "."), LeafSha256: sha256.Sum256([]byte("leaf one"))}}}
		failing := &testDnsCtLog{err: errors.New("synthetic outage")}
		result, err = runDnsCtReplay(ctx, args, []DnsCtLog{first, failing}, now)
		if err != nil || !result.Failed || result.InsertedCount != 0 {
			t.Fatalf("a failing log: %+v %v", result, err)
		}
		second := &testDnsCtLog{names: []*model.DnsCtName{{Name: "lost00006." + strings.TrimSuffix(root, "."), LeafSha256: sha256.Sum256([]byte("leaf two"))}}}
		result, err = runDnsCtReplay(ctx, args, []DnsCtLog{first, second}, now)
		if err != nil || result.Failed || result.NameCount != 2 || result.InsertedCount != 2 {
			t.Fatalf("both logs: %+v %v", result, err)
		}
		if want := dumpTime.Add(-7 * 24 * time.Hour); !first.since.Equal(want) || !second.since.Equal(want) {
			t.Fatalf("since %s and %s, want %s", first.since, second.since, want)
		}
		names, err := model.GetDnsRestoreIncidentNames(ctx, root, args.IncidentId)
		if err != nil || len(names) != 2 {
			t.Fatalf("recorded %v %v", names, err)
		}
	})
}

//go:build sidecar_e2e && sidecar_localstack && sidecar_pending

// A T3 world on its defaults runs real keys (world_networks.go's keys and config rows): each device's own key client at
// the gkm ServeGkm serves, the config repo's heads on evs, and each network bootstrapped by its first owner's follower,
// FakeGkm and the in-memory heads left untouched; and a bootstrap on real keys is refused at once while gkm or evs
// does not answer at its origin. Run them with the private stores through the build's wrapper, -tags 'sidecar_e2e
// sidecar_pending sidecar_localstack', as world_evs_e2e_test.go's cases run.

package sidecart3

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/gkmservice"
	"github.com/urnetwork/sidecar/protocol"

	"github.com/urnetwork/server"
)

// What the real-keys cases run with (QD-702).
type t3RealKeysSettings struct {
	// The network's sidecars: its owner's first device, which bootstraps it, and the devices it admits.
	SidecarCount int
	// Each case's bound.
	CaseTimeout time.Duration
}

func defaultT3RealKeysSettings() *t3RealKeysSettings {
	return &t3RealKeysSettings{
		SidecarCount: 3,
		CaseTimeout:  5 * time.Minute,
	}
}

// A world on default settings with evs and gkm served, and a network of count sidecars its first bootstrapped and
// whose others it admitted.
func newT3RealKeysWorld(ctx context.Context, tb testing.TB, count int) (*T3World, *T3Network, []*T3Device) {
	world, err := NewT3World(tb, nil)
	if err != nil {
		tb.Fatal(err)
	}
	if _, err := world.ServeEvs(ctx, nil); err != nil {
		tb.Fatal(err)
	}
	if _, err := world.ServeGkm(ctx, nil); err != nil {
		tb.Fatal(err)
	}
	network, err := world.AddNetwork(ctx, "a")
	if err != nil {
		tb.Fatal(err)
	}
	devices, err := network.AddSidecars(ctx, count, nil)
	if err != nil {
		tb.Fatal(err)
	}
	if err := network.Bootstrap(ctx, devices[0]); err != nil {
		tb.Fatal(err)
	}
	started := time.Now()
	if err := network.Admit(ctx, devices[1:]); err != nil {
		tb.Fatal(err)
	}
	tb.Logf("%d devices admitted and standing %s after the first admission began", len(devices)-1, time.Since(started).Round(time.Millisecond))
	return world, network, devices
}

// The config repo the network's genesis names, as device's follower verified it.
func t3RealKeysConfigRepoId(tb testing.TB, device *T3Device) sidecar.Id {
	follower := device.Follower()
	if follower == nil || follower.VerifiedState() == nil {
		tb.Fatalf("%s holds no verified state", device.Name)
	}
	configRepoId, err := sidecar.IdFromBytes(follower.VerifiedState().Genesis().GetConfigRepoId())
	if err != nil {
		tb.Fatalf("the genesis names no config repo: %v", err)
	}
	return configRepoId
}

// On its default settings a world's devices run their real key clients against the gkm it serves: the owner's
// follower bootstraps the network, forming the config group at gkm and pushing the first head on evs, each admitted
// device stands holding the config repo's generation through its own key client, a repo the owner creates forms its
// group at gkm, and every device writes the repo and reads every other's write. FakeGkm holds no group of either repo
// and the network's in-memory config heads took no head, so nothing went the old way.
func TestT3WorldRunsOnRealKeysByDefault(t *testing.T) {
	settings := defaultT3RealKeysSettings()
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), settings.CaseTimeout)
		defer cancel()
		if DefaultT3WorldSettings().FakeGkm {
			tb.Fatal("a world's default settings run FakeGkm")
		}
		world, network, devices := newT3RealKeysWorld(ctx, tb, settings.SidecarCount)
		defer world.Close()
		owner := devices[0]
		for _, device := range devices {
			if device.keys == nil {
				tb.Fatalf("%s runs FakeGkm's key client", device.Name)
			}
		}

		configRepoId := t3RealKeysConfigRepoId(tb, owner)
		for _, device := range devices {
			key, err := device.Gkm.CurrentRepoKey(ctx, configRepoId)
			if err != nil {
				tb.Fatalf("%s holds no key of the config repo through its own key client: %v", device.Name, err)
			}
			if key.Generation() < 1 {
				tb.Errorf("%s holds the config repo at generation %d", device.Name, key.Generation())
			}
		}
		if version := network.heads.Version(); version != 0 {
			tb.Errorf("the network's in-memory config heads took a head, at %d", version)
		}
		if _, _, ok := world.gkm.Newest(configRepoId); ok {
			tb.Errorf("FakeGkm holds a group of the config repo")
		}

		repo, err := network.CreateRepo(ctx, owner, protocol.RepoKind_REPO_KIND_GENERAL)
		if err != nil {
			tb.Fatal(err)
		}
		repoId := repo.RepoId()
		if _, _, ok := world.gkm.Newest(repoId); ok {
			tb.Errorf("FakeGkm holds a group of the repo the owner created")
		}
		// the repo's first GroupForm, which gkm's KF1 stores and evs's activate route reads
		first, err := world.Host().Blobs.FirstCluster()
		if err != nil {
			tb.Fatal(err)
		}
		if _, err := first.Get(ctx, gkmservice.GkmFormKey(repoId, 1)); err != nil {
			tb.Errorf("gkm stored no GroupForm of the repo at its KF1: %v", err)
		}
		if _, err := network.Grant(ctx, owner, repoId, network.MemberId(), protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER); err != nil {
			tb.Fatal(err)
		}

		contents := map[string][]byte{}
		for _, device := range devices {
			client, err := device.EvsClient()
			if err != nil {
				tb.Fatal(err)
			}
			path := device.Name + ".txt"
			content := []byte(fmt.Sprintf("%s writes on its own keys\n", device.Name))
			if _, err := client.Repo(repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(content), Size: int64(len(content))}); err != nil {
				tb.Fatalf("%s's write: %v", device.Name, err)
			}
			contents[path] = content
		}
		for _, device := range devices {
			client, err := device.EvsClient()
			if err != nil {
				tb.Fatal(err)
			}
			for path, content := range contents {
				var read bytes.Buffer
				if _, err := client.Repo(repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true}, &read); err != nil {
					tb.Errorf("%s's read of %s: %v", device.Name, path, err)
					continue
				}
				if !bytes.Equal(read.Bytes(), content) {
					tb.Errorf("%s read %q at %s, not %q", device.Name, read.Bytes(), path, content)
				}
			}
		}
	})
}

// A bootstrap on real keys is refused at once, its follower never started, while gkm or evs does not answer at its
// origin: the follower's bootstrap forms the config group at gkm and pushes the first head on evs, and without them it
// would wait out SyncTimeout and fail on an error that names neither.
func TestT3WorldRealKeysBootstrapRefusesWithoutGkmOrEvs(t *testing.T) {
	settings := defaultT3RealKeysSettings()
	(&server.TestEnv{ApplyDbMigrations: true}).Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(context.Background(), settings.CaseTimeout)
		defer cancel()
		world, err := NewT3World(tb, nil)
		if err != nil {
			tb.Fatal(err)
		}
		defer world.Close()
		network, err := world.AddNetwork(ctx, "a")
		if err != nil {
			tb.Fatal(err)
		}
		devices, err := network.AddSidecars(ctx, 1, nil)
		if err != nil {
			tb.Fatal(err)
		}
		owner := devices[0]
		refused := func(service string) {
			err := network.Bootstrap(ctx, owner)
			if err == nil || !strings.Contains(err.Error(), "only with "+service+" answering at its origin") {
				tb.Fatalf("the bootstrap with %s unserved: %v", service, err)
			}
			if network.Owner() != nil || owner.Follower() != nil || owner.Sidecar().ConfigFollower() != nil {
				tb.Fatalf("the bootstrap refused for %s left an owner or a follower", service)
			}
		}
		refused("gkm")
		if _, err := world.ServeGkm(ctx, nil); err != nil {
			tb.Fatal(err)
		}
		refused("evs")
	})
}

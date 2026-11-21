//go:build sidecar_e2e && sidecar_pending && sidecar_localstack

// The blogger case's change sub-case (blogger_test.go): each writer's webserver clears its cache at the changes its
// sidecar's EvsClient raises for the blog repo (blogger_web_test.go), so every commit of another writer must reach it
// as a change whatever else the client did meanwhile. This runs that alone, on the real hint path: one network of two
// writers and a shared repo, the reader's EvsClient composed with its evs component, as ur daemon composes it, and
// subscribed to the article directory, its change listener keeping the newest version it heard. Each round the other
// writer commits an article's next version; in the reading rounds the reader's own client reads that path, fresh,
// at once, as a webserver's miss does, and in the control rounds it does nothing. Every round must bring the
// listener a change at or past the commit within SettleTimeout. The rounds that miss are reported with whether the
// reader's read raised its pin across the commit first, the evidence that a statement another call of the client
// verified is one its subscriber's read never raises.

package sidecare2e

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
)

// What the change sub-case runs with (QD-702).
type bloggerChangeSettings struct {
	// The rounds, every other one a reading round, the first of them reading.
	RoundCount int
	// How long a round's change may take to reach the listener (blogger's SettleTimeout).
	SettleTimeout time.Duration
	// The case's bound beside the world's.
	PhaseTimeout time.Duration
	// The repo directory the article lives in, and its name.
	ArticleDirectory string
	ArticleName      string
}

func defaultBloggerChangeSettings() *bloggerChangeSettings {
	return &bloggerChangeSettings{
		RoundCount:       8,
		SettleTimeout:    defaultBloggerSettings().SettleTimeout,
		PhaseTimeout:     4 * time.Minute,
		ArticleDirectory: defaultBloggerSettings().ArticleDirectory,
		ArticleName:      "e2e-blogger-change",
	}
}

// One round: whether the reader read at once, the commit's ref version, the reader's pin before and after its read,
// and what the listener heard by the round's bound.
type bloggerChangeRound struct {
	read       bool
	refVersion uint64
	pinBefore  uint64
	pinAfter   uint64
	heard      uint64
	latency    time.Duration
}

func (self *bloggerChangeRound) describe() string {
	line := fmt.Sprintf("a commit at ref version %d", self.refVersion)
	if self.read {
		line += fmt.Sprintf(", the reader's read raising its pin from %d to %d", self.pinBefore, self.pinAfter)
	} else {
		line += ", no read"
	}
	if self.refVersion <= self.heard {
		return line + fmt.Sprintf(", heard in %s", self.latency.Round(time.Millisecond))
	}
	return line + fmt.Sprintf(", the newest change heard %d", self.heard)
}

// See the file header.
func TestE2eBloggerChangeAfterTheReadersOwnRead(t *testing.T) {
	settings := defaultBloggerChangeSettings()
	e2eTestEnv().Run(t, func(t testing.TB) {
		world := NewWorld(t, DefaultWorldSettings())
		defer world.Close()
		ctx, cancel := world.StepContext(t.Context(), settings.PhaseTimeout)
		defer cancel()
		network, err := world.AddNetwork(ctx, worldNetworkName(0), 2, nil)
		if err != nil {
			world.FailProgress(t, "the enrollment", err, nil)
		}
		repoId, err := network.CreateSharedRepo(ctx, protocol.RepoGrantRole_REPO_GRANT_ROLE_WRITER)
		if err != nil {
			world.FailProgress(t, "the repo", err, nil)
		}
		reader, writer := network.Clients[0], network.Clients[1]
		readerClient, err := reader.EvsClient()
		if err != nil {
			t.Fatal(err)
		}
		writerClient, err := writer.EvsClient()
		if err != nil {
			t.Fatal(err)
		}
		// the reader's evs component, through which evs's hints reach its client
		if reader.Sidecar().EvsClient() == nil {
			if _, err := reader.Sidecar().AddComponent(sidecar.NewEvsComponent(readerClient)); err != nil {
				t.Fatal(err)
			}
		}
		path := settings.ArticleDirectory + "/" + settings.ArticleName
		version := 0
		write := func() (*sidecar.EvsWriteResult, error) {
			version += 1
			body := []byte(fmt.Sprintf("%s version %d\n", settings.ArticleName, version))
			return writerClient.Repo(repoId).Write(ctx, &sidecar.EvsWriteArgs{Path: path, Content: bytes.NewReader(body), Size: int64(len(body))})
		}
		readFresh := func() (*sidecar.EvsFileInfo, error) {
			var content bytes.Buffer
			return readerClient.Repo(repoId).ReadHead(ctx, &sidecar.EvsReadArgs{Path: path, Fresh: true}, &content)
		}
		if _, err := write(); err != nil {
			t.Fatalf("the first version: %v", err)
		}
		info, err := readFresh()
		if err != nil {
			t.Fatalf("the reader's first read: %v", err)
		}
		pinVersion := func() uint64 {
			pin, ok, err := readerClient.Pins().Pin(repoId, info.RefId)
			if err != nil || !ok {
				return 0
			}
			return pin.Version
		}
		subscription, err := readerClient.Repo(repoId).Subscribe(ctx, &sidecar.EvsSubscribeArgs{Paths: []string{settings.ArticleDirectory}})
		if err != nil {
			t.Fatalf("the reader's subscription: %v", err)
		}
		defer subscription.Close()
		heard := sidecar.NewMonitorValue(uint64(0))
		removeListener := readerClient.AddRepoChangeListener(&repoId, func(change *sidecar.EvsRepoChange) {
			heard.Update(func(newest uint64) uint64 {
				return max(newest, change.Version)
			})
		})
		defer removeListener()

		rounds := []*bloggerChangeRound{}
		for index := range settings.RoundCount {
			round := &bloggerChangeRound{read: index%2 == 0}
			result, err := write()
			if err != nil {
				t.Fatalf("round %d's commit: %v", index, err)
			}
			answeredAt := time.Now()
			round.refVersion = result.Version
			if round.read {
				round.pinBefore = pinVersion()
				if _, err := readFresh(); err != nil {
					t.Fatalf("round %d's read: %v", index, err)
				}
				round.pinAfter = pinVersion()
			}
			deadline := time.After(settings.SettleTimeout)
			for waiting := true; waiting; {
				newest, changed := heard.Get()
				round.heard = newest
				if round.refVersion <= newest {
					round.latency = time.Since(answeredAt)
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("round %d: %v", index, ctx.Err())
				case <-deadline:
					waiting = false
				case <-changed:
				}
			}
			rounds = append(rounds, round)
		}
		lines, missed := []string{}, []string{}
		for index, round := range rounds {
			lines = append(lines, fmt.Sprintf("round %d: %s", index, round.describe()))
			if round.heard < round.refVersion {
				missed = append(missed, fmt.Sprintf("round %d: %s", index, round.describe()))
			}
		}
		t.Logf("the rounds: %s", strings.Join(lines, "; "))
		if 0 < len(missed) {
			t.Errorf("%d of %d commits never reached %s's change listener within %s: EvsClient raises a change only for a statement its subscriber's own read brings past the pin, so a read of the client's own that verified the statement first leaves nothing to raise and a cache cleared at changes keeps what the commit replaced: %s", len(missed), len(rounds), reader.Name, settings.SettleTimeout, strings.Join(missed, "; "))
		}
	})
}

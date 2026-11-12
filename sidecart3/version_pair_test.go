//go:build sidecar_localstack

// §22.8's version pair (TEST-M2-16; version_pair.go) on server's exchange, which needs the private test stores and
// server's schema: a newer side's planted types and next wire major, and an older side's retired type and previous
// major, each answered UNIMPLEMENTED or VERSION with the receiver's range, never dropped. N here is this build: the
// result's Open says so, and the test holds that it does.

package sidecart3

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/sidecar"
	sidecarprotocol "github.com/urnetwork/sidecar/protocol"
)

func TestT3VersionPairAnswersVersionOrUnimplemented(t *testing.T) {
	testEnvWithSchema().Run(t, func(tb testing.TB) {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		platform, err := newExchangePlatform(ctx, t, DefaultExchangeSettings())
		if err != nil {
			t.Fatal(err)
		}
		defer platform.Close()
		pair := &VersionPair{
			Older: &VersionSide{
				Name:      "N",
				CommitSet: map[string]string{"sidecar": "this build", "server": "this build"},
				// a type N still sends that N+1 retired
				PlantedMessageTypes: []sidecarprotocol.MessageType{0x8f7f},
				PlantedWireVersions: []uint32{sidecar.WireVersionMajor - 1},
			},
			Newer: &VersionSide{
				Name:                "N+1",
				CommitSet:           map[string]string{"sidecar": "this build", "server": "this build"},
				PlantedMessageTypes: DefaultNewerPlantedMessageTypes(),
				PlantedWireVersions: []uint32{sidecar.WireVersionMajor + 1},
			},
		}
		result, err := RunVersionPair(ctx, t, platform, pair, nil)
		if err != nil {
			t.Fatal(err)
		}
		want := len(pair.Older.PlantedMessageTypes) + len(pair.Newer.PlantedMessageTypes) + len(pair.Older.PlantedWireVersions) + len(pair.Newer.PlantedWireVersions)
		if len(result.Answers) != want {
			t.Errorf("%d answers, not %d", len(result.Answers), want)
		}
		for _, problem := range result.Problems() {
			t.Error(problem)
		}
		if len(result.Open) == 0 || !strings.Contains(result.Open[0], "no binary of another commit set ran") {
			t.Errorf("a run of this build against itself reads %q, not open on another commit set", result.Open)
		}
	})
}

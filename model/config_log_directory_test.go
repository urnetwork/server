package model

// The team directory at api's sequencer (TEAM-M6-10, D-TEAM-28, §8.8): one per network. A second directory
// RepoCreate is refused at its position with the tip, as every reader refuses it, whichever device files it and
// whatever the tier; the tier check is TestConfigLogRefusesTheDirectoryOutsideTheTeamTier's.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

func TestConfigLogRefusesASecondDirectory(t *testing.T) {
	ctx := context.Background()
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(ctx, cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	settings.TeamTier = func(ctx context.Context, networkId server.Id) bool {
		return true
	}
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	networkId := server.Id(world.NetworkId())
	signedHeader, rows, genesis, configPushes := world.BaseOne()
	pin, err := sequencer.AppendBase(ctx, networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	if err != nil {
		t.Fatal(err)
	}
	world.SetTip(pin)

	first, err := sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_DIRECTORY))
	if err != nil {
		t.Fatalf("the first directory: %v", err)
	}
	world.SetTip(first)
	_, err = sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_DIRECTORY))
	configLogError := &ConfigLogError{}
	if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden || configLogError.Tip.GetVersion() != first.GetVersion() ||
		!errors.Is(err, sidecar.ErrConfigRule) {
		t.Fatalf("a second directory: %v", err)
	}
	tip, err := sequencer.Tip(ctx, networkId)
	if err != nil || tip.GetVersion() != first.GetVersion() {
		t.Fatalf("the refusal moved the tip: %v %v", tip, err)
	}
	// another kind still lands after it
	if general, err := sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_GENERAL)); err != nil || general.GetVersion() != first.GetVersion()+1 {
		t.Fatalf("a general repo after the refusal: %v %v", general, err)
	}
}

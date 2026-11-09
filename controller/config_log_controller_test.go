//go:build sidecar_pending

package controller

// §7.9's routes through the controller (TEAM-M2-13): a bound first device publishes base 1 with its genesis,
// appends a statement and reads the log back from 0; a client whose sidecar is not admitted posts no plain
// statement and no base, nor base 1 for a genesis naming another device; and a process with no first
// metadata cluster answers 503. Bindings are real, on MG1's tables, so this runs under sidecar_pending.

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The status and code a route's refusal carries.
func configLogRefusal(err error) (int, string) {
	configLogError := &model.ConfigLogError{}
	if !errors.As(err, &configLogError) {
		return 0, ""
	}
	return configLogError.Status, configLogError.Code
}

// A sequencer on T2's fake first cluster, installed for the test and removed after it.
func installTestConfigLogSequencer(t testing.TB, clock sidecar.Clock) {
	t.Helper()
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	settings := model.DefaultConfigLogSettings()
	settings.Clock = clock
	model.SetConfigLogSequencer(model.NewConfigLogSequencer(cluster, settings, nil))
	t.Cleanup(func() { model.SetConfigLogSequencer(nil) })
}

func TestConfigLogRoutesServeABoundFirstDevice(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, userId := newSidecarBindTestClient(t, ctx, "config-log-routes")
		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession); err != nil {
			t.Fatal(err)
		}
		world := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(clientSession.ByJwt.NetworkId), sidecar.Id(userId), sidecar.Id(keys.sidecarId))
		installTestConfigLogSequencer(t, world.Clock())

		signedHeader, rows, signedGenesis, configPushes := world.BaseOne()
		base, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: signedHeader, Rows: rows, Genesis: signedGenesis, ConfigPushes: configPushes}, clientSession)
		if err != nil || base.N != 1 {
			t.Fatalf("base 1: %v %+v", err, base)
		}
		world.SetTip(&bridge.AuthPin{Version: base.N, Chain: base.Chain})
		statement, err := NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: world.MemberAdmit()}, clientSession)
		if err != nil || statement.N != 2 {
			t.Fatalf("a statement: %v %+v", err, statement)
		}
		read, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{}, clientSession)
		if err != nil {
			t.Fatal(err)
		}
		if string(read.Genesis) != string(signedGenesis) || len(read.Entries) != 2 || len(read.Rows) != 1 || read.Tip.N != 2 || read.More {
			t.Errorf("the read: %d entries, %d rows, tip %+v", len(read.Entries), len(read.Rows), read.Tip)
		}
		later, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{After: 1}, clientSession)
		if err != nil || len(later.Entries) != 1 || later.Genesis != nil {
			t.Errorf("a read after 1: %v", err)
		}
	})
}

func TestConfigLogRoutesRefuseAnUnadmittedClient(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		clientSession, userId := newSidecarBindTestClient(t, ctx, "config-log-unadmitted")
		if _, err := NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: []byte{1}}, clientSession); err == nil {
			t.Fatal("a process with no first cluster appended")
		} else if status, _ := configLogRefusal(err); status != http.StatusServiceUnavailable {
			t.Errorf("no sequencer: %v", err)
		}

		world := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(clientSession.ByJwt.NetworkId), sidecar.Id(userId), sidecar.NewId())
		installTestConfigLogSequencer(t, world.Clock())

		signedHeader, rows, signedGenesis, configPushes := world.BaseOne()
		_, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: signedHeader, Rows: rows, Genesis: signedGenesis, ConfigPushes: configPushes}, clientSession)
		if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_unadmitted" {
			t.Errorf("base 1 for another device's genesis: %v", err)
		}
		_, err = NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: world.MemberAdmit()}, clientSession)
		if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_unadmitted" {
			t.Errorf("an unadmitted client's plain statement: %v", err)
		}
		_, err = NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: signedHeader, Rows: rows}, clientSession)
		if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_unadmitted" {
			t.Errorf("an unadmitted client's base: %v", err)
		}
		if _, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{}, clientSession); err != nil {
			t.Errorf("an unadmitted client reads the log: %v", err)
		}

		// bound now, its genesis naming a member other than the network's owner
		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession); err != nil {
			t.Fatal(err)
		}
		stranger := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(clientSession.ByJwt.NetworkId), sidecar.NewId(), sidecar.Id(keys.sidecarId))
		installTestConfigLogSequencer(t, stranger.Clock())
		signedHeader, rows, signedGenesis, configPushes = stranger.BaseOne()
		_, err = NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: signedHeader, Rows: rows, Genesis: signedGenesis, ConfigPushes: configPushes}, clientSession)
		if status, code := configLogRefusal(err); status != http.StatusForbidden || code != "config_unadmitted" {
			t.Errorf("base 1 for a genesis naming another owner: %v", err)
		}
	})
}

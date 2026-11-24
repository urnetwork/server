//go:build sidecar_pending

package controller

// DESIGN-COMPUTE §16.6's authorization features at api (CACL-M11-03): a base turning on a feature some service
// serving the network lacks is refused, feature_unsupported with the tip, before anything is appended, and one
// turning on nothing lands; GET /sidecar/sync/core keeps each sidecar's stated features and names the features the
// services implement, and lists the sidecars lacking one to an Owner's or Admin's device alone. Bindings and the log
// are real, on MG1's tables, so this runs under sidecar_pending, as config_log_controller_test.go does.

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"net/http"
	"slices"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/session"
)

// rows with their settings' auth_features set to features.
func testFeatureRows(t testing.TB, rows []byte, features sidecar.AuthFeature) []byte {
	t.Helper()
	projection := &bridge.ProjectionRows{}
	if err := proto.Unmarshal(rows, projection); err != nil {
		t.Fatal(err)
	}
	projection.Settings = &bridge.ProjectionSettings{AuthFeatures: uint32(features)}
	featureRows, err := proto.MarshalOptions{Deterministic: true}.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	return featureRows
}

// A base the world's owner signs after prev, folding through it, over rows, naming the config head base 1 names.
func testFeatureBaseHeader(t testing.TB, world *sidecartest.ConfigLogSuiteWorld, baseOneHeader []byte, prev *bridge.AuthPin, rows []byte) []byte {
	t.Helper()
	signed := &bridge.Signed{}
	baseOne := &bridge.ProjectionHeader{}
	if err := proto.Unmarshal(baseOneHeader, signed); err != nil {
		t.Fatal(err)
	}
	if err := proto.Unmarshal(signed.GetBody(), baseOne); err != nil {
		t.Fatal(err)
	}
	rowsSha256 := sha256.Sum256(rows)
	return world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, &bridge.ProjectionHeader{
		NetworkId:        baseOne.GetNetworkId(),
		Version:          prev.GetVersion() + 1,
		PrevChain:        prev.GetChain(),
		FoldedThrough:    prev.GetVersion(),
		ConfigVersion:    baseOne.GetConfigVersion(),
		ConfigHeadSha256: baseOne.GetConfigHeadSha256(),
		RowsSha256:       rowsSha256[:],
		RowsByteCount:    uint32(len(rows)),
	})
}

// A bound first device whose network's log holds base 1, with its world and base 1's header and rows.
func testFeatureNetwork(t testing.TB, ctx context.Context, label string) (*session.ClientSession, server.Id, *sidecartest.ConfigLogSuiteWorld, []byte, []byte, *NetworkConfigPinResult) {
	t.Helper()
	clientSession, userId := newSidecarBindTestClient(t, ctx, label)
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
	return clientSession, userId, world, signedHeader, rows, base
}

// The refusal's code and tip, as the route answers them.
func testFeatureRefusal(err error) (int, string, *bridge.AuthPin) {
	configLogError := &model.ConfigLogError{}
	if !errors.As(err, &configLogError) {
		return 0, "", nil
	}
	return configLogError.Status, configLogError.Code, configLogError.Tip
}

func TestConfigProjectionRefusesUnsupportedFeature(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()

		// base 1 turning a feature on: no service names it, so it is refused before any log exists, with no tip
		clientSession, userId := newSidecarBindTestClient(t, ctx, "config-log-feature-base-one")
		keys := newSidecarBindTestKeys(t)
		if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, clientSession, 1)}, clientSession); err != nil {
			t.Fatal(err)
		}
		world := sidecartest.NewConfigLogSuiteWorldFor(ctx, nil, sidecar.Id(clientSession.ByJwt.NetworkId), sidecar.Id(userId), sidecar.Id(keys.sidecarId))
		installTestConfigLogSequencer(t, world.Clock())
		baseOneHeader, baseOneRows, signedGenesis, configPushes := world.BaseOne()
		genesisChain := bridge.GenesisChain(signedGenesis)
		featureRows := testFeatureRows(t, baseOneRows, sidecar.AuthFeatureAgentRepos)
		featureHeader := testFeatureBaseHeader(t, world, baseOneHeader, &bridge.AuthPin{Version: 0, Chain: genesisChain[:]}, featureRows)
		_, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: featureHeader, Rows: featureRows, Genesis: signedGenesis, ConfigPushes: configPushes}, clientSession)
		if status, code, tip := testFeatureRefusal(err); status != http.StatusForbidden || code != configFeatureUnsupported || tip != nil {
			t.Fatalf("base 1 turning agent_repos on: %v", err)
		}
		if read, err := NetworkConfigProjection(&NetworkConfigProjectionArgs{}, clientSession); err != nil || len(read.Entries) != 0 || read.Genesis != nil {
			t.Fatalf("a refused base 1 was appended: %v", err)
		}

		// a later base turning a feature on, refused with the tip, while any service serving the network lacks it
		clientSession, _, world, baseOneHeader, baseOneRows, base := testFeatureNetwork(t, ctx, "config-log-feature")
		networkId := clientSession.ByJwt.NetworkId
		tip := &bridge.AuthPin{Version: base.N, Chain: base.Chain}
		featureRows = testFeatureRows(t, baseOneRows, sidecar.AuthFeatureAgentRepos)
		featureHeader = testFeatureBaseHeader(t, world, baseOneHeader, tip, featureRows)
		publish := func() error {
			_, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: featureHeader, Rows: featureRows}, clientSession)
			return err
		}
		refusedWithTip := func(name string) {
			t.Helper()
			status, code, refusalTip := testFeatureRefusal(publish())
			if status != http.StatusForbidden || code != configFeatureUnsupported || !proto.Equal(refusalTip, tip) {
				t.Fatalf("%s: %d %s, tip %v", name, status, code, refusalTip)
			}
		}
		refusedWithTip("no service names agent_repos")
		every := "sidecar:\n  auth_features: {api: [agent_repos], evs: [agent_repos], gkm: [agent_repos], mxedge: [agent_repos], compute: [agent_repos]}\n"
		restoreConfig := server.Testing_PushSidecarConfig(every)
		refusedWithTip("every service names it but api's release lacks it")
		restoreConfig()

		// with api's release standing in as implementing it: the services serving the network decide
		defer server.Testing_SetSidecarAuthFeaturesImplemented(sidecar.AuthFeatureAgentRepos)()
		served := "sidecar:\n  auth_features: {api: [agent_repos], evs: [agent_repos]}\n  evs: {enabled: true, networks: [" + networkId.String() + "]}\n"
		restoreConfig = server.Testing_PushSidecarConfig(served + "  mxedge: {enabled: true, networks: [" + networkId.String() + "]}\n")
		refusedWithTip("mxedge serves the network and names nothing")
		restoreConfig()
		restoreConfig = server.Testing_PushSidecarConfig(served)
		// gkm, mxedge and compute serve the network no gate's way, so api and evs decide: the check passes, and api's
		// own verifier, whose real release lacks the feature, refuses the base as every reader of its release would
		status, code, _ := testFeatureRefusal(publish())
		if status != http.StatusForbidden || code != "config_refused" {
			t.Fatalf("every service serving the network implementing agent_repos: %d %s", status, code)
		}
		restoreConfig()

		// a base turning nothing on lands
		plainHeader := testFeatureBaseHeader(t, world, baseOneHeader, tip, baseOneRows)
		plain, err := NetworkConfigProjectionPublish(&NetworkConfigProjectionPublishArgs{SignedHeader: plainHeader, Rows: baseOneRows}, clientSession)
		if err != nil || plain.N != 2 {
			t.Fatalf("a base turning nothing on: %v %+v", err, plain)
		}
	})
}

// A second client of the network, bound to a fresh sidecar, which the log admits as a Member's device.
func testFeatureMemberDevice(t testing.TB, ctx context.Context, ownerSession *session.ClientSession, userId server.Id, label string, world *sidecartest.ConfigLogSuiteWorld) (*session.ClientSession, server.Id) {
	t.Helper()
	networkId := ownerSession.ByJwt.NetworkId
	userSession := session.Testing_CreateClientSession(ctx, &session.ByJwt{NetworkId: networkId, UserId: userId, NetworkName: label})
	authResult, err := model.AuthNetworkClient(&model.AuthNetworkClientArgs{Description: "synthetic", DeviceSpec: "synthetic"}, userSession)
	if err != nil || authResult.Error != nil || authResult.ByClientJwt == nil {
		t.Fatalf("second client: %v %+v", err, authResult)
	}
	byJwt, err := session.ParseByJwt(ctx, *authResult.ByClientJwt)
	if err != nil {
		t.Fatal(err)
	}
	memberSession := session.Testing_CreateClientSession(ctx, byJwt)
	keys := newSidecarBindTestKeys(t)
	if _, err := SidecarBind(&SidecarBindArgs{Binding: keys.bind(t, memberSession, 1)}, memberSession); err != nil {
		t.Fatal(err)
	}
	memberId := sidecar.NewId()
	admitMember, err := NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header: world.Header(), MemberId: memberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
	})}, ownerSession)
	if err != nil {
		t.Fatalf("the member's admission: %v", err)
	}
	world.SetTip(&bridge.AuthPin{Version: admitMember.N, Chain: admitMember.Chain})
	admitDevice, err := NetworkConfigDelta(&NetworkConfigDeltaArgs{Signed: world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header:            world.Header(),
		NetworkId:         sidecar.Id(networkId).Bytes(),
		SidecarId:         sidecar.Id(keys.sidecarId).Bytes(),
		IdentityPublicKey: ed25519.NewKeyFromSeed(keys.identitySeed).Public().(ed25519.PublicKey),
		IdentityAlg:       "ed25519",
		MemberId:          memberId.Bytes(),
		Capabilities:      uint32(bridge.SidecarCapability_SIDECAR_CAPABILITY_INVITED),
	})}, ownerSession)
	if err != nil {
		t.Fatalf("the member's device's admission: %v", err)
	}
	world.SetTip(&bridge.AuthPin{Version: admitDevice.N, Chain: admitDevice.Chain})
	return memberSession, keys.sidecarId
}

func TestSidecarSyncCoreAuthFeatures(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(t testing.TB) {
		ctx := context.Background()
		ownerSession, userId, world, _, _, _ := testFeatureNetwork(t, ctx, "sync-core-features")
		networkId := ownerSession.ByJwt.NetworkId
		ownerSidecarId := server.Id(world.OwnerSidecarId())
		memberSession, memberSidecarId := testFeatureMemberDevice(t, ctx, ownerSession, userId, "sync-core-features", world)
		settings := model.DefaultSidecarFeaturesSettings()

		// none named, no feature: every answer names none and lists nothing lacking it
		core, err := SidecarSyncTopicWithFeatures(sidecar.PushTopicCore, "", "agent_repos", ownerSession)
		if err != nil || core.AuthFeatures == nil || len(core.AuthFeatures) != 0 || core.AuthFeaturesLacking == nil || len(core.AuthFeaturesLacking.Sidecars) != 0 {
			t.Fatalf("with no service naming a feature: %+v %v", core, err)
		}

		defer server.Testing_SetSidecarAuthFeaturesImplemented(sidecar.AuthFeatureAgentRepos | sidecar.AuthFeatureListeners)()
		defer server.Testing_PushSidecarConfig("sidecar:\n  auth_features: {api: [agent_repos, listeners]}\n")()
		// a sidecar the log no longer holds: its entry is the removal drop's
		goneSidecarId := server.NewId()
		if err := model.SetSidecarStatedAuthFeatures(ctx, settings, networkId, goneSidecarId, sidecar.AuthFeatureAgentRepos); err != nil {
			t.Fatal(err)
		}

		// the Member's device states none and is told the services' features, and no other sidecar's
		core, err = SidecarSyncTopicWithFeatures(sidecar.PushTopicCore, "", "", memberSession)
		if err != nil || !slices.Equal(core.AuthFeatures, []string{"agent_repos", "listeners"}) || core.AuthFeaturesLacking != nil {
			t.Fatalf("the Member's device: %+v %v", core, err)
		}
		// the Owner's device states agent_repos and is told every standing sidecar lacking one, in id order
		core, err = SidecarSyncTopicWithFeatures(sidecar.PushTopicCore, "", "agent_repos,nothing", ownerSession)
		if err != nil || !slices.Equal(core.AuthFeatures, []string{"agent_repos", "listeners"}) || core.AuthFeaturesLacking == nil || core.AuthFeaturesLacking.More {
			t.Fatalf("the Owner's device: %+v %v", core, err)
		}
		want := []*SidecarSyncCoreSidecarResult{
			{SidecarId: ownerSidecarId, AuthFeatures: []string{"agent_repos"}},
			{SidecarId: memberSidecarId, AuthFeatures: []string{}},
		}
		slices.SortFunc(want, func(a *SidecarSyncCoreSidecarResult, b *SidecarSyncCoreSidecarResult) int {
			return a.SidecarId.Cmp(b.SidecarId)
		})
		listed := core.AuthFeaturesLacking.Sidecars
		if len(listed) != len(want) {
			t.Fatalf("listed %d sidecars", len(listed))
		}
		for i := range want {
			if listed[i].SidecarId != want[i].SidecarId || !slices.Equal(listed[i].AuthFeatures, want[i].AuthFeatures) {
				t.Errorf("sidecar %d: %+v, want %+v", i, listed[i], want[i])
			}
		}

		// what api keeps: each sidecar's last statement, the removed one's dropped
		stated, err := model.GetSidecarStatedAuthFeatures(ctx, settings, networkId)
		if err != nil {
			t.Fatal(err)
		}
		if len(stated) != 2 || stated[ownerSidecarId] != sidecar.AuthFeatureAgentRepos || stated[memberSidecarId] != 0 {
			t.Fatalf("api keeps %v", stated)
		}
		// a later statement replaces the earlier one, and a sidecar stating both features is no longer listed
		if _, err := SidecarSyncTopicWithFeatures(sidecar.PushTopicCore, "", "agent_repos,listeners", memberSession); err != nil {
			t.Fatal(err)
		}
		core, err = SidecarSyncTopicWithFeatures(sidecar.PushTopicCore, "", "agent_repos,listeners", ownerSession)
		if err != nil || core.AuthFeaturesLacking == nil || len(core.AuthFeaturesLacking.Sidecars) != 0 {
			t.Fatalf("every sidecar stating both: %+v %v", core, err)
		}
		// past the listed count the answer says more were left out
		for _, sidecarId := range []server.Id{memberSidecarId, ownerSidecarId} {
			if err := model.SetSidecarStatedAuthFeatures(ctx, settings, networkId, sidecarId, 0); err != nil {
				t.Fatal(err)
			}
		}
		lackingSettings := model.DefaultSidecarFeaturesSettings()
		lackingSettings.ListedSidecarCount = 1
		sequencer, err := model.CurrentConfigLogSequencer()
		if err != nil {
			t.Fatal(err)
		}
		lacking, err := sequencer.SidecarFeaturesLacking(ctx, lackingSettings, networkId, ownerSidecarId, sidecar.AuthFeatureAgentRepos)
		if err != nil || lacking == nil || len(lacking.Sidecars) != 1 || !lacking.More {
			t.Fatalf("past the listed count: %+v %v", lacking, err)
		}
		if lacking, err := sequencer.SidecarFeaturesLacking(ctx, lackingSettings, networkId, memberSidecarId, sidecar.AuthFeatureAgentRepos); err != nil || lacking != nil {
			t.Fatalf("a Member's device asked the model directly: %+v %v", lacking, err)
		}
	})
}

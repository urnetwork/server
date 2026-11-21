package dnshost

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/dnsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"
	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/server"
)

// A DNS block with its ordinary read-only credential, over an owner-signed synthetic projection.
// A request for a cold network queues verification. Subsequent reads, writes and TLSA checks see
// only this signer's grants and still honor the network's standing.
func TestHostedDnsGatesUseTheVerifiedProjection(t *testing.T) {
	fake := sidecartest.NewFakeHost(t.Context(), nil, nil, nil, nil)
	host := fake.ServiceHost()
	cluster, err := fake.Blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	network, owner := world.NetworkId(), world.OwnerSidecarId()
	fake.Abuse.Set(connect.Id(network), sidecar.AbuseStandingStandard, true)
	headerBytes, originalRows, genesis, _ := world.BaseOne()
	rows := &bridge.ProjectionRows{}
	signed := &bridge.Signed{}
	header := &bridge.ProjectionHeader{}
	for _, item := range []struct {
		data    []byte
		message proto.Message
	}{{originalRows, rows}, {headerBytes, signed}} {
		if err := proto.Unmarshal(item.data, item.message); err != nil {
			t.Fatal(err)
		}
	}
	if err := proto.Unmarshal(signed.Body, header); err != nil {
		t.Fatal(err)
	}
	marshal := func(message proto.Message) []byte {
		t.Helper()
		data, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	atom := sidecar.NewId().Bytes()
	rule := func(name string, action protocol.AclAction) *protocol.AclRule {
		return &protocol.AclRule{RuleId: sidecar.NewId().Bytes(), SrcAtoms: [][]byte{atom}, Targets: []*protocol.AclTarget{{PublicName: name}}, Actions: []uint32{uint32(action)}}
	}
	rows.TeamScopes = []*bridge.TeamScopeRow{
		{Label: "foreign-unit", Owner: &bridge.TeamScopeRow_MemberId{MemberId: sidecar.NewId().Bytes()}},
		{Label: "tag-unit", Owner: &bridge.TeamScopeRow_TagAtom{TagAtom: sidecar.NewId().Bytes()}},
	}
	rows.Acl = marshal(&protocol.AclRows{HasDocument: true, Atoms: []*protocol.AclAtom{{Atom: atom, SidecarIds: [][]byte{owner.Bytes()}}}, Rules: []*protocol.AclRule{
		rule("write", protocol.AclAction_ACL_ACTION_ADMIN),
		rule("view", protocol.AclAction_ACL_ACTION_PUBLISH),
		rule("smtp/25/mx", protocol.AclAction_ACL_ACTION_LISTEN),
		rule("smtp/25/mx.foreign-unit", protocol.AclAction_ACL_ACTION_LISTEN),
		rule("smtp/25/mx.tag-unit", protocol.AclAction_ACL_ACTION_LISTEN),
	}})
	body := marshal(rows)
	digest := sha256.Sum256(body)
	header.RowsSha256, header.RowsByteCount = digest[:], uint32(len(body))
	entry := &bridge.AuthEntry{NetworkId: network.Bytes(), N: 1, PrevChain: header.PrevChain, Kind: bridge.AuthEntryKindBase, TimeMs: world.Clock().Now().UnixMilli(), Body: world.SignAsOwner(bridge.SignedType_SIGNED_PROJECTION_HEADER, header)}
	entryBytes := marshal(entry)
	bearer := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x28}, 32))
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+bearer {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case sidecar.AclGateApiProjectionPath + network.String():
			if r.URL.Query().Get("after") == "0" {
				json.NewEncoder(w).Encode(map[string]any{"genesis": genesis, "entries": [][]byte{entryBytes}})
			} else {
				json.NewEncoder(w).Encode(map[string]any{"entries": [][]byte{}})
			}
		case fmt.Sprintf("%s%s/%x", sidecar.AclGateApiRowsPath, network, digest):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer origin.Close()
	loaded := make(chan error, 2)
	gateSettings := sidecar.DefaultAclGateSettings()
	gateSettings.PollInterval = 0
	gateSettings.OnLoad = func(_ sidecar.Id, _ *bridge.AuthPin, err error) {
		select {
		case loaded <- err:
		default:
		}
	}
	settings := &HostSettings{ApiUrl: origin.URL, SecretDirectory: hostTestSecrets(t, map[string]string{server.InternalGateCredentialFileName(server.InternalGateCallerDns): "current: " + bearer + "\n"})}
	deps := &dnsservice.DnsApiDeps{}
	stop, err := installDnsProjection(t.Context(), host, settings, nil, gateSettings, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	now := world.Clock().Now()
	if verdict := deps.RecordGate.MayAdmin(network, owner, "write", now); verdict != dnsservice.DnsGateUnavailable {
		t.Fatalf("cold gate: %v", verdict)
	}
	select {
	case err := <-loaded:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("DNS did not load its projection")
	}
	stranger := sidecar.NewId()
	for _, result := range []struct {
		name      string
		got, want dnsservice.DnsGateVerdict
	}{
		{"admin", deps.RecordGate.MayAdmin(network, owner, "write", now), dnsservice.DnsGateAllowed},
		{"view", deps.RecordGate.MayView(network, owner, "view", now), dnsservice.DnsGateAllowed},
		{"publish only", deps.RecordGate.MayAdmin(network, owner, "view", now), dnsservice.DnsGateDenied},
		{"ungranted", deps.RecordGate.MayView(network, owner, "elsewhere", now), dnsservice.DnsGateDenied},
		{"receiver", deps.KeyRecordGate.MayReceive(network, owner, "", now), dnsservice.DnsGateAllowed},
		{"signed tag scope", deps.KeyRecordGate.MayReceive(network, owner, "tag-unit", now), dnsservice.DnsGateAllowed},
		{"other member scope", deps.KeyRecordGate.MayReceive(network, owner, "foreign-unit", now), dnsservice.DnsGateDenied},
		{"other receiver", deps.KeyRecordGate.MayReceive(network, stranger, "", now), dnsservice.DnsGateDenied},
		{"unknown scope", deps.KeyRecordGate.MayReceive(network, owner, "absent", now), dnsservice.DnsGateUnavailable},
	} {
		if result.got != result.want {
			t.Errorf("%s: got %v, want %v", result.name, result.got, result.want)
		}
	}
	fake.Abuse.SetStanding(connect.Id(network), sidecar.AbuseStandingSuspended)
	if deps.RecordGate.MayView(network, owner, "view", now) != dnsservice.DnsGateDenied || deps.KeyRecordGate.MayReceive(network, owner, "", now) != dnsservice.DnsGateDenied {
		t.Error("suspension did not close both gates")
	}
}

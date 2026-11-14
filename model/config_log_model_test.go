package model

// api's log sequencer (TEAM-M2-13): sidecartest's shared store suite, which T2's fake api passes too
// (TEAM-M2-12), on T2's fake Blobs here and on the local stack's MinIO in
// config_log_model_localstack_test.go; network_acl_head following each append; a refusal's body; the
// writes spending the write budget as P0; the gate path, a rows-only verifier reading the log from
// the store, under the Postgres tripwire (I-12); and a RepoPrefixRules judged as every reader judges it
// (ACL-M2-18).

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/evsservice"
	"github.com/urnetwork/sidecar/protocol"
	"github.com/urnetwork/sidecar/protocol/bridge"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// The suite's view of a sequencer: its ids are sidecar's, and a ConfigLogError is the suite's refusal.
type testConfigLogSuiteSequencer struct {
	sequencer *ConfigLogSequencer
}

func testConfigLogSuiteError(err error) error {
	configLogError := &ConfigLogError{}
	if errors.As(err, &configLogError) {
		return &sidecartest.ConfigLogRefusal{Status: configLogError.Status, Tip: configLogError.Tip, Err: configLogError.Err}
	}
	return err
}

func (self testConfigLogSuiteSequencer) AppendStatement(ctx context.Context, networkId sidecar.Id, signed []byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendStatement(ctx, server.Id(networkId), signed)
	return pin, testConfigLogSuiteError(err)
}

func (self testConfigLogSuiteSequencer) AppendBase(ctx context.Context, networkId sidecar.Id, signedHeader []byte, rows []byte, genesis []byte, configPushes [][]byte) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendBase(ctx, server.Id(networkId), &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	return pin, testConfigLogSuiteError(err)
}

func (self testConfigLogSuiteSequencer) AppendTick(ctx context.Context, networkId sidecar.Id, tick *bridge.ApiTick) (*bridge.AuthPin, error) {
	pin, err := self.sequencer.AppendTick(ctx, server.Id(networkId), tick)
	return pin, testConfigLogSuiteError(err)
}

// A sequencer of the suite's: on its cluster and clock, its heads advanced by advanceHead.
func testConfigLogSuiteFactory(advanceHead func(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error)) sidecartest.ConfigLogSequencerFactory {
	return func(clock sidecar.Clock, cluster sidecar.BlobCluster) sidecartest.ConfigLogSequencer {
		settings := DefaultConfigLogSettings()
		settings.Clock = clock
		return testConfigLogSuiteSequencer{sequencer: NewConfigLogSequencer(cluster, settings, advanceHead)}
	}
}

// T2's fake first cluster.
func testConfigLogFakeCluster(t *testing.T) sidecar.BlobCluster {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := blobs.FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	return cluster
}

// The shared suite on server's sequencer over T2's fake Blobs: every behavior a schedule relies on, as
// the fake api has it.
func TestConfigLogSequencerPassesTheStoreSuite(t *testing.T) {
	sidecartest.RunConfigLogStoreSuite(t, testConfigLogFakeCluster(t), testConfigLogSuiteFactory(nil))
}

// Each append advances network_acl_head to its pin, and a head that cannot advance leaves the append
// answered: the entry is committed in the store, which readers and the restore step read.
func TestConfigLogAppendsAdvanceTheAclHead(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	advanced := []uint64{}
	failing := false
	sequencer := NewConfigLogSequencer(cluster, settings, func(ctx context.Context, networkId server.Id, version uint64, chain []byte, hasDocument bool) (bool, error) {
		if failing {
			return false, errors.New("the database is away")
		}
		advanced = append(advanced, version)
		return true, nil
	})
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	if _, err := world.PostMemberAdmit(suite); err != nil {
		t.Fatal(err)
	}
	failing = true
	if _, err := world.PostMemberAdmit(suite); err != nil {
		t.Fatalf("an append whose head did not advance: %v", err)
	}
	if len(advanced) != 2 || advanced[0] != 1 || advanced[1] != 2 {
		t.Errorf("the head advanced to %v", advanced)
	}
}

// With the process's write budget installed, every proj/ and projrows/ PUT is issued to it as P0 of kind
// log, priced, before it is sent, so a first cluster that refuses unbudgeted writes takes the appends;
// without it, that cluster refuses them before anything is sent.
func TestConfigLogWritesSpendTheBudgetAsP0(t *testing.T) {
	blobs := sidecartest.NewFakeBlobs(sidecartest.NewFakeClock(sidecartest.DefaultClockStart()), sidecartest.NewFaultQueue(), nil, sidecartest.DefaultFakeBlobsSettings())
	cluster, err := evsservice.NewBudgetedBlobs(blobs).FirstCluster()
	if err != nil {
		t.Fatal(err)
	}
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()

	unbudgeted := testConfigLogSuiteSequencer{sequencer: NewConfigLogSequencer(cluster, settings, nil)}
	refusal := &sidecartest.ConfigLogRefusal{}
	if err := world.Start(unbudgeted); !errors.As(err, &refusal) || refusal.Status != http.StatusServiceUnavailable || !errors.Is(err, evsservice.ErrWriteUnbudgeted) {
		t.Fatalf("an unbudgeted base 1: %v", err)
	}

	budget := sidecartest.NewFakeWriteBudget(nil)
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	sequencer.SetWriteBudget(budget, func(objectByteCount int64) int64 { return 3 * objectByteCount })
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	if _, err := world.PostMemberAdmit(suite); err != nil {
		t.Fatal(err)
	}
	read, err := sequencer.Read(t.Context(), server.Id(world.NetworkId()), 0)
	if err != nil {
		t.Fatal(err)
	}
	written := len(read.SignedGenesis)
	for _, object := range append(read.Entries, read.Rows...) {
		written += len(object)
	}
	if got := budget.P0ByteCount(cluster.ClusterId(), sidecar.WriteP0KindLog); got != int64(3*written) {
		t.Errorf("issued %d priced bytes for %d written", got, written)
	}
	if violations := budget.Violations(); len(violations) != 0 {
		t.Errorf("the budget's violations: %v", violations)
	}
}

// A refusal answers the router's 4xx body with its code and the tip, and a 503 none.
func TestConfigLogErrorsAnswerTheirStatusAndTip(t *testing.T) {
	tip := &bridge.AuthPin{Version: 7, Chain: make([]byte, 32)}
	refused := &ConfigLogError{Status: http.StatusForbidden, Code: "config_refused", Tip: tip, Err: errors.New("a cause the client never sees")}
	if refused.Error() != "403 The statement fails at the tip." {
		t.Errorf("the message %q", refused.Error())
	}
	body, err := json.Marshal(refused.HttpErrorResultBody())
	if err != nil {
		t.Fatal(err)
	}
	decoded := &ConfigLogRefusal{}
	if err := json.Unmarshal(body, decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Code != "config_refused" || decoded.Tip == nil || decoded.Tip.N != 7 {
		t.Errorf("the body %s", body)
	}
	moved := &ConfigLogError{Status: http.StatusConflict, Code: "config_tip_moved", Tip: tip}
	if moved.Error() != "409 The tip moved." {
		t.Errorf("the 409's message %q", moved.Error())
	}
	unavailable := &ConfigLogError{Status: http.StatusServiceUnavailable, Err: errors.New("store")}
	if unavailable.Error() != "503 The log is unavailable." {
		t.Errorf("the 503's message %q", unavailable.Error())
	}
	if _, err := CurrentConfigLogSequencer(); err == nil {
		t.Error("a process with no first cluster has a sequencer")
	}
}

// I-12: a gate's path, a rows-only verifier applying the log as the store holds it, takes no Postgres
// connection: the tripwire counts none across reads and applies.
func TestTheGatePathTakesNoPostgres(t *testing.T) {
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(t.Context(), cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	suite := testConfigLogSuiteSequencer{sequencer: sequencer}
	if err := world.Start(suite); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := world.PostMemberAdmit(suite); err != nil {
			t.Fatal(err)
		}
	}
	networkId := server.Id(world.NetworkId())

	attempts, stop := server.DenyPostgresForTest(t)
	read, err := sequencer.Read(t.Context(), networkId, 0)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := sidecar.NewConfigLogRowsOnly(nil, read.SignedGenesis)
	if err != nil {
		t.Fatal(err)
	}
	rows := sidecar.ConfigRowsOnlySource(func(networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
		return configLogGetRows(t.Context(), cluster, networkId, rowsSha256)
	})
	verdicts, err := gate.ApplyRun(read.Entries, rows)
	if err != nil {
		t.Fatal(err)
	}
	stop()
	if attempts() != 0 {
		t.Errorf("the gate path took %d Postgres connections", attempts())
	}
	if len(verdicts) != 4 || gate.Pin().GetVersion() != 4 {
		t.Errorf("the gate applied %d entries to %d", len(verdicts), gate.Pin().GetVersion())
	}
}

// A base's rows from projrows/.
func configLogGetRows(ctx context.Context, cluster sidecar.BlobCluster, networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
	rows, found, err := configLogGet(ctx, cluster, sidecar.ConfigRowsKey(networkId, rowsSha256))
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errors.New("no such rows")
	}
	return rows, nil
}

// §8.8: api alone refuses a directory RepoCreate outside the team tier, 403 with the tip, since readers know no tier;
// on the team tier it lands, and a general repo's lands whatever the tier.
func TestConfigLogRefusesTheDirectoryOutsideTheTeamTier(t *testing.T) {
	ctx := context.Background()
	cluster := testConfigLogFakeCluster(t)
	team := false
	world := sidecartest.NewConfigLogSuiteWorld(ctx, cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	settings.TeamTier = func(ctx context.Context, networkId server.Id) bool {
		return team
	}
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	networkId := server.Id(world.NetworkId())
	signedHeader, rows, genesis, configPushes := world.BaseOne()
	pin, err := sequencer.AppendBase(ctx, networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	if err != nil {
		t.Fatal(err)
	}
	world.SetTip(pin)

	general, err := sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_GENERAL))
	if err != nil {
		t.Fatalf("a general repo outside the team tier: %v", err)
	}
	world.SetTip(general)
	_, err = sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_DIRECTORY))
	configLogError := &ConfigLogError{}
	if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden || configLogError.Tip.GetVersion() != general.GetVersion() ||
		!errors.Is(err, sidecar.ErrConfigRule) {
		t.Fatalf("the directory outside the team tier: %v", err)
	}
	team = true
	if directory, err := sequencer.AppendStatement(ctx, networkId, world.RepoCreate(protocol.RepoKind_REPO_KIND_DIRECTORY)); err != nil || directory.GetVersion() != general.GetVersion()+1 {
		t.Fatalf("the directory on the team tier: %v", err)
	}
}

// ACL-M2-18 (DESIGN-DETAILS 16.2.2, QD-212): api sequences a RepoPrefixRules through the shared rule table as every
// reader takes it, and refuses each row of the id-level table as the readers do, 403 with the tip and the readers'
// refusal class: a signer that is neither a repo admin nor a config writer, a record repo, a deleted repo, a body of 47,
// 48 and 303 B and one past the cap, and the 257th set in a repo, naming the limit. Each refused row's positive twin
// lands: the same device once the repo names it admin, a general repo, 304 B, the 256th set. A gate reading the log as
// the store holds it folds the 256 rows.
func TestConfigLogSequencesRepoPrefixRulesAsReadersJudgeThem(t *testing.T) {
	ctx := context.Background()
	cluster := testConfigLogFakeCluster(t)
	world := sidecartest.NewConfigLogSuiteWorld(ctx, cluster)
	settings := DefaultConfigLogSettings()
	settings.Clock = world.Clock()
	sequencer := NewConfigLogSequencer(cluster, settings, nil)
	networkId := server.Id(world.NetworkId())
	signedHeader, rows, genesis, configPushes := world.BaseOne()
	tip, err := sequencer.AppendBase(ctx, networkId, &ConfigLogBase{SignedHeader: signedHeader, Rows: rows, Genesis: genesis, ConfigPushes: configPushes})
	if err != nil {
		t.Fatal(err)
	}
	world.SetTip(tip)
	post := func(name string, signed []byte) {
		t.Helper()
		pin, err := sequencer.AppendStatement(ctx, networkId, signed)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		tip = pin
		world.SetTip(pin)
	}
	refuse := func(name string, signed []byte, class error) *ConfigLogError {
		t.Helper()
		_, err := sequencer.AppendStatement(ctx, networkId, signed)
		configLogError := &ConfigLogError{}
		if !errors.As(err, &configLogError) || configLogError.Status != http.StatusForbidden || configLogError.Tip.GetVersion() != tip.GetVersion() ||
			!errors.Is(err, class) {
			t.Errorf("%s: %v, want a 403 of %v at %d", name, err, class, tip.GetVersion())
		}
		return configLogError
	}
	repo := func(kind protocol.RepoKind) sidecar.Id {
		t.Helper()
		repoId := sidecar.Id(server.NewId())
		post("a RepoCreate", world.SignAsOwner(bridge.SignedType_SIGNED_REPO_CREATE, &protocol.RepoCreate{Header: world.Header(), RepoId: repoId.Bytes(), Kind: kind}))
		return repoId
	}
	general, record, deleted := repo(protocol.RepoKind_REPO_KIND_GENERAL), repo(protocol.RepoKind_REPO_KIND_RECORD), repo(protocol.RepoKind_REPO_KIND_GENERAL)
	post("a RepoDelete", world.SignAsOwner(bridge.SignedType_SIGNED_REPO_DELETE, &protocol.RepoDelete{
		Header: world.Header(), RepoId: deleted.Bytes(), DeletionId: sidecar.Id(server.NewId()).Bytes(),
	}))
	prefixRules := func(repoId sidecar.Id, byteCount int) *protocol.RepoPrefixRules {
		return &protocol.RepoPrefixRules{Header: world.Header(), RepoId: repoId.Bytes(), SealedRules: make([]byte, byteCount)}
	}
	asOwner := func(repoId sidecar.Id, byteCount int) []byte {
		return world.SignAsOwner(bridge.SignedType_SIGNED_REPO_PREFIX_RULES, prefixRules(repoId, byteCount))
	}

	// a member's device the owner admits, whose key the test holds: neither a repo admin nor a config writer
	memberId, deviceId := sidecar.Id(server.NewId()), sidecar.Id(server.NewId())
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	post("a MemberAdmit", world.SignAsOwner(bridge.SignedType_SIGNED_MEMBER_ADMIT, &protocol.MemberAdmit{
		Header: world.Header(), MemberId: memberId.Bytes(), Role: bridge.MemberRole_MEMBER_ROLE_MEMBER,
	}))
	post("the device's SidecarAdmit", world.SignAsOwner(bridge.SignedType_SIGNED_SIDECAR_ADMIT, &protocol.SidecarAdmit{
		Header: world.Header(), NetworkId: world.NetworkId().Bytes(), SidecarId: deviceId.Bytes(), IdentityPublicKey: publicKey,
		IdentityAlg: "ed25519", MemberId: memberId.Bytes(),
	}))
	asDevice := func(repoId sidecar.Id, byteCount int) []byte {
		t.Helper()
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(prefixRules(repoId, byteCount))
		if err != nil {
			t.Fatal(err)
		}
		signedType := bridge.SignedType_SIGNED_REPO_PREFIX_RULES
		signed, err := proto.MarshalOptions{Deterministic: true}.Marshal(&bridge.Signed{
			Body: body, SignerId: deviceId.Bytes(), Type: uint32(signedType),
			Signature: ed25519.Sign(privateKey, bridge.SignedBytes(bridge.SignedPurposeAcl, signedType, body)),
		})
		if err != nil {
			t.Fatal(err)
		}
		return signed
	}

	refuse("a signer that is neither", asDevice(general, 304), sidecar.ErrConfigAuthority)
	refuse("a record repo", asOwner(record, 304), sidecar.ErrConfigRule)
	refuse("a deleted repo", asOwner(deleted, 304), sidecar.ErrConfigRule)
	refuse("a repo the log does not hold", asOwner(sidecar.Id(server.NewId()), 304), sidecar.ErrConfigRule)
	for _, byteCount := range []int{47, 48, 303, 48 + 256*256} {
		refuse("a body past the form or the cap", asOwner(general, byteCount), sidecar.ErrConfigRule)
	}
	post("a config writer's set of 304 B", asOwner(general, 304))
	post("the device made the repo's admin", world.SignAsOwner(bridge.SignedType_SIGNED_REPO_ADMIN, &protocol.RepoAdmin{
		Header: world.Header(), RepoId: general.Bytes(), SidecarId: deviceId.Bytes(), Change: protocol.RepoAdminChange_REPO_ADMIN_CHANGE_ADD,
	}))
	post("the repo admin's set", asDevice(general, 48+2*256))
	setMaxCount := sidecar.DefaultAclPrefixSettings().SetMaxCount
	for range setMaxCount - 2 {
		post("a set up to the cap", asOwner(general, 304))
	}
	if refused := refuse("the 257th set", asOwner(general, 304), sidecar.ErrConfigRule); refused.Err == nil || !strings.Contains(refused.Err.Error(), "SetMaxCount") {
		t.Errorf("the 257th set's refusal names no limit: %v", refused.Err)
	}

	// a gate reading the log as the store holds it holds the rows api's verifier folded
	read, err := sequencer.Read(ctx, networkId, 0)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := sidecar.NewConfigLogRowsOnly(nil, read.SignedGenesis)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.ApplyRun(read.Entries, sidecar.ConfigRowsOnlySource(func(networkId sidecar.Id, rowsSha256 []byte) ([]byte, error) {
		return configLogGetRows(ctx, cluster, networkId, rowsSha256)
	})); err != nil {
		t.Fatal(err)
	}
	acl, err := gate.State().Acl()
	if err != nil {
		t.Fatal(err)
	}
	sets := acl.Repo(general).GetPrefixRules()
	if len(sets) != setMaxCount || sets[1].GetAt() <= sets[0].GetAt() || string(sets[1].GetSigner()) != string(deviceId.Bytes()) {
		t.Errorf("the gate holds %d sets", len(sets))
	}
}

package model

// api's log sequencer (TEAM-M2-13): sidecartest's shared store suite, which T2's fake api passes too
// (TEAM-M2-12), on T2's fake Blobs here and on the local stack's MinIO in
// config_log_model_localstack_test.go; network_acl_head following each append; a refusal's body; and
// the gate path, a rows-only verifier reading the log from the store, under the Postgres tripwire (I-12).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/urnetwork/sidecar"
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

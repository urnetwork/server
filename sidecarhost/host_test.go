package sidecarhost

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/sidecar"
	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
)

// sidecar/testdata/gates/table.yml, which server's reader and sidecartest's fake answer too: the gates
// as an engine reads them through ServiceHost.Gates must answer it alike.
const gateTablePath = "../../sidecar/testdata/gates/table.yml"

type gateTable struct {
	Buckets map[string]int  `yaml:"buckets"`
	Cases   []gateTableCase `yaml:"cases"`
}

type gateTableCase struct {
	Name   string          `yaml:"name"`
	File   string          `yaml:"file"`
	Broken bool            `yaml:"broken"`
	Expect gateTableExpect `yaml:"expect"`
}

type gateTableService struct {
	Enabled bool     `yaml:"enabled"`
	Writes  bool     `yaml:"writes"`
	Percent int      `yaml:"percent"`
	Admits  []string `yaml:"admits"`
	Refuses []string `yaml:"refuses"`
}

type gateTableExpect struct {
	Services map[string]gateTableService `yaml:"services"`
	Format2  *struct {
		Admits  []string `yaml:"admits"`
		Refuses []string `yaml:"refuses"`
		Purge   string   `yaml:"purge"`
		Drop    string   `yaml:"drop"`
	} `yaml:"format2"`
	Switches          map[string]bool `yaml:"switches"`
	PlaceNew          string          `yaml:"place_new"`
	Reseed            bool            `yaml:"reseed"`
	Drained           []string        `yaml:"drained"`
	SocketsControlQps *struct {
		Rate  float64 `yaml:"rate"`
		Burst int     `yaml:"burst"`
	} `yaml:"sockets_control_qps"`
	UrnetworkEdge string `yaml:"urnetwork_edge"`
	FileClusters  map[string]struct {
		SettlePassedBuild string `yaml:"settle_passed_build"`
		HddCapacityOps    int64  `yaml:"hdd_capacity_ops"`
	} `yaml:"file_clusters"`
	NoFileClusters []string `yaml:"no_file_clusters"`
}

func readGateTable(t *testing.T) *gateTable {
	t.Helper()
	data, err := os.ReadFile(gateTablePath)
	if err != nil {
		t.Fatal(err)
	}
	table := &gateTable{}
	if err := yaml.Unmarshal(data, table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 || len(table.Buckets) == 0 {
		t.Fatal("the gate table holds nothing")
	}
	return table
}

func gateTableId(t *testing.T, idStr string) connect.Id {
	t.Helper()
	id, err := connect.ParseId(idStr)
	if err != nil {
		t.Fatalf("%q: %s", idStr, err)
	}
	return id
}

// Holds gates to one case: what it names, and every other gate closed.
func requireGateTableCase(t *testing.T, table *gateTable, c gateTableCase, gates sidecar.ServiceGates) {
	t.Helper()
	bucketIds := []connect.Id{}
	for idStr := range table.Buckets {
		bucketIds = append(bucketIds, gateTableId(t, idStr))
	}
	for _, service := range []string{
		sidecar.ServiceGateEvs, sidecar.ServiceGateGkm, sidecar.ServiceGateSockets, sidecar.ServiceGateTlsedge,
		sidecar.ServiceGateMxedge, sidecar.ServiceGateSmtpedge, sidecar.ServiceGateDns, "storage",
	} {
		gate := gates.Service(service)
		want, ok := c.Expect.Services[service]
		if !ok {
			if gate.Enabled() || gate.Writes() {
				t.Errorf("%s: %s is open", c.Name, service)
			}
			for _, networkId := range bucketIds {
				if gate.Admits(networkId) {
					t.Errorf("%s: closed %s admits %s", c.Name, service, networkId)
				}
			}
			continue
		}
		if gate.Enabled() != want.Enabled || gate.Writes() != want.Writes || gate.Percent() != want.Percent {
			t.Errorf("%s: %s enabled %t writes %t percent %d, want %+v", c.Name, service, gate.Enabled(), gate.Writes(), gate.Percent(), want)
		}
		for _, idStr := range want.Admits {
			if !gate.Admits(gateTableId(t, idStr)) {
				t.Errorf("%s: %s refuses %s", c.Name, service, idStr)
			}
		}
		for _, idStr := range want.Refuses {
			if gate.Admits(gateTableId(t, idStr)) {
				t.Errorf("%s: %s admits %s", c.Name, service, idStr)
			}
		}
	}

	format2 := gates.Format2()
	if c.Expect.Format2 == nil {
		for _, networkId := range bucketIds {
			if format2.Admits(networkId) {
				t.Errorf("%s: closed format2 admits %s", c.Name, networkId)
			}
		}
		if format2.Purge() != sidecar.Format2Observe || format2.Drop() != sidecar.Format2Observe {
			t.Errorf("%s: closed format2 deletes", c.Name)
		}
	} else {
		for _, idStr := range c.Expect.Format2.Admits {
			if !format2.Admits(gateTableId(t, idStr)) {
				t.Errorf("%s: format2 refuses %s", c.Name, idStr)
			}
		}
		for _, idStr := range c.Expect.Format2.Refuses {
			if format2.Admits(gateTableId(t, idStr)) {
				t.Errorf("%s: format2 admits %s", c.Name, idStr)
			}
		}
		if string(format2.Purge()) != c.Expect.Format2.Purge || string(format2.Drop()) != c.Expect.Format2.Drop {
			t.Errorf("%s: format2 purge %s drop %s", c.Name, format2.Purge(), format2.Drop())
		}
	}

	for _, name := range []string{sidecar.GateSwitchPush, sidecar.GateSwitchWake, sidecar.GateSwitchBridge, sidecar.GateSwitchEscrowDrawOrder, "other"} {
		if gates.Switch(name) != c.Expect.Switches[name] {
			t.Errorf("%s: switch %s is %t", c.Name, name, gates.Switch(name))
		}
	}
	if gates.PlaceNew() != c.Expect.PlaceNew {
		t.Errorf("%s: place_new %q", c.Name, gates.PlaceNew())
	}
	if gates.Reseed() != c.Expect.Reseed {
		t.Errorf("%s: reseed %t", c.Name, gates.Reseed())
	}
	if !slices.Equal(gates.Drained(), c.Expect.Drained) && (len(gates.Drained()) != 0 || len(c.Expect.Drained) != 0) {
		t.Errorf("%s: drained %q", c.Name, gates.Drained())
	}
	rate, burst, ok := gates.SocketsControlQps()
	if c.Expect.SocketsControlQps == nil {
		if ok {
			t.Errorf("%s: sockets_control_qps set", c.Name)
		}
	} else if !ok || rate != c.Expect.SocketsControlQps.Rate || burst != c.Expect.SocketsControlQps.Burst {
		t.Errorf("%s: sockets_control_qps %v %d %t", c.Name, rate, burst, ok)
	}
	edgeId, ok := gates.UrnetworkEdge()
	if c.Expect.UrnetworkEdge == "" {
		if ok {
			t.Errorf("%s: urnetwork_edge %s", c.Name, edgeId)
		}
	} else if !ok || edgeId != gateTableId(t, c.Expect.UrnetworkEdge) {
		t.Errorf("%s: urnetwork_edge %s %t", c.Name, edgeId, ok)
	}
	for clusterId, want := range c.Expect.FileClusters {
		got := gates.FileCluster(clusterId)
		if got.SettlePassedBuild != want.SettlePassedBuild || got.HddCapacityOps != want.HddCapacityOps {
			t.Errorf("%s: file cluster %s %+v", c.Name, clusterId, got)
		}
	}
	for _, clusterId := range c.Expect.NoFileClusters {
		if got := gates.FileCluster(clusterId); got != (sidecar.FileClusterGate{}) {
			t.Errorf("%s: file cluster %s passed %+v", c.Name, clusterId, got)
		}
	}
	if gates.RetryAfter() <= 0 {
		t.Errorf("%s: no retry after", c.Name)
	}
}

// The gates an engine reads through ServiceHost.Gates answer the table as server's reader and the
// fake do, each case read fresh, as a process's first read is. A broken file on a first read opens
// nothing.
func TestGatesAnswerTheGateTable(t *testing.T) {
	table := readGateTable(t)
	for _, c := range table.Cases {
		pop := server.Testing_PushSidecarConfig(c.File)
		ctx, cancel := context.WithCancel(t.Context())
		gates := NewGates(ctx, nil)
		requireGateTableCase(t, table, c, gates)
		cancel()
		pop()
	}
}

// A renewal takes the reader's newest read, and a later broken read keeps the last good one.
func TestGatesRenewFromTheReader(t *testing.T) {
	popOpen := server.Testing_PushSidecarConfig("sidecar:\n  evs: {enabled: true, percent: 100}\n")
	defer popOpen()
	// no renewal but the test's own
	gates := newGates(t.Context(), &GatesSettings{PollInterval: time.Hour, RetryAfter: time.Minute}, server.CurrentSidecarConfig)
	networkId := connect.NewId()
	if !gates.Service(sidecar.ServiceGateEvs).Admits(networkId) {
		t.Fatal("the open gate refused")
	}
	popClosed := server.Testing_PushSidecarConfig("sidecar:\n  evs: {enabled: false}\n")
	defer popClosed()
	if !gates.Service(sidecar.ServiceGateEvs).Admits(networkId) {
		t.Fatal("the gate moved before a renewal")
	}
	gates.renew()
	if gates.Service(sidecar.ServiceGateEvs).Admits(networkId) {
		t.Fatal("the renewal kept the old read")
	}
}

// The host a block's main builds: every ServiceHost field this package owns, the options' others, and
// Tasks only with Postgres (plan-OPS-small-readings #8).
func TestNewServiceHostAssemblesTheHost(t *testing.T) {
	pop := server.Testing_PushSidecarConfig("")
	defer pop()
	clock := sidecar.RealClock{}
	host := NewServiceHost(t.Context(), &ServiceHostOptions{Clock: clock})
	if host.Authenticator == nil || host.Metrics == nil || host.Redis == nil || host.Gates == nil {
		t.Fatalf("host %+v", host)
	}
	if host.Tasks != nil {
		t.Fatal("a host with no Postgres has Tasks")
	}
	if host.Blobs != nil || host.Push != nil || host.Quota != nil || host.ServiceClient != nil || host.ClockCheck != nil || host.Budget != nil || host.Abuse != nil {
		t.Fatalf("a field no option gave is set: %+v", host)
	}
	if host.Clock != clock {
		t.Fatal("the clock is not the option's")
	}

	tasks := NewTasks(nil)
	host = NewServiceHost(t.Context(), &ServiceHostOptions{Postgres: true, Tasks: tasks})
	if host.Tasks != tasks {
		t.Fatal("a host with Postgres does not hold the given Tasks")
	}
	host = NewServiceHost(t.Context(), &ServiceHostOptions{Postgres: true})
	if host.Tasks == nil {
		t.Fatal("a host with Postgres has no Tasks")
	}
}

// A block's main hands its abuse gate to the engines, and one that hands none has loaded no network, so
// every gate reading it refuses (ABUSE-M5-02, I-12).
func TestNewServiceHostTakesTheAbuseGate(t *testing.T) {
	pop := server.Testing_PushSidecarConfig("")
	defer pop()
	networkId := connect.NewId()

	host := NewServiceHost(t.Context(), &ServiceHostOptions{})
	if standing, ok := host.AbuseOrUnloaded().Standing(networkId); ok || standing != "" {
		t.Errorf("a host given no gate answers %q, %v", standing, ok)
	}
	if verified, ok := host.AbuseOrUnloaded().Verified(networkId); ok || verified {
		t.Errorf("a host given no gate answers verified %v, %v", verified, ok)
	}

	abuse := sidecartest.NewFakeAbuseGate()
	abuse.Set(networkId, sidecar.AbuseStandingRestricted, true)
	host = NewServiceHost(t.Context(), &ServiceHostOptions{Abuse: abuse})
	if host.Abuse != abuse {
		t.Fatal("the gate is not the option's")
	}
	if standing, ok := host.AbuseOrUnloaded().Standing(networkId); !ok || standing != sidecar.AbuseStandingRestricted {
		t.Errorf("standing %q, %v", standing, ok)
	}
}

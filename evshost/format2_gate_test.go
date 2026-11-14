package evshost

import (
	"fmt"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/sidecarhost"
)

// A network id whose gate bucket is bucket.
func hostTestNetworkAt(t testing.TB, bucket int) sidecar.Id {
	t.Helper()
	for i := 0; i < 100*1000; i += 1 {
		networkId := connect.NewId()
		if sidecar.ServiceGateBucket(networkId) == bucket {
			return sidecar.Id(networkId)
		}
	}
	t.Fatalf("no network id in bucket %d", bucket)
	return sidecar.Id{}
}

// The block's format2 gate over server's reader of sidecar.yml, as sidecarhost builds a block's gates: each file
// is a process's first read. A listed network and one under the percent are in only while `enabled`; an absent
// file, an absent block and a file that does not parse read as off with purge and drop observe; a mode deletes
// only when it is exactly `on`.
func TestHostFormat2GateReadsServersSidecarYml(t *testing.T) {
	listed, at40, at41 := hostTestNetworkAt(t, 90), hostTestNetworkAt(t, 40), hostTestNetworkAt(t, 41)
	listedStr := connect.Id(listed).String()
	for _, c := range []struct {
		name   string
		file   string
		admits []sidecar.Id
		out    []sidecar.Id
		purge  sidecar.Format2Mode
		drop   sidecar.Format2Mode
	}{
		{name: "no file", file: "", out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "no format2 block", file: "sidecar:\n  evs: {enabled: true, percent: 100, blocks: 1}\n", out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "a file that does not parse", file: "sidecar:\n  format2: {enabled: true, percent: 100, purge: on\n", out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "the design's default", file: "sidecar:\n  format2: {enabled: false, networks: [], percent: 0, purge: observe, drop: observe}\n", out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "listed while disabled", file: fmt.Sprintf("sidecar:\n  format2: {enabled: false, networks: [%q], percent: 100}\n", listedStr), out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "listed, percent 0", file: fmt.Sprintf("sidecar:\n  format2: {enabled: true, networks: [%q], percent: 0}\n", listedStr), admits: []sidecar.Id{listed}, out: []sidecar.Id{at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
		{name: "percent 41, purge on", file: "sidecar:\n  format2: {enabled: true, percent: 41, purge: on}\n", admits: []sidecar.Id{at40}, out: []sidecar.Id{listed, at41}, purge: sidecar.Format2On, drop: sidecar.Format2Observe},
		{name: "percent 42, drop on", file: "sidecar:\n  format2: {enabled: true, percent: 42, drop: \"on\"}\n", admits: []sidecar.Id{at40, at41}, out: []sidecar.Id{listed}, purge: sidecar.Format2Observe, drop: sidecar.Format2On},
		{name: "modes that are not on", file: "sidecar:\n  format2: {enabled: true, purge: \"On\", drop: delete}\n", out: []sidecar.Id{listed, at40, at41}, purge: sidecar.Format2Observe, drop: sidecar.Format2Observe},
	} {
		pop := server.Testing_PushSidecarConfig(c.file)
		evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
			host.Gates = sidecarhost.NewGates(t.Context(), nil)
		})
		gate := evsHost.Format2Gate()
		for _, networkId := range c.admits {
			if !gate.Admits(networkId) {
				t.Errorf("%s: bucket %d refused", c.name, sidecar.ServiceGateBucket(connect.Id(networkId)))
			}
		}
		for _, networkId := range c.out {
			if gate.Admits(networkId) {
				t.Errorf("%s: bucket %d admitted", c.name, sidecar.ServiceGateBucket(connect.Id(networkId)))
			}
		}
		if gate.PurgeMode() != c.purge || gate.DropMode() != c.drop {
			t.Errorf("%s: purge %s drop %s, want %s and %s", c.name, gate.PurgeMode(), gate.DropMode(), c.purge, c.drop)
		}
		pop()
	}
}

// A host given no gates at all answers closed and observe, as a missing file does.
func TestHostFormat2GateWithoutGatesIsClosed(t *testing.T) {
	evsHost := newHostTestHost(t, func(host *sidecar.ServiceHost, deps *HostDeps) {
		host.Gates = nil
	})
	gate := evsHost.Format2Gate()
	if gate.Admits(hostTestNetworkAt(t, 0)) || gate.PurgeMode() != sidecar.Format2Observe || gate.DropMode() != sidecar.Format2Observe {
		t.Fatal("a host with no gates opened format 2")
	}
}

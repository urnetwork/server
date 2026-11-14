package server

import (
	"crypto/sha256"
	"encoding/binary"
	"math"
	"net/http"
	"slices"
	"strconv"
	"time"
)

// What §19.11's gates in sidecar.yml answer (D-OPS-10; sidecar_config.go reads
// the file). A network is in a service's gate when the gate is enabled and the
// network is listed, or enabled and SidecarGateBucket(network) is under its
// percent: one rule and one bucket for every gate, so format 2's gate
// (EVS2-M10-05) and §6's core topic agree with the engines. A disabled service
// stays ready and answers 503 with Retry-After (WriteGateClosed), and
// `writes: false` is the incident switch.
//
// Engines cannot import server, so they read these through the root's
// sidecar.ServiceGates, which sidecarhost adapts from this reader. The sidecar
// root computes the bucket on its own, and sidecar/testdata/gates/table.yml
// holds both readers, this one and sidecartest's fake, to one table.

// The services §19.11 gates by name, as the file spells them.
const (
	SidecarGateEvs      = "evs"
	SidecarGateFormat2  = "format2"
	SidecarGateGkm      = "gkm"
	SidecarGateSockets  = "sockets"
	SidecarGateTlsedge  = "tlsedge"
	SidecarGateMxedge   = "mxedge"
	SidecarGateSmtpedge = "smtpedge"
	SidecarGateDns      = "dns"
)

// §19.11's switches, each only `enabled`.
const (
	// §6's EnableSidecarPush
	SidecarSwitchPush = "push"
	// §19.4's push sender
	SidecarSwitchWake = "wake"
	// §17's EnableBridge
	SidecarSwitchBridge = "bridge"
	// §20.9 (D-QUOTA-9): on only once M6's Gn-out registers MG6's index, and
	// never off while a team_credit balance is in window
	SidecarSwitchEscrowDrawOrder = "escrow_draw_order"
	// §8.10's contract gate (ACL-M4-01): CreateContract consults the gate its
	// process installed only while this is on
	SidecarSwitchAclContractGate = "acl_contract_gate"
)

// A network's bucket, 0 to 99: the first 8 bytes of SHA-256 over the id's 16
// bytes, big-endian, mod 100 (plan-OPS-small-readings #2). Every percent gate
// reads it, and sidecar.ServiceGateBucket computes the same; the gate table pins
// both, so a network's place in a rollout never moves between releases.
func SidecarGateBucket(networkId Id) int {
	sum := sha256.Sum256(networkId[:])
	return int(binary.BigEndian.Uint64(sum[:8]) % 100)
}

// One service's gate as one read of the file left it. The zero value and a nil
// gate are closed. Immutable once read.
type SidecarServiceGate struct {
	enabled    bool
	writes     bool
	networkIds map[Id]bool
	percent    int
}

// A gate from its yaml: writes defaults true inside a present gate, a percent
// out of range reads 0, and an id that does not parse is skipped.
func newSidecarServiceGate(name string, y *sidecarGateYaml) *SidecarServiceGate {
	gate := &SidecarServiceGate{
		enabled:    y.Enabled,
		writes:     y.Writes == nil || *y.Writes,
		networkIds: map[Id]bool{},
		percent:    y.Percent,
	}
	if gate.percent < 0 || 100 < gate.percent {
		gate.percent = 0
	}
	for _, networkId := range parseSidecarConfigIds(name+".networks", y.Networks) {
		gate.networkIds[networkId] = true
	}
	return gate
}

// The gate's `enabled`. Off, it admits no network, listed or not.
func (self *SidecarServiceGate) Enabled() bool {
	return self != nil && self.enabled
}

// Whether the service takes writes: enabled, with `writes` left on.
func (self *SidecarServiceGate) Writes() bool {
	return self.Enabled() && self.writes
}

// Whether the network may use the service: enabled, and listed or under the
// percent by bucket.
func (self *SidecarServiceGate) Admits(networkId Id) bool {
	if !self.Enabled() {
		return false
	}
	return self.networkIds[networkId] || SidecarGateBucket(networkId) < self.percent
}

// The share of networks admitted by bucket, 0 to 100.
func (self *SidecarServiceGate) Percent() int {
	if self == nil {
		return 0
	}
	return self.percent
}

// The listed networks, sorted, for an adapter that rebuilds the gate.
func (self *SidecarServiceGate) NetworkIds() []Id {
	if self == nil {
		return nil
	}
	networkIds := make([]Id, 0, len(self.networkIds))
	for networkId := range self.networkIds {
		networkIds = append(networkIds, networkId)
	}
	slices.SortFunc(networkIds, func(a Id, b Id) int { return a.Cmp(b) })
	return networkIds
}

// format2's purge and drop modes. Anything but exactly `on` reads observe, so
// every failure keeps content.
type SidecarFormat2Mode string

const (
	// logs each candidate, or runs a drop job through verify, and deletes nothing
	SidecarFormat2Observe SidecarFormat2Mode = "observe"
	// deletes, after D-TEST-12's soak for purge and D-TEST-18's for drop
	SidecarFormat2On SidecarFormat2Mode = "on"
)

func parseSidecarFormat2Mode(mode string) SidecarFormat2Mode {
	if mode == string(SidecarFormat2On) {
		return SidecarFormat2On
	}
	return SidecarFormat2Observe
}

// §11.6's format 2 gate, with v1's purge and drop modes. A nil gate admits
// nothing and deletes nothing. Immutable once read.
type SidecarFormat2Gate struct {
	gate  *SidecarServiceGate
	purge SidecarFormat2Mode
	drop  SidecarFormat2Mode
}

// Whether the network's pushes may carry format 2.
func (self *SidecarFormat2Gate) Admits(networkId Id) bool {
	return self != nil && self.gate.Admits(networkId)
}

// The gate's networks and percent, as every other gate's.
func (self *SidecarFormat2Gate) Gate() *SidecarServiceGate {
	if self == nil {
		return nil
	}
	return self.gate
}

// Whether purge deletes or only logs its candidates.
func (self *SidecarFormat2Gate) Purge() SidecarFormat2Mode {
	if self == nil || self.purge != SidecarFormat2On {
		return SidecarFormat2Observe
	}
	return SidecarFormat2On
}

// Whether drop jobs delete or only verify.
func (self *SidecarFormat2Gate) Drop() SidecarFormat2Mode {
	if self == nil || self.drop != SidecarFormat2On {
		return SidecarFormat2Observe
	}
	return SidecarFormat2On
}

// plan-EVSB-settle-gate's entry for one file cluster. A cluster's settle check
// passes only while every host's MinIO build equals SettlePassedBuild, which
// EVSB-M2-18 compares with admin info, so the zero value has passed nothing.
type SidecarFileCluster struct {
	// the build that passed filecluster-suite and FS-114; empty, none has
	SettlePassedBuild string
	// what §22.8's M3 hdd run measured, in drive operations a second; zero,
	// unmeasured, so evs refuses no upload on HDD grounds and drop jobs pause
	HddCapacityOps int64
}

// The named service's gate. A service the file holds no gate for, or one this
// reader does not know, answers a closed gate, never nil.
func (self *SidecarConfig) Service(name string) *SidecarServiceGate {
	if gate, ok := self.serviceGates[name]; ok {
		return gate
	}
	return &SidecarServiceGate{}
}

// §11.6's format 2 gate. Never nil.
func (self *SidecarConfig) Format2() *SidecarFormat2Gate {
	if self.format2 == nil {
		return &SidecarFormat2Gate{gate: &SidecarServiceGate{}}
	}
	return self.format2
}

// Whether the named switch is on: push, wake, bridge or escrow_draw_order.
func (self *SidecarConfig) Switch(name string) bool {
	return self.switches[name]
}

// §6's EnableSidecarPush.
func (self *SidecarConfig) Push() bool {
	return self.Switch(SidecarSwitchPush)
}

// §19.4's push sender.
func (self *SidecarConfig) Wake() bool {
	return self.Switch(SidecarSwitchWake)
}

// §17's EnableBridge.
func (self *SidecarConfig) Bridge() bool {
	return self.Switch(SidecarSwitchBridge)
}

// §20.9's draw order (D-QUOTA-9), read at every draw and return site.
func (self *SidecarConfig) EscrowDrawOrder() bool {
	return self.Switch(SidecarSwitchEscrowDrawOrder)
}

// §8.10's contract gate, read at every CreateContract of a process that
// installed one.
func (self *SidecarConfig) AclContractGate() bool {
	return self.Switch(SidecarSwitchAclContractGate)
}

// Whether the file names escrow_draw_order at all, on or off. A file that names it
// off is an explicit off, which §20.9's gate refuses while a team_credit balance is
// in window (model.EscrowDrawOrderEnabled); a file that never named it leaves a
// process that never read it on in today's order.
func (self *SidecarConfig) EscrowDrawOrderNamed() bool {
	return self.escrowDrawOrderNamed
}

// §19.10: re-seed mode, after site loss only. Absent is false.
func (self *SidecarConfig) Reseed() bool {
	return self.reseed
}

// §19.10's restore hold (plan-OPS-restore-hold, QD-076): while true, api's
// public routes but its status route, the token exchange with them, answer 503
// with Retry-After, and CreateContract answers Setup; open streams, sockets and
// edges are untouched. The restore runbook sets it before the restored primary
// answers, and clears it once the api step ends.
func (self *SidecarConfig) RestoreHold() bool {
	return self.restoreHold
}

// §19.9: the metadata cluster that takes new repos. Empty, when the file names
// none, means the first cluster.
func (self *SidecarConfig) PlaceNew() string {
	return self.placeNew
}

// W1: the lb interfaces the dns edges leave out of their answers.
func (self *SidecarConfig) Drained() []string {
	return slices.Clone(self.drained)
}

// §9.10 (D-SOCK-15): the control rate and burst per process and calling client.
// ok is false when the file sets none that is positive, and the engine keeps its
// settings' own.
func (self *SidecarConfig) SocketsControlQps() (rate float64, burst int, ok bool) {
	return self.socketsRate, self.socketsBurst, self.socketsSet
}

// §6.10.1, §9.6: the urnetwork-edge platform network, when the file names one.
func (self *SidecarConfig) UrnetworkEdge() (Id, bool) {
	if self.urnetworkEdge == nil {
		return Id{}, false
	}
	return *self.urnetworkEdge, true
}

// §20.6's Empty balance (QD-639): how many evs blocks serve the env, which each
// evs process divides evs_unpaid_read_rate by. The operator sets it with
// services.yml's evs block count; no host derives it from its own view of its
// peers. ok is false when the file sets none that is at least 1: there is no
// default, and server/evshost starts no block whose gate is enabled without one.
func (self *SidecarConfig) EvsBlocks() (blocks int, ok bool) {
	return self.evsBlocks, 1 <= self.evsBlocks
}

// plan-EVSB-settle-gate's entry for one cluster; an absent entry has passed
// nothing.
func (self *SidecarConfig) FileCluster(clusterId string) (SidecarFileCluster, bool) {
	cluster, ok := self.fileClusters[clusterId]
	return cluster, ok
}

// The gates open to one network, for §6's core topic and GET
// /sidecar/sync/core's gates (plan-CORE-core-hint-body #2): each per-network
// gate that admits it, in §19.11's order, then dns while it is enabled and the
// client-visible switches that are on. wake and escrow_draw_order are the
// server's own and never listed.
func (self *SidecarConfig) GatesFor(networkId Id) []string {
	gates := []string{}
	for _, name := range []string{SidecarGateEvs, SidecarGateFormat2, SidecarGateGkm, SidecarGateSockets, SidecarGateTlsedge, SidecarGateMxedge, SidecarGateSmtpedge} {
		admits := false
		if name == SidecarGateFormat2 {
			admits = self.Format2().Admits(networkId)
		} else {
			admits = self.Service(name).Admits(networkId)
		}
		if admits {
			gates = append(gates, name)
		}
	}
	if self.Service(SidecarGateDns).Enabled() {
		gates = append(gates, SidecarGateDns)
	}
	for _, name := range []string{SidecarSwitchPush, SidecarSwitchBridge} {
		if self.Switch(name) {
			gates = append(gates, name)
		}
	}
	return gates
}

// Answers a request a closed gate refuses: 503, with Retry-After in whole
// seconds, the reread interval when retryAfter is not positive. A closed
// service stays ready, so warpctl does not revert it (§19.11).
func WriteGateClosed(w http.ResponseWriter, retryAfter time.Duration) {
	if retryAfter <= 0 {
		retryAfter = defaultSidecarConfigReader.settings.RereadInterval
	}
	seconds := max(int(math.Ceil(retryAfter.Seconds())), 1)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.WriteHeader(http.StatusServiceUnavailable)
}

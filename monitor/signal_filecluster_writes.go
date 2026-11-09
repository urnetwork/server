package monitor

// Signal filecluster-writes implements SIGNALS.md §29.2 (EVSB-M3-01; §19.6's parity row, owner decision 145,
// FS-63, §19.10's first-cluster row, D-OPS-9, FS-50). Every 30 s it reads each file cluster's admin info:
//
//   - filecluster-refusing-writes pages §19.8's rota at any hour from M3 while a set has fewer drives online
//     than its write quorum, N/2 + 1 of its N (5 of 8, 9 of 16), or while the cluster shows other than one
//     pool, on which server.FileCluster refuses every write. While the first metadata cluster refuses writes,
//     every cluster's do too, since each checks rev/ there (§19.10), so the first's page names that;
//   - filecluster-multi-pool warns on a cluster showing more than one pool (§19.8, FS-63);
//   - filecluster-writes-unobservable reports a cluster whose admin info did not read.
//
// A drive counts as online when its host answers and admin info calls it ok, whether it rebuilds or not. A
// drive on a MinIO host whose clock is unsure also sits out writes (§19.6's clocks row); that shows as
// filecluster-settle's clock warn, not here.

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/urnetwork/server"
)

func NewFileClusterWritesSignal() Signal {
	return &signalAdapter{
		number: "29.2", key: "filecluster-writes", name: "File-cluster write quorum and pools",
		probe: fileClusterWritesProbe{},
	}
}

const fileClusterWritesProbeId = "filecluster/writes"

// SIGNALS.md §29.2's classes.
const (
	fileClusterWritesClassRefusing     = "filecluster-refusing-writes"
	fileClusterWritesClassMultiPool    = "filecluster-multi-pool"
	fileClusterWritesClassUnobservable = "filecluster-writes-unobservable"
)

type fileClusterWritesProbe struct{}

func (fileClusterWritesProbe) id() string             { return fileClusterWritesProbeId }
func (fileClusterWritesProbe) tier() string           { return tierPage }
func (fileClusterWritesProbe) cadence() time.Duration { return 30 * time.Second }

func (self fileClusterWritesProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings := env.cfg.sidecarStorage
	if gate, ok := fileClusterSignalGate(settings, fileClusterWritesProbeId, fileClusterWritesClassUnobservable); ok {
		return []finding{gate}, nil
	}
	clusterIds := settings.AdminSource.ClusterIds()
	findings := []finding{}
	for index, clusterId := range clusterIds {
		info, err := settings.AdminSource.ReadAdminInfo(ctx, clusterId)
		if err != nil {
			findings = append(findings, self.writesFinding(settings, fileClusterWritesClassUnobservable, clusterId,
				"the cluster's admin info did not read, so whether it takes writes is unknown", err.Error()))
			continue
		}
		// the source lists the first metadata cluster first
		findings = append(findings, self.evaluate(settings, clusterId, index == 0, info)...)
	}
	return findings, nil
}

// One cluster's findings from one admin info read.
func (self fileClusterWritesProbe) evaluate(settings *SidecarStorageSettings, clusterId string, first bool, info *server.FileClusterAdminInfo) []finding {
	hostOnlines := map[string]bool{}
	for _, host := range info.Hosts {
		hostOnlines[host.Endpoint] = host.State == fileClusterHostOnline
	}
	setDriveCounts := map[string]int{}
	setOnlineCounts := map[string]int{}
	for _, drive := range info.Drives {
		set := fileClusterSetName(drive.PoolIndex, drive.SetIndex)
		setDriveCounts[set] += 1
		if online, listed := hostOnlines[drive.HostEndpoint]; listed && !online {
			continue
		}
		if drive.State == fileClusterDriveOk {
			setOnlineCounts[set] += 1
		}
	}
	short := []string{}
	for set, driveCount := range setDriveCounts {
		if quorum := driveCount/2 + 1; setOnlineCounts[set] < quorum {
			short = append(short, fmt.Sprintf("set %s %d of %d online, quorum %d", set, setOnlineCounts[set], driveCount, quorum))
		}
	}
	slices.Sort(short)

	findings := []finding{}
	reasons := slices.Clone(short)
	if info.PoolCount != 1 {
		reasons = append(reasons, fmt.Sprintf("%d pools", info.PoolCount))
	}
	if len(info.Drives) == 0 {
		reasons = append(reasons, "no drive listed")
	}
	if 0 < len(reasons) {
		symptom := "the cluster refuses writes"
		if first {
			symptom += "; it is the first metadata cluster, so every cluster's writes stop with it (§19.10)"
		}
		findings = append(findings, self.writesFinding(settings, fileClusterWritesClassRefusing, clusterId, symptom, strings.Join(reasons, "; ")))
	} else {
		findings = append(findings, healthyFinding(fileClusterWritesProbeId, tierPage, fileClusterWritesClassRefusing, clusterId))
	}
	if 1 < info.PoolCount {
		findings = append(findings, self.writesFinding(settings, fileClusterWritesClassMultiPool, clusterId,
			"the cluster shows more than one pool: the metadata tier grows only by whole clusters (owner decision 145)",
			fmt.Sprintf("pools=%d", info.PoolCount)))
	} else {
		findings = append(findings, healthyFinding(fileClusterWritesProbeId, tierWarn, fileClusterWritesClassMultiPool, clusterId))
	}
	return findings
}

// A finding of class at the cluster, sustained and tiered as the class is.
func (self fileClusterWritesProbe) writesFinding(settings *SidecarStorageSettings, class string, clusterId string, symptom string, observed string) finding {
	finding := finding{
		probeId:  fileClusterWritesProbeId,
		tier:     tierWarn,
		class:    class,
		target:   clusterId,
		symptom:  symptom,
		observed: observed,
		playbook: "SIGNALS.md §29.2; §19.10's file cluster rows (DESIGN.md)",
	}
	switch class {
	case fileClusterWritesClassRefusing:
		finding.tier = tierPage
		finding.sustain = settings.PageSustain
		finding.action = "Restore the set's quorum: bring its hosts or drives back, or within 1 h of the page move a host's drives into the cold spare chassis or fit spare drives (§19.10). A cluster with more than one pool takes no write until the extra pool is gone."
	case fileClusterWritesClassMultiPool:
		finding.sustain = settings.WarnSustain
		finding.action = "Remove the pool: a pool checks preconditions only inside itself (§19.9), so the cluster's writes stay refused while it stands."
	case fileClusterWritesClassUnobservable:
		finding.sustain = 1
		finding.unobservable = true
	}
	return finding
}

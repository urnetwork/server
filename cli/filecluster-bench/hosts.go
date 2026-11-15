package main

// The hosts mode (EVSB-M3-03; §22.8's hosts row, §19.6's parity and durability rows, D-OPS-14), on a cluster before
// M3's pilot, each of the segment and metadata clusters in turn:
//
//	protected   probe objects written with every host up, so every shard exists
//	one down    the first host taken down: reads and writes go on, probes written while it is out
//	two down    a second host taken down: writes get 503 (or an unknown outcome, which the client retries as one),
//	            the protected probes still read, and how the probes written while the first was out read is recorded
//	return      both hosts brought back, as the cold spare returns one (§19.10's runbook): writes resume, and the time
//	            until admin info shows every drive online and none healing is the heal's, with the bytes the returned
//	            hosts' drives hold over it as its throughput, an estimate
//
// Hosts go down and up through the operator's templates, -host_down_command and -host_up_command, with {host} the
// host's name from admin info, {index} its place in the vault file's endpoint list from 1 and {cluster} the
// cluster's id: on T3 a docker stop and start of its containers, on production what the rota does by hand or by
// its tooling. No host is named here. The drive-level case of FS-40 (a write missing one drive of a protected set,
// then two hosts down) needs a drive taken out alone, which no template here does. Not safe for concurrent use.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/sidecar"

	"github.com/urnetwork/server"
)

// What the hosts mode runs. DefaultHostsSettings is the run's.
type HostsSettings struct {
	// Probes written in each state: 64, each ProbeByteCount bytes, 16 KiB.
	ProbeCount     int
	ProbeByteCount int64
	// The hosts taken down, by place in the endpoint list from 1: the first and the third, as FS-40's A and C.
	FirstHost  int
	SecondHost int
	// How long the cluster sits with a state before its probes, so admin info and the hosts see it: 30 s.
	Settle time.Duration
	// How long both hosts stay down while writes are probed: 2 min, well inside §19.6's 4 h RTO, which the
	// production drill measures by the cold spare's own return.
	HoldDown time.Duration
	// How often admin info is read while the heal runs, and the most the run waits for it: 30 s and 48 h.
	HealPollInterval time.Duration
	HealTimeout      time.Duration
	// §19.6's RTO for a host: 4 h.
	Rto time.Duration
	// Each probe's own deadline: 15 s, past the store's 10 s, so a refusal is the store's answer.
	ProbeTimeout time.Duration
	// The tries a probe makes past an unknown outcome, as evs retries one, the client having moved off the host that
	// failed it: 3. A write whose retry meets 412 had landed on an earlier try.
	ProbeRetryCount  int
	CleanupListLimit int
	CleanupInFlight  int
}

func DefaultHostsSettings() *HostsSettings {
	return &HostsSettings{
		ProbeCount:       64,
		ProbeByteCount:   16 * 1024,
		FirstHost:        1,
		SecondHost:       3,
		Settle:           30 * time.Second,
		HoldDown:         2 * time.Minute,
		HealPollInterval: 30 * time.Second,
		HealTimeout:      48 * time.Hour,
		Rto:              4 * time.Hour,
		ProbeTimeout:     15 * time.Second,
		ProbeRetryCount:  3,
		CleanupListLimit: 1000,
		CleanupInFlight:  64,
	}
}

// What a state's probes found.
type HostsProbe struct {
	State string `json:"state"`
	// Writes tried, and those stored, refused (an answer such as 503) and unknown (no answer in time).
	WritesTried   int `json:"writes_tried"`
	WritesStored  int `json:"writes_stored"`
	WritesRefused int `json:"writes_refused"`
	WritesUnknown int `json:"writes_unknown"`
	// Reads of the protected probes, and of those written while one host was out.
	ProtectedTried int `json:"protected_tried"`
	ProtectedRead  int `json:"protected_read"`
	DegradedTried  int `json:"degraded_tried"`
	DegradedRead   int `json:"degraded_read"`
}

// One cluster's drill.
type HostsCluster struct {
	ClusterId string        `json:"cluster_id"`
	Kind      string        `json:"kind"`
	Probes    []*HostsProbe `json:"probes"`
	// From the second host's going down to the first write stored after the return.
	WritesRefusedSeconds float64 `json:"writes_refused_seconds"`
	// From the return until every drive was online and none healing, and whether that came within the timeout.
	HealSeconds float64 `json:"heal_seconds"`
	HealEnded   bool    `json:"heal_ended"`
	// The bytes the returned hosts' drives held once healed, over the heal's time: an estimate of its throughput.
	HealByteCount      uint64   `json:"heal_byte_count"`
	HealBytesPerSecond float64  `json:"heal_bytes_per_second"`
	Errors             []string `json:"errors"`
}

// A hosts run's file.
type HostsMeasurement struct {
	RowHeader
	Clusters []*HostsCluster `json:"clusters"`
}

// What a hosts run found against §22.8's hosts row.
type HostsVerdict struct {
	Pass    bool     `json:"pass"`
	Reasons []string `json:"reasons"`
}

// The probe of state in cluster, nil when it has none.
func (self *HostsCluster) probe(state string) *HostsProbe {
	for _, probe := range self.Probes {
		if probe.State == state {
			return probe
		}
	}
	return nil
}

// The drill's states.
const (
	HostsStateProtected = "protected"
	HostsStateOneDown   = "one-down"
	HostsStateTwoDown   = "two-down"
	HostsStateReturned  = "returned"
)

// Judges a hosts run: see the file header.
func JudgeHosts(measurement *HostsMeasurement, settings *HostsSettings) *HostsVerdict {
	verdict := &HostsVerdict{}
	fail := func(format string, args ...any) {
		verdict.Reasons = append(verdict.Reasons, fmt.Sprintf(format, args...))
	}
	if measurement.Smoke {
		fail("it is a smoke run, which proves nothing")
	}
	if len(measurement.CommitSet) == 0 {
		fail("it names no build")
	}
	kinds := map[string]bool{}
	for _, cluster := range measurement.Clusters {
		kinds[cluster.Kind] = true
		for _, err := range cluster.Errors {
			fail("%s: %s", cluster.ClusterId, err)
		}
		protected := cluster.probe(HostsStateProtected)
		oneDown := cluster.probe(HostsStateOneDown)
		twoDown := cluster.probe(HostsStateTwoDown)
		returned := cluster.probe(HostsStateReturned)
		if protected == nil || oneDown == nil || twoDown == nil || returned == nil {
			fail("%s: the drill did not reach every state", cluster.ClusterId)
			continue
		}
		if protected.WritesStored != protected.WritesTried || protected.WritesTried == 0 {
			fail("%s: %d of %d writes stored with every host up", cluster.ClusterId, protected.WritesStored, protected.WritesTried)
		}
		if oneDown.WritesStored != oneDown.WritesTried || oneDown.ProtectedRead != oneDown.ProtectedTried {
			fail("%s: with one host down %d of %d writes stored and %d of %d protected reads answered", cluster.ClusterId, oneDown.WritesStored, oneDown.WritesTried, oneDown.ProtectedRead, oneDown.ProtectedTried)
		}
		if 0 < twoDown.WritesStored {
			fail("%s: with a second host down %d writes were stored, where every one must get 503", cluster.ClusterId, twoDown.WritesStored)
		}
		if twoDown.ProtectedRead != twoDown.ProtectedTried {
			fail("%s: with a second host down %d of %d protected probes read", cluster.ClusterId, twoDown.ProtectedRead, twoDown.ProtectedTried)
		}
		if returned.WritesStored != returned.WritesTried || returned.ProtectedRead != returned.ProtectedTried || returned.DegradedRead != returned.DegradedTried {
			fail("%s: after the return %d of %d writes stored, %d of %d protected and %d of %d degraded probes read", cluster.ClusterId,
				returned.WritesStored, returned.WritesTried, returned.ProtectedRead, returned.ProtectedTried, returned.DegradedRead, returned.DegradedTried)
		}
		if !cluster.HealEnded {
			fail("%s: the heal did not end within the run's wait", cluster.ClusterId)
		}
		if settings.Rto.Seconds() < cluster.WritesRefusedSeconds {
			fail("%s: writes were refused %.0f s, past §19.6's %s RTO", cluster.ClusterId, cluster.WritesRefusedSeconds, settings.Rto)
		}
	}
	if !kinds[server.FileClusterKindSegment] || !kinds[server.FileClusterKindMetadata] {
		fail("the drill ran on %d clusters, not on both a segment and a metadata cluster", len(measurement.Clusters))
	}
	verdict.Pass = len(verdict.Reasons) == 0
	return verdict
}

func WriteHostsVerdict(out io.Writer, measurement *HostsMeasurement, verdict *HostsVerdict) error {
	b := &strings.Builder{}
	fmt.Fprintf(b, "§22.8's hosts drill, %s: %s\n", measurement.Started.UTC().Format(time.RFC3339), map[bool]string{true: "pass", false: "fail"}[verdict.Pass])
	for _, cluster := range measurement.Clusters {
		fmt.Fprintf(b, "  %s (%s): writes refused %.0f s; heal %.0f s (ended %t), ~%.1f MB/s\n", cluster.ClusterId, cluster.Kind, cluster.WritesRefusedSeconds, cluster.HealSeconds, cluster.HealEnded, cluster.HealBytesPerSecond/1e6)
		for _, probe := range cluster.Probes {
			fmt.Fprintf(b, "    %-9s writes %d/%d (refused %d, unknown %d), protected reads %d/%d, degraded reads %d/%d\n", probe.State,
				probe.WritesStored, probe.WritesTried, probe.WritesRefused, probe.WritesUnknown, probe.ProtectedRead, probe.ProtectedTried, probe.DegradedRead, probe.DegradedTried)
		}
	}
	for _, reason := range verdict.Reasons {
		fmt.Fprintf(b, "  %s\n", reason)
	}
	_, err := io.WriteString(out, b.String())
	return err
}

// Takes a cluster's host down or brings it back.
type hostControl func(ctx context.Context, clusterId string, index int, host string, up bool) error

func templateHostControl(downTemplate string, upTemplate string, run func(ctx context.Context, command string) ([]byte, error)) hostControl {
	return func(ctx context.Context, clusterId string, index int, host string, up bool) error {
		template := downTemplate
		if up {
			template = upTemplate
		}
		if !hostStatsHostPattern.MatchString(host) || !hostStatsHostPattern.MatchString(clusterId) {
			return fmt.Errorf("a host or cluster name the shell must not see: %q, %q", host, clusterId)
		}
		command := strings.NewReplacer("{host}", host, "{index}", strconv.Itoa(index), "{cluster}", clusterId).Replace(template)
		_, err := run(ctx, command)
		return err
	}
}

// What the drill reads of a cluster: its admin info, nil when unread.
type hostsAdminInfo func(ctx context.Context, clusterId string) *server.FileClusterAdminInfo

// Runs the drill on each cluster: see the file header.
func RunHosts(ctx context.Context, store sidecar.BlobStore, clusterIds []string, kinds map[string]string, control hostControl, adminInfo hostsAdminInfo, clock sidecar.Clock, settings *HostsSettings, prefix string, measurement *HostsMeasurement) error {
	notes := &benchNotes{}
	measurement.Mode = ModeHosts
	measurement.Started = clock.Now()
	defer func() {
		measurement.Ended = clock.Now()
		measurement.Notes = append(measurement.Notes, notes.list()...)
	}()
	for _, clusterId := range clusterIds {
		cluster, err := store.Cluster(clusterId)
		if err != nil {
			return err
		}
		drill := &HostsCluster{ClusterId: clusterId, Kind: kinds[clusterId]}
		measurement.Clusters = append(measurement.Clusters, drill)
		clusterPrefix := prefix + clusterId + "/"
		err = runHostsDrill(ctx, cluster, drill, control, adminInfo, clock, settings, clusterPrefix)
		if cleanupErr := cleanupPrefix(context.WithoutCancel(ctx), cluster, clusterPrefix, settings.CleanupListLimit, settings.CleanupInFlight); cleanupErr != nil {
			notes.add(fmt.Sprintf("cleanup left objects under %s: %v", clusterPrefix, cleanupErr))
		}
		if err != nil {
			drill.Errors = append(drill.Errors, err.Error())
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return nil
}

// The host at a place in admin info's host list, from 1, by name.
func hostAt(info *server.FileClusterAdminInfo, index int) (string, error) {
	if info == nil || index < 1 || len(info.Hosts) < index {
		return "", fmt.Errorf("no host %d", index)
	}
	name, _, err := net.SplitHostPort(strings.TrimPrefix(strings.TrimPrefix(info.Hosts[index-1].Endpoint, "http://"), "https://"))
	if err != nil {
		return info.Hosts[index-1].Endpoint, nil
	}
	return name, nil
}

func runHostsDrill(ctx context.Context, cluster sidecar.BlobCluster, drill *HostsCluster, control hostControl, adminInfo hostsAdminInfo, clock sidecar.Clock, settings *HostsSettings, prefix string) error {
	info := adminInfo(ctx, drill.ClusterId)
	firstHost, err := hostAt(info, settings.FirstHost)
	if err != nil {
		return err
	}
	secondHost, err := hostAt(info, settings.SecondHost)
	if err != nil {
		return err
	}
	wait := func(duration time.Duration) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-clock.After(duration):
			return nil
		}
	}
	probeKey := func(state string, n int) string {
		return fmt.Sprintf("%s%s/o%06d", prefix, state, n)
	}
	written := map[string][]string{}
	// one state's probes: its writes, then reads of what earlier states wrote
	probe := func(state string) *HostsProbe {
		result := &HostsProbe{State: state}
		for n := range settings.ProbeCount {
			key := probeKey(state, n)
			result.WritesTried += 1
			outcome := "unknown"
			for attempt := 0; attempt <= settings.ProbeRetryCount; attempt += 1 {
				probeCtx, cancel := context.WithTimeout(ctx, settings.ProbeTimeout)
				_, err := cluster.PutIfNoneMatch(probeCtx, key, opsBody(settings.ProbeByteCount, int64(n), 0), nil)
				cancel()
				var unknown *sidecar.BlobUnknownError
				if err == nil || (0 < attempt && errors.Is(err, sidecar.ErrBlobPrecondition)) {
					outcome = "stored"
					break
				}
				if !errors.As(err, &unknown) {
					outcome = "refused"
					break
				}
			}
			switch outcome {
			case "stored":
				result.WritesStored += 1
				written[state] = append(written[state], key)
			case "refused":
				result.WritesRefused += 1
			default:
				result.WritesUnknown += 1
			}
		}
		read := func(keys []string) (int, int) {
			ok := 0
			for _, key := range keys {
				for attempt := 0; attempt <= settings.ProbeRetryCount; attempt += 1 {
					probeCtx, cancel := context.WithTimeout(ctx, settings.ProbeTimeout)
					object, err := cluster.Get(probeCtx, key)
					if err == nil {
						_, err = io.Copy(io.Discard, object.Body)
						object.Body.Close()
					}
					cancel()
					var unknown *sidecar.BlobUnknownError
					if err == nil {
						ok += 1
						break
					}
					if !errors.As(err, &unknown) {
						break
					}
				}
			}
			return len(keys), ok
		}
		result.ProtectedTried, result.ProtectedRead = read(written[HostsStateProtected])
		result.DegradedTried, result.DegradedRead = read(written[HostsStateOneDown])
		drill.Probes = append(drill.Probes, result)
		return result
	}

	probe(HostsStateProtected)
	if err := control(ctx, drill.ClusterId, settings.FirstHost, firstHost, false); err != nil {
		return fmt.Errorf("taking host %d down: %w", settings.FirstHost, err)
	}
	if err := wait(settings.Settle); err != nil {
		return err
	}
	probe(HostsStateOneDown)
	if err := control(ctx, drill.ClusterId, settings.SecondHost, secondHost, false); err != nil {
		return fmt.Errorf("taking host %d down: %w", settings.SecondHost, err)
	}
	refusedFrom := clock.Now()
	if err := wait(settings.Settle); err != nil {
		return err
	}
	probe(HostsStateTwoDown)
	if err := wait(settings.HoldDown); err != nil {
		return err
	}
	// the second host returns first, as the cold spare does, then the first
	for _, host := range []struct {
		index int
		name  string
	}{{settings.SecondHost, secondHost}, {settings.FirstHost, firstHost}} {
		if err := control(ctx, drill.ClusterId, host.index, host.name, true); err != nil {
			return fmt.Errorf("bringing host %d back: %w", host.index, err)
		}
	}
	returnedAt := clock.Now()
	// the first write stored after the return ends the refused span
	for {
		probeCtx, cancel := context.WithTimeout(ctx, settings.ProbeTimeout)
		_, err := cluster.PutIfNoneMatch(probeCtx, prefix+"return/_"+strconv.FormatInt(clock.Now().UnixNano(), 10), []byte("x"), nil)
		cancel()
		if err == nil {
			break
		}
		if err := wait(settings.Settle); err != nil {
			return err
		}
		if settings.HealTimeout < clock.Now().Sub(returnedAt) {
			return errors.New("no write was stored after the return")
		}
	}
	drill.WritesRefusedSeconds = clock.Now().Sub(refusedFrom).Seconds()
	if err := wait(settings.Settle); err != nil {
		return err
	}
	probe(HostsStateReturned)
	// the heal: until every drive is online and none heals
	for {
		info := adminInfo(ctx, drill.ClusterId)
		if info != nil && 0 < len(info.Drives) {
			healed := true
			var returnedBytes uint64
			for _, drive := range info.Drives {
				if drive.Healing || drive.State != "ok" {
					healed = false
				}
				if host := strings.TrimPrefix(strings.TrimPrefix(drive.HostEndpoint, "http://"), "https://"); strings.HasPrefix(host, firstHost+":") || strings.HasPrefix(host, secondHost+":") {
					returnedBytes += drive.UsedByteCount
				}
			}
			if healed {
				drill.HealEnded = true
				drill.HealSeconds = clock.Now().Sub(returnedAt).Seconds()
				drill.HealByteCount = returnedBytes
				if 0 < drill.HealSeconds {
					drill.HealBytesPerSecond = float64(returnedBytes) / drill.HealSeconds
				}
				return nil
			}
		}
		if settings.HealTimeout < clock.Now().Sub(returnedAt) {
			return nil
		}
		if err := wait(settings.HealPollInterval); err != nil {
			return err
		}
	}
}

func runHostsMode(ctx context.Context, args []string, stdout io.Writer) (bool, error) {
	defaults := DefaultHostsSettings()
	settings := *defaults
	flags := flag.NewFlagSet("filecluster-bench hosts", flag.ContinueOnError)
	common := &runFlags{}
	common.register(flags, false)
	clusterIds := stringsFlag{}
	flags.Var(&clusterIds, "cluster", "a cluster to drill, repeatable; none drills the segment cluster and every metadata cluster")
	downTemplate := flags.String("host_down_command", "", "a command that takes one host of a cluster down; {host}, {index} and {cluster} name it")
	upTemplate := flags.String("host_up_command", "", "a command that brings it back, as the cold spare returns a host")
	verdictPath := flags.String("verdict", "", "where the verdict's text goes, if anywhere")
	flags.DurationVar(&settings.Settle, "settle", defaults.Settle, "how long a state sits before its probes")
	flags.DurationVar(&settings.HoldDown, "hold", defaults.HoldDown, "how long both hosts stay down")
	flags.DurationVar(&settings.HealPollInterval, "heal_poll", defaults.HealPollInterval, "how often admin info is read while the heal runs")
	flags.DurationVar(&settings.HealTimeout, "heal_timeout", defaults.HealTimeout, "the most the run waits for the heal")
	flags.IntVar(&settings.ProbeCount, "probes", defaults.ProbeCount, "probes written in each state")
	if err := flags.Parse(args); err != nil {
		return false, err
	}
	if *downTemplate == "" || *upTemplate == "" {
		return false, refuseStart("the hosts drill needs -host_down_command and -host_up_command: it takes hosts down only through the operator's own commands")
	}
	clusters, err := openClusters(ctx, common.clustersPath)
	if err != nil {
		return false, err
	}
	defer clusters.Close()
	if len(clusterIds) == 0 {
		segment, err := clusters.SegmentCluster()
		if err != nil {
			return false, refuseStart("%v", err)
		}
		clusterIds = append(stringsFlag{segment.ClusterId()}, clusters.MetadataClusterIds()...)
	}
	kinds := map[string]string{}
	for _, clusterId := range clusterIds {
		kinds[clusterId] = clusters.ClusterKind(clusterId)
		if kinds[clusterId] == "" {
			return false, refuseStart("no cluster %s", clusterId)
		}
	}
	measurement := &HostsMeasurement{}
	if err := common.rowHeader(ctx, &measurement.RowHeader, ModeHosts, clusters, "", map[string]string{
		"clusters": strings.Join(clusterIds, ","),
		"settle":   settings.Settle.String(),
		"hold":     settings.HoldDown.String(),
		"probes":   fmt.Sprint(settings.ProbeCount),
	}); err != nil {
		return false, err
	}
	control := templateHostControl(*downTemplate, *upTemplate, func(ctx context.Context, command string) ([]byte, error) {
		return execCommand(ctx, "/", "/bin/sh", "-c", command)
	})
	adminInfo := func(ctx context.Context, clusterId string) *server.FileClusterAdminInfo {
		info, err := clusters.ReadAdminInfo(ctx, clusterId)
		if err != nil {
			return nil
		}
		return info
	}
	runErr := RunHosts(ctx, clusters, clusterIds, kinds, control, adminInfo, sidecar.RealClock{}, &settings, runPrefix(), measurement)
	if err := writeRowFile(common.outPath, stdout, measurement); err != nil {
		return false, err
	}
	if runErr != nil {
		return false, runErr
	}
	if *verdictPath != "" {
		file, err := os.Create(*verdictPath)
		if err != nil {
			return false, err
		}
		defer file.Close()
		if err := WriteHostsVerdict(file, measurement, JudgeHosts(measurement, &settings)); err != nil {
			return false, err
		}
	}
	return true, nil
}

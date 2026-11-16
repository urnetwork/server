package monitor

// Signal lb-map-stale implements SIGNALS.md §31.2 (OPS-M6-01; §13.4's custom-host map, W1, §19.8's M6 row,
// D-OPS-9, R-OPS-11, FS-50, FS-122, owner decision 143): each minute it reads the map's state as the dns block
// keeps it in Postgres (dns_custom_host_map's versions, dns_custom_host_map_applied's one row an lb interface,
// and the versions network_dns_custom_host's active rows hold), through the monitor's read-only session, and
// judges it with dns.yml's map settings by dnsservice.DnsMapSettings.Judge, the same judgment the dns block sets
// readiness from and the dns edges leave interfaces out of user-name answers by (DNS-M6-03):
//
//   - lb-map-stale: an interface not current for the newest map version older than map.stale_after_s (§13.4's
//     20 min), which the edges have left out of user-name answers, pages §19.8's rota at any hour from M6: that
//     interface's lbmap and reload logs, else drain it (W1), naming its hosts;
//   - lb-map-host-pending: a custom host's map version not ready on every interface once it is older than
//     map.stale_after_s, a host pending past 20 min, pages the same row, naming the version and the interfaces
//     not current for it;
//   - lb-map-unobservable: a state that does not read, whether dns.yml, the query or a row, is a visibility
//     alert, which holds every page open and never clears one.
//
// An interface is named by its row's name, <host>-<interface> as lbmap reports it and sidecar.yml's drained
// names it, a version by its number, and no finding names a custom host or an address (I-13): the dns block's
// GET /network/dns and the restore lists name the hosts.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/urnetwork/sidecar/dnsservice"

	"github.com/urnetwork/server"
)

// SIGNALS.md §31.2's signal.
func NewLbMapStaleSignal() Signal {
	return newLbMapStaleSignal(loadLbMapStaleSettings)
}

func newLbMapStaleSignal(settings func() (*LbMapStaleSettings, error)) Signal {
	return &signalAdapter{
		number: "31.2", key: "lb-map-stale", name: "lb interfaces' custom-host map",
		probe: lbMapStaleProbe{settings: settings, cadenceValue: DefaultLbMapStaleSettings().Cadence},
	}
}

const lbMapStaleProbeId = "lb/map-stale"

// SIGNALS.md §31.2's classes.
const (
	lbMapClassStale        = "lb-map-stale"
	lbMapClassHostPending  = "lb-map-host-pending"
	lbMapClassUnobservable = "lb-map-unobservable"
)

// The signal's own target, for findings about its settings or its read rather than an interface.
const lbMapStaleTarget = "lb-map"

// The probe's settings: dns.yml's root and map windows.
type LbMapStaleSettings struct {
	// The deployment's one user root, whose custom hosts the map holds, lowercase with its final dot.
	Root string
	// dns.yml's map windows, which the judgment reads.
	Map dnsservice.DnsMapSettings
	// How often the map is judged. The registry reads the default.
	Cadence time.Duration
	// Consecutive runs an interface is map-stale, or a version pending, before it pages: the judgment already
	// waits map.stale_after_s, so one run.
	PageSustain int
	// Consecutive blind runs before the visibility alert reaches Slack; a blind run holds pages open from the
	// first.
	BlindSustain int
}

// The production defaults. Root and Map are dns.yml's.
func DefaultLbMapStaleSettings() *LbMapStaleSettings {
	return &LbMapStaleSettings{
		Cadence:      time.Minute,
		PageSustain:  1,
		BlindSustain: 2,
	}
}

// No dns.yml, so the map is not judged here: configured off, not blind.
var errLbMapStaleNoDnsYml = errors.New("config has no dns.yml")

// dns.yml from config: its first root, the user root the map is for, and its map windows.
func loadLbMapStaleSettings() (*LbMapStaleSettings, error) {
	resource, err := server.Config.SimpleResource("dns.yml")
	if err != nil {
		return nil, errLbMapStaleNoDnsYml
	}
	data, err := resource.BytesE()
	if err != nil {
		return nil, fmt.Errorf("dns.yml does not read: %w", err)
	}
	dnsSettings, err := dnsservice.LoadDnsSettings(data)
	if err != nil {
		return nil, fmt.Errorf("dns.yml is refused: %w", err)
	}
	if len(dnsSettings.Roots) == 0 {
		return nil, errors.New("dns.yml names no root")
	}
	settings := DefaultLbMapStaleSettings()
	settings.Root = dnsSettings.Roots[0]
	settings.Map = dnsSettings.Map
	return settings, nil
}

type lbMapStaleProbe struct {
	settings     func() (*LbMapStaleSettings, error)
	cadenceValue time.Duration
}

func (lbMapStaleProbe) id() string                  { return lbMapStaleProbeId }
func (lbMapStaleProbe) tier() string                { return tierPage }
func (self lbMapStaleProbe) cadence() time.Duration { return self.cadenceValue }

// A root as the query may hold it: a lowercase dns name with its final dot, nothing else.
var lbMapRootRegex = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]*[a-z0-9])?\.)+$`)

// The map's state in one read, one row a version, an applied interface or a host version, each column text:
// kind, then for a version its number and creation in unix milliseconds; for an interface its name, reported
// version, poll time, probe version, probe time and host probes as JSON; for a host version its number. Times
// are numbers, so no time format is parsed.
func lbMapStateSql(root string) string {
	return `
		SELECT 'version', version::text, (floor(extract(epoch FROM create_time) * 1000))::bigint::text, '', '', '', ''
		FROM dns_custom_host_map
		UNION ALL
		SELECT 'interface', interface_name, reported_version::text,
			coalesce((floor(extract(epoch FROM poll_time) * 1000))::bigint::text, ''),
			probe_version::text,
			coalesce((floor(extract(epoch FROM probe_time) * 1000))::bigint::text, ''),
			coalesce(host_probes::text, '')
		FROM dns_custom_host_map_applied
		UNION ALL
		SELECT 'host', map_version::text, '', '', '', '', ''
		FROM network_dns_custom_host
		WHERE root = '` + root + `' AND state = 'active' AND map_version > 0
		GROUP BY map_version
	`
}

// host_probes' JSON, as the dns block writes it.
type lbMapHostProbesJson struct {
	Added   []string        `json:"added,omitempty"`
	Dropped []string        `json:"dropped,omitempty"`
	Results map[string]bool `json:"results,omitempty"`
}

// A version or serial column.
func lbMapUint(text string) (uint64, error) {
	return strconv.ParseUint(text, 10, 64)
}

// A time column in unix milliseconds, the zero time for none.
func lbMapTime(text string) (time.Time, error) {
	if text == "" {
		return time.Time{}, nil
	}
	milliseconds, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	return time.UnixMilli(milliseconds).UTC(), nil
}

// The state the rows hold, or why they do not read.
func lbMapState(rows []pgRow) (*dnsservice.DnsMapState, error) {
	state := &dnsservice.DnsMapState{}
	for index, row := range rows {
		if len(row) != 7 {
			return nil, fmt.Errorf("row %d holds %d columns, not 7", index, len(row))
		}
		switch row[0] {
		case "version":
			version, err := lbMapUint(row[1])
			if err != nil {
				return nil, fmt.Errorf("row %d's version: %w", index, err)
			}
			createTime, err := lbMapTime(row[2])
			if err != nil || createTime.IsZero() {
				return nil, fmt.Errorf("row %d's create time does not read", index)
			}
			state.Versions = append(state.Versions, dnsservice.DnsMapVersion{Version: version, CreateTime: createTime})
		case "interface":
			lbInterface := &dnsservice.DnsMapInterface{Name: row[1], HostProbes: map[string]bool{}}
			var err error
			if lbInterface.ReportedVersion, err = lbMapUint(row[2]); err != nil {
				return nil, fmt.Errorf("row %d's reported version: %w", index, err)
			}
			if lbInterface.PollTime, err = lbMapTime(row[3]); err != nil {
				return nil, fmt.Errorf("row %d's poll time: %w", index, err)
			}
			if lbInterface.ProbeVersion, err = lbMapUint(row[4]); err != nil {
				return nil, fmt.Errorf("row %d's probe version: %w", index, err)
			}
			if lbInterface.ProbeTime, err = lbMapTime(row[5]); err != nil {
				return nil, fmt.Errorf("row %d's probe time: %w", index, err)
			}
			if row[6] != "" {
				hostProbes := &lbMapHostProbesJson{}
				if err := json.Unmarshal([]byte(row[6]), hostProbes); err != nil {
					return nil, fmt.Errorf("row %d's host probes are no JSON object", index)
				}
				lbInterface.AddedHosts = hostProbes.Added
				lbInterface.DroppedHosts = hostProbes.Dropped
				for host, passed := range hostProbes.Results {
					lbInterface.HostProbes[host] = passed
				}
			}
			state.Interfaces = append(state.Interfaces, lbInterface)
		case "host":
			version, err := lbMapUint(row[1])
			if err != nil {
				return nil, fmt.Errorf("row %d's host version: %w", index, err)
			}
			state.HostVersions = append(state.HostVersions, version)
		default:
			return nil, fmt.Errorf("row %d is of no kind the query writes", index)
		}
	}
	slices.SortFunc(state.Versions, func(a dnsservice.DnsMapVersion, b dnsservice.DnsMapVersion) int {
		return compareUint64(a.Version, b.Version)
	})
	slices.SortFunc(state.Interfaces, func(a *dnsservice.DnsMapInterface, b *dnsservice.DnsMapInterface) int {
		return strings.Compare(a.Name, b.Name)
	})
	slices.Sort(state.HostVersions)
	return state, nil
}

func compareUint64(a uint64, b uint64) int {
	switch {
	case a < b:
		return -1
	case b < a:
		return 1
	}
	return 0
}

// The blind finding: the state did not read, which holds every page open.
func lbMapBlindFinding(settings *LbMapStaleSettings, observed string) finding {
	sustain := DefaultLbMapStaleSettings().BlindSustain
	if settings != nil {
		sustain = settings.BlindSustain
	}
	return finding{
		probeId: lbMapStaleProbeId, tier: tierWarn, class: lbMapClassUnobservable, target: lbMapStaleTarget,
		sustain: sustain, unobservable: true,
		symptom:  "the custom-host map's state does not read, so no interface's map is judged",
		observed: observed,
		mechanism: "The monitor judges each lb interface's custom-host map from the dns block's own state in " +
			"Postgres (§13.4); without it, a map-stale interface or a host pending past 20 minutes cannot page.",
		action: "Check dns.yml's map block and the monitor's read-only session to the primary; the observed value " +
			"says which failed. While this lasts no lb-map incident clears.",
		verify: "The next run reads the state, and this alert clears.",
	}
}

func (self lbMapStaleProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	settings, err := self.settings()
	switch {
	case errors.Is(err, errLbMapStaleNoDnsYml):
		return []finding{{
			probeId: lbMapStaleProbeId, tier: tierWarn, class: lbMapClassUnobservable, target: lbMapStaleTarget,
			sustain: 1, configuredOff: true,
			symptom: "config has no dns.yml, so no custom-host map is judged here",
		}}, nil
	case err != nil:
		return []finding{lbMapBlindFinding(nil, err.Error())}, nil
	case !lbMapRootRegex.MatchString(settings.Root):
		return []finding{lbMapBlindFinding(settings, "dns.yml's root is not a lowercase name with its final dot")}, nil
	case settings.Map.StaleAfterSeconds <= 0:
		return []finding{lbMapBlindFinding(settings, "dns.yml's map.stale_after_s is not positive")}, nil
	}
	rows, err := env.runner.pg(ctx, lbMapStateSql(settings.Root))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return []finding{lbMapBlindFinding(settings, "the state's query failed: "+classifyObservationError(err))}, nil
	}
	state, err := lbMapState(rows)
	if err != nil {
		return []finding{lbMapBlindFinding(settings, "the state does not read: "+err.Error())}, nil
	}
	now := env.now()
	judgment := settings.Map.Judge(state, now)
	staleAfter := time.Duration(settings.Map.StaleAfterSeconds) * time.Second

	findings := []finding{healthyFinding(lbMapStaleProbeId, tierWarn, lbMapClassUnobservable, lbMapStaleTarget)}
	for _, lbInterface := range state.Interfaces {
		if !judgment.MapStale[lbInterface.Name] {
			findings = append(findings, healthyFinding(lbMapStaleProbeId, tierPage, lbMapClassStale, lbInterface.Name))
			continue
		}
		findings = append(findings, finding{
			probeId: lbMapStaleProbeId, tier: tierPage, class: lbMapClassStale, target: lbInterface.Name,
			sustain:  settings.PageSustain,
			symptom:  "an lb interface's custom-host map is stale, so the dns edges leave it out of user-name answers",
			observed: fmt.Sprintf("reported version %d, probed version %d", lbInterface.ReportedVersion, lbInterface.ProbeVersion),
			mechanism: "Each interface's lbmap loads every custom-host map version and reports it, and the dns edges' " +
				"route probes confirm it (§13.4, W1). An interface not current for a version older than " +
				staleAfter.String() + " is map-stale: its nginx may send a custom host's visitors to the wrong branch.",
			action: "§19.8's M6 row: read that interface's lbmap and nginx reload logs; else drain it (W1, " +
				"sidecar.yml's drained), and name its custom hosts from the dns block's GET /network/dns.",
			verify: "The interface reports and serves the newest version, the edges' probes pass, and this clears.",
		})
	}
	for _, version := range state.HostVersions {
		if judgment.Ready[version] {
			findings = append(findings, healthyFinding(lbMapStaleProbeId, tierPage, lbMapClassHostPending, lbMapVersionTarget(version)))
			continue
		}
		created := time.Time{}
		for _, known := range state.Versions {
			if known.Version == version {
				created = known.CreateTime
			}
		}
		if created.IsZero() || now.Sub(created) < staleAfter {
			// pending, but not past its window yet
			findings = append(findings, healthyFinding(lbMapStaleProbeId, tierPage, lbMapClassHostPending, lbMapVersionTarget(version)))
			continue
		}
		findings = append(findings, finding{
			probeId: lbMapStaleProbeId, tier: tierPage, class: lbMapClassHostPending, target: lbMapVersionTarget(version),
			sustain:  settings.PageSustain,
			symptom:  "custom hosts of a map version are pending past " + staleAfter.String(),
			observed: "interfaces not current for it: " + strings.Join(judgment.Pending[version], ", "),
			mechanism: "A custom host is ready once every lb interface is current for its map version (§13.4); until " +
				"then its certificate is not ordered and its owner sees it pending.",
			action: "§19.8's M6 row: read each named interface's lbmap and nginx reload logs; else drain it (W1).",
			verify: "Every interface reports the version, its hosts turn ready, and this clears.",
		})
	}
	return findings, nil
}

// A map version as a finding names it.
func lbMapVersionTarget(version uint64) string {
	return fmt.Sprintf("custom-host-map-v%d", version)
}

package monitor

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A synthetic tagged Mimir vector evaluated at one clock.
type fleetFixture struct {
	at   time.Time
	rows []map[string]any
}

func newFleetFixture(at time.Time) *fleetFixture {
	return &fleetFixture{at: at}
}

func (self *fleetFixture) labeled(part string, labels map[string]string, value float64) *fleetFixture {
	metric := map[string]string{fleetPartLabel: part}
	for name, labelValue := range labels {
		metric[name] = labelValue
	}
	text := strconv.FormatFloat(value, 'f', -1, 64)
	switch {
	case math.IsNaN(value):
		text = "NaN"
	case math.IsInf(value, 1):
		text = "+Inf"
	case math.IsInf(value, -1):
		text = "-Inf"
	}
	self.rows = append(self.rows, map[string]any{"metric": metric, "value": []any{float64(self.at.Unix()), text}})
	return self
}

func (self *fleetFixture) scalar(part string, value float64) *fleetFixture {
	return self.labeled(part, nil, value)
}

func (self *fleetFixture) fresh(family string, slot fleetSlot, sourceTime time.Time) *fleetFixture {
	return self.labeled("fresh/"+family, map[string]string{"job": slot.service, "host": slot.host, "block": slot.block}, float64(sourceTime.Unix()))
}

func (self *fleetFixture) json(t testing.TB) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"status": "success", "data": map[string]any{"resultType": "vector", "result": self.rows}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// Placement is desired inventory: an optional service without hosts is not
// required, a required one without placement is a gap, and a disabled or
// excluded host is a slot that can only be unknown.
func TestFleetSlotInventoryKeepsDesiredPlacement(t *testing.T) {
	settings := syntheticSettings(&syntheticSource{})
	settings.Hosts = append(settings.Hosts,
		HostSettings{Name: "edge-a.example", Roles: []string{"services"}},
		HostSettings{Name: "edge-b.example", Roles: []string{"services"}},
	)
	settings.ExcludedHosts = []string{"edge-b.example"}
	settings.LogServices = []string{"api", "proxy"}
	settings.LogServiceHosts = map[string][]string{"api": {"edge-a.example", "edge-b.example", "edge-c.example"}, "alt": {}}
	settings.LogServiceBlocks = map[string][]string{"api": {"g1"}}
	env, err := newProbeEnv(settings.withDefaults())
	if err != nil {
		t.Fatal(err)
	}
	inventory := newFleetSlotInventory(env, []string{"api", "proxy", "alt"}, []string{"alt"})
	labels := []string{}
	for _, slot := range inventory.slots {
		labels = append(labels, slot.label()+"="+strconv.FormatBool(slot.allowed))
	}
	if got := strings.Join(labels, " "); got != "api/edge-a.example/g1=true api/edge-b.example/g1=false api/edge-c.example/g1=false" {
		t.Fatalf("slots = %s", got)
	}
	if len(inventory.missingServices) != 1 || inventory.missingServices[0] != "proxy" {
		t.Fatalf("missing services = %v", inventory.missingServices)
	}
	selector := inventory.selector("synthetic")
	if selector != `env="synthetic",job=~"api",host=~"edge-a\\.example",block=~"g1"` {
		t.Fatalf("selector = %s", selector)
	}
}

func TestParseFleetSamplesRejectsUnqualifiedResponses(t *testing.T) {
	at := time.Date(2026, 10, 22, 6, 30, 0, 0, time.UTC)
	good := newFleetFixture(at).scalar("one", 1).json(t)
	if samples, err := parseFleetSamples(good, at, FleetReadSettings{MaxResponseBytes: 4096, MaxRows: 4}); err != nil || len(samples) != 1 || samples[0].part != "one" || len(samples[0].labels) != 0 {
		t.Fatalf("clean response = %+v, %v", samples, err)
	}
	for name, raw := range map[string]string{
		"oversize":   good + strings.Repeat(" ", 4096),
		"not json":   "<html>",
		"error":      `{"status":"error","error":"synthetic","data":{"resultType":"vector","result":[]}}`,
		"matrix":     `{"status":"success","data":{"resultType":"matrix","result":[]}}`,
		"warnings":   `{"status":"success","warnings":["synthetic"],"data":{"resultType":"vector","result":[]}}`,
		"infos":      `{"status":"success","infos":["synthetic"],"data":{"resultType":"vector","result":[]}}`,
		"untagged":   `{"status":"success","data":{"resultType":"vector","result":[{"metric":{},"value":[` + strconv.FormatInt(at.Unix(), 10) + `,"1"]}]}}`,
		"other time": newFleetFixture(at.Add(-time.Minute)).scalar("one", 1).json(t),
		"bad value":  `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"monitor_part":"one"},"value":[` + strconv.FormatInt(at.Unix(), 10) + `,"one"]}]}}`,
		"rows":       newFleetFixture(at).scalar("a", 1).scalar("b", 1).scalar("c", 1).scalar("d", 1).scalar("e", 1).json(t),
	} {
		if samples, err := parseFleetSamples(raw, at, FleetReadSettings{MaxResponseBytes: 4096, MaxRows: 4}); err == nil {
			t.Fatalf("%s accepted: %+v", name, samples)
		} else if strings.Contains(err.Error(), "synthetic") {
			t.Fatalf("%s error echoed response text", name)
		}
	}
}

func TestFleetSlotCoverageReasons(t *testing.T) {
	at := time.Date(2026, 10, 22, 6, 30, 0, 0, time.UTC)
	inventory := fleetSlotInventory{slots: []fleetSlot{
		{service: "proxy", host: "a.example", block: "g1", allowed: true},
		{service: "proxy", host: "b.example", block: "g1", allowed: true},
		{service: "proxy", host: "c.example", block: "g1", allowed: true},
		{service: "proxy", host: "d.example", block: "g1", allowed: true},
		{service: "proxy", host: "e.example", block: "g1", allowed: false},
		{service: "proxy", host: "f.example", block: "g1", allowed: true},
	}, missingServices: []string{"connect"}}
	fixture := newFleetFixture(at)
	for _, slot := range inventory.slots {
		fixture.fresh("one", slot, at.Add(-time.Minute))
	}
	fixture.fresh("one", inventory.slots[1], at.Add(-time.Minute)) // b: duplicate
	fixture.rows = fixture.rows[1:]                                // a: missing
	fixture.fresh("two", inventory.slots[1], at)
	fixture.fresh("two", inventory.slots[2], at.Add(-4*time.Minute)) // c: stale
	fixture.fresh("two", inventory.slots[3], at)
	fixture.fresh("two", inventory.slots[5], at)
	samples, err := parseFleetSamples(fixture.json(t), at, FleetReadSettings{MaxResponseBytes: 1024 * 1024, MaxRows: 1000})
	if err != nil {
		t.Fatal(err)
	}
	coverage := newFleetSlotCoverage(inventory, samples, []string{"one", "two"}, at, FleetReadSettings{Freshness: 3 * time.Minute, FutureSkew: 30 * time.Second}, func(slot fleetSlot) string {
		if slot.host == "f.example" {
			return "capability-off"
		}
		return ""
	})
	if coverage.full() || coverage.complete != 1 || coverage.expected != 6 {
		t.Fatalf("coverage = %+v", coverage)
	}
	want := "connect[placement-missing],proxy/a.example/g1[missing],proxy/b.example/g1[ambiguous],proxy/c.example/g1[stale],proxy/e.example/g1[excluded],proxy/f.example/g1[capability-off]"
	if got := strings.Join(coverage.gaps, ","); got != want {
		t.Fatalf("gaps = %s", got)
	}
	if listed := coverage.listed(2); listed != "connect[placement-missing],proxy/a.example/g1[missing],and 4 more" {
		t.Fatalf("listed = %s", listed)
	}
}

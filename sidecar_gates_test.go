package server

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// What §19.11's gates answer (sidecar_gates.go), against the gate table the
// sidecar's fake answers too, and the reader's failure rules for the gates.
// Pure: an override stands in for the file, and a fake clock moves rereads.

// sidecar/testdata/gates/table.yml, beside this checkout in the monoroot.
const sidecarGateTablePath = "../sidecar/testdata/gates/table.yml"

type sidecarGateTable struct {
	Buckets map[string]int         `yaml:"buckets"`
	Cases   []sidecarGateTableCase `yaml:"cases"`
}

type sidecarGateTableService struct {
	Enabled bool     `yaml:"enabled"`
	Writes  bool     `yaml:"writes"`
	Percent int      `yaml:"percent"`
	Admits  []string `yaml:"admits"`
	Refuses []string `yaml:"refuses"`
}

type sidecarGateTableCase struct {
	Name   string `yaml:"name"`
	File   string `yaml:"file"`
	Broken bool   `yaml:"broken"`
	Expect struct {
		Services map[string]sidecarGateTableService `yaml:"services"`
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
		NoFileClusters []string            `yaml:"no_file_clusters"`
		GatesFor       map[string][]string `yaml:"gates_for"`
	} `yaml:"expect"`
}

func readSidecarGateTable(t *testing.T) *sidecarGateTable {
	t.Helper()
	data, err := os.ReadFile(sidecarGateTablePath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("no sidecar checkout beside the server (%s)", sidecarGateTablePath)
	}
	if err != nil {
		t.Fatal(err)
	}
	table := &sidecarGateTable{}
	if err := yaml.Unmarshal(data, table); err != nil {
		t.Fatal(err)
	}
	if len(table.Cases) == 0 || len(table.Buckets) == 0 {
		t.Fatal("the gate table holds nothing")
	}
	return table
}

func requireSidecarId(t *testing.T, idStr string) Id {
	t.Helper()
	id, err := ParseId(idStr)
	if err != nil {
		t.Fatalf("%q: %s", idStr, err)
	}
	return id
}

// Holds one read to one case: what the case names, and every other gate closed.
func requireSidecarGateTableCase(t *testing.T, table *sidecarGateTable, c sidecarGateTableCase, config *SidecarConfig) {
	t.Helper()
	bucketIds := []Id{}
	for idStr := range table.Buckets {
		bucketIds = append(bucketIds, requireSidecarId(t, idStr))
	}
	for _, name := range []string{SidecarGateEvs, SidecarGateGkm, SidecarGateSockets, SidecarGateTlsedge, SidecarGateMxedge, SidecarGateSmtpedge, SidecarGateDns, "storage"} {
		gate := config.Service(name)
		want, ok := c.Expect.Services[name]
		if !ok {
			if gate.Enabled() || gate.Writes() {
				t.Errorf("%s: %s is open", c.Name, name)
			}
			for _, networkId := range bucketIds {
				if gate.Admits(networkId) {
					t.Errorf("%s: closed %s admits %s", c.Name, name, networkId)
				}
			}
			continue
		}
		if gate.Enabled() != want.Enabled || gate.Writes() != want.Writes || gate.Percent() != want.Percent {
			t.Errorf("%s: %s enabled %t writes %t percent %d, want %+v", c.Name, name, gate.Enabled(), gate.Writes(), gate.Percent(), want)
		}
		for _, idStr := range want.Admits {
			if !gate.Admits(requireSidecarId(t, idStr)) {
				t.Errorf("%s: %s refuses %s", c.Name, name, idStr)
			}
		}
		for _, idStr := range want.Refuses {
			if gate.Admits(requireSidecarId(t, idStr)) {
				t.Errorf("%s: %s admits %s", c.Name, name, idStr)
			}
		}
	}

	format2 := config.Format2()
	if want := c.Expect.Format2; want == nil {
		for _, networkId := range bucketIds {
			if format2.Admits(networkId) {
				t.Errorf("%s: closed format2 admits %s", c.Name, networkId)
			}
		}
		if format2.Purge() != SidecarFormat2Observe || format2.Drop() != SidecarFormat2Observe {
			t.Errorf("%s: closed format2 deletes", c.Name)
		}
	} else {
		for _, idStr := range want.Admits {
			if !format2.Admits(requireSidecarId(t, idStr)) {
				t.Errorf("%s: format2 refuses %s", c.Name, idStr)
			}
		}
		for _, idStr := range want.Refuses {
			if format2.Admits(requireSidecarId(t, idStr)) {
				t.Errorf("%s: format2 admits %s", c.Name, idStr)
			}
		}
		if string(format2.Purge()) != want.Purge || string(format2.Drop()) != want.Drop {
			t.Errorf("%s: format2 purge %s drop %s, want %s and %s", c.Name, format2.Purge(), format2.Drop(), want.Purge, want.Drop)
		}
	}

	for _, name := range []string{SidecarSwitchPush, SidecarSwitchWake, SidecarSwitchBridge, SidecarSwitchEscrowDrawOrder, "other"} {
		if config.Switch(name) != c.Expect.Switches[name] {
			t.Errorf("%s: switch %s is %t", c.Name, name, config.Switch(name))
		}
	}
	if config.Push() != c.Expect.Switches[SidecarSwitchPush] || config.Wake() != c.Expect.Switches[SidecarSwitchWake] ||
		config.Bridge() != c.Expect.Switches[SidecarSwitchBridge] || config.EscrowDrawOrder() != c.Expect.Switches[SidecarSwitchEscrowDrawOrder] {
		t.Errorf("%s: a named switch disagrees with Switch", c.Name)
	}
	if config.PlaceNew() != c.Expect.PlaceNew {
		t.Errorf("%s: place_new %q, want %q", c.Name, config.PlaceNew(), c.Expect.PlaceNew)
	}
	if config.Reseed() != c.Expect.Reseed {
		t.Errorf("%s: reseed %t", c.Name, config.Reseed())
	}
	if drained := config.Drained(); !slices.Equal(drained, c.Expect.Drained) && (len(drained) != 0 || len(c.Expect.Drained) != 0) {
		t.Errorf("%s: drained %q, want %q", c.Name, drained, c.Expect.Drained)
	}
	rate, burst, ok := config.SocketsControlQps()
	if want := c.Expect.SocketsControlQps; want == nil {
		if ok {
			t.Errorf("%s: sockets_control_qps set to %v and %d", c.Name, rate, burst)
		}
	} else if !ok || rate != want.Rate || burst != want.Burst {
		t.Errorf("%s: sockets_control_qps %v %d %t, want %+v", c.Name, rate, burst, ok, *want)
	}
	edge, ok := config.UrnetworkEdge()
	if c.Expect.UrnetworkEdge == "" {
		if ok {
			t.Errorf("%s: urnetwork_edge set to %s", c.Name, edge)
		}
	} else if !ok || edge != requireSidecarId(t, c.Expect.UrnetworkEdge) {
		t.Errorf("%s: urnetwork_edge %s %t, want %s", c.Name, edge, ok, c.Expect.UrnetworkEdge)
	}
	for clusterId, want := range c.Expect.FileClusters {
		got, ok := config.FileCluster(clusterId)
		if !ok || got.SettlePassedBuild != want.SettlePassedBuild || got.HddCapacityOps != want.HddCapacityOps {
			t.Errorf("%s: cluster %s %+v %t, want %+v", c.Name, clusterId, got, ok, want)
		}
	}
	for _, clusterId := range c.Expect.NoFileClusters {
		if got, ok := config.FileCluster(clusterId); ok || got != (SidecarFileCluster{}) {
			t.Errorf("%s: cluster %s has passed %+v", c.Name, clusterId, got)
		}
	}
	for idStr, want := range c.Expect.GatesFor {
		if got := config.GatesFor(requireSidecarId(t, idStr)); !slices.Equal(got, want) && (len(got) != 0 || len(want) != 0) {
			t.Errorf("%s: gates for %s are %q, want %q", c.Name, idStr, got, want)
		}
	}
}

// The reader answers every case of the table sidecartest's fake answers, and a
// file it cannot read opens nothing on a first read. Every key of §19.11's
// sample parses as written: the table holds the sample, comments and all.
func TestSidecarGatesAnswerTheGateTable(t *testing.T) {
	table := readSidecarGateTable(t)
	for _, c := range table.Cases {
		config, err := parseSidecarConfig([]byte(c.File))
		if c.Broken {
			if err == nil {
				t.Errorf("%s: a file the table calls broken parsed", c.Name)
			}
			func() {
				defer Testing_PushSidecarConfig(c.File)()
				config := loadSidecarConfig(nil)
				if !config.P0Enforce {
					t.Errorf("%s: a first read of a broken file does not enforce", c.Name)
				}
				requireSidecarGateTableCase(t, table, sidecarGateTableCase{Name: c.Name}, config)
			}()
			continue
		}
		if err != nil {
			t.Errorf("%s: %s", c.Name, err)
			continue
		}
		requireSidecarGateTableCase(t, table, c, config)
		// the file read through Config answers the same as the text parsed
		func() {
			defer Testing_PushSidecarConfig(c.File)()
			requireSidecarGateTableCase(t, table, c, CurrentSidecarConfig())
		}()
	}
}

// The bucket is pinned by the table, so no release moves a network in or out of
// a percent rollout, and the sidecar root's bucket is pinned to the same.
func TestSidecarGateBucketHoldsTheTablesVectors(t *testing.T) {
	table := readSidecarGateTable(t)
	for idStr, want := range table.Buckets {
		if got := SidecarGateBucket(requireSidecarId(t, idStr)); got != want {
			t.Errorf("%s is in bucket %d, not the table's %d", idStr, got, want)
		}
	}
}

// Admits at percent 0, 1, 99 and 100 on ids of known buckets; listed beats
// percent; enabled false closes a listed network.
func TestSidecarGateAdmitsAtThePercentBoundaries(t *testing.T) {
	bucketIds := map[int]Id{}
	for idStr, bucket := range readSidecarGateTable(t).Buckets {
		bucketIds[bucket] = requireSidecarId(t, idStr)
	}
	for _, c := range []struct {
		percent int
		admits  map[int]bool
	}{
		{percent: 0, admits: map[int]bool{0: false, 1: false, 98: false, 99: false}},
		{percent: 1, admits: map[int]bool{0: true, 1: false, 98: false, 99: false}},
		{percent: 99, admits: map[int]bool{0: true, 1: true, 98: true, 99: false}},
		{percent: 100, admits: map[int]bool{0: true, 1: true, 98: true, 99: true}},
	} {
		gate := newSidecarServiceGate("evs", &sidecarGateYaml{Enabled: true, Percent: c.percent})
		for bucket, want := range c.admits {
			if got := gate.Admits(bucketIds[bucket]); got != want {
				t.Errorf("percent %d, bucket %d: admits %t", c.percent, bucket, got)
			}
		}
	}
	listed := newSidecarServiceGate("evs", &sidecarGateYaml{Enabled: true, Networks: []string{bucketIds[99].String()}, Percent: 1})
	if !listed.Admits(bucketIds[99]) || !listed.Admits(bucketIds[0]) || listed.Admits(bucketIds[1]) {
		t.Error("a listed network is not in whatever the percent")
	}
	closed := newSidecarServiceGate("evs", &sidecarGateYaml{Enabled: false, Networks: []string{bucketIds[0].String()}, Percent: 100})
	if closed.Admits(bucketIds[0]) || closed.Writes() {
		t.Error("enabled: false leaves a listed network in")
	}
}

// A closed service answers 503 with Retry-After, by default the reread interval
// in whole seconds, and never a 200.
func TestWriteGateClosedAnswers503WithRetryAfter(t *testing.T) {
	for _, c := range []struct {
		retryAfter time.Duration
		header     string
	}{
		{retryAfter: 0, header: "60"},
		{retryAfter: -time.Second, header: "60"},
		{retryAfter: 1500 * time.Millisecond, header: "2"},
		{retryAfter: time.Millisecond, header: "1"},
	} {
		recorder := httptest.NewRecorder()
		WriteGateClosed(recorder, c.retryAfter)
		if recorder.Code != 503 || recorder.Header().Get("Retry-After") != c.header {
			t.Errorf("retry after %s: %d with Retry-After %q", c.retryAfter, recorder.Code, recorder.Header().Get("Retry-After"))
		}
	}
}

// The reader's failure rules hold for the gates as for P0: absent is inert; a
// broken first read opens nothing; a broken or absent later read keeps the last
// good one.
func TestSidecarGatesFailClosed(t *testing.T) {
	networkId := NewId()
	good := fmt.Sprintf("sidecar:\n  evs: {enabled: true, networks: [%q]}\n  push: {enabled: true}\n", networkId)
	const broken = "sidecar:\n  evs: {enabled: true, percent: fifty}\n"

	newTestSidecarConfigHome(t)
	if config := loadSidecarConfig(nil); config.Service(SidecarGateEvs).Admits(networkId) || config.Push() {
		t.Fatal("an absent file opens a gate")
	}
	func() {
		defer Testing_PushSidecarConfig(broken)()
		config := loadSidecarConfig(nil)
		if config.Service(SidecarGateEvs).Enabled() || config.Push() || !config.P0Enforce {
			t.Fatal("a first read of a broken file opens a gate or leaves P0 off")
		}
	}()
	var previous *SidecarConfig
	func() {
		defer Testing_PushSidecarConfig(good)()
		previous = loadSidecarConfig(nil)
	}()
	if !previous.Service(SidecarGateEvs).Admits(networkId) || !previous.Push() {
		t.Fatal("the good file did not open its gates")
	}
	func() {
		defer Testing_PushSidecarConfig(broken)()
		if config := loadSidecarConfig(previous); config != previous {
			t.Fatal("a broken later read replaced the last good one")
		}
	}()
	if config := loadSidecarConfig(previous); config != previous {
		t.Fatal("an absent later read replaced the last good one")
	}
}

// A gate moves within the reread interval of a push and not before.
func TestSidecarGatesMoveAtTheReread(t *testing.T) {
	networkId := NewId()
	text := "sidecar: {}\n"
	reader, now := newTestSidecarConfigReader(func(previous *SidecarConfig) *SidecarConfig {
		config, err := parseSidecarConfig([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return config
	})
	if reader.get().Service(SidecarGateGkm).Admits(networkId) {
		t.Fatal("an empty file admits")
	}
	text = fmt.Sprintf("sidecar:\n  gkm: {enabled: true, networks: [%q]}\n", networkId)
	*now = now.Add(59 * time.Second)
	if reader.get().Service(SidecarGateGkm).Admits(networkId) {
		t.Fatal("the gate moved inside the interval")
	}
	*now = now.Add(time.Second)
	if !reader.get().Service(SidecarGateGkm).Admits(networkId) {
		t.Fatal("the gate did not move at the interval")
	}
}

// A node through documents and aliases.
func sidecarYamlResolve(node *yaml.Node) *yaml.Node {
	for node != nil && (node.Kind == yaml.DocumentNode || node.Kind == yaml.AliasNode) {
		if node.Kind == yaml.AliasNode {
			node = node.Alias
			continue
		}
		if len(node.Content) == 0 {
			return nil
		}
		node = node.Content[0]
	}
	return node
}

// A key's value in a mapping, a direct key before a merged one, as yaml.v3's
// typed decoding reaches it.
func sidecarYamlMappingValue(mapping *yaml.Node, key string) *yaml.Node {
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if keyNode := sidecarYamlResolve(mapping.Content[index]); keyNode != nil && keyNode.Kind == yaml.ScalarNode && keyNode.ShortTag() != "!!merge" && keyNode.Value == key {
			return mapping.Content[index+1]
		}
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if keyNode := sidecarYamlResolve(mapping.Content[index]); keyNode == nil || keyNode.ShortTag() != "!!merge" {
			continue
		}
		merged := sidecarYamlResolve(mapping.Content[index+1])
		candidates := []*yaml.Node{merged}
		if merged != nil && merged.Kind == yaml.SequenceNode {
			candidates = merged.Content
		}
		for _, candidate := range candidates {
			if candidate = sidecarYamlResolve(candidate); candidate != nil && candidate.Kind == yaml.MappingNode {
				if value := sidecarYamlMappingValue(candidate, key); value != nil {
					return value
				}
			}
		}
	}
	return nil
}

// A lookup in the text's node tree. A node reading tolerates what typed decoding
// skips unread, such as a key given twice where no field reads it, so it judges
// every text the typed reading takes.
func sidecarYamlLookup(node *yaml.Node, keys ...string) (*yaml.Node, bool) {
	for _, key := range keys {
		node = sidecarYamlResolve(node)
		if node == nil || node.Kind != yaml.MappingNode {
			return nil, false
		}
		if node = sidecarYamlMappingValue(node, key); node == nil {
			return nil, false
		}
	}
	node = sidecarYamlResolve(node)
	return node, node != nil
}

// Whether a key reads true, by yaml.v3's own decoding of a bool, old-style YAML
// 1.1 trues included.
func sidecarYamlTrue(node *yaml.Node, keys ...string) bool {
	value, ok := sidecarYamlLookup(node, keys...)
	if !ok {
		return false
	}
	decoded := false
	return value.Decode(&decoded) == nil && decoded
}

// Whether a sequence holds a string that parses to networkId.
func sidecarYamlListsId(node *yaml.Node, networkId Id, keys ...string) bool {
	value, ok := sidecarYamlLookup(node, keys...)
	if !ok || value.Kind != yaml.SequenceNode {
		return false
	}
	for _, item := range value.Content {
		itemStr := ""
		if item = sidecarYamlResolve(item); item != nil && item.Decode(&itemStr) == nil {
			if id, err := ParseId(itemStr); err == nil && id == networkId {
				return true
			}
		}
	}
	return false
}

// A parse never panics and never opens a gate the file does not open: whatever
// the typed reading opens, a generic reading of the same text shows the key that
// opens it.
func FuzzSidecarConfig(f *testing.F) {
	if data, err := os.ReadFile(sidecarGateTablePath); err == nil {
		table := &sidecarGateTable{}
		if err := yaml.Unmarshal(data, table); err == nil {
			for _, c := range table.Cases {
				f.Add(c.File)
			}
		}
	}
	f.Add("sidecar:\n  evs: {enabled: true, writes: false, networks: [5ca1ab1e-0000-4000-8000-000000000004], percent: 100}\n")
	f.Add("sidecar:\n  p0_enforce: {enabled: true}\n  preview_networks: [5ca1ab1e-0000-4000-8000-000000000004]\n")
	f.Add("sidecar: {format2: {enabled: true, percent: 7, purge: on, drop: \"on\"}, reseed: true}\n")
	f.Fuzz(func(t *testing.T, text string) {
		config, err := parseSidecarConfig([]byte(text))
		if err != nil {
			return
		}
		generic := &yaml.Node{}
		if err := yaml.Unmarshal([]byte(text), generic); err != nil {
			t.Fatalf("the typed reading parsed what a node reading refuses: %s", err)
		}
		probeIds := []Id{}
		for _, gate := range config.serviceGates {
			probeIds = append(probeIds, gate.NetworkIds()...)
		}
		probeIds = append(probeIds, config.Format2().Gate().NetworkIds()...)
		probeIds = append(probeIds, NewId())
		for name, gate := range config.serviceGates {
			if gate.Enabled() && !sidecarYamlTrue(generic, "sidecar", name, "enabled") {
				t.Fatalf("%s is enabled with no enabled: true in %q", name, text)
			}
			if gate.Writes() {
				if writes, ok := sidecarYamlLookup(generic, "sidecar", name, "writes"); ok && writes.ShortTag() != "!!null" && !sidecarYamlTrue(generic, "sidecar", name, "writes") {
					t.Fatalf("%s takes writes with writes %q in %q", name, writes.Value, text)
				}
			}
			for _, networkId := range gate.NetworkIds() {
				if !sidecarYamlListsId(generic, networkId, "sidecar", name, "networks") {
					t.Fatalf("%s lists %s, which %q does not", name, networkId, text)
				}
			}
			if 0 < gate.Percent() {
				percent := 0
				value, ok := sidecarYamlLookup(generic, "sidecar", name, "percent")
				if !ok || value.Decode(&percent) != nil || percent != gate.Percent() {
					t.Fatalf("%s admits %d%% with percent %d in %q", name, gate.Percent(), percent, text)
				}
			}
		}
		format2 := config.Format2()
		for _, networkId := range probeIds {
			if format2.Admits(networkId) && !sidecarYamlTrue(generic, "sidecar", "format2", "enabled") {
				t.Fatalf("format2 admits with no enabled: true in %q", text)
			}
		}
		for _, c := range []struct {
			mode SidecarFormat2Mode
			key  string
		}{{mode: format2.Purge(), key: "purge"}, {mode: format2.Drop(), key: "drop"}} {
			if c.mode != SidecarFormat2On {
				continue
			}
			mode := ""
			if value, ok := sidecarYamlLookup(generic, "sidecar", "format2", c.key); !ok || value.Decode(&mode) != nil || mode != "on" {
				t.Fatalf("format2's %s deletes with %q in %q", c.key, mode, text)
			}
		}
		for name, on := range config.switches {
			if on && !sidecarYamlTrue(generic, "sidecar", name, "enabled") {
				t.Fatalf("switch %s is on with no enabled: true in %q", name, text)
			}
		}
		if config.Reseed() && !sidecarYamlTrue(generic, "sidecar", "reseed") {
			t.Fatalf("reseed is on in %q", text)
		}
		if config.P0Enforce && !sidecarYamlTrue(generic, "sidecar", "p0_enforce", "enabled") {
			t.Fatalf("p0 enforces with no enabled: true in %q", text)
		}
		for networkId := range config.previewNetworkIds {
			if !sidecarYamlListsId(generic, networkId, "sidecar", "preview_networks") {
				t.Fatalf("%s previews, which %q does not list", networkId, text)
			}
		}
		if edge, ok := config.UrnetworkEdge(); ok {
			edgeStr := ""
			value, found := sidecarYamlLookup(generic, "sidecar", "urnetwork_edge")
			if !found || value.Decode(&edgeStr) != nil {
				t.Fatalf("urnetwork_edge %s with no key in %q", edge, text)
			}
			if id, err := ParseId(edgeStr); err != nil || id != edge {
				t.Fatalf("urnetwork_edge %s from %q in %q", edge, edgeStr, text)
			}
		}
	})
}

// config/main/sidecar.yml and config/local/sidecar.yml parse and are dark: no
// gate open, no switch on, P0 log-only, no network named.
func TestSidecarConfigFilesAreDark(t *testing.T) {
	for _, env := range []string{"main", "local"} {
		path := "../config/" + env + "/sidecar.yml"
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			t.Skipf("no config checkout beside the server (%s)", path)
		}
		if err != nil {
			t.Fatal(err)
		}
		config, err := parseSidecarConfig(data)
		if err != nil {
			t.Fatalf("%s: %s", path, err)
		}
		if config.P0Enforce || 0 < len(config.previewNetworkIds) || config.Reseed() || config.PlaceNew() != "" || 0 < len(config.Drained()) {
			t.Errorf("%s is not dark", path)
		}
		for _, name := range []string{SidecarGateEvs, SidecarGateGkm, SidecarGateSockets, SidecarGateTlsedge, SidecarGateMxedge, SidecarGateSmtpedge, SidecarGateDns} {
			gate := config.Service(name)
			if gate.Enabled() || gate.Percent() != 0 || 0 < len(gate.NetworkIds()) {
				t.Errorf("%s opens %s", path, name)
			}
		}
		if format2 := config.Format2(); format2.Gate().Enabled() || format2.Purge() != SidecarFormat2Observe || format2.Drop() != SidecarFormat2Observe {
			t.Errorf("%s opens format2", path)
		}
		for name, on := range config.switches {
			if on {
				t.Errorf("%s turns %s on", path, name)
			}
		}
		if _, ok := config.UrnetworkEdge(); ok {
			t.Errorf("%s names a network for urnetwork_edge", path)
		}
		if len(config.fileClusters) != 0 {
			t.Errorf("%s records a settle pass", path)
		}
	}
}

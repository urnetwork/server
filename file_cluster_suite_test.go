package server

// §22.9's file-cluster suite's own rules (EVSB-M2-17), on the fake store file_cluster_transport_test.go keeps: FS-63's
// refusal, a cluster with two pools failing the suite with nothing written; a cluster with no hooks or host control
// not passing, its FS-114 and host cases not run; a cluster of one host refused before any request (QD-756); a
// lifecycle rule and an unstamped build each failing; and a report naming settle_passed_build only for a run where
// every case ran and passed. The cases themselves run on T3's real clusters in file_cluster_suite_t3_test.go.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

// The fake store's requests through a transport that answers a bucket's lifecycle as lifecycle says, as the fake
// store does not.
type fileClusterSuiteLifecycleTransport struct {
	base      http.RoundTripper
	lifecycle string
}

func (self *fileClusterSuiteLifecycleTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if _, ok := request.URL.Query()["lifecycle"]; ok {
		if self.lifecycle == "" {
			return fakeS3Error(request, http.StatusNotFound, "NoSuchLifecycleConfiguration"), nil
		}
		return fakeS3Response(request, http.StatusOK, http.Header{"Content-Type": {"application/xml"}}, []byte(self.lifecycle)), nil
	}
	return self.base.RoundTrip(request)
}

// The hosts of the fake target: T3's metadata shape, 4 hosts of 2 drives.
var fileClusterSuiteTestEndpoints = []string{"host-a.example:9000", "host-b.example:9000", "host-c.example:9000", "host-d.example:9000"}

// Admin info of one pool whose set every host of the fake target serves two drives of, each host on commitId, in
// madmin's JSON as MinIO fills it.
func fileClusterSuiteFakeAdminInfo(commitId string) []byte {
	servers := []map[string]any{}
	for _, endpoint := range fileClusterSuiteTestEndpoints {
		servers = append(servers, map[string]any{
			"state":       "online",
			"endpoint":    endpoint,
			"commitID":    commitId,
			"poolNumber":  1,
			"poolNumbers": []int{1},
			"drives": []map[string]any{
				{"endpoint": "d1", "state": "ok", "pool_index": 0, "used_inodes": 10, "free_inodes": 90},
				{"endpoint": "d2", "state": "ok", "pool_index": 0, "used_inodes": 10, "free_inodes": 90},
			},
		})
	}
	body, err := json.Marshal(map[string]any{
		"mode":    "online",
		"backend": map[string]any{"backendType": "Erasure", "totalSets": []int{1}},
		"servers": servers,
		"pools":   map[string]any{"0": map[string]any{"0": map[string]any{"rawUsage": 100, "rawCapacity": 1000}}},
	})
	if err != nil {
		panic(fmt.Sprintf("the fake admin info: %v", err))
	}
	return body
}

// A fake store of one pool that 4 hosts serve, each on commitId.
func newFileClusterSuiteFakeStore(commitId string) *fakeS3 {
	store := newFakeS3(1, commitId)
	store.adminInfo = fileClusterSuiteFakeAdminInfo(commitId)
	return store
}

// A suite target on store: meta-1 at 4 hosts of 2 drives, with neither hooks nor host control.
func newFileClusterSuiteFakeTarget(store *fakeS3, lifecycle string) *FileClusterSuiteTarget {
	config := fileClusterTestConfigs(fileClusterSuiteTestEndpoints...)[0]
	settings := DefaultFileClusterSuiteSettings()
	settings.WaitLimit = 5 * time.Second
	target := &FileClusterSuiteTarget{
		Config:        config,
		DrivesPerHost: 2,
		Settings:      settings,
		newBaseTransport: func(tls bool) (http.RoundTripper, error) {
			return &fileClusterSuiteLifecycleTransport{base: store.transport("any"), lifecycle: lifecycle}, nil
		},
	}
	for _, endpoint := range fileClusterSuiteTestEndpoints {
		target.Hosts = append(target.Hosts, &FileClusterSuiteHost{Endpoint: endpoint})
	}
	return target
}

// A 40 hex digit commit as the fork's release stamps it.
const fileClusterSuiteTestCommit = "80a987bd32f8ea6f5206d19d6c3266ca5a751c08"

func fileClusterSuiteCaseOf(report *FileClusterSuiteReport, name string) *FileClusterSuiteCase {
	for _, c := range report.Cases {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// FS-63's suite refusal: a cluster with more than one pool fails the suite before any write, every later case not run.
func TestFileClusterSuiteRefusesTwoPoolsBeforeAnyWrite(t *testing.T) {
	store := newFakeS3(2, fileClusterSuiteTestCommit)
	report, err := RunFileClusterSuite(t.Context(), newFileClusterSuiteFakeTarget(store, ""))
	if err != nil {
		t.Fatal(err)
	}
	if c := fileClusterSuiteCaseOf(report, "one-pool"); c == nil || c.Outcome != FileClusterSuiteFail || !strings.Contains(c.Detail, "2 pools") {
		t.Fatalf("the pool case: %+v", c)
	}
	for _, c := range report.Cases {
		if c.Name != "one-pool" && c.Name != "one-build" && c.Outcome != FileClusterSuiteNotRun {
			t.Errorf("%s ran on a two-pool cluster: %+v", c.Name, c)
		}
	}
	for _, request := range store.takeRequests() {
		if request.method == http.MethodPut || request.method == http.MethodDelete || request.method == http.MethodPost {
			t.Errorf("a %s reached a two-pool cluster", request.method)
		}
	}
	if report.Passed() {
		t.Error("a two-pool cluster passed")
	}
	if build, ok := report.SettlePassedBuild(); ok {
		t.Errorf("a two-pool cluster's build %q recorded", build)
	}
}

// The one-pool twin, with no barrier hooks and no host control: the checks pass, the FS-114 and host cases do not
// run, and the suite does not pass, so a production cluster's run alone records no build.
func TestFileClusterSuiteWithoutHooksDoesNotPass(t *testing.T) {
	store := newFileClusterSuiteFakeStore(fileClusterSuiteTestCommit)
	report, err := RunFileClusterSuite(t.Context(), newFileClusterSuiteFakeTarget(store, ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"one-pool", "one-build", "no-lifecycle-rule"} {
		if c := fileClusterSuiteCaseOf(report, name); c == nil || c.Outcome != FileClusterSuitePass {
			t.Errorf("%s: %+v", name, c)
		}
	}
	for _, spec := range fileClusterSuiteCases() {
		c := fileClusterSuiteCaseOf(report, spec.name)
		if c == nil || c.Outcome != FileClusterSuiteNotRun || c.Detail == "" {
			t.Errorf("%s with no hooks or control: %+v", spec.name, c)
		}
	}
	if report.Build != fileClusterSuiteTestCommit {
		t.Errorf("the build %q", report.Build)
	}
	if report.Passed() {
		t.Error("a run with cases not run passed")
	}
	if _, ok := report.SettlePassedBuild(); ok {
		t.Error("a run with cases not run named a build")
	}
	if text := report.String(); !strings.Contains(text, "not passed: record no settle_passed_build") {
		t.Errorf("the report:\n%s", text)
	}
}

// QD-756: a cluster of one host is no cluster the suite runs on. Its target is refused before any request reaches
// the store, from the hosts file and from a target built by hand, and the refusal names the rule.
func TestFileClusterSuiteRefusesAClusterOfOneHost(t *testing.T) {
	store := newFakeS3(1, fileClusterSuiteTestCommit)
	target := newFileClusterSuiteFakeTarget(store, "")
	target.Config = fileClusterTestConfigs("host-a.example:9000")[0]
	target.Hosts = target.Hosts[:1]
	target.DrivesPerHost = 8
	report, err := RunFileClusterSuite(t.Context(), target)
	if !errors.Is(err, ErrFileClusterConfig) || report != nil {
		t.Fatalf("a target of one host: %+v, %v", report, err)
	}
	for _, part := range []string{"one host", "4 hosts", "§19.6"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the refusal does not say %q: %v", part, err)
		}
	}
	if requests := store.takeRequests(); len(requests) != 0 {
		t.Errorf("%d requests reached a cluster of one host", len(requests))
	}

	entry := &FileClusterSuiteHostsEntry{DrivesPerHost: 8, Hooks: []string{"127.0.0.1:52311"}, Containers: []string{"sidecar-test-fc-a"}}
	if _, err := NewFileClusterSuiteTarget(fileClusterTestConfigs("127.0.0.1:52301")[0], entry, nil); !errors.Is(err, ErrFileClusterConfig) || !strings.Contains(err.Error(), "one host") {
		t.Errorf("a hosts file entry for a cluster of one host: %v", err)
	}

	// two hosts are a cluster the suite starts on
	two := newFileClusterSuiteFakeTarget(store, "")
	two.Config = fileClusterTestConfigs(fileClusterSuiteTestEndpoints[:2]...)[0]
	two.Hosts = two.Hosts[:2]
	if _, err := RunFileClusterSuite(t.Context(), two); err != nil {
		t.Errorf("a target of two hosts: %v", err)
	}
}

// QD-254: a bucket with a lifecycle rule fails the suite.
func TestFileClusterSuiteFailsALifecycleRule(t *testing.T) {
	lifecycle := `<LifecycleConfiguration><Rule><ID>expire</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter>` +
		`<Expiration><Days>1</Days></Expiration></Rule></LifecycleConfiguration>`
	report, err := RunFileClusterSuite(t.Context(), newFileClusterSuiteFakeTarget(newFileClusterSuiteFakeStore(fileClusterSuiteTestCommit), lifecycle))
	if err != nil {
		t.Fatal(err)
	}
	if c := fileClusterSuiteCaseOf(report, "no-lifecycle-rule"); c == nil || c.Outcome != FileClusterSuiteFail {
		t.Fatalf("a lifecycle rule: %+v", c)
	}
}

// settle_passed_build names a commit: a build admin info does not show as one, as an unstamped build's, fails.
func TestFileClusterSuiteNamesOnlyAStampedCommit(t *testing.T) {
	for _, build := range []string{"DEVELOPMENT.GOGET", "", "80A987BD32F8EA6F5206D19D6C3266CA5A751C08", "80a987bd"} {
		report, err := RunFileClusterSuite(t.Context(), newFileClusterSuiteFakeTarget(newFileClusterSuiteFakeStore(build), ""))
		if err != nil {
			t.Fatal(err)
		}
		if c := fileClusterSuiteCaseOf(report, "one-build"); c == nil || c.Outcome != FileClusterSuiteFail || report.Build != "" {
			t.Errorf("build %q: %+v, report build %q", build, c, report.Build)
		}
	}
}

// A report names its build only when every case ran and passed.
func TestFileClusterSuiteReportNamesItsBuildOnlyWhenWhole(t *testing.T) {
	passed := func() *FileClusterSuiteReport {
		return &FileClusterSuiteReport{
			ClusterId: "meta-1",
			Build:     fileClusterSuiteTestCommit,
			Cases:     []*FileClusterSuiteCase{{Name: "a", Outcome: FileClusterSuitePass}, {Name: "b", Outcome: FileClusterSuitePass}},
		}
	}
	if build, ok := passed().SettlePassedBuild(); !ok || build != fileClusterSuiteTestCommit {
		t.Errorf("a whole run: %q %v", build, ok)
	}
	if text := passed().String(); !strings.Contains(text, "passed: settle_passed_build: "+fileClusterSuiteTestCommit) {
		t.Errorf("a whole run's report:\n%s", text)
	}
	for _, outcome := range []FileClusterSuiteOutcome{FileClusterSuiteFail, FileClusterSuiteNotRun} {
		report := passed()
		report.Cases[1].Outcome = outcome
		if _, ok := report.SettlePassedBuild(); ok || report.Passed() {
			t.Errorf("a run with a case %s named its build", outcome)
		}
	}
	report := passed()
	report.Build = ""
	if _, ok := report.SettlePassedBuild(); ok {
		t.Error("a run with no one build named one")
	}
	if (&FileClusterSuiteReport{Build: fileClusterSuiteTestCommit}).Passed() {
		t.Error("a run of no cases passed")
	}
}

// The hosts file names every host's hook and container or none, and its drive timeout reaches the settings.
func TestFileClusterSuiteHostsFile(t *testing.T) {
	text := []byte(`clusters:
  meta-1:
    drives_per_host: 2
    drive_timeout: 5s
    hooks: [127.0.0.1:52111, 127.0.0.1:52112]
    containers: [sidecar-test-fc-a, sidecar-test-fc-b]
  meta-2:
    drives_per_host: 2
    hooks: [127.0.0.1:52311, 127.0.0.1:52312]
`)
	hostsFile, err := ParseFileClusterSuiteHostsFile(text)
	if err != nil {
		t.Fatal(err)
	}
	config := fileClusterTestConfigs("127.0.0.1:52101", "127.0.0.1:52102")[0]
	target, err := NewFileClusterSuiteTarget(config, hostsFile.Clusters["meta-1"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if target.Settings.DriveTimeout != 5*time.Second || len(target.Hosts) != 2 || target.Hosts[1].HookAddress != "127.0.0.1:52112" {
		t.Fatalf("the target: %+v %+v", target.Settings, target.Hosts)
	}
	if docker, ok := target.Hosts[0].Control.(*FileClusterSuiteDockerHost); !ok || docker.Container != "sidecar-test-fc-a" {
		t.Errorf("host 0's control: %+v", target.Hosts[0].Control)
	}
	three := fileClusterTestConfigs("127.0.0.1:52301", "127.0.0.1:52302", "127.0.0.1:52303")[0]
	if _, err := NewFileClusterSuiteTarget(three, hostsFile.Clusters["meta-2"], nil); !errors.Is(err, ErrFileClusterConfig) || !strings.Contains(err.Error(), "hooks and containers") {
		t.Errorf("two hooks for three hosts: %v", err)
	}
	if _, err := NewFileClusterSuiteTarget(three, nil, nil); !errors.Is(err, ErrFileClusterConfig) {
		t.Errorf("no entry: %v", err)
	}
	if _, err := ParseFileClusterSuiteHostsFile([]byte("clusters: [")); err == nil {
		t.Error("a hosts file that does not parse")
	}
	// the vault file's shape parses as LoadFileClusters reads it
	configs, err := ParseFileClusterConfigs([]byte(`clusters:
  meta-1: {kind: metadata, first: true, endpoints: [127.0.0.1:52101], access_key: a, secret_key: s, bucket: b, region: us-east-1}
`))
	if err != nil || len(configs) != 1 || !bytes.Equal([]byte(configs[0].Bucket), []byte("b")) {
		t.Errorf("the clusters file: %+v %v", configs, err)
	}
}

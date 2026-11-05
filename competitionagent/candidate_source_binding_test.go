//go:build linux

// Source binding uses private graph documents and fake image GET/create
// responses. No real daemon, host probe, image, compiler, or source helper runs.
package competitionagent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The production proxy fixture retains all common caps/profile admission.
// This extension supplies an actual helper-shaped post-patch source document.
type candidateSourceTest struct {
	fixture  *dockerProxyTest
	source   string
	identity candidateSourceIdentity
	imageId  string
	labels   map[string]string
}

// Source bytes, image labels, and the precommitted build-key prefix agree.
func newCandidateSourceTest(t *testing.T) *candidateSourceTest {
	t.Helper()
	fixture := newDockerProxyTest(t)
	source := filepath.Join(fixture.plan.ContextRoot, ".evidence-runtime", "evaluation-sources.Q1w2E3r4", "candidate")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	repositories := map[string]string{}
	for index, repository := range strings.Fields("server connect sdk proxy glog goidenticons userwireguard sn warp gvisor") {
		repositories[repository] = fmt.Sprintf("%040x", index+1)
	}
	keyPrefix := strings.TrimSuffix(strings.TrimPrefix(fixture.plan.BuildCgroupName, "urnetwork-submission-build-"), ".slice")
	self := &candidateSourceTest{fixture: fixture, source: source, imageId: "sha256:" + strings.Repeat("c", 64),
		identity: candidateSourceIdentity{Schema: 1, Kind: "sim-latency-evaluation-source", Temporary: true,
			BaseImageId: fixture.plan.BaseImageDigest, BaseSha: strings.Repeat("a", 40), SourceEpoch: 1, Branch: "sim-latency",
			SourceLockSha256: strings.Repeat("e", 64), Repositories: repositories, CandidatePatchSha256: strings.Repeat("d", 64), CandidatePathBase: "mono_root_v1"},
		labels: map[string]string{candidateSourceLabelPrefix + "image-kind": "submission",
			candidateSourceLabelPrefix + "source-materialization": "required-readonly-run-source/v1",
			candidateSourceLabelPrefix + "builtin-source-kind":    "pristine-evaluator-base",
			candidateSourceLabelPrefix + "image-key":              keyPrefix + strings.Repeat("0", 32),
			candidateSourceLabelPrefix + "base-sha":               strings.Repeat("a", 40), candidateSourceLabelPrefix + "patch-sha256": strings.Repeat("d", 64),
			"org.opencontainers.image.revision": repositories["server"]}}
	self.writeIdentity(t)
	return self
}

// Tests rehash malformed documents too, so schema controls cannot pass merely
// because an earlier digest check rejected the changed bytes first.
func (self *candidateSourceTest) writeIdentity(t *testing.T) {
	t.Helper()
	content, err := json.Marshal(&self.identity)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(self.source, ".evaluation-source.json")
	if _, err := os.Lstat(path); err == nil {
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(path, content, 0400); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	self.labels[candidateSourceLabelPrefix+"candidate-source-identity-sha256"] = digestDockerBytes(content)
}

// Only exact image-inspection GET and create POST are served. Counts prove
// that failed source admission never issued an upstream mutation.
func (self *candidateSourceTest) daemon(t *testing.T, mutate func(map[string]any)) (*atomic.Int64, *atomic.Int64) {
	t.Helper()
	reads, creates := &atomic.Int64{}, &atomic.Int64{}
	self.fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/images/") && strings.HasSuffix(r.URL.Path, "/json"):
			reads.Add(1)
			object := map[string]any{"Id": self.imageId, "Config": map[string]any{"Labels": self.labels}}
			if mutate != nil {
				mutate(object)
			}
			_ = json.NewEncoder(w).Encode(object)
		case r.Method == http.MethodPost && r.URL.Path == "/v1.56/containers/create":
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]string{"Id": strings.Repeat("f", 64)})
		default:
			http.Error(w, "unexpected synthetic endpoint", http.StatusInternalServerError)
		}
	})
	return reads, creates
}

// Both real client encodings describe the same exact read-only workspace.
func (self *candidateSourceTest) create(t *testing.T, structured bool, mutate func(map[string]any)) *httptest.ResponseRecorder {
	t.Helper()
	return dockerPolicyTestCreate(t, self.fixture, func(object map[string]any) {
		object["Image"] = self.imageId
		host := object["HostConfig"].(map[string]any)
		if structured {
			host["Mounts"] = []map[string]any{{"Type": "bind", "Source": self.source, "Target": "/workspace", "ReadOnly": true,
				"BindOptions": map[string]any{"Propagation": "rprivate", "ReadOnlyForceRecursive": true}}}
		} else {
			host["Binds"] = []string{self.source + ":/workspace:ro,rprivate"}
		}
		if mutate != nil {
			mutate(object)
		}
	})
}

// Every submission probe/measured group must bind the same authenticated
// post-patch graph, irrespective of legacy or structured mount encoding.
func TestCandidateSourceBindingAcceptsExactImageAndBothReadOnlyMountForms(t *testing.T) {
	for _, structured := range []bool{false, true} {
		fixture := newCandidateSourceTest(t)
		reads, creates := fixture.daemon(t, nil)
		for _, group := range fixture.fixture.plan.CgroupNames {
			if group == fixture.fixture.plan.BuildCgroupName {
				continue
			}
			response := fixture.create(t, structured, func(object map[string]any) { object["HostConfig"].(map[string]any)["CgroupParent"] = group })
			if response.Code != http.StatusCreated {
				t.Fatalf("exact candidate image/source binding was rejected: structured=%v group=%s status=%d", structured, group, response.Code)
			}
		}
		if reads.Load() != int64(len(fixture.fixture.plan.CgroupNames)-1) || creates.Load() != reads.Load() {
			t.Fatal("submission create bypassed its image inspection")
		}
	}
}

// Sealing intentionally makes source readable by the mapped container user.
// Custody is in the private outer directories, not a private candidate leaf.
func TestCandidateSourceBindingAcceptsSealedMeasurementModes(t *testing.T) {
	fixture := newCandidateSourceTest(t)
	if err := os.Chmod(filepath.Join(fixture.source, ".evaluation-source.json"), 0444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fixture.source, 0555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(fixture.source, 0700) })
	_, creates := fixture.daemon(t, nil)
	if response := fixture.create(t, true, nil); response.Code != http.StatusCreated || creates.Load() != 1 {
		t.Fatal("sealed 0555/0444 candidate source was rejected")
	}
}

// Either exposed outer root invalidates custody even when the candidate leaf
// and identity are correctly sealed for measurement.
func TestCandidateSourceBindingRefusesExposedOuterRoots(t *testing.T) {
	for _, runRoot := range []bool{false, true} {
		fixture := newCandidateSourceTest(t)
		outer := filepath.Dir(fixture.source)
		if runRoot {
			outer = fixture.fixture.plan.ContextRoot
		}
		if err := os.Chmod(outer, 0755); err != nil {
			t.Fatal(err)
		}
		_, creates := fixture.daemon(t, nil)
		if response := fixture.create(t, false, nil); response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatal("public outer source root authorized a candidate create")
		}
	}
}

// Omitting, replacing, exposing writable, or duplicating the workspace cannot
// fall back to the image's intentionally pristine built-in source graph.
func TestCandidateSourceBindingRejectsMissingAndMismatchedMounts(t *testing.T) {
	for _, mutate := range []func(*candidateSourceTest, map[string]any){
		func(fixture *candidateSourceTest, host map[string]any) { host["Binds"] = nil },
		func(fixture *candidateSourceTest, host map[string]any) {
			host["Binds"] = []string{fixture.source + ":/workspace:rw"}
		},
		func(fixture *candidateSourceTest, host map[string]any) {
			host["Binds"] = []string{fixture.source + ":/workspace:ro", fixture.source + ":/workspace:ro"}
		},
		func(fixture *candidateSourceTest, host map[string]any) {
			host["Binds"] = []string{filepath.Dir(fixture.source) + ":/workspace:ro"}
		},
		func(fixture *candidateSourceTest, host map[string]any) {
			baseline := filepath.Join(filepath.Dir(fixture.source), "baseline")
			dockerPolicyTestDirectory(t, baseline)
			host["Binds"] = []string{baseline + ":/workspace:ro"}
		},
		func(fixture *candidateSourceTest, host map[string]any) {
			other := filepath.Join(filepath.Dir(fixture.fixture.plan.ContextRoot), strings.Repeat("1", 64), ".evidence-runtime", "evaluation-sources.Q1w2E3r4", "candidate")
			dockerPolicyTestDirectory(t, other)
			host["Binds"] = []string{other + ":/workspace:ro"}
		},
	} {
		fixture := newCandidateSourceTest(t)
		_, creates := fixture.daemon(t, nil)
		response := fixture.create(t, false, func(object map[string]any) { mutate(fixture, object["HostConfig"].(map[string]any)) })
		if response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatal("invalid candidate workspace reached daemon create")
		}
	}
}

// Matching image labels cannot bless a malformed, stale, foreign or incomplete
// post-patch identity, even if the attacker rehashes the changed document.
func TestCandidateSourceBindingRejectsInvalidPostPatchIdentity(t *testing.T) {
	for _, mutate := range []func(*candidateSourceIdentity){
		func(identity *candidateSourceIdentity) { identity.Schema = 2 },
		func(identity *candidateSourceIdentity) { identity.Kind = "synthetic-other-source" },
		func(identity *candidateSourceIdentity) { identity.Temporary = false },
		func(identity *candidateSourceIdentity) { identity.SourceEpoch = -1 },
		func(identity *candidateSourceIdentity) { identity.Branch = "synthetic-branch" },
		func(identity *candidateSourceIdentity) { identity.Staging = true },
		func(identity *candidateSourceIdentity) { identity.BaseImageId = "sha256:" + strings.Repeat("0", 64) },
		func(identity *candidateSourceIdentity) { identity.BaseSha = strings.Repeat("0", 40) },
		func(identity *candidateSourceIdentity) { identity.CandidatePatchSha256 = "" },
		func(identity *candidateSourceIdentity) { identity.CandidatePatchSha256 = strings.Repeat("0", 64) },
		func(identity *candidateSourceIdentity) { identity.SourceLockSha256 = "" },
		func(identity *candidateSourceIdentity) { delete(identity.Repositories, "sn") },
		func(identity *candidateSourceIdentity) {
			identity.Repositories["synthetic-extra"] = strings.Repeat("0", 40)
		},
		func(identity *candidateSourceIdentity) { identity.Repositories["server"] = strings.Repeat("0", 40) },
		func(identity *candidateSourceIdentity) { identity.Repositories["warp"] = strings.Repeat("0", 64) },
	} {
		fixture := newCandidateSourceTest(t)
		mutate(&fixture.identity)
		fixture.writeIdentity(t)
		_, creates := fixture.daemon(t, nil)
		if response := fixture.create(t, false, nil); response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatal("invalid post-patch source identity reached daemon create")
		}
	}
}

// The source-materialization class cannot claim that the image's inherited
// pristine workspace was patched; its exact image/build/source labels bind it.
func TestCandidateSourceBindingRejectsChangedImageLabelsAndMutableAliases(t *testing.T) {
	for _, mutate := range []func(*candidateSourceTest){
		func(fixture *candidateSourceTest) { delete(fixture.labels, candidateSourceLabelPrefix+"image-kind") },
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"source-materialization"] = "synthetic-builtin-source"
		},
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"builtin-source-kind"] = "patched-candidate"
		},
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"image-key"] = strings.Repeat("0", 64)
		},
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"candidate-source-identity-sha256"] = strings.Repeat("0", 64)
		},
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"base-sha"] = strings.Repeat("0", 40)
		},
		func(fixture *candidateSourceTest) {
			fixture.labels[candidateSourceLabelPrefix+"patch-sha256"] = strings.Repeat("0", 64)
		},
		func(fixture *candidateSourceTest) {
			fixture.labels["org.opencontainers.image.revision"] = strings.Repeat("0", 40)
		},
		func(fixture *candidateSourceTest) {
			fixture.imageId = "urnetwork/sim-latency-submission:synthetic-mutable"
		},
	} {
		fixture := newCandidateSourceTest(t)
		mutate(fixture)
		_, creates := fixture.daemon(t, nil)
		if response := fixture.create(t, true, nil); response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatal("changed image source class reached daemon create")
		}
	}
}

// Relative symlinks are rejected even when they stay inside the confined root.
// A hard link or writable identity likewise lacks a unique sealed generation.
func TestCandidateSourceBindingRefusesSymlinkHardlinkAndWritableIdentities(t *testing.T) {
	for _, kind := range []string{"leaf", "ancestor", "hardlink", "writable"} {
		fixture := newCandidateSourceTest(t)
		path := filepath.Join(fixture.source, ".evaluation-source.json")
		switch kind {
		case "leaf":
			retained := filepath.Join(fixture.source, "identity-retained.json")
			if err := os.Rename(path, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(retained), path); err != nil {
				t.Fatal(err)
			}
		case "ancestor":
			retained := fixture.source + "-retained"
			if err := os.Rename(fixture.source, retained); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(retained), fixture.source); err != nil {
				t.Fatal(err)
			}
		case "hardlink":
			if err := os.Link(path, filepath.Join(fixture.source, "identity-hardlink.json")); err != nil {
				t.Fatal(err)
			}
		case "writable":
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := fixture.fixture.proxy.readCandidateSourceIdentity(fixture.source); err == nil {
			t.Fatalf("raw held-descriptor reader accepted unsafe %s identity", kind)
		}
		_, creates := fixture.daemon(t, nil)
		if response := fixture.create(t, false, nil); response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatalf("unsafe %s source identity reached daemon create", kind)
		}
	}
}

// A non-success, unavailable, oversized, ambiguous or different-id image
// response is not permission to execute an otherwise valid source mount.
func TestCandidateSourceBindingRefusesUnknownAndMalformedImageFacts(t *testing.T) {
	for _, kind := range []string{"not-found", "server-error", "oversized", "bad-json", "case-alias", "lower-id", "wrong-id", "no-config"} {
		fixture := newCandidateSourceTest(t)
		creates := &atomic.Int64{}
		fixture.fixture.daemon(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				creates.Add(1)
				w.WriteHeader(http.StatusCreated)
				return
			}
			switch kind {
			case "not-found":
				w.WriteHeader(http.StatusNotFound)
			case "server-error":
				w.WriteHeader(http.StatusInternalServerError)
			case "oversized":
				_, _ = w.Write([]byte(strings.Repeat(" ", int(fixture.fixture.settings.MaximumDocumentBytes)+1)))
			case "bad-json":
				_, _ = w.Write([]byte("{"))
			case "case-alias":
				_, _ = fmt.Fprintf(w, `{"Id":%q,"id":%q,"Config":{"Labels":{}}}`, fixture.imageId, fixture.imageId)
			case "lower-id":
				_, _ = fmt.Fprintf(w, `{"id":%q,"Config":{"Labels":{}}}`, fixture.imageId)
			case "wrong-id":
				_ = json.NewEncoder(w).Encode(map[string]any{"Id": "sha256:" + strings.Repeat("0", 64), "Config": map[string]any{"Labels": fixture.labels}})
			case "no-config":
				_ = json.NewEncoder(w).Encode(map[string]string{"Id": fixture.imageId})
			}
		})
		if response := fixture.create(t, true, nil); response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatalf("unknown image fact %s authorized create", kind)
		}
	}
}

// Trusted sidecars are exact protected references and a fixed user, not a
// family wildcard. Their returned config id is not another request authority.
func TestCandidateSourceBindingAllowsOnlyExactProtectedBackingReferences(t *testing.T) {
	for _, backing := range []bool{false, true} {
		fixture := newCandidateSourceTest(t)
		fixture.labels = nil
		reads, creates := fixture.daemon(t, nil)
		for _, image := range fixture.fixture.plan.BackingImages {
			response := fixture.create(t, false, func(object map[string]any) {
				object["Image"], object["User"] = image, "999:999"
				object["HostConfig"].(map[string]any)["Binds"] = nil
				if !backing {
					object["Image"] = fixture.imageId
				}
			})
			if backing && response.Code != http.StatusCreated || !backing && response.Code != http.StatusForbidden {
				t.Fatalf("backing reference authority changed: backing=%v status=%d", backing, response.Code)
			}
		}
		if reads.Load() != 2 || backing && creates.Load() != 2 || !backing && creates.Load() != 0 {
			t.Fatal("backing image exempted a non-authoritative alias")
		}
	}
	fixture := newCandidateSourceTest(t)
	fixture.labels = nil
	_, creates := fixture.daemon(t, nil)
	for _, image := range []string{"postgres:synthetic", "redis:synthetic@sha256:" + strings.Repeat("f", 64)} {
		response := fixture.create(t, true, func(object map[string]any) { object["Image"], object["User"] = image, "999:999" })
		if response.Code != http.StatusForbidden || creates.Load() != 0 {
			t.Fatal("an unlisted sidecar reference acquired source exemption")
		}
	}
	response := fixture.create(t, true, func(object map[string]any) {
		object["Image"], object["User"] = fixture.fixture.plan.BackingImages[0], "65532:65532"
		object["HostConfig"].(map[string]any)["Mounts"] = nil
	})
	if response.Code != http.StatusForbidden || creates.Load() != 0 {
		t.Fatal("backing-image exemption accepted the wrong fixed-role user")
	}
}

// The retained plan owns a copy of the exact two-reference authority.
func TestCandidateSourceBindingBackingListIsClosedAndCopied(t *testing.T) {
	for _, images := range [][]string{
		nil,
		{"postgres:synthetic@sha256:" + strings.Repeat("a", 64)},
		{"postgres:synthetic@sha256:" + strings.Repeat("a", 64), "postgres:other@sha256:" + strings.Repeat("b", 64)},
		{"postgres:synthetic", "redis:synthetic@sha256:" + strings.Repeat("b", 64)},
		{"docker.io/library/postgres:synthetic@sha256:" + strings.Repeat("a", 64), "redis:synthetic@sha256:" + strings.Repeat("b", 64)},
	} {
		if validateDockerBackingImages(images) == nil {
			t.Fatalf("unreviewed backing-image list was accepted: %v", images)
		}
	}
	fixture := newDockerProxyTest(t)
	actual := fixture.proxy.plan.BackingImages[0]
	fixture.plan.BackingImages[0] = "postgres:synthetic-mutable"
	if fixture.proxy.plan.BackingImages[0] != actual {
		t.Fatal("caller mutated the retained proxy backing-image authority")
	}
}

// Byte/depth bounds and exact keys apply to the held source document itself,
// independently of the enclosing Docker create document's parser.
func TestCandidateSourceBindingIdentityJsonIsStrictAndBounded(t *testing.T) {
	for _, kind := range []string{"duplicate", "case-alias", "unknown", "trailing", "byte-bound", "depth-bound"} {
		fixture := newCandidateSourceTest(t)
		path := filepath.Join(fixture.source, ".evaluation-source.json")
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "duplicate":
			content = []byte(strings.Replace(string(content), `"schema":1`, `"schema":1,"schema":1`, 1))
		case "case-alias":
			content = []byte(strings.Replace(string(content), `"schema":1`, `"Schema":1`, 1))
		case "unknown":
			content = []byte(strings.Replace(string(content), `"schema":1`, `"schema":1,"synthetic_extra_authority":true`, 1))
		case "trailing":
			content = append(content, []byte(" {}")...)
		case "byte-bound":
			fixture.fixture.proxy.settings.MaximumDocumentBytes = 16
		case "depth-bound":
			fixture.fixture.proxy.settings.MaximumDocumentDepth = 1
		}
		if err := os.Chmod(path, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0400); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0400); err != nil {
			t.Fatal(err)
		}
		if _, _, err := fixture.fixture.proxy.readCandidateSourceIdentity(fixture.source); err == nil {
			t.Fatalf("source identity ignored its %s control", kind)
		}
	}
}

// The common bind validator's finite tracked paths do not grant those host
// capabilities to measurements, probes or backing services outside build.
func TestCandidateSourceBindingReservesTrackedFilesToBuildRole(t *testing.T) {
	for _, backing := range []bool{false, true} {
		for _, binding := range []struct {
			leaf   string
			target string
			mode   string
		}{
			{leaf: "check-cache", target: candidateSourceCacheTarget, mode: "rw"},
			{leaf: "compile-cache", target: candidateSourceCacheTarget, mode: "rw"},
			{leaf: "output", target: "/candidate-output", mode: "rw"},
			{leaf: "output", target: "/artifacts", mode: "ro"},
		} {
			fixture := newCandidateSourceTest(t)
			fixture.labels = nil
			reads, creates := fixture.daemon(t, nil)
			var object map[string]json.RawMessage
			if err := json.Unmarshal(fixture.fixture.create, &object); err != nil {
				t.Fatal(err)
			}
			if backing {
				object["Image"], _ = json.Marshal(fixture.fixture.plan.BackingImages[0])
				object["User"] = json.RawMessage(`"999:999"`)
			}
			var host dockerPolicyHostConfig
			if err := json.Unmarshal(object["HostConfig"], &host); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", binding.leaf)
			dockerPolicyTestDirectory(t, source)
			host.Binds = []string{source + ":" + binding.target + ":" + binding.mode}
			if err := fixture.fixture.proxy.verifyCandidateSourceBinding(context.Background(), object, host); err == nil || reads.Load() != 0 || creates.Load() != 0 {
				t.Fatalf("tracked %s leaked outside build role: backing=%v", binding.leaf, backing)
			}
		}
	}
}

// The common gate's finite writable-path extension is integrated separately.
// This helper tests the new final source-binding hook on real stage schemas.
func (self *candidateSourceTest) trackedBuild(t *testing.T, stage string, mutate func(map[string]json.RawMessage, *dockerPolicyHostConfig)) error {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(self.fixture.create, &object); err != nil {
		t.Fatal(err)
	}
	labels := map[string]string{cleanupJobLabel: self.fixture.plan.JobId, cleanupRoundLabel: self.fixture.plan.RoundId,
		dockerProxyRunLabel: self.fixture.plan.RunId, candidateSourceBuildStageLabel: stage}
	object["Labels"], _ = json.Marshal(labels)
	var host dockerPolicyHostConfig
	if err := json.Unmarshal(object["HostConfig"], &host); err != nil {
		t.Fatal(err)
	}
	host.CgroupParent = self.fixture.plan.BuildCgroupName
	cache := filepath.Join(self.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", stage+"-cache")
	dockerPolicyTestDirectory(t, cache)
	host.Binds = []string{self.source + ":/workspace:ro", cache + ":" + candidateSourceCacheTarget + ":rw"}
	if stage == "compile" {
		output := filepath.Join(self.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", "output")
		dockerPolicyTestDirectory(t, output)
		host.Binds = append(host.Binds, output+":/candidate-output:rw")
	}
	if mutate != nil {
		mutate(object, &host)
	}
	return self.fixture.proxy.verifyCandidateSourceBinding(context.Background(), object, host)
}

// Both fresh base-image stages bind the same candidate graph but never the
// same mutable download cache; only compile obtains its output directory.
func TestCandidateSourceBindingTrackedBuildRequiresDisjointStageBindings(t *testing.T) {
	for _, stage := range []string{"check", "compile"} {
		fixture := newCandidateSourceTest(t)
		if err := fixture.trackedBuild(t, stage, nil); err != nil {
			t.Fatalf("actual %s source/cache/output stage was rejected: %v", stage, err)
		}
	}
}

// A stage cannot borrow the other cache, produce output during check, omit
// source, or execute another image/user/group under the build label.
func TestCandidateSourceBindingTrackedBuildRejectsIsolationBypasses(t *testing.T) {
	for _, stage := range []string{"check", "compile"} {
		for _, mutate := range []func(*candidateSourceTest, map[string]json.RawMessage, *dockerPolicyHostConfig){
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				host.Binds = host.Binds[1:]
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				host.Binds = host.Binds[:1]
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				host.Binds[1] = filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", "synthetic-shared-cache") + ":" + candidateSourceCacheTarget + ":rw"
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				other := "compile"
				if stage == "compile" {
					other = "check"
				}
				host.Binds[1] = filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", other+"-cache") + ":" + candidateSourceCacheTarget + ":rw"
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				host.Binds[1] = strings.TrimSuffix(host.Binds[1], ":rw") + ":ro"
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				object["Image"], _ = json.Marshal(fixture.imageId)
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				object["User"] = json.RawMessage(`"999:999"`)
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				labels := map[string]string{cleanupJobLabel: fixture.fixture.plan.JobId, cleanupRoundLabel: fixture.fixture.plan.RoundId, dockerProxyRunLabel: fixture.fixture.plan.RunId}
				object["Labels"], _ = json.Marshal(labels)
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				host.CgroupParent = fixture.fixture.plan.CgroupNames[len(fixture.fixture.plan.CgroupNames)-1]
			},
			func(fixture *candidateSourceTest, object map[string]json.RawMessage, host *dockerPolicyHostConfig) {
				output := filepath.Join(fixture.fixture.plan.ContextRoot, ".evidence-runtime", "tracked-build", "output")
				host.Binds = append(host.Binds, output+":/candidate-output:rw")
			},
		} {
			fixture := newCandidateSourceTest(t)
			if err := fixture.trackedBuild(t, stage, func(object map[string]json.RawMessage, host *dockerPolicyHostConfig) { mutate(fixture, object, host) }); err == nil {
				t.Fatalf("tracked %s source/cache/output bypass was accepted", stage)
			}
		}
	}
}

// Ordinary base probes skip image GET, but entering the build cgroup without
// its stage does not acquire a generic base-container execution escape.
func TestCandidateSourceBindingBaseProbeSkipsInspectButBuildNeedsStage(t *testing.T) {
	fixture := newCandidateSourceTest(t)
	reads, creates := fixture.daemon(t, nil)
	if response := dockerPolicyTestCreate(t, fixture.fixture, nil); response.Code != http.StatusCreated || reads.Load() != 0 || creates.Load() != 1 {
		t.Fatal("pristine base probe unexpectedly required submission image inspection")
	}
	response := dockerPolicyTestCreate(t, fixture.fixture, func(object map[string]any) {
		object["HostConfig"].(map[string]any)["CgroupParent"] = fixture.fixture.plan.BuildCgroupName
	})
	if response.Code != http.StatusForbidden || reads.Load() != 0 || creates.Load() != 1 {
		t.Fatal("base-image build escaped its mandatory stage/source grammar")
	}
}

// Original expiry and explicit request cancellation refuse inspection before
// even the fake daemon is contacted; no later clock can reopen that lifetime.
func TestCandidateSourceBindingInspectionPreservesDeadlineAndCancellation(t *testing.T) {
	fixture := newCandidateSourceTest(t)
	reads, _ := fixture.daemon(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := fixture.fixture.proxy.inspectCandidateSourceImage(ctx, fixture.imageId); err == nil {
		t.Fatal("canceled source inspection proceeded")
	}
	fixture.fixture.proxy.plan.DeadlineAt = fixture.fixture.settings.Now().Add(-time.Nanosecond)
	if _, _, err := fixture.fixture.proxy.inspectCandidateSourceImage(context.Background(), fixture.imageId); err == nil || reads.Load() != 0 {
		t.Fatal("expired original source-inspection lifetime was reset")
	}
}

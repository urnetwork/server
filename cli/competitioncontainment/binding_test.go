//go:build linux

// Synthetic signatures and private files exercise provenance, never a daemon.
package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/server/competitionagent"
)

type bindingTestArtifacts struct {
	contents map[string][]byte
	verifies int
}

func bindingTestFile(path string, content []byte) competitionagent.File {
	digest := sha256.Sum256(content)
	return competitionagent.File{Path: path, Sha256: hex.EncodeToString(digest[:]), Bytes: int64(len(content))}
}

func (self *bindingTestArtifacts) Verify(expected competitionagent.File) error {
	self.verifies++
	content, ok := self.contents[expected.Path]
	if !ok || bindingTestFile(expected.Path, content) != expected {
		return errors.New("synthetic artifact changed")
	}
	return nil
}

func (self *bindingTestArtifacts) Read(path string, maximum int64) ([]byte, error) {
	content, ok := self.contents[path]
	if !ok || int64(len(content)) > maximum {
		return nil, errors.New("synthetic document unavailable")
	}
	return append([]byte(nil), content...), nil
}

type bindingTestFixture struct {
	config          competitionagent.Config
	boundary        competitionagent.RuntimeResourceBoundary
	manifest        competitionagent.StartManifest
	response        competitionagent.Response
	key             ed25519.PrivateKey
	public          ed25519.PublicKey
	patch, baseline string
	files           *bindingTestArtifacts
}

func newBindingTest(t *testing.T) *bindingTestFixture {
	t.Helper()
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := competitionagent.Config{Schema: 1, HostId: "synthetic-host", Limits: competitionagent.DefaultLimits(), EvaluatorImageDigest: "sha256:" + strings.Repeat("1", 64), SourceLockSha256: strings.Repeat("2", 64), QualificationSha256: strings.Repeat("3", 64), ConfigLocalSha256: strings.Repeat("4", 64), VaultLocalSha256: strings.Repeat("5", 64), Evaluator: competitionagent.Pin{Sha256: strings.Repeat("6", 64)}, Agent: competitionagent.Pin{Sha256: strings.Repeat("7", 64)}, SubmissionBuilderSha256: strings.Repeat("8", 64), DockerRole: competitionagent.DockerRoleConfig{ResourceBoundary: competitionagent.Pin{Sha256: strings.Repeat("9", 64)}, ResourcePolicy: competitionagent.Pin{Sha256: strings.Repeat("a", 64)}}}
	patch := bindingTestFile("canonical.patch", []byte("synthetic operator patch\n"))
	baseline := bindingTestFile("round-baseline.json", []byte("{\"synthetic_baseline\":true}\n"))
	identity := competitionagent.Identity{HostId: c.HostId, JobId: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", RoundId: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", RoundEpoch: 2, SourceEpoch: 1, Attempt: 1, StartedAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)}
	identity.DeadlineAt = identity.StartedAt.Add(time.Hour)
	identity.RunId = competitionagent.RunId(identity.JobId, 0, 1)
	request, err := json.Marshal(map[string]any{"job_id": identity.JobId, "round_id": identity.RoundId, "patch_sha256": patch.Sha256, "round_baseline_sha256": baseline.Sha256, "evaluation_policy": map[string]any{"replicates": 1}})
	if err != nil {
		t.Fatal(err)
	}
	m := competitionagent.StartManifest{Identity: identity, EvaluatorImageDigest: c.EvaluatorImageDigest, EvaluatorCommandSha256: c.Evaluator.Sha256, SourceLockSha256: c.SourceLockSha256, QualificationSha256: c.QualificationSha256, ConfigLocalSha256: c.ConfigLocalSha256, VaultLocalSha256: c.VaultLocalSha256, RoundBaselineSha256: baseline.Sha256, EvaluatorRequest: request, Inputs: []competitionagent.File{patch, baseline}}
	worker, err := json.Marshal(map[string]any{"schema": 1, "job_id": identity.JobId, "eval_error": nil, "score": map[string]any{"placeable": true, "baseline_sha256": baseline.Sha256}})
	if err != nil {
		t.Fatal(err)
	}
	files := &bindingTestArtifacts{contents: map[string][]byte{"baseline.json": []byte("{\"synthetic_baseline\":true}\n"), "worker-result.json": worker, "evidence/runs/candidate-01/containers.json": []byte("[]\n")}}
	completion, err := json.Marshal(map[string]any{"schema": 1, "kind": "sim-latency-worker-evaluation-complete", "job_id": identity.JobId, "round_id": identity.RoundId, "attempt": identity.Attempt, "base_image_id": c.EvaluatorImageDigest, "patch_sha256": patch.Sha256, "cleanup_complete": true, "artifacts": map[string]any{"baseline": baseline.Sha256}})
	if err != nil {
		t.Fatal(err)
	}
	files.contents["evaluation.complete.json"] = completion
	manifestSha, err := competitionagent.ManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	response := competitionagent.Response{Schema: 1, HostId: c.HostId, RunId: identity.RunId, Identity: &identity, FencingToken: 1, State: "succeeded", ManifestSha256: manifestSha}
	for _, path := range []string{"baseline.json", "worker-result.json", "evaluation.complete.json", "evidence/runs/candidate-01/containers.json"} {
		response.Files = append(response.Files, bindingTestFile(path, files.contents[path]))
	}
	f := &bindingTestFixture{config: c, boundary: competitionagent.RuntimeResourceBoundary{ContainerRootUid: 200000, ContainerRootGid: 300000, ContainerIdMapLength: 65536}, manifest: m, response: response, key: key, public: public, patch: patch.Sha256, baseline: baseline.Sha256, files: files}
	f.sign(t)
	return f
}

func (self *bindingTestFixture) sign(t *testing.T) {
	t.Helper()
	if err := competitionagent.SignResponse(&self.response, self.key); err != nil {
		t.Fatal(err)
	}
}

func (self *bindingTestFixture) bind() (map[string]any, error) {
	return privateContainmentBinding(self.config, self.boundary, self.manifest, self.response, self.public, self.patch, self.baseline, self.files)
}

func TestPrivateBindingAuthenticatesImportedBaselineAndLocalCandidate(t *testing.T) {
	f := newBindingTest(t)
	binding, err := f.bind()
	if err != nil || binding["baseline_sha256"] != f.baseline || binding["local_container_evidence_count"] != 1 || binding["grading_ready"] != false || f.files.verifies != len(f.response.Files) {
		t.Fatal("shared-baseline binding failed or granted readiness", binding, err)
	}
}

func TestPrivateBindingRejectsUnsignedResultBeforeFileAccess(t *testing.T) {
	f := newBindingTest(t)
	f.response.State = "failed"
	if _, err := f.bind(); err == nil || f.files.verifies != 0 {
		t.Fatal("unsigned result touched artifacts", err, f.files.verifies)
	}
}

func TestPrivateBindingRejectsWrongSignedScope(t *testing.T) {
	for _, change := range []func(*bindingTestFixture){
		func(f *bindingTestFixture) { f.response.HostId = "synthetic-other" },
		func(f *bindingTestFixture) { f.response.ManifestSha256 = strings.Repeat("b", 64) },
		func(f *bindingTestFixture) { f.response.State = "failed" },
		func(f *bindingTestFixture) { f.config.SourceLockSha256 = strings.Repeat("c", 64) },
		func(f *bindingTestFixture) { f.baseline = strings.Repeat("d", 64) },
		func(f *bindingTestFixture) { f.patch = strings.Repeat("e", 64) },
	} {
		f := newBindingTest(t)
		change(f)
		f.sign(t)
		if _, err := f.bind(); err == nil {
			t.Fatal("signed but mismatched scope was accepted")
		}
	}
}

func TestPrivateBindingRejectsChangedAndDuplicateArtifacts(t *testing.T) {
	for _, change := range []func(*bindingTestFixture){
		func(f *bindingTestFixture) { f.files.contents["baseline.json"] = []byte("changed\n") },
		func(f *bindingTestFixture) { f.response.Files = append(f.response.Files, f.response.Files[0]) },
		func(f *bindingTestFixture) { f.response.Files[0].Path = "../synthetic-escape" },
		func(f *bindingTestFixture) { f.config.Limits.FileCount = 1 },
		func(f *bindingTestFixture) { f.config.Limits.ArtifactBytes = 1 },
	} {
		f := newBindingTest(t)
		change(f)
		f.sign(t)
		if _, err := f.bind(); err == nil {
			t.Fatal("unbounded or modified artifacts were accepted")
		}
	}
}

// An authenticated response still cannot substitute a different local run or
// pad a candidate-only qualification with another run's containment records.
func TestPrivateBindingRejectsExtraAndMissingContainerEvidence(t *testing.T) {
	for _, path := range []string{"evidence/runs/candidate-02/containers.json", "evidence/runs/baseline-01/containers.json", "evidence/runs/other-01/containers.json", ""} {
		f := newBindingTest(t)
		if path == "" {
			f.response.Files = f.response.Files[:len(f.response.Files)-1]
		} else {
			f.files.contents[path] = []byte("[]\n")
			f.response.Files = append(f.response.Files, bindingTestFile(path, f.files.contents[path]))
		}
		f.sign(t)
		if _, err := f.bind(); err == nil {
			t.Fatal("incorrect local containment inventory accepted", path)
		}
	}
}

func TestPrivateBindingTransportSuccessCannotHideScoreFailure(t *testing.T) {
	f := newBindingTest(t)
	content := []byte(`{"schema":1,"job_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","eval_error":{"code":"synthetic_build_failure"},"score":null}`)
	f.files.contents["worker-result.json"] = content
	for i, file := range f.response.Files {
		if file.Path == "worker-result.json" {
			f.response.Files[i] = bindingTestFile(file.Path, content)
		}
	}
	f.sign(t)
	if _, err := f.bind(); err == nil {
		t.Fatal("authenticated transport failure became containment evidence")
	}
}

// Exercise the actual evaluator projection: worker-result deliberately omits
// native score provenance, which remains authenticated by the completion file.
func TestPrivateBindingAcceptsActualEvaluatorScoreProjection(t *testing.T) {
	f := newBindingTest(t)
	script, err := os.ReadFile(filepath.Join("..", "..", "connect", "sim-latency", "evaluator", "container", "evaluator-subevaluation.sh"))
	if err != nil {
		t.Fatal(err)
	}
	query := regexp.MustCompile(`(?s)score_json="\$\(jq \\\s*'([^']+)' "\$artifact_dir/score.json"\)"`).FindSubmatch(script)
	if len(query) != 2 {
		t.Fatal("actual evaluator score projection unavailable")
	}
	command := exec.CommandContext(t.Context(), "/usr/bin/jq", string(query[1]))
	command.Stdin = bytes.NewReader([]byte(`{"score_schema":1,"placeable":true,"baseline_sha256":"` + f.baseline + `","diagnostics":{}}`))
	projected, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	var score map[string]any
	if err := json.Unmarshal(projected, &score); err != nil {
		t.Fatal(err)
	}
	if _, exists := score["baseline_sha256"]; exists {
		t.Fatal("regression did not exercise omitted baseline projection")
	}
	worker, err := json.Marshal(map[string]any{"schema": 1, "job_id": f.manifest.Identity.JobId, "eval_error": nil, "score": score})
	if err != nil {
		t.Fatal(err)
	}
	f.files.contents["worker-result.json"] = worker
	for i, file := range f.response.Files {
		if file.Path == "worker-result.json" {
			f.response.Files[i] = bindingTestFile(file.Path, worker)
		}
	}
	f.sign(t)
	if _, err := f.bind(); err != nil {
		t.Fatal("authenticated completion baseline rejected after actual projection", err)
	}
}

func TestPrivateBindingRejectsChangedCompletionBaseline(t *testing.T) {
	f := newBindingTest(t)
	var completion map[string]any
	if err := json.Unmarshal(f.files.contents["evaluation.complete.json"], &completion); err != nil {
		t.Fatal(err)
	}
	completion["artifacts"].(map[string]any)["baseline"] = strings.Repeat("e", 64)
	content, err := json.Marshal(completion)
	if err != nil {
		t.Fatal(err)
	}
	f.files.contents["evaluation.complete.json"] = content
	for i, file := range f.response.Files {
		if file.Path == "evaluation.complete.json" {
			f.response.Files[i] = bindingTestFile(file.Path, content)
		}
	}
	f.sign(t)
	if _, err := f.bind(); err == nil {
		t.Fatal("signed completion substituted another baseline")
	}
}

func TestContainmentArtifactReaderRejectsSymlinksAndDirectories(t *testing.T) {
	directory := t.TempDir()
	root, err := os.Open(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(directory, "regular"), []byte("synthetic bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("regular", filepath.Join(directory, "alias")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nested", filepath.Join(directory, "parent-alias")); err != nil {
		t.Fatal(err)
	}
	source := containmentFileSource{root: root}
	if err := source.Verify(bindingTestFile("regular", []byte("synthetic bytes"))); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"alias", "nested", "parent-alias/file", "../regular", "."} {
		if file, err := source.open(path); err == nil {
			_ = file.Close()
			t.Fatal("unsafe artifact path opened", path)
		}
	}
	if _, err := source.Read("regular", 1); err == nil {
		t.Fatal("document bound ignored")
	}
}

// Holding only the final directory would follow an attacker-replaced ancestor.
// These checks run without root and never read live authority or credentials.
func TestContainmentDirectoryRejectsIntermediateSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "nested"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	directory, err := containmentDirectory(filepath.Join(root, "real", "nested"), false)
	if err != nil {
		t.Fatal(err)
	}
	directory.Close()
	for _, path := range []string{filepath.Join(root, "alias", "nested"), filepath.Join(root, "alias"), "relative", root + "/../"} {
		if directory, err := containmentDirectory(path, false); err == nil {
			directory.Close()
			t.Fatal("unsafe containment directory accepted", path)
		}
	}
	if directory, err := containmentDirectory(root, true); err == nil {
		directory.Close()
		t.Fatal("test-owned authority path was accepted through writable ancestors")
	}
}

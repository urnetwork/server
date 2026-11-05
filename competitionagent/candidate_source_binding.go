//go:build linux

// Submission images inherit a pristine workspace. Candidate execution must
// instead mount its authenticated post-patch graph from this run read-only.
// The trusted source preparer separately verifies every actual Git worktree;
// this gate binds that bounded identity to the immutable image before create.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

const candidateSourceLabelPrefix = "com.urnetwork.competition."
const candidateSourceBuildStageLabel = candidateSourceLabelPrefix + "builder-stage"
const candidateSourceCacheTarget = "/opt/urnetwork/go/pkg/mod/cache/download/github.com/urnetwork/server/@v"

var candidateSourceCommitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
var candidateSourceRootPattern = regexp.MustCompile(`^\.evidence-runtime/evaluation-sources\.[0-9A-Za-z]{8}/candidate$`)
var candidateSourceBackingPattern = regexp.MustCompile(`^(postgres|redis):[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}@sha256:[0-9a-f]{64}$`)

// Exactly these two protected Compose references are authority. Another tag,
// digest, image-id alias, registry prefix or family match is not equivalent.
func validateDockerBackingImages(images []string) error {
	if len(images) != 2 {
		return errors.New("Docker backing-image authority must name exactly postgres and redis")
	}
	seen := map[string]bool{}
	for _, image := range images {
		if !candidateSourceBackingPattern.MatchString(image) {
			return errors.New("Docker backing image is not an exact digest-qualified protected reference")
		}
		family, _, _ := strings.Cut(image, ":")
		if seen[family] {
			return errors.New("Docker backing-image authority repeats a service family")
		}
		seen[family] = true
	}
	return nil
}

// This is the finite identity emitted by the pinned source preparer and
// updated by the trusted patch applicator, not a manifest-selected host path.
type candidateSourceIdentity struct {
	Schema               int               `json:"schema"`
	Kind                 string            `json:"kind"`
	Temporary            bool              `json:"temporary"`
	BaseImageId          string            `json:"base_image_id"`
	BaseSha              string            `json:"base_sha"`
	SourceEpoch          int               `json:"source_epoch"`
	Staging              bool              `json:"staging"`
	Branch               string            `json:"branch"`
	SourceLockSha256     string            `json:"source_lock_sha256"`
	Repositories         map[string]string `json:"repositories"`
	CandidatePatchSha256 string            `json:"candidate_patch_sha256"`
	CandidatePathBase    string            `json:"candidate_path_base,omitempty"`
}

// Only already-admitted bind mounts participate in the source contract.
type candidateSourceMount struct {
	source   string
	target   string
	readOnly bool
}

// Called after the common privilege/resource/mount policy, before networking
// and before any daemon mutation. Base probes need no redundant image read.
func (self *DockerProxy) verifyCandidateSourceBinding(parent context.Context, object map[string]json.RawMessage, host dockerPolicyHostConfig) error {
	if err := validateDockerBackingImages(self.plan.BackingImages); err != nil {
		return err
	}
	var config struct {
		Image, User string
		Labels      map[string]string
	}
	if json.Unmarshal(mustDockerPolicyObject(object), &config) != nil || config.Image == "" {
		return errors.New("candidate source binding has no exact image authority")
	}
	stage, hasStage := config.Labels[candidateSourceBuildStageLabel]
	if host.CgroupParent == self.plan.BuildCgroupName {
		if !hasStage || stage != "check" && stage != "compile" || config.Image != self.plan.BaseImageDigest || config.User != "65532:65532" {
			return errors.New("tracked build changed its fixed stage, base image or user")
		}
		return self.verifyTrackedBuildSource(host, stage)
	}
	if hasStage {
		return errors.New("tracked build stage escaped its exact build cgroup")
	}
	mounts, err := candidateSourceBindMounts(host)
	if err != nil {
		return err
	}
	trackedRoot := filepath.Join(self.plan.ContextRoot, ".evidence-runtime", "tracked-build")
	for _, mount := range mounts {
		if mount.target == candidateSourceCacheTarget || mount.target == "/candidate-output" || mount.source == trackedRoot || pathWithin(trackedRoot, mount.source) {
			return errors.New("tracked build cache or output escaped its exact build role")
		}
	}
	if config.Image == self.plan.BaseImageDigest {
		return nil
	}
	backing := false
	for _, image := range self.plan.BackingImages {
		backing = backing || config.Image == image
	}
	immutable := strings.HasPrefix(config.Image, "sha256:") && digestPattern.MatchString(strings.TrimPrefix(config.Image, "sha256:"))
	if !backing && !immutable {
		return errors.New("candidate image must use its full immutable config id")
	}
	labels, imageId, err := self.inspectCandidateSourceImage(parent, config.Image)
	if err != nil {
		return err
	}
	if immutable && imageId != config.Image {
		return errors.New("candidate image inspect resolved another immutable id")
	}
	if labels[candidateSourceLabelPrefix+"image-kind"] != "submission" {
		if backing && config.User == "999:999" && labels[candidateSourceLabelPrefix+"image-kind"] == "" {
			return nil
		}
		return errors.New("non-base image is neither a submission nor an exact protected backing image")
	}
	if !immutable || labels[candidateSourceLabelPrefix+"source-materialization"] != "required-readonly-run-source/v1" ||
		labels[candidateSourceLabelPrefix+"builtin-source-kind"] != "pristine-evaluator-base" {
		return errors.New("submission image changed its mandatory source-materialization class")
	}
	key := labels[candidateSourceLabelPrefix+"image-key"]
	if !digestPattern.MatchString(key) || self.plan.BuildCgroupName != "urnetwork-submission-build-"+key[:32]+".slice" ||
		!digestPattern.MatchString(labels[candidateSourceLabelPrefix+"candidate-source-identity-sha256"]) ||
		!candidateSourceCommitPattern.MatchString(labels[candidateSourceLabelPrefix+"base-sha"]) ||
		!candidateSourceCommitPattern.MatchString(labels["org.opencontainers.image.revision"]) ||
		!digestPattern.MatchString(labels[candidateSourceLabelPrefix+"patch-sha256"]) {
		return errors.New("submission image source labels differ from its committed build identity")
	}
	source, err := self.singleCandidateSourceMount(mounts)
	if err != nil {
		return err
	}
	identity, digest, err := self.readCandidateSourceIdentity(source)
	if err != nil {
		return err
	}
	if digest != labels[candidateSourceLabelPrefix+"candidate-source-identity-sha256"] || identity.BaseSha != labels[candidateSourceLabelPrefix+"base-sha"] ||
		identity.CandidatePatchSha256 != labels[candidateSourceLabelPrefix+"patch-sha256"] || identity.Repositories["server"] != labels["org.opencontainers.image.revision"] {
		return errors.New("mounted candidate graph does not match the immutable submission image")
	}
	return nil
}

// Inspection uses only the installed daemon transport, fixed GET grammar,
// bounded json and the smaller of the operation and original run deadlines.
func (self *DockerProxy) inspectCandidateSourceImage(parent context.Context, image string) (map[string]string, string, error) {
	if self.settings.MaximumDocumentBytes <= 0 || self.settings.MaximumDocumentBytes == math.MaxInt64 || self.settings.OperationTimeout <= 0 ||
		!self.settings.Now().Before(self.plan.DeadlineAt) {
		return nil, "", errors.New("candidate image inspection has no remaining bounded run lifetime")
	}
	ctx, cancel := context.WithTimeout(parent, self.settings.OperationTimeout)
	defer cancel()
	ctx, cancelDeadline := context.WithDeadline(ctx, self.plan.DeadlineAt)
	defer cancelDeadline()
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/images/"+url.PathEscape(image)+"/json", nil)
	if err != nil {
		return nil, "", err
	}
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > self.settings.MaximumDocumentBytes ||
		validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
		return nil, "", errors.New("candidate image inspection is unavailable, malformed or oversized")
	}
	var object, config map[string]json.RawMessage
	var imageId string
	if json.Unmarshal(content, &object) != nil || json.Unmarshal(object["Id"], &imageId) != nil ||
		json.Unmarshal(object["Config"], &config) != nil || config == nil || !strings.HasPrefix(imageId, "sha256:") ||
		!digestPattern.MatchString(strings.TrimPrefix(imageId, "sha256:")) {
		return nil, "", errors.New("candidate image inspection omitted its exact id or config")
	}
	var labels map[string]string
	if raw := config["Labels"]; len(raw) != 0 && json.Unmarshal(raw, &labels) != nil {
		return nil, "", errors.New("candidate image inspection has malformed image labels")
	}
	return labels, imageId, nil
}

// Legacy and structured Docker clients share one source contract. The common
// create gate has already rejected capabilities and duplicate mount targets.
func candidateSourceBindMounts(host dockerPolicyHostConfig) ([]candidateSourceMount, error) {
	var mounts []candidateSourceMount
	for _, bind := range host.Binds {
		parts := strings.Split(bind, ":")
		if len(parts) != 3 {
			return nil, errors.New("candidate source legacy bind is malformed")
		}
		readOnly, found := false, false
		for _, option := range strings.Split(parts[2], ",") {
			switch option {
			case "ro", "rw":
				if found {
					return nil, errors.New("candidate source bind repeated its access mode")
				}
				readOnly, found = option == "ro", true
			case "rprivate":
			default:
				return nil, errors.New("candidate source bind has an unreviewed option")
			}
		}
		if !found {
			return nil, errors.New("candidate source bind omitted its access mode")
		}
		mounts = append(mounts, candidateSourceMount{source: parts[0], target: parts[1], readOnly: readOnly})
	}
	for _, content := range host.Mounts {
		var mount struct {
			Type, Source, Target string
			ReadOnly             bool
		}
		if json.Unmarshal(content, &mount) != nil {
			return nil, errors.New("candidate source structured mount is malformed")
		}
		if mount.Type == "bind" {
			mounts = append(mounts, candidateSourceMount{source: mount.Source, target: mount.Target, readOnly: mount.ReadOnly})
		}
	}
	return mounts, nil
}

// Only one exact candidate leaf beneath this run can replace /workspace.
// Another run, the baseline graph, or a parent/child alias is not a substitute.
func (self *DockerProxy) singleCandidateSourceMount(mounts []candidateSourceMount) (string, error) {
	source := ""
	for _, mount := range mounts {
		if mount.target != "/workspace" {
			continue
		}
		if source != "" || !mount.readOnly || !filepath.IsAbs(mount.source) || filepath.Clean(mount.source) != mount.source || strings.ContainsAny(mount.source, "\x00\r\n") {
			return "", errors.New("candidate workspace source is repeated, writable or aliased")
		}
		relative, err := filepath.Rel(self.plan.ContextRoot, mount.source)
		if err != nil || !candidateSourceRootPattern.MatchString(filepath.ToSlash(relative)) {
			return "", errors.New("candidate workspace source is outside this run's exact source graph")
		}
		source = mount.source
	}
	if source == "" {
		return "", errors.New("submission image omitted its mandatory read-only candidate workspace")
	}
	return source, nil
}

// Native opens never use os.Root's confined-symlink retry. The held source
// directory and regular identity generation are rechecked after the bounded
// read. This does not claim pathname custody against trusted host-root changes.
func (self *DockerProxy) readCandidateSourceIdentity(source string) (*candidateSourceIdentity, string, error) {
	maximum := self.settings.MaximumDocumentBytes
	relative, pathErr := filepath.Rel(self.plan.ContextRoot, source)
	if maximum <= 0 || maximum == math.MaxInt64 || pathErr != nil || !filepath.IsAbs(source) || filepath.Clean(source) != source ||
		!candidateSourceRootPattern.MatchString(filepath.ToSlash(relative)) {
		return nil, "", errors.New("candidate source identity path or byte bound is invalid")
	}
	root, err := openDirectoryNoFollow("/")
	if err != nil {
		return nil, "", err
	}
	defer root.Close()
	contextDirectory, err := openRelativeNoFollow(root, strings.TrimPrefix(self.plan.ContextRoot, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer contextDirectory.Close()
	contextInfo, err := contextDirectory.Stat()
	if err != nil || !contextInfo.IsDir() || contextInfo.Mode().Perm() != 0700 {
		return nil, "", errors.New("candidate source run root is not private")
	}
	parentDirectory, err := openRelativeNoFollow(contextDirectory, filepath.ToSlash(filepath.Dir(relative)), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer parentDirectory.Close()
	parentInfo, err := parentDirectory.Stat()
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode().Perm() != 0700 {
		return nil, "", errors.New("candidate source temporary outer parent is not private")
	}
	directory, err := openRelativeNoFollow(parentDirectory, "candidate", os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer directory.Close()
	directoryInfo, err := directory.Stat()
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0700 && directoryInfo.Mode().Perm() != 0555 {
		return nil, "", errors.New("candidate source graph is neither private builder input nor sealed measurement input")
	}
	directoryStat, ok := directoryInfo.Sys().(*syscall.Stat_t)
	if !ok || self.protected && directoryStat.Uid != 0 && directoryStat.Uid != self.plan.WorkerUid {
		return nil, "", errors.New("candidate source graph belongs to another host principal")
	}
	file, err := openRelativeNoFollow(directory, ".evaluation-source.json", os.O_RDONLY, 0)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximum || info.Mode().Perm() != 0400 && info.Mode().Perm() != 0444 {
		return nil, "", errors.New("candidate source identity is not a sealed bounded regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 || self.protected && stat.Uid != 0 && stat.Uid != self.plan.WorkerUid {
		return nil, "", errors.New("candidate source identity has ambiguous ownership or hard links")
	}
	content, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(content)) > maximum || validateDocumentKeys(content) != nil || validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
		return nil, "", errors.New("candidate source identity is malformed or oversized")
	}
	currentDirectory, err := openRelativeNoFollow(root, strings.TrimPrefix(source, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer currentDirectory.Close()
	currentInfo, err := currentDirectory.Stat()
	if err != nil || !os.SameFile(directoryInfo, currentInfo) || currentInfo.Mode() != directoryInfo.Mode() {
		return nil, "", errors.New("candidate source directory generation changed")
	}
	currentContext, err := openRelativeNoFollow(root, strings.TrimPrefix(self.plan.ContextRoot, "/"), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer currentContext.Close()
	currentContextInfo, err := currentContext.Stat()
	if err != nil || !os.SameFile(contextInfo, currentContextInfo) || currentContextInfo.Mode().Perm() != 0700 {
		return nil, "", errors.New("candidate source run-root custody changed")
	}
	currentParent, err := openRelativeNoFollow(currentContext, filepath.ToSlash(filepath.Dir(relative)), os.O_RDONLY|syscall.O_DIRECTORY, 0)
	if err != nil {
		return nil, "", err
	}
	defer currentParent.Close()
	currentParentInfo, err := currentParent.Stat()
	if err != nil || !os.SameFile(parentInfo, currentParentInfo) || currentParentInfo.Mode().Perm() != 0700 {
		return nil, "", errors.New("candidate source outer-parent custody changed")
	}
	currentFile, err := openRelativeNoFollow(currentDirectory, ".evaluation-source.json", os.O_RDONLY, 0)
	if err != nil {
		return nil, "", err
	}
	defer currentFile.Close()
	currentFileInfo, err := currentFile.Stat()
	if err != nil || !os.SameFile(info, currentFileInfo) || info.Size() != currentFileInfo.Size() || !info.ModTime().Equal(currentFileInfo.ModTime()) {
		return nil, "", errors.New("candidate source identity generation changed")
	}
	currentStat, ok := currentFileInfo.Sys().(*syscall.Stat_t)
	if !ok || currentStat.Ctim != stat.Ctim || currentStat.Nlink != 1 || currentFileInfo.Mode() != info.Mode() {
		return nil, "", errors.New("candidate source identity custody changed")
	}
	identity := &candidateSourceIdentity{}
	if decodeStrict(content, identity) != nil || identity.Schema != 1 || identity.Kind != "sim-latency-evaluation-source" || !identity.Temporary ||
		identity.BaseImageId != self.plan.BaseImageDigest || !candidateSourceCommitPattern.MatchString(identity.BaseSha) ||
		!digestPattern.MatchString(identity.CandidatePatchSha256) || !digestPattern.MatchString(identity.SourceLockSha256) ||
		identity.SourceEpoch < 0 || identity.SourceEpoch > 6 || !identity.Staging && identity.Branch != "sim-latency" || identity.Staging && identity.Branch != "sim-latency-staging" ||
		identity.CandidatePathBase != "" && identity.CandidatePathBase != "mono_root_v1" {
		return nil, "", errors.New("candidate source identity is not the exact temporary post-patch class")
	}
	repositories := strings.Fields("server connect sdk proxy glog goidenticons userwireguard sn warp gvisor")
	if len(identity.Repositories) != len(repositories) {
		return nil, "", errors.New("candidate source identity does not contain exactly ten product repositories")
	}
	for _, repository := range repositories {
		if !candidateSourceCommitPattern.MatchString(identity.Repositories[repository]) {
			return nil, "", errors.New("candidate source identity omitted a canonical product commit")
		}
	}
	return identity, digestDockerBytes(content), nil
}

// Check and compile use disjoint mutable download caches. Only compile can
// write the fixed output leaf; neither stage can acquire another host bind.
func (self *DockerProxy) verifyTrackedBuildSource(host dockerPolicyHostConfig, stage string) error {
	mounts, err := candidateSourceBindMounts(host)
	if err != nil {
		return err
	}
	source, err := self.singleCandidateSourceMount(mounts)
	if err != nil {
		return err
	}
	if _, _, err := self.readCandidateSourceIdentity(source); err != nil {
		return err
	}
	cache := filepath.Join(self.plan.ContextRoot, ".evidence-runtime", "tracked-build", stage+"-cache")
	output := filepath.Join(self.plan.ContextRoot, ".evidence-runtime", "tracked-build", "output")
	cacheFound, outputFound := false, false
	for _, mount := range mounts {
		switch mount.target {
		case "/workspace":
		case candidateSourceCacheTarget:
			if cacheFound || mount.readOnly || mount.source != cache {
				return errors.New("tracked build changed its exact stage-local mutable cache")
			}
			cacheFound = true
		case "/candidate-output":
			if stage != "compile" || outputFound || mount.readOnly || mount.source != output {
				return errors.New("tracked build changed its compile-only mutable output")
			}
			outputFound = true
		default:
			return errors.New("tracked build requested another host bind")
		}
	}
	if !cacheFound || outputFound != (stage == "compile") {
		return errors.New("tracked build omitted its exact cache or compile output")
	}
	return nil
}

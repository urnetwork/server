// New per-call containment has a distinct entrypoint, claim namespace, artifact
// census and size profile. Historical flat receipts cannot qualify this profile.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
)

// Compare fixed original process journals with the outer captured bytes and the
// independently reread durable root claim. A failed original remains evidence,
// never a new admission, successful verdict or retry permission.
func validateWinnerMultipassJournals(artifacts map[string][]byte, pin currentQgQualificationPin, owner, attempt string, settings currentQgQualificationSettings, envelope winnerMultipassBrokerEnvelope, result winnerMultipassBrokerResult) error {
	var provider winnerSnowProviderSettings
	if decodeStrictJSONBytes(artifacts["provider-settings.json"], &provider, "multi-pass captured provider") != nil || validateWinnerSnowProviderSettings(provider) != nil || currentQgQualificationHash(artifacts["provider-settings.json"]) != pin.Sha256 {
		return errors.New("multi-pass journals have different provider pins")
	}
	if result.Schema != 1 || result.Kind != "winner-review-multipass-call-result/v1" || result.Operation != envelope.Operation || result.CallId != envelope.Call.Id || result.RequestSha256 != winnerMultipassSha256(envelope.Payload) || result.StartedAt.IsZero() || result.CompletedAt.Before(result.StartedAt) {
		return errors.New("multi-pass journal result belongs to a different call or clock")
	}
	if err := validateWinnerMultipassBrokerEnvelope(envelope, provider, result.StartedAt, defaultWinnerMultipassProviderLimits()); err != nil {
		return err
	}
	canonical, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	if !bytes.Equal(canonical, artifacts["call.started.json"]) || !bytes.Equal(bytes.TrimSpace(artifacts["broker.stdout"]), artifacts["call.complete.json"]) {
		return errors.New("multi-pass original process journals differ from wrapper input/output")
	}
	leaves := map[string][]byte{"call.started.json": artifacts["call.started.json"], "call.complete.json": artifacts["call.complete.json"]}
	names := map[string]string{"tokenize.request.json": result.TokenizerRequestArtifact, "tokenize.response.json": result.TokenizerResponseArtifact, "transport.response": result.TransportResponseArtifact}
	for required, actual := range names {
		if actual != "" {
			if actual != required || len(artifacts[actual]) == 0 {
				return errors.New("multi-pass result relabeled or omitted an original transport leaf")
			}
			leaves[actual] = artifacts[actual]
		}
	}
	state := filepath.Join(attempt, "member-state")
	if err := validateCurrentQgSnowQualificationStateCensus(state, len(leaves)); err != nil {
		return err
	}
	mapped, err := currentQgQualificationMappedUid(artifacts["uid-map.txt"], 65532)
	if err != nil {
		return err
	}
	info, err := os.Lstat(state)
	if err != nil || info.Sys().(*syscall.Stat_t).Uid != uint32(mapped) || info.Sys().(*syscall.Stat_t).Gid != 0 {
		return errors.New("multi-pass state directory changed its mapped owner")
	}
	for name, content := range leaves {
		path := filepath.Join(state, name)
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(mapped) || info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return errors.New("multi-pass process journal changed owner, mode or file type")
		}
		if _, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: path, Sha256: currentQgQualificationHash(content)}, winnerMultipassCapturedLeafLimit(name, envelope.Settings), false); err != nil {
			return err
		}
	}
	if envelope.Operation == "generate" {
		if result.TokenizerRequestArtifact != "" || result.TokenizerResponseArtifact != "" || !reflect.DeepEqual(result.Admission, envelope.Admission) {
			return errors.New("generation changed or substituted original tokenizer admission")
		}
		var claim struct {
			Schema            int    `json:"schema"`
			Kind              string `json:"kind"`
			IdempotencySha256 string `json:"idempotency_key_sha256"`
			InputSha256       string `json:"input_sha256"`
			SettingsSha256    string `json:"provider_settings_sha256"`
			ModelsSha256      string `json:"model_set_sha256"`
			Owner             string `json:"owner"`
			Attempt           string `json:"attempt_directory"`
			AutomaticRetries  bool   `json:"automatic_retries"`
		}
		if err := decodeCurrentQgSnowQualificationProof(artifacts["attempt-claim.json"], &claim); err != nil {
			return err
		}
		members := []map[string]string{}
		for _, member := range provider.Members {
			members = append(members, map[string]string{"name": member.Name, "model": member.Model, "revision": member.Revision, "reasoning": member.Reasoning})
		}
		memberBytes, _ := json.Marshal(members)
		key := winnerMultipassSha256([]byte(envelope.Owner.Id + "/" + envelope.Call.Id))
		if claim.Schema != 1 || claim.Kind != "snow-multipass-single-call-claim" || claim.IdempotencySha256 != key || claim.InputSha256 != winnerMultipassSha256(artifacts["input.json"]) || claim.SettingsSha256 != pin.Sha256 || claim.ModelsSha256 != winnerMultipassSha256(memberBytes) || claim.Owner != owner || claim.Attempt != attempt || claim.AutomaticRetries {
			return errors.New("generation claim is not the original one-shot owner/call input")
		}
		directory := filepath.Join(filepath.Dir(settings.StateRoot), "generation-claims")
		if err := currentQgQualificationPrivateRoot(directory); err != nil {
			return err
		}
		if _, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(directory, key+".json"), Sha256: winnerMultipassSha256(artifacts["attempt-claim.json"])}, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true); err != nil {
			return err
		}
		if result.TransportResponseArtifact != "" && result.TransportResponseSha256 != winnerMultipassSha256(artifacts["transport.response"]) {
			return errors.New("generation original transport digest changed")
		}
	} else if envelope.Operation == "tokenize" {
		if result.TransportResponseArtifact != "" {
			return errors.New("tokenizer invocation contains generation transport")
		}
		if result.TokenizerRequestArtifact != "" {
			expected, err := winnerMultipassTokenizerPayload(envelope)
			if err != nil {
				return err
			}
			if !bytes.Equal(expected, artifacts["tokenize.request.json"]) {
				return errors.New("tokenizer original request does not render the exact full prompt")
			}
		}
		if result.TokenizerResponseArtifact != "" && result.TransportResponseSha256 != winnerMultipassSha256(artifacts["tokenize.response.json"]) {
			return errors.New("tokenizer original response digest changed")
		}
	} else {
		return errors.New("unplanned journal operation")
	}
	if result.Error != "" {
		return nil
	}
	if !result.CompletedAt.Before(envelope.Owner.Deadline) || !result.CompletedAt.Before(envelope.CallDeadline) || result.TransportStatus != 200 {
		return errors.New("successful multi-pass result is late or lacks a successful original HTTP terminal")
	}
	if envelope.Operation == "tokenize" {
		if result.Admission == nil || len(leaves) != 4 {
			return errors.New("successful tokenization lacks its complete original census")
		}
		var decoded struct {
			Count           int      `json:"count"`
			MaximumModelLen int      `json:"max_model_len"`
			Tokens          []int64  `json:"tokens"`
			TokenStrings    []string `json:"token_strs"`
		}
		if err := decodeStrictJSONBytes(artifacts["tokenize.response.json"], &decoded, "retained original tokenizer response"); err != nil {
			return err
		}
		tokens, _ := json.Marshal(decoded.Tokens)
		admission := result.Admission
		if decoded.Count != len(decoded.Tokens) || decoded.Count != admission.InputTokens || decoded.MaximumModelLen != admission.MaximumContextTokens || slices.ContainsFunc(decoded.Tokens, func(token int64) bool { return token < 0 }) || admission.RenderedPromptSha256 != winnerMultipassSha256(tokens) || admission.TokenizerRequestSha256 != winnerMultipassSha256(artifacts["tokenize.request.json"]) || admission.TokenizerResponseSha256 != winnerMultipassSha256(artifacts["tokenize.response.json"]) {
			return errors.New("retained tokenizer proof does not reproduce original admission")
		}
		return validateWinnerMultipassTokenization(&winnerMultipassPlan{Settings: envelope.Settings}, envelope.Call, envelope.Payload, *admission)
	}
	if len(leaves) != 3 || result.TransportResponseArtifact != "transport.response" {
		return errors.New("successful generation lacks original visible transport")
	}
	raw := artifacts["transport.response"]
	modelSettings := defaultWinnerSnowModelSettings()
	if err := winnerMultipassVisibleTransport(raw, result.TransportContentType, modelSettings.MaximumEventBytes); err != nil {
		return err
	}
	var terminal []byte
	if strings.HasPrefix(strings.ToLower(result.TransportContentType), "application/json") {
		terminal = raw
		err = validateWinnerModelResponseIdentity(raw, envelope.Call.Member.Model, "json_response", modelSettings)
	} else {
		terminal, err = readPinnedModelResponse(bytes.NewReader(raw), modelSettings, envelope.Call.Member.Model)
	}
	if err != nil {
		return err
	}
	if !bytes.Equal(terminal, result.VisibleResponse) {
		return errors.New("visible terminal is not the exact retained provider transport terminal")
	}
	_, err = winnerMultipassVisibleResponse(winnerMultipassCallEvidence{CallId: result.CallId, Admission: *result.Admission, VisibleResponse: result.VisibleResponse, VisibleResponseSha256: result.VisibleResponseSha256,
		TransportResponseSha256: result.TransportResponseSha256, TransportReceiptSha256: winnerMultipassSha256(artifacts["terminal.json"]), ProviderOutput: result.ProviderOutput, Observation: result.Observation}, envelope.Call, envelope.Settings)
	return err
}

// Exact bounded artifact names are shared with protected custody. Unknown names
// are rejected rather than silently omitted from an otherwise valid manifest.
func winnerMultipassCapturedLeafLimit(name string, settings winnerMultipassSettings) int64 {
	limits := defaultWinnerMultipassProviderLimits()
	switch name {
	case "input.json", "call.started.json":
		return int64(limits.MaximumEnvelopeBytes)
	case "broker.stdout", "call.complete.json":
		return int64(limits.MaximumResultBytes)
	case "tokenize.request.json":
		return int64(settings.MaximumRequestBytes)
	case "tokenize.response.json":
		return int64(limits.MaximumTokenizerBytes)
	case "transport.response":
		return int64(settings.MaximumResponseBytes)
	case "broker.stderr":
		return 64 * 1024
	case "image-inspect.json", "network-inspect.json", "container-inspect.json", "container-state.json", "cgroup.json", "uid-map.txt", "provider-settings.json", "provider-settings-ownership.json", "member-state-ownership.json", "attempt-claim.json", "provider-address.txt", "fence.json", "fence-forward.txt", "fence-input.txt", "fence-chain.txt", "terminal.json", "evidence-manifest.json":
		return int64(limits.MaximumMetadataBytes)
	default:
		return 0
	}
}

// Actual root-owned observations are authenticated before descriptors become
// custody input. A native release gate still separately proves image/wrapper pins.
func authenticateWinnerMultipassContainment(input, output, diagnostics []byte, canceled bool, profile winnerMultipassContainmentProfile) (*winnerMultipassContainmentReceipt, error) {
	if !strings.HasPrefix(profile.ImageId, "sha256:") || !validSha256(strings.TrimPrefix(profile.ImageId, "sha256:")) || !validSha256(profile.ProviderSettings.Sha256) || !filepath.IsAbs(profile.StateRoot) || filepath.Clean(profile.StateRoot) != profile.StateRoot {
		return nil, errors.New("multi-pass containment profile is not pinned")
	}
	settings := defaultCurrentQgQualificationSettings()
	settings.StateRoot = profile.StateRoot
	digest, err := authenticateWinnerMultipassContainmentInternal(input, output, diagnostics, canceled, profile.ImageId, settings, currentQgSnowQualificationProfile(profile.ProviderSettings))
	if err != nil {
		return nil, err
	}
	lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
	var receipt currentQgQualificationWrapperReceipt
	if err := decodeStrictJSONBytes(lines[len(lines)-1], &receipt, "verified multi-pass wrapper receipt"); err != nil {
		return nil, err
	}
	manifestBytes, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(receipt.AttemptDirectory, "evidence-manifest.json"), Sha256: receipt.ManifestSha256}, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	var manifest struct {
		Schema    int                             `json:"schema"`
		Kind      string                          `json:"kind"`
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if err := decodeStrictJSONBytes(manifestBytes, &manifest, "verified multi-pass manifest"); err != nil {
		return nil, err
	}
	result := &winnerMultipassContainmentReceipt{AttemptDirectory: receipt.AttemptDirectory, ManifestSha256: receipt.ManifestSha256, WrapperReceiptSha256: digest, WrapperReceipt: append([]byte{}, lines[len(lines)-1]...), Artifacts: manifest.Artifacts, Canceled: canceled}
	result.Artifacts = append(result.Artifacts, currentQgQualificationBinding{Path: "evidence-manifest.json", Sha256: receipt.ManifestSha256, Bytes: int64(len(manifestBytes))})
	if !canceled {
		result.Result = &winnerMultipassBrokerResult{}
		if err := decodeStrictJSONBytes(output, result.Result, "verified multi-pass result"); err != nil {
			return nil, err
		}
	}
	return result, nil
}

// Independent actual-container verification for the new profile only.
func authenticateWinnerMultipassContainmentInternal(input, output, diagnostics []byte, canceled bool, image string, settings currentQgQualificationSettings, profile currentQgQualificationProviderProfile) (string, error) {
	if !profile.Snow {
		return "", errors.New("multi-pass containment has no historical key profile")
	}
	var envelope winnerMultipassBrokerEnvelope
	if rejectWinnerReviewCaseAliases(input) != nil || decodeStrictJSONBytes(input, &envelope, "multi-pass original envelope") != nil || validateWinnerMultipassSettings(envelope.Settings) != nil {
		return "", errors.New("multi-pass original envelope is malformed")
	}
	if canceled != (envelope.Operation == "qualification_cancel_before_provider") || canceled && len(output) != 0 {
		return "", errors.New("multi-pass cancellation is not the exact pre-input empty-output control")
	}
	if err := currentQgQualificationPrivateRoot(settings.StateRoot); err != nil {
		return "", err
	}
	var result winnerMultipassBrokerResult
	if !canceled && (rejectWinnerReviewCaseAliases(output) != nil || decodeStrictJSONBytes(output, &result, "multi-pass original result") != nil) {
		return "", errors.New("multi-pass original result is malformed")
	}
	lines := bytes.Split(bytes.TrimSpace(diagnostics), []byte("\n"))
	if len(lines) == 0 {
		return "", errors.New("wrapper containment receipt missing")
	}
	var receipt currentQgQualificationWrapperReceipt
	if err := decodeStrictJSONBytes(lines[len(lines)-1], &receipt, "actual wrapper receipt"); err != nil {
		return "", err
	}
	if receipt.Schema != 1 || receipt.Kind != "winner-review-container-receipt" || !validSha256(receipt.Owner) || !receipt.CleanupVerified || filepath.Dir(receipt.AttemptDirectory) != settings.StateRoot || !strings.HasPrefix(receipt.AttemptDirectory, settings.StateRoot+"/invocation.") {
		return "", errors.New("actual wrapper owner/cleanup receipt invalid")
	}
	if err := currentQgQualificationPrivateRoot(receipt.AttemptDirectory); err != nil {
		return "", err
	}
	manifestBytes, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(receipt.AttemptDirectory, "evidence-manifest.json"), Sha256: receipt.ManifestSha256}, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return "", err
	}
	var manifest struct {
		Schema    int                             `json:"schema"`
		Kind      string                          `json:"kind"`
		Artifacts []currentQgQualificationBinding `json:"artifacts"`
	}
	if err := decodeStrictJSONBytes(manifestBytes, &manifest, "actual broker evidence manifest"); err != nil {
		return "", err
	}
	if manifest.Schema != 1 || manifest.Kind != "winner-review-container-evidence" || len(manifest.Artifacts) > 26 {
		return "", errors.New("multi-pass actual artifact manifest is malformed")
	}
	artifacts := map[string][]byte{}
	for _, binding := range manifest.Artifacts {
		if binding.Path == "" || filepath.Base(binding.Path) != binding.Path || artifacts[binding.Path] != nil || binding.Bytes < 0 || winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings) <= 0 || binding.Bytes > winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings) {
			return "", errors.New("actual wrapper evidence binding invalid")
		}
		content, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(receipt.AttemptDirectory, binding.Path), Sha256: binding.Sha256}, winnerMultipassCapturedLeafLimit(binding.Path, envelope.Settings), true)
		if err != nil || int64(len(content)) != binding.Bytes {
			return "", errors.New("actual wrapper evidence bytes changed")
		}
		artifacts[binding.Path] = content
	}
	requiredArtifacts := []string{"input.json", "image-inspect.json", "network-inspect.json", "container-inspect.json", "cgroup.json", "uid-map.txt", "provider-address.txt", "fence.json", "fence-forward.txt", "fence-input.txt", "fence-chain.txt", "broker.stdout", "broker.stderr", "terminal.json", "provider-settings.json", "provider-settings-ownership.json", "member-state-ownership.json"}
	if !canceled {
		requiredArtifacts = append(requiredArtifacts, "container-state.json", "call.started.json", "call.complete.json")
		if envelope.Operation == "generate" {
			requiredArtifacts = append(requiredArtifacts, "attempt-claim.json")
		}
		for _, name := range []string{result.TokenizerRequestArtifact, result.TokenizerResponseArtifact, result.TransportResponseArtifact} {
			if name != "" {
				requiredArtifacts = append(requiredArtifacts, name)
			}
		}
	}
	if len(manifest.Artifacts) != len(requiredArtifacts) {
		return "", errors.New("multi-pass actual wrapper artifact census changed")
	}
	for _, required := range requiredArtifacts {
		if _, present := artifacts[required]; !present {
			return "", errors.New("actual wrapper omitted required containment evidence")
		}
	}
	if !bytes.Equal(artifacts["input.json"], input) || !bytes.Equal(artifacts["broker.stdout"], output) {
		return "", errors.New("wrapper containment receipt belongs to different request/output bytes")
	}
	var terminal struct {
		Schema                int    `json:"schema"`
		Kind                  string `json:"kind"`
		Owner                 string `json:"owner"`
		ImageId               string `json:"image_id"`
		ContainerId           string `json:"container_id"`
		NetworkId             string `json:"network_id"`
		WrapperExit           int    `json:"wrapper_exit"`
		BrokerExit            *int   `json:"broker_exit"`
		BrokerExitObserved    bool   `json:"broker_exit_observed"`
		CleanupVerified       bool   `json:"cleanup_verified"`
		ProviderInputReleased bool   `json:"provider_input_released"`
	}
	if err := decodeStrictJSONBytes(artifacts["terminal.json"], &terminal, "actual review terminal"); err != nil {
		return "", err
	}
	if terminal.Schema != 1 || terminal.Kind != "winner-review-container-terminal" || terminal.Owner != receipt.Owner || terminal.ImageId != image || !validSha256(terminal.ContainerId) || !validSha256(terminal.NetworkId) || terminal.WrapperExit != receipt.WrapperExit || !terminal.CleanupVerified || terminal.ProviderInputReleased == canceled {
		return "", errors.New("actual review terminal identity/cancellation invalid")
	}
	if canceled && (terminal.BrokerExit != nil || terminal.BrokerExitObserved || receipt.WrapperExit != 143) {
		return "", errors.New("actual cancellation did not preserve its nonzero terminal status")
	}
	var imageInspections []struct{ Id string }
	if json.Unmarshal(artifacts["image-inspect.json"], &imageInspections) != nil || len(imageInspections) != 1 || imageInspections[0].Id != image {
		return "", errors.New("actual broker image inspection differs from qualified image")
	}
	var containerIdentities []struct{ Id string }
	if json.Unmarshal(artifacts["container-inspect.json"], &containerIdentities) != nil || len(containerIdentities) != 1 || containerIdentities[0].Id != terminal.ContainerId {
		return "", errors.New("actual broker container receipt substituted another child")
	}
	if !canceled {
		if (result.Error == "") != (receipt.WrapperExit == 0) {
			return "", errors.New("multi-pass result and actual wrapper status disagree")
		}
		var state struct {
			Status    string
			Running   bool
			ExitCode  int
			OomKilled bool `json:"OOMKilled"`
		}
		if json.Unmarshal(artifacts["container-state.json"], &state) != nil || state.Status != "exited" || state.Running || state.OomKilled || !terminal.BrokerExitObserved || terminal.BrokerExit == nil || state.ExitCode != *terminal.BrokerExit {
			return "", errors.New("actual broker terminal process state invalid")
		}
	}
	var networks []struct {
		Id, Driver, Scope                string
		Internal, EnableIPv6, Attachable bool
		Labels, Options                  map[string]string
		IPAM                             struct{ Config []struct{ Subnet string } }
	}
	if json.Unmarshal(artifacts["network-inspect.json"], &networks) != nil || len(networks) != 1 {
		return "", errors.New("actual review network inspection invalid")
	}
	network := networks[0]
	if network.Id != terminal.NetworkId || network.Driver != "bridge" || network.Scope != "local" || network.Internal || network.EnableIPv6 || network.Attachable || network.Labels["com.urnetwork.review.owner"] != receipt.Owner || network.Options["com.docker.network.bridge.enable_icc"] != "false" || len(network.IPAM.Config) != 1 {
		return "", errors.New("actual review network escaped its owner/isolation plan")
	}
	subnet := network.IPAM.Config[0].Subnet
	address, _, err := net.ParseCIDR(subnet)
	if err != nil || !address.IsPrivate() {
		return "", errors.New("actual review network subnet invalid")
	}
	var fence struct {
		Schema               int    `json:"schema"`
		Kind                 string `json:"kind"`
		Owner, Chain         string
		SourceSubnet         string `json:"source_subnet"`
		ProviderHost         string `json:"provider_host"`
		ProviderIp           string `json:"provider_ipv4"`
		ProviderPort         int    `json:"provider_port"`
		ExternalDns          bool   `json:"external_dns"`
		HostInputDenied      bool   `json:"host_input_denied"`
		DefaultDeny          bool   `json:"default_deny"`
		InstalledBeforeStart bool   `json:"installed_before_start"`
	}
	if json.Unmarshal(artifacts["fence.json"], &fence) != nil || fence.Schema != 1 || fence.Kind != "winner-review-provider-egress-fence" || fence.Owner != receipt.Owner || fence.Chain != "URRV_"+receipt.Owner[:16] || fence.SourceSubnet != subnet || fence.ProviderHost != profile.ProviderHost || fence.ProviderPort != profile.ProviderPort || fence.ExternalDns || !fence.HostInputDenied || !fence.DefaultDeny || !fence.InstalledBeforeStart {
		return "", errors.New("actual provider fence metadata invalid")
	}
	provider := net.ParseIP(fence.ProviderIp)
	if provider == nil || provider.To4() == nil || (!profile.Snow && (!provider.IsGlobalUnicast() || provider.IsPrivate())) || (profile.Snow && fence.ProviderIp != profile.ProviderIp) || strings.TrimSpace(string(artifacts["provider-address.txt"])) != fence.ProviderIp {
		return "", errors.New("actual provider address invalid")
	}
	if err := validateCurrentQgQualificationFenceForProvider(artifacts["fence-chain.txt"], artifacts["fence-forward.txt"], artifacts["fence-input.txt"], receipt.Owner, subnet, fence.ProviderIp, profile.ProviderPort); err != nil {
		return "", err
	}
	profile.ProviderIp = fence.ProviderIp
	if profile.Snow {
		profile.StateDirectory = filepath.Join(receipt.AttemptDirectory, "member-state")
	}
	if err := validateWinnerMultipassContainer(artifacts["container-inspect.json"], receipt.Owner, image, terminal.NetworkId, profile); err != nil {
		return "", err
	}
	var cgroup struct {
		Schema      int `json:"schema"`
		Pid         int `json:"container_pid"`
		Kind, Path  string
		MemoryMax   string `json:"memory_max"`
		SwapMax     string `json:"memory_swap_max"`
		PidsMax     string `json:"pids_max"`
		CpuMax      string `json:"cpu_max"`
		CpuSet      string `json:"cpuset_cpus"`
		BeforeInput bool   `json:"observed_before_provider_input"`
	}
	if json.Unmarshal(artifacts["cgroup.json"], &cgroup) != nil || cgroup.Schema != 1 || cgroup.Pid < 1 || cgroup.Kind != "winner-review-running-cgroup" || !strings.HasPrefix(cgroup.Path, "/") || cgroup.MemoryMax != "2147483648" || cgroup.SwapMax != "0" || cgroup.PidsMax != "64" || cgroup.CpuMax != "100000 100000" || cgroup.CpuSet != "20,22" || !cgroup.BeforeInput {
		return "", errors.New("actual running broker cgroup limits were not observed before provider input")
	}
	if profile.Snow {
		if err := validateCurrentQgSnowQualificationSettingsOwnership(artifacts["uid-map.txt"], artifacts["provider-settings.json"], artifacts["provider-settings-ownership.json"], profile.Settings, receipt.Owner, terminal.ContainerId, cgroup.Pid); err != nil {
			return "", err
		}
		if err := validateCurrentQgSnowQualificationStateOwnership(artifacts["uid-map.txt"], artifacts["member-state-ownership.json"], profile.StateDirectory, receipt.Owner, terminal.ContainerId, cgroup.Pid); err != nil {
			return "", err
		}
		if !canceled {
			if err := validateWinnerMultipassJournals(artifacts, profile.Settings, receipt.Owner, receipt.AttemptDirectory, settings, envelope, result); err != nil {
				return "", err
			}
		} else if err := validateCurrentQgSnowQualificationStateCensus(profile.StateDirectory, 0); err != nil {
			return "", err
		}
	} else {
		if err := validateCurrentQgQualificationKeyOwnership(artifacts["uid-map.txt"], artifacts["key-ownership.json"], receipt.Owner, terminal.ContainerId, cgroup.Pid); err != nil {
			return "", err
		}
	}
	return currentQgQualificationHash(lines[len(lines)-1]), nil
}

// Release authorization remains a separate native gate. This profile specifies
// what actual process/network/file evidence must match, never its own acceptance.
type winnerMultipassContainmentProfile struct {
	ImageId          string                    `json:"image_id"`
	ProviderSettings currentQgQualificationPin `json:"provider_settings"`
	StateRoot        string                    `json:"state_root"`
}

// Parent custody retains these exact originals and the manifest independently.
// A valid failed/canceled attempt is evidence of containment, not review success.
type winnerMultipassContainmentReceipt struct {
	AttemptDirectory     string                          `json:"attempt_directory"`
	ManifestSha256       string                          `json:"manifest_sha256"`
	WrapperReceiptSha256 string                          `json:"wrapper_receipt_sha256"`
	WrapperReceipt       []byte                          `json:"wrapper_receipt"`
	Artifacts            []currentQgQualificationBinding `json:"artifacts"`
	Result               *winnerMultipassBrokerResult    `json:"result"`
	Canceled             bool                            `json:"canceled"`
}

// The old host invoker's 1-MiB stdout bound does not admit this separate profile.
func invokeWinnerMultipassBroker(ctx context.Context, broker currentQgQualificationPin, input []byte) ([]byte, []byte, error) {
	limits := defaultWinnerMultipassProviderLimits()
	if len(input) == 0 || len(input) > limits.MaximumEnvelopeBytes {
		return nil, nil, errors.New("multi-pass wrapper stdin exceeds its fixed bound")
	}
	if err := verifySourceSupervisorProgram(broker.Path, broker.Sha256); err != nil {
		return nil, nil, err
	}
	command := exec.CommandContext(ctx, broker.Path)
	command.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	command.Stdin = bytes.NewReader(input)
	command.Cancel = func() error { return command.Process.Signal(syscall.SIGTERM) }
	command.WaitDelay = defaultCurrentQgQualificationSettings().CleanupGrace
	output, diagnostics := &sourceSupervisorOutput{limit: limits.MaximumResultBytes}, &sourceSupervisorOutput{limit: 64 * 1024}
	command.Stdout, command.Stderr = output, diagnostics
	err := command.Run()
	if output.exceeded || diagnostics.exceeded {
		err = errors.Join(err, errors.New("multi-pass wrapper output exceeded independent capture bound"))
	}
	return output.content, diagnostics.content, err
}

// Decode the actual inspection, not a desired command or rewritten flat receipt.
func validateWinnerMultipassContainer(content []byte, owner, image, network string, profile currentQgQualificationProviderProfile) error {
	var containers []struct {
		Id, Image string
		Config    struct {
			User                 string
			Entrypoint, Cmd, Env []string
			Labels               map[string]string
		}
		HostConfig struct {
			NetworkMode                                                       string
			ReadonlyRootfs, Privileged, Init                                  bool
			CgroupnsMode, CpusetCpus, PidMode, IpcMode                        string
			UtsMode                                                           string `json:"UTSMode"`
			NanoCpus, Memory, MemorySwap                                      int64
			PidsLimit                                                         int
			CapDrop, CapAdd, SecurityOpt, Dns, ExtraHosts, VolumesFrom, Binds []string
			Sysctls                                                           map[string]string
			Devices, DeviceRequests                                           []json.RawMessage
			Tmpfs, PortBindings                                               map[string]json.RawMessage
			Ulimits                                                           []struct {
				Name       string
				Soft, Hard int64
			}
		}
		Mounts []struct {
			Type, Source, Destination string
			Rw                        bool `json:"RW"`
		}
	}
	if json.Unmarshal(content, &containers) != nil || len(containers) != 1 {
		return errors.New("actual review container inspection invalid")
	}
	container := containers[0]
	host := container.HostConfig
	if !profile.Snow {
		return errors.New("multi-pass containment has no historical credential profile")
	}
	entrypoint := []string{"/review-broker", "review-multipass-broker"}
	command := []string{"--review-provider-settings=/run/review/provider.json", "--review-provider-settings-sha256=" + profile.Settings.Sha256, "--review-state-directory=/run/review/state"}
	hosts := []string{}
	mountSource, mountDestination := profile.Settings.Path, "/run/review/provider.json"
	if host.ExtraHosts == nil {
		host.ExtraHosts = []string{}
	}
	if container.Config.Cmd == nil {
		container.Config.Cmd = []string{}
	}
	noPrivilege := reflect.DeepEqual(host.SecurityOpt, []string{"no-new-privileges:true"})
	expectedMounts := 2
	if !validSha256(container.Id) || container.Image != image || container.Config.Labels["com.urnetwork.review.owner"] != owner || container.Config.User != "65532:65532" || !reflect.DeepEqual(container.Config.Entrypoint, entrypoint) || !reflect.DeepEqual(container.Config.Cmd, command) || host.NetworkMode != network || !host.ReadonlyRootfs || host.Privileged || !host.Init || host.CgroupnsMode != "private" || host.CpusetCpus != "20,22" || host.NanoCpus != 1000000000 || host.Memory != 2147483648 || host.MemorySwap != 2147483648 || host.PidsLimit != 64 || !reflect.DeepEqual(host.CapDrop, []string{"ALL"}) || len(host.CapAdd) != 0 || !noPrivilege || !reflect.DeepEqual(host.Dns, []string{"127.0.0.1"}) || !reflect.DeepEqual(host.ExtraHosts, hosts) || host.Sysctls["net.ipv6.conf.all.disable_ipv6"] != "1" || host.Sysctls["net.ipv6.conf.default.disable_ipv6"] != "1" || host.PidMode != "" || host.IpcMode != "private" || host.UtsMode != "" || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.Binds) != 0 || len(host.VolumesFrom) != 0 || len(host.Tmpfs) != 0 || len(host.PortBindings) != 0 || len(container.Mounts) != expectedMounts {
		return errors.New("actual review container gained authority or lost resource/network bounds")
	}
	providerMount, stateMount := false, false
	for _, mount := range container.Mounts {
		if mount.Type == "bind" && mount.Source == mountSource && mount.Destination == mountDestination && !mount.Rw && !providerMount {
			providerMount = true
			continue
		}
		if profile.Snow && mount.Type == "bind" && profile.StateDirectory != "" && mount.Source == profile.StateDirectory && mount.Destination == "/run/review/state" && mount.Rw && !stateMount {
			stateMount = true
			continue
		}
		return errors.New("actual broker gained an unapproved writable leaf or provider mount")
	}
	if !providerMount || (profile.Snow && !stateMount) {
		return errors.New("actual broker omitted the required provider/state leaf")
	}
	if profile.Snow {
		boundedFiles := false
		for _, limit := range host.Ulimits {
			if limit.Name == "fsize" && limit.Soft == int64(defaultWinnerMultipassSettings().MaximumResponseBytes) && limit.Hard == limit.Soft {
				boundedFiles = true
			}
		}
		if !boundedFiles {
			return errors.New("actual Snow journal files lack the independent hard size bound")
		}
	}
	for _, entry := range container.Config.Env {
		if !strings.HasPrefix(entry, "PATH=") && !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "HOSTNAME=") {
			return errors.New("actual review container inherited ambient environment")
		}
	}
	return nil
}

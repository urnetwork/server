// All container/kernel/UID observations here are explicit private synthetic
// captures. No Docker, firewall, native host path or provider is invoked.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Independently assembled capture exercises the complete actual reader with
// reserved provider identities and a synthetic private bridge subnet only.
func currentQgContainmentFixture(t *testing.T, canceled bool) ([]byte, []byte, []byte, string, currentQgQualificationSettings, string, currentQgQualificationProviderProfile) {
	t.Helper()
	plan, expectations, _, settings := currentQgExecutionFixture(t)
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil || os.Chmod(parent, 0700) != nil {
		t.Fatal("private synthetic capture root unavailable", err)
	}
	settings.StateRoot = filepath.Join(parent, "attempts")
	if err := os.Mkdir(settings.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	attempt, err := os.MkdirTemp(settings.StateRoot, "invocation.")
	if err != nil {
		t.Fatal(err)
	}
	owner, container, network := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("d", 64)
	image := "sha256:" + strings.Repeat("c", 64)
	provider := defaultWinnerSnowProviderSettings()
	providerBytes, _ := json.Marshal(provider)
	profile := currentQgQualificationProviderProfile{Snow: true, ProviderHost: "review.synthetic.example", ProviderIp: "192.0.2.20", ProviderPort: 18000,
		Settings: currentQgQualificationPin{Path: "/synthetic/provider.json", Sha256: currentQgQualificationHash(providerBytes)}, StateDirectory: filepath.Join(attempt, "member-state")}
	if err := os.Mkdir(profile.StateDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	encode := func(value any) []byte {
		content, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	request := plan.Cases[4].Request
	output := []byte{}
	if canceled {
		request.Operation = "qualification_cancel_before_provider"
	} else {
		output = currentQgExecutionMockResponse(t, request, expectations[4], nil)
		var result winnerReviewBrokerResult
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatal(err)
		}
		var evidence winnerSnowEvidence
		if err := json.Unmarshal([]byte(result.PrivateEvidence), &evidence); err != nil {
			t.Fatal(err)
		}
		evidence.ProviderSettingsSha256 = profile.Settings.Sha256
		output = encode(winnerSnowBrokerResult(request, evidence.Judgment, encode(evidence)))
	}
	input := encode(request)
	subnet, chain, comment := "10.254.42.0/24", "URRV_"+owner[:16], "urnetwork-review-"+owner
	uidMap := []byte("0 2400000 65536\n")
	artifacts := map[string][]byte{
		"input.json": input, "broker.stdout": output, "broker.stderr": {}, "uid-map.txt": uidMap, "provider-settings.json": providerBytes, "provider-address.txt": []byte(profile.ProviderIp + "\n"),
		"image-inspect.json": encode([]any{map[string]any{"Id": image}}),
		"network-inspect.json": encode([]any{map[string]any{"Id": network, "Driver": "bridge", "Scope": "local", "Internal": false, "EnableIPv6": false, "Attachable": false,
			"Labels": map[string]string{"com.urnetwork.review.owner": owner}, "Options": map[string]string{"com.docker.network.bridge.enable_icc": "false"}, "IPAM": map[string]any{"Config": []any{map[string]any{"Subnet": subnet}}}}}),
		"container-inspect.json": encode([]any{map[string]any{"Id": container, "Image": image,
			"Config": map[string]any{"User": "65532:65532", "Entrypoint": []string{"/review-broker", "review-broker"}, "Cmd": []string{"--review-provider-settings=/run/review/provider.json", "--review-provider-settings-sha256=" + profile.Settings.Sha256, "--review-state-directory=/run/review/state"}, "Env": []string{"PATH=/usr/bin", "HOME=/"}, "Labels": map[string]string{"com.urnetwork.review.owner": owner}},
			"HostConfig": map[string]any{"NetworkMode": network, "ReadonlyRootfs": true, "Privileged": false, "Init": true, "CgroupnsMode": "private", "CpusetCpus": "20,22", "PidMode": "", "IpcMode": "private", "UTSMode": "", "NanoCpus": 1000000000, "Memory": 2147483648, "MemorySwap": 2147483648, "PidsLimit": 64,
				"CapDrop": []string{"ALL"}, "CapAdd": []string{}, "SecurityOpt": []string{"no-new-privileges:true"}, "Dns": []string{"127.0.0.1"}, "ExtraHosts": []string{}, "Sysctls": map[string]string{"net.ipv6.conf.all.disable_ipv6": "1", "net.ipv6.conf.default.disable_ipv6": "1"}, "Ulimits": []any{map[string]any{"Name": "fsize", "Soft": settings.MaximumMemberJournalBytes, "Hard": settings.MaximumMemberJournalBytes}}},
			"Mounts": []any{map[string]any{"Type": "bind", "Source": profile.Settings.Path, "Destination": "/run/review/provider.json", "RW": false}, map[string]any{"Type": "bind", "Source": profile.StateDirectory, "Destination": "/run/review/state", "RW": true}}}}),
		"cgroup.json": encode(map[string]any{"schema": 1, "kind": "winner-review-running-cgroup", "container_pid": 7777, "path": "/synthetic/cgroup", "memory_max": "2147483648", "memory_swap_max": "0", "pids_max": "64", "cpu_max": "100000 100000", "cpuset_cpus": "20,22", "observed_before_provider_input": true}),
		"provider-settings-ownership.json": encode(map[string]any{"schema": 1, "kind": "winner-review-provider-settings-ownership", "owner": owner, "container_id": container, "container_pid": 7777, "path": profile.Settings.Path,
			"container_uid": 65532, "mapped_host_uid": 2465532, "host_uid": 0, "host_gid": 0, "mode": "0444", "bytes": len(providerBytes), "links": 1, "device": "77", "inode": "888", "uid_map_sha256": currentQgQualificationHash(uidMap), "provider_settings_sha256": profile.Settings.Sha256, "observed_before_provider_input": true}),
		"member-state-ownership.json": encode(map[string]any{"schema": 1, "kind": "winner-review-member-state-ownership", "owner": owner, "container_id": container, "container_pid": 7777, "path": profile.StateDirectory,
			"container_uid": 65532, "mapped_host_uid": 2465532, "host_uid": 2465532, "host_gid": 0, "mode": "0700", "device": "77", "inode": "889", "uid_map_sha256": currentQgQualificationHash(uidMap), "observed_before_provider_input": true}),
		"fence.json":        encode(map[string]any{"schema": 1, "kind": "winner-review-provider-egress-fence", "owner": owner, "chain": chain, "source_subnet": subnet, "provider_host": profile.ProviderHost, "provider_ipv4": profile.ProviderIp, "provider_port": profile.ProviderPort, "external_dns": false, "host_input_denied": true, "default_deny": true, "installed_before_start": true}),
		"fence-forward.txt": []byte(fmt.Sprintf("-A DOCKER-USER -s %s -m comment --comment %s -j %s\n", subnet, comment, chain)),
		"fence-input.txt":   []byte(fmt.Sprintf("-A INPUT -s %s -m comment --comment %s -j DROP\n", subnet, comment)),
		"fence-chain.txt":   []byte(fmt.Sprintf("-A %s -d %s/32 -p tcp -m tcp --dport %d -j ACCEPT\n-A %s -j DROP\n", chain, profile.ProviderIp, profile.ProviderPort, chain)),
	}
	status, observed := 0, true
	var brokerExit any = 0
	if canceled {
		status, observed, brokerExit = 143, false, nil
	} else {
		artifacts["container-state.json"] = encode(map[string]any{"Status": "exited", "Running": false, "ExitCode": 0, "OOMKilled": false})
		var result winnerReviewBrokerResult
		var evidence winnerSnowEvidence
		if json.Unmarshal(output, &result) != nil || json.Unmarshal([]byte(result.PrivateEvidence), &evidence) != nil {
			t.Fatal("synthetic original evidence unavailable")
		}
		readable, err := winnerModelInput(request)
		if err != nil {
			t.Fatal(err)
		}
		for index, member := range evidence.Members {
			started := map[string]any{"schema": 1, "input_sha256": currentQgQualificationHash(readable), "provider_settings_sha256": profile.Settings.Sha256, "model": member.Model, "request_sha256": member.RequestSha256}
			complete := map[string]any{"schema": 1, "observation": member}
			for phase, content := range map[string][]byte{"started": encode(started), "complete": encode(complete)} {
				name := fmt.Sprintf("member-%02d.%s.json", index+1, phase)
				if err := os.WriteFile(filepath.Join(profile.StateDirectory, name), content, 0600); err != nil {
					t.Fatal(err)
				}
				artifacts[name] = content
			}
		}
		models := []map[string]string{}
		for _, member := range provider.Members {
			models = append(models, map[string]string{"name": member.Name, "model": member.Model, "revision": member.Revision, "reasoning": member.Reasoning})
		}
		key := currentQgQualificationHash([]byte(request.Template.IdempotencyKey))
		artifacts["attempt-claim.json"] = encode(map[string]any{"schema": 1, "kind": "snow-review-single-attempt-claim", "idempotency_key_sha256": key, "input_sha256": currentQgQualificationHash(input), "provider_settings_sha256": profile.Settings.Sha256,
			"model_set_sha256": currentQgQualificationHash(encode(models)), "owner": owner, "attempt_directory": attempt, "automatic_retries": false})
		claims := filepath.Join(parent, "snow-review-claims")
		if err := os.Mkdir(claims, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(claims, key+".json"), artifacts["attempt-claim.json"], 0600); err != nil {
			t.Fatal(err)
		}
	}
	artifacts["terminal.json"] = encode(map[string]any{"schema": 1, "kind": "winner-review-container-terminal", "owner": owner, "image_id": image, "container_id": container, "network_id": network,
		"wrapper_exit": status, "broker_exit": brokerExit, "broker_exit_observed": observed, "cleanup_verified": true, "provider_input_released": !canceled})
	diagnostics := currentQgContainmentWriteFixture(t, attempt, owner, status, artifacts)
	return input, output, diagnostics, image, settings, attempt, profile
}

// Unit-only rewrites preserve exact byte pins for intentionally forged captures;
// the independent reader still must reject wrong content/authority.
func currentQgContainmentWriteFixture(t *testing.T, attempt, owner string, status int, artifacts map[string][]byte) []byte {
	t.Helper()
	bindings := []currentQgQualificationBinding{}
	for name, content := range artifacts {
		if err := os.WriteFile(filepath.Join(attempt, name), content, 0600); err != nil {
			t.Fatal(err)
		}
		bindings = append(bindings, currentQgQualificationBinding{Path: name, Sha256: currentQgQualificationHash(content), Bytes: int64(len(content))})
	}
	manifest, _ := json.Marshal(map[string]any{"schema": 1, "kind": "winner-review-container-evidence", "artifacts": bindings})
	if err := os.WriteFile(filepath.Join(attempt, "evidence-manifest.json"), manifest, 0600); err != nil {
		t.Fatal(err)
	}
	receipt, _ := json.Marshal(currentQgQualificationWrapperReceipt{Schema: 1, Kind: "winner-review-container-receipt", Owner: owner, AttemptDirectory: attempt, ManifestSha256: currentQgQualificationHash(manifest), WrapperExit: status, CleanupVerified: true})
	return append(receipt, '\n')
}

// Completed custody includes exactly23 captured artifacts, all four original
// member journals and an independently reread global whole-attempt claim.
func TestCurrentQgQualificationContainmentAuthenticatesCompleteCapture(t *testing.T) {
	input, output, diagnostics, image, settings, attempt, profile := currentQgContainmentFixture(t, false)
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, false, image, settings, profile); err != nil {
		t.Fatal("complete synthetic current capture refused", err)
	}
	if err := os.WriteFile(filepath.Join(attempt, "member-02.complete.json"), []byte(`{"schema":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, false, image, settings, profile); err == nil {
		t.Fatal("changed second original journal retained completed authority")
	}
	input, output, diagnostics, image, settings, attempt, profile = currentQgContainmentFixture(t, false)
	var request winnerReviewBrokerRequest
	if err := json.Unmarshal(input, &request); err != nil {
		t.Fatal(err)
	}
	key := currentQgQualificationHash([]byte(request.Template.IdempotencyKey))
	if err := os.WriteFile(filepath.Join(filepath.Dir(settings.StateRoot), "snow-review-claims", key+".json"), []byte(`{"synthetic_replaced":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, false, image, settings, profile); err == nil {
		t.Fatal("captured manifest hid a changed independent durable claim")
	}
}

// Cancellation retains17 artifacts, actual null broker exit and an empty
// state leaf. A hidden started journal cannot be represented as pre-input.
func TestCurrentQgQualificationContainmentCancellationIsBeforeInput(t *testing.T) {
	input, output, diagnostics, image, settings, _, profile := currentQgContainmentFixture(t, true)
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, true, image, settings, profile); err != nil || len(output) != 0 {
		t.Fatal("actual-shape pre-input cancellation refused", err)
	}
	if err := os.WriteFile(filepath.Join(profile.StateDirectory, "member-01.started.json"), []byte(`{"synthetic_started":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, true, image, settings, profile); err == nil {
		t.Fatal("hidden started member was accepted as pre-input cancellation")
	}
}

// Both UID translation and the actual container/fence forbid extra authority.
// Test addresses are reserved; no network endpoint or native process is used.
func TestCurrentQgQualificationContainmentRejectsExtraAuthority(t *testing.T) {
	input, output, diagnostics, image, settings, attempt, profile := currentQgContainmentFixture(t, false)
	content, err := os.ReadFile(filepath.Join(attempt, "container-inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(c map[string]any) {
			c["HostConfig"].(map[string]any)["SecurityOpt"] = []string{"no-new-privileges:true", "seccomp=unconfined"}
		},
		func(c map[string]any) { c["Mounts"].([]any)[0].(map[string]any)["RW"] = true },
		func(c map[string]any) {
			c["Config"].(map[string]any)["Cmd"] = []string{"--model-key-file=/synthetic/key"}
		},
		func(c map[string]any) { c["HostConfig"].(map[string]any)["Ulimits"] = []any{} },
	} {
		var containers []map[string]any
		if err := json.Unmarshal(content, &containers); err != nil {
			t.Fatal(err)
		}
		mutate(containers[0])
		changed, _ := json.Marshal(containers)
		if validateCurrentQgQualificationContainerForProvider(changed, strings.Repeat("a", 64), image, strings.Repeat("d", 64), profile) == nil {
			t.Fatal("current QG capture allowed extra writable/privileged authority")
		}
	}
	if _, err := currentQgQualificationMappedUid([]byte("0 2400000 65536\n"), 65532); err != nil {
		t.Fatal("complete synthetic UID map refused", err)
	}
	for _, content := range [][]byte{[]byte("0 2400000 65532\n"), []byte("0 2400000 65536\n65532 2700000 1\n"), []byte("0 4294967290 65536\n")} {
		if _, err := currentQgQualificationMappedUid(content, 65532); err == nil {
			t.Fatal("missing/overlapping/overflow UID map accepted")
		}
	}
	if _, err := authenticateCurrentQgQualificationContainmentForProvider(append(append([]byte{}, input...), ' '), output, diagnostics, false, image, settings, profile); err == nil {
		t.Fatal("capture from different exact input bytes accepted")
	}
	if bytes.Equal(input, output) {
		t.Fatal("synthetic fixture accidentally conflated input and output")
	}
}

package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const subtensorMarker = "monitor-signal-17.1-subtensor"

// Signal subtensor implements SIGNALS.md §17.1. It compares each local node
// and overlay gateway with the configured public reference chain. In
// particular, it distinguishes a reused-database warp fallback from a genuine
// warp stuck on historical GRANDPA finality proofs and verifies the configured
// container image and data-generation identity.
func NewSubtensorSignal() Signal {
	return &signalAdapter{
		number: "17.1", key: "subtensor", name: "Subtensor node, gateway, and synchronization health",
		probe: subtensorProbe{},
	}
}

type subtensorProbe struct{}

func (subtensorProbe) id() string             { return "subtensor/node-health" }
func (subtensorProbe) tier() string           { return tierWarn }
func (subtensorProbe) cadence() time.Duration { return time.Minute }

type subtensorScriptConfig struct {
	OverlayAddress string                  `json:"overlay_address"`
	PublicRPCURL   string                  `json:"public_rpc_url"`
	Nodes          []SubtensorNodeSettings `json:"nodes"`
}

type subtensorObservation struct {
	Units          map[string]string          `json:"units"`
	OverlayPresent bool                       `json:"overlay_present"`
	Public         subtensorRPCObservation    `json:"public"`
	Nodes          []subtensorNodeObservation `json:"nodes"`
}

type subtensorRPCObservation struct {
	Chain      string                  `json:"chain"`
	Genesis    string                  `json:"genesis"`
	Head       string                  `json:"head"`
	Runtime    subtensorRuntimeVersion `json:"runtime"`
	EVMChainID string                  `json:"evm_chain_id"`
	EthGetLogs bool                    `json:"eth_get_logs"`
	Health     subtensorHealth         `json:"health"`
	Sync       subtensorSyncState      `json:"sync"`
	Errors     map[string]string       `json:"errors"`
}

type subtensorNodeObservation struct {
	Name                   string                  `json:"name"`
	SyncMode               string                  `json:"sync_mode"`
	RPCPort                int                     `json:"rpc_port"`
	GatewayPort            int                     `json:"gateway_port"`
	Direct                 subtensorRPCObservation `json:"direct"`
	Gateway                subtensorRPCObservation `json:"gateway"`
	FirstHead              string                  `json:"first_head"`
	SecondHead             string                  `json:"second_head"`
	GatewayHTTP            int                     `json:"gateway_http"`
	ContainerImage         string                  `json:"container_image"`
	ContainerStarted       string                  `json:"container_started"`
	DataPath               string                  `json:"data_path"`
	RuntimeUID             int64                   `json:"runtime_uid"`
	RuntimeGID             int64                   `json:"runtime_gid"`
	DataPathUID            int64                   `json:"data_path_uid"`
	DataPathGID            int64                   `json:"data_path_gid"`
	DataPathMode           int64                   `json:"data_path_mode"`
	DataPermissionObserved bool                    `json:"data_permission_observed"`
	DataRuntimeWritable    bool                    `json:"data_runtime_writable"`
	DataPermissionError    bool                    `json:"data_permission_error"`
	WarpFallback           bool                    `json:"warp_fallback"`
	WarpProofStarted       bool                    `json:"warp_proof_started"`
	ContainerError         string                  `json:"container_error"`
}

type subtensorRuntimeVersion struct {
	SpecName           string `json:"specName"`
	SpecVersion        int64  `json:"specVersion"`
	TransactionVersion int64  `json:"transactionVersion"`
}

type subtensorHealth struct {
	Peers     int64 `json:"peers"`
	IsSyncing bool  `json:"isSyncing"`
}

type subtensorSyncState struct {
	StartingBlock int64 `json:"startingBlock"`
	CurrentBlock  int64 `json:"currentBlock"`
	HighestBlock  int64 `json:"highestBlock"`
}

func (subtensorProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	hosts := env.cfg.hostsWithRole("subtensor")
	findings := []finding{}
	for _, target := range hosts {
		if target.subtensor == nil {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", fmt.Errorf("subtensor settings are missing")))
			continue
		}
		if err := validateSubtensorSettings(target.subtensor); err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", err))
			continue
		}
		command, err := subtensorCommand(target)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", err))
			continue
		}
		output, err := env.runner.shell(ctx, target, command)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", err))
			continue
		}
		observation, err := parseSubtensorObservation(output)
		if err != nil {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", err))
			continue
		}
		findings = append(findings, evaluateSubtensor(target, observation)...)
	}
	return findings, nil
}

func validateSubtensorSettings(settings *SubtensorHostSettings) error {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(settings.PublicRPCURL))
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return fmt.Errorf("subtensor: invalid public RPC URL")
	}
	if settings.ExpectedChain == "" || settings.ExpectedGenesisHash == "" ||
		settings.ExpectedSpecName == "" || settings.ExpectedSpecVersion <= 0 ||
		settings.ExpectedTransactionVersion <= 0 || settings.ExpectedEVMChainID == "" {
		return fmt.Errorf("subtensor: incomplete expected chain identity")
	}
	if len(settings.Nodes) == 0 {
		return fmt.Errorf("subtensor: no nodes configured")
	}
	seen := map[string]bool{}
	for _, node := range settings.Nodes {
		if node.Name == "" || seen[node.Name] {
			return fmt.Errorf("subtensor: empty or duplicate node name %q", node.Name)
		}
		seen[node.Name] = true
		if node.SyncMode != "full" && node.SyncMode != "warp" {
			return fmt.Errorf("subtensor: node %s has unsupported sync mode %q", node.Name, node.SyncMode)
		}
		if node.RPCPort < 1 || node.RPCPort > 65535 || node.GatewayPort < 1 || node.GatewayPort > 65535 {
			return fmt.Errorf("subtensor: node %s has invalid ports", node.Name)
		}
		if (node.ExpectedImage != "" || node.ExpectedDataPath != "") && node.ContainerName == "" {
			return fmt.Errorf("subtensor: node %s has deployment expectations without a container name", node.Name)
		}
		if node.ExpectedImage != "" && !strings.HasPrefix(node.ExpectedImage, "ghcr.io/raofoundation/subtensor@sha256:") {
			return fmt.Errorf("subtensor: node %s has an invalid expected image", node.Name)
		}
		if node.ExpectedDataPath != "" && !strings.HasPrefix(node.ExpectedDataPath, "/data/") {
			return fmt.Errorf("subtensor: node %s has an invalid expected data path", node.Name)
		}
	}
	return nil
}

func subtensorCommand(target *host) (string, error) {
	config, err := json.Marshal(subtensorScriptConfig{
		OverlayAddress: target.overlayIp,
		PublicRPCURL:   target.subtensor.PublicRPCURL,
		Nodes:          target.subtensor.Nodes,
	})
	if err != nil {
		return "", err
	}
	return "# " + subtensorMarker + "\nSUBTENSOR_MONITOR_CONFIG=" + shellSingleQuote(string(config)) + " python3 - <<'PY'\n" + subtensorScript + "\nPY", nil
}

const subtensorScript = `import json
import os
import subprocess
import time
import urllib.request

config = json.loads(os.environ["SUBTENSOR_MONITOR_CONFIG"])

def rpc(url, method, params=None):
    payload = json.dumps({"jsonrpc": "2.0", "id": 1, "method": method, "params": params or []}).encode()
    request = urllib.request.Request(url, data=payload, headers={"content-type": "application/json"})
    with urllib.request.urlopen(request, timeout=5) as response:
        document = json.load(response)
    if "error" in document:
        raise RuntimeError(str(document["error"]))
    if "result" not in document:
        raise RuntimeError("JSON-RPC response has no result")
    return document["result"]

def read(target, key, url, method, params=None):
    try:
        target[key] = rpc(url, method, params)
    except Exception as error:
        target.setdefault("errors", {})[key] = "%s: %s" % (type(error).__name__, error)

def inspect_rpc(url):
    result = {"errors": {}}
    read(result, "chain", url, "system_chain")
    read(result, "genesis", url, "chain_getBlockHash", [0])
    read(result, "head", url, "chain_getHeader")
    if isinstance(result.get("head"), dict):
        result["head"] = result["head"].get("number", "")
    read(result, "runtime", url, "state_getRuntimeVersion")
    read(result, "evm_chain_id", url, "eth_chainId")
    return result

def unit_state(unit):
    run = subprocess.run(["systemctl", "is-active", unit], text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)
    return run.stdout.strip() or "unknown"

def inspect_container(node):
    name = node.get("ContainerName", "")
    if not name:
        return {}
    try:
        output = subprocess.check_output(
            ["sudo", "-n", "/usr/local/sbin/subtensor-monitor", name],
            text=True, stderr=subprocess.STDOUT,
            timeout=10,
        )
        result = json.loads(output)
        if not isinstance(result, dict):
            raise RuntimeError("Subtensor monitor helper returned a non-object")
        return result
    except Exception as error:
        return {"container_error": "%s: %s" % (type(error).__name__, error)}

observation = {
    "units": {unit: unit_state(unit) for unit in ("subtensor", "nginx", "openvpn@by-pre")},
    "overlay_present": False,
    "public": inspect_rpc(config["public_rpc_url"]),
    "nodes": [],
}

try:
    addresses = json.loads(subprocess.check_output(["ip", "-j", "address", "show"], text=True))
    observation["overlay_present"] = any(
        address.get("local") == config["overlay_address"]
        for interface in addresses
        for address in interface.get("addr_info", [])
    )
except Exception:
    observation["overlay_present"] = False

for node in config["nodes"]:
    direct_url = "http://127.0.0.1:%d" % node["RPCPort"]
    gateway_url = "http://%s:%d" % (config["overlay_address"], node["GatewayPort"])
    direct = inspect_rpc(direct_url)
    read(direct, "health", direct_url, "system_health")
    read(direct, "sync", direct_url, "system_syncState")
    read(direct, "eth_get_logs", direct_url, "eth_getLogs", [{"fromBlock": "latest", "toBlock": "latest"}])
    direct["eth_get_logs"] = "eth_get_logs" in direct and isinstance(direct["eth_get_logs"], list)
    gateway = inspect_rpc(gateway_url)
    gateway_http = 0
    try:
        with urllib.request.urlopen(gateway_url + "/healthz", timeout=5) as response:
            gateway_http = response.status
    except Exception as error:
        gateway.setdefault("errors", {})["healthz"] = "%s: %s" % (type(error).__name__, error)
    node_observation = {
        "name": node["Name"],
        "sync_mode": node["SyncMode"],
        "rpc_port": node["RPCPort"],
        "gateway_port": node["GatewayPort"],
        "direct": direct,
        "gateway": gateway,
        "first_head": direct.get("head", ""),
        "second_head": "",
        "gateway_http": gateway_http,
    }
    node_observation.update(inspect_container(node))
    observation["nodes"].append(node_observation)

time.sleep(15)
for node in observation["nodes"]:
    direct_url = "http://127.0.0.1:%d" % node["rpc_port"]
    gateway_url = "http://%s:%d" % (config["overlay_address"], node["gateway_port"])
    try:
        node["second_head"] = rpc(direct_url, "chain_getHeader").get("number", "")
    except Exception as error:
        node["direct"].setdefault("errors", {})["second_head"] = "%s: %s" % (type(error).__name__, error)
    try:
        node["gateway"]["head"] = rpc(gateway_url, "chain_getHeader").get("number", "")
    except Exception as error:
        node["gateway"].setdefault("errors", {})["head"] = "%s: %s" % (type(error).__name__, error)

print(json.dumps(observation, separators=(",", ":"), sort_keys=True))`

func parseSubtensorObservation(output string) (subtensorObservation, error) {
	var observation subtensorObservation
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &observation); err != nil {
		return observation, fmt.Errorf("subtensor: parse observation: %w", err)
	}
	if observation.Units == nil || len(observation.Nodes) == 0 {
		return observation, fmt.Errorf("subtensor: incomplete observation")
	}
	return observation, nil
}

func evaluateSubtensor(target *host, observation subtensorObservation) []finding {
	settings := target.subtensor
	findings := []finding{}
	unitProblems := []string{}
	for _, unit := range []string{"subtensor", "nginx", "openvpn@by-pre"} {
		if observation.Units[unit] != "active" {
			unitProblems = append(unitProblems, unit+"="+firstNonempty(observation.Units[unit], "missing"))
		}
	}
	if !observation.OverlayPresent {
		unitProblems = append(unitProblems, "overlay-address=absent")
	}
	if len(unitProblems) > 0 {
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierPage, class: "subtensor-gateway",
			target: target.name, frame: "host", sustain: 2,
			symptom:   "The Subtensor host lifecycle or overlay gateway identity is unavailable",
			mechanism: "subtensor.service is only a oneshot launcher; the container, nginx, and the OpenVPN-owned bind address can fail independently after that launcher exits successfully.",
			baseline:  "subtensor, nginx, and openvpn@by-pre are active and the configured overlay address is present on the host.",
			observed:  strings.Join(unitProblems, " "),
			action:    "Restore the first failed lifecycle layer. For an absent overlay address, repair OpenVPN ownership before restarting nginx; do not treat active (exited) as node health.",
			verify:    "Require all three units active, the exact overlay address present, and both gateway health/RPC paths successful.",
			playbook:  "SIGNALS.md §17.1",
		})
	}

	publicHead, publicHeadErr := subtensorHex(observation.Public.Head)
	if len(observation.Public.Errors) > 0 || publicHeadErr != nil {
		findings = append(findings, cannotObserveFinding(target.name+"/subtensor-public-reference", fmt.Errorf("public RPC: %s", subtensorErrors(observation.Public.Errors, publicHeadErr))))
	} else {
		identityProblems := subtensorIdentityProblems(settings, observation.Public, true)
		runtimeAhead := observation.Public.Runtime.SpecVersion > settings.ExpectedSpecVersion &&
			observation.Public.Chain == settings.ExpectedChain &&
			strings.EqualFold(observation.Public.Genesis, settings.ExpectedGenesisHash) &&
			observation.Public.Runtime.SpecName == settings.ExpectedSpecName &&
			strings.EqualFold(observation.Public.EVMChainID, settings.ExpectedEVMChainID)
		if runtimeAhead {
			findings = append(findings, finding{
				probeId: "subtensor/node-health", tier: tierPage, class: "subtensor-runtime-ahead",
				target: target.name, frame: "public-reference", sustain: 1,
				symptom:   "The public Subtensor runtime is newer than the configured current-runtime pin",
				mechanism: "The public RPC still matches the exact configured chain, genesis, runtime name, and EVM chain identity, but its on-chain Wasm spec version advanced beyond the static monitor expectation. This is an upstream runtime transition and stale conformance pin, not evidence of a wrong RPC surface or failed local node.",
				baseline:  "The independently verified current upstream runtime and transaction versions match every owning configuration.",
				observed: fmt.Sprintf(
					"public_head=%d specVersion=%d expected_specVersion=%d transactionVersion=%d expected_transactionVersion=%d",
					publicHead, observation.Public.Runtime.SpecVersion, settings.ExpectedSpecVersion,
					observation.Public.Runtime.TransactionVersion, settings.ExpectedTransactionVersion,
				),
				evidence: fmt.Sprintf(
					"chain=%q genesis=%q runtime=%s/%d transaction=%d evm=%q",
					observation.Public.Chain, observation.Public.Genesis, observation.Public.Runtime.SpecName,
					observation.Public.Runtime.SpecVersion, observation.Public.Runtime.TransactionVersion,
					observation.Public.EVMChainID,
				),
				context:  "This is an upstream/configuration boundary. Lagging local nodes correctly report the historical runtime at their own heads until they import the transition block; do not restart, replace, or reset a progressing database merely because the public current runtime advanced.",
				action:   "Independently verify the official upstream release artifact and exact on-chain transition, then update expected_spec_version—and expected_transaction_version if it changed—in each stale owning configuration while preserving owners that already match. Rebuild and promote the monitor after its inventory changes; do not restart either node solely for this pin update.",
				verify:   "The public reference repeatedly retains the exact chain/genesis/EVM identity at the verified newer runtime, every configuration owner agrees on it, and the runtime-ahead alert clears after monitor promotion while progressing historical nodes retain their ordinary lag classifications. At convergence, each node and gateway must report the new pinned runtime.",
				playbook: "SIGNALS.md §17.1",
			})
		} else if len(identityProblems) > 0 {
			findings = append(findings, subtensorIdentityFinding(target.name, "public-reference", identityProblems, observation.Public))
		}
	}

	configuredNodes := map[string]SubtensorNodeSettings{}
	for _, node := range settings.Nodes {
		configuredNodes[node.Name] = node
	}
	seen := map[string]bool{}
	for _, node := range observation.Nodes {
		configured, ok := configuredNodes[node.Name]
		if !ok || seen[node.Name] {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor", fmt.Errorf("unexpected or duplicate node %q", node.Name)))
			continue
		}
		seen[node.Name] = true
		findings = append(findings, evaluateSubtensorNode(target, configured, node, publicHead, publicHeadErr)...)
	}
	for name := range configuredNodes {
		if !seen[name] {
			findings = append(findings, cannotObserveFinding(target.name+"/subtensor-"+name, fmt.Errorf("configured node is absent from observation")))
		}
	}
	return findings
}

func evaluateSubtensorNode(target *host, configured SubtensorNodeSettings, node subtensorNodeObservation, publicHead int64, publicHeadErr error) []finding {
	settings := target.subtensor
	findings := []finding{}
	identity := target.name + "/" + configured.Name
	firstHead, firstErr := subtensorHex(node.FirstHead)
	secondHead, secondErr := subtensorHex(node.SecondHead)
	essentialErrors := map[string]string{}
	for _, key := range []string{"chain", "genesis", "head", "runtime", "health", "sync", "second_head"} {
		if value := node.Direct.Errors[key]; value != "" {
			essentialErrors[key] = value
		}
	}
	if firstErr != nil {
		essentialErrors["head"] = firstErr.Error()
	}
	if secondErr != nil {
		essentialErrors["second_head"] = secondErr.Error()
	}
	if len(essentialErrors) > 0 {
		findings = append(findings, cannotObserveFinding(identity+"/direct-rpc", fmt.Errorf("%s", subtensorErrors(essentialErrors, nil))))
		return findings
	}

	if configured.ContainerName != "" {
		if node.ContainerError != "" {
			findings = append(findings, cannotObserveFinding(identity+"/container-identity", fmt.Errorf("%s", node.ContainerError)))
		} else {
			if !node.DataPermissionObserved {
				findings = append(findings, cannotObserveFinding(identity+"/data-permission", fmt.Errorf("runtime and bind-mount ownership are unavailable")))
			} else if !node.DataRuntimeWritable || (node.DataPermissionError && secondHead <= firstHead) {
				headAdvanced := secondHead > firstHead
				action := "Apply the committed Xops runtime-ownership repair with run-subtensor.sh and require Compose to preserve both container identities. Restore access before considering a restart. If the same framed process remains frozen for two further bounded samples after access is restored, preserve its generation and obtain explicit authorization for a service-scoped same-generation restart; recover one node at a time and do not erase it or select a new generation solely for EACCES."
				context := "This is an Xops ownership regression, not peer starvation, disk exhaustion, or evidence that the preserved database must be discarded. An already-open database can continue temporarily after path access is revoked, so RPC health is not a negative control."
				if node.DataRuntimeWritable && node.DataPermissionError && !headAdvanced {
					action = "Ownership is already repaired; do not rerun run-subtensor.sh or replace the database generation. Preserve both container identities and obtain explicit authorization for a service-scoped same-generation restart of only this framed node. Recover the archive first, prove its head advances across two bounded samples, then separately recover the lightnode while proving the archive identity and progress remain intact."
					context = "The exact runtime account can write the retained data path again, but this unchanged process still exposes the earlier RocksDB EACCES signature and does not advance. Provisioning is no longer the recovery boundary: the database process latched the background failure. This is not peer starvation, disk exhaustion, or evidence that the preserved database must be discarded."
				}
				findings = append(findings, finding{
					probeId: "subtensor/node-health", tier: tierPage, class: "subtensor-data-permission",
					target: target.name, frame: configured.Name, sustain: 1,
					symptom:   fmt.Sprintf("%s cannot safely continue writing its mounted Subtensor database", identity),
					mechanism: "The pinned image starts as root, assigns /data to its UID/GID 10001 runtime account, and then drops privilege. A later host reconciliation that resets the active bind-mount root to root:root 0750 removes traverse/write permission; a RocksDB background reopen then fails with EACCES and block import freezes even though the original process and RPC remain live.",
					baseline:  "The running node's exact runtime UID/GID has write plus traverse permission on its sole read-write /data bind mount, and the exact container generation either has no retained RocksDB permission error or advances after it.",
					observed: fmt.Sprintf(
						"runtime_uid=%d runtime_gid=%d data_uid=%d data_gid=%d data_mode=%#o runtime_writable=%t current_generation_database_permission_error=%t head_advanced=%t",
						node.RuntimeUID, node.RuntimeGID, node.DataPathUID, node.DataPathGID, node.DataPathMode,
						node.DataRuntimeWritable, node.DataPermissionError, headAdvanced,
					),
					evidence: fmt.Sprintf("container=%s data_path=%s started_at=%s", configured.ContainerName, node.DataPath, firstNonempty(node.ContainerStarted, "unknown")),
					context:  context,
					action:   action,
					verify:   "The runtime account passes test -w /data in both containers, their IDs and start times remain unchanged during permission repair, and both best heads advance across two samples. A retained permission signature clears this class only when that exact process advances; a restart, if separately authorized, must replace only the framed container and retain its data path plus the archive identity.",
					playbook: "SIGNALS.md §17.4",
				})
			}
			deploymentProblems := []string{}
			if configured.ExpectedImage != "" && node.ContainerImage != configured.ExpectedImage {
				deploymentProblems = append(deploymentProblems, fmt.Sprintf("image=%q expected=%q", node.ContainerImage, configured.ExpectedImage))
			}
			if configured.ExpectedDataPath != "" && node.DataPath != configured.ExpectedDataPath {
				deploymentProblems = append(deploymentProblems, fmt.Sprintf("data_path=%q expected=%q", node.DataPath, configured.ExpectedDataPath))
			}
			if len(deploymentProblems) > 0 {
				action := "Reconcile the digest-pinned image and data generation through the owning Subtensor playbook, then re-read the live container identity."
				if configured.SyncMode == "warp" {
					action = "After explicit operational authorization, run xops/main/ansible/run-subtensor-lightnode.sh from the committed xops revision. It must preserve old generations, recreate only subtensor-lightnode, and prove the archive container identity did not change."
				}
				findings = append(findings, finding{
					probeId: "subtensor/node-health", tier: tierWarn, class: "subtensor-deployment-drift",
					target: target.name, frame: configured.Name, sustain: 1,
					symptom:   fmt.Sprintf("%s is not running its configured image and data generation", identity),
					mechanism: "A healthy RPC or --sync argv does not prove the active container uses the release with the required consensus fixes or the intended empty generation.",
					baseline:  "The live container Config.Image and sole /data mount exactly match the configured immutable digest and generation path.",
					observed:  strings.Join(deploymentProblems, "; "),
					evidence:  fmt.Sprintf("container=%s started_at=%s", configured.ContainerName, firstNonempty(node.ContainerStarted, "unknown")),
					context:   "This is an operational deployment/storage repair; preserving failed generations is required for rollback and evidence.",
					action:    action,
					verify:    "Require exact live image and /data identities, a new container start only where intended, unchanged archive identity for an isolated lightnode repair, and all RPC convergence gates.",
					playbook:  "SIGNALS.md §17.4",
				})
			}
		}
	}

	gatewayErrors := map[string]string{}
	for _, key := range []string{"healthz", "chain", "genesis", "head"} {
		if value := node.Gateway.Errors[key]; value != "" {
			gatewayErrors[key] = value
		}
	}
	if node.GatewayHTTP != 200 || len(gatewayErrors) > 0 {
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierPage, class: "subtensor-gateway",
			target: target.name, frame: configured.Name, sustain: 2,
			symptom:   fmt.Sprintf("%s's Subtensor overlay gateway does not reproduce its direct node RPC", identity),
			mechanism: "The backing loopback RPC is observable, but nginx health or JSON-RPC is not; overlay address ordering, nginx lifecycle, or the node-specific upstream is broken.",
			baseline:  fmt.Sprintf("gateway port %d returns HTTP 200 and the same chain/genesis identity as loopback port %d", configured.GatewayPort, configured.RPCPort),
			observed:  fmt.Sprintf("gateway_http=%d errors=%s", node.GatewayHTTP, subtensorErrors(gatewayErrors, nil)),
			action:    "Check the exact overlay bind, nginx unit/journal, and this node's loopback upstream before changing chain data.",
			verify:    "Require /healthz HTTP 200 and matching direct/gateway chain, genesis, and recent head.",
			playbook:  "SIGNALS.md §17.1",
		})
	} else {
		gatewayHead, gatewayHeadErr := subtensorHex(node.Gateway.Head)
		problems := []string{}
		if node.Gateway.Chain != node.Direct.Chain {
			problems = append(problems, fmt.Sprintf("gateway chain=%q direct=%q", node.Gateway.Chain, node.Direct.Chain))
		}
		if !strings.EqualFold(node.Gateway.Genesis, node.Direct.Genesis) {
			problems = append(problems, fmt.Sprintf("gateway genesis=%q direct=%q", node.Gateway.Genesis, node.Direct.Genesis))
		}
		if gatewayHeadErr != nil || absInt64(gatewayHead-secondHead) > 128 {
			problems = append(problems, fmt.Sprintf("gateway head=%q direct=%d", node.Gateway.Head, secondHead))
		}
		if len(problems) > 0 {
			findings = append(findings, subtensorIdentityFinding(target.name, configured.Name+"-gateway", problems, node.Gateway))
		}
	}

	if problems := subtensorIdentityProblems(settings, node.Direct, false); len(problems) > 0 {
		findings = append(findings, subtensorIdentityFinding(target.name, configured.Name, problems, node.Direct))
	}
	if node.Direct.Health.Peers <= 0 {
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierWarn, class: "subtensor-peers",
			target: target.name, frame: configured.Name, sustain: 3,
			symptom:   fmt.Sprintf("%s has no retained Subtensor peer", identity),
			mechanism: "A local RPC and listener can remain healthy while a zero-peer node freezes and eventually reports its own stale head as the sync target.",
			baseline:  "Every node retains at least one peer; public inbound reachability and multiple peers are preferred.",
			observed:  fmt.Sprintf("peers=%d is_syncing=%t head=%d", node.Direct.Health.Peers, node.Direct.Health.IsSyncing, secondHead),
			action:    "Verify container DNS and bootnode TCP, then independently test the node's public P2P port and inspect incoming-connection counters.",
			verify:    "Require peers above zero and advancing heads for more than one sample; do not accept isSyncing=false alone.",
			playbook:  "SIGNALS.md §17.2",
		})
	}
	if secondHead <= firstHead {
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierWarn, class: "subtensor-progress",
			target: target.name, frame: configured.Name, sustain: 3,
			symptom:   fmt.Sprintf("%s did not advance across the bounded head sample", identity),
			mechanism: "The RPC is serving a static local database; peer loss, import failure, or resource pressure can freeze it without closing the listener.",
			baseline:  "The best head advances across the fifteen-second source-of-truth sample while the public chain advances.",
			observed:  fmt.Sprintf("first_head=%d second_head=%d peers=%d", firstHead, secondHead, node.Direct.Health.Peers),
			action:    "Inspect peer state and recent import errors, then distinguish a frozen node from an unusually long block interval with a longer sample.",
			verify:    "Require repeated head progress and a nonzero peer population.",
			playbook:  "SIGNALS.md §17.2",
		})
	}

	targetHead := node.Direct.Sync.HighestBlock
	if publicHeadErr == nil && publicHead > targetHead {
		targetHead = publicHead
	}
	lag := targetHead - secondHead
	if lag < 0 {
		lag = 0
	}
	warpMaxLag := settings.WarpMaxLag
	if warpMaxLag <= 0 {
		warpMaxLag = 4096
	}
	if configured.SyncMode == "warp" && lag > warpMaxLag {
		class := "subtensor-warp-bootstrap"
		sustain := 15
		mechanism := "The node is configured for warp sync but has not reached the near-head band. Startup evidence is required to distinguish a normal cold bootstrap from a database fallback or a historical finality-proof failure."
		evidence := fmt.Sprintf("startup_fallback=%t finality_proof_download=%t starting_block=%d image=%q data_path=%q", node.WarpFallback, node.WarpProofStarted, node.Direct.Sync.StartingBlock, node.ContainerImage, node.DataPath)
		context := "A cold warp may be behind briefly. Do not reuse or delete a failed data generation, and do not restart the full archive playbook to repair only this lightnode."
		action := "Keep the lightnode out of cutover and inspect its bounded startup log, live image provenance, /data mount, peers, and head progression before choosing a new generation."
		verify := "Require the live /data mount to equal the configured new generation, a post-rollout lightnode identity, unchanged archive container ID/start time, no cold-start warp fallback, a near-current head, nonzero peers, current runtime identity, and successful gateway RPC."
		if node.Direct.Sync.StartingBlock > 0 {
			sustain = 1
			class = "subtensor-warp-resume"
			mechanism = "The process started from an already-progressed database, so this is a same-generation resume rather than a cold warp bootstrap. The nonzero process-start block is authoritative even when the bounded startup-log helper no longer retains an early explicit fallback line. This proves a container or host lifecycle interruption after the generation had acquired state; it does not prove the original empty-generation warp failed."
			context = "This is an operational lifecycle boundary. An advancing resumed generation retains useful state; replacing it solely to remove a fallback line can discard progress and repeat the same historical checkpoint. The full-host Xops playbook must preserve an existing lightnode, while generation replacement remains isolated."
			action = "Do not reset this progressing generation solely because the process resumed retained state. Use the committed full-host lightnode-preservation guard, keep tracking head and lag slope, and select a new empty generation with run-subtensor-lightnode.sh only if progress stops or a newer proven checkpoint materially improves the recovery boundary."
			verify = "The same live /data generation and container continue advancing with nonzero peers and shrinking lag; a subsequent full-host configuration run preserves the exact lightnode ID, and any intentional replacement uses the isolated runner without changing the archive identity."
		} else if node.WarpFallback {
			sustain = 1
			class = "subtensor-warp-fallback"
			mechanism = "The startup discriminator proves Subtensor rejected a partially synced database and falls back to full sync before establishing a retained starting block. The configured command can still say --sync=warp."
			context = "This is an operational storage/deployment repair. Reusing the same partial path reproduces the failure; deleting it destroys recoverable state."
			action = "After explicit operational authorization, select the next empty generation and run xops/main/ansible/run-subtensor-lightnode.sh from the committed xops revision. It must preserve old paths and recreate only subtensor-lightnode. Do not run the full run-subtensor.sh merely to change this generation while archive progress must remain uninterrupted."
		} else if node.WarpProofStarted && secondHead <= 1 {
			class = "subtensor-warp-checkpoint"
			mechanism = "The node reached peers and entered GRANDPA finality-proof download without falling back, but remained at genesis. This is the testnet historical-checkpoint failure reproduced with v447, which predates the corrected checkpoint transition and signing sets in v448."
			context = "This is a pinned-node-binary defect plus an operational generation change, not a Grafana exporter error, peer-install failure, or reason to erase either failed database."
			action = "Pin an attested upstream release containing commits add2b31a19ccf650ad50d79e8ba2668e6494f56f and 0876234316a3b9107ce1eb0781b04ae55f5df89e, select the next empty generation, and deploy only with xops/main/ansible/run-subtensor-lightnode.sh."
		}
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierWarn, class: class,
			target: target.name, frame: configured.Name, sustain: sustain,
			symptom:   fmt.Sprintf("%s is configured for warp sync but remains %d blocks behind", identity, lag),
			mechanism: mechanism,
			baseline:  fmt.Sprintf("A warp node reaches within %d blocks of the public/reference head after bootstrap", warpMaxLag),
			observed:  fmt.Sprintf("sync_mode=%s starting_block=%d current_head=%d target_head=%d lag=%d peers=%d is_syncing=%t", configured.SyncMode, node.Direct.Sync.StartingBlock, secondHead, targetHead, lag, node.Direct.Health.Peers, node.Direct.Health.IsSyncing),
			evidence:  evidence,
			context:   context,
			action:    action,
			verify:    verify,
			playbook:  "SIGNALS.md §17.4",
		})
	} else if lag > 128 || node.Direct.Health.IsSyncing {
		class := "subtensor-sync-lag"
		mechanism := "The node is importing historical blocks and is healthy for bootstrap only while peers remain nonzero and the head advances; it is not ready for cutover."
		if !node.Direct.Health.IsSyncing && lag > 128 {
			class = "subtensor-stale-convergence"
			mechanism = "The node says synchronization is complete while the external/reference head is materially ahead; a zero-peer stale database can make highestBlock collapse to currentBlock."
		}
		findings = append(findings, finding{
			probeId: "subtensor/node-health", tier: tierWarn, class: class,
			target: target.name, frame: configured.Name, sustain: 1,
			symptom:   fmt.Sprintf("%s is %d blocks behind the Subtensor target", identity, lag),
			mechanism: mechanism,
			baseline:  "Cutover requires isSyncing=false, a near-reference head, current runtime/transaction identity, EVM chain identity, and eth_getLogs.",
			observed:  fmt.Sprintf("current_head=%d target_head=%d lag=%d peers=%d is_syncing=%t runtime=%d", secondHead, targetHead, lag, node.Direct.Health.Peers, node.Direct.Health.IsSyncing, node.Direct.Runtime.SpecVersion),
			context:   "Archive full-sync lag is an expected operational wait while progress and peers remain healthy; it is not an exporter defect.",
			action:    "Keep the node out of cutover and track lag slope. Intervene only if progress/peers fail or measured catch-up no longer converges.",
			verify:    "Require the lag to reach the configured near-head band and every current-runtime interface gate to pass.",
			playbook:  "SIGNALS.md §17.2",
		})
	} else {
		currentProblems := []string{}
		if node.Direct.Runtime.SpecVersion != settings.ExpectedSpecVersion {
			currentProblems = append(currentProblems, fmt.Sprintf("specVersion=%d expected=%d", node.Direct.Runtime.SpecVersion, settings.ExpectedSpecVersion))
		}
		if node.Direct.Runtime.TransactionVersion != settings.ExpectedTransactionVersion {
			currentProblems = append(currentProblems, fmt.Sprintf("transactionVersion=%d expected=%d", node.Direct.Runtime.TransactionVersion, settings.ExpectedTransactionVersion))
		}
		if !strings.EqualFold(node.Direct.EVMChainID, settings.ExpectedEVMChainID) {
			currentProblems = append(currentProblems, fmt.Sprintf("evm_chain_id=%q expected=%q", node.Direct.EVMChainID, settings.ExpectedEVMChainID))
		}
		if !node.Direct.EthGetLogs {
			currentProblems = append(currentProblems, "eth_getLogs=unavailable")
		}
		if len(currentProblems) > 0 {
			findings = append(findings, subtensorIdentityFinding(target.name, configured.Name+"-current-runtime", currentProblems, node.Direct))
		}
	}
	return findings
}

func subtensorIdentityProblems(settings *SubtensorHostSettings, observed subtensorRPCObservation, includeCurrentRuntime bool) []string {
	problems := []string{}
	if observed.Chain != settings.ExpectedChain {
		problems = append(problems, fmt.Sprintf("chain=%q expected=%q", observed.Chain, settings.ExpectedChain))
	}
	if !strings.EqualFold(observed.Genesis, settings.ExpectedGenesisHash) {
		problems = append(problems, fmt.Sprintf("genesis=%q expected=%q", observed.Genesis, settings.ExpectedGenesisHash))
	}
	if observed.Runtime.SpecName != settings.ExpectedSpecName {
		problems = append(problems, fmt.Sprintf("specName=%q expected=%q", observed.Runtime.SpecName, settings.ExpectedSpecName))
	}
	if includeCurrentRuntime && observed.Runtime.SpecVersion != settings.ExpectedSpecVersion {
		problems = append(problems, fmt.Sprintf("specVersion=%d expected=%d", observed.Runtime.SpecVersion, settings.ExpectedSpecVersion))
	}
	if includeCurrentRuntime && observed.Runtime.TransactionVersion != settings.ExpectedTransactionVersion {
		problems = append(problems, fmt.Sprintf("transactionVersion=%d expected=%d", observed.Runtime.TransactionVersion, settings.ExpectedTransactionVersion))
	}
	if includeCurrentRuntime && !strings.EqualFold(observed.EVMChainID, settings.ExpectedEVMChainID) {
		problems = append(problems, fmt.Sprintf("evm_chain_id=%q expected=%q", observed.EVMChainID, settings.ExpectedEVMChainID))
	}
	return problems
}

func subtensorIdentityFinding(hostName, frame string, problems []string, observed subtensorRPCObservation) finding {
	return finding{
		probeId: "subtensor/node-health", tier: tierPage, class: "subtensor-identity",
		target: hostName, frame: frame, sustain: 1,
		symptom:   "A Subtensor RPC surface does not match the configured chain or runtime identity",
		mechanism: "A reachable RPC can be the wrong chain, stale deployment, or wrong gateway upstream; reachability alone is not a safe cutover gate.",
		baseline:  "Chain name, genesis hash, runtime name, and—at the current head—runtime/transaction/EVM identities match the pinned deployment.",
		observed:  strings.Join(problems, "; "),
		evidence:  fmt.Sprintf("chain=%q genesis=%q runtime=%s/%d transaction=%d evm=%q", observed.Chain, observed.Genesis, observed.Runtime.SpecName, observed.Runtime.SpecVersion, observed.Runtime.TransactionVersion, observed.EVMChainID),
		action:    "Compare the direct node, overlay gateway, public reference, rendered Compose digest, and active xops vars before changing data or traffic.",
		verify:    "Repeat every identity method through direct and gateway RPC and require exact configured values at convergence.",
		playbook:  "SIGNALS.md §17.1",
	}
}

func subtensorHex(raw string) (int64, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 3 || !strings.HasPrefix(raw, "0x") {
		return 0, fmt.Errorf("invalid hex block %q", raw)
	}
	value, err := strconv.ParseInt(raw[2:], 16, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid hex block %q", raw)
	}
	return value, nil
}

func subtensorErrors(errors map[string]string, extra error) string {
	parts := make([]string, 0, len(errors)+1)
	for key, value := range errors {
		parts = append(parts, key+"="+value)
	}
	if extra != nil {
		parts = append(parts, extra.Error())
	}
	sort.Strings(parts)
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "; ")
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

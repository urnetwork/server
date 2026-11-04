// A worker may request one owned network proof, never a host namespace/PID or
// command. Kernel bridge facts are read only inside the pinned private engine.
package competitionagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
)

// Both the daemon object and the private namespace must match the immutable
// run. No host-network ip command or default-Docker fallback is available.
func (self *DockerProxy) proveHostNetwork(ctx context.Context, networkId string) (HostNetworkBoundary, error) {
	if !digestPattern.MatchString(networkId) || self.plan.HostFiles == nil {
		return HostNetworkBoundary{}, errors.New("network proof has no complete owned identity")
	}
	owned, err := self.inspectOwnedDocker(ctx, networkId, true)
	if err != nil || owned != networkId {
		return HostNetworkBoundary{}, errors.New("network proof belongs to another run")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/networks/"+networkId, nil)
	if err != nil {
		return HostNetworkBoundary{}, err
	}
	response, err := self.transport.RoundTrip(request)
	if err != nil {
		return HostNetworkBoundary{}, err
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, self.settings.MaximumDocumentBytes+1))
	if err != nil || response.StatusCode != http.StatusOK || int64(len(content)) > self.settings.MaximumDocumentBytes || validateDockerMutationJson(content, self.settings.MaximumDocumentDepth) != nil {
		return HostNetworkBoundary{}, errors.New("owned network observation is unavailable")
	}
	bridge, err := validateHostNetworkInspection(content, self.plan, networkId)
	if err != nil {
		return HostNetworkBoundary{}, err
	}
	scope := RuntimeResourceScope{BaseDirectory: self.plan.BaseDirectory, TrustedOperatorUid: self.plan.TrustedOperatorUid,
		StateDirectory: filepath.Dir(filepath.Dir(self.plan.StateDirectory)), WorkDirectory: filepath.Dir(self.plan.ContextRoot),
		ScratchDirectory: filepath.Dir(self.plan.ScratchDirectory), WorkerUid: self.plan.WorkerUid, WorkerGid: self.plan.HostFiles.WorkerGid}
	settings := DefaultRuntimeResourceBoundarySettings()
	boundary, err := VerifyRuntimeResourceBoundary(ctx, self.plan.HostFiles.ResourceBoundary, scope, settings)
	if err != nil {
		return HostNetworkBoundary{}, err
	}
	defer boundary.Close()
	if boundary.PrivateSocketPath != self.plan.UpstreamSocketPath || boundary.WorkDeviceId != self.plan.HostFiles.WorkDeviceId {
		return HostNetworkBoundary{}, errors.New("network proof changed its admitted private runtime")
	}
	link, err := (runtimeBoundaryNativeHost{}).NetworkLink(ctx, *boundary, bridge, settings)
	if err != nil || validateHostNetworkLink(link, bridge) != nil {
		return HostNetworkBoundary{}, errors.New("private bridge has an unqualified kernel address")
	}
	return HostNetworkBoundary{Schema: Schema, NetworkId: networkId, BridgeName: bridge, PrivateNamespace: true,
		IsolatedGateway: true, LinkSha256: digestDockerBytes(link)}, nil
}

// Labels alone cannot turn a host-addressed or external network into a safe
// per-run bridge. Every gateway/driver field remains positively constrained.
func validateHostNetworkInspection(content []byte, plan DockerProxyPlan, networkId string) (string, error) {
	var object struct {
		Id, Driver, Scope string
		Internal          *bool
		EnableIPv6        *bool
		Attachable        *bool
		Ingress           *bool
		Labels            map[string]string
		Options           map[string]string
	}
	if !digestPattern.MatchString(plan.RunId) || !digestPattern.MatchString(networkId) || json.Unmarshal(content, &object) != nil || object.Id != networkId ||
		object.Driver != "bridge" || object.Scope != "local" || object.Internal == nil || !*object.Internal || object.EnableIPv6 == nil || *object.EnableIPv6 ||
		object.Attachable == nil || *object.Attachable || object.Ingress == nil || *object.Ingress ||
		object.Labels[cleanupJobLabel] != plan.JobId || object.Labels[cleanupRoundLabel] != plan.RoundId || object.Labels[cleanupRunLabel] != plan.RunId ||
		object.Options["com.docker.network.bridge.gateway_mode_ipv4"] != "isolated" || object.Options["com.docker.network.bridge.gateway_mode_ipv6"] != "isolated" ||
		object.Options["com.docker.network.bridge.name"] != "unev-"+plan.RunId[:10] {
		return "", errors.New("network object is not the exact private isolated run bridge")
	}
	return object.Options["com.docker.network.bridge.name"], nil
}

// An isolated bridge has no IPv4/global address. Link-local IPv6 is harmless
// here; containers have IPv6 disabled, and no external namespace link exists.
func validateHostNetworkLink(content []byte, bridge string) error {
	var links []struct {
		Name      string `json:"ifname"`
		Addresses []struct {
			Family       string `json:"family"`
			Local        string `json:"local"`
			Scope        string `json:"scope"`
			PrefixLength int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if json.Unmarshal(content, &links) != nil || len(links) != 1 || links[0].Name != bridge || links[0].Addresses == nil {
		return errors.New("private bridge kernel identity is absent or ambiguous")
	}
	for _, address := range links[0].Addresses {
		ip := net.ParseIP(address.Local)
		if address.Family != "inet6" || address.Scope != "link" || address.PrefixLength != 64 || ip == nil || ip.To4() != nil || !ip.IsLinkLocalUnicast() {
			return errors.New("private bridge retains a host-reachable address")
		}
	}
	return nil
}

// The public client accepts only the exact finite projection returned for its
// own run/network. Raw tool output cannot supply an alternate namespace.
func validateHostNetworkProof(proof HostNetworkBoundary, runId, networkId string) error {
	if !digestPattern.MatchString(runId) || !digestPattern.MatchString(networkId) || proof.Schema != Schema || proof.NetworkId != networkId ||
		proof.BridgeName != "unev-"+runId[:10] || !proof.PrivateNamespace || !proof.IsolatedGateway || !digestPattern.MatchString(proof.LinkSha256) {
		return errors.New("private network proof belongs to another boundary")
	}
	return nil
}

// The URL probe is the only pass whose tunnels keep their clients' unread
// identity keys local. The pass is built without running it, so nothing
// reaches the operator.
package work

import (
	"testing"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/qualityprobe/ingest"
	"github.com/urnetwork/server/qualityprobe/providertunnel"
)

// Root cause: the URL probe's derived clients published identity keys nothing
// reads. Its pass opts every tunnel in, over whatever config its caller built.
func TestProviderUrlProbePassKeepsClientKeysLocal(t *testing.T) {
	args := providerEgressProbeArgs(defaultProviderEgressProbeSettings("example.test"), 0)
	operator := &ingest.Client{ServerUrl: "https://operator.example", OperatorSecret: "synthetic", ShardIndex: args.ShardIndex, ShardCount: args.ShardCount}
	pass := newProviderUrlProbePass(args, operator, providertunnel.Config{ApiUrl: "https://api.example"}, server.NewId())
	if !pass.fullOptions.TunnelConfig.KeepClientKeysLocal {
		t.Error("the URL probe's tunnels publish their clients' unread keys")
	}
	if pass.fullOptions.TunnelConfig.ApiUrl != "https://api.example" {
		t.Error("the opt-in replaced the caller's tunnel config")
	}
}

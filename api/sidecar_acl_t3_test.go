//go:build sidecar_localstack && sidecar_pending

package api_test

// §22.7's policy canaries on T3 (ACL-M2-16; §8.7, owner decision 72, R-ACL-12, X2, I-18): the T2 test's document of
// canaries (sidecartest.AdmissionNet.SaveAclCanaryPolicy), saved on a T3 Net through server's real sequencer with api's
// own read-cache script (model.NewConfigLogRedisEntryCache) on the private Redis and its network_acl_head advance
// (model.AdvanceNetworkAclHead) on the private Postgres, over the Net's metadata cluster on the local stack's MinIO.
// sidecart3.CaptureStores adds every object, Redis key and sidecar table row to the Net's capture, network_acl_head and
// network_acl_ack among them (local/sidecar-columns.yml), and the scan finds no canary, tag, group or K_sock in any
// form. It needs MG2's tables (sidecar_pending) and the local stack (local/stack.sh up), and fails rather than skips
// without them.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

func TestAclPolicyCanariesReachNoStoreOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		net := sidecartest.NewNet(ctx, sidecart3.NetSettings(tb, nil))
		defer net.Close()
		report, err := sidecart3.CaptureStores(net, nil)
		if err != nil {
			tb.Fatal(err)
		}
		tier, err := net.AdmissionTier()
		if err != nil {
			tb.Fatal(err)
		}
		tier.AclCompile = sidecartest.AclAdmissionCompile
		tier.NewApi = sidecart3.ConfigLogApiWith(func(settings *model.ConfigLogSettings) {
			settings.EntryCache = model.NewConfigLogRedisEntryCache(nil)
		}, model.AdvanceNetworkAclHead)
		admission := sidecartest.NewAdmissionNet(tb, tier)
		defer admission.Close()
		admission.SaveAclCanaryPolicy(net.Capture())

		// api advanced the network's head to the tip, with the document, as its append path does
		head, err := model.GetNetworkAclHead(ctx, server.Id(admission.NetworkId()))
		if err != nil {
			tb.Fatal(err)
		}
		if head == nil || head.Version != admission.Tip().GetVersion() || !head.HasDocument {
			tb.Fatalf("network_acl_head: %+v, the tip %d", head, admission.Tip().GetVersion())
		}

		for _, finding := range net.Capture().Scan() {
			tb.Errorf("%s", finding)
		}
		objects, redisKeys, rows := report.Counts()
		// the log's proj/ and projrows/ objects, {acl_}e and {acl_}v, and the network's head row at least
		if objects < 12 || redisKeys < 2 || rows < 1 {
			tb.Errorf("the scan read %d objects, %d Redis keys and %d rows", objects, redisKeys, rows)
		}
		for _, err := range report.Errors() {
			tb.Errorf("not read: %v", err)
		}
		tb.Logf("scanned %d objects, %d Redis keys and %d rows; tables not read: %v", objects, redisKeys, rows, report.TablesNotRead())
	})
}

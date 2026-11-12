//go:build sidecar_localstack && sidecar_pending

package api_test

// §22.7's policy canaries on T3 (ACL-M2-16; §8.7, owner decision 72, R-ACL-12, X2, I-18): the T2 test's document of
// canaries (sidecartest.AdmissionNet.SaveAclCanaryPolicy), saved on a T3 Net through server's real sequencer with api's
// own read-cache script (model.NewConfigLogRedisEntryCache) on the private Redis and its network_acl_head advance
// (model.AdvanceNetworkAclHead) on the private Postgres, over the Net's metadata cluster on the local stack's MinIO.
// sidecart3.CaptureStores adds every object, Redis key and sidecar table row to the Net's capture, network_acl_head and
// network_acl_ack among them (local/sidecar-columns.yml), and the scan finds no canary, tag, group or K_sock in any
// form, and under sidecar_leaktest the same note leaked into a sidecar table through Postgres's own leak is found. It
// needs MG2's tables (sidecar_pending) and the local stack (local/stack.sh up), and fails rather than skips without
// them.

import (
	"testing"

	"github.com/urnetwork/sidecar/sidecartest"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
	"github.com/urnetwork/server/sidecart3"
)

// A T3 Net whose stores CaptureStores reads, and on its metadata cluster the canary document saved through server's
// sequencer with api's read-cache script and network_acl_head advance. The caller closes both.
func aclT3CanaryWorld(tb testing.TB) (*sidecartest.Net, *sidecart3.CaptureStoresReport, *sidecartest.AdmissionNet, *sidecartest.AclCanaryPolicy) {
	net := sidecartest.NewNet(tb.Context(), sidecart3.NetSettings(tb, nil))
	report, err := sidecart3.CaptureStores(net, nil)
	if err != nil {
		net.Close()
		tb.Fatal(err)
	}
	tier, err := net.AdmissionTier()
	if err != nil {
		net.Close()
		tb.Fatal(err)
	}
	tier.AclCompile = sidecartest.AclAdmissionCompile
	tier.NewApi = sidecart3.ConfigLogApiWith(func(settings *model.ConfigLogSettings) {
		settings.EntryCache = model.NewConfigLogRedisEntryCache(nil)
	}, model.AdvanceNetworkAclHead)
	admission := sidecartest.NewAdmissionNet(tb, tier)
	return net, report, admission, admission.SaveAclCanaryPolicy(net.Capture())
}

func TestAclPolicyCanariesReachNoStoreOnT3(t *testing.T) {
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		ctx := tb.Context()
		net, report, admission, _ := aclT3CanaryWorld(tb)
		defer net.Close()
		defer admission.Close()

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

// Under sidecar_leaktest: Postgres's own leak (sidecart3's capture_stores_leak.go) carries the saved policy's note into a
// sidecar table, and CaptureStores' scan finds it there, so the clean scan above reads what a server bug would write.
func TestAclPolicyCanaryLeakedIntoPostgresIsCaughtOnT3(t *testing.T) {
	leak := sidecartest.LeakClasses()[sidecartest.PrivacyClassPostgres]
	if !sidecartest.LeaksLinked || leak == nil {
		t.Skip("the leaks link only under sidecar_leaktest, which test.sh's leak leg builds")
	}
	server.DefaultTestEnv().Run(t, func(tb testing.TB) {
		net, _, admission, policy := aclT3CanaryWorld(tb)
		defer net.Close()
		defer admission.Close()
		if findings := net.Capture().Scan(); len(findings) != 0 {
			tb.Fatalf("findings before the leak: %v", findings)
		}
		leak.Put(tb, net, []byte(policy.Note))
		for _, finding := range net.Capture().Scan() {
			if finding.Needle == "acl policy note" && finding.Class == sidecartest.PrivacyClassPostgres {
				return
			}
		}
		tb.Error("a policy note leaked into Postgres went unseen")
	})
}

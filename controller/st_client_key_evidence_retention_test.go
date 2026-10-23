// The client-key history expiration reaches exactly the run-less history of
// the two content-only kinds for the configured deployment, and joins the
// bucket's complete code-owned rule set without changing any other rule.
package controller

import (
	"bytes"
	"context"
	"encoding/xml"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7/pkg/lifecycle"
	"github.com/urnetwork/server"
	"github.com/urnetwork/server/startifact"
)

func stClientKeyEvidenceRetentionConfigTest(t *testing.T) *StConfig {
	t.Helper()
	config := &StConfig{DeploymentId: "test-deployment", Netuid: 7}
	SetStConfig(config)
	t.Cleanup(func() { SetStConfig(nil) })
	return config
}

// One rule per content-only kind, ending in that kind's run-less directory:
// it covers the kind's history copies and nothing on the content route, under
// another kind, or under another deployment or netuid.
func TestStClientKeyEvidenceHistoryRulesScopeExactPrefixes(t *testing.T) {
	config := stClientKeyEvidenceRetentionConfigTest(t)
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	rules := stClientKeyEvidenceHistoryLifecycleRules(store)
	kinds := startifact.EvidenceHistoryUnindexedKinds()
	if len(rules) != len(kinds) {
		t.Fatalf("rules = %+v, want one per content-only kind", rules)
	}
	contentHash := "sha256:" + strings.Repeat("ab", 32)
	contentKey, err := startifact.EvidenceContentKey(store, contentHash)
	if err != nil {
		t.Fatal(err)
	}
	for index, kind := range kinds {
		rule := rules[index]
		want, err := startifact.EvidenceHistoryRunPrefix(store, config.DeploymentId, uint16(config.Netuid), kind, startifact.EvidenceDeploymentHistoryRunID)
		if err != nil {
			t.Fatal(err)
		}
		if rule.KeyPrefix != want || rule.TTL != 24*time.Hour {
			t.Fatalf("%s rule = %+v, want prefix %s and one day", kind, rule, want)
		}
		covered, err := startifact.EvidenceHistoryKey(store, config.DeploymentId, uint16(config.Netuid), kind, startifact.EvidenceDeploymentHistoryRunID, contentHash)
		if err != nil || !strings.HasPrefix(covered, rule.KeyPrefix) {
			t.Fatalf("%s rule misses its own history copy %s (%v)", kind, covered, err)
		}
		outside := []string{contentKey}
		for _, other := range []struct {
			deployment string
			netuid     uint16
			kind       string
			runId      string
		}{
			{config.DeploymentId, uint16(config.Netuid), "deployment-manifest", startifact.EvidenceDeploymentHistoryRunID},
			{config.DeploymentId, uint16(config.Netuid), "scenario-result", "run-1"},
			{config.DeploymentId, uint16(config.Netuid), kind, startifact.EvidenceLegacyDeploymentHistoryRunID},
			{config.DeploymentId, 8, kind, startifact.EvidenceDeploymentHistoryRunID},
			{config.DeploymentId + "-2", uint16(config.Netuid), kind, startifact.EvidenceDeploymentHistoryRunID},
			{config.DeploymentId, uint16(config.Netuid), kind + "x", startifact.EvidenceDeploymentHistoryRunID},
		} {
			key, err := startifact.EvidenceHistoryKey(store, other.deployment, other.netuid, other.kind, other.runId, contentHash)
			if err != nil {
				t.Fatal(err)
			}
			outside = append(outside, key)
		}
		for _, key := range outside {
			if strings.HasPrefix(key, rule.KeyPrefix) {
				t.Fatalf("%s rule %s reaches %s", kind, rule.KeyPrefix, key)
			}
		}
	}
}

// Without an st configuration or a valid netuid there is no deployment to
// scope, so no rule is produced.
func TestStClientKeyEvidenceHistoryRulesNeedAConfiguredDeployment(t *testing.T) {
	SetStConfig(nil)
	store := server.NewLocalBlobStore(t.TempDir(), "blob")
	if rules := stClientKeyEvidenceHistoryLifecycleRules(store); len(rules) != 0 {
		t.Fatalf("no st config produced %+v", rules)
	}
	for _, netuid := range []uint64{0, 1 << 16} {
		SetStConfig(&StConfig{DeploymentId: "test-deployment", Netuid: netuid})
		if rules := stClientKeyEvidenceHistoryLifecycleRules(store); len(rules) != 0 {
			t.Fatalf("netuid %d produced %+v", netuid, rules)
		}
	}
	SetStConfig(nil)
	if rules := stClientKeyEvidenceHistoryLifecycleRules(nil); len(rules) != 0 {
		t.Fatalf("no store produced %+v", rules)
	}
}

// The taskworker's single bucket write carries the client-key history rules
// next to the stats and feedback rules and keeps a foreign rule byte for byte.
func TestApplyBlobRetentionAddsClientKeyEvidenceHistoryRules(t *testing.T) {
	config := stClientKeyEvidenceRetentionConfigTest(t)
	fake := newFakeMinio(t)
	foreign := lifecycle.Rule{ID: "synthetic-operator-rule", Status: "Enabled", RuleFilter: lifecycle.Filter{Prefix: "operator-archive/"}, Expiration: lifecycle.Expiration{Days: 31}}
	initial := lifecycle.NewConfiguration()
	initial.Rules = []lifecycle.Rule{foreign}
	raw, err := xml.Marshal(initial)
	if err != nil {
		t.Fatal(err)
	}
	fake.lifecycleConfigs = map[string][]byte{"blob": raw}
	pop := server.Vault.PushSimpleResource("minio.yml", []byte(fakeMinioYml(fake, "blob")))
	defer pop()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	ApplyBlobRetention(ctx)
	writes := fake.Writes()
	if len(writes) != 1 {
		t.Fatalf("retention wrote %d configurations, want one complete write", len(writes))
	}
	rules := blobRetentionRulesTest(t, writes[0].body)
	env, _ := server.Env()
	if rules["urnetwork-ttl-logs"].RuleFilter.Prefix != "logs/" || rules["urnetwork-ttl-blob-"+strings.ReplaceAll(env, "/", "-")+"-findproviders2"].RuleFilter.Prefix != "blob/"+env+"/findproviders2/" {
		t.Fatal("the write lost a stats or feedback rule", rules)
	}
	for _, kind := range startifact.EvidenceHistoryUnindexedKinds() {
		prefix := "blob/st/v1/evidence/history/" + config.DeploymentId + "/7/" + kind + "/" + startifact.EvidenceDeploymentHistoryRunID + "/"
		rule, ok := rules["urnetwork-ttl-"+strings.ReplaceAll(strings.Trim(prefix, "/"), "/", "-")]
		if !ok || rule.RuleFilter.Prefix != prefix || rule.Expiration.Days != 1 || rule.Status != "Enabled" {
			t.Fatalf("%s history rule = %+v (present %t), want enabled one-day expiration of %s", kind, rule, ok, prefix)
		}
	}
	before, _ := xml.Marshal(foreign)
	after, _ := xml.Marshal(rules[foreign.ID])
	if !bytes.Equal(before, after) || len(rules) != 5 {
		t.Fatalf("the write changed the foreign rule or carried an unexpected rule: %d rules", len(rules))
	}
}

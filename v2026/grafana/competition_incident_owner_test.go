// The competition alert rules live in warp (grafana/alerting/competition.yml) and name their
// incident owner in an owner label. Neither module depends on the other, so warp's
// TestCompetitionAlertRulesRetainIncidentOwner holds the labels to its own copy of the contact,
// and these tests hold the contact to warp.
package grafana

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Runs in any checkout. A new contact is a change to warp's rules and its copy in the same move.
func TestCompetitionIncidentContactMatchesWarpCopy(t *testing.T) {
	if CompetitionIncidentContact != "support@ur.xyz" {
		t.Fatalf("incident contact %q differs from warp's copy in grafana/competition_owner_test.go and its owner labels in grafana/alerting/competition.yml", CompetitionIncidentContact)
	}
}

// Compares with the rules themselves when warp is checked out beside server, as at the mono root.
// Without warp there is nothing to compare, and the copy above is what remains.
func TestCompetitionIncidentContactIsWarpRuleOwner(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "warp", "grafana", "alerting", "competition.yml"))
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("warp is not checked out beside server")
	}
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Groups []struct {
			Rules []struct {
				Uid    string            `yaml:"uid"`
				Labels map[string]string `yaml:"labels"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal(body, &document); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, group := range document.Groups {
		for _, rule := range group.Rules {
			count++
			if rule.Labels["owner"] != CompetitionIncidentContact {
				t.Errorf("warp rule %s owner=%q, want %q", rule.Uid, rule.Labels["owner"], CompetitionIncidentContact)
			}
		}
	}
	if count == 0 {
		t.Fatal("warp's competition alert file holds no rules")
	}
}

// Baseline provenance and private-executor admission are independent exact
// qualifications on the same local physical host, not interchangeable hashes.
package controller

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/urnetwork/server/v2026/model"
)

func TestCompetitionDistributedBaselineAndPrivateExecutorQualificationsRemainDistinct(t *testing.T) {
	settings := validSettings()
	settings.DistributedEvaluationPolicy = competitionDistributedSlotTestPolicy(t)
	settings.distributedRoundEpoch = 2
	settings.EvaluationPolicy.HostQualificationSha256 = strings.Repeat("c", 64)
	check := passingHostCheck(settings)
	for _, host := range settings.DistributedEvaluationPolicy.Hosts {
		if host.Transport == "local" {
			check.HostId = host.HostId
			if host.QualificationSha256 == check.QualificationSha256 {
				t.Fatal("synthetic baseline and private fingerprints must differ")
			}
		}
	}
	if err := validateCompetitionDistributedBaselineHost(settings, check); err != nil {
		t.Fatal("distinct exact baseline/private qualifications blocked cold-round preparation", err)
	}
	for _, change := range []func(*HostSelfCheck){
		func(value *HostSelfCheck) { value.QualificationSha256 = strings.Repeat("d", 64) },
		func(value *HostSelfCheck) { value.HostId = "another-physical-host.example" },
		func(value *HostSelfCheck) { value.ImageDigest = "sha256:" + strings.Repeat("e", 64) },
		func(value *HostSelfCheck) { value.CleanupVerified = false },
	} {
		changed := check
		change(&changed)
		if validateCompetitionDistributedBaselineHost(settings, changed) == nil {
			t.Fatal("baseline producer lost its exact frozen/physical containment gate")
		}
	}
}

func TestCompetitionDistributedBaselineProducerRetainsIndependentSealedDependency(t *testing.T) {
	dependencies, program, err := roundBaselinePreparationPolicyDependencies(model.CompetitionG2VolumePolicyMinimumWork)
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(filepath.Join("..", "connect", "sim-latency", "evaluator", "container", program))
	if err != nil {
		t.Fatal(err)
	}
	pattern := regexp.MustCompile(`(?m)^    \[ "\$\(sha256sum -- "\$source" \| awk '\{print \$1\}'\)" = ([0-9a-f]{64}) \] \|\| return 1$`)
	matches := pattern.FindAllSubmatch(content, -1)
	if len(matches) != 1 || string(matches[0][1]) != dependencies["evaluator-minimum-work.sh"] {
		t.Fatal("baseline producer was rebound to private-executor helper bytes without an independently sealed control release")
	}
}

// Execute the actual host policy with synthetic service names, without scanning
// the host, restarting services or loading operational credentials.
package main

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// The external interpreter is required by needrestart's configuration format;
// all test orchestration and assertions remain in Go.
func checkCompetitionNeedrestartPolicy(t *testing.T, services []string, expected string) {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the checked-in restart policy")
	}
	policy := filepath.Join(filepath.Dir(source), "evaluator", "needrestart-competition.conf")
	script := `
use strict;
use warnings;
our %nrconf;
$nrconf{override_rc} = { qr(^existing-example[.]service$) => 1 };
my $policy = shift @ARGV;
my $expected = shift @ARGV;
do $policy or die "cannot load policy: $@ $!";
my $preserved = 0;
for my $pattern (keys %{$nrconf{override_rc}}) {
  $preserved = 1 if 'existing-example.service' =~ $pattern && $nrconf{override_rc}->{$pattern} == 1;
}
die "existing policy was replaced" unless $preserved;
die "global restart mode changed" if exists $nrconf{restart};
for my $service (@ARGV) {
  my @matches = grep { $service =~ $_ && $nrconf{override_rc}->{$_} == 0 } keys %{$nrconf{override_rc}};
  die "wrong restart selection for $service" if (($expected eq 'defer') != !!@matches);
}
`
	arguments := append([]string{"-e", script, policy, expected}, services...)
	command := exec.CommandContext(t.Context(), "perl", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("restart policy rejected: %v\n%s", err, output)
	}
}

// Each durable runtime and transient build namespace is excluded, including
// the network guard and containerd behind the private Docker engine.
func TestCompetitionNeedrestartPolicyDefersCompetitionServices(t *testing.T) {
	checkCompetitionNeedrestartPolicy(t, []string{
		"urnetwork-competition-example.service",
		"urnetwork-subeval-example.service",
		"urnetwork-private-engine.service",
		"urnetwork-private-engine-network.service",
		"urnetwork-private-engine-network-namespace.service",
		"urnetwork-private-containerd.service",
		"competition-example-build.service",
	}, "defer")
}

// Anchor the namespaces and retain unrelated restart choices/security updates.
func TestCompetitionNeedrestartPolicyPreservesUnrelatedServices(t *testing.T) {
	checkCompetitionNeedrestartPolicy(t, []string{
		"unrelated-example.service",
		"existing-example.service",
		"other-competition-example.service",
		"urnetwork-private-unrelated.service",
		"urnetwork-private-engine.service.extra",
		"competition-example.timer",
	}, "ordinary")
}

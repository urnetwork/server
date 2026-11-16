package main

// §19.11's escrow_draw_order rule in every gate's report (OPS-M6-01; D-OPS-10, §20.9, D-QUOTA-9): the switch turns
// on only once M6's Gn-out registers MG6, whose transfer_balance team_credit index every draw and return site reads,
// and never off while a team_credit balance is in window. Each gate reads every release env's
// config/<env>/sidecar.yml in the config checkout and fails an env whose escrow_draw_order is on while this build
// links no MG6 marker (server.SidecarMg6Linked: the code migration that ends MG6, in the migrations slice only once
// M6's Gn-out promoted the group). The gate tool runs as a release leg, without the sidecar_pending tag, so it links
// what a release links; local and canary set the tag and are not read. The rule's other half, never off while a
// team_credit balance is in window, is the owner's, since only api's rows say whether one is
// (xops/main/ansible/SIDECAR-RECOVERY.md).

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"gopkg.in/yaml.v3"
)

// The envs whose builds set the sidecar_pending tag, which a release's rule does not read.
var escrowGatePendingEnvs = []string{"local", "canary"}

// The one switch the rule reads, as sidecar.yml writes it.
type escrowGateSidecarYaml struct {
	Sidecar struct {
		EscrowDrawOrder struct {
			Enabled bool `yaml:"enabled"`
		} `yaml:"escrow_draw_order"`
	} `yaml:"sidecar"`
}

// The rule's result over configRoot, the config checkout, for a build whose MG6 link linked reports.
func escrowDrawOrderCheck(configRoot string, linked func() bool) *gateResult {
	result := &gateResult{label: "escrow_draw_order on only once MG6 is registered (§19.11)"}
	entries, err := os.ReadDir(configRoot)
	if err != nil {
		result.outcome = gateOpen
		result.details = append(result.details, "no config checkout here: "+err.Error())
		return result
	}
	failed := false
	for _, entry := range entries {
		if !entry.IsDir() || slices.Contains(escrowGatePendingEnvs, entry.Name()) {
			continue
		}
		path := filepath.Join(configRoot, entry.Name(), "sidecar.yml")
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			failed = true
			result.details = append(result.details, fmt.Sprintf("%s: %v", entry.Name(), err))
			continue
		}
		sidecarYaml := &escrowGateSidecarYaml{}
		if err := yaml.Unmarshal(data, sidecarYaml); err != nil {
			failed = true
			result.details = append(result.details, fmt.Sprintf("%s: sidecar.yml does not parse: %v", entry.Name(), err))
			continue
		}
		if !sidecarYaml.Sidecar.EscrowDrawOrder.Enabled {
			result.details = append(result.details, entry.Name()+": off")
			continue
		}
		if !linked() {
			failed = true
			result.details = append(result.details, entry.Name()+": on before M6's Gn-out registered MG6's index; turn it off")
			continue
		}
		result.details = append(result.details, entry.Name()+": on, MG6 registered")
	}
	result.outcome = gatePass
	if failed {
		result.outcome = gateFail
	}
	return result
}

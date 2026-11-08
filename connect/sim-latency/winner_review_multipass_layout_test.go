// An explicitly enabled local layout command measures authentic frozen context
// without tokenizer/model traffic, runtime-proof invention or review authority.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Input is a root-owned private configuration, not candidate or model choices.
type winnerMultipassLayoutConfig struct {
	Packet           currentQgQualificationPin       `json:"packet"`
	RepositoriesRoot string                          `json:"repositories_root"`
	OutputDirectory  string                          `json:"output_directory"`
	Settings         winnerMultipassSettings         `json:"settings"`
	Capacity         winnerMultipassCapacitySettings `json:"capacity"`
	AtomsPerScan     int                             `json:"atoms_per_scan"` // Positive uniform layout grouping only; default one, never token admission.
}

// Actual source bytes are counted but never printed in the command log.
type winnerMultipassLayoutCase struct {
	Position             int                      `json:"position"`
	PlanSha256           string                   `json:"layout_plan_sha256"`
	Artifacts            int                      `json:"artifacts"`
	ArtifactBytes        int64                    `json:"artifact_bytes"`
	CoverageAtoms        int                      `json:"coverage_atoms"`
	ScanCalls            int                      `json:"scan_calls"`
	GenerationCalls      int                      `json:"generation_calls"`
	TokenizerInvocations int                      `json:"tokenizer_invocations"`
	Capacity             *winnerMultipassCapacity `json:"capacity"`
	CapacityError        string                   `json:"capacity_error"`
}

// A layout is explicitly not a prospective owner or a provider qualification.
type winnerMultipassLayoutReport struct {
	Schema                       int                         `json:"schema"`
	Kind                         string                      `json:"kind"`
	Packet                       currentQgQualificationPin   `json:"authenticated_seed"`
	SourceCommits                map[string]string           `json:"frozen_source_commits"`
	SourceLockSha256             string                      `json:"frozen_source_lock_sha256"`
	Config                       winnerMultipassLayoutConfig `json:"configuration"`
	Cases                        []winnerMultipassLayoutCase `json:"cases"`
	GenerationCalls              int                         `json:"layout_generation_calls"`
	TokenizerInvocations         int                         `json:"layout_tokenizer_invocations"`
	RequiredDurableBytes         int64                       `json:"layout_required_durable_bytes"`
	RequiredMemoryBytes          int64                       `json:"layout_required_native_memory_bytes"`
	ExceedsWholeGroupCalls       bool                        `json:"exceeds_whole_group_call_ceiling"`
	ExceedsWholeGroupArtifacts   bool                        `json:"exceeds_whole_group_artifact_ceiling"`
	ProviderCallsMade            int                         `json:"provider_calls_made"`
	NonInferenceNetworkCallsMade int                         `json:"non_inference_network_calls_made"`
	RuntimeContextsObserved      bool                        `json:"runtime_contexts_observed"`
	GenerationAuthorized         bool                        `json:"generation_authorized"`
	Qualified                    bool                        `json:"qualified"`
}

// This opt-in is read-only toward Git/network/DB/API. It writes only a newly
// created private report directory and never normalizes a moving main checkout.
func TestWinnerMultipassQualificationLayoutCommand(t *testing.T) {
	if os.Getenv("SIM_LATENCY_MULTIPASS_LAYOUT_COMMAND") == "" {
		return
	}
	if os.Getenv("SIM_LATENCY_MULTIPASS_LAYOUT_COMMAND") != "prepare" {
		t.Fatal("layout command only accepts offline prepare")
	}
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		t.Fatal(err)
	}
	configPin := currentQgQualificationPin{Path: os.Getenv("SIM_LATENCY_MULTIPASS_LAYOUT_CONFIG"), Sha256: os.Getenv("SIM_LATENCY_MULTIPASS_LAYOUT_CONFIG_SHA256")}
	content, err := readCurrentQgQualificationPin(configPin, 64*1024, true)
	if err != nil {
		t.Fatal(err)
	}
	config := winnerMultipassLayoutConfig{Settings: defaultWinnerMultipassSettings(), Capacity: defaultWinnerMultipassCapacitySettings(), AtomsPerScan: 1}
	if err := decodeStrictJSONBytes(content, &config, "offline multipass layout configuration"); err != nil {
		t.Fatal(err)
	}
	if validateWinnerMultipassSettings(config.Settings) != nil || len(config.Settings.TokenizationProfiles) != 0 || config.AtomsPerScan < 1 || config.AtomsPerScan > 16 || !filepath.IsAbs(config.RepositoriesRoot) || filepath.Clean(config.RepositoriesRoot) != config.RepositoriesRoot || !filepath.IsAbs(config.OutputDirectory) || filepath.Clean(config.OutputDirectory) != config.OutputDirectory {
		t.Fatal("offline layout configuration is not canonical or claims runtime proof")
	}
	if err := currentQgQualificationPrivateRoot(filepath.Dir(config.OutputDirectory)); err != nil {
		t.Fatal(err)
	}
	packetBytes, err := readCurrentQgQualificationPin(config.Packet, 32*1024*1024, true)
	if err != nil {
		t.Fatal(err)
	}
	var seed controller.CompetitionSourceReviewPacket
	if err := decodeStrictJSONBytes(packetBytes, &seed, "authenticated frozen layout seed"); err != nil {
		t.Fatal(err)
	}
	lock, err := model.CompetitionSourceLockDigest(seed.SourceCommits)
	if err != nil || seed.Seal.Staging || seed.SourceGeneration != 0 || seed.ActivationGeneration != 1 || seed.SourceCommits["server"] != seed.BaseSha || lock != seed.Entry.BaseSourceLockSha256 || controller.ValidateCompetitionReviewInstrument(&seed) != nil {
		t.Fatal("layout seed is not authenticated activated frozen context")
	}
	instrument, err := loadWinnerWorkloadReviewContext(t.Context(), config.RepositoriesRoot, &seed, defaultWinnerWorkloadReviewSettings())
	if err != nil {
		t.Fatal(err)
	}
	sources, err := winnerMultipassQualificationSources()
	if err != nil {
		t.Fatal(err)
	}
	requests, err := currentQgQualificationRequests(&seed, instrument, sources)
	if err != nil {
		t.Fatal(err)
	}
	report := winnerMultipassLayoutReport{Schema: 1, Kind: "winner-review-multipass-offline-layout-not-authority/v1", Packet: config.Packet, SourceCommits: seed.SourceCommits, SourceLockSha256: lock, Config: config, Cases: []winnerMultipassLayoutCase{}, RequiredDurableBytes: 64 * 1024 * 1024, RequiredMemoryBytes: 384 * 1024 * 1024}
	// Intentionally expired synthetic clocks and absent runtime proofs make these
	// layouts unusable as live requests even if someone mistakes their filenames.
	started := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC)
	artifactBytes := int64(0)
	for index, request := range requests {
		request.Operation = "review_elected_winner"
		bundle := winnerMultipassBundle{Request: request, SupplementalFiles: []winnerMultipassSourceFile{}, RequiredSupplementalPaths: []string{}, UnresolvedContext: []string{}}
		plan, err := prepareWinnerMultipass(bundle, winnerMultipassOwner{Id: fmt.Sprintf("offline-layout-only-control-%02d", index+1), StartedAt: started, Deadline: started.Add(time.Hour)}, config.Settings, defaultWinnerSnowProviderSettings())
		if err != nil {
			t.Fatal(err)
		}
		groups := []winnerMultipassScanGroup{}
		for _, member := range plan.Provider.Members {
			for offset := 0; offset < len(plan.Units); offset += config.AtomsPerScan {
				group := winnerMultipassScanGroup{Member: member.Name, UnitIds: []string{}}
				for _, unit := range plan.Units[offset:min(offset+config.AtomsPerScan, len(plan.Units))] {
					group.UnitIds = append(group.UnitIds, unit.Id)
				}
				groups = append(groups, group)
			}
		}
		if err := setWinnerMultipassScanGroups(plan, groups); err != nil {
			t.Fatal(err)
		}
		planBytes, _ := json.Marshal(plan)
		item := winnerMultipassLayoutCase{Position: index + 1, PlanSha256: winnerMultipassSha256(planBytes), Artifacts: len(plan.Artifacts), CoverageAtoms: len(plan.Units)}
		for _, artifact := range plan.Artifacts {
			item.ArtifactBytes += int64(artifact.Bytes)
		}
		artifactBytes += item.ArtifactBytes
		if index != 0 {
			for _, call := range plan.Calls {
				if call.Stage == "scan" {
					item.ScanCalls++
				}
			}
			item.GenerationCalls = len(plan.Calls)
			item.TokenizerInvocations = item.GenerationCalls + item.ScanCalls
			capacity, capacityErr := estimateWinnerMultipassCapacity(plan, bundle, item.TokenizerInvocations, config.Capacity)
			item.Capacity = capacity
			if capacityErr != nil {
				item.CapacityError = capacityErr.Error()
			}
			if capacity == nil {
				t.Fatal(errors.New("offline layout cannot determine bounded capacity"))
			}
			report.GenerationCalls += item.GenerationCalls
			report.TokenizerInvocations += item.TokenizerInvocations
			report.RequiredDurableBytes += capacity.RequiredDurableBytes - 64*1024*1024
			report.RequiredMemoryBytes += capacity.RequiredMemoryBytes - 384*1024*1024 - int64(config.Settings.MaximumBundleBytes)
		}
		report.Cases = append(report.Cases, item)
	}
	report.RequiredMemoryBytes += artifactBytes
	limits := defaultWinnerMultipassProviderLimits()
	report.RequiredDurableBytes += int64(1+config.Capacity.ProtectedCopies) * (int64(limits.MaximumEnvelopeBytes+limits.MaximumResultBytes) + 32*int64(limits.MaximumMetadataBytes))
	report.ExceedsWholeGroupCalls = report.GenerationCalls > config.Settings.MaximumCalls
	report.ExceedsWholeGroupArtifacts = artifactBytes > int64(config.Settings.MaximumBundleBytes)
	if err := os.Mkdir(config.OutputDirectory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := writeCurrentQgQualificationJson(filepath.Join(config.OutputDirectory, "layout-report.json"), report); err != nil {
		t.Fatal(err)
	}
	t.Logf("Offline frozen control layout only: generations=%d tokenizers=%d durable_bytes=%d native_memory_bytes=%d; token admission pending; no network/provider/API calls", report.GenerationCalls, report.TokenizerInvocations, report.RequiredDurableBytes, report.RequiredMemoryBytes)
}

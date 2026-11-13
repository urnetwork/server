// Fresh preparation binds the actual frozen instrument and original native
// fixture observations. It is neither a model report nor permission to infer.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/urnetwork/server/v2026/controller"
	"github.com/urnetwork/server/v2026/model"
)

// All desired labels stay in their own private file, never in a request bundle.
type winnerMultipassQualificationPreparation struct {
	Schema                 int                         `json:"schema"`
	Kind                   string                      `json:"kind"`
	Status                 string                      `json:"status"`
	Seed                   currentQgQualificationPin   `json:"authenticated_instrument_seed"`
	RepositoriesRoot       string                      `json:"repositories_root"`
	NativeFixtureEvidence  currentQgQualificationPin   `json:"native_fixture_evidence"`
	SourceLockSha256       string                      `json:"source_lock_sha256"`
	InstrumentSha256       string                      `json:"instrument_sha256"`
	EvaluatedImageDigest   string                      `json:"evaluated_image_digest"`
	WorkloadCensusSha256   string                      `json:"workload_census_sha256"`
	ProductionCensusSha256 string                      `json:"production_census_sha256"`
	Requests               []currentQgQualificationPin `json:"requests"`
	ExpectedOutcomes       currentQgQualificationPin   `json:"private_expected_outcomes"`
	ProviderCallsMade      int                         `json:"provider_calls_made"`
}

// Returned source identities can be compared with the actual elected candidate
// by the native gate. No caller may substitute a moving main instrument.
type winnerMultipassQualificationInputs struct {
	Requests             []winnerReviewBrokerRequest
	Expectations         []currentQgQualificationExpectation
	SourceLockSha256     string
	InstrumentSha256     string
	EvaluatedImageDigest string
}

// Rebuild independently from frozen Git and retained real compiler/probe outputs.
// Original opaque/missing-helper failures remain failures, not fabricated tests.
func rebuildWinnerMultipassQualificationInputs(ctx context.Context, seedPin currentQgQualificationPin, repositoriesRoot string, nativeFixturePin currentQgQualificationPin) (*winnerMultipassQualificationInputs, error) {
	if !filepath.IsAbs(repositoriesRoot) || filepath.Clean(repositoriesRoot) != repositoriesRoot {
		return nil, errors.New("qualification needs canonical frozen repository roots")
	}
	content, err := readCurrentQgQualificationPin(seedPin, 32*1024*1024, true)
	if err != nil {
		return nil, err
	}
	seed, err := decodeWinnerMultipassInstrumentSeed(content)
	if err != nil {
		return nil, err
	}
	lock, err := model.CompetitionSourceLockDigest(seed.SourceCommits)
	if err != nil || seed.Seal.Staging || seed.SourceGeneration != 0 || seed.ActivationGeneration != 1 || seed.SourceCommits["server"] != seed.BaseSha || lock != seed.Entry.BaseSourceLockSha256 || controller.ValidateCompetitionReviewInstrument(seed) != nil {
		return nil, errors.New("qualification seed lacks activated complete frozen source/instrument authority")
	}
	instrument, err := loadWinnerWorkloadReviewContext(ctx, repositoriesRoot, seed, defaultWinnerWorkloadReviewSettings())
	if err != nil {
		return nil, err
	}
	sources, err := winnerMultipassQualificationSources()
	if err != nil {
		return nil, err
	}
	fixtureSeed, err := winnerMultipassQualificationInstrumentSeed(seed)
	if err != nil {
		return nil, err
	}
	requests, err := currentQgQualificationRequests(fixtureSeed, instrument, sources)
	if err != nil {
		return nil, err
	}
	expectations := []currentQgQualificationExpectation{}
	for _, source := range sources {
		expectations = append(expectations, source.Expectation)
	}
	native, err := readCurrentQgQualificationPin(nativeFixturePin, 16*1024*1024, true)
	if err != nil {
		return nil, err
	}
	requests, expectations, err = bindCurrentQgNativeFixtureEvidence(requests, sources, native, expectations)
	if err != nil {
		return nil, err
	}
	instrumentBytes, err := json.Marshal(seed.Instrument)
	if err != nil {
		return nil, err
	}
	return &winnerMultipassQualificationInputs{Requests: requests, Expectations: expectations, SourceLockSha256: lock, InstrumentSha256: winnerMultipassSha256(instrumentBytes), EvaluatedImageDigest: seed.EvaluatedImageDigest}, nil
}

// Exact canonical bytes are fsynced before their private pin is published.
func writeWinnerMultipassQualificationCanonical(path string, value any) (currentQgQualificationPin, error) {
	content, err := json.Marshal(value)
	if err != nil {
		return currentQgQualificationPin{}, err
	}
	if err := writePrivateReviewFile(path, content); err != nil {
		return currentQgQualificationPin{}, err
	}
	parent, err := os.Open(filepath.Dir(path))
	if err != nil {
		return currentQgQualificationPin{}, err
	}
	if err := errors.Join(parent.Sync(), parent.Close()); err != nil {
		return currentQgQualificationPin{}, err
	}
	return currentQgQualificationPin{Path: path, Sha256: winnerMultipassSha256(content)}, nil
}

// This is the only fresh preparation writer used by the new native CLI. It
// creates a new private directory and exports no provider-specific desired label.
func prepareWinnerMultipassQualificationInputFiles(ctx context.Context, seedPin currentQgQualificationPin, repositoriesRoot string, nativeFixturePin currentQgQualificationPin, directory string) (*winnerMultipassQualificationPreparation, currentQgQualificationPin, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, currentQgQualificationPin{}, errors.New("fresh qualification output directory must be canonical")
	}
	if err := currentQgQualificationPrivateRoot(filepath.Dir(directory)); err != nil {
		return nil, currentQgQualificationPin{}, err
	}
	inputs, err := rebuildWinnerMultipassQualificationInputs(ctx, seedPin, repositoriesRoot, nativeFixturePin)
	if err != nil {
		return nil, currentQgQualificationPin{}, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return nil, currentQgQualificationPin{}, err
	}
	preparation := &winnerMultipassQualificationPreparation{Schema: 1, Kind: "winner-review-multipass-qualification-preparation/v1", Status: "PREPARED_NATIVE_FIXTURES_BOUND_NOT_PROVIDER_QUALIFIED", Seed: seedPin, RepositoriesRoot: repositoriesRoot, NativeFixtureEvidence: nativeFixturePin,
		SourceLockSha256: inputs.SourceLockSha256, InstrumentSha256: inputs.InstrumentSha256, EvaluatedImageDigest: inputs.EvaluatedImageDigest, WorkloadCensusSha256: currentQgWorkloadCensusSha256, ProductionCensusSha256: currentQgProductionCensusSha256, Requests: []currentQgQualificationPin{}}
	for index, request := range inputs.Requests {
		pin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(directory, fmt.Sprintf("case-%02d.request.json", index+1)), request)
		if err != nil {
			return preparation, currentQgQualificationPin{}, err
		}
		preparation.Requests = append(preparation.Requests, pin)
	}
	preparation.ExpectedOutcomes, err = writeWinnerMultipassQualificationCanonical(filepath.Join(directory, "private-expected-outcomes.json"), inputs.Expectations)
	if err != nil {
		return preparation, currentQgQualificationPin{}, err
	}
	pin, err := writeWinnerMultipassQualificationCanonical(filepath.Join(directory, "preparation.json"), preparation)
	return preparation, pin, err
}

// Authenticate the new preparation rather than accepting valid-looking SHA
// strings. This helper is shared by CLI preparation, genuine run and winner gate.
func authenticateWinnerMultipassQualificationInputs(ctx context.Context, release winnerMultipassQualificationRelease) (*winnerMultipassQualificationInputs, error) {
	if err := validateWinnerMultipassQualificationReleaseShape(release); err != nil {
		return nil, err
	}
	content, err := readCurrentQgQualificationPin(release.Preparation, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	var preparation winnerMultipassQualificationPreparation
	if decodeStrictJSONBytes(content, &preparation, "fresh original qualification preparation") != nil || preparation.Schema != 1 || preparation.Kind != "winner-review-multipass-qualification-preparation/v1" || preparation.Status != "PREPARED_NATIVE_FIXTURES_BOUND_NOT_PROVIDER_QUALIFIED" || preparation.ProviderCallsMade != 0 || len(preparation.Requests) != 19 || preparation.NativeFixtureEvidence != release.NativeFixtureEvidence || preparation.WorkloadCensusSha256 != currentQgWorkloadCensusSha256 || preparation.ProductionCensusSha256 != currentQgProductionCensusSha256 {
		return nil, errors.New("qualification preparation is historical, incomplete, changed or falsely marked executed")
	}
	inputs, err := rebuildWinnerMultipassQualificationInputs(ctx, preparation.Seed, preparation.RepositoriesRoot, release.NativeFixtureEvidence)
	if err != nil {
		return nil, err
	}
	if inputs.SourceLockSha256 != preparation.SourceLockSha256 || inputs.InstrumentSha256 != preparation.InstrumentSha256 || inputs.EvaluatedImageDigest != preparation.EvaluatedImageDigest {
		return nil, errors.New("qualification preparation replaced its frozen instrument/source context")
	}
	for index, request := range inputs.Requests {
		pin := preparation.Requests[index]
		if pin.Path != filepath.Join(filepath.Dir(release.Preparation.Path), fmt.Sprintf("case-%02d.request.json", index+1)) {
			return nil, errors.New("qualification prepared request census/order changed")
		}
		actual, err := readCurrentQgQualificationPin(pin, 16*1024*1024, true)
		if err != nil {
			return nil, err
		}
		canonical, _ := json.Marshal(request)
		if !bytes.Equal(actual, canonical) {
			return nil, errors.New("qualification request differs from independent frozen/native-fixture reconstruction")
		}
	}
	if preparation.ExpectedOutcomes.Path != filepath.Join(filepath.Dir(release.Preparation.Path), "private-expected-outcomes.json") {
		return nil, errors.New("private outcome census moved outside its original preparation")
	}
	actual, err := readCurrentQgQualificationPin(preparation.ExpectedOutcomes, int64(defaultWinnerMultipassProviderLimits().MaximumMetadataBytes), true)
	if err != nil {
		return nil, err
	}
	canonical, _ := json.Marshal(inputs.Expectations)
	if !bytes.Equal(actual, canonical) {
		return nil, errors.New("private qualification outcomes were retuned after preparation")
	}
	return inputs, nil
}

// Match the complete request, not only its before/after source or desired label.
func validateWinnerMultipassQualificationOriginalInputs(plan *winnerMultipassQualificationPlan, inputs *winnerMultipassQualificationInputs) error {
	if plan == nil || inputs == nil || len(plan.Cases) != 19 || len(inputs.Requests) != 19 || len(inputs.Expectations) != 19 {
		return errors.New("qualification original request provenance is incomplete")
	}
	for index, item := range plan.Cases {
		request := inputs.Requests[index]
		request.Operation = "review_elected_winner"
		if !reflect.DeepEqual(item.Bundle.Request, request) || item.Expectation != inputs.Expectations[index] {
			return errors.New("qualification group substituted original source, native observations, instrument, instructions or labels")
		}
	}
	return nil
}

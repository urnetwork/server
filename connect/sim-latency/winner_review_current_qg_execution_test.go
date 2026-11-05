// Current two-model qualification has a distinct typed command and authority.
// Historical commands/receipts cannot select it or supply missing new evidence.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/urnetwork/server/controller"
	"github.com/urnetwork/server/model"
)

// Ten hours is an independent ceiling, not a default or an authorization. The
// operator must precommit an exact positive whole-suite budget for 19 cases.
const currentQgQualificationMaximumSeconds = 10 * 60 * 60

// No alternate provider, endpoint, model pool, retries or main token exists.
type currentQgExecutionConfig struct {
	Schema              int                       `json:"schema"`
	Kind                string                    `json:"kind"`
	OutputDirectory     string                    `json:"output_directory"`
	TotalTimeoutSeconds int                       `json:"total_timeout_seconds"`
	Preparation         currentQgQualificationPin `json:"preparation"`
	InstrumentSeed      currentQgQualificationPin `json:"instrument_seed"`
	RepositoriesRoot    string                    `json:"repositories_root"`
	FixtureEvidence     currentQgQualificationPin `json:"fixture_evidence"`
	Broker              currentQgQualificationPin `json:"broker"`
	BrokerConfiguration currentQgQualificationPin `json:"broker_configuration"`
	ProviderSettings    currentQgQualificationPin `json:"provider_settings"`
	Image               currentQgQualificationPin `json:"image"`
	UnitQualification   currentQgQualificationPin `json:"unit_qualification"`
	ExecutionPlan       currentQgQualificationPin `json:"execution_plan"`
	Authorization       currentQgQualificationPin `json:"authorization"`
}

// A fresh reviewed authorization binds this exact plan/output and budget. It
// is permission for one attempt, never proof that inference or containment ran.
type currentQgExecutionAuthorization struct {
	Schema              int       `json:"schema"`
	Kind                string    `json:"kind"`
	Policy              string    `json:"policy"`
	ExecutionPlanSha256 string    `json:"execution_plan_sha256"`
	OutputDirectory     string    `json:"output_directory"`
	TotalTimeoutSeconds int       `json:"total_timeout_seconds"`
	AuthorizedAt        time.Time `json:"authorized_at"`
	SingleAttempt       bool      `json:"single_qualification_attempt"`
	NoExternalProviders bool      `json:"no_external_providers"`
}

// Strict decoding refuses silently expanded preparation capabilities.
type currentQgPreparationManifest struct {
	Schema                 int                         `json:"schema"`
	Kind                   string                      `json:"kind"`
	Status                 string                      `json:"status"`
	ProviderCalls          int                         `json:"provider_calls_made"`
	Policy                 string                      `json:"policy"`
	Members                []winnerSnowMember          `json:"members"`
	InstrumentSeedSha256   string                      `json:"instrument_seed_sha256"`
	TrafficSha256          string                      `json:"traffic_addendum_sha256"`
	ProductionSha256       string                      `json:"production_addendum_sha256"`
	WorkloadCensusSha256   string                      `json:"workload_census_sha256"`
	ProductionCensusSha256 string                      `json:"production_census_sha256"`
	CaseCount              int                         `json:"case_count"`
	Cancellations          int                         `json:"pre_input_cancellations"`
	MaximumCalls           int                         `json:"maximum_genuine_member_calls"`
	NativeFixturePending   bool                        `json:"native_fixture_execution_evidence_pending"`
	ContainmentPending     bool                        `json:"wrapper_containment_qualification_pending"`
	ProviderPending        bool                        `json:"fresh_genuine_provider_qualification_pending"`
	ExpectedFile           string                      `json:"expected_outcomes_file"`
	ExpectedSha256         string                      `json:"expected_outcomes_sha256"`
	Inputs                 []currentQgPreparationInput `json:"inputs"`
}

// Every request and preview is independently reread before final preparation.
type currentQgPreparationInput struct {
	Position      int    `json:"position"`
	RequestFile   string `json:"broker_request_file"`
	RequestSha256 string `json:"semantic_request_sha256"`
	InputFile     string `json:"provider_input_file"`
	InputSha256   string `json:"provider_input_sha256"`
}

// The driver verifies actual log events, its own binary and the closed source
// census, not just an externally supplied accepted/count assertion.
type currentQgExecutionUnitReceipt struct {
	Schema            int                         `json:"schema"`
	Kind              string                      `json:"kind"`
	Accepted          bool                        `json:"accepted"`
	BinarySha256      string                      `json:"binary_sha256"`
	GoVersion         string                      `json:"go_version"`
	Race              bool                        `json:"race"`
	ConsecutivePasses int                         `json:"consecutive_passes"`
	TestNames         []string                    `json:"test_names"`
	Sources           []currentQgQualificationPin `json:"sources"`
	Logs              []currentQgQualificationPin `json:"logs"`
}

// The native compiled parser recomputes the union from retained originals and
// original readable input. Missing/uncertain evidence never authorizes a winner.
func validateCurrentQgExecutionResponse(content []byte, request winnerReviewBrokerRequest, expected currentQgQualificationExpectation, provider winnerSnowProviderSettings, providerSha256 string) (currentQgQualificationObservation, error) {
	observation := currentQgQualificationObservation{Expected: expected.Decision, ResponseSha256: currentQgQualificationHash(content)}
	if rejectWinnerReviewCaseAliases(content) != nil {
		return observation, errors.New("current QG response changed native field casing")
	}
	var result winnerReviewBrokerResult
	if err := decodeStrictJSONBytes(content, &result, "current QG original broker response"); err != nil {
		return observation, err
	}
	if result.Schema != 1 || result.Model != model.CompetitionSnowWinnerReviewModel || result.Reasoning != model.CompetitionSnowWinnerReviewReasoning ||
		!result.NoMainCredentials || !result.NoCandidateExecution || len(result.PrivateEvidence) == 0 || int64(len(result.PrivateEvidence)) > defaultCurrentQgQualificationSettings().MaximumSnowEvidenceBytes ||
		currentQgQualificationHash([]byte(result.PrivateEvidence)) != result.Review.PrivateEvidenceSha256 || validateWinnerSnowEvidence(&result, request) != nil {
		return observation, errors.New("current QG response lacks genuine native provenance/input binding")
	}
	var evidence winnerSnowEvidence
	if err := decodeStrictJSONBytes([]byte(result.PrivateEvidence), &evidence, "current QG retained original members"); err != nil {
		return observation, err
	}
	if !validSha256(providerSha256) || evidence.ProviderSettingsSha256 != providerSha256 || !reflect.DeepEqual(evidence.ProviderSettings, provider) ||
		validateCurrentQgQualificationMembers(request, evidence.Members, expected) != nil || evidence.Judgment.Decision != expected.Decision {
		return observation, errors.New("current QG response lost exact provider settings or expected independent originals")
	}
	authorityErr := validateWinnerReviewBrokerResult(&result, request.Template, request.Request.Policy, request)
	if expected.Decision == "uncertain" {
		if authorityErr == nil || result.Review.RewardDisposition != "pending" || result.Review.SourceDisposition != "deferred" {
			return observation, errors.New("completed current QG uncertainty became authority")
		}
	} else if (expected.Decision != "approve" && expected.Decision != "disqualify") || authorityErr != nil {
		return observation, errors.New("current QG union does not meet native authority contract")
	}
	for _, member := range evidence.Members {
		observation.ProviderResponseIds = append(observation.ProviderResponseIds, member.ProviderResponseId)
	}
	observation.Observed, observation.PrivateEvidenceSha256 = expected.Decision, result.Review.PrivateEvidenceSha256
	observation.WouldAuthorize, observation.Passed = authorityErr == nil, true
	return observation, nil
}

// The final plan is reproducible from independently pinned preparation, source
// observations and release, not a caller-authored set of desired model inputs.
func currentQgExecutionPlan(config currentQgExecutionConfig, image *currentQgQualificationImage, provider winnerSnowProviderSettings, requests []winnerReviewBrokerRequest, expectations []currentQgQualificationExpectation) (currentQgQualificationPlan, error) {
	if image == nil {
		return currentQgQualificationPlan{}, errors.New("current QG final plan lacks its independently qualified image")
	}
	plan := currentQgQualificationPlan{Schema: 1, Kind: "winner-review-current-qg-qualification-plan/v1", PolicyModel: model.CompetitionSnowWinnerReviewModel,
		ProviderModel: model.CompetitionSnowWinnerReviewModel, Reasoning: model.CompetitionSnowWinnerReviewReasoning, BrokerSha256: config.Broker.Sha256,
		ActualImageId: image.ImageId, ImageReceiptSha256: config.Image.Sha256, ProviderSettingsSha256: config.ProviderSettings.Sha256, Members: provider.Members, Cases: []currentQgQualificationCase{},
		CaseTimeoutSeconds: 1800, TotalTimeoutSeconds: config.TotalTimeoutSeconds, PreparationSha256: config.Preparation.Sha256, FixtureEvidenceSha256: config.FixtureEvidence.Sha256, UnitReceiptSha256: config.UnitQualification.Sha256}
	if config.TotalTimeoutSeconds <= 0 || config.TotalTimeoutSeconds > currentQgQualificationMaximumSeconds || image == nil || validateWinnerSnowProviderSettings(provider) != nil || len(requests) != 19 || len(expectations) != 19 {
		return plan, errors.New("current QG final plan requires exact release, all 19 cases and an explicit bounded suite budget")
	}
	for index, request := range requests {
		content, err := json.Marshal(request)
		if err != nil || currentQgQualificationContainsExpectedLabels(content) || !request.Request.StartedAt.IsZero() || !request.Request.Deadline.IsZero() || expectations[index].Position != index+1 {
			return plan, errors.New("current QG final plan changed private labels, frozen clocks or case order")
		}
		plan.Cases = append(plan.Cases, currentQgQualificationCase{Position: index + 1, Name: expectations[index].Name, ExpectedDecision: expectations[index].Decision, Request: request, RequestSha256: currentQgQualificationHash(content)})
	}
	return plan, nil
}

// Rebuild the complete semantic requests from authenticated fixed Git objects.
// A file hash proves bytes, while root separately owns activated-ledger custody.
func loadCurrentQgExecutionCases(ctx context.Context, config currentQgExecutionConfig, settings currentQgQualificationSettings) ([]winnerReviewBrokerRequest, []currentQgQualificationExpectation, error) {
	content, err := readCurrentQgQualificationPin(config.Preparation, settings.MaximumBytes, true)
	if err != nil {
		return nil, nil, err
	}
	var manifest currentQgPreparationManifest
	if err := decodeStrictJSONBytes(content, &manifest, "current QG original preparation manifest"); err != nil {
		return nil, nil, err
	}
	if manifest.Schema != 1 || manifest.Kind != "current-qg-review-qualification-preparation/v1" || manifest.Status != "PREPARED_NOT_AUTHORIZED_NOT_QUALIFIED" || manifest.ProviderCalls != 0 ||
		manifest.Policy != model.CompetitionSnowWinnerReviewPolicy || !reflect.DeepEqual(manifest.Members, defaultWinnerSnowProviderSettings().Members) ||
		manifest.InstrumentSeedSha256 != config.InstrumentSeed.Sha256 || manifest.TrafficSha256 != winnerWorkloadReviewAddendumSha256() || manifest.ProductionSha256 != newWinnerProductionBenefitReviewContext().AddendumSha256 ||
		manifest.WorkloadCensusSha256 != currentQgWorkloadCensusSha256 || manifest.ProductionCensusSha256 != currentQgProductionCensusSha256 || manifest.CaseCount != 19 || manifest.Cancellations != 1 || manifest.MaximumCalls != 36 ||
		!manifest.NativeFixturePending || !manifest.ContainmentPending || !manifest.ProviderPending || manifest.ExpectedFile != "private-expected-outcomes.json" || len(manifest.Inputs) != 19 {
		return nil, nil, errors.New("current QG preparation census, release or pending authority changed")
	}
	seedContent, err := readCurrentQgQualificationPin(config.InstrumentSeed, settings.MaximumBytes, true)
	if err != nil {
		return nil, nil, err
	}
	var seed controller.CompetitionSourceReviewPacket
	if err := decodeStrictJSONBytes(seedContent, &seed, "current QG actual epoch-one instrument seed"); err != nil {
		return nil, nil, err
	}
	lock, err := model.CompetitionSourceLockDigest(seed.SourceCommits)
	if err != nil || seed.Seal.Staging || seed.SourceGeneration != 0 || seed.ActivationGeneration != 1 || seed.BaseSha != "a9e8e1a710b24c749e6499a0606938252c83e136" ||
		lock != "ec322ed2db023e15ea5dc3eea60a6ca5989661bba737c340fea3477d8d1300cf" || seed.EvaluatedImageDigest != "sha256:98c9061c4a43a25e7b581a5193ca6e7f48e49e7b7fc03b7c1b6190a029befee2" || controller.ValidateCompetitionReviewInstrument(&seed) != nil {
		return nil, nil, errors.New("current QG instrument seed is not the authenticated frozen epoch-one source/image/full binary policy")
	}
	instrument, err := loadWinnerWorkloadReviewContext(ctx, config.RepositoriesRoot, &seed, defaultWinnerWorkloadReviewSettings())
	if err != nil {
		return nil, nil, err
	}
	censusBytes := [][]byte{}
	for _, name := range []string{"workload-review-qualification-v1.json", "production-benefit-review-qualification-v1.json"} {
		content, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			return nil, nil, err
		}
		censusBytes = append(censusBytes, content)
	}
	sources, err := currentQgQualificationSources(censusBytes[0], censusBytes[1])
	if err != nil {
		return nil, nil, err
	}
	requests, err := currentQgQualificationRequests(&seed, instrument, sources)
	if err != nil {
		return nil, nil, err
	}
	expectations := []currentQgQualificationExpectation{}
	for _, source := range sources {
		expectations = append(expectations, source.Expectation)
	}
	expectedContent, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(filepath.Dir(config.Preparation.Path), manifest.ExpectedFile), Sha256: manifest.ExpectedSha256}, settings.MaximumBytes, true)
	if err != nil {
		return nil, nil, err
	}
	var actualExpectations []currentQgQualificationExpectation
	if decodeStrictJSONBytes(expectedContent, &actualExpectations, "current QG private expectations") != nil || !reflect.DeepEqual(actualExpectations, expectations) {
		return nil, nil, errors.New("current QG expected labels were retuned or omitted")
	}
	for index, request := range requests {
		binding := manifest.Inputs[index]
		if binding.Position != index+1 || binding.RequestFile != fmt.Sprintf("case-%02d.broker-request.json", index+1) || binding.InputFile != fmt.Sprintf("case-%02d.provider-input.json", index+1) {
			return nil, nil, errors.New("current QG prepared input order or file census changed")
		}
		requestBytes, _ := json.Marshal(request)
		actual, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(filepath.Dir(config.Preparation.Path), binding.RequestFile), Sha256: binding.RequestSha256}, settings.MaximumBytes, true)
		if err != nil || !bytes.Equal(actual, requestBytes) {
			return nil, nil, errors.New("current QG prepared request differs from independently rebuilt source/instrument")
		}
		readable := request
		readable.Operation = "review_elected_winner"
		input, _ := winnerModelInput(readable)
		actual, err = readCurrentQgQualificationPin(currentQgQualificationPin{Path: filepath.Join(filepath.Dir(config.Preparation.Path), binding.InputFile), Sha256: binding.InputSha256}, settings.MaximumBytes, true)
		if err != nil || !bytes.Equal(actual, input) {
			return nil, nil, errors.New("current QG readable input preview changed")
		}
	}
	fixtureBytes, err := readCurrentQgQualificationPin(config.FixtureEvidence, settings.MaximumBytes, true)
	if err != nil {
		return nil, nil, err
	}
	return bindCurrentQgNativeFixtureEvidence(requests, sources, fixtureBytes, expectations)
}

// No group/world writable ancestor, symlink, duplicate hardlink or ambient
// untrusted owner can redirect these root-installed executable/public leaves.
func currentQgRootPublicPin(pin currentQgQualificationPin, settings currentQgQualificationSettings, mode os.FileMode) ([]byte, error) {
	content, err := readCurrentQgQualificationPin(pin, settings.MaximumBytes, false)
	if err != nil {
		return nil, err
	}
	for path := pin.Path; ; path = filepath.Dir(path) {
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || info.Sys().(*syscall.Stat_t).Uid != 0 || info.Mode().Perm()&0022 != 0 ||
			(path == pin.Path && (info.Sys().(*syscall.Stat_t).Nlink != 1 || mode != 0 && info.Mode().Perm() != mode)) {
			return nil, errors.New("current QG installed public leaf or protected ancestor changed")
		}
		if path == "/" {
			return content, nil
		}
	}
}

// A models-list response or old provider receipt cannot qualify the new image,
// wrapper, current test driver or original two-member settings tuple.
func authenticateCurrentQgExecutionRelease(config currentQgExecutionConfig, settings currentQgQualificationSettings) (*currentQgQualificationImage, winnerSnowProviderSettings, error) {
	var provider winnerSnowProviderSettings
	if config.Schema != 1 || config.Kind != "winner-review-current-qg-qualification-config/v1" || config.TotalTimeoutSeconds <= 0 || config.TotalTimeoutSeconds > currentQgQualificationMaximumSeconds ||
		config.BrokerConfiguration.Path != "/etc/urnetwork/competition-review-v2.json" || config.ProviderSettings.Path != "/var/lib/urnetwork/competition-review/snow-provider-v2.json" ||
		config.Broker.Sha256 != "fde923bc2199580facbf55a205c020bc5dc5d34d046df2be85c8f4ba1f08a72b" {
		return nil, provider, errors.New("current QG release requires explicit exact current wrapper/settings/config and finite suite budget")
	}
	if _, err := currentQgRootPublicPin(config.Broker, settings, 0555); err != nil {
		return nil, provider, err
	}
	if err := verifySourceSupervisorProgram(config.Broker.Path, config.Broker.Sha256); err != nil {
		return nil, provider, err
	}
	configuration, err := currentQgRootPublicPin(config.BrokerConfiguration, settings, 0)
	if err != nil {
		return nil, provider, err
	}
	var deployment struct {
		ImageId                string `json:"image_id"`
		ProviderSettingsFile   string `json:"provider_settings_file"`
		ProviderSettingsSha256 string `json:"provider_settings_sha256"`
	}
	if decodeCurrentQgSnowQualificationProof(configuration, &deployment) != nil || deployment.ProviderSettingsFile != config.ProviderSettings.Path || deployment.ProviderSettingsSha256 != config.ProviderSettings.Sha256 {
		return nil, provider, errors.New("current QG installed deployment differs from independent provider/image pins")
	}
	if _, err := currentQgRootPublicPin(config.ProviderSettings, settings, 0444); err != nil {
		return nil, provider, err
	}
	provider, err = loadWinnerSnowProviderSettings(config.ProviderSettings.Path, config.ProviderSettings.Sha256)
	if err != nil {
		return nil, provider, err
	}
	imageBytes, err := readCurrentQgQualificationPin(config.Image, settings.MaximumBytes, true)
	if err != nil {
		return nil, provider, err
	}
	var image currentQgQualificationImage
	if decodeStrictJSONBytes(imageBytes, &image, "current QG fresh scratch qualification") != nil || image.Schema != 1 || image.Kind != "winner-review-current-qg-scratch-image-qualification/v1" || !image.Accepted ||
		image.SourceCommit != "d49abe810d7dc3c46c0463733d7d2244fe37f09a" || image.BinarySha256 != "7492105bd90b77b61db8e7ddbca1212338e958226faa091734fec14a6c568e01" ||
		image.GoVersion != "go1.27.3" || image.ImageId != "sha256:6a21bcbcda2db0e88406eee44ec78e19dd1827e9c32a66791baa5e06b11387a0" || image.ImageId != deployment.ImageId ||
		image.PolicyModel != model.CompetitionSnowWinnerReviewModel || image.ProviderModel != model.CompetitionSnowWinnerReviewModel || len(image.Evidence) == 0 {
		return nil, provider, errors.New("current QG fresh actual d49 image/build qualification is absent or relabeled")
	}
	for _, pin := range image.Evidence {
		if _, err := readCurrentQgQualificationPin(pin, settings.MaximumBytes, true); err != nil {
			return nil, provider, err
		}
	}
	if err := authenticateCurrentQgExecutionUnits(config.UnitQualification, settings); err != nil {
		return nil, provider, err
	}
	return &image, provider, nil
}

// Verify every qualified log independently and bind the running executable.
func authenticateCurrentQgExecutionUnits(pin currentQgQualificationPin, settings currentQgQualificationSettings) error {
	content, err := readCurrentQgQualificationPin(pin, settings.MaximumBytes, true)
	if err != nil {
		return err
	}
	var receipt currentQgExecutionUnitReceipt
	if decodeStrictJSONBytes(content, &receipt, "current QG driver unit qualification") != nil || receipt.Schema != 1 || receipt.Kind != "winner-review-current-qg-unit-qualification/v1" || !receipt.Accepted ||
		!receipt.Race || receipt.GoVersion != "go1.27.3" || receipt.ConsecutivePasses != 3 || len(receipt.Logs) != 3 || !reflect.DeepEqual(receipt.TestNames, currentQgExecutionTestNames()) || len(receipt.Sources) == 0 || !validSha256(receipt.BinarySha256) {
		return errors.New("current QG requires its exact new source/test census and three complete race passes")
	}
	path, err := os.Executable()
	if err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	digest := sha256.New()
	count, hashErr := io.Copy(digest, io.LimitReader(file, settings.MaximumExecutableBytes+1))
	closeErr := file.Close()
	if hashErr != nil || closeErr != nil || count > settings.MaximumExecutableBytes || hex.EncodeToString(digest.Sum(nil)) != receipt.BinarySha256 {
		return errors.New("current QG running driver differs from the independently qualified executable")
	}
	seenSources := map[string]bool{}
	for _, source := range receipt.Sources {
		if _, err := readCurrentQgQualificationPin(source, settings.MaximumBytes, false); err != nil {
			return err
		}
		if seenSources[filepath.Base(source.Path)] {
			return errors.New("current QG unit source census has duplicate leaf identities")
		}
		seenSources[filepath.Base(source.Path)] = true
	}
	for _, name := range currentQgExecutionSourceNames() {
		if !seenSources[name] {
			return errors.New("current QG unit receipt omitted a current owned source or exact wrapper/input artifact")
		}
	}
	seenLogs := map[string]bool{}
	for _, log := range receipt.Logs {
		if seenLogs[log.Sha256] {
			return errors.New("current QG unit qualification reused a pass log")
		}
		seenLogs[log.Sha256] = true
		content, err := readCurrentQgQualificationPin(log, settings.MaximumBytes, false)
		if err != nil {
			return err
		}
		if err := validateCurrentQgUnitLog(content, receipt.TestNames); err != nil {
			return err
		}
	}
	return nil
}

// Each exact top-level test must run/pass once, with one package pass, no skip,
// duplicate/failure or unexpected test/package. Logs remain original bytes.
func validateCurrentQgUnitLog(content []byte, names []string) error {
	allowed, runs, passes := map[string]bool{}, map[string]int{}, map[string]int{}
	for _, name := range names {
		if name == "" || allowed[name] {
			return errors.New("current QG unit census is empty or duplicated")
		}
		allowed[name] = true
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	packagePasses := 0
	for {
		var event struct{ Action, Package, Test string }
		if err := decoder.Decode(&event); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return errors.New("current QG unit log is not a complete JSON event stream")
		}
		if event.Package != "github.com/urnetwork/server/connect/sim-latency" || event.Test != "" && !allowed[event.Test] {
			return errors.New("current QG unit log executed an unprecommitted test/package")
		}
		switch event.Action {
		case "start", "output":
		case "run":
			runs[event.Test]++
		case "pass":
			if event.Test == "" {
				packagePasses++
			} else {
				passes[event.Test]++
			}
		default:
			return errors.New("current QG unit log skipped, failed or has an unsupported action")
		}
	}
	if packagePasses != 1 || len(runs) != len(allowed) || len(passes) != len(allowed) {
		return errors.New("current QG unit log omitted a complete top-level/package pass")
	}
	for name := range allowed {
		if runs[name] != 1 || passes[name] != 1 {
			return errors.New("current QG unit log omitted or duplicated a test execution")
		}
	}
	return nil
}

// The semantic plan hash, not the authorization file name/time or output root,
// owns the single global attempt. Native per-case claims separately bind exact
// wrapper/image/settings/input, excluding changed directories/deadlines.
func consumeCurrentQgExecutionAuthorization(config currentQgExecutionConfig, settings currentQgQualificationSettings, plan currentQgQualificationPlan) error {
	content, err := readCurrentQgQualificationPin(config.Authorization, settings.MaximumBytes, true)
	if err != nil {
		return err
	}
	var authorization currentQgExecutionAuthorization
	if decodeStrictJSONBytes(content, &authorization, "current QG single-attempt authorization") != nil || authorization.Schema != 1 || authorization.Kind != "winner-review-current-qg-one-attempt-authorization/v1" ||
		authorization.Policy != model.CompetitionSnowWinnerReviewPolicy || authorization.ExecutionPlanSha256 != config.ExecutionPlan.Sha256 || authorization.OutputDirectory != config.OutputDirectory ||
		authorization.TotalTimeoutSeconds != config.TotalTimeoutSeconds || !authorization.SingleAttempt || !authorization.NoExternalProviders || authorization.AuthorizedAt.IsZero() || authorization.AuthorizedAt.After(time.Now().UTC()) {
		return errors.New("current QG needs a fresh complete precommitted one-attempt authorization")
	}
	parent := filepath.Dir(settings.StateRoot)
	if err := currentQgQualificationPrivateRoot(parent); err != nil {
		return err
	}
	root := filepath.Join(parent, "current-qg-qualification-authorizations")
	if err := os.Mkdir(root, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := currentQgQualificationPrivateRoot(root); err != nil {
		return err
	}
	semantic, err := json.Marshal(plan)
	if err != nil {
		return err
	}
	if err := writeCurrentQgQualificationJson(filepath.Join(root, currentQgQualificationHash(semantic)+".used.json"), map[string]any{"schema": 1, "kind": "current-qg-qualification-attempt-consumed/v1", "execution_plan_sha256": config.ExecutionPlan.Sha256, "authorization_sha256": config.Authorization.Sha256, "output_directory": config.OutputDirectory, "automatic_retries": false}); err != nil {
		return err
	}
	file, err := os.Open(root)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

// prepare writes only a new private final plan. run requires three distinct
// explicit current-QG opt-ins, real runtime qualification, independently pinned
// final plan and one-attempt authority before the installed wrapper can start.
func TestCurrentQgQualificationExecutionCommand(t *testing.T) {
	command := os.Getenv("SIM_LATENCY_CURRENT_QG_QUALIFICATION_COMMAND")
	if command == "" {
		return
	}
	if command != "prepare" && command != "run" {
		t.Fatal("current QG command accepts only explicit prepare or run")
	}
	if command == "run" && (os.Getenv("SIM_LATENCY_CURRENT_QG_QUALIFICATION_REAL_OPT_IN") != "1" || os.Getenv("SIM_LATENCY_CURRENT_QG_QUALIFICATION_SNOW_OPT_IN") != "1") {
		t.Fatal("current QG run requires its own separate genuine and local-Snow opt-ins")
	}
	if err := validateCurrentQgQualificationRuntime(); err != nil {
		t.Fatal(err)
	}
	settings := defaultCurrentQgQualificationSettings()
	configBytes, err := readCurrentQgQualificationPin(currentQgQualificationPin{Path: os.Getenv("SIM_LATENCY_CURRENT_QG_QUALIFICATION_CONFIG"), Sha256: os.Getenv("SIM_LATENCY_CURRENT_QG_QUALIFICATION_CONFIG_SHA256")}, settings.MaximumBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	var config currentQgExecutionConfig
	if err := decodeStrictJSONBytes(configBytes, &config, "current QG qualification configuration"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	_, err = withCalibrationScoringSlot(settings.ScoringLock, func() (*calibrationAnalysisReport, error) {
		image, provider, err := authenticateCurrentQgExecutionRelease(config, settings)
		if err != nil {
			return nil, err
		}
		requests, expectations, err := loadCurrentQgExecutionCases(ctx, config, settings)
		if err != nil {
			return nil, err
		}
		plan, err := currentQgExecutionPlan(config, image, provider, requests, expectations)
		if err != nil {
			return nil, err
		}
		if !filepath.IsAbs(config.OutputDirectory) || filepath.Clean(config.OutputDirectory) != config.OutputDirectory {
			return nil, errors.New("current QG command needs a canonical new private output directory")
		}
		if err := currentQgQualificationPrivateRoot(filepath.Dir(config.OutputDirectory)); err != nil {
			return nil, err
		}
		if command == "prepare" {
			if err := os.Mkdir(config.OutputDirectory, 0700); err != nil {
				return nil, err
			}
			return nil, writeCurrentQgQualificationJson(filepath.Join(config.OutputDirectory, "precommitted-execution-plan.json"), plan)
		}
		pinnedPlan, err := readCurrentQgQualificationPin(config.ExecutionPlan, settings.MaximumBytes, true)
		if err != nil {
			return nil, err
		}
		var original currentQgQualificationPlan
		if decodeStrictJSONBytes(pinnedPlan, &original, "current QG precommitted final execution plan") != nil || !reflect.DeepEqual(plan, original) {
			return nil, errors.New("current QG actual plan differs from the independently authorized precommit")
		}
		if err := os.Mkdir(config.OutputDirectory, 0700); err != nil {
			return nil, err
		}
		if err := writePrivateReviewFile(filepath.Join(config.OutputDirectory, "configuration.json"), configBytes); err != nil {
			return nil, err
		}
		if err := writePrivateReviewFile(filepath.Join(config.OutputDirectory, "precommitted-execution-plan.json"), pinnedPlan); err != nil {
			return nil, err
		}
		if err := consumeCurrentQgExecutionAuthorization(config, settings, plan); err != nil {
			return nil, err
		}
		settings.TotalTimeout = time.Duration(config.TotalTimeoutSeconds) * time.Second
		hooks := currentQgQualificationHooks{Mode: "genuine_provider", Now: time.Now,
			ValidatePlan: func(actual currentQgQualificationPlan) error {
				if !reflect.DeepEqual(actual, plan) {
					return errors.New("current QG final plan changed after authorization")
				}
				return nil
			}, ValidateResponse: func(output []byte, request winnerReviewBrokerRequest, expected string) (currentQgQualificationObservation, error) {
				for index, item := range plan.Cases {
					if item.Request.Packet.Entry.JobId == request.Packet.Entry.JobId && expected == expectations[index].Decision {
						return validateCurrentQgExecutionResponse(output, request, expectations[index], provider, config.ProviderSettings.Sha256)
					}
				}
				return currentQgQualificationObservation{}, errors.New("current QG executor requested an unprecommitted case")
			}, Invoke: func(ctx context.Context, input []byte) ([]byte, []byte, error) {
				for _, pin := range []currentQgQualificationPin{config.BrokerConfiguration, config.ProviderSettings} {
					if _, err := currentQgRootPublicPin(pin, settings, 0); err != nil {
						return nil, nil, err
					}
				}
				return invokeCurrentQgQualificationBroker(ctx, config.Broker, input)
			}, AuthenticateContainment: func(input, output, diagnostics []byte, canceled bool) (string, error) {
				return authenticateCurrentQgQualificationContainmentForProvider(input, output, diagnostics, canceled, image.ImageId, settings, currentQgSnowQualificationProfile(config.ProviderSettings))
			}}
		report, err := executeCurrentQgQualification(ctx, config.OutputDirectory, plan, settings, hooks)
		if report != nil {
			t.Logf("current QG genuine attempt complete=%t qualified=%t observations=%d; no main/API/database writes", report.Complete, report.Qualified, len(report.Observations))
		}
		return nil, err
	})
	if err != nil {
		t.Fatal("current QG qualification failed closed; retain all originals; no automatic retry: ", err)
	}
}

// Compiler identity is read from the actual running executable in every real
// command. This helper also keeps runtime out of candidate-controlled inputs.
func currentQgQualificationGoVersion() string {
	return runtime.Version()
}

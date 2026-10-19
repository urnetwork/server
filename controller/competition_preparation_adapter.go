// The trusted preparation executable publishes exact runtime content and
// gathers independent role acknowledgments. It never activates source, approves
// candidates, manufactures qualification, or treats one API as the whole fleet.
package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/urnetwork/server"
	"github.com/urnetwork/server/model"
)

// The supervisor supplies identity, not runtime authority. Every consequential
// step reloads the immutable intent and checks the still-live database lease.
type CompetitionPreparationRequest struct {
	Operation string                            `json:"operation"`
	Intent    model.CompetitionSourceActivation `json:"intent"`
	Lease     model.CompetitionSupervisorLease  `json:"lease"`
}

// Deployment-owned inputs stay outside frozen candidate source. Commands are
// trusted control-plane brokers, not candidate programs; their SHA-256 pins and
// explicit role images are reviewed together. The token remains in a private file.
type CompetitionPreparationConfig struct {
	Environment             string `json:"environment"`
	CompetitionId           string `json:"competition_id"`
	SeasonId                string `json:"season_id"`
	Staging                 bool   `json:"staging"`
	ApiBaseUrl              string `json:"api_base_url"`
	ApiImageDigest          string `json:"api_image_digest"`
	SourcePromoterTokenFile string `json:"source_promoter_token_file"`
	QualificationCommand    string `json:"qualification_command"`
	QualificationSha256     string `json:"qualification_sha256"`
	WorkerCommand           string `json:"worker_command"`
	WorkerSha256            string `json:"worker_sha256"`
	WorkerId                string `json:"worker_id"`
	WorkerImageDigest       string `json:"worker_image_digest"`
	TimeoutSeconds          int    `json:"timeout_seconds"`
}

// Successful output is a publication reference, not a caller-created readiness
// receipt. Actual API and worker receipts remain in their authoritative store.
type CompetitionPreparationResult struct {
	Generation      int64     `json:"generation"`
	TargetRoundId   server.Id `json:"target_round_id"`
	EnvelopeSha256  string    `json:"envelope_sha256"`
	EnvelopeKey     string    `json:"envelope_key"`
	EnvelopeVersion string    `json:"envelope_version"`
}

// Strict stdin parsing occurs before loading credentials or contacting services.
func DecodeCompetitionPreparationRequest(reader io.Reader) (*CompetitionPreparationRequest, error) {
	request := &CompetitionPreparationRequest{}
	if err := decodeCompetitionPreparationJson(reader, competitionRuntimeManifestLimit, request); err != nil {
		return nil, err
	}
	if request.Operation != "propagate_and_qualify" || request.Intent.Generation < 1 || request.Intent.TargetEpoch < 1 || request.Intent.TargetEpoch > 6 ||
		request.Intent.TargetRoundId == (server.Id{}) || !sha256Pattern.MatchString(request.Intent.IntentSha256) ||
		!tokenNamePattern.MatchString(request.Lease.OwnerId) || request.Lease.FencingToken < 1 {
		return nil, errors.New("preparation request identity is invalid")
	}
	return request, nil
}

// Reads an existing private regular file without following a terminal symlink.
// Commands and credentials must also live in operator-controlled parent paths.
func readCompetitionPreparationFile(filePath string, limit int64, private bool) ([]byte, error) {
	if !filepath.IsAbs(filePath) || filepath.Clean(filePath) != filePath {
		return nil, errors.New("preparation file must have an absolute canonical path")
	}
	info, err := os.Lstat(filePath)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o022 != 0 || private && info.Mode().Perm()&0o077 != 0 || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("preparation file is absent, unsafe or oversized")
	}
	file, err := openFailureArtifactPath(filepath.Dir(filePath), filepath.Base(filePath), false)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) {
		return nil, errors.New("preparation file changed while opening")
	}
	content, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(content)) > limit {
		return nil, errors.New("preparation file cannot be read within its bound")
	}
	return content, nil
}

// Inventory is local, private and explicit; no mutable remote command or URL
// can arrive through a source intent, review, candidate patch or stdin field.
func LoadCompetitionPreparationConfig(filePath string) (*CompetitionPreparationConfig, error) {
	content, err := readCompetitionPreparationFile(filePath, 64*1024, true)
	if err != nil {
		return nil, err
	}
	config := &CompetitionPreparationConfig{}
	if err := decodeCompetitionPreparationJson(bytes.NewReader(content), 64*1024, config); err != nil {
		return nil, err
	}
	return config, nil
}

// Normalization removes only database lifecycle fields. A copied digest may
// not bless a request whose manifest, timing, source refs or target was changed.
func competitionPreparationIntentDigest(intent model.CompetitionSourceActivation) (string, error) {
	intent.IntentSha256, intent.State, intent.ActivatedAt, intent.AbandonedAt = "", "prepared", nil, nil
	return model.CompetitionAutomationDigest(intent)
}

// Shared with genuine qualification: validates the source transition and
// lease in one transaction, then releases it before any external execution.
// It never renews a dead supervisor's lease on the child's behalf.
func CheckCompetitionPreparationFence(ctx context.Context, settings *Settings, request CompetitionPreparationRequest) (intent *model.CompetitionSourceActivation, err error) {
	if settings == nil || !settings.AutomationPolicy.Enabled || settings.AutomationPolicy.RequiredApiCapability != model.CompetitionRuntimeBlobCapability || request.Operation != "propagate_and_qualify" || !sourceModeMatches(settings, &request.Intent) {
		return nil, ErrAutomationDisabled
	}
	requestedDigest, err := competitionPreparationIntentDigest(request.Intent)
	if err != nil || requestedDigest != request.Intent.IntentSha256 {
		return nil, ErrConflict
	}
	store := PostgresStore{}
	err = captureDatabaseError(func() {
		server.Tx(ctx, func(tx server.PgTx) {
			intent = nil
			automationScopeLockInTx(ctx, tx, settings)
			server.Raise(store.requireAutomationLeaseInTx(ctx, tx, settings, request.Lease))
			server.Raise(ensureAutomationNotHeldInTx(ctx, tx, settings, "source_activation"))
			stored, loadErr := sourceActivationInTx(ctx, tx, settings, request.Intent.Generation)
			server.Raise(loadErr)
			storedDigest, digestErr := competitionPreparationIntentDigest(*stored)
			server.Raise(digestErr)
			if stored.ActivatedAt != nil || stored.AbandonedAt != nil || stored.IntentSha256 != requestedDigest || storedDigest != requestedDigest {
				panic(ErrConflict)
			}
			current, loadErr := currentSourceActivationInTx(ctx, tx, settings)
			if loadErr != nil && !errors.Is(loadErr, ErrNotFound) {
				server.Raise(loadErr)
			}
			if current == nil && stored.ExpectedGeneration != 0 || current != nil && current.Generation != stored.ExpectedGeneration {
				panic(ErrConflict)
			}
			server.Raise(validateSourceAuthorizationInTx(ctx, tx, settings, stored, current))
			intent = stored
		})
	})
	return intent, err
}

// Small private seams make cancellation and failure ordering deterministic in
// tests; the exported entrypoint always binds concrete storage and execution.
type competitionPreparationAdapter struct {
	settings   *Settings
	config     CompetitionPreparationConfig
	archive    *blobArtifactArchive
	client     *http.Client
	checkFence func(context.Context, *Settings, CompetitionPreparationRequest) (*model.CompetitionSourceActivation, error)
	receipts   func(context.Context, *Settings, int64) ([]model.CompetitionSourceReadiness, error)
	runProgram func(context.Context, string, string, any, []string) error
}

// The endpoint is HTTPS with normal certificate verification, no URL secrets,
// no query/fragment and no redirects. A path prefix may select an approved API.
func validateCompetitionPreparationConfig(settings *Settings, config CompetitionPreparationConfig) error {
	if settings == nil || config.Environment != settings.AutomationPolicy.Environment || config.CompetitionId != settings.CompetitionId || config.SeasonId != settings.AutomationPolicy.SeasonId || config.Staging != settings.AutomationPolicy.Staging ||
		!settings.AutomationPolicy.Enabled || settings.AutomationPolicy.RequiredApiCapability != model.CompetitionRuntimeBlobCapability ||
		!imageDigestPattern.MatchString(config.ApiImageDigest) || !imageDigestPattern.MatchString(config.WorkerImageDigest) || !tokenNamePattern.MatchString(config.WorkerId) || config.TimeoutSeconds < 60 || config.TimeoutSeconds > 4*60*60 {
		return errors.New("preparation configuration does not bind its scope, runtime images and bounded duration")
	}
	endpoint, err := url.Parse(config.ApiBaseUrl)
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Opaque != "" || strings.Contains(endpoint.EscapedPath(), "%") {
		return errors.New("preparation API must be an authenticated HTTPS origin or path prefix")
	}
	if err := validatePinnedCommand(config.QualificationCommand, config.QualificationSha256, "qualification"); err != nil {
		return err
	}
	if err := validatePinnedCommand(config.WorkerCommand, config.WorkerSha256, "worker"); err != nil {
		return err
	}
	if !filepath.IsAbs(config.SourcePromoterTokenFile) {
		return errors.New("preparation source-promoter token file must be explicit")
	}
	return nil
}

// The real entrypoint cannot replace qualification or worker acknowledgment
// with a caller callback. It checks every step under a cancelable live fence.
func RunCompetitionPreparation(ctx context.Context, settings *Settings, config CompetitionPreparationConfig, request CompetitionPreparationRequest) (*CompetitionPreparationResult, error) {
	if err := validateCompetitionPreparationConfig(settings, config); err != nil {
		return nil, err
	}
	archive, ok := settings.artifactArchive.(*blobArtifactArchive)
	if !ok || archive == nil {
		return nil, errors.New("protected runtime archive is unavailable")
	}
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}, MaxIdleConns: 1, IdleConnTimeout: time.Minute, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 20 * time.Second}
	defer transport.CloseIdleConnections()
	adapter := &competitionPreparationAdapter{
		settings: settings, config: config, archive: archive,
		client:     &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		checkFence: CheckCompetitionPreparationFence, receipts: (PostgresStore{}).sourcePreparationReceipts, runProgram: runCompetitionPreparationProgram,
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()
	operationCtx, stop := context.WithCancelCause(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-operationCtx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			probeCtx, probeCancel := context.WithTimeout(operationCtx, 5*time.Second)
			_, err := adapter.checkFence(probeCtx, settings, request)
			probeCancel()
			if err != nil {
				stop(err)
				return
			}
		}
	}()
	defer func() { stop(nil); <-done }()
	result, err := adapter.run(operationCtx, request)
	if cause := context.Cause(operationCtx); cause != nil {
		return nil, cause
	}
	return result, err
}

// API role evidence precedes host qualification so the qualifier can use the
// real API release identity. No source pointer changes on partial completion.
func (self *competitionPreparationAdapter) run(ctx context.Context, request CompetitionPreparationRequest) (*CompetitionPreparationResult, error) {
	intent, err := self.checkFence(ctx, self.settings, request)
	if err != nil {
		return nil, err
	}
	request.Intent = *intent
	receipt, err := self.archive.publishRuntimeManifest(ctx, self.settings, intent)
	if err != nil {
		return nil, err
	}
	if _, err := self.checkFence(ctx, self.settings, request); err != nil {
		return nil, err
	}
	if err := self.acknowledgeApi(ctx, intent); err != nil {
		return nil, err
	}
	if _, err := self.checkFence(ctx, self.settings, request); err != nil {
		return nil, err
	}
	if err := self.runProgram(ctx, self.config.QualificationCommand, self.config.QualificationSha256, request, nil); err != nil {
		return nil, err
	}
	if _, err := self.checkFence(ctx, self.settings, request); err != nil {
		return nil, err
	}
	arguments := []string{"--source-readiness=" + strconv.FormatInt(intent.Generation, 10), "--worker_id=" + self.config.WorkerId}
	if err := self.runProgram(ctx, self.config.WorkerCommand, self.config.WorkerSha256, nil, arguments); err != nil {
		return nil, err
	}
	if _, err := self.checkFence(ctx, self.settings, request); err != nil {
		return nil, err
	}
	readiness, err := self.receipts(ctx, self.settings, intent.Generation)
	if err != nil {
		return nil, err
	}
	if err := sourcePreparationReceiptsMatch(intent, readiness); err != nil {
		return nil, err
	}
	for _, role := range readiness {
		if role.Role == "api" && role.RuntimeImageDigest != self.config.ApiImageDigest || role.Role == "worker" && (role.RuntimeImageDigest != self.config.WorkerImageDigest || role.ActorId != self.config.WorkerId) {
			return nil, errors.New("prepared role differs from the pinned deployment identity")
		}
	}
	if _, err := self.checkFence(ctx, self.settings, request); err != nil {
		return nil, err
	}
	return &CompetitionPreparationResult{Generation: intent.Generation, TargetRoundId: intent.TargetRoundId, EnvelopeSha256: receipt.Sha256, EnvelopeKey: receipt.Key, EnvelopeVersion: receipt.VersionId}, nil
}

// Token bytes are sent only to the configured TLS endpoint. The response must
// advertise blob capability and independently report the pinned API release.
func (self *competitionPreparationAdapter) acknowledgeApi(ctx context.Context, intent *model.CompetitionSourceActivation) error {
	content, err := readCompetitionPreparationFile(self.config.SourcePromoterTokenFile, 4096, true)
	if err != nil {
		return err
	}
	defer clear(content)
	token := strings.TrimSpace(string(content))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return errors.New("source-promoter credential is malformed")
	}
	endpoint := strings.TrimRight(self.config.ApiBaseUrl, "/") + "/competition/v2/source/" + strconv.FormatInt(intent.Generation, 10) + "/readiness"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := self.client.Do(request)
	if err != nil {
		return errors.New("source readiness API transport failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("X-Competition-Api-Capability") != model.CompetitionRuntimeBlobCapability {
		return errors.New("source readiness API did not acknowledge the required runtime capability")
	}
	var receipt model.CompetitionSourceReadiness
	if err := decodeCompetitionPreparationJson(response.Body, 64*1024, &receipt); err != nil {
		return err
	}
	if receipt.Role != "api" || receipt.RuntimeImageDigest != self.config.ApiImageDigest || !sourcePreparationReceiptMatches(intent, receipt) {
		return errors.New("source readiness API returned another intent or runtime identity")
	}
	return nil
}

// Only pinned trusted brokers execute. Candidate identities remain JSON data,
// never shell interpolation or command arguments. Cancellation terminates the
// complete process group and waits for cleanup before this adapter returns.
func runCompetitionPreparationProgram(ctx context.Context, commandPath, expectedDigest string, request any, arguments []string) error {
	if err := validatePinnedCommand(commandPath, expectedDigest, "preparation"); err != nil {
		return err
	}
	file, err := openFailureArtifactPath(filepath.Dir(commandPath), filepath.Base(commandPath), false)
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Mode().Perm()&0o022 != 0 || info.Mode().Perm()&0o111 == 0 || info.Size() > 256*1024*1024 {
		return errors.New("preparation command is unsafe or oversized")
	}
	hash := sha256.New()
	_, err = io.Copy(hash, io.LimitReader(file, 256*1024*1024+1))
	if err != nil || hex.EncodeToString(hash.Sum(nil)) != expectedDigest {
		return errors.New("preparation command does not match its pinned executable")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	input, err := json.Marshal(request)
	if err != nil {
		return err
	}
	// Execute the verified inode, not a path that could be replaced after the
	// hash check. Trusted brokers use explicit absolute configuration paths.
	command := exec.CommandContext(ctx, "/proc/self/fd/3", arguments...)
	command.Args[0] = commandPath
	command.ExtraFiles = []*os.File{file}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGTERM) }
	command.WaitDelay = 30 * time.Second
	command.Stdin = bytes.NewReader(input)
	command.Stdout, command.Stderr = &boundedBuffer{limit: 64 * 1024}, &boundedBuffer{limit: 64 * 1024}
	if err := command.Run(); err != nil {
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
		return fmt.Errorf("pinned preparation command failed: %w", err)
	}
	return nil
}

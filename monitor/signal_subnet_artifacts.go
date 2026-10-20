// SIGNALS.md §22.8a checks current retrieval of independently enrolled signed
// payout objects. It replays the existing canonical artifact verifier and
// cannot certify chain acceptance, a complete history or independent storage.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/urfoundation/sn/payoutartifact"
)

const subnetArtifactReadBudget = 5 * time.Second
const subnetArtifactRunBudget = 60 * time.Second

// The reader is a partial §22.8 component; the nine-family sentinel is unchanged.
func NewSubnetArtifactsSignal() Signal {
	return &signalAdapter{
		number: "22.8a",
		key:    "subnet-artifacts",
		name:   "Enrolled subnet payout artifact availability",
		probe:  subnetArtifactsProbe{},
	}
}

// One sequential reader owns at most eight requests, each capped at one MiB
// plus one sentinel byte that detects an oversized body before decoding.
type subnetArtifactsProbe struct{}

// Stable signal identity does not contain enrolled deployment or signer data.
func (subnetArtifactsProbe) id() string { return "subnet/artifacts" }

// Missing evidence uses the documented warning band, with no guessed escalation.
func (subnetArtifactsProbe) tier() string { return tierWarn }

// The existing §22 observation cadence is used for the enrolled fixed objects.
func (subnetArtifactsProbe) cadence() time.Duration { return time.Minute }

// Queueing and local replay remain inside the same finite run owner.
func (subnetArtifactsProbe) runBudget() time.Duration { return subnetArtifactRunBudget }

// Missing injected transport must never fall through to native network contact.
type unavailableSubnetArtifactClient struct{}

// Only a fixed error escapes the absent source implementation.
func (unavailableSubnetArtifactClient) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("subnet artifact source unavailable")
}

// Native requests have standard certificate verification and no ambient proxy,
// redirect, cookie, automatic decompression or cross-probe connection pool.
func newSubnetArtifactHttpClient() (*http.Client, *http.Transport) {
	dialer := &net.Dialer{Timeout: subnetArtifactReadBudget, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		DialContext:            dialer.DialContext,
		TLSHandshakeTimeout:    subnetArtifactReadBudget,
		ResponseHeaderTimeout:  subnetArtifactReadBudget,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 16 * 1024,
		MaxIdleConns:           2,
		MaxIdleConnsPerHost:    1,
		MaxConnsPerHost:        1,
		IdleConnTimeout:        subnetArtifactReadBudget,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       subnetArtifactReadBudget,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}, transport
}

// The private cause chain stays inspectable with Is/As, while Error is safe.
// A live parent's source cancellation aborts the census. Only the actual read
// owner's expired deadline permits cleanup cancellation to remain unknown.
func subnetArtifactCancellation(parent context.Context, cause, readOwnerErr error) error {
	if contextErr := parent.Err(); contextErr != nil {
		if cause == nil {
			return contextErr
		}
		return &signalContextError{contextErr: contextErr, sourceErr: cause}
	}
	if errors.Is(cause, context.Canceled) && readOwnerErr != context.DeadlineExceeded {
		return &signalContextError{contextErr: context.Canceled, sourceErr: cause}
	}
	return nil
}

// Every source failure is an availability gap, never proof of accepted fraud or
// of loss of all backing copies. Only the approved object/origin ordinal is public.
func subnetArtifactUnknown(target, reason string, sustain int) finding {
	return finding{
		// The existing scope reducer preserves global unknown findings under this id.
		probeId: "monitor/visibility", tier: tierWarn, class: "subnet-history-availability",
		target: target, sustain: sustain, unobservable: true,
		symptom:   "An enrolled subnet payout artifact cannot be independently read and verified",
		mechanism: "A complete current read of the enrolled signed object is unavailable or fails its independent expectation. Successful sibling reads do not fill this gap.",
		baseline:  "Both configured origins return the complete canonical payout object under the current reviewed expectation, with its exact signer, deployment, policy, boundaries and reconstructed payout root.",
		observed:  "state=unknown reason=" + reason,
		evidence:  "Scope is enrolled fixed-object availability. Finalized chain acceptance, complete paged history, earned-claim evidence loss and independent storage remain unproved.",
		action:    "Inspect the approved enrollment and affected API origin privately. Recover the original artifact bytes or repair the read path; do not replace the expected hash or signer from a failing response.",
		verify:    "Repeat a current bounded read at both enrolled origins and verify the original object. The full subnet correctness coverage warning remains until all nine families are qualified.",
		playbook:  "SIGNALS.md §22.8a",
	}
}

// The complete effective settings generation is checked before contact and
// after every result. The shared closure reloads local inputs, never the API.
func subnetArtifactAuthority(ctx context.Context, env *probeEnv, started time.Time) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	now := env.now()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if now.Before(started) {
		return "clock-regressed", nil
	}
	if reason := env.cfg.subnetArtifacts.validate(now); reason != "" {
		return reason, nil
	}
	if env.cfg.routerGenerationCheck == nil {
		return "settings-generation-unavailable", nil
	}
	current, err := env.cfg.routerGenerationCheck(ctx)
	if canceled := subnetArtifactCancellation(ctx, err, nil); canceled != nil {
		return "", canceled
	}
	if err != nil {
		return "settings-generation-unavailable", nil
	}
	if !current {
		return "settings-generation-stale", nil
	}
	now = env.now()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if now.Before(started) {
		return "clock-regressed", nil
	}
	if reason := env.cfg.subnetArtifacts.validate(now); reason != "" {
		return reason, nil
	}
	return "", nil
}

// Enrollment is checked even when new subnet work is disabled: historical
// object retention is a separate obligation and absence cannot imply disposal.
func (subnetArtifactsProbe) check(ctx context.Context, env *probeEnv) (findings []finding, err error) {
	var lastReadErr error
	// Every return withholds findings after parent cancellation, including
	// configuration exits and the final clock check after the last origin.
	defer func() {
		if contextErr := ctx.Err(); contextErr != nil {
			findings = nil
			err = &signalContextError{contextErr: contextErr, sourceErr: errors.Join(err, lastReadErr)}
		}
	}()
	started := env.now()
	reason, err := subnetArtifactAuthority(ctx, env, started)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		return []finding{subnetArtifactUnknown("subnet-artifacts/configuration", reason, 1)}, nil
	}
	client := env.subnetArtifactClient
	if client == nil {
		var transport *http.Transport
		client, transport = newSubnetArtifactHttpClient()
		defer transport.CloseIdleConnections()
	}
	findings = make([]finding, 0, len(env.cfg.subnetArtifacts.Objects)*2)
	for i, expected := range env.cfg.subnetArtifacts.Objects {
		for j, origin := range env.cfg.subnetArtifacts.Origins {
			if canceled := ctx.Err(); canceled != nil {
				return nil, canceled
			}
			target := fmt.Sprintf("subnet-artifacts/object-%d/origin-%d", i+1, j+1)
			reason, readErr, readOwnerErr := readSubnetArtifact(ctx, client, origin, expected, subnetArtifactReadBudget)
			lastReadErr = readErr
			if canceled := subnetArtifactCancellation(ctx, readErr, readOwnerErr); canceled != nil {
				return nil, canceled
			}
			authorityReason, authorityErr := subnetArtifactAuthority(ctx, env, started)
			if authorityErr != nil {
				return nil, &signalContextError{contextErr: authorityErr, sourceErr: readErr}
			}
			if authorityReason != "" {
				return []finding{subnetArtifactUnknown("subnet-artifacts/configuration", authorityReason, 1)}, nil
			}
			if reason == "" {
				findings = append(findings, healthyFinding("subnet/artifacts", tierWarn, "subnet-history-availability", target))
			} else {
				findings = append(findings, subnetArtifactUnknown(target, reason, 2))
			}
		}
	}
	return findings, nil
}

// Transport bytes are bounded before decoding. Every returned response body is
// closed before evaluation; read/close errors and late cancellation deny success.
// The final result carries the actual child context state sampled before cancel,
// independently of any context sentinels claimed by the returned source cause.
func readSubnetArtifact(parent context.Context, client grafanaHTTPClient, origin string, expected SubnetArtifactExpectation, budget time.Duration) (reason string, err, readOwnerErr error) {
	if err := parent.Err(); err != nil {
		return "unavailable", err, nil
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer func() {
		readOwnerErr = ctx.Err()
		if readOwnerErr != nil {
			err = errors.Join(err, readOwnerErr)
			if reason == "" {
				reason = "read-owner-ended"
			}
		}
		cancel()
	}()
	endpoint := origin + "/sn/artifact?" + url.Values{"hash": []string{expected.ContentHash}}.Encode()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "request-invalid", err, nil
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("Cache-Control", "no-cache, no-store")
	response, requestErr := doScopedGrafanaRequest(client, request)
	if response == nil || response.Body == nil {
		return "response-unavailable", errors.Join(requestErr, ctx.Err()), nil
	}
	if requestErr != nil {
		return "request-unavailable", errors.Join(requestErr, response.Body.Close(), ctx.Err()), nil
	}
	if response.StatusCode != http.StatusOK || response.Uncompressed || response.Header.Get("Content-Encoding") != "" || len(response.Header.Values("Content-Type")) != 1 || response.Header.Get("Content-Type") != "application/json" || len(response.Header.Values("ETag")) != 1 || response.Header.Get("ETag") != `"`+expected.ContentHash+`"` || response.ContentLength < -1 || response.ContentLength > maximumSubnetArtifactBytes || response.Request != nil && (response.Request.URL == nil || response.Request.URL.String() != endpoint) {
		return "response-contract-invalid", errors.Join(response.Body.Close(), ctx.Err()), nil
	}
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, maximumSubnetArtifactBytes+1))
	closeErr := response.Body.Close()
	if readErr != nil || closeErr != nil || ctx.Err() != nil {
		return "body-unavailable", errors.Join(readErr, closeErr, ctx.Err()), nil
	}
	if len(raw) > maximumSubnetArtifactBytes || response.ContentLength >= 0 && response.ContentLength != int64(len(raw)) {
		return "body-bound-or-length-invalid", nil, nil
	}
	wireHash := sha256.Sum256(raw)
	if hex.EncodeToString(wireHash[:]) != expected.WireSha256 {
		return "artifact-wire-mismatch", nil, nil
	}
	artifact, decodeErr := payoutartifact.DecodeWithContext(ctx, raw)
	if decodeErr != nil || ctx.Err() != nil {
		return "artifact-invalid", errors.Join(decodeErr, ctx.Err()), nil
	}
	if artifact.ContentHash != expected.ContentHash || artifact.DeploymentID != expected.DeploymentId || artifact.ChainID != expected.ChainId || artifact.GenesisHash != expected.GenesisHash || artifact.Netuid != expected.Netuid || strings.ToLower(artifact.Coordinator.Hex()) != expected.Coordinator || strings.ToLower(artifact.SettlementVault.Hex()) != expected.SettlementVault || artifact.Epoch != expected.Epoch || artifact.NoID != expected.NoId || artifact.PolicyHash != expected.PolicyHash || strings.ToLower(artifact.Signer.Hex()) != expected.Signer || artifact.Start.Number != expected.StartNumber || artifact.Start.Hash != expected.StartHash || artifact.End.Number != expected.EndNumber || artifact.End.Hash != expected.EndHash || "0x"+hex.EncodeToString(artifact.PayoutRoot[:]) != expected.PayoutRoot {
		return "artifact-expectation-mismatch", nil, nil
	}
	return "", ctx.Err(), nil
}

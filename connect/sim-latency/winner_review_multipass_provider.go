// One contained invocation performs either non-inference tokenization or one
// already-admitted generation. Fixed Snow identities and tool-free transport
// remain unchanged; no retry, proxy, credential or arbitrary endpoint is accepted.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// This explicit narrow rendering contract is derived from vLLM 0.30.0's
// construct_input_messages, ResponsesRequest.build_chat_params/build_tok_params,
// TokenizeChatRequest and shared OnlineRenderer.preprocess_chat. Runtime/genuine
// qualification must additionally prove actual count/usage equality for both pins.
const winnerMultipassTokenizerContract = "vllm-0.30.0-responses-string-tool-free/1; instructions=system,input=user; no-history; add_generation_prompt=true; continue_final_message=false; add_special_tokens=true; tools=null; reasoning_effort=exact-member; enable_thinking=true; no-truncation; rendered-prompt-digest=sha256(canonical-json-token-id-array)"

// Compiled rendering identity is distinct from runtime deployment provenance.
func winnerMultipassTokenizerContractSha256() string {
	return winnerMultipassSha256([]byte(winnerMultipassTokenizerContract))
}

// Limits apply to one contained process, not an unbounded global model owner.
type winnerMultipassProviderLimits struct {
	MaximumEnvelopeBytes        int           `json:"maximum_envelope_bytes"`         // Positive stdin bytes; default 8 MiB covers one base64 4-MiB payload.
	MaximumResultBytes          int           `json:"maximum_result_bytes"`           // Positive stdout and completion-journal bytes; default 4 MiB.
	MaximumTokenizerBytes       int           `json:"maximum_tokenizer_bytes"`        // Positive tokenizer reply bytes; default 8 MiB.
	MaximumMetadataBytes        int           `json:"maximum_metadata_bytes"`         // Positive model/version/running bytes; default 256 KiB.
	MaximumVisibleTerminalBytes int           `json:"maximum_visible_terminal_bytes"` // Positive terminal bytes; default existing 1-MiB SSE event bound. Full visible transport remains retained separately.
	TokenizerTimeout            time.Duration `json:"tokenizer_timeout"`              // Positive per-request bound; default five minutes includes an explicitly authorized cold load.
	GenerationTimeout           time.Duration `json:"generation_timeout"`             // Positive per-call bound; default original thirty minutes.
}

// These resource defaults do not select models or authorize a model load.
func defaultWinnerMultipassProviderLimits() winnerMultipassProviderLimits {
	return winnerMultipassProviderLimits{MaximumEnvelopeBytes: 8 * 1024 * 1024, MaximumResultBytes: 4 * 1024 * 1024, MaximumTokenizerBytes: 8 * 1024 * 1024, MaximumMetadataBytes: 256 * 1024, MaximumVisibleTerminalBytes: 1024 * 1024,
		TokenizerTimeout: 5 * time.Minute, GenerationTimeout: 30 * time.Minute}
}

// The parent binds this exact payload to its durable owner/claim and full plan.
// Complete source bundles stay outside the provider container; no parent mount
// or model-selected retrieval can inject additional source.
type winnerMultipassBrokerEnvelope struct {
	Schema                int                       `json:"schema"`
	Kind                  string                    `json:"kind"`
	Operation             string                    `json:"operation"`
	Owner                 winnerMultipassOwner      `json:"owner"`
	CallDeadline          time.Time                 `json:"call_deadline"`
	PlanSha256            string                    `json:"plan_sha256"`
	BundleSha256          string                    `json:"bundle_sha256"`
	Call                  winnerMultipassCall       `json:"call"`
	Payload               []byte                    `json:"payload"`
	Settings              winnerMultipassSettings   `json:"settings"`
	Admission             *winnerMultipassAdmission `json:"admission"`
	AuthorizedClaimSha256 string                    `json:"authorized_claim_sha256"`
	AllowModelLoad        bool                      `json:"allow_model_load"`
}

// Fixed artifact names are authenticated by the outer containment manifest.
// Host capture must retain their original bytes, not only these references.
type winnerMultipassBrokerResult struct {
	Schema                    int                        `json:"schema"`
	Kind                      string                     `json:"kind"`
	Operation                 string                     `json:"operation"`
	CallId                    string                     `json:"call_id"`
	RequestSha256             string                     `json:"request_sha256"`
	StartedAt                 time.Time                  `json:"started_at"`
	CompletedAt               time.Time                  `json:"completed_at"`
	Admission                 *winnerMultipassAdmission  `json:"admission"`
	TokenizerRequestArtifact  string                     `json:"tokenizer_request_artifact"`
	TokenizerResponseArtifact string                     `json:"tokenizer_response_artifact"`
	VisibleResponse           []byte                     `json:"visible_response"`
	VisibleResponseSha256     string                     `json:"visible_response_sha256"`
	TransportResponseSha256   string                     `json:"transport_response_sha256"`
	TransportResponseArtifact string                     `json:"transport_response_artifact"`
	ProviderOutput            string                     `json:"provider_output"`
	Observation               winnerMultipassObservation `json:"observation"`
	Error                     string                     `json:"error"`
}

// Only this exact payload shape is renderable/admissible; unsupported Responses
// options, tools, history, sampling changes and hidden-reasoning requests fail.
type winnerMultipassPayload struct {
	Model     string `json:"model"`
	Reasoning struct {
		Effort string `json:"effort"`
	} `json:"reasoning"`
	IncludeReasoning    bool              `json:"include_reasoning"`
	Store               bool              `json:"store"`
	Stream              bool              `json:"stream"`
	Tools               []json.RawMessage `json:"tools"`
	ToolChoice          string            `json:"tool_choice"`
	Truncation          string            `json:"truncation"`
	MaximumOutputTokens int               `json:"max_output_tokens"`
	Instructions        string            `json:"instructions"`
	Input               string            `json:"input"`
	Text                struct {
		Format struct {
			Type   string          `json:"type"`
			Name   string          `json:"name"`
			Strict bool            `json:"strict"`
			Schema json.RawMessage `json:"schema"`
		} `json:"format"`
	} `json:"text"`
}

// Schema/instruction bytes are separately bound by the parent plan. This guard
// constrains the entire network protocol before any request, even for tokenization.
func decodeWinnerMultipassPayload(envelope winnerMultipassBrokerEnvelope) (winnerMultipassPayload, error) {
	var payload winnerMultipassPayload
	var fields map[string]json.RawMessage
	if len(envelope.Payload) == 0 || len(envelope.Payload) > envelope.Settings.MaximumRequestBytes ||
		rejectWinnerReviewCaseAliases(envelope.Payload) != nil || decodeStrictJSONBytes(envelope.Payload, &payload, "multi-pass exact Responses payload") != nil ||
		json.Unmarshal(envelope.Payload, &fields) != nil || len(fields) != 12 {
		return payload, errors.New("multi-pass provider payload is not the complete fixed tool-free contract")
	}
	if payload.Model != envelope.Call.Member.Model || payload.Reasoning.Effort != envelope.Call.Member.Reasoning || payload.IncludeReasoning || payload.Store || !payload.Stream ||
		payload.Tools == nil || len(payload.Tools) != 0 || payload.ToolChoice != "none" || payload.Truncation != "disabled" || payload.MaximumOutputTokens != envelope.Settings.MaximumOutputTokens ||
		strings.TrimSpace(payload.Instructions) == "" || payload.Input == "" || payload.Text.Format.Type != "json_schema" || payload.Text.Format.Name != "competition_complete_coverage_review" ||
		!payload.Text.Format.Strict || !json.Valid(payload.Text.Format.Schema) {
		return payload, errors.New("multi-pass provider payload changes model, effort, privacy, schema, tools or truncation")
	}
	return payload, nil
}

// Exact fixed member/render pins and a bounded owner precede all network access.
func validateWinnerMultipassBrokerEnvelope(envelope winnerMultipassBrokerEnvelope, provider winnerSnowProviderSettings, now time.Time, limits winnerMultipassProviderLimits) error {
	if err := validateWinnerSnowProviderSettings(provider); err != nil {
		return err
	}
	if err := validateWinnerMultipassSettings(envelope.Settings); err != nil {
		return err
	}
	ceiling := defaultWinnerMultipassProviderLimits()
	if limits.MaximumEnvelopeBytes <= 0 || limits.MaximumEnvelopeBytes > ceiling.MaximumEnvelopeBytes || limits.MaximumResultBytes <= 0 || limits.MaximumResultBytes > ceiling.MaximumResultBytes ||
		limits.MaximumTokenizerBytes <= 0 || limits.MaximumTokenizerBytes > ceiling.MaximumTokenizerBytes || limits.MaximumMetadataBytes <= 0 || limits.MaximumMetadataBytes > ceiling.MaximumMetadataBytes ||
		limits.MaximumVisibleTerminalBytes <= 0 || limits.MaximumVisibleTerminalBytes > ceiling.MaximumVisibleTerminalBytes || limits.TokenizerTimeout <= 0 || limits.TokenizerTimeout > ceiling.TokenizerTimeout ||
		limits.GenerationTimeout <= 0 || limits.GenerationTimeout > ceiling.GenerationTimeout {
		return errors.New("multi-pass provider limits disable or exceed the qualified containment bounds")
	}
	if envelope.Schema != 1 || envelope.Kind != "winner-review-multipass-call/v1" || (envelope.Operation != "tokenize" && envelope.Operation != "generate") ||
		!validSha256(envelope.PlanSha256) || !validSha256(envelope.BundleSha256) || !slices.Contains(provider.Members, envelope.Call.Member) ||
		!regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`).MatchString(envelope.Call.Id) || !strings.HasPrefix(envelope.Call.Id, envelope.Call.Member.Name+"-") ||
		(envelope.Call.Stage != "scan" && envelope.Call.Stage != "merge" && envelope.Call.Stage != "final") ||
		envelope.Owner.Id == "" || envelope.Owner.StartedAt.IsZero() || now.Before(envelope.Owner.StartedAt) || !now.Before(envelope.Owner.Deadline) ||
		envelope.Owner.Deadline.Sub(envelope.Owner.StartedAt) > envelope.Settings.MaximumOwnerDuration || !envelope.CallDeadline.After(now) ||
		envelope.CallDeadline.After(envelope.Owner.Deadline) || envelope.CallDeadline.Sub(now) > limits.GenerationTimeout {
		return errors.New("multi-pass contained envelope identity or immutable clock is invalid")
	}
	profile, found := envelope.Settings.TokenizationProfiles[envelope.Call.Member.Name]
	if !found || profile.Member != envelope.Call.Member || profile.TokenizerContractSha256 != winnerMultipassTokenizerContractSha256() || !validSha256(profile.RuntimeEvidenceSha256) || profile.MaximumContextTokens <= envelope.Settings.MaximumOutputTokens {
		return errors.New("multi-pass runtime/rendering contract is not prospectively pinned")
	}
	if _, err := decodeWinnerMultipassPayload(envelope); err != nil {
		return err
	}
	if envelope.Operation == "generate" {
		if !validSha256(envelope.AuthorizedClaimSha256) || envelope.Admission == nil || envelope.AllowModelLoad {
			return errors.New("generation lacks a durable claim, admission, or resident-only boundary")
		}
		plan := &winnerMultipassPlan{Owner: envelope.Owner, Settings: envelope.Settings, Provider: provider}
		return validateWinnerMultipassAdmission(plan, envelope.Call, envelope.Payload, *envelope.Admission)
	}
	if envelope.AuthorizedClaimSha256 != "" || envelope.Admission != nil {
		return errors.New("tokenizer operation cannot consume a generation claim or impersonate a generation receipt")
	}
	return nil
}

// Match vLLM's Responses rendering exactly for the accepted narrow text shape.
// No model template text is overridden; its deployed tokenizer owns rendering.
func winnerMultipassTokenizerPayload(envelope winnerMultipassBrokerEnvelope) ([]byte, error) {
	payload, err := decodeWinnerMultipassPayload(envelope)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"model": payload.Model, "messages": []any{
		map[string]any{"role": "system", "content": payload.Instructions}, map[string]any{"role": "user", "content": payload.Input}},
		"add_generation_prompt": true, "continue_final_message": false, "add_special_tokens": true, "return_token_strs": false,
		"chat_template_kwargs": map[string]any{"reasoning_effort": payload.Reasoning.Effort, "enable_thinking": true}, "tools": nil})
}

// All paths derive from immutable settings; candidate text never supplies a URL.
func winnerMultipassHttp(ctx context.Context, client *http.Client, provider winnerSnowProviderSettings, member winnerSnowMember, method, path string, body []byte, maximumBytes int, timeout time.Duration) ([]byte, int, string, error) {
	if err := validateWinnerSnowProviderSettings(provider); err != nil {
		return nil, 0, "", err
	}
	if !slices.Contains(provider.Members, member) {
		return nil, 0, "", errors.New("unknown Snow member")
	}
	prefix := "/upstream/" + member.Model
	if (method != http.MethodGet || (path != "/running" && path != prefix+"/version" && path != prefix+"/v1/models")) &&
		(method != http.MethodPost || (path != prefix+"/tokenize" && path != "/v1/responses")) {
		return nil, 0, "", errors.New("unapproved multi-pass provider operation")
	}
	origin := strings.TrimSuffix(provider.Endpoint, "/v1/responses")
	request, err := http.NewRequestWithContext(ctx, method, origin+path, bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", err
	}
	if method == http.MethodPost {
		request.Header.Set("Content-Type", "application/json")
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	bounded := *client
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	if native, ok := transport.(*http.Transport); ok {
		owned := native.Clone()
		owned.Proxy = nil
		defer owned.CloseIdleConnections()
		bounded.Transport = owned
	}
	bounded.Timeout = timeout
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := bounded.Do(request)
	if err != nil {
		return nil, 0, "", errors.New("multi-pass provider transport failed; no retry authorized")
	}
	defer response.Body.Close()
	content, err := io.ReadAll(io.LimitReader(response.Body, int64(maximumBytes)+1))
	if err != nil || len(content) > maximumBytes {
		return nil, response.StatusCode, response.Header.Get("Content-Type"), errors.New("multi-pass provider response exceeded its fixed bound or could not be read")
	}
	return content, response.StatusCode, response.Header.Get("Content-Type"), nil
}

// Never trigger model loading with an unadmitted generation. Tokenization may
// explicitly load the selected model using non-inference upstream metadata only.
func requireWinnerMultipassResident(ctx context.Context, client *http.Client, provider winnerSnowProviderSettings, member winnerSnowMember, limits winnerMultipassProviderLimits, allowLoad bool) error {
	content, status, _, err := winnerMultipassHttp(ctx, client, provider, member, http.MethodGet, "/running", nil, limits.MaximumMetadataBytes, limits.TokenizerTimeout)
	if err != nil || status != http.StatusOK {
		return errors.New("resident Snow model state is unavailable")
	}
	var running struct {
		Running []struct {
			Model string `json:"model"`
			State string `json:"state"`
		} `json:"running"`
	}
	if json.Unmarshal(content, &running) != nil {
		return errors.New("resident Snow model metadata is malformed")
	}
	ready := len(running.Running) == 1 && running.Running[0].Model == member.Model && running.Running[0].State == "ready"
	if !ready && !allowLoad {
		return errors.New("selected admitted model is not the sole ready resident; no model load authorized")
	}
	version, status, _, err := winnerMultipassHttp(ctx, client, provider, member, http.MethodGet, "/upstream/"+member.Model+"/version", nil, limits.MaximumMetadataBytes, limits.TokenizerTimeout)
	var decoded struct {
		Version string `json:"version"`
	}
	if err != nil || status != http.StatusOK || json.Unmarshal(version, &decoded) != nil || decoded.Version != "0.30.0" {
		return errors.New("actual Snow backend differs from the pinned tokenizer rendering version")
	}
	if !ready {
		return requireWinnerMultipassResident(ctx, client, provider, member, limits, false)
	}
	return nil
}

// Journal leaves are exclusive and fsynced; an interrupted invocation cannot
// overwrite originals. The outer qualified wrapper authenticates owner/mounts.
func persistWinnerMultipassBrokerLeaf(directory, name string, content []byte, maximumBytes int) error {
	allowed := []string{"call.started.json", "call.complete.json", "tokenize.request.json", "tokenize.response.json", "transport.response"}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || !slices.Contains(allowed, name) || len(content) > maximumBytes {
		return errors.New("multi-pass private journal leaf or bound is invalid")
	}
	file, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("multi-pass invocation already has an original journal; no retry authorized")
	}
	_, writeErr := file.Write(content)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	parent, err := os.Open(directory)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

// Hidden reasoning is neither requested nor retained. Failure to parse a stream
// means privacy cannot be proved, so only its digest survives that refusal.
func winnerMultipassVisibleTransport(content []byte, contentType string, maximumEventBytes int) error {
	check := func(value []byte) error {
		var decoded any
		if err := decodeStrictJSONBytes(value, &decoded, "visible provider event"); err != nil {
			return err
		}
		var inspect func(any) bool
		inspect = func(value any) bool {
			switch item := value.(type) {
			case map[string]any:
				if kind, ok := item["type"].(string); ok && (kind == "reasoning" || kind == "reasoning_text" || strings.HasPrefix(kind, "response.reasoning")) {
					return false
				}
				if encrypted, ok := item["encrypted_content"].(string); ok && encrypted != "" {
					return false
				}
				for _, child := range item {
					if !inspect(child) {
						return false
					}
				}
			case []any:
				for _, child := range item {
					if !inspect(child) {
						return false
					}
				}
			}
			return true
		}
		if !inspect(decoded) {
			return errors.New("provider returned hidden reasoning despite disabled inclusion; not retained")
		}
		return nil
	}
	if strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		return check(content)
	}
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, min(4096, maximumEventBytes)), maximumEventBytes)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if value == "[DONE]" {
			continue
		}
		if err := check([]byte(value)); err != nil {
			return err
		}
	}
	return scanner.Err()
}

// One invocation has no retry path. The caller retains the result on every
// error; only the full native plan validator can interpret a final judgment.
func executeWinnerMultipassBroker(ctx context.Context, client *http.Client, envelope winnerMultipassBrokerEnvelope, provider winnerSnowProviderSettings, directory string, limits winnerMultipassProviderLimits) (*winnerMultipassBrokerResult, error) {
	result := &winnerMultipassBrokerResult{Schema: 1, Kind: "winner-review-multipass-call-result/v1", Operation: envelope.Operation, CallId: envelope.Call.Id,
		RequestSha256: winnerMultipassSha256(envelope.Payload), StartedAt: time.Now().UTC(), Admission: envelope.Admission}
	if err := validateWinnerMultipassBrokerEnvelope(envelope, provider, result.StartedAt, limits); err != nil {
		result.Error = err.Error()
		return result, err
	}
	ctx, cancel := context.WithDeadline(ctx, envelope.CallDeadline)
	defer cancel()
	intent, _ := json.Marshal(envelope)
	if err := persistWinnerMultipassBrokerLeaf(directory, "call.started.json", intent, limits.MaximumEnvelopeBytes); err != nil {
		result.Error = err.Error()
		return result, err
	}
	work := func() error {
		if err := requireWinnerMultipassResident(ctx, client, provider, envelope.Call.Member, limits, envelope.AllowModelLoad); err != nil {
			return err
		}
		if envelope.Operation == "tokenize" {
			request, err := winnerMultipassTokenizerPayload(envelope)
			if err != nil {
				return err
			}
			if err := persistWinnerMultipassBrokerLeaf(directory, "tokenize.request.json", request, envelope.Settings.MaximumRequestBytes); err != nil {
				return err
			}
			result.TokenizerRequestArtifact = "tokenize.request.json"
			response, status, _, err := winnerMultipassHttp(ctx, client, provider, envelope.Call.Member, http.MethodPost, "/upstream/"+envelope.Call.Member.Model+"/tokenize", request, limits.MaximumTokenizerBytes, limits.TokenizerTimeout)
			result.TransportResponseSha256 = winnerMultipassSha256(response)
			if err != nil {
				return err
			}
			if err := persistWinnerMultipassBrokerLeaf(directory, "tokenize.response.json", response, limits.MaximumTokenizerBytes); err != nil {
				return err
			}
			result.TokenizerResponseArtifact = "tokenize.response.json"
			if status != http.StatusOK {
				return fmt.Errorf("Snow tokenizer HTTP %d", status)
			}
			var decoded struct {
				Count           int      `json:"count"`
				MaximumModelLen int      `json:"max_model_len"`
				Tokens          []int64  `json:"tokens"`
				TokenStrings    []string `json:"token_strs"`
			}
			if err := decodeStrictJSONBytes(response, &decoded, "Snow full tokenizer result"); err != nil {
				return err
			}
			if decoded.Count <= 0 || decoded.Count != len(decoded.Tokens) || decoded.MaximumModelLen != envelope.Settings.TokenizationProfiles[envelope.Call.Member.Name].MaximumContextTokens ||
				slices.ContainsFunc(decoded.Tokens, func(token int64) bool { return token < 0 }) {
				return errors.New("Snow tokenizer returned clipped, invalid or changed runtime metadata")
			}
			tokenBytes, _ := json.Marshal(decoded.Tokens)
			result.Admission = &winnerMultipassAdmission{CallId: envelope.Call.Id, RequestSha256: result.RequestSha256, Model: envelope.Call.Member.Model, Revision: envelope.Call.Member.Revision,
				TokenizerContractSha256: winnerMultipassTokenizerContractSha256(), RenderedPromptSha256: winnerMultipassSha256(tokenBytes), TokenizerRequestSha256: winnerMultipassSha256(request),
				TokenizerResponseSha256: winnerMultipassSha256(response), MaximumContextTokens: decoded.MaximumModelLen, InputTokens: decoded.Count, OutputTokens: envelope.Settings.MaximumOutputTokens, NoTruncation: true}
			// An over-context count is valid prospective evidence, not generation admission.
			return validateWinnerMultipassTokenization(&winnerMultipassPlan{Settings: envelope.Settings}, envelope.Call, envelope.Payload, *result.Admission)
		}
		response, status, contentType, err := winnerMultipassHttp(ctx, client, provider, envelope.Call.Member, http.MethodPost, "/v1/responses", envelope.Payload, envelope.Settings.MaximumResponseBytes, limits.GenerationTimeout)
		result.TransportResponseSha256 = winnerMultipassSha256(response)
		if err != nil {
			return err
		}
		modelSettings := defaultWinnerSnowModelSettings()
		if err := winnerMultipassVisibleTransport(response, contentType, modelSettings.MaximumEventBytes); err != nil {
			return err
		}
		if err := persistWinnerMultipassBrokerLeaf(directory, "transport.response", response, envelope.Settings.MaximumResponseBytes); err != nil {
			return err
		}
		result.TransportResponseArtifact = "transport.response"
		if status != http.StatusOK {
			return fmt.Errorf("Snow generation HTTP %d", status)
		}
		var terminal []byte
		if strings.HasPrefix(strings.ToLower(contentType), "application/json") {
			terminal = response
			err = validateWinnerModelResponseIdentity(terminal, envelope.Call.Member.Model, "json_response", modelSettings)
		} else {
			terminal, err = readPinnedModelResponse(bytes.NewReader(response), modelSettings, envelope.Call.Member.Model)
		}
		if err != nil {
			return err
		}
		if len(terminal) > limits.MaximumVisibleTerminalBytes {
			return errors.New("original visible terminal exceeds its fixed event bound; full transport retained")
		}
		result.VisibleResponse = append([]byte{}, terminal...)
		result.VisibleResponseSha256 = winnerMultipassSha256(terminal)
		var decoded winnerModelResponse
		if err := json.Unmarshal(terminal, &decoded); err != nil {
			return err
		}
		for _, item := range decoded.Output {
			for _, content := range item.Content {
				if content.Type == "output_text" {
					if len(content.Text) > envelope.Settings.MaximumObservationBytes {
						return errors.New("structured observation exceeds its bound; full original terminal and transport retained")
					}
					result.ProviderOutput = content.Text
				}
			}
		}
		if err := decodeStrictJSONBytes([]byte(result.ProviderOutput), &result.Observation, "multi-pass original observation"); err != nil {
			return err
		}
		result.Admission = envelope.Admission
		// Placeholder transport proof is not publication authority; the qualified
		// host replaces it with the real outer containment-manifest digest.
		probe := winnerMultipassCallEvidence{CallId: envelope.Call.Id, Admission: *envelope.Admission, VisibleResponse: result.VisibleResponse, VisibleResponseSha256: result.VisibleResponseSha256,
			TransportResponseSha256: result.TransportResponseSha256, TransportReceiptSha256: envelope.AuthorizedClaimSha256, ProviderOutput: result.ProviderOutput, Observation: result.Observation}
		_, err = winnerMultipassVisibleResponse(probe, envelope.Call, envelope.Settings)
		return err
	}
	err := work()
	result.CompletedAt = time.Now().UTC()
	if !result.CompletedAt.Before(envelope.Owner.Deadline) || !result.CompletedAt.Before(envelope.CallDeadline) {
		err = errors.Join(err, errors.New("contained call completed outside its original half-open owner/call clock"))
	}
	if err != nil {
		result.Error = err.Error()
	}
	content, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return result, errors.Join(err, marshalErr)
	}
	journalErr := persistWinnerMultipassBrokerLeaf(directory, "call.complete.json", content, limits.MaximumResultBytes)
	return result, errors.Join(err, journalErr)
}

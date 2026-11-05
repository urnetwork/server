// Pure capacity arithmetic plus a fail-closed typed stdin adapter. No traversal,
// source acquisition or native command execution occurs in this helper.
package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"math"
	"os"
)

const captureCeiling int64 = 80 * 1024 * 1024
const globalCeiling int64 = 20 * 1024 * 1024 * 1024
const phaseCeiling int64 = 5 * 1024 * 1024 * 1024

// SHA fields bind externally verified typed evidence, not guessed path roles.
type capacityInput struct {
	Schema                  string `json:"schema"`
	Profile                 string `json:"profile"`
	StreamScope             string `json:"stream_scope"`
	PhaseScope              string `json:"phase_scope"`
	CaptureBasisSha         string `json:"capture_basis_sha256"`
	RecordBasisSha          string `json:"record_basis_sha256"`
	GlobalPhaseBasisSha     string `json:"global_phase_basis_sha256"`
	GlobalIncludesPhaseOnce bool   `json:"global_includes_phase_once"`
	Streams                 int64  `json:"captured_stream_bytes"`
	Records                 int64  `json:"physical_record_proof_bytes"`
	ExtraProof              int64  `json:"additional_proof_bytes"`
	RequestedStreams        int64  `json:"requested_stream_bytes"`
	MetadataReserve         int64  `json:"metadata_reserve_bytes"`
	Global                  int64  `json:"global_work_bytes"`
	Phase                   int64  `json:"phase_work_bytes"`
	Free                    int64  `json:"available_bytes"`
}
type capacityResult struct {
	Green              bool     `json:"green"`
	Profile            string   `json:"profile"`
	PhaseCeiling       int64    `json:"phase_ceiling_bytes"`
	Reasons            []string `json:"reasons"`
	CaptureProjected   int64    `json:"projected_capture_bytes"`
	IncrementalReserve int64    `json:"incremental_phase_reserve_bytes"`
	GlobalProjected    int64    `json:"projected_global_work_bytes"`
}

// Exact current caps stay fixed; this performs no automatic envelope increase.
func capacity(input capacityInput) capacityResult {
	result := capacityResult{Reasons: []string{}}
	phaseLimit := phaseCeiling
	switch input.Profile {
	case "old-5gib":
	case "new-red-6gib":
		phaseLimit = 6 * 1024 * 1024 * 1024
	default:
		result.Reasons = append(result.Reasons, "unknown_profile")
		return result
	}
	result.Profile = input.Profile
	result.PhaseCeiling = phaseLimit
	validSha := func(value string) bool {
		decoded, err := hex.DecodeString(value)
		return err == nil && len(decoded) == 32
	}
	if input.Schema != "capacity-preflight-v1" || input.StreamScope != "qualified-capture-ledger" || input.PhaseScope != "registered-roots-with-phase-once" || !input.GlobalIncludesPhaseOnce || !validSha(input.CaptureBasisSha) || !validSha(input.RecordBasisSha) || !validSha(input.GlobalPhaseBasisSha) {
		result.Reasons = append(result.Reasons, "scope_or_authority_unknown")
		return result
	}
	if input.RequestedStreams != 5*1024*1024 || input.MetadataReserve != 512*1024 {
		result.Reasons = append(result.Reasons, "profile_reserve_mismatch")
		return result
	}
	for _, value := range []int64{input.Streams, input.Records, input.ExtraProof, input.RequestedStreams, input.MetadataReserve, input.Global, input.Phase, input.Free} {
		if value < 0 {
			result.Reasons = append(result.Reasons, "negative_input")
			return result
		}
	}
	if input.Phase > input.Global {
		result.Reasons = append(result.Reasons, "phase_not_contained_in_global")
		return result
	}
	add := func(values ...int64) (int64, bool) {
		var total int64
		for _, value := range values {
			if value > math.MaxInt64-total {
				return 0, false
			}
			total += value
		}
		return total, true
	}
	projected, ok := add(input.Streams, input.Records, input.ExtraProof, input.RequestedStreams, input.MetadataReserve)
	if !ok {
		result.Reasons = append(result.Reasons, "integer_overflow")
		return result
	}
	result.CaptureProjected = projected
	if input.Phase < phaseLimit {
		result.IncrementalReserve = phaseLimit - input.Phase
	}
	projected, ok = add(input.Global, result.IncrementalReserve)
	if !ok {
		result.Reasons = append(result.Reasons, "integer_overflow")
		return result
	}
	result.GlobalProjected = projected
	if input.Phase > phaseLimit {
		result.Reasons = append(result.Reasons, "phase_ceiling_exceeded")
	}
	if result.CaptureProjected >= captureCeiling {
		result.Reasons = append(result.Reasons, "capture_capacity")
	}
	if result.GlobalProjected >= globalCeiling {
		result.Reasons = append(result.Reasons, "global_capacity")
	}
	if input.Free <= result.IncrementalReserve {
		result.Reasons = append(result.Reasons, "free_capacity")
	}
	result.Green = len(result.Reasons) == 0
	return result
}

// JSON is bounded and parsed before the shell guard can reach its launch branch.
func main() {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, 16*1024+1))
	var input capacityInput
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	invalid := err != nil || len(raw) > 16*1024 || decoder.Decode(&input) != nil
	var trailing any
	if !invalid {
		invalid = decoder.Decode(&trailing) != io.EOF
	}
	// Required fields cannot silently become zero, and duplicate keys are ambiguous.
	if !invalid {
		required := map[string]bool{}
		for _, name := range []string{"schema", "profile", "stream_scope", "phase_scope", "capture_basis_sha256", "record_basis_sha256", "global_phase_basis_sha256", "global_includes_phase_once", "captured_stream_bytes", "physical_record_proof_bytes", "additional_proof_bytes", "requested_stream_bytes", "metadata_reserve_bytes", "global_work_bytes", "phase_work_bytes", "available_bytes"} {
			required[name] = false
		}
		tokens := json.NewDecoder(bytes.NewReader(raw))
		first, tokenErr := tokens.Token()
		invalid = tokenErr != nil || first != json.Delim('{')
		for !invalid && tokens.More() {
			token, tokenErr := tokens.Token()
			name, ok := token.(string)
			seen, known := required[name]
			if tokenErr != nil || !ok || !known || seen {
				invalid = true
				break
			}
			var value json.RawMessage
			if tokens.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				invalid = true
				break
			}
			required[name] = true
		}
		for _, seen := range required {
			if !seen {
				invalid = true
			}
		}
	}
	if invalid {
		_ = json.NewEncoder(os.Stdout).Encode(capacityResult{Green: false, Reasons: []string{"input_schema"}})
		os.Exit(1)
	}
	result := capacity(input)
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		os.Exit(1)
	}
	if !result.Green {
		os.Exit(1)
	}
}

package monitor

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"strings"
)

type pgSampleSourceFailure struct {
	Phase           string `json:"phase"`
	Cause           string `json:"cause"`
	StderrTruncated bool   `json:"stderr_truncated"`
}

func parsePgSampleSourceFailure(raw string) (*pgSampleSourceFailure, bool) {
	if len(raw) > 1024 || !pgSampleUniqueObject(raw) {
		return nil, false
	}
	var wire struct {
		Kind            string `json:"kind"`
		Schema          int    `json:"schema"`
		Phase           string `json:"phase"`
		Cause           string `json:"cause"`
		StderrTruncated *bool  `json:"stderr_truncated"`
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&wire) != nil || d.Decode(new(any)) != io.EOF || wire.Kind != "source_failure" || wire.Schema != 1 || wire.StderrTruncated == nil {
		return nil, false
	}
	if !pgSampleFailureEnum(wire.Phase, "bootstrap", "child_start", "child_process", "connection", "identity", "authority", "history_start", "sample_wait", "activity", "blockers", "history_end", "output", "stdin") ||
		!pgSampleFailureEnum(wire.Cause, "invalid_input", "missing_executable", "permission_denied", "owner_deadline", "io_error", "adapter_error", "authority_mismatch", "statement_timeout", "lock_timeout", "authentication", "pg_hba", "connection_refused", "connection_timeout", "name_resolution", "network_unreachable", "tls", "database_missing", "schema_mismatch", "pgss_unavailable", "sql_error", "child_exit_unknown", "output_cap", "broken_pipe") {
		return nil, false
	}
	return &pgSampleSourceFailure{Phase: wire.Phase, Cause: wire.Cause, StderrTruncated: *wire.StderrTruncated}, true
}

func pgSampleFailureEnum(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// An absent remote envelope cannot identify a failed SQL statement. Reuse the
// existing finite transport taxonomy and retain only our exact command exits.
func pgSampleTransportFailure(err error) *pgSampleSourceFailure {
	f := &pgSampleSourceFailure{Phase: "host_transport", Cause: classifyObservationError(err)}
	var sshFailure *sshCommandError
	var exitFailure *exec.ExitError
	if errors.As(err, &sshFailure) && errors.As(sshFailure, &exitFailure) {
		switch exitFailure.ExitCode() {
		case 74:
			f.Phase, f.Cause = "host_identity", "authority_mismatch"
		case 124, 137:
			f.Phase, f.Cause = "remote_owner", "owner_deadline"
		case 127:
			f.Phase, f.Cause = "remote_bootstrap", "missing_executable"
		}
	}
	return f
}

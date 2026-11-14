// Decode runtime members independently so missing or malformed RPC results
// cannot manufacture identity authority or discard unrelated host evidence.
package monitor

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Presence belongs to each measured member, including an explicitly measured
// zero version. Unexported metadata never changes the producer's wire shape.
type subtensorRuntimeAuthority struct {
	SpecName           bool
	SpecVersion        bool
	TransactionVersion bool
}

// Runtime is decoded separately from the host's independent observations.
func (self *subtensorRPCObservation) UnmarshalJSON(data []byte) error {
	type wireRpc subtensorRPCObservation
	var decoded wireRpc
	wire := struct {
		*wireRpc
		Runtime json.RawMessage `json:"runtime"`
	}{wireRpc: &decoded}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*self = subtensorRPCObservation(decoded)
	self.Runtime = decodeSubtensorRuntime(wire.Runtime)
	return nil
}

// Invalid members remain unknown while independently valid siblings survive.
func decodeSubtensorRuntime(data []byte) subtensorRuntimeVersion {
	observed := &subtensorRuntimeAuthority{}
	runtime := subtensorRuntimeVersion{observed: observed}
	var fields map[string]json.RawMessage
	if json.Unmarshal(data, &fields) != nil {
		return runtime
	}
	var name *string
	if json.Unmarshal(fields["specName"], &name) == nil && name != nil && strings.TrimSpace(*name) != "" {
		runtime.SpecName, observed.SpecName = *name, true
	}
	for _, field := range []struct {
		name     string
		value    *int64
		observed *bool
	}{
		{name: "specVersion", value: &runtime.SpecVersion, observed: &observed.SpecVersion},
		{name: "transactionVersion", value: &runtime.TransactionVersion, observed: &observed.TransactionVersion},
	} {
		var value *int64
		if json.Unmarshal(fields[field.name], &value) == nil && value != nil && *value >= 0 {
			*field.value, *field.observed = *value, true
		}
	}
	return runtime
}

// A producer-reported runtime failure withholds that method's whole result.
// Direct in-process observations retain the authority of their explicit fields.
func (self subtensorRPCObservation) runtimeAuthority() subtensorRuntimeAuthority {
	if self.Errors["runtime"] != "" {
		return subtensorRuntimeAuthority{}
	}
	if self.Runtime.observed != nil {
		return *self.Runtime.observed
	}
	return subtensorRuntimeAuthority{
		SpecName:           strings.TrimSpace(self.Runtime.SpecName) != "",
		SpecVersion:        self.Runtime.SpecVersion >= 0,
		TransactionVersion: self.Runtime.TransactionVersion >= 0,
	}
}

// Only fixed structural wording reaches a visibility finding.
func (self subtensorRPCObservation) runtimeProblem() error {
	observed := self.runtimeAuthority()
	if !observed.SpecName || !observed.SpecVersion || !observed.TransactionVersion {
		return fmt.Errorf("runtime identity fields are incomplete or invalid")
	}
	return nil
}

// Identity evidence must not render an unobserved member as measured zero.
func (self subtensorRPCObservation) runtimeEvidence() string {
	observed := self.runtimeAuthority()
	name, spec, transaction := "unknown", "unknown", "unknown"
	if observed.SpecName {
		name = self.Runtime.SpecName
	}
	if observed.SpecVersion {
		spec = strconv.FormatInt(self.Runtime.SpecVersion, 10)
	}
	if observed.TransactionVersion {
		transaction = strconv.FormatInt(self.Runtime.TransactionVersion, 10)
	}
	return fmt.Sprintf("runtime=%s/%s transaction=%s", name, spec, transaction)
}

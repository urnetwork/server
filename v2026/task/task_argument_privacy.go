package task

import "encoding/json"

type privateTaskArgumentMarker bool

func (present *privateTaskArgumentMarker) UnmarshalJSON(_ []byte) error {
	// Presence is sufficient even if corruption or a duplicate member changes
	// the value to false or null. The consumer validates the required true value.
	*present = true
	return nil
}

// ArgumentsForLog hides private recovery data from task logs and operator output.
// The envelope marker applies even when a worker cannot dispatch the target.
// Reader support must be deployed before any producer emits such arguments.
func ArgumentsForLog(raw string) string {
	// Decode only the marker so large public arguments are skipped without
	// allocating copies of each field. JSON's field matching also covers escaped
	// keys and the same case folding used when decoding a task's actual payload.
	var marker struct {
		Private privateTaskArgumentMarker `json:"_private_task_arguments"`
	}
	if json.Unmarshal([]byte(raw), &marker) != nil {
		return "[invalid task arguments]"
	}
	if marker.Private {
		return "[private task arguments]"
	}
	return raw
}

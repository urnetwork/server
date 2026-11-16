// Prospective qualification metadata is not an admission or acceptance API.
package competitionagent

import "errors"

// Bind the entire boundary with the existing byte representation, without
// inventing a future receipt pin or capturing any admitted native proof.
func prospectiveNativeQualificationIdentity(boundary RuntimeResourceBoundary) (string, error) {
	if boundary.NativeClass == nil || boundary.NativeClass.validateRelease(boundary) != nil {
		return "", errors.New("prospective qualification has no complete closed runtime release")
	}
	return encodeRuntimeNativeIdentity(boundary)
}

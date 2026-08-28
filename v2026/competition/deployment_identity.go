package competition

// Runtime image identity is supplied by the deployment runner after it pulls,
// inspects, and pins the exact Docker content id. It is control-plane evidence,
// not part of the score or frozen evaluation source.

import (
	"errors"
	"os"
	"strings"
)

const runtimeImageDigestEnvironment = "WARP_IMAGE_DIGEST"

func runtimeImageDigest() (string, error) {
	return validateRuntimeImageDigest(os.Getenv(runtimeImageDigestEnvironment))
}

func validateRuntimeImageDigest(value string) (string, error) {
	imageDigest := strings.TrimSpace(value)
	if !imageDigestPattern.MatchString(imageDigest) {
		return "", errors.New("runtime image digest must be an exact sha256 content identity")
	}
	return imageDigest, nil
}

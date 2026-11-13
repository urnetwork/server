// Scratch-image COPY must not preserve private host build permissions: the
// broker runs as a non-root uid and the CA bundle must remain publicly readable.
package main

import (
	"os"
	"strings"
	"testing"
)

// Explicit Docker COPY modes are independent of the builder's umask and file
// owner. The previous statements inherited root-only 0700/0600 inputs.
func TestWinnerReviewScratchImagesOverridePrivateInputModes(t *testing.T) {
	for _, path := range []string{"evaluator/review/Dockerfile", "evaluator/review/Dockerfile.snow"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(content), "\n")
		for _, required := range []string{"COPY --chmod=0555 sim-latency /review-broker", "COPY --chmod=0444 ca-certificates.crt /etc/ssl/certs/ca-certificates.crt"} {
			count := 0
			for _, line := range lines {
				if strings.TrimSpace(line) == required {
					count++
				}
			}
			if count != 1 {
				t.Fatalf("%s needs exactly one public-mode COPY %q, found %d", path, required, count)
			}
		}
	}
}

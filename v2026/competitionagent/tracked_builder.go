// BuildKit receives only trusted COPY/LABEL assembly. Arbitrary Go checks,
// initialization and compilation already joined tracked direct containers;
// another Dockerfile instruction cannot silently restore an unguarded runner.
package competitionagent

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"
)

// The protected file pin is independently checked by the caller. This small
// grammar additionally enforces the no-execution class even for a bad release
// selection; no external frontend, stage, source image or heredoc is admitted.
func validateTrackedSubmissionDockerfile(content []byte) error {
	if !utf8.Valid(content) || strings.ContainsAny(string(content), "\x00\r") {
		return errors.New("tracked assembly Dockerfile is not canonical text")
	}
	arguments := map[string]bool{}
	for _, name := range strings.Fields("EVALUATOR_BASE_IMAGE BASE_SHA CANDIDATE_SHA PATCH_SHA256 POLICY_SHA256 BUILDER_SHA256 PATCH_VALIDATOR_SHA256 CANDIDATE_COMMITS_SHA256 ORIGINAL_SOURCE_IDENTITY_SHA256 CANDIDATE_SOURCE_IDENTITY_SHA256 SUBMISSION_IDENTITY") {
		arguments[name] = true
	}
	allowedCopies := map[string]bool{
		"--chown=0:0 --chmod=0444 canonical.patch policy.json original-source.json candidate-commits.json validated-identity.json tracked-compiler.json /opt/urnetwork/submission/": true,
		"--chown=0:0 --chmod=0555 sim-latency /opt/urnetwork/bin/sim-latency":                                                                                                       true,
		"--chown=0:0 --chmod=0444 image-identity.json /opt/urnetwork/image-identity.json":                                                                                           true,
	}
	from := false
	logical := ""
	for _, raw := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(line, "#") {
			if strings.Contains(strings.ToLower(line), "syntax=") || strings.Contains(strings.ToLower(line), "escape=") || logical != "" {
				return errors.New("tracked assembly introduced frontend or parser authority")
			}
			continue
		}
		if line == "" {
			if logical != "" {
				return errors.New("tracked assembly has an ambiguous continuation")
			}
			continue
		}
		continued := strings.HasSuffix(line, "\\")
		if continued {
			line = strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		}
		logical += line
		if continued {
			logical += " "
			continue
		}
		fields := strings.Fields(logical)
		if len(fields) < 2 {
			return errors.New("tracked assembly instruction omitted its fixed value")
		}
		value := strings.TrimSpace(strings.TrimPrefix(logical, fields[0]))
		switch fields[0] {
		case "ARG":
			name, initial, hasInitial := strings.Cut(value, "=")
			if !arguments[name] || hasInitial && (name != "EVALUATOR_BASE_IMAGE" || initial != "urnetwork/sim-latency-evaluator-base:unset") {
				return errors.New("tracked assembly introduced another argument authority")
			}
		case "FROM":
			if from || value != "${EVALUATOR_BASE_IMAGE}" {
				return errors.New("tracked assembly changed its sole frozen base")
			}
			from = true
		case "COPY":
			if !from || !allowedCopies[strings.Join(strings.Fields(value), " ")] {
				return errors.New("tracked assembly expanded its finite COPY vocabulary")
			}
		case "USER":
			if !from || value != "0:0" && value != "65532:65532" {
				return errors.New("tracked assembly changed its fixed image user")
			}
		case "LABEL":
			if !from || strings.Contains(value, "<<") {
				return errors.New("tracked assembly introduced ambiguous label syntax")
			}
		case "CMD":
			var command []string
			if !from || json.Unmarshal([]byte(value), &command) != nil || len(command) != 1 || command[0] != "identity" {
				return errors.New("tracked assembly changed its immutable metadata dispatch")
			}
		default:
			return errors.New("tracked assembly contains an executable or unreviewed instruction")
		}
		logical = ""
	}
	if !from || logical != "" {
		return errors.New("tracked assembly has no complete sole frozen base")
	}
	return nil
}

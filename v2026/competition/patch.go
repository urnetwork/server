package competition

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	hunkHeaderPattern = regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@(?: .*)?$`)
	indexLinePattern  = regexp.MustCompile(`^index [0-9a-f]{7,64}\.\.[0-9a-f]{7,64}(?: 100644)?$`)
)

const protectedSimulatorTreePattern = "connect/sim-latency/**"

var hardForbiddenPatchPaths = []string{
	".git/**",
	".github/**",
	"**/go.mod",
	"**/go.sum",
	"go.mod",
	"go.sum",
	"vendor/**",
	"api/**",
	"cli/**",
	"competition/**",
	"config/**",
	"vault/**",
	"site/**",
	"db_migrations.go",
	"db_migrations_*.go",
	protectedSimulatorTreePattern,
	"stats/**",
}

type CanonicalPatch struct {
	Bytes  []byte
	Sha256 string
	Paths  []string
}

func ValidateAndCanonicalizePatch(raw string, policy PatchPolicy) (*CanonicalPatch, *CompetitionError) {
	bytes := []byte(raw)
	if len(bytes) == 0 {
		return nil, submissionError("empty_patch", "patch must not be empty")
	}
	if policy.MaxPatchBytes < len(bytes) {
		return nil, submissionError("patch_too_large", fmt.Sprintf("patch exceeds %d bytes", policy.MaxPatchBytes))
	}
	if !utf8.Valid(bytes) || strings.IndexByte(raw, 0) >= 0 {
		return nil, submissionError("invalid_patch_encoding", "patch must be valid UTF-8 text without NUL bytes")
	}
	if strings.Contains(raw, "\r") {
		return nil, submissionError("noncanonical_patch", "patch must use LF line endings")
	}
	if !strings.HasSuffix(raw, "\n") {
		return nil, submissionError("noncanonical_patch", "patch must end with exactly one LF")
	}
	if strings.HasSuffix(raw, "\n\n") {
		return nil, submissionError("noncanonical_patch", "patch must not contain extra trailing blank lines")
	}
	for _, r := range raw {
		if r < 0x20 && r != '\n' && r != '\t' {
			return nil, submissionError("invalid_patch_encoding", "patch contains a disallowed control character")
		}
	}
	lines := strings.Split(strings.TrimSuffix(raw, "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "diff --git ") {
		return nil, submissionError("invalid_patch_structure", "patch must start with a diff --git header")
	}
	paths := []string{}
	seen := map[string]bool{}
	currentPath := ""
	oldHeaderSeen, newHeaderSeen, hunkSeen := false, false, false
	oldRemaining, newRemaining := 0, 0
	lastHunkContent := false
	finishSection := func() *CompetitionError {
		if currentPath != "" && (!oldHeaderSeen || !newHeaderSeen || !hunkSeen) {
			return submissionError("invalid_patch_structure", fmt.Sprintf("path %q is missing canonical file headers or a hunk", currentPath))
		}
		if hunkSeen && (oldRemaining != 0 || newRemaining != 0) {
			return submissionError("invalid_patch_structure", fmt.Sprintf("path %q has a hunk whose declared line counts do not match its body", currentPath))
		}
		return nil
	}
	for i, line := range lines {
		lower := strings.ToLower(line)
		switch {
		case strings.HasPrefix(line, "diff --git "):
			if sectionErr := finishSection(); sectionErr != nil {
				return nil, sectionErr
			}
			filePath, err := parseDiffHeader(line)
			if err != nil {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: %s", i+1, err))
			}
			if seen[filePath] {
				return nil, submissionError("duplicate_patch_path", fmt.Sprintf("path %q appears more than once", filePath))
			}
			if !pathAllowed(filePath, policy) {
				return nil, submissionError("path_not_allowed", fmt.Sprintf("path %q is outside the editable surface", filePath))
			}
			seen[filePath] = true
			paths = append(paths, filePath)
			currentPath = filePath
			oldHeaderSeen, newHeaderSeen, hunkSeen = false, false, false
		case strings.HasPrefix(line, "diff "):
			return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: only diff --git file headers are accepted", i+1))
		case !hunkSeen && strings.HasPrefix(line, "--- "):
			if currentPath == "" || oldHeaderSeen || line != "--- a/"+currentPath {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: old-file header does not match diff path", i+1))
			}
			oldHeaderSeen = true
		case !hunkSeen && strings.HasPrefix(line, "+++ "):
			if currentPath == "" || !oldHeaderSeen || newHeaderSeen || line != "+++ b/"+currentPath {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: new-file header does not match diff path", i+1))
			}
			newHeaderSeen = true
		case strings.HasPrefix(line, "@@ "):
			if currentPath == "" || !oldHeaderSeen || !newHeaderSeen {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: hunk appears before canonical file headers", i+1))
			}
			if hunkSeen && (oldRemaining != 0 || newRemaining != 0) {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: previous hunk line counts do not match its body", i+1))
			}
			var err error
			oldRemaining, newRemaining, err = parseHunkHeader(line)
			if err != nil {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: %s", i+1, err))
			}
			hunkSeen = true
			lastHunkContent = false
		case strings.HasPrefix(line, "@@@"):
			return nil, submissionError("invalid_patch_structure", "combined diffs are not accepted")
		case !hunkSeen && strings.HasPrefix(line, "index "):
			if currentPath == "" || !indexLinePattern.MatchString(line) {
				return nil, submissionError("unsupported_patch_operation", "only regular 100644 file indexes are accepted")
			}
		case !hunkSeen && strings.HasPrefix(line, "Binary files "), line == "GIT binary patch":
			return nil, submissionError("binary_patch", "binary patches are not accepted")
		case !hunkSeen && (strings.HasPrefix(line, "new file mode ") || strings.HasPrefix(line, "deleted file mode ") ||
			strings.HasPrefix(line, "old mode ") || strings.HasPrefix(line, "new mode ") ||
			strings.HasPrefix(line, "rename from ") || strings.HasPrefix(line, "rename to ") ||
			strings.HasPrefix(line, "copy from ") || strings.HasPrefix(line, "copy to ") ||
			strings.HasPrefix(line, "similarity index ") || strings.HasPrefix(line, "dissimilarity index ")):
			return nil, submissionError("unsupported_patch_operation", "new/deleted files, renames, copies, and mode changes are not accepted")
		case strings.Contains(lower, "subproject commit"):
			return nil, submissionError("submodule_patch", "submodule changes are not accepted")
		case (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")) &&
			(strings.HasPrefix(strings.TrimSpace(line[1:]), "//go:build") || strings.HasPrefix(strings.TrimSpace(line[1:]), "// +build")):
			return nil, submissionError("build_tag_patch", "build constraint changes are not accepted")
		case hunkSeen:
			switch {
			case strings.HasPrefix(line, " "):
				oldRemaining--
				newRemaining--
				lastHunkContent = true
			case strings.HasPrefix(line, "+"):
				newRemaining--
				lastHunkContent = true
			case strings.HasPrefix(line, "-"):
				oldRemaining--
				lastHunkContent = true
			case line == `\ No newline at end of file` && lastHunkContent:
				lastHunkContent = false
			default:
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: invalid unified-diff hunk line", i+1))
			}
			if oldRemaining < 0 || newRemaining < 0 {
				return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: hunk body exceeds its declared line counts", i+1))
			}
		default:
			return nil, submissionError("invalid_patch_structure", fmt.Sprintf("line %d: unexpected file metadata", i+1))
		}
	}
	if sectionErr := finishSection(); sectionErr != nil {
		return nil, sectionErr
	}
	if len(paths) == 0 {
		return nil, submissionError("invalid_patch_structure", "patch contains no diff --git file header")
	}
	if !sort.StringsAreSorted(paths) {
		return nil, submissionError("noncanonical_patch", "file sections must be sorted by path")
	}
	hash := sha256.Sum256(bytes)
	return &CanonicalPatch{Bytes: bytes, Sha256: hex.EncodeToString(hash[:]), Paths: paths}, nil
}

func parseHunkHeader(line string) (int, int, error) {
	match := hunkHeaderPattern.FindStringSubmatch(line)
	if match == nil {
		return 0, 0, fmt.Errorf("malformed hunk header")
	}
	count := func(value string) (int, error) {
		if value == "" {
			return 1, nil
		}
		return strconv.Atoi(value)
	}
	oldCount, err := count(match[2])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid old-file hunk count")
	}
	newCount, err := count(match[4])
	if err != nil {
		return 0, 0, fmt.Errorf("invalid new-file hunk count")
	}
	return oldCount, newCount, nil
}

func parseDiffHeader(line string) (string, error) {
	fields := strings.Fields(line)
	if len(fields) != 4 || fields[0] != "diff" || fields[1] != "--git" {
		return "", fmt.Errorf("malformed diff header")
	}
	if strings.ContainsAny(fields[2]+fields[3], `"'\\`) || !strings.HasPrefix(fields[2], "a/") || !strings.HasPrefix(fields[3], "b/") {
		return "", fmt.Errorf("quoted, escaped, or non-canonical paths are not accepted")
	}
	a, b := strings.TrimPrefix(fields[2], "a/"), strings.TrimPrefix(fields[3], "b/")
	if a != b {
		return "", fmt.Errorf("rename-like diff header is not accepted")
	}
	if err := validateRepositoryPath(a); err != nil {
		return "", err
	}
	return a, nil
}

func validateRepositoryPath(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return fmt.Errorf("unsafe repository path")
	}
	clean := path.Clean(value)
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("path traversal is not accepted")
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == ".git" || segment == ".." || segment == "." {
			return fmt.Errorf("repository metadata path is not accepted")
		}
	}
	return nil
}

func pathAllowed(value string, policy PatchPolicy) bool {
	forbidden := append(append([]string{}, hardForbiddenPatchPaths...), policy.ForbiddenPaths...)
	if matchAny(forbidden, value) {
		return false
	}
	return matchAny(policy.AllowedPaths, value)
}

func matchAny(patterns []string, value string) bool {
	for _, pattern := range patterns {
		// filepath.Match does not let ** cross separators. Support the common
		// directory-tree suffix explicitly while retaining strict Match syntax.
		if strings.HasSuffix(pattern, "/**") {
			prefix := strings.TrimSuffix(pattern, "**")
			if strings.HasPrefix(value, prefix) {
				return true
			}
		}
		if matched, _ := filepath.Match(pattern, value); matched {
			return true
		}
	}
	return false
}

func submissionError(code, message string) *CompetitionError {
	return &CompetitionError{Kind: "submission", Code: code, Message: message, Retriable: false}
}

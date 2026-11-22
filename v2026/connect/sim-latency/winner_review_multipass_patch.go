// Post-patch context is a pure exact application of the authenticated text patch.
// No checkout, Git hook, candidate execution, fuzzy context or offset search is used.
package main

import (
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/urnetwork/server/v2026/controller"
)

// Preserve every file-section byte, including metadata and no-newline markers.
func winnerMultipassPatchSections(patch string) map[string]string {
	sections := map[string]string{}
	current := ""
	for _, line := range strings.SplitAfter(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			fields := strings.Fields(line)
			if len(fields) == 4 {
				current = strings.TrimPrefix(fields[2], "a/")
			}
		}
		if current != "" {
			sections[current] += line
		}
	}
	return sections
}

// Canonical syntax is checked by the existing validator; exact hunk application
// additionally proves that the before/after code agrees with the locked base.
func deriveWinnerMultipassCandidates(request winnerReviewBrokerRequest, artifacts []winnerMultipassArtifact) ([]winnerMultipassArtifact, error) {
	patch, err := controller.ValidateAndCanonicalizePatch(string(request.Packet.Patch), request.Packet.PatchPolicy)
	if err != nil {
		return nil, err
	}
	sections := winnerMultipassPatchSections(string(request.Packet.Patch))
	candidates := []winnerMultipassArtifact{}
	for _, artifact := range artifacts {
		if artifact.Kind != "touched_base" {
			continue
		}
		operation, found := patch.FileOperations[artifact.Path]
		section, sectionFound := sections[artifact.Path]
		if !found || !sectionFound {
			return nil, errors.New("candidate derivation is missing a canonical file operation")
		}
		content, applyErr := applyWinnerMultipassPatchSection(artifact.Content, section)
		if applyErr != nil {
			return nil, applyErr
		}
		if operation.Operation == "delete" && content != "" || operation.Operation == "create" && !artifact.Absent {
			return nil, errors.New("candidate creation/deletion contradicts frozen source existence")
		}
		candidates = append(candidates, winnerMultipassArtifact{Id: "candidate:" + artifact.Path, Kind: "candidate", Path: artifact.Path, Commit: artifact.Commit,
			Sha256: winnerMultipassSha256([]byte(content)), Content: content, Absent: operation.Operation == "delete", DerivationSha256: patch.Sha256})
	}
	if len(candidates) != len(patch.Paths) {
		return nil, errors.New("candidate derivation does not cover every changed path")
	}
	return candidates, nil
}

// Input and output lines retain their actual newline state. Coordinates must
// match exactly; repeated context never permits heuristic relocation.
func applyWinnerMultipassPatchSection(before, section string) (string, error) {
	baseLines := strings.SplitAfter(before, "\n")
	if len(baseLines) > 0 && baseLines[len(baseLines)-1] == "" {
		baseLines = baseLines[:len(baseLines)-1]
	}
	lines := strings.Split(strings.TrimSuffix(section, "\n"), "\n")
	pattern := regexp.MustCompile(`^@@ -([0-9]+)(?:,([0-9]+))? \+([0-9]+)(?:,([0-9]+))? @@`)
	output := []string{}
	position := 0
	hadHunk := false
	for index := 0; index < len(lines); index++ {
		line := lines[index]
		if !strings.HasPrefix(line, "@@ ") {
			continue
		}
		hadHunk = true
		match := pattern.FindStringSubmatch(line)
		if match == nil {
			return "", errors.New("candidate derivation hunk is malformed")
		}
		coordinates := [4]int{}
		for i := range coordinates {
			value := match[i+1]
			if value == "" {
				value = "1"
			}
			parsed, err := strconv.Atoi(value)
			if err != nil {
				return "", errors.New("candidate derivation hunk coordinate is invalid")
			}
			coordinates[i] = parsed
		}
		oldStart, oldCount, newStart, newCount := coordinates[0], coordinates[1], coordinates[2], coordinates[3]
		oldIndex := oldStart
		if oldCount > 0 {
			oldIndex--
		}
		if oldIndex < position || oldIndex > len(baseLines) {
			return "", errors.New("candidate derivation hunk overlaps or escapes the frozen file")
		}
		output = append(output, baseLines[position:oldIndex]...)
		position = oldIndex
		newIndex := newStart
		if newCount > 0 {
			newIndex--
		}
		if newIndex != len(output) {
			return "", errors.New("candidate derivation new-file coordinates disagree")
		}
		oldSeen, newSeen := 0, 0
		for index+1 < len(lines) && !strings.HasPrefix(lines[index+1], "@@ ") {
			index++
			body := lines[index]
			if body == "" {
				return "", errors.New("candidate derivation hunk has an empty structural line")
			}
			prefix := body[0]
			if prefix != ' ' && prefix != '-' && prefix != '+' {
				return "", errors.New("candidate derivation hunk contains unsupported metadata")
			}
			content := body[1:] + "\n"
			if index+1 < len(lines) && lines[index+1] == `\ No newline at end of file` {
				content = body[1:]
				index++
			}
			if prefix != '+' {
				if position >= len(baseLines) || baseLines[position] != content {
					return "", errors.New("candidate derivation context does not match the frozen base")
				}
				position++
				oldSeen++
			}
			if prefix != '-' {
				output = append(output, content)
				newSeen++
			}
		}
		if oldSeen != oldCount || newSeen != newCount {
			return "", errors.New("candidate derivation hunk counts disagree")
		}
	}
	if !hadHunk {
		if before != "" {
			return "", errors.New("nonempty candidate file has no textual hunk")
		}
		return "", nil
	}
	output = append(output, baseLines[position:]...)
	for i, line := range output {
		if i+1 < len(output) && !strings.HasSuffix(line, "\n") {
			return "", errors.New("candidate derivation has a nonterminal missing newline")
		}
	}
	return strings.Join(output, ""), nil
}

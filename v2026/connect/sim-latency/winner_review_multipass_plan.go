// Complete-source spans prefer declaration boundaries. Oversized declarations
// are explicitly marked, overlapped and never represented as complete functions.
package main

import (
	"errors"
	"go/parser"
	"go/token"
	"sort"
	"strings"
)

// Every byte belongs to one core span, including comments, whitespace, headers
// and absent/empty artifacts. Line boundaries preserve exact UTF-8 code text.
func splitWinnerMultipassArtifact(artifact winnerMultipassArtifact, settings winnerMultipassSettings) ([]winnerMultipassSpan, error) {
	content := artifact.Content
	lineEnds := []int{0}
	for offset, value := range []byte(content) {
		if value == '\n' {
			lineEnds = append(lineEnds, offset+1)
		}
	}
	if lineEnds[len(lineEnds)-1] != len(content) {
		lineEnds = append(lineEnds, len(content))
	}
	declarationEnds := []int{}
	declarationRanges := [][2]int{}
	if strings.HasSuffix(artifact.Path, ".go") && content != "" {
		fileSet := token.NewFileSet()
		if parsed, err := parser.ParseFile(fileSet, artifact.Path, content, parser.ParseComments|parser.AllErrors); err == nil {
			for _, declaration := range parsed.Decls {
				start, end := fileSet.Position(declaration.Pos()).Offset, fileSet.Position(declaration.End()).Offset
				line := sort.SearchInts(lineEnds, end)
				if line < len(lineEnds) {
					end = lineEnds[line]
				}
				declarationEnds = append(declarationEnds, end)
				declarationRanges = append(declarationRanges, [2]int{start, end})
			}
		}
	}
	spans := []winnerMultipassSpan{}
	for start := 0; start < len(content) || len(spans) == 0; {
		limit := min(start+settings.SpanBytes, len(content))
		end := limit
		if limit < len(content) {
			line := sort.SearchInts(lineEnds, limit+1) - 1
			if line < 0 || lineEnds[line] <= start {
				return nil, errors.New("source line exceeds the fixed span bound; no truncation permitted")
			}
			end = lineEnds[line]
			decl := sort.SearchInts(declarationEnds, limit+1) - 1
			if decl >= 0 && declarationEnds[decl] > start {
				end = declarationEnds[decl]
			}
		}
		startLine, endLine := sort.SearchInts(lineEnds, start), sort.SearchInts(lineEnds, end)
		contextStartLine := max(0, startLine-settings.OverlapLines)
		for start-lineEnds[contextStartLine] > settings.MaximumOverlapBytes {
			contextStartLine++
		}
		contextEndLine := min(len(lineEnds)-1, endLine+settings.OverlapLines)
		for lineEnds[contextEndLine]-end > settings.MaximumOverlapBytes {
			contextEndLine--
		}
		contextStart, contextEnd := lineEnds[contextStartLine], lineEnds[contextEndLine]
		split := false
		for _, declaration := range declarationRanges {
			if declaration[0] < start && start < declaration[1] || declaration[0] < end && end < declaration[1] {
				split = true
			}
		}
		spans = append(spans, winnerMultipassSpan{ArtifactId: artifact.Id, Start: start, End: end, ContextStart: contextStart, ContextEnd: contextEnd,
			Sha256: winnerMultipassSha256([]byte(content[start:end])), ContextSha256: winnerMultipassSha256([]byte(content[contextStart:contextEnd])), SplitDeclaration: split})
		start = end
		if start == len(content) {
			break
		}
	}
	return spans, nil
}

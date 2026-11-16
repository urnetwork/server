// The logical migration census retains its positional protocol, while each
// physical PostgreSQL statement owns a bounded planner input in one snapshot.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// Bounds belong to the individual migration observation, not a global pool.
// Nonpositive values refuse the bounded census rather than disabling a guard.
type MigrationCensusSettings struct {
	// Maximum scalar arguments per JSON batch; PostgreSQL permits at most 100.
	MaximumColumnsPerStatement int
	// Complete physical SQL bytes, including catalog CTEs and framing.
	MaximumStatementBytes int
	// Whole logical source bytes admitted before lexical decoding.
	MaximumInputBytes int
	// Whole client file-reader program bytes, including repeated CTEs.
	MaximumProgramBytes int
	// Total positional scalar columns and physical query count.
	MaximumColumns int
	MaximumBatches int
	// Matched SQL delimiters and nested comment depth.
	MaximumNesting int
	// One decoded frame and the complete raw CSV response, in bytes.
	MaximumFrameBytes    int
	MaximumResponseBytes int
	// Complete identity rows; one extra row is an overflow witness, not a truncation.
	MaximumCatalogRows int
}

// Column and byte bounds jointly limit planner expansion. Catalog history is
// still complete; an oversized future predicate produces unknown, never green.
func DefaultMigrationCensusSettings() MigrationCensusSettings {
	return MigrationCensusSettings{MaximumColumnsPerStatement: 16, MaximumStatementBytes: 512 * 1024,
		MaximumInputBytes: 32 * 1024 * 1024, MaximumProgramBytes: 64 * 1024 * 1024, MaximumColumns: 4096, MaximumBatches: 512,
		MaximumNesting: 256, MaximumFrameBytes: 64 * 1024, MaximumResponseBytes: 4 * 1024 * 1024, MaximumCatalogRows: 4096}
}

// A zero value is handled only by the owning settings constructor.
func (self MigrationCensusSettings) validate() error {
	if self.MaximumColumnsPerStatement <= 0 || self.MaximumStatementBytes <= 0 || self.MaximumInputBytes <= 0 || self.MaximumProgramBytes <= 0 ||
		self.MaximumColumns <= 0 || self.MaximumBatches <= 0 || self.MaximumNesting <= 0 || self.MaximumFrameBytes <= 0 ||
		self.MaximumResponseBytes <= 0 || self.MaximumCatalogRows <= 0 || self.MaximumCatalogRows == int(^uint(0)>>1) ||
		self.MaximumColumnsPerStatement > self.MaximumColumns || self.MaximumColumnsPerStatement > 100 {
		return errors.New("migration census owner bounds are invalid")
	}
	return nil
}

type migrationCensusContextKey struct{}

// Only the completed, validated physical observation publishes these fields.
// The marker is private and fresh for each blocking signal invocation.
type migrationCensusObservation struct {
	settings        MigrationCensusSettings
	catalogRows     []pgRow
	catalogComplete bool
}

func migrationCensusFromContext(ctx context.Context) *migrationCensusObservation {
	observation, _ := ctx.Value(migrationCensusContextKey{}).(*migrationCensusObservation)
	return observation
}

// Token offsets refer to the immutable source query. Comments and quoted
// bodies never contribute delimiters or statement structure.
type migrationSqlToken struct {
	start, end, depth int
	word              string
}

func migrationSqlWord(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '_'
}

// PostgreSQL standard strings, E strings, identifiers, dollar bodies and
// nested comments have distinct escaping rules; malformed input fails closed.
func migrationSqlTokens(query string, maximumNesting int) ([]migrationSqlToken, error) {
	tokens := []migrationSqlToken{}
	depth := 0
	delimiters := []byte{}
	for offset := 0; offset < len(query); {
		start := offset
		value := query[offset]
		if value == ' ' || value == '\n' || value == '\r' || value == '\t' {
			offset++
			continue
		}
		if strings.HasPrefix(query[offset:], "--") {
			end := strings.IndexByte(query[offset:], '\n')
			if end < 0 {
				break
			}
			offset += end + 1
			continue
		}
		if strings.HasPrefix(query[offset:], "/*") {
			comments := 1
			offset += 2
			for comments > 0 && offset < len(query) {
				if strings.HasPrefix(query[offset:], "/*") {
					comments++
					offset += 2
				} else if strings.HasPrefix(query[offset:], "*/") {
					comments--
					offset += 2
				} else {
					offset++
				}
				if comments > maximumNesting {
					return nil, errors.New("migration SQL comment nesting exceeded")
				}
			}
			if comments != 0 {
				return nil, errors.New("migration SQL comment is unterminated")
			}
			continue
		}
		escapedString := (value == 'e' || value == 'E') && offset+1 < len(query) && query[offset+1] == '\'' && (offset == 0 || !migrationSqlWord(query[offset-1]) && query[offset-1] != '$')
		if value == '\'' || value == '"' || escapedString {
			quote := value
			if escapedString {
				offset++
				quote = '\''
			}
			offset++
			closed := false
			for offset < len(query) {
				if escapedString && query[offset] == '\\' {
					offset += 2
					continue
				}
				if query[offset] == quote {
					if offset+1 < len(query) && query[offset+1] == quote {
						offset += 2
						continue
					}
					offset++
					closed = true
					break
				}
				offset++
			}
			if !closed {
				return nil, errors.New("migration SQL quoted value is unterminated")
			}
			tokens = append(tokens, migrationSqlToken{start: start, end: offset, depth: depth, word: "quoted"})
			continue
		}
		if value == '$' && (offset == 0 || !migrationSqlWord(query[offset-1]) && query[offset-1] != '$') {
			end := offset + 1
			for end < len(query) && migrationSqlWord(query[end]) {
				end++
			}
			if end < len(query) && query[end] == '$' && (end == offset+1 || query[offset+1] < '0' || query[offset+1] > '9') {
				delimiter := query[offset : end+1]
				closeAt := strings.Index(query[end+1:], delimiter)
				if closeAt < 0 {
					return nil, errors.New("migration SQL dollar body is unterminated")
				}
				offset = end + 1 + closeAt + len(delimiter)
				tokens = append(tokens, migrationSqlToken{start: start, end: offset, depth: depth, word: "quoted"})
				continue
			}
		}
		if value == '(' || value == '[' {
			delimiters = append(delimiters, value)
			depth++
			if depth > maximumNesting {
				return nil, errors.New("migration SQL nesting exceeded")
			}
			offset++
			continue
		}
		if value == ')' || value == ']' {
			if len(delimiters) == 0 || value == ')' && delimiters[len(delimiters)-1] != '(' || value == ']' && delimiters[len(delimiters)-1] != '[' {
				return nil, errors.New("migration SQL delimiters are unbalanced")
			}
			delimiters = delimiters[:len(delimiters)-1]
			depth--
			if depth < 0 {
				return nil, errors.New("migration SQL parentheses are unbalanced")
			}
			offset++
			continue
		}
		if migrationSqlWord(value) {
			for offset < len(query) && (migrationSqlWord(query[offset]) || query[offset] == '$') {
				offset++
			}
			tokens = append(tokens, migrationSqlToken{start: start, end: offset, depth: depth, word: strings.ToUpper(query[start:offset])})
			continue
		}
		if value == '\\' {
			return nil, errors.New("migration SQL contains a client metacommand")
		}
		offset++
		tokens = append(tokens, migrationSqlToken{start: start, end: offset, depth: depth, word: string(value)})
	}
	if depth != 0 {
		return nil, errors.New("migration SQL parentheses are unterminated")
	}
	return tokens, nil
}

type migrationCensusBatch struct {
	index, offset, columns int
	query                  string
}

type migrationCensusPlan struct {
	settings MigrationCensusSettings
	digest   string
	columns  int
	batches  []migrationCensusBatch
}

// Split only the closed WITH ... SELECT ... FROM version source shape. Each
// expression remains byte-identical and every original position occurs once.
func planMigrationCensus(query string, settings MigrationCensusSettings) (*migrationCensusPlan, error) {
	if err := settings.validate(); err != nil || len(query) > settings.MaximumInputBytes {
		return nil, errors.New("migration census input exceeds its owner bounds")
	}
	tokens, err := migrationSqlTokens(query, settings.MaximumNesting)
	if err != nil || len(tokens) == 0 || tokens[0].word != "WITH" {
		return nil, errors.New("migration census source shape is invalid")
	}
	selectAt, fromAt := -1, -1
	for index, token := range tokens {
		if token.word == ";" && (token.depth != 0 || index != len(tokens)-1) {
			return nil, errors.New("migration census has an additional statement")
		}
		if token.depth != 0 {
			continue
		}
		if token.word == "SELECT" && selectAt < 0 {
			selectAt = index
		} else if token.word == "SELECT" || selectAt >= 0 && (token.word == "UNION" || token.word == "INTERSECT" || token.word == "EXCEPT" || token.word == "INTO") {
			return nil, errors.New("migration census has an unexpected top-level operation")
		} else if token.word == "FROM" && selectAt >= 0 && fromAt < 0 {
			fromAt = index
		}
	}
	if selectAt < 0 || fromAt < 0 || fromAt+1 >= len(tokens) || tokens[fromAt+1].word != "VERSION" ||
		fromAt+2 < len(tokens) && (fromAt+3 != len(tokens) || tokens[fromAt+2].word != ";") {
		return nil, errors.New("migration census must end at its exact version source")
	}
	prefix := query[:tokens[selectAt].start]
	expressions := []string{}
	start := tokens[selectAt].end
	for _, token := range tokens[selectAt+1 : fromAt] {
		if token.depth == 0 && token.word == "," {
			expressions = append(expressions, query[start:token.start])
			start = token.end
		}
	}
	expressions = append(expressions, query[start:tokens[fromAt].start])
	if len(expressions) > settings.MaximumColumns {
		return nil, errors.New("migration census column count exceeded")
	}
	digest := sha256.Sum256([]byte(query))
	plan := &migrationCensusPlan{settings: settings, digest: hex.EncodeToString(digest[:]), columns: len(expressions)}
	statement := func(index, offset int, values []string) string {
		return prefix + fmt.Sprintf("SELECT json_build_object('kind','batch','index',%d,'offset',%d,'values',json_build_array(%s))::text FROM version;", index, offset, strings.Join(values, ",\n"))
	}
	values := []string{}
	offset := 0
	flush := func() {
		plan.batches = append(plan.batches, migrationCensusBatch{index: len(plan.batches), offset: offset, columns: len(values), query: statement(len(plan.batches), offset, values)})
		offset += len(values)
		values = nil
	}
	for _, expression := range expressions {
		if strings.TrimSpace(expression) == "" {
			return nil, errors.New("migration census contains an empty column")
		}
		next := append(append([]string(nil), values...), expression)
		if len(values) != 0 && (len(next) > settings.MaximumColumnsPerStatement || len(statement(len(plan.batches), offset, next)) > settings.MaximumStatementBytes) {
			flush()
			next = []string{expression}
		}
		if len(statement(len(plan.batches), offset, next)) > settings.MaximumStatementBytes {
			return nil, fmt.Errorf("migration census column %d exceeds its statement byte bound", offset+len(values))
		}
		values = next
		if len(plan.batches) >= settings.MaximumBatches {
			return nil, errors.New("migration census batch count exceeded")
		}
	}
	if len(values) != 0 {
		flush()
	}
	return plan, nil
}

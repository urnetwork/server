// The migration transport publishes one complete census only after every
// indexed row, identity frame and post-commit trailer has been authenticated.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// The protocol prologue binds the complete logical source, not one batch.
func (self *migrationCensusPlan) beginQuery() string {
	return fmt.Sprintf("SELECT json_build_object('kind','begin','schema',1,'sha256','%s','columns',%d,'batches',%d)::text;", self.digest, self.columns, len(self.batches))
}

// Count one overflow witness rather than scanning an unbounded history.
func (self *migrationCensusPlan) catalogCountQuery() string {
	return fmt.Sprintf("SELECT json_build_object('kind','catalog','present',true,'count',(SELECT count(*) FROM (SELECT 1 FROM migration_catalog LIMIT %d) AS bounded))::text;", self.settings.MaximumCatalogRows+1)
}

// Ordinals frame transport order separately from the immutable source index;
// gaps or duplicates in the source history still reach the original reducer.
func (self *migrationCensusPlan) catalogIdentityQuery() string {
	return fmt.Sprintf("WITH bounded_catalog AS MATERIALIZED (SELECT migration_index,trim(identity_sha256) AS identity_sha256 FROM migration_catalog LIMIT %d) SELECT json_build_object('kind','identity','ordinal',row_number() OVER (ORDER BY migration_index)-1,'values',json_build_array(migration_index,identity_sha256))::text FROM bounded_catalog ORDER BY migration_index;", self.settings.MaximumCatalogRows+1)
}

// Only execution after a successful commit may produce this trailer.
func (self *migrationCensusPlan) completeQuery() string {
	return fmt.Sprintf("SELECT json_build_object('kind','complete','schema',1,'sha256','%s')::text;", self.digest)
}

// psql's file reader sends one terminated command at a time. Never pass this
// program through -c or a PostgreSQL multi-statement simple-query submission:
// the server would retain the entire logical census's parse tree again.
func (self *migrationCensusPlan) script() (string, error) {
	for _, statement := range []string{self.beginQuery(), self.catalogCountQuery(), self.catalogIdentityQuery(), self.completeQuery()} {
		if len(statement) > self.settings.MaximumStatementBytes {
			return "", errors.New("migration census metadata exceeds its statement byte bound")
		}
	}
	var script strings.Builder
	script.WriteString("BEGIN TRANSACTION ISOLATION LEVEL REPEATABLE READ READ ONLY;\nSET LOCAL standard_conforming_strings=on;\n")
	script.WriteString(self.beginQuery() + "\n")
	for _, batch := range self.batches {
		if script.Len()+len(batch.query)+1 > self.settings.MaximumProgramBytes {
			return "", errors.New("migration census program exceeds its owner bound")
		}
		script.WriteString(batch.query)
		script.WriteByte('\n')
	}
	// The relation need not exist at an early published prefix. This is a
	// closed client conditional, not SQL supplied by a catalog value.
	script.WriteString("SELECT to_regclass('public.migration_catalog') IS NOT NULL AS migration_census_catalog \\gset\n\\if :migration_census_catalog\n")
	script.WriteString(self.catalogCountQuery() + "\n")
	script.WriteString(self.catalogIdentityQuery() + "\n")
	script.WriteString("\\else\nSELECT json_build_object('kind','catalog','present',false,'count',0)::text;\n\\endif\nCOMMIT;\n")
	script.WriteString(self.completeQuery() + "\n")
	if script.Len() > self.settings.MaximumProgramBytes {
		return "", errors.New("migration census program exceeds its owner bound")
	}
	return script.String(), nil
}

// Reject duplicate or trailing keys instead of letting a second JSON field
// silently replace an earlier authority field. Strings remain opaque data.
func migrationCensusFrame(encoded string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(strings.NewReader(encoded))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("migration census frame is not an object")
	}
	fields := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		key, valid := token.(string)
		if err != nil || !valid {
			return nil, errors.New("migration census frame key is invalid")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("migration census frame key is duplicated")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, errors.New("migration census frame is incomplete")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("migration census frame has trailing data")
	}
	return fields, nil
}

// Each frame kind has an exact key set; missing, extra or mistyped metadata
// cannot shift a logical column or make a truncated observation look closed.
func migrationCensusFrameShape(fields map[string]json.RawMessage, kind string, keys ...string) bool {
	if len(fields) != len(keys)+1 {
		return false
	}
	var actual string
	if json.Unmarshal(fields["kind"], &actual) != nil || actual != kind {
		return false
	}
	for _, key := range keys {
		if _, present := fields[key]; !present {
			return false
		}
	}
	return true
}

// Metadata integers have one representation, not a rounded floating value.
func migrationCensusFrameInt(fields map[string]json.RawMessage, key string) (int, error) {
	raw, exists := fields[key]
	if !exists {
		return 0, errors.New("migration census integer is absent")
	}
	value, err := strconv.Atoi(string(raw))
	if err != nil || value < 0 || strconv.Itoa(value) != string(raw) {
		return 0, errors.New("migration census integer is invalid")
	}
	return value, nil
}

// JSON null must not retain a previous decoded digest or metadata value.
func migrationCensusFrameString(fields map[string]json.RawMessage, key string) (string, error) {
	raw, exists := fields[key]
	var value string
	if !exists || len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &value) != nil {
		return "", errors.New("migration census string is invalid")
	}
	return value, nil
}

// The logical source returns scalar columns. Do not accept nested JSON that
// could masquerade as a source row; numeric identities never pass float64.
func migrationCensusFrameValues(raw json.RawMessage, count int) (pgRow, error) {
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil || len(values) != count {
		return nil, errors.New("migration census value count is invalid")
	}
	row := make(pgRow, len(values))
	for index, encoded := range values {
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		switch value := value.(type) {
		case nil:
			row[index] = ""
		case bool:
			row[index] = "f"
			if value {
				row[index] = "t"
			}
		case string:
			row[index] = value
		case json.Number:
			row[index] = value.String()
		default:
			return nil, errors.New("migration census value is not scalar")
		}
	}
	return row, nil
}

// No partial row escapes: successful CSV decoding, header, all batches,
// bounded identity census and the post-commit trailer are one protocol.
func (self *migrationCensusPlan) parse(out string) ([]pgRow, []pgRow, error) {
	if len(out) > self.settings.MaximumResponseBytes {
		return nil, nil, errors.New("migration census response exceeds its owner bound")
	}
	rows, err := parsePgRows(out)
	if err != nil || len(rows) < len(self.batches)+3 || len(rows) > len(self.batches)+self.settings.MaximumCatalogRows+3 {
		return nil, nil, errors.New("migration census response is incomplete")
	}
	frames := make([]map[string]json.RawMessage, 0, len(rows))
	for _, row := range rows {
		if len(row) != 1 || len(row[0]) > self.settings.MaximumFrameBytes {
			return nil, nil, errors.New("migration census frame exceeds its owner shape")
		}
		frame, err := migrationCensusFrame(row[0])
		if err != nil {
			return nil, nil, err
		}
		frames = append(frames, frame)
	}
	begin := frames[0]
	digest, digestErr := migrationCensusFrameString(begin, "sha256")
	schema, schemaErr := migrationCensusFrameInt(begin, "schema")
	columns, columnErr := migrationCensusFrameInt(begin, "columns")
	batches, batchErr := migrationCensusFrameInt(begin, "batches")
	if !migrationCensusFrameShape(begin, "begin", "schema", "sha256", "columns", "batches") || schemaErr != nil || schema != 1 ||
		columnErr != nil || columns != self.columns || batchErr != nil || batches != len(self.batches) ||
		digestErr != nil || digest != self.digest {
		return nil, nil, errors.New("migration census begin frame is invalid")
	}
	completeRow := make(pgRow, 0, self.columns)
	for index, batch := range self.batches {
		frame := frames[index+1]
		actualIndex, indexErr := migrationCensusFrameInt(frame, "index")
		offset, offsetErr := migrationCensusFrameInt(frame, "offset")
		if !migrationCensusFrameShape(frame, "batch", "index", "offset", "values") || indexErr != nil || actualIndex != batch.index ||
			offsetErr != nil || offset != batch.offset || offset != len(completeRow) {
			return nil, nil, errors.New("migration census batch order is invalid")
		}
		values, err := migrationCensusFrameValues(frame["values"], batch.columns)
		if err != nil {
			return nil, nil, err
		}
		completeRow = append(completeRow, values...)
	}
	catalogAt := len(self.batches) + 1
	catalog := frames[catalogAt]
	var present bool
	count, countErr := migrationCensusFrameInt(catalog, "count")
	boolean := string(catalog["present"])
	if !migrationCensusFrameShape(catalog, "catalog", "present", "count") || boolean != "true" && boolean != "false" || json.Unmarshal(catalog["present"], &present) != nil ||
		countErr != nil || count > self.settings.MaximumCatalogRows || !present && count != 0 || len(frames) != catalogAt+count+2 {
		return nil, nil, errors.New("migration census catalog frame is invalid")
	}
	catalogRows := make([]pgRow, 0, count)
	for index := 0; index < count; index++ {
		frame := frames[catalogAt+1+index]
		ordinal, ordinalErr := migrationCensusFrameInt(frame, "ordinal")
		if !migrationCensusFrameShape(frame, "identity", "ordinal", "values") || ordinalErr != nil || ordinal != index {
			return nil, nil, errors.New("migration census identity order is invalid")
		}
		row, err := migrationCensusFrameValues(frame["values"], 2)
		if err != nil {
			return nil, nil, err
		}
		catalogRows = append(catalogRows, row)
	}
	end := frames[len(frames)-1]
	schema, schemaErr = migrationCensusFrameInt(end, "schema")
	digest, digestErr = migrationCensusFrameString(end, "sha256")
	if !migrationCensusFrameShape(end, "complete", "schema", "sha256") || schemaErr != nil || schema != 1 ||
		digestErr != nil || digest != self.digest || len(completeRow) != self.columns {
		return nil, nil, errors.New("migration census completion frame is invalid")
	}
	return []pgRow{completeRow}, catalogRows, nil
}

// The transport callback owns the child until return. A late successful child
// or canceled owner cannot publish either artifacts or catalog identities.
func runMigrationCensus(ctx context.Context, query string, observation *migrationCensusObservation, run func(string) (string, error)) ([]pgRow, error) {
	observation.catalogRows = nil
	observation.catalogComplete = false
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	plan, err := planMigrationCensus(query, observation.settings)
	if err != nil {
		return nil, err
	}
	script, err := plan.script()
	if err != nil {
		return nil, err
	}
	out, err := run(script)
	if err != nil {
		return nil, err
	}
	rows, catalogRows, err := plan.parse(out)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	observation.catalogRows = catalogRows
	observation.catalogComplete = true
	return rows, nil
}

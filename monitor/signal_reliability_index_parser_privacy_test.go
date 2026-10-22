// Public reliability-index parser controls keep malformed scalar values out of returned errors.
package monitor

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// A complete partitioned row isolates one malformed scalar from relation-scope authority.
func reliabilityIndexPrivateScalarRow() Row {
	definition := "CREATE INDEX synthetic_reliability_cover ON ONLY public.client_reliability" + reliabilityIndexDesiredDefinitionSuffix
	return Row{"t", "f", "t", "t", definition, "10", "10", "0", "partitioned"}
}

// The real public signal must reject one source cell using only its owned field diagnostic.
func requireReliabilityIndexPrivateScalar(t *testing.T, column int, value, diagnostic string) {
	t.Helper()
	row := reliabilityIndexPrivateScalarRow()
	row[column] = value
	calls := 0
	settings := syntheticSettings(&syntheticSource{postgresFn: func(query string) ([]Row, error) {
		calls++
		if query != reliabilityIndexCatalogQuery {
			t.Fatal("parser control did not reach the owning catalog query")
		}
		return []Row{row}, nil
	}})
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), settings)
	if calls != 1 || err == nil || len(alerts) != 0 {
		t.Fatal("malformed scalar did not remain a single unobservable public execution")
	}
	if err.Error() != diagnostic {
		t.Fatal("malformed scalar returned more than the owned field diagnostic")
	}
	for _, rendered := range []string{fmt.Sprint(err), fmt.Sprintf("%+v", err), fmt.Sprintf("%#v", err)} {
		if strings.Contains(rendered, value) && value != "" {
			t.Fatal("malformed scalar escaped returned-error formatting")
		}
	}
}

// Malformed partitioned boolean retains rejection and field identity without private source text.
func TestReliabilityIndexPublicPartitionedParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 0, "synthetic-private-scalar-0\n\t\"", "invalid partitioned boolean")
}

// Malformed old-index-present boolean retains rejection and field identity without private source text.
func TestReliabilityIndexPublicOldIndexPresenceParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 1, "synthetic-private-scalar-1\n\t\"", "invalid old-index-present boolean")
}

// Malformed desired-index-present boolean retains rejection and field identity without private source text.
func TestReliabilityIndexPublicDesiredIndexPresenceParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 2, "synthetic-private-scalar-2\n\t\"", "invalid desired-index-present boolean")
}

// Malformed desired-index-valid boolean retains rejection and field identity without private source text.
func TestReliabilityIndexPublicDesiredIndexValidityParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 3, "synthetic-private-scalar-3\n\t\"", "invalid desired-index-valid boolean")
}

// Malformed table partition count retains rejection and field identity without private source text.
func TestReliabilityIndexPublicPartitionCountParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 5, "synthetic-private-scalar-5\n\t\"", "invalid table partition count")
}

// Malformed attached child index count retains rejection and field identity without private source text.
func TestReliabilityIndexPublicAttachedChildCountParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 6, "synthetic-private-scalar-6\n\t\"", "invalid attached child index count")
}

// Malformed invalid child index count retains rejection and field identity without private source text.
func TestReliabilityIndexPublicInvalidChildCountParserErrorPrivacy(t *testing.T) {
	requireReliabilityIndexPrivateScalar(t, 7, "synthetic-private-scalar-7\n\t\"", "invalid invalid child index count")
}

// Grammar and conversion failures still refuse every boolean/count column.
func TestReliabilityIndexPublicMalformedScalarGrammar(t *testing.T) {
	for _, field := range []struct {
		column     int
		diagnostic string
		values     []string
	}{
		{column: 0, diagnostic: "partitioned boolean", values: []string{"", "truthy", "2"}},
		{column: 1, diagnostic: "old-index-present boolean", values: []string{"", "truthy", "2"}},
		{column: 2, diagnostic: "desired-index-present boolean", values: []string{"", "truthy", "2"}},
		{column: 3, diagnostic: "desired-index-valid boolean", values: []string{"", "truthy", "2"}},
		{column: 5, diagnostic: "table partition count", values: []string{"", "-1", "1.5", strings.Repeat("9", 128)}},
		{column: 6, diagnostic: "attached child index count", values: []string{"", "-1", "1.5", strings.Repeat("9", 128)}},
		{column: 7, diagnostic: "invalid child index count", values: []string{"", "-1", "1.5", strings.Repeat("9", 128)}},
	} {
		for _, value := range field.values {
			requireReliabilityIndexPrivateScalar(t, field.column, value, "invalid "+field.diagnostic)
		}
	}
}

// Valid qualified physical definition evidence remains intentional drift output.
func TestReliabilityIndexPublicValidDefinitionEvidenceConserved(t *testing.T) {
	row := reliabilityIndexPrivateScalarRow()
	row[3] = "f"
	alerts, err := NewReliabilityIndexSignal().Run(context.Background(), reliabilityIndexSyntheticSettings(t, row))
	if err != nil {
		t.Fatal("valid physical definition became a parser failure")
	}
	alert := requireAlertClass(t, alerts, "reliability-index-drift")
	if !strings.Contains(alert.Evidence, "desired_definition="+row[4]) || !strings.Contains(alert.Markdown(), row[4]) {
		t.Fatal("owning parser privacy removed qualified definition evidence")
	}
}

// The generic visibility renderer was already safe; this is a conservation control.
func TestReliabilityIndexMalformedScalarVisibilityRemainsPrivate(t *testing.T) {
	const private = "synthetic-private-visibility-scalar"
	row := reliabilityIndexPrivateScalarRow()
	row[3] = private
	settings := reliabilityIndexSyntheticSettings(t, row)
	signal := NewReliabilityIndexSignal()
	_, err := signal.Run(context.Background(), settings)
	if err == nil {
		t.Fatal("visibility control did not reach scalar refusal")
	}
	alert := visibilityAlert(settings, signal, err)
	encoded, encodeErr := json.Marshal(alert)
	if encodeErr != nil {
		t.Fatal("visibility control could not encode fixed public output")
	}
	if alert.Class != "cannot-observe" || !alert.unobservable || strings.Contains(alert.Markdown(), private) || strings.Contains(string(encoded), private) {
		t.Fatal("fixed visibility no longer preserves unknown and scalar privacy")
	}
}

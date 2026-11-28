// audit_device_event's family declaration, its cutover's pure rules and db
// audit's acceptance of the cutover (db_audit_device_event_partition.go).
package server

import (
	"slices"
	"testing"
	"time"
)

// The family is registered as decided: daily by event_time, 3 days ahead, kept
// 7 days with no DEFAULT partition, its legacy partition retiring like a day,
// out of the REINDEX rotation, with its staged parent kept out of db audit.
func TestAuditDeviceEventFamilyDeclaration(t *testing.T) {
	family := auditDeviceEventPartitionFamily
	if !slices.Contains(PartitionFamilies(), family) {
		t.Fatal("the family is not registered")
	}
	if family.Name != "audit_device_event" || !slices.Equal(family.Tables, []string{"audit_device_event"}) || family.KeyColumn != "event_time" ||
		family.KeyKind != PartitionKeyTimestamp || family.Granularity != PartitionGranularityDaily || family.AheadRanges != 3 ||
		family.Retention != 7*24*time.Hour || family.DefaultPartition || family.KeepLegacyPartition || !family.ReindexExempt ||
		family.BeforeCutover == nil || !slices.Equal(family.CutoverTables, []string{"audit_device_event_staged"}) {
		t.Fatalf("the family is not as decided: %+v", family)
	}
	day := PartitionGranularityDaily.RangeOf(time.Date(2026, 11, 18, 13, 0, 0, 0, time.UTC))
	if leaf := family.LeafName("audit_device_event", day); leaf != "audit_device_event_p20261118" {
		t.Fatalf("the day's leaf is %s", leaf)
	}
	if !PartitionRegistryReindexExempt("audit_device_event", true) || !PartitionRegistryReindexExempt("audit_device_event_p20261118", false) {
		t.Fatal("the rotation rebuilds the family")
	}
	settings := DefaultAuditDeviceEventPartitionSettings()
	if settings.MinCutoverLead <= settings.CutoverAbandonMargin || settings.RollupDays < 90 || settings.ExclusiveLockTimeout >= PgLockTimeout*2 {
		t.Fatalf("the cutover's settings do not hold together: %+v", settings)
	}
}

// Prepare drops the unique event id constraint and nothing else unique.
func TestAuditDeviceEventUniqueEventIdConstraint(t *testing.T) {
	shape := &contractTableShape{
		table:   "audit_device_event",
		relkind: "r",
		columns: []contractColumn{{name: "event_time", typeName: "timestamp without time zone", notNull: true}},
		indexes: []contractIndex{
			{name: "audit_device_event_pkey", valid: true, ready: true, primary: true, unique: true, constraint: "audit_device_event_pkey", constraintDefinition: "PRIMARY KEY (event_time, device_id, event_id)"},
			{name: "audit_device_event_event_id_key", valid: true, ready: true, unique: true, constraint: "audit_device_event_event_id_key", constraintDefinition: "UNIQUE (event_id)"},
		},
	}
	if name := auditDeviceEventUniqueEventIdConstraint(shape); name != "audit_device_event_event_id_key" {
		t.Fatalf("found %q", name)
	}
	if problems := auditDeviceEventCutoverProblems(shape); len(problems) != 0 {
		t.Fatalf("the unique event id index blocked the cutover it drops: %v", problems)
	}
	shape.indexes[1].constraintDefinition = "UNIQUE (event_id, device_id)"
	if name := auditDeviceEventUniqueEventIdConstraint(shape); name != "" {
		t.Fatalf("a different unique constraint was taken for the event id's: %q", name)
	}
	if problems := auditDeviceEventCutoverProblems(shape); !slices.ContainsFunc(problems, func(problem string) bool {
		return problem == "audit_device_event unique index audit_device_event_event_id_key is not the primary key"
	}) {
		t.Fatalf("a different unique index did not block the cutover: %v", problems)
	}
}

// The unique event id constraint the migrations still define is not drift
// once the cutover dropped it, nor is a prepared cutover's legacy bound;
// another missing constraint still is.
func TestSchemaDiffAcceptsAuditDeviceEventCutover(t *testing.T) {
	pkey := auditConstraint{name: "audit_device_event_pkey", typ: "p", def: "PRIMARY KEY (event_time, device_id, event_id)"}
	unique := auditConstraint{name: "audit_device_event_event_id_key", typ: "u", def: "UNIQUE (event_id)"}
	snapshot := func(constraints ...auditConstraint) *SchemaSnapshot {
		return &SchemaSnapshot{
			tables:      map[string]bool{"audit_device_event": true},
			columns:     map[string][]auditColumn{},
			indexes:     map[string]auditIndex{},
			constraints: map[string][]auditConstraint{"audit_device_event": constraints},
			partitioned: map[string]bool{},
		}
	}
	expected := snapshot(pkey, unique)
	bound := auditConstraint{name: "audit_device_event_legacy_bound", typ: "c", def: "CHECK ((event_time < '2026-11-30 00:00:00'::timestamp without time zone))"}
	if diff := diffSchemas(expected, snapshot(pkey, bound)); diff.HasDifferences() {
		t.Fatalf("the cutover drifts:\n%s", diff.Report())
	}
	diff := diffSchemas(expected, snapshot(unique))
	if missing := diff.missingConstraints["audit_device_event"]; len(missing) != 1 || missing[0].name != pkey.name {
		t.Fatalf("a missing primary key was accepted: %+v", diff.missingConstraints)
	}
}

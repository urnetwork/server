// The catalog side of the table swap (db_table_swap.go): a table's shape as
// the catalog has it, the statements that build its twin from that shape, the
// copy with its checksums, and the comparison that proves the twin is the
// table under other names.
//
// The shape covers what a reader, a writer, the monitor's catalog contracts or
// db audit can observe of a table: its columns in order with their types,
// NOT NULL, generated expressions, defaults, storage, compression, collation,
// statistics targets, options and comments; its CHECK and NOT NULL constraints
// by name; its indexes with their definitions, constraints, validity and
// comments; its storage settings, the TOAST table's included; its owner and its
// effective grants; its persistence, replica identity, access method and
// tablespace; and its owned sequences. Whatever the swap cannot copy
// faithfully is a refusal instead.
package server

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

type tableSwapShape struct {
	name            string
	oid             uint32
	kind            string
	persistence     string
	replicaIdentity string
	accessMethod    string
	tablespace      uint32
	owner           string
	// the effective grants (the default when relacl is NULL): grantee,
	// privilege and grant option, sorted
	grants []string
	// sorted for comparison; the catalog's own order, kept beside, is the
	// order the twin's statements give them in, so the text matches too
	reloptions             []string
	toastReloptions        []string
	orderedReloptions      []string
	orderedToastReloptions []string
	comment                string
	byteCount              int64
	estimatedRows          int64
	columns                []tableSwapColumn
	// CHECK and NOT NULL constraints, sorted by kind and name
	constraints []tableSwapConstraint
	// by name
	indexes   []tableSwapIndex
	sequences []tableSwapSequence
	// the primary key's columns, in key order
	primaryKey []string
	// what the swap cannot copy faithfully
	refusals []string
}

type tableSwapColumn struct {
	name        string
	typeName    string
	notNull     bool
	generated   string
	identity    string
	defaultSql  string
	storage     string
	compression string
	collation   string
	statistics  int
	// sorted for comparison, and in the catalog's order
	options        []string
	orderedOptions []string
	comment        string
	granted        bool
}

type tableSwapConstraint struct {
	kind       string
	name       string
	definition string
	validated  bool
	noInherit  bool
	comment    string
}

type tableSwapIndex struct {
	name   string
	unique bool
	// pg_get_indexdef after USING: the method, keys, INCLUDE, WITH and WHERE,
	// which name neither the index nor its table
	body       string
	usable     bool
	tablespace uint32
	comment    string
	// the constraint the index backs, p or u, or empty
	constraintKind    string
	constraintComment string
}

type tableSwapSequence struct {
	qualifiedName string
	column        string
}

var tableSwapIndexDefinitionPattern = regexp.MustCompile(`^CREATE (UNIQUE )?INDEX (\S+) ON (ONLY )?(\S+) USING (.+)$`)

// The shape of public.<name>, read through conn (which may be a transaction
// holding the table's lock), or nil when no such relation exists.
func readTableSwapShape(ctx context.Context, conn PgQuerier, name string) *tableSwapShape {
	var oid *uint32
	Raise(conn.QueryRow(ctx, `SELECT to_regclass($1)::oid`, "public."+name).Scan(&oid))
	if oid == nil {
		return nil
	}
	shape := &tableSwapShape{name: name, oid: *oid}
	var aclNull bool
	var partition bool
	var typed bool
	var rowSecurity bool
	Raise(conn.QueryRow(
		ctx,
		`
		SELECT
			c.relkind::text,
			c.relpersistence::text,
			c.relreplident::text,
			c.relispartition,
			c.reloftype <> 0,
			c.relrowsecurity OR c.relforcerowsecurity,
			coalesce((SELECT amname FROM pg_am WHERE oid = c.relam), ''),
			c.reltablespace,
			pg_get_userbyid(c.relowner),
			c.relacl IS NULL,
			coalesce(c.reloptions, ARRAY[]::text[]),
			coalesce((SELECT toast.reloptions FROM pg_class toast WHERE toast.oid = c.reltoastrelid), ARRAY[]::text[]),
			coalesce(obj_description(c.oid, 'pg_class'), ''),
			pg_total_relation_size(c.oid),
			greatest(c.reltuples, 0)::bigint
		FROM pg_class c
		WHERE c.oid = $1
		`,
		shape.oid,
	).Scan(
		&shape.kind, &shape.persistence, &shape.replicaIdentity, &partition, &typed, &rowSecurity,
		&shape.accessMethod, &shape.tablespace, &shape.owner, &aclNull,
		&shape.reloptions, &shape.toastReloptions, &shape.comment, &shape.byteCount, &shape.estimatedRows,
	))
	shape.orderedReloptions = slices.Clone(shape.reloptions)
	shape.orderedToastReloptions = slices.Clone(shape.toastReloptions)
	slices.Sort(shape.reloptions)
	slices.Sort(shape.toastReloptions)

	result, err := conn.Query(
		ctx,
		`
		SELECT coalesce(grantee.rolname, 'PUBLIC'), acl.privilege_type, acl.is_grantable
		FROM pg_class c
		CROSS JOIN LATERAL aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) acl
		LEFT JOIN pg_roles grantee ON grantee.oid = acl.grantee
		WHERE c.oid = $1
		ORDER BY 1, 2, 3
		`,
		shape.oid,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var grantee string
			var privilege string
			var grantable bool
			Raise(result.Scan(&grantee, &privilege, &grantable))
			shape.grants = append(shape.grants, fmt.Sprintf("%s|%s|%t", grantee, privilege, grantable))
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT
			a.attname,
			format_type(a.atttypid, a.atttypmod),
			a.attnotnull,
			a.attgenerated::text,
			a.attidentity::text,
			coalesce(pg_get_expr(d.adbin, d.adrelid), ''),
			a.attstorage::text,
			a.attcompression::text,
			coalesce((SELECT collname FROM pg_collation WHERE oid = a.attcollation), ''),
			coalesce(a.attstattarget::integer, -1),
			coalesce(a.attoptions, ARRAY[]::text[]),
			coalesce(col_description(a.attrelid, a.attnum), ''),
			a.attacl IS NOT NULL
		FROM pg_attribute a
		LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND 0 < a.attnum AND NOT a.attisdropped
		ORDER BY a.attnum
		`,
		shape.oid,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var column tableSwapColumn
			Raise(result.Scan(
				&column.name, &column.typeName, &column.notNull, &column.generated, &column.identity, &column.defaultSql,
				&column.storage, &column.compression, &column.collation, &column.statistics, &column.options,
				&column.comment, &column.granted,
			))
			column.orderedOptions = slices.Clone(column.options)
			slices.Sort(column.options)
			shape.columns = append(shape.columns, column)
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT contype::text, conname, pg_get_constraintdef(oid), convalidated, connoinherit,
			coalesce(obj_description(oid, 'pg_constraint'), '')
		FROM pg_constraint
		WHERE conrelid = $1 AND contype IN ('c', 'n')
		ORDER BY contype, conname
		`,
		shape.oid,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var constraint tableSwapConstraint
			Raise(result.Scan(&constraint.kind, &constraint.name, &constraint.definition, &constraint.validated, &constraint.noInherit, &constraint.comment))
			shape.constraints = append(shape.constraints, constraint)
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT
			index_class.relname,
			pg_get_indexdef(i.indexrelid),
			i.indisunique,
			i.indisvalid AND i.indisready AND i.indislive,
			index_class.reltablespace,
			coalesce(obj_description(i.indexrelid, 'pg_class'), ''),
			coalesce(con.contype::text, ''),
			coalesce(obj_description(con.oid, 'pg_constraint'), ''),
			coalesce(con.condeferrable, false),
			i.indisprimary,
			CASE WHEN i.indisprimary THEN ARRAY(
				SELECT a.attname::text
				FROM unnest(i.indkey) WITH ORDINALITY k(attnum, position)
				JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
				ORDER BY k.position
			) ELSE ARRAY[]::text[] END
		FROM pg_index i
		JOIN pg_class index_class ON index_class.oid = i.indexrelid
		LEFT JOIN pg_constraint con
			ON con.conindid = i.indexrelid AND con.conrelid = i.indrelid AND con.contype IN ('p', 'u', 'x')
		WHERE i.indrelid = $1
		ORDER BY index_class.relname
		`,
		shape.oid,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var index tableSwapIndex
			var definition string
			var deferrable bool
			var primary bool
			var keyColumns []string
			Raise(result.Scan(
				&index.name, &definition, &index.unique, &index.usable, &index.tablespace, &index.comment,
				&index.constraintKind, &index.constraintComment, &deferrable, &primary, &keyColumns,
			))
			match := tableSwapIndexDefinitionPattern.FindStringSubmatch(definition)
			if match == nil {
				shape.refusals = append(shape.refusals, fmt.Sprintf("index %s has a definition the swap cannot rebuild: %s", index.name, definition))
				continue
			}
			index.body = match[5]
			if !index.usable {
				shape.refusals = append(shape.refusals, fmt.Sprintf("index %s is invalid or not ready", index.name))
			}
			if index.tablespace != 0 {
				shape.refusals = append(shape.refusals, fmt.Sprintf("index %s is in a tablespace", index.name))
			}
			if index.constraintKind == "x" {
				shape.refusals = append(shape.refusals, fmt.Sprintf("index %s backs an exclusion constraint", index.name))
			}
			if deferrable {
				shape.refusals = append(shape.refusals, fmt.Sprintf("the constraint of index %s is deferrable", index.name))
			}
			if primary {
				shape.primaryKey = keyColumns
			}
			shape.indexes = append(shape.indexes, index)
		}
	})

	result, err = conn.Query(
		ctx,
		`
		SELECT quote_ident(sequence_namespace.nspname) || '.' || quote_ident(sequence_class.relname), a.attname
		FROM pg_depend d
		JOIN pg_class sequence_class ON sequence_class.oid = d.objid AND sequence_class.relkind = 'S'
		JOIN pg_namespace sequence_namespace ON sequence_namespace.oid = sequence_class.relnamespace
		JOIN pg_attribute a ON a.attrelid = d.refobjid AND a.attnum = d.refobjsubid
		WHERE d.classid = 'pg_class'::regclass AND d.refclassid = 'pg_class'::regclass
			AND d.refobjid = $1 AND d.deptype = 'a'
		ORDER BY 2, 1
		`,
		shape.oid,
	)
	WithPgResult(result, err, func() {
		for result.Next() {
			var sequence tableSwapSequence
			Raise(result.Scan(&sequence.qualifiedName, &sequence.column))
			shape.sequences = append(shape.sequences, sequence)
		}
	})

	var triggers, rules, policies, publications, foreignKeys, otherConstraints, deferrables, inheritance, statistics, dependents, typeUsers int64
	Raise(conn.QueryRow(
		ctx,
		`
		SELECT
			(SELECT count(*) FROM pg_trigger WHERE tgrelid = $1 AND NOT tgisinternal),
			(SELECT count(*) FROM pg_rewrite WHERE ev_class = $1),
			(SELECT count(*) FROM pg_policy WHERE polrelid = $1),
			(SELECT count(*) FROM pg_publication_rel WHERE prrelid = $1),
			(SELECT count(*) FROM pg_constraint WHERE (conrelid = $1 OR confrelid = $1) AND contype = 'f'),
			(SELECT count(*) FROM pg_constraint WHERE conrelid = $1 AND contype NOT IN ('p', 'u', 'c', 'n', 'f')),
			(SELECT count(*) FROM pg_constraint WHERE conrelid = $1 AND contype IN ('c', 'n') AND condeferrable),
			(SELECT count(*) FROM pg_inherits WHERE inhrelid = $1 OR inhparent = $1),
			(SELECT count(*) FROM pg_statistic_ext WHERE stxrelid = $1),
			(SELECT count(*) FROM pg_depend
				WHERE refclassid = 'pg_class'::regclass AND refobjid = $1
					AND classid IN ('pg_rewrite'::regclass, 'pg_proc'::regclass)),
			(SELECT count(*) FROM pg_depend
				WHERE refclassid = 'pg_type'::regclass AND deptype = 'n'
					AND refobjid = (SELECT reltype FROM pg_class WHERE oid = $1))
		`,
		shape.oid,
	).Scan(&triggers, &rules, &policies, &publications, &foreignKeys, &otherConstraints, &deferrables, &inheritance, &statistics, &dependents, &typeUsers))

	refuse := func(count int64, what string) {
		if 0 < count {
			shape.refusals = append(shape.refusals, fmt.Sprintf("%s has %d %s", name, count, what))
		}
	}
	if shape.kind != "r" {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s is not a plain table (relkind %s)", name, shape.kind))
	}
	if partition {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s is a partition", name))
	}
	if shape.persistence != "p" {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s is not a permanent table (persistence %s)", name, shape.persistence))
	}
	if shape.replicaIdentity != "d" {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s has a replica identity other than its primary key (%s)", name, shape.replicaIdentity))
	}
	if typed {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s is a typed table", name))
	}
	if rowSecurity {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s has row security on", name))
	}
	if shape.tablespace != 0 {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s is in a tablespace", name))
	}
	refuse(triggers, "trigger(s)")
	refuse(rules, "rule(s)")
	refuse(policies, "row security polic(ies)")
	refuse(publications, "publication(s)")
	refuse(foreignKeys, "foreign key(s), from it or into it")
	refuse(otherConstraints, "exclusion or trigger constraint(s)")
	refuse(deferrables, "deferrable constraint(s)")
	refuse(inheritance, "inheritance parent(s) or child(ren)")
	refuse(statistics, "extended statistics object(s)")
	refuse(dependents, "dependent view(s) or function(s), which would keep reading the old table")
	refuse(typeUsers, "column(s) or function(s) that use its row type")
	for _, column := range shape.columns {
		if column.identity != "" {
			shape.refusals = append(shape.refusals, fmt.Sprintf("%s.%s is an identity column", name, column.name))
		}
		if column.granted {
			shape.refusals = append(shape.refusals, fmt.Sprintf("%s.%s has column grants", name, column.name))
		}
	}
	if len(shape.primaryKey) == 0 {
		shape.refusals = append(shape.refusals, fmt.Sprintf("%s has no primary key to read the copy through", name))
	}
	return shape
}

// The names a swap of table gives, each within PostgreSQL's identifier
// length: the twin and the indexes it builds, and the old table and its
// indexes.
func tableSwapNameRefusals(table string, shape *tableSwapShape) (refusals []string) {
	names := []string{table + tableSwapNextSuffix, tableSwapOldName(table, tableSwapStampLayout)}
	for _, index := range shape.indexes {
		names = append(names, tableSwapNextName(index.name), tableSwapOldName(index.name, tableSwapStampLayout))
	}
	for _, name := range names {
		if pgMaxIdentifierLength < len(name) {
			refusals = append(refusals, fmt.Sprintf("the swap would name a relation %s, longer than %d bytes", name, pgMaxIdentifierLength))
		}
	}
	return
}

func tableSwapLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func tableSwapQuotedColumns(columns []tableSwapColumn, stored bool) string {
	names := []string{}
	for _, column := range columns {
		if stored && column.generated != "" {
			continue
		}
		names = append(names, quoteIdentifier(column.name))
	}
	return strings.Join(names, ", ")
}

// The statements that create twin, without indexes, as the table: columns with
// their defaults, generated expressions, storage, compression, collations,
// NOT NULL and CHECK constraints and comments (LIKE), then what LIKE does not
// copy: the storage settings, column statistics targets and options, the
// owner, the grants and the table's comment.
func (self *tableSwapShape) twinSql(twin string) []string {
	quotedTwin := "public." + quoteIdentifier(twin)
	statements := []string{
		fmt.Sprintf(
			"CREATE TABLE %s (LIKE public.%s INCLUDING DEFAULTS INCLUDING GENERATED INCLUDING CONSTRAINTS INCLUDING STORAGE INCLUDING COMPRESSION INCLUDING COMMENTS)",
			quotedTwin, quoteIdentifier(self.name),
		),
	}
	if 0 < len(self.orderedReloptions) {
		statements = append(statements, fmt.Sprintf("ALTER TABLE %s SET (%s)", quotedTwin, strings.Join(self.orderedReloptions, ", ")))
	}
	if 0 < len(self.orderedToastReloptions) {
		options := []string{}
		for _, option := range self.orderedToastReloptions {
			options = append(options, "toast."+option)
		}
		statements = append(statements, fmt.Sprintf("ALTER TABLE %s SET (%s)", quotedTwin, strings.Join(options, ", ")))
	}
	for _, column := range self.columns {
		if 0 <= column.statistics {
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET STATISTICS %d", quotedTwin, quoteIdentifier(column.name), column.statistics))
		}
		if 0 < len(column.orderedOptions) {
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET (%s)", quotedTwin, quoteIdentifier(column.name), strings.Join(column.orderedOptions, ", ")))
		}
	}
	statements = append(statements, fmt.Sprintf("ALTER TABLE %s OWNER TO %s", quotedTwin, quoteIdentifier(self.owner)))
	for _, grant := range self.grants {
		parts := strings.Split(grant, "|")
		grantee := parts[0]
		if grantee == self.owner {
			continue
		}
		if grantee != "PUBLIC" {
			grantee = quoteIdentifier(grantee)
		}
		statement := fmt.Sprintf("GRANT %s ON %s TO %s", parts[1], quotedTwin, grantee)
		if parts[2] == "true" {
			statement += " WITH GRANT OPTION"
		}
		statements = append(statements, statement)
	}
	if self.comment != "" {
		statements = append(statements, fmt.Sprintf("COMMENT ON TABLE %s IS %s", quotedTwin, tableSwapLiteral(self.comment)))
	}
	return statements
}

// The copy of every row into target, read through the primary key, as one
// statement: a DO block that copies, compares the count and two
// order-independent 64-bit checksums of the rows read with those of the rows
// written, refuses more than maxRows, and keeps the count for the report
// (tableSwapRowsSetting). A checksum covers every column, generated ones
// included, in the table's order; the rows written are the target's own, as
// RETURNING projects them from the stored tuples.
func (self *tableSwapShape) copySql(target string, maxRows int64) string {
	all := tableSwapQuotedColumns(self.columns, false)
	stored := tableSwapQuotedColumns(self.columns, true)
	order := []string{}
	for _, column := range self.primaryKey {
		order = append(order, quoteIdentifier(column))
	}
	return fmt.Sprintf(
		`DO $table_swap$
DECLARE
	source_count bigint;
	source_xor bigint;
	source_sum numeric;
	copied_count bigint;
	copied_xor bigint;
	copied_sum numeric;
BEGIN
	WITH source_rows AS MATERIALIZED (
		SELECT %s FROM public.%s ORDER BY %s
	), copied AS (
		INSERT INTO public.%s (%s) SELECT %s FROM source_rows
		RETURNING %s
	)
	SELECT
		(SELECT count(*) FROM source_rows),
		(SELECT coalesce(bit_xor(hashtextextended(source_row::text, 0)), 0) FROM source_rows source_row),
		(SELECT coalesce(sum(hashtextextended(source_row::text, 1)::numeric), 0) FROM source_rows source_row),
		(SELECT count(*) FROM copied),
		(SELECT coalesce(bit_xor(hashtextextended(copied_row::text, 0)), 0) FROM copied copied_row),
		(SELECT coalesce(sum(hashtextextended(copied_row::text, 1)::numeric), 0) FROM copied copied_row)
	INTO source_count, source_xor, source_sum, copied_count, copied_xor, copied_sum;
	IF source_count IS DISTINCT FROM copied_count OR source_xor IS DISTINCT FROM copied_xor OR source_sum IS DISTINCT FROM copied_sum THEN
		RAISE EXCEPTION 'the copy into %% does not match %%: %% rows (checksums %%, %%) against %% rows (checksums %%, %%)',
			%s, %s, copied_count, copied_xor, copied_sum, source_count, source_xor, source_sum;
	END IF;
	IF %d < source_count THEN
		RAISE EXCEPTION '%% holds %% rows, more than the swap copies under its lock (%%)', %s, source_count, %d;
	END IF;
	IF (SELECT count(*) FROM public.%s) <> source_count THEN
		RAISE EXCEPTION '%% does not hold the %% rows it was given', %s, source_count;
	END IF;
	PERFORM set_config('%s', source_count::text, true);
END
$table_swap$`,
		all, quoteIdentifier(self.name), strings.Join(order, ", "),
		quoteIdentifier(target), stored, stored,
		all,
		tableSwapLiteral(target), tableSwapLiteral(self.name),
		maxRows, tableSwapLiteral(self.name), maxRows,
		quoteIdentifier(target), tableSwapLiteral(target),
		tableSwapRowsSetting,
	)
}

// The canonical description of public.<relation> as one md5, read by the
// database: its storage, owner, grants and comment, its columns, its CHECK and
// NOT NULL constraints, its indexes and their constraints, and the counts of
// what the swap refuses (triggers, rules, policies, publications, foreign
// keys, inheritance, extended statistics, dependent views and functions, and
// users of its row type). Neither the relation's name nor any oid enters it,
// and an index's name enters with indexSuffix removed, so a twin whose
// indexes carry the suffix describes the same as its table. The swap compares
// the table's under its lock with the one the rehearsal read, and the twin's
// with the table's.
func tableSwapFingerprintSql(relation string, indexSuffix string) string {
	regclass := tableSwapLiteral("public."+quoteIdentifier(relation)) + "::regclass"
	indexName := "ic.relname::text"
	if indexSuffix != "" {
		indexName = fmt.Sprintf("regexp_replace(ic.relname::text, %s, '')", tableSwapLiteral(regexp.QuoteMeta(indexSuffix)+"$"))
	}
	return fmt.Sprintf(`(SELECT md5(concat_ws(E'\n',
	(SELECT concat_ws('|', c.relkind, c.relpersistence, c.relreplident, c.relispartition, c.reloftype <> 0,
		c.relrowsecurity OR c.relforcerowsecurity, coalesce(am.amname, ''), c.reltablespace, pg_get_userbyid(c.relowner),
		coalesce((SELECT string_agg(o, ',' ORDER BY o) FROM unnest(coalesce(c.reloptions, ARRAY[]::text[])) o), ''),
		coalesce((SELECT string_agg(o, ',' ORDER BY o) FROM pg_class toast, unnest(coalesce(toast.reloptions, ARRAY[]::text[])) o WHERE toast.oid = c.reltoastrelid), ''),
		coalesce((SELECT string_agg(entry, ',' ORDER BY entry) FROM (
			SELECT coalesce(grantee.rolname, 'PUBLIC') || ':' || acl.privilege_type || ':' || acl.is_grantable AS entry
			FROM aclexplode(coalesce(c.relacl, acldefault('r', c.relowner))) acl
			LEFT JOIN pg_roles grantee ON grantee.oid = acl.grantee
		) grants), ''),
		coalesce(obj_description(c.oid, 'pg_class'), ''))
	 FROM pg_class c LEFT JOIN pg_am am ON am.oid = c.relam WHERE c.oid = %[1]s),
	(SELECT string_agg(concat_ws('|', a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, a.attgenerated, a.attidentity,
		coalesce(pg_get_expr(d.adbin, d.adrelid), ''), a.attstorage, a.attcompression,
		coalesce(column_collation.collname, ''), coalesce(a.attstattarget::integer, -1),
		coalesce((SELECT string_agg(o, ',' ORDER BY o) FROM unnest(coalesce(a.attoptions, ARRAY[]::text[])) o), ''),
		coalesce(col_description(a.attrelid, a.attnum), ''), a.attacl IS NULL), E'\n' ORDER BY a.attnum)
	 FROM pg_attribute a
	 LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
	 LEFT JOIN pg_collation column_collation ON column_collation.oid = a.attcollation
	 WHERE a.attrelid = %[1]s AND 0 < a.attnum AND NOT a.attisdropped),
	(SELECT string_agg(concat_ws('|', con.contype, con.conname, pg_get_constraintdef(con.oid), con.convalidated, con.connoinherit,
		con.condeferrable, coalesce(obj_description(con.oid, 'pg_constraint'), '')), E'\n' ORDER BY con.contype, con.conname)
	 FROM pg_constraint con WHERE con.conrelid = %[1]s AND con.contype IN ('c', 'n')),
	(SELECT string_agg(concat_ws('|', %[2]s, i.indisunique, i.indisprimary, i.indisvalid AND i.indisready AND i.indislive,
		ic.reltablespace, regexp_replace(pg_get_indexdef(i.indexrelid), '^CREATE (UNIQUE )?INDEX \S+ ON (ONLY )?\S+ USING ', ''),
		coalesce(con.contype::text, ''), coalesce(con.condeferrable, false),
		coalesce(obj_description(i.indexrelid, 'pg_class'), ''), coalesce(obj_description(con.oid, 'pg_constraint'), '')),
		E'\n' ORDER BY %[2]s)
	 FROM pg_index i
	 JOIN pg_class ic ON ic.oid = i.indexrelid
	 LEFT JOIN pg_constraint con ON con.conindid = i.indexrelid AND con.conrelid = i.indrelid AND con.contype IN ('p', 'u', 'x')
	 WHERE i.indrelid = %[1]s),
	(SELECT concat_ws('|',
		(SELECT count(*) FROM pg_trigger WHERE tgrelid = %[1]s AND NOT tgisinternal),
		(SELECT count(*) FROM pg_rewrite WHERE ev_class = %[1]s),
		(SELECT count(*) FROM pg_policy WHERE polrelid = %[1]s),
		(SELECT count(*) FROM pg_publication_rel WHERE prrelid = %[1]s),
		(SELECT count(*) FROM pg_constraint WHERE (conrelid = %[1]s OR confrelid = %[1]s) AND contype NOT IN ('p', 'u', 'c', 'n')),
		(SELECT count(*) FROM pg_inherits WHERE inhrelid = %[1]s OR inhparent = %[1]s),
		(SELECT count(*) FROM pg_statistic_ext WHERE stxrelid = %[1]s),
		(SELECT count(*) FROM pg_depend
			WHERE refclassid = 'pg_class'::regclass AND refobjid = %[1]s
				AND classid IN ('pg_rewrite'::regclass, 'pg_proc'::regclass)),
		(SELECT count(*) FROM pg_depend
			WHERE refclassid = 'pg_type'::regclass AND deptype = 'n'
				AND refobjid = (SELECT reltype FROM pg_class WHERE oid = %[1]s)))))))`,
		regclass, indexName)
}

// The statements that build the table's indexes on target under name(index):
// each index, its primary key or unique constraint on it, and their comments.
func (self *tableSwapShape) indexSql(target string, name func(string) string) []string {
	quotedTarget := "public." + quoteIdentifier(target)
	statements := []string{}
	for _, index := range self.indexes {
		indexName := quoteIdentifier(name(index.name))
		unique := ""
		if index.unique {
			unique = "UNIQUE "
		}
		statements = append(statements, fmt.Sprintf("CREATE %sINDEX %s ON %s USING %s", unique, indexName, quotedTarget, index.body))
		switch index.constraintKind {
		case "p":
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s PRIMARY KEY USING INDEX %s", quotedTarget, indexName, indexName))
		case "u":
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s UNIQUE USING INDEX %s", quotedTarget, indexName, indexName))
		}
		if index.comment != "" {
			statements = append(statements, fmt.Sprintf("COMMENT ON INDEX public.%s IS %s", indexName, tableSwapLiteral(index.comment)))
		}
		if index.constraintKind != "" && index.constraintComment != "" {
			statements = append(statements, fmt.Sprintf("COMMENT ON CONSTRAINT %s ON %s IS %s", indexName, quotedTarget, tableSwapLiteral(index.constraintComment)))
		}
	}
	return statements
}

// The statements that drop every index of the table, a constraint's through
// its constraint.
func (self *tableSwapShape) dropIndexSql() []string {
	quotedTable := "public." + quoteIdentifier(self.name)
	statements := []string{}
	for _, index := range self.indexes {
		if index.constraintKind != "" {
			statements = append(statements, fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT %s", quotedTable, quoteIdentifier(index.name)))
		} else {
			statements = append(statements, fmt.Sprintf("DROP INDEX public.%s", quoteIdentifier(index.name)))
		}
	}
	return statements
}

// How target differs from the table, with the table's index named
// indexName(index) on target. None means target is the table under other
// names.
func (self *tableSwapShape) differences(target *tableSwapShape, indexName func(string) string) (differences []string) {
	if target == nil {
		return []string{"the target does not exist"}
	}
	differ := func(what string, expected any, actual any) {
		if fmt.Sprint(expected) != fmt.Sprint(actual) {
			differences = append(differences, fmt.Sprintf("%s: %v, not %v", what, actual, expected))
		}
	}
	differ("kind", self.kind, target.kind)
	differ("persistence", self.persistence, target.persistence)
	differ("replica identity", self.replicaIdentity, target.replicaIdentity)
	differ("access method", self.accessMethod, target.accessMethod)
	differ("tablespace", self.tablespace, target.tablespace)
	differ("owner", self.owner, target.owner)
	differ("grants", self.grants, target.grants)
	differ("storage settings", self.reloptions, target.reloptions)
	differ("toast storage settings", self.toastReloptions, target.toastReloptions)
	differ("comment", self.comment, target.comment)
	if len(self.columns) != len(target.columns) {
		differences = append(differences, fmt.Sprintf("%d columns, not %d", len(target.columns), len(self.columns)))
	} else {
		for i, column := range self.columns {
			expected, actual := column, target.columns[i]
			expected.orderedOptions, actual.orderedOptions = nil, nil
			differ(fmt.Sprintf("column %d", i+1), fmt.Sprintf("%+v", expected), fmt.Sprintf("%+v", actual))
		}
	}
	if len(self.constraints) != len(target.constraints) {
		differences = append(differences, fmt.Sprintf("constraints %+v, not %+v", target.constraints, self.constraints))
	} else {
		for i, constraint := range self.constraints {
			differ(fmt.Sprintf("constraint %s", constraint.name), fmt.Sprintf("%+v", constraint), fmt.Sprintf("%+v", target.constraints[i]))
		}
	}
	targetIndexes := map[string]tableSwapIndex{}
	for _, index := range target.indexes {
		targetIndexes[index.name] = index
	}
	for _, index := range self.indexes {
		name := indexName(index.name)
		targetIndex, ok := targetIndexes[name]
		if !ok {
			differences = append(differences, fmt.Sprintf("index %s is missing", name))
			continue
		}
		delete(targetIndexes, name)
		expected := index
		expected.name = name
		differ(fmt.Sprintf("index %s", name), fmt.Sprintf("%+v", expected), fmt.Sprintf("%+v", targetIndex))
	}
	for name := range targetIndexes {
		differences = append(differences, fmt.Sprintf("index %s is extra", name))
	}
	differ("primary key", self.primaryKey, target.primaryKey)
	return
}

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// What check-tables found: findings fail it, notes do not.
type checkTablesReport struct {
	findings          []string
	notes             []string
	createdTableCount int
}

// One table line of sidecar-tables.conf.
type tablesConfLine struct {
	table   string
	dump    string
	orphans string
	group   string
	row     string
	ttl     string
	line    int
}

// A CREATE TABLE in a SQL string, the name in group 1.
var createTablePattern = regexp.MustCompile(`(?i)\bCREATE\s+(?:UNLOGGED\s+)?TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?"?([a-z_][a-z0-9_]*)"?`)

// A bound of a ttl, as §2.8 writes them: "7 d", "24 h", "13 months", "a 7-day
// window". A duration followed by "after" counts from an event with no bound of
// its own (a token's exp, a decision's not_after, a case's close), so it bounds
// nothing here.
var ttlBoundPattern = regexp.MustCompile(`(?i)\b([0-9]+)[ -](min|minutes?|h|hours?|d|days?|weeks?|months?|years?)\b(\s+after\b)?`)

// The longest a ttl's fixed bounds allow, in hours, and whether it has one.
func ttlBoundHours(ttl string) (float64, bool) {
	longest := 0.0
	found := false
	for _, match := range ttlBoundPattern.FindAllStringSubmatch(ttl, -1) {
		if match[3] != "" {
			continue
		}
		count, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		unit := strings.ToLower(match[2])
		hours := 0.0
		switch {
		case strings.HasPrefix(unit, "min"):
			hours = count / 60
		case unit == "h" || strings.HasPrefix(unit, "hour"):
			hours = count
		case unit == "d" || strings.HasPrefix(unit, "day"):
			hours = count * 24
		case strings.HasPrefix(unit, "week"):
			hours = count * 24 * 7
		case strings.HasPrefix(unit, "month"):
			hours = count * 24 * 30
		case strings.HasPrefix(unit, "year"):
			hours = count * 24 * 365
		}
		found = true
		longest = max(longest, hours)
	}
	return longest, found
}

// §19.6 and D-OPS-18: no dump holds a table whose ttl is at most 7 d.
const shortTtlHours = 7 * 24

// Reads sidecar-tables.conf's table lines. family lines name prefixes the main
// dump excludes, and no table's dump decision, so they are not read here.
func readTablesConf(path string) ([]*tablesConfLine, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := []*tablesConfLine{}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	lineNumber := 0
	for scanner.Scan() {
		lineNumber += 1
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if fields[0] == "family" {
			continue
		}
		if len(fields) < 6 {
			return nil, fmt.Errorf("%s:%d: a table line needs table, dump, orphans, group, row and ttl", path, lineNumber)
		}
		lines = append(lines, &tablesConfLine{
			table:   fields[0],
			dump:    fields[1],
			orphans: fields[2],
			group:   fields[3],
			row:     fields[4],
			ttl:     strings.Join(fields[5:], " "),
			line:    lineNumber,
		})
	}
	return lines, scanner.Err()
}

// One row of §2.8's list, as far as this check reads it.
type metadataRow struct {
	Store     string `yaml:"store"`
	Ttl       string `yaml:"ttl"`
	Withdrawn string `yaml:"withdrawn"`
}

func readMetadataList(path string) (map[string]*metadataRow, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	rows := map[string]*metadataRow{}
	if err := yaml.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return rows, nil
}

// The tables each group creates, by group name: the pending groups by the
// function each CREATE TABLE sits in, the registered ones by the span from
// promote's comment to the group's marker in db_migrations.go. A CREATE TABLE
// reached through a constant declared elsewhere is not seen; the groups write
// their SQL inline.
func readCreatedTables(serverDirectory string) (map[string]string, error) {
	created := map[string]string{}
	add := func(table string, groupName string) error {
		if other, ok := created[table]; ok && other != groupName {
			return fmt.Errorf("%s is created by both %s and %s", table, other, groupName)
		}
		created[table] = groupName
		return nil
	}

	pendingPath := filepath.Join(serverDirectory, pendingFileName)
	pendingData, err := os.ReadFile(pendingPath)
	if err != nil {
		return nil, err
	}
	pending, err := parseGoSource(pendingPath, pendingData)
	if err != nil {
		return nil, err
	}
	for name, function := range pending.functions() {
		match := partFunctionPattern.FindStringSubmatch(name)
		if match == nil {
			match = groupFunctionPattern.FindStringSubmatch(name)
		}
		if match == nil {
			continue
		}
		var walkErr error
		ast.Inspect(function, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING || walkErr != nil {
				return walkErr == nil
			}
			value, err := strconv.Unquote(literal.Value)
			if err != nil {
				walkErr = err
				return false
			}
			for _, tableMatch := range createTablePattern.FindAllStringSubmatch(value, -1) {
				if err := add(strings.ToLower(tableMatch[1]), "MG"+match[1]); err != nil {
					walkErr = err
					return false
				}
			}
			return true
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}

	migrationsData, err := os.ReadFile(filepath.Join(serverDirectory, migrationsFileName))
	if err != nil {
		return nil, err
	}
	text := string(migrationsData)
	registered := regexp.MustCompile(`// MG([0-9]) \(§19\.7\), registered by its Gn-out`)
	for _, match := range registered.FindAllStringSubmatchIndex(text, -1) {
		number := text[match[2]:match[3]]
		marker := "newCodeMigration(sidecarMg" + number + "MigrationId"
		end := strings.Index(text[match[1]:], marker)
		if end < 0 {
			return nil, fmt.Errorf("db_migrations.go registers MG%s without its marker", number)
		}
		for _, tableMatch := range createTablePattern.FindAllStringSubmatch(text[match[1]:match[1]+end], -1) {
			if err := add(strings.ToLower(tableMatch[1]), "MG"+number); err != nil {
				return nil, err
			}
		}
	}
	return created, nil
}

// Whether a §2.8 store names the table, by name or by a family pattern such as
// network_dns_*.
func storeNamesTable(store string, table string) bool {
	for _, word := range regexp.MustCompile(`[A-Za-z0-9_*]+`).FindAllString(store, -1) {
		if word == table {
			return true
		}
		if prefix, ok := strings.CutSuffix(word, "*"); ok && prefix != "" && strings.HasPrefix(table, prefix) {
			return true
		}
	}
	return false
}

// Compares the created tables with the conf, and each line with §2.8.
func checkTables(serverDirectory string, tablesConfPath string, metadataListPath string) (*checkTablesReport, error) {
	created, err := readCreatedTables(serverDirectory)
	if err != nil {
		return nil, err
	}
	lines, err := readTablesConf(tablesConfPath)
	if err != nil {
		return nil, err
	}
	rows, err := readMetadataList(metadataListPath)
	if err != nil {
		return nil, err
	}

	report := &checkTablesReport{createdTableCount: len(created)}
	finding := func(format string, arguments ...any) {
		report.findings = append(report.findings, fmt.Sprintf(format, arguments...))
	}
	lineOf := map[string]*tablesConfLine{}
	for _, line := range lines {
		if other, ok := lineOf[line.table]; ok {
			finding("%s has two lines in sidecar-tables.conf, %d and %d", line.table, other.line, line.line)
			continue
		}
		lineOf[line.table] = line
	}

	tables := []string{}
	for table := range created {
		tables = append(tables, table)
	}
	slices.Sort(tables)
	for _, table := range tables {
		groupName := created[table]
		line, ok := lineOf[table]
		if !ok {
			finding("%s, which %s creates, has no line in sidecar-tables.conf, so the sidecar dump leaves it out", table, groupName)
			continue
		}
		if line.group != groupName {
			finding("%s is created by %s and its line names %s", table, groupName, line.group)
		}
	}

	unbuilt := 0
	for _, line := range lines {
		if _, ok := created[line.table]; !ok {
			unbuilt += 1
		}
		switch line.dump {
		case "dump", "none":
		default:
			finding("%s's line says %q, neither dump nor none", line.table, line.dump)
			continue
		}
		row, ok := rows[line.row]
		if !ok {
			finding("%s's line names %s, which §2.8's list does not hold", line.table, line.row)
		} else if row.Withdrawn != "" {
			finding("%s's line names %s, which §2.8's list withdrew", line.table, line.row)
		} else if !storeNamesTable(row.Store, line.table) {
			finding("%s's line names %s, whose store does not name the table", line.table, line.row)
		}
		bound, bounded := ttlBoundHours(line.ttl)
		if line.dump == "dump" && bounded && bound <= shortTtlHours {
			finding("%s is dumped, and its ttl, %q, is at most 7 d: no dump may hold it (§19.6, D-OPS-18)", line.table, line.ttl)
		}
		if line.dump == "none" && bounded && shortTtlHours < bound {
			finding("%s is left out of every dump, and its ttl, %q, is past 7 d: the sidecar dump must hold it (D-OPS-18)", line.table, line.ttl)
		}
	}
	if 0 < unbuilt {
		report.notes = append(report.notes, fmt.Sprintf("%d lines name tables no group creates yet, which their owners' tasks add", unbuilt))
	}
	return report, nil
}

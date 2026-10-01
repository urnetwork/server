// SIGNALS.md §2.1a: optional finite query/load observation, never remediation.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const pgQuerySampleBudget = 40 * time.Second
const pgQuerySampleMaxBytes = 4 * 1024 * 1024
const pgQuerySampleCount = 12

func NewPgQuerySampleSignal() Signal {
	return &signalAdapter{number: "2.1a", key: "pg-query-sample", name: "Bounded PostgreSQL query/load sample", probe: pgQuerySampleProbe{}}
}

type pgQuerySampleProbe struct {
	syncAttemptFile func(*os.File) error
}

func (pgQuerySampleProbe) id() string               { return "pg/query-sample" }
func (pgQuerySampleProbe) tier() string             { return tierWarn }
func (pgQuerySampleProbe) cadence() time.Duration   { return 15 * time.Minute }
func (pgQuerySampleProbe) runBudget() time.Duration { return pgQuerySampleBudget }

// The input is SQL owned by this program, not caller text. Families are only
// recognition aids; unknown statements keep a private run-local query token.
func pgSampleFamily(q string) string {
	return `CASE
 WHEN ` + q + ` ~ '^commit( |;|$)' THEN 'commit'
 WHEN ` + q + ` ~ '^rollback( |;|$)' THEN 'rollback'
 WHEN ` + q + ` ~ '^(begin|start transaction)( |;|$)' THEN 'begin'
 WHEN ` + q + ` ~ '^reindex (table|index) concurrently ' THEN 'reindex_concurrent'
 WHEN ` + q + ` ~ '^(autovacuum:|vacuum )' THEN 'vacuum'
 WHEN ` + q + ` LIKE 'delete from audit_contract_event%event_details%event_time%' THEN 'audit_daily_delete'
 WHEN ` + q + ` LIKE '%sum(contract_close.used_transfer_byte_count)%from transfer_contract%' THEN 'audit_daily_sum'
 WHEN ` + q + ` LIKE '%min(transfer_contract.create_time)%max(transfer_contract.close_time)%from transfer_escrow_sweep%' THEN 'legacy_payout_range'
 WHEN ` + q + ` LIKE 'update transfer_escrow_sweep%temp_account_payment%' THEN 'payout_finalize'
 WHEN ` + q + ` LIKE 'insert into network_connection_reliability_score%' THEN 'reliability_insert'
 WHEN ` + q + ` LIKE '%client_reliability_running%' THEN 'reliability_running'
 WHEN ` + q + ` LIKE '%pending_task%' THEN 'pending_task_access'
 WHEN ` + q + ` LIKE '%client_tls_certificate%' THEN 'tls_metadata_access'
 WHEN ` + q + ` LIKE '%st_client_key_head%' THEN 'signed_key_access'
 WHEN ` + q + ` LIKE '%transfer_escrow%' THEN 'escrow_access'
 WHEN ` + q + ` LIKE '%contract_close%' THEN 'contract_close_access'
 WHEN ` + q + ` LIKE '%transfer_contract%' THEN 'transfer_contract_access'
 ELSE 'other' END`
}
func pgSampleWait(alias string) string {
	return `CASE
 WHEN ` + alias + `.wait_event IS NULL THEN 'none'
 WHEN ` + alias + `.wait_event IN ('WALWrite','WALInsert','WALSync','BufferMapping','DataFileRead','DataFileWrite','DataFileExtend','ClientRead','ClientWrite','transactionid','virtualxid','relation','MessageQueueReceive') THEN coalesce(` + alias + `.wait_event_type,'unknown')||':'||` + alias + `.wait_event
 WHEN ` + alias + `.wait_event_type IN ('Activity','BufferPin','Client','Extension','IO','IPC','Lock','LWLock','Timeout') THEN ` + alias + `.wait_event_type||':other'
 ELSE 'other' END`
}
func pgSampleOwner(alias string) string {
	return `CASE WHEN ` + alias + `.client_addr IS NULL THEN 'local' WHEN ` + alias + `.client_addr <<= '127.0.0.0/8'::inet OR ` + alias + `.client_addr = '::1'::inet THEN 'loopback' ELSE 'remote' END`
}
func pgSampleApp(alias string) string {
	return `CASE WHEN coalesce(` + alias + `.application_name,'')='' THEN 'unset' WHEN ` + alias + `.application_name IN ('pg_dump','pg_restore','psql') THEN ` + alias + `.application_name ELSE 'other' END`
}
func pgSampleBackend(alias string) string {
	return `CASE WHEN ` + alias + `.backend_type IN ('client backend','parallel worker','autovacuum worker','autovacuum launcher','checkpointer','background writer','walwriter','walsender','walreceiver','logical replication worker','logical replication launcher') THEN ` + alias + `.backend_type ELSE 'other' END`
}
func pgSampleState(alias string) string {
	return `CASE WHEN ` + alias + `.state IN ('active','idle','idle in transaction','idle in transaction (aborted)','fastpath function call','disabled') THEN ` + alias + `.state ELSE 'unknown' END`
}
func pgSampleActivitySQL(index int) string {
	return fmt.Sprintf(`WITH a AS MATERIALIZED (
 SELECT *, lower(btrim(regexp_replace(left(coalesce(query,''),2048),'\s+',' ','g'))) AS normalized
 FROM pg_stat_activity WHERE pid<>pg_backend_pid()
 ), grouped AS (
 SELECT coalesce(query_id::text,'none') q, %s state, %s wait,
 %s owner, %s app, %s backend, %s family,
 CASE WHEN datid=(SELECT oid FROM pg_database WHERE datname=current_database()) THEN 'current' ELSE 'other' END db_scope,
 count(*)::int n,
 coalesce(greatest(0,extract(epoch FROM max(clock_timestamp()-query_start))),0)::float8 query_age,
 coalesce(greatest(0,extract(epoch FROM max(clock_timestamp()-xact_start))),0)::float8 xact_age,
 coalesce(greatest(0,extract(epoch FROM max(clock_timestamp()-state_change))),0)::float8 state_age
 FROM a GROUP BY 1,2,3,4,5,6,7,8
 ), selected AS (SELECT * FROM grouped ORDER BY (state='active') DESC,n DESC,q,wait,owner,backend LIMIT 128)
 SELECT json_build_object('kind','activity','sample',%d,'at',extract(epoch FROM clock_timestamp()),
 'total',(SELECT count(*) FROM a),'groups',(SELECT count(*) FROM grouped),
 'rows',coalesce((SELECT json_agg(json_build_array(q,state,wait,owner,app,backend,family,db_scope,n,query_age,xact_age,state_age)) FROM selected),'[]'::json));
 `, pgSampleState("a"), pgSampleWait("a"), pgSampleOwner("a"), pgSampleApp("a"), pgSampleBackend("a"), pgSampleFamily("normalized"), index)
}
func pgSampleHistorySQL(index int) string {
	return fmt.Sprintf(`WITH history AS MATERIALIZED (
 SELECT queryid::text q,sum(calls)::float8 calls,sum(total_exec_time)::float8 exec_ms,max(max_exec_time)::float8 max_ms,
 min(%s) family
 FROM (SELECT *,lower(btrim(regexp_replace(left(query,2048),'\s+',' ','g'))) normalized FROM pg_stat_statements
 WHERE dbid=(SELECT oid FROM pg_database WHERE datname=current_database())) s
 GROUP BY queryid
 ), selected AS (SELECT * FROM history ORDER BY exec_ms DESC,q LIMIT 5000)
 SELECT json_build_object('kind','history','sample',%d,'at',extract(epoch FROM clock_timestamp()),
 'reset',extract(epoch FROM (SELECT stats_reset FROM pg_stat_statements_info)),
 'total',(SELECT count(*) FROM history), 'rows',coalesce((SELECT json_agg(json_build_array(q,calls,exec_ms,max_ms,family)) FROM selected),'[]'::json));
 `, pgSampleFamily("normalized"), index)
}
func pgSampleBlockersSQL() string {
	return `WITH a AS MATERIALIZED (
 SELECT *,lower(btrim(regexp_replace(left(coalesce(query,''),2048),'\s+',' ','g'))) normalized
 FROM pg_stat_activity WHERE pid<>pg_backend_pid()
 ), selected_waiters AS MATERIALIZED (SELECT * FROM a WHERE wait_event_type='Lock' ORDER BY query_start NULLS LAST,pid LIMIT 16),
 waiters AS MATERIALIZED (SELECT *,pg_blocking_pids(pid) blocking FROM selected_waiters),
 links AS (SELECT w.*,b.pid blocker_pid FROM waiters w CROSS JOIN LATERAL
 (SELECT pid FROM unnest(w.blocking) WITH ORDINALITY t(pid,n) WHERE n<=16) b)
 SELECT json_build_object('kind','blockers','sample',0,'at',extract(epoch FROM clock_timestamp()),
 'total',(SELECT count(*) FROM a WHERE wait_event_type='Lock'),
 'rows',coalesce((SELECT json_agg(json_build_array(w.pid::text,w.blocker_pid::text,coalesce(w.query_id::text,'none'),coalesce(b.query_id::text,'none'),
 ` + pgSampleFamily("w.normalized") + `,` + pgSampleFamily("b.normalized") + `,` + pgSampleState("b") + `,` + pgSampleWait("b") + `,` + pgSampleOwner("b") + `,` + pgSampleApp("b") + `,
 coalesce(greatest(0,extract(epoch FROM clock_timestamp()-b.xact_start)),0)::float8,
 b.pid IS NOT NULL,cardinality(w.blocking))) FROM links w LEFT JOIN a b ON b.pid=w.blocker_pid),'[]'::json));
 `
}
func pgQuerySampleSQL(database string) string {
	var b strings.Builder
	b.WriteString("SELECT json_build_object('kind','identity','sample',0,'at',extract(epoch FROM clock_timestamp()),'primary',NOT pg_is_in_recovery(),'read_only',current_setting('transaction_read_only')='on','database_ok',current_database()=" + "'" + strings.ReplaceAll(database, "'", "''") + "');\n")
	b.WriteString(pgSampleHistorySQL(0))
	for i := 0; i < pgQuerySampleCount; i++ {
		if i > 0 {
			b.WriteString("SELECT pg_sleep(2);\n")
		}
		b.WriteString(pgSampleActivitySQL(i))
	}
	b.WriteString(pgSampleBlockersSQL())
	b.WriteString(pgSampleHistorySQL(1))
	return b.String()
}

// One bounded subprocess; stdin delivery shares the same absolute owner budget
// as reads. Raw stderr is discarded; errors expose fixed categories only.
const pgQuerySampleProgram = `import json,os,selectors,subprocess,sys,time
p=None
try:
 cfg=json.loads(sys.stdin.buffer.read(262145));assert set(cfg)=={'password','user','database','port','sql'}
 assert all(type(cfg[k]) is str for k in ('password','user','database','sql')) and type(cfg['port']) is int
 assert len(cfg['sql'])<200000 and 0<cfg['port']<65536
 env=dict(os.environ,PGPASSWORD=cfg['password'],PGCONNECT_TIMEOUT='3',PGOPTIONS='-c statement_timeout=3000 -c default_transaction_read_only=on -c idle_in_transaction_session_timeout=3000')
 deadline=time.monotonic()+32
 p=subprocess.Popen(['psql','-X','-q','-A','-t','-h','localhost','-p',str(cfg['port']),'-U',cfg['user'],'-d',cfg['database'],'-v','ON_ERROR_STOP=1','-f','-'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,env=env)
 payload=memoryview(cfg['sql'].encode());output=bytearray();sel=selectors.DefaultSelector()
 for f in (p.stdin,p.stdout):os.set_blocking(f.fileno(),False)
 sel.register(p.stdin,selectors.EVENT_WRITE);sel.register(p.stdout,selectors.EVENT_READ)
 while sel.get_map():
  left=deadline-time.monotonic();assert left>0
  for key,event in sel.select(min(left,0.2)):
   if key.fileobj is p.stdin:
    n=os.write(p.stdin.fileno(),payload[:65536]);payload=payload[n:]
    if not payload:sel.unregister(p.stdin);p.stdin.close()
   else:
    chunk=os.read(p.stdout.fileno(),65536)
    if not chunk:sel.unregister(p.stdout);p.stdout.close()
    else:output.extend(chunk);assert len(output)<=4194304
 assert p.wait(timeout=max(.001,deadline-time.monotonic()))==0
 sys.stdout.buffer.write(output)
except BaseException:
 if p is not None and p.poll() is None:p.kill();p.wait()
 sys.exit(76)
`

type pgSampleWire struct {
	Kind       string              `json:"kind"`
	Sample     int                 `json:"sample"`
	At         float64             `json:"at"`
	Total      int                 `json:"total"`
	Groups     int                 `json:"groups"`
	Reset      *float64            `json:"reset"`
	Primary    bool                `json:"primary"`
	ReadOnly   bool                `json:"read_only"`
	DatabaseOK bool                `json:"database_ok"`
	Rows       [][]json.RawMessage `json:"rows"`
}
type pgSampleLoad struct {
	Query           string   `json:"query_token"`
	Family          string   `json:"family"`
	State           string   `json:"state"`
	Wait            string   `json:"wait"`
	Owner           string   `json:"client_owner"`
	Application     string   `json:"declared_application"`
	Backend         string   `json:"backend"`
	Scope           string   `json:"database_scope"`
	BackendSamples  int      `json:"backend_samples"`
	SeenSamples     int      `json:"seen_samples"`
	Peak            int      `json:"peak_count"`
	QueryAge        float64  `json:"max_query_age_s"`
	TransactionAge  float64  `json:"max_transaction_age_s"`
	StateAge        float64  `json:"max_state_age_s"`
	CompletedCalls  *float64 `json:"completed_lifetime_calls,omitempty"`
	CompletedMeanMS *float64 `json:"completed_lifetime_mean_ms,omitempty"`
	CompletedMaxMS  *float64 `json:"completed_lifetime_max_ms,omitempty"`
}
type pgSampleCompleted struct {
	Query  string  `json:"query_token"`
	Family string  `json:"family"`
	Calls  float64 `json:"lifetime_calls"`
	ExecMS float64 `json:"lifetime_exec_ms"`
}
type pgSampleBlocker struct {
	Waiter         string  `json:"waiter_token"`
	Blocker        string  `json:"blocker_token"`
	WaiterQuery    string  `json:"waiter_query_token"`
	BlockerQuery   string  `json:"blocker_query_token"`
	WaiterFamily   string  `json:"waiter_family"`
	BlockerFamily  string  `json:"blocker_family"`
	State          string  `json:"blocker_state"`
	Wait           string  `json:"blocker_wait"`
	Owner          string  `json:"client_owner"`
	Application    string  `json:"declared_application"`
	TransactionAge float64 `json:"max_transaction_age_s"`
	Observed       bool    `json:"blocker_observed"`
}
type pgQuerySampleReceipt struct {
	Schema                    int                 `json:"schema"`
	RequestedAt               time.Time           `json:"requested_at"`
	FinishedAt                time.Time           `json:"finished_at"`
	Complete                  bool                `json:"complete"`
	Reason                    string              `json:"reason,omitempty"`
	Samples                   int                 `json:"samples"`
	SampleClocks              []float64           `json:"sample_clocks"`
	BackendTotals             []int               `json:"backend_totals"`
	OmittedGroups             int                 `json:"omitted_group_samples"`
	Load                      []pgSampleLoad      `json:"load"`
	Completed                 []pgSampleCompleted `json:"completed_lifetime_end"`
	HistoryDeltaQualified     bool                `json:"history_delta_qualified"`
	HistoryRows               []int               `json:"history_rows"`
	HistoryPaired             int                 `json:"history_paired_ids"`
	HistoryUnpaired           int                 `json:"history_unpaired_ids"`
	HistoryNonmonotonic       int                 `json:"history_nonmonotonic_ids"`
	LoadOutputTruncated       bool                `json:"load_output_truncated"`
	CompletedOutputTruncated  bool                `json:"completed_output_truncated"`
	HistoryTruncated          bool                `json:"history_truncated"`
	Blockers                  []pgSampleBlocker   `json:"blockers"`
	LockWaiters               int                 `json:"lock_waiters"`
	BlockerSelectionTruncated bool                `json:"blocker_selection_truncated"`
	Qualifiers                []string            `json:"qualifiers"`
}

var pgSampleID = regexp.MustCompile(`^(none|-?[0-9]{1,20})$`)
var pgSampleAllowed = map[string][]string{
	"state":   {"active", "idle", "idle in transaction", "idle in transaction (aborted)", "fastpath function call", "disabled", "unknown"},
	"owner":   {"local", "loopback", "remote"},
	"app":     {"unset", "pg_dump", "pg_restore", "psql", "other"},
	"scope":   {"current", "other"},
	"backend": {"client backend", "parallel worker", "autovacuum worker", "autovacuum launcher", "checkpointer", "background writer", "walwriter", "walsender", "walreceiver", "logical replication worker", "logical replication launcher", "other"},
	"family":  {"commit", "rollback", "begin", "reindex_concurrent", "vacuum", "audit_daily_delete", "audit_daily_sum", "legacy_payout_range", "payout_finalize", "reliability_insert", "reliability_running", "pending_task_access", "tls_metadata_access", "signed_key_access", "escrow_access", "contract_close_access", "transfer_contract_access", "other"},
	"wait":    {"none", "LWLock:WALWrite", "LWLock:WALInsert", "IO:WALWrite", "IO:WALSync", "LWLock:WALSync", "LWLock:BufferMapping", "IO:DataFileRead", "IO:DataFileWrite", "IO:DataFileExtend", "Client:ClientRead", "Client:ClientWrite", "Lock:transactionid", "Lock:virtualxid", "Lock:relation", "IPC:MessageQueueReceive", "Activity:other", "BufferPin:other", "Client:other", "Extension:other", "IO:other", "IPC:other", "Lock:other", "LWLock:other", "Timeout:other", "other"},
}

func pgSampleAllowedValue(field, value string) bool {
	for _, allowed := range pgSampleAllowed[field] {
		if value == allowed {
			return true
		}
	}
	return false
}
func pgSampleUniqueObject(raw string) bool {
	d := json.NewDecoder(strings.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return false
		}
	}
	tok, err = d.Token()
	return err == nil && tok == json.Delim('}') && d.Decode(new(any)) == io.EOF
}

func pgSampleText(raw json.RawMessage) (string, error) {
	var s string
	err := json.Unmarshal(raw, &s)
	if err != nil || len(s) > 64 {
		return "", errors.New("text")
	}
	return s, nil
}
func pgSampleNumber(raw json.RawMessage) (float64, error) {
	var n float64
	err := json.Unmarshal(raw, &n)
	if string(raw) == "null" || err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 || n > 9007199254740991 {
		return 0, errors.New("number")
	}
	return n, nil
}
func parsePgQuerySample(raw string, now time.Time) (pgQuerySampleReceipt, error) {
	r := pgQuerySampleReceipt{Schema: 1, RequestedAt: now}
	fail := func() (pgQuerySampleReceipt, error) {
		return r, errors.New("monitor: invalid bounded PostgreSQL sample")
	}
	if len(raw) > pgQuerySampleMaxBytes || !strings.HasSuffix(raw, "\n") {
		return fail()
	}
	// Only sequential private ordinal tokens leave this reducer. Query IDs and
	// PIDs are never included in an error, receipt, log, or alert.
	tokens := map[string]string{}
	token := func(kind, id string) string {
		if id == "none" {
			return "unknown"
		}
		k := kind + id
		if v := tokens[k]; v != "" {
			return v
		}
		v := fmt.Sprintf("%s%d", kind, len(tokens)+1)
		tokens[k] = v
		return v
	}
	type history struct {
		calls, exec, max float64
		family           string
	}
	histories := []map[string]history{{}, {}}
	seenHistory := [2]bool{}
	seenIdentity := false
	seenBlockers := false
	loads := map[string]*pgSampleLoad{}
	queryForLoad := map[string]string{}
	lastClock := float64(0)
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var w pgSampleWire
		d := json.NewDecoder(strings.NewReader(line))
		d.DisallowUnknownFields()
		if !pgSampleUniqueObject(line) || d.Decode(&w) != nil || w.At < float64(now.Add(-time.Minute).Unix()) || w.At > float64(now.Add(pgQuerySampleBudget).Unix()) || w.At < lastClock || w.Total < 0 || w.Total > 1000000 {
			return fail()
		}
		lastClock = w.At
		switch w.Kind {
		case "identity":
			if seenIdentity || !w.Primary || !w.ReadOnly || !w.DatabaseOK {
				return fail()
			}
			seenIdentity = true
		case "history":
			if !seenIdentity || w.Sample < 0 || w.Sample > 1 || seenHistory[w.Sample] || len(w.Rows) > 5000 || len(w.Rows) > w.Total {
				return fail()
			}
			seenHistory[w.Sample] = true
			r.HistoryRows = append(r.HistoryRows, len(w.Rows))
			r.HistoryTruncated = r.HistoryTruncated || w.Total > len(w.Rows)
			for _, row := range w.Rows {
				if len(row) != 5 {
					return fail()
				}
				q, e := pgSampleText(row[0])
				if e != nil || !pgSampleID.MatchString(q) || q == "none" {
					return fail()
				}
				a, e := pgSampleNumber(row[1])
				if e != nil || math.Trunc(a) != a {
					return fail()
				}
				b, e := pgSampleNumber(row[2])
				if e != nil {
					return fail()
				}
				c, e := pgSampleNumber(row[3])
				if e != nil {
					return fail()
				}
				family, e := pgSampleText(row[4])
				if e != nil || !pgSampleAllowedValue("family", family) {
					return fail()
				}
				if _, exists := histories[w.Sample][q]; exists {
					return fail()
				}
				histories[w.Sample][q] = history{a, b, c, family}
			}
		case "activity":
			if !seenHistory[0] || seenHistory[1] || w.Sample != r.Samples || w.Sample >= pgQuerySampleCount || len(w.Rows) > 128 || w.Groups < len(w.Rows) || w.Groups > w.Total {
				return fail()
			}
			if r.Samples > 0 && w.At-r.SampleClocks[r.Samples-1] < 1.9 {
				return fail()
			}
			r.Samples++
			r.SampleClocks = append(r.SampleClocks, w.At)
			r.BackendTotals = append(r.BackendTotals, w.Total)
			r.OmittedGroups += w.Groups - len(w.Rows)
			seen := map[string]bool{}
			total := 0
			for _, row := range w.Rows {
				if len(row) != 12 {
					return fail()
				}
				parts := make([]string, 8)
				for i := range parts {
					v, e := pgSampleText(row[i])
					if e != nil {
						return fail()
					}
					parts[i] = v
				}
				if !pgSampleID.MatchString(parts[0]) {
					return fail()
				}
				for i, v := range parts[1:] {
					if !pgSampleAllowedValue([]string{"state", "wait", "owner", "app", "backend", "family", "scope"}[i], v) {
						return fail()
					}
				}
				nums := make([]float64, 4)
				for i := range nums {
					v, e := pgSampleNumber(row[i+8])
					if e != nil {
						return fail()
					}
					nums[i] = v
				}
				if nums[0] < 1 || nums[0] > 1e6 || math.Trunc(nums[0]) != nums[0] {
					return fail()
				}
				key := strings.Join(parts, "\x00")
				if seen[key] {
					return fail()
				}
				seen[key] = true
				total += int(nums[0])
				l := loads[key]
				if l == nil {
					l = &pgSampleLoad{Query: token("q", parts[0]), State: parts[1], Wait: parts[2], Owner: parts[3], Application: parts[4], Backend: parts[5], Family: parts[6], Scope: parts[7]}
					loads[key] = l
					queryForLoad[key] = parts[0]
				}
				l.BackendSamples += int(nums[0])
				l.SeenSamples++
				l.Peak = max(l.Peak, int(nums[0]))
				l.QueryAge = max(l.QueryAge, nums[1])
				l.TransactionAge = max(l.TransactionAge, nums[2])
				l.StateAge = max(l.StateAge, nums[3])
			}
			if total > w.Total || (w.Groups == len(w.Rows) && total != w.Total) {
				return fail()
			}
		case "blockers":
			if r.Samples != pgQuerySampleCount || seenBlockers || len(w.Rows) > 256 {
				return fail()
			}
			seenBlockers = true
			r.LockWaiters = w.Total
			r.BlockerSelectionTruncated = w.Total > 16
			for _, row := range w.Rows {
				if len(row) != 13 {
					return fail()
				}
				parts := make([]string, 10)
				for i := range parts {
					v, e := pgSampleText(row[i])
					if e != nil {
						return fail()
					}
					parts[i] = v
				}
				for _, v := range parts[:4] {
					if !pgSampleID.MatchString(v) {
						return fail()
					}
				}
				for i, v := range parts[4:] {
					if !pgSampleAllowedValue([]string{"family", "family", "state", "wait", "owner", "app"}[i], v) {
						return fail()
					}
				}
				age, e := pgSampleNumber(row[10])
				if e != nil {
					return fail()
				}
				var observed bool
				if json.Unmarshal(row[11], &observed) != nil {
					return fail()
				}
				count, e := pgSampleNumber(row[12])
				if e != nil || math.Trunc(count) != count || count < 1 {
					return fail()
				}
				r.BlockerSelectionTruncated = r.BlockerSelectionTruncated || count > 16
				r.Blockers = append(r.Blockers, pgSampleBlocker{token("p", parts[0]), token("p", parts[1]), token("q", parts[2]), token("q", parts[3]), parts[4], parts[5], parts[6], parts[7], parts[8], parts[9], age, observed})
			}
		default:
			return fail()
		}
	}
	if !seenIdentity || !seenHistory[0] || !seenHistory[1] || !seenBlockers || r.Samples != pgQuerySampleCount {
		return fail()
	}
	// Global reset and nondecreasing counters do not prove an entry survived
	// eviction/recreation or selective reset. Never publish interval deltas
	// without per-entry lifetime continuity. These are endpoint gauges only.
	r.HistoryDeltaQualified = false
	for key, l := range loads {
		if l.Scope == "current" {
			if h, ok := histories[1][queryForLoad[key]]; ok && h.calls > 0 {
				calls, mean, maxTime := h.calls, h.exec/h.calls, h.max
				l.CompletedCalls = &calls
				l.CompletedMeanMS = &mean
				l.CompletedMaxMS = &maxTime
			}
		}
		r.Load = append(r.Load, *l)
	}
	sort.Slice(r.Load, func(i, j int) bool {
		a, b := r.Load[i], r.Load[j]
		if (a.State == "active") != (b.State == "active") {
			return a.State == "active"
		}
		if a.BackendSamples != b.BackendSamples {
			return a.BackendSamples > b.BackendSamples
		}
		return a.Query < b.Query
	})
	if len(r.Load) > 80 {
		r.LoadOutputTruncated = true
		r.Load = r.Load[:80]
	}
	for q, b := range histories[1] {
		a, ok := histories[0][q]
		if !ok {
			r.HistoryUnpaired++
		} else {
			r.HistoryPaired++
			if b.calls < a.calls || b.exec < a.exec {
				r.HistoryNonmonotonic++
			}
		}
		r.Completed = append(r.Completed, pgSampleCompleted{token("q", q), b.family, b.calls, b.exec})
	}
	sort.Slice(r.Completed, func(i, j int) bool { return r.Completed[i].ExecMS > r.Completed[j].ExecMS })
	if len(r.Completed) > 30 {
		r.CompletedOutputTruncated = true
		r.Completed = r.Completed[:30]
	}

	r.Complete = true
	r.Qualifiers = []string{"12 snapshots are backend-samples, not distinct statements, continuous waits or CPU attribution", "query/transaction/state ages are separate; wait residence unknown", "completed statistics are endpoint entry-lifetime gauges, not sample-window or per-owner runtimes; exclude canceled/incomplete and possibly utility statements; eviction/selective-reset continuity is unproved, so interval deltas are withheld", "history is current database only, capped to top5000 lifetime execution-time IDs at each endpoint; missing entries unknown", "active groups prioritized then capped128 per snapshot, output top80; blockers one final snapshot of oldest16 lock waiters, at most16 blockers each", "SQL family matching is descriptive source shape, not runtime executable or task ownership", "local/loopback client owner and declared application do not identify originating service through PgBouncer", "NULL query IDs share an unknown token; recognized families only partly distinguish those statements", "raw SQL, database/application values, client addresses, PIDs and query IDs are not retained"}
	return r, nil
}

// Persist both contents and directory entry before any remote contact. A failed
// durability step consumes this local marker and fails closed; no automatic retry.
func pgSampleCreateAttempt(marker string, now time.Time, syncFile func(*os.File) error) (bool, error) {
	if syncFile == nil {
		syncFile = func(f *os.File) error { return f.Sync() }
	}
	f, err := os.OpenFile(marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, writeErr := f.WriteString(now.Format(time.RFC3339Nano) + "\n")
	syncErr := syncFile(f)
	closeErr := f.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return false, err
	}
	dir, err := os.Open(filepath.Dir(marker))
	if err != nil {
		return false, err
	}
	syncErr = syncFile(dir)
	closeErr = dir.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return false, err
	}
	// The per-signal directory can have just been created. Persist its entry
	// in the existing state directory as well as the marker entry itself.
	parent, err := os.Open(filepath.Dir(filepath.Dir(marker)))
	if err != nil {
		return false, err
	}
	syncErr = syncFile(parent)
	closeErr = parent.Close()
	if err := errors.Join(syncErr, closeErr); err != nil {
		return false, err
	}
	return true, nil
}

func (p pgQuerySampleProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	until := env.cfg.pgQuerySampleUntil
	if until.IsZero() {
		return nil, nil
	}
	now := env.now().UTC()
	if !now.Before(until) || until.Sub(now) > 24*time.Hour {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithDeadline(ctx, until)
	defer cancel()
	h := env.cfg.hostByRole("pg-primary")
	if h == nil || h.disabled {
		return nil, errors.New("monitor: bounded PG sample primary unavailable")
	}
	if env.cfg.routerGenerationCheck != nil {
		ok, err := env.cfg.routerGenerationCheck(ctx)
		if err != nil || !ok {
			return nil, errors.New("monitor: bounded PG sample inventory changed")
		}
	}
	dir := filepath.Join(env.cfg.stateDir, "pg-query-sample")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, errors.New("monitor: bounded PG sample state unavailable")
	}
	hash := sha256.Sum256([]byte(until.UTC().Format(time.RFC3339Nano)))
	name := hex.EncodeToString(hash[:8])
	marker := filepath.Join(dir, name+".attempt")
	created, err := pgSampleCreateAttempt(marker, now, p.syncAttemptFile)
	if err != nil {
		return nil, errors.New("monitor: bounded PG sample durable marker unavailable")
	}
	if !created {
		return nil, nil
	}

	r := pgQuerySampleReceipt{Schema: 1, RequestedAt: now, Reason: "source-unavailable"}
	input, _ := json.Marshal(map[string]any{"password": env.cfg.pgPassword, "user": env.cfg.pgUser, "database": env.cfg.pgDb, "port": env.cfg.pgPort, "sql": pgQuerySampleSQL(env.cfg.pgDb)})
	command := "[ \"$(hostname -s)\" = " + shellSingleQuote(h.name) + " ] || exit 74; exec timeout -k 2s 35s python3 -c " + shellSingleQuote(pgQuerySampleProgram)
	out, readErr := env.runner.sshTimeout(ctx, h, command, string(input), pgQuerySampleBudget)
	if readErr == nil {
		parsed, parseErr := parsePgQuerySample(out, now)
		if parseErr == nil {
			r = parsed
		} else {
			r.Reason = "projection-unavailable"
		}
	}
	r.FinishedAt = env.now().UTC()
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(raw) > 262144 {
		return nil, errors.New("monitor: bounded PG sample receipt exceeds bound")
	}
	tmp, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return nil, errors.New("monitor: bounded PG sample receipt unavailable")
	}
	defer os.Remove(tmp.Name())
	_, werr := tmp.Write(append(raw, '\n'))
	cerr := tmp.Close()
	if werr != nil || cerr != nil || os.Rename(tmp.Name(), filepath.Join(dir, name+".json")) != nil {
		return nil, errors.New("monitor: bounded PG sample receipt write failed")
	}
	if r.Complete {
		return nil, nil
	}
	return []finding{{probeId: "pg/query-sample", tier: tierWarn, class: "pg-query-sample-unavailable", target: pgTarget(env), sustain: 1, symptom: "Bounded PostgreSQL query/load sample is incomplete", observed: "reason=" + r.Reason, evidence: "Finite private receipt retained in pg-query-sample; no raw SQL or identities", mechanism: "Admission, source or projection was unavailable; the spent attempt is not retried.", baseline: "One complete read-only12-snapshot sample under the shared monitor host budget.", action: "Inspect private finite receipt before authorizing a successor; do not infer health or tune pools from missing evidence.", verify: "A separately authorized finite sample qualifies.", playbook: "SIGNALS.md §2.1a"}}, nil
}

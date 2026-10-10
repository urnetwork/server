// SIGNALS.md §8.12a retains a finite current-process release witness.
package monitor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	apiReleaseProofMaxBytes = 1024 * 1024
	apiReleaseProofMaxRows  = 1024
	apiReleaseProofBudget   = 40 * time.Second
)

// APIReleaseProofSettings is a process-owned, finite expectation, separate
// from mutable service config annotations. It never changes deployed state.
type APIReleaseProofSettings struct {
	Environment     string               `json:"environment"`
	Revision        string               `json:"revision"`
	Version         string               `json:"version"`
	ImageDigests    []string             `json:"image_digests"`
	SelectionFloors map[string]time.Time `json:"selection_floors"`
	ExpiresAt       time.Time            `json:"expires_at"`
}

func cloneAPIReleaseProofSettings(p *APIReleaseProofSettings) *APIReleaseProofSettings {
	if p == nil {
		return nil
	}
	c := *p
	c.ImageDigests = append([]string(nil), p.ImageDigests...)
	c.SelectionFloors = make(map[string]time.Time, len(p.SelectionFloors))
	for k, v := range p.SelectionFloors {
		c.SelectionFloors[k] = v
	}
	return &c
}

var releaseProofName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
var releaseProofVersion = regexp.MustCompile(`^[0-9A-Za-z.+_-]{1,128}$`)

func (p *APIReleaseProofSettings) validate() error {
	invalid := func() error { return fmt.Errorf("monitor: invalid API release proof expectation") }
	if !releaseProofName.MatchString(p.Environment) || !validGoSourceRevision(p.Revision) || !releaseProofVersion.MatchString(p.Version) || len(p.ImageDigests) == 0 || len(p.ImageDigests) > 8 || len(p.SelectionFloors) == 0 || len(p.SelectionFloors) > 8 || p.ExpiresAt.IsZero() {
		return invalid()
	}
	seen := map[string]bool{}
	for _, digest := range p.ImageDigests {
		if !validOCIImageDigest(digest) || seen[digest] {
			return invalid()
		}
		seen[digest] = true
	}
	for block, floor := range p.SelectionFloors {
		if !releaseProofName.MatchString(block) || floor.IsZero() || !floor.Before(p.ExpiresAt) || p.ExpiresAt.Sub(floor) > 48*time.Hour {
			return invalid()
		}
	}
	return nil
}

// LoadAPIReleaseProofSettings accepts only a small local JSON object. Parser
// input and errors never enter diagnostics; the canonical expectation hash is
// retained in the finite receipt instead of its raw contents.
func LoadAPIReleaseProofSettings(path string) (*APIReleaseProofSettings, error) {
	// Nonblocking open plus the descriptor check rejects FIFOs/devices even
	// if the path changes during admission. Only regular local files qualify.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("monitor: cannot open API release proof expectation")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("monitor: API release proof expectation must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil || len(raw) > 16384 {
		return nil, fmt.Errorf("monitor: API release proof expectation exceeds its read bound")
	}
	if !uniqueReleaseProofJSONKeys(raw) {
		return nil, fmt.Errorf("monitor: invalid API release proof JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var p APIReleaseProofSettings
	if err := decoder.Decode(&p); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("monitor: invalid API release proof JSON")
	}
	if err := p.validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

func uniqueReleaseProofJSONKeys(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 4 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		delim, nested := token.(json.Delim)
		if !nested {
			return true
		}
		if delim != '{' && delim != '[' {
			return false
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || keys[name] {
					return false
				}
				// encoding/json also matches struct fields case-insensitively.
				// Permit only the canonical root spelling so aliases cannot
				// create a second, silently overriding policy value.
				if depth == 0 && !slices.Contains([]string{"environment", "revision", "version", "image_digests", "selection_floors", "expires_at"}, name) {
					return false
				}
				keys[name] = true
			}
			if !value(depth + 1) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && ((delim == '{' && end == json.Delim('}')) || (delim == '[' && end == json.Delim(']')))
	}
	return value(0)
}

func NewAPIReleaseProofSignal() Signal {
	return &signalAdapter{number: "8.12a", key: "api-release-proof", name: "Bounded API release proof", probe: apiReleaseProofProbe{}}
}

type apiReleaseProofProbe struct{}

func (apiReleaseProofProbe) id() string               { return "deploy/api-release-proof" }
func (apiReleaseProofProbe) tier() string             { return tierWarn }
func (apiReleaseProofProbe) cadence() time.Duration   { return 15 * time.Minute }
func (apiReleaseProofProbe) runBudget() time.Duration { return apiReleaseProofBudget }

var apiReleaseProofFamilies = map[string]string{
	"start": "process_start_time_seconds", "source": "urnetwork_source_info",
	"build": "urnetwork_build_info", "ready": "urnetwork_api_ready",
}

func apiReleaseProofQuery(environment string, scope providerPickerScope) string {
	pattern := func(values []string) string {
		parts := append([]string(nil), values...)
		sort.Strings(parts)
		for i := range parts {
			parts[i] = regexp.QuoteMeta(parts[i])
		}
		return strconv.Quote(strings.Join(parts, "|"))
	}
	selector := fmt.Sprintf("env=%s,job=\"api\",host=~%s,block=~%s", strconv.Quote(environment), pattern(scope.hosts), pattern(scope.blocks))
	parts := []string{}
	for _, family := range []string{"start", "source", "build", "ready"} {
		metric := apiReleaseProofFamilies[family] + "{" + selector + "}"
		for _, isTime := range []bool{false, true} {
			value, label := metric, family
			if isTime {
				value, label = "timestamp("+metric+")", family+"/time"
			}
			parts = append(parts, "label_replace(("+value+"),\"monitor_release\","+strconv.Quote(label)+",\"\",\"\")")
		}
	}
	return strings.Join(parts, " or ")
}

type apiReleaseProofBlock struct {
	Targeted     bool           `json:"targeted"`
	Expected     int            `json:"expected_slots"`
	CurrentReady int            `json:"current_source_ready_slots"`
	Qualified    int            `json:"qualified_slots"`
	Reasons      map[string]int `json:"unqualified_reasons"`
}

type apiReleaseProofReceipt struct {
	Schema          int                              `json:"schema"`
	EvaluationAt    time.Time                        `json:"evaluation_at"`
	CompletedAt     time.Time                        `json:"completed_at"`
	ExpectationHash string                           `json:"expectation_sha256"`
	SourceAvailable bool                             `json:"source_available"`
	Qualified       bool                             `json:"all_targeted_slots_qualified"`
	Reason          string                           `json:"reason,omitempty"`
	Blocks          map[string]*apiReleaseProofBlock `json:"blocks"`
}

func newAPIReleaseProofReceipt(p *APIReleaseProofSettings, now time.Time, scope providerPickerScope) apiReleaseProofReceipt {
	raw, _ := json.Marshal(p)
	hash := sha256.Sum256(raw)
	r := apiReleaseProofReceipt{Schema: 1, EvaluationAt: now, ExpectationHash: hex.EncodeToString(hash[:]), Blocks: map[string]*apiReleaseProofBlock{}}
	for slot := range scope.slots {
		_, block, _ := strings.Cut(slot, "\x00")
		if r.Blocks[block] == nil {
			_, targeted := p.SelectionFloors[block]
			r.Blocks[block] = &apiReleaseProofBlock{Targeted: targeted, Reasons: map[string]int{}}
		}
		r.Blocks[block].Expected++
	}
	return r
}

type apiReleaseProofSample struct {
	pickerSample
	labels [4]string
	seen   bool
}

func parseAPIReleaseProof(raw, environment string, now time.Time, scope providerPickerScope, expectation *APIReleaseProofSettings) apiReleaseProofReceipt {
	r := newAPIReleaseProofReceipt(expectation, now, scope)
	invalid := func() apiReleaseProofReceipt {
		bad := newAPIReleaseProofReceipt(expectation, now, scope)
		bad.Reason = "invalid-source-response"
		return bad
	}
	var response struct {
		mimirInstantResponse
		Warnings []string `json:"warnings"`
		Infos    []string `json:"infos"`
	}
	if len(raw) > apiReleaseProofMaxBytes || json.Unmarshal([]byte(raw), &response) != nil || response.Status != "success" || response.Data.ResultType != "vector" || len(response.Warnings) != 0 || len(response.Infos) != 0 || len(response.Data.Result) > apiReleaseProofMaxRows {
		return invalid()
	}
	processes := map[pickerProcessKey]map[string]apiReleaseProofSample{}
	for _, row := range response.Data.Result {
		at, value, err := mimirInstantValue(row.Value)
		if err != nil || at.Unix() != now.Unix() || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 9007199254740991 {
			return invalid()
		}
		m := row.Metric
		key := pickerProcessKey{m["host"], m["block"], m["instance"]}
		if m["env"] != environment || m["job"] != "api" || !scope.slots[key.host+"\x00"+key.block] || key.instance == "" || len(key.instance) > 512 {
			return invalid()
		}
		field := m["monitor_release"]
		isTime := strings.HasSuffix(field, "/time")
		family := strings.TrimSuffix(field, "/time")
		metric, ok := apiReleaseProofFamilies[family]
		if !ok || (m["__name__"] != "" && m["__name__"] != metric) {
			return invalid()
		}
		for label, v := range m {
			allowed := label == "env" || label == "job" || label == "host" || label == "block" || label == "instance" || label == "monitor_release" || label == "__name__" || (family == "source" && (label == "revision" || label == "modified" || label == "image_digest")) || (family == "build" && label == "version")
			if !allowed || len(v) > 512 {
				return invalid()
			}
		}
		if processes[key] == nil {
			processes[key] = map[string]apiReleaseProofSample{}
			if len(processes) > 80 {
				return invalid()
			}
		}
		sample := processes[key][family]
		labels := [4]string{m["revision"], m["modified"], m["image_digest"], m["version"]}
		if sample.seen && labels != sample.labels {
			return invalid()
		}
		sample.seen, sample.labels = true, labels
		if isTime {
			if sample.hasTime {
				return invalid()
			}
			sample.sourceTime, sample.hasTime = value, true
		} else {
			if sample.hasValue {
				return invalid()
			}
			sample.value, sample.hasValue = value, true
		}
		processes[key][family] = sample
	}
	fresh := func(s apiReleaseProofSample) bool {
		age := float64(now.Unix()) - s.sourceTime
		return s.hasValue && s.hasTime && s.sourceTime > 0 && age >= -30 && age <= 90
	}
	r.SourceAvailable = true
	for slot, allowed := range scope.slots {
		host, block, _ := strings.Cut(slot, "\x00")
		out := r.Blocks[block]
		reason := "missing-process"
		var chosen map[string]apiReleaseProofSample
		newest, count, ties, unknown := float64(0), 0, 0, false
		for key, samples := range processes {
			if key.host != host || key.block != block {
				continue
			}
			count++
			start := samples["start"]
			if !fresh(start) || start.value <= 0 || start.value > float64(now.Unix()+30) {
				unknown = true
				continue
			}
			if start.value > newest {
				newest, chosen, ties = start.value, samples, 1
			} else if start.value == newest {
				ties++
			}
		}
		if !allowed {
			reason = "excluded-slot"
		} else if unknown || ties > 1 || count > 4 {
			reason = "ambiguous-process"
		} else if chosen != nil {
			reason = ""
			for _, family := range []string{"source", "build", "ready"} {
				sample := chosen[family]
				if !fresh(sample) || sample.sourceTime != chosen["start"].sourceTime || sample.value != 1 {
					reason = "missing-stale-or-not-ready"
				}
			}
			source, build := chosen["source"], chosen["build"]
			if !validGoSourceRevision(source.labels[0]) || (source.labels[1] != "true" && source.labels[1] != "false") || !validOCIImageDigest(source.labels[2]) || !releaseProofVersion.MatchString(build.labels[3]) {
				reason = "invalid-artifact-witness"
			}
			if reason == "" {
				out.CurrentReady++
				if out.Targeted {
					floor := expectation.SelectionFloors[block]
					switch {
					case newest <= float64(floor.UnixNano())/1e9:
						reason = "start-not-after-selection"
					case source.labels[0] != expectation.Revision || source.labels[1] != "false":
						reason = "source-mismatch"
					case !slices.Contains(expectation.ImageDigests, source.labels[2]):
						reason = "image-mismatch"
					case build.labels[3] != expectation.Version:
						reason = "version-mismatch"
					default:
						out.Qualified++
					}
				}
			}
		}
		if reason != "" {
			out.Reasons[reason]++
		}
	}
	// Evaluate final counts after every slot; partial prefixes cannot qualify.
	r.Qualified = len(expectation.SelectionFloors) > 0
	for block := range expectation.SelectionFloors {
		out := r.Blocks[block]
		r.Qualified = r.Qualified && out != nil && out.Expected > 0 && out.Qualified == out.Expected
	}
	return r
}

// A bounded loopback-only read emits raw metrics to the local in-memory
// reducer. Neither query nor labels enter shell arguments or persisted logs.
func apiReleaseProofProgram(query string, now time.Time) string {
	q, _ := json.Marshal(query)
	return fmt.Sprintf(`import signal,sys,urllib.parse,urllib.request
signal.signal(signal.SIGALRM,lambda *_: (_ for _ in ()).throw(TimeoutError()))
signal.alarm(19)
try:
 class NoRedirect(urllib.request.HTTPRedirectHandler):
  def redirect_request(self,*args,**kwargs): return None
 opener=urllib.request.build_opener(urllib.request.ProxyHandler({}),NoRedirect())
 data=urllib.parse.urlencode({'query':%s,'time':%d}).encode()
 with opener.open('http://127.0.0.1:3100/prometheus/api/v1/query',data=data,timeout=15) as response:
  raw=response.read(%d)
 if len(raw)>%d: raise ValueError()
 sys.stdout.buffer.write(raw)
except Exception:
 raise SystemExit(1)
`, q, now.Unix(), apiReleaseProofMaxBytes+1, apiReleaseProofMaxBytes)
}

func writeAPIReleaseProofReceipt(stateDir string, r apiReleaseProofReceipt) error {
	dir := filepath.Join(stateDir, "api-release-proof")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("monitor: cannot create API release proof receipt directory")
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil || len(raw) > 65536 {
		return fmt.Errorf("monitor: invalid API release proof receipt")
	}
	f, err := os.CreateTemp(dir, ".receipt-")
	if err != nil {
		return fmt.Errorf("monitor: cannot create API release proof receipt")
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(append(raw, '\n')); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(f.Name(), filepath.Join(dir, "latest.json")) != nil {
		return fmt.Errorf("monitor: cannot publish API release proof receipt")
	}
	return nil
}

func (apiReleaseProofProbe) check(ctx context.Context, env *probeEnv) ([]finding, error) {
	p := env.cfg.apiReleaseProof
	if p == nil {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := env.now().UTC().Truncate(time.Second)
	scope := pickerScope(env)
	r := newAPIReleaseProofReceipt(p, now, scope)
	switch {
	case p.Environment != env.cfg.env:
		r.Reason = "environment-mismatch"
	case !now.Before(p.ExpiresAt):
		r.Reason = "expectation-expired"
	case len(scope.slots) == 0 || len(scope.slots) > providerPickerMaxSlots || len(scope.hosts) == 0 || len(scope.blocks) == 0:
		r.Reason = "inventory-unavailable-or-over-bound"
	default:
		for block, floor := range p.SelectionFloors {
			if !slices.Contains(scope.blocks, block) || floor.After(now) {
				r.Reason = "selection-outside-inventory-or-future"
			}
		}
	}
	if r.Reason == "" {
		var gateway *host
		for _, h := range env.cfg.hostsWithRole("services") {
			if !h.disabled && !scope.excluded[h.name] {
				gateway = h
				break
			}
		}
		if gateway == nil {
			r.Reason = "gateway-unavailable"
		} else {
			// Exactly one selected gateway, no retry/failover after contact.
			command := "[ \"$(hostname -s)\" = " + shellSingleQuote(gateway.name) + " ] || exit 74; exec timeout 20s python3 -"
			out, err := env.runner.sshTimeout(ctx, gateway, command, apiReleaseProofProgram(apiReleaseProofQuery(env.cfg.env, scope), now), 20*time.Second)
			if err != nil {
				r.Reason = "bounded-source-unavailable"
			} else {
				r = parseAPIReleaseProof(out, env.cfg.env, now, scope, p)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r.CompletedAt = env.now().UTC()
	if err := writeAPIReleaseProofReceipt(env.cfg.stateDir, r); err != nil {
		return nil, err
	}
	if r.Qualified {
		return nil, nil // The positive witness is the private, atomic receipt.
	}
	return []finding{{probeId: "deploy/api-release-proof", tier: tierWarn, class: "api-release-unqualified", target: "api-release", sustain: 1,
		symptom:   "API release identity or readiness remains unqualified",
		mechanism: "The newest fresh API process must match the explicit source, immutable image and version expectation, pass startup readiness, and start after its block selection floor.",
		baseline:  "Every inventory-required slot in each targeted block qualifies in one coherent fresh scrape.",
		observed:  fmt.Sprintf("source_available=%t all_targeted_slots_qualified=%t reason=%s", r.SourceAvailable, r.Qualified, firstNonempty(r.Reason, "incomplete-current-witness")),
		evidence:  "Finite per-block counts and reason categories are retained in the private state directory's api-release-proof/latest.json; no host, process, query, address or provider labels are retained.",
		context:   "Missing or stale observations are not proof of an outage. This operational source/readiness witness is not remote executable-byte attestation, continuous health, search-index readiness or a latency measurement.",
		action:    "Inspect the current private receipt and pinned expectation. Hold wider promotion until qualified; preserve the shared monitor admission budget.",
		verify:    "A later fresh receipt qualifies every targeted slot against the same reviewed expectation; no absent receipt or alert silence supplies proof.", playbook: "SIGNALS.md §8.12a"}}, nil
}

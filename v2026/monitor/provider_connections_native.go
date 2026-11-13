// Bounded anomaly-only native cohort observations for the provider canary.
package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

type providerConnectionsNativeRow struct {
	Block string  `json:"block"`
	Start float64 `json:"start"`
}

type providerConnectionsNativeFrame struct {
	Schema      int                            `json:"schema"`
	Host        string                         `json:"host"`
	Environment string                         `json:"environment"`
	Started     float64                        `json:"started"`
	Completed   float64                        `json:"completed"`
	Complete    bool                           `json:"complete"`
	Rows        []providerConnectionsNativeRow `json:"rows"`
}

type providerConnectionsNativePoint struct {
	started   time.Time
	completed time.Time
	value     float64
}

type providerConnectionsNativeObservation struct {
	current  float64
	best     float64
	complete bool
	at       time.Time
	reason   string
	err      error
}
type providerConnectionsNativeWindow struct {
	scope      string
	points     []providerConnectionsNativePoint
	provedBest float64
	provedAt   time.Time
}

func (self *providerConnectionsProbe) resetNative() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.window.points = nil
}

// Counts and length-framed fields separate hosts from blocks even when a
// topology change moves an identical name across those two inventories.
func providerConnectionsScopeKey(environment string, hosts []*host, blocks []string) string {
	var key strings.Builder
	field := func(value string) {
		key.WriteString(strconv.Itoa(len(value)))
		key.WriteByte(':')
		key.WriteString(value)
	}
	field(environment)
	field(strconv.Itoa(len(hosts)))
	for _, h := range hosts {
		field(h.name)
	}
	field(strconv.Itoa(len(blocks)))
	for _, block := range blocks {
		field(block)
	}
	return key.String()
}

// Desired scope is a local ownership boundary, independent of returned labels.
func (self *providerConnectionsProbe) bindNativeScope(environment string, hosts []*host, blocks []string) {
	scope := providerConnectionsScopeKey(environment, hosts, blocks)
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.window.scope != scope {
		self.window = providerConnectionsNativeWindow{scope: scope}
		self.lastFailure = nil
	}
}

func (self *providerConnectionsProbe) clearNative() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.window = providerConnectionsNativeWindow{}
	self.lastFailure = nil
}

// Qualified recovery invalidates the older incident witness immediately.
func (self *providerConnectionsProbe) clearNativeFailure() {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	self.lastFailure = nil
	self.window.provedAt = time.Time{}
}

// Retain a proved failure for one cadence with its original measurement clock.
// Later unavailable runs remain visibility-only; the notifier retains its page.
func (self *providerConnectionsProbe) unknownFindings(target, reason string, now time.Time) []finding {
	out := []finding{providerConnectionsUnknown(target, reason)}
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.lastFailure != nil && now.Sub(self.lastFailureAt) >= 0 && now.Sub(self.lastFailureAt) <= 75*time.Second {
		prior := *self.lastFailure
		prior.observed += " observation_state=prior_complete_window current_source=unknown"
		out = append(out, prior)
	}
	return out
}

// Actual reader intervals, including admission and transport, bracket each
// census. Require five minutes even if the first read is slow and the last is
// fast. Seven bounded points permit one extra cadence for nonzero durations.
func (self *providerConnectionsProbe) addNative(scope string, started, completed time.Time, value float64) (float64, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	if self.window.scope != scope {
		self.window = providerConnectionsNativeWindow{scope: scope}
	}
	points := self.window.points
	if completed.Before(started) || completed.Sub(started) > 22*time.Second {
		self.window.points = nil
		return 0, false
	}
	if len(points) > 0 {
		last := points[len(points)-1]
		gap := started.Sub(last.started)
		if started.Before(last.completed) || completed.Before(last.completed) || gap > 75*time.Second {
			points = nil
		} else if gap < 45*time.Second {
			return 0, false
		}
	}
	points = append(points, providerConnectionsNativePoint{started: started, completed: completed, value: value})
	if len(points) > 7 {
		points = append([]providerConnectionsNativePoint(nil), points[len(points)-7:]...)
	}
	// Keep the shortest available suffix whose conservative interval separation
	// spans five minutes, so an obsolete high point cannot prolong the window.
	for len(points) > 6 && started.Sub(points[1].completed) >= 5*time.Minute {
		points = points[1:]
	}
	self.window.points = points
	if len(points) < 6 || started.Sub(points[0].completed) < 5*time.Minute || completed.Sub(points[0].started) > 6*time.Minute+15*time.Second {
		return 0, false
	}
	best := 0.0
	for _, point := range points {
		best = max(best, point.value)
	}
	self.window.provedBest, self.window.provedAt = best, completed
	return best, true
}

// A prior complete native failure remains independently visible for one cadence
// after a source gap. It is labelled with its own window end, never current.
func (self *providerConnectionsProbe) priorNativeFailure(now time.Time, level float64) (float64, time.Time, bool) {
	self.stateLock.Lock()
	defer self.stateLock.Unlock()
	age := now.Sub(self.window.provedAt)
	return self.window.provedBest, self.window.provedAt, !self.window.provedAt.IsZero() && age >= 0 && age <= 75*time.Second && level >= providerConnectionsBaselineFloor && self.window.provedBest < providerConnectionsCollapseRatio*level
}

// Runs at most one bounded reader per configured host, at most four, through
// the existing runner/admission. It never retries, invokes a container, reads
// service endpoints or contacts PostgreSQL. Missing live gauges remain unknown.
func (self *providerConnectionsProbe) nativeSample(ctx context.Context, env *probeEnv, now time.Time, hosts []*host, blocks []string, frame *providerConnectionsFrame) providerConnectionsNativeObservation {
	if ctx.Err() != nil {
		self.resetNative()
		return providerConnectionsNativeObservation{reason: "native_census_cancelled", err: ctx.Err()}
	}
	type result struct {
		host      *host
		frame     providerConnectionsNativeFrame
		started   time.Time
		completed time.Time
		ok        bool
		err       error
	}
	results := make(chan result, len(hosts))
	var wait sync.WaitGroup
	for _, h := range hosts {
		wait.Add(1)
		go func(h *host) {
			defer wait.Done()
			readCtx, cancel := context.WithTimeout(ctx, 22*time.Second)
			defer cancel()
			if readCtx.Err() != nil {
				results <- result{host: h, err: readCtx.Err()}
				return
			}
			began := env.now().UTC()
			out, err := env.runner.shell(readCtx, h, providerConnectionsNativeCommand(env.cfg.env, h.name, blocks))
			ended := env.now().UTC()
			parsed, ok := parseProviderConnectionsNative(out, env.cfg.env, h.name, began, blocks)
			clockOk := !began.Before(now) && !ended.Before(began) && ended.Sub(began) <= 22*time.Second && ended.Sub(now) <= 50*time.Second
			results <- result{host: h, frame: parsed, started: began, completed: ended, ok: err == nil && readCtx.Err() == nil && ok && clockOk, err: errors.Join(err, readCtx.Err())}
		}(h)
	}
	wait.Wait()
	close(results)
	frames := map[string]providerConnectionsNativeFrame{}
	var started, completed time.Time
	var causes []error
	valid := true
	for r := range results {
		valid = valid && r.ok
		if r.err != nil {
			causes = append(causes, r.err)
		}
		frames[r.host.name] = r.frame
		if started.IsZero() || r.started.Before(started) {
			started = r.started
		}
		if completed.IsZero() || r.completed.After(completed) {
			completed = r.completed
		}
	}
	if ctx.Err() != nil {
		self.resetNative()
		return providerConnectionsNativeObservation{reason: "native_census_cancelled", err: errors.Join(append(causes, ctx.Err())...)}
	}
	if !valid || started.IsZero() || completed.Sub(started) > 22*time.Second {
		self.resetNative()
		return providerConnectionsNativeObservation{reason: "native_census_unavailable", err: errors.Join(causes...)}
	}
	total := 0.0
	for _, h := range hosts {
		for _, row := range frames[h.name].Rows {
			matches, value := 0, 0.0
			for id, process := range frame.processes {
				start := process.samples["start"]
				if id.host != h.name || id.block != row.Block || !start.valueSeen || math.Abs(start.value-row.Start) > 0.000001 {
					continue
				}
				matches++
				var reason string
				value, reason = providerConnectionsProcessValue(process, completed)
				if reason != "" {
					self.resetNative()
					return providerConnectionsNativeObservation{reason: "native_live_process_gauge_unavailable"}
				}
			}
			if matches != 1 {
				self.resetNative()
				return providerConnectionsNativeObservation{reason: "native_process_generation_unmatched"}
			}
			total += value
		}
	}
	if total > providerConnectionsMaximum {
		self.resetNative()
		return providerConnectionsNativeObservation{reason: "native_count_invalid"}
	}
	// A complete, stable empty slot contributes zero only to this observation.
	// No prior minute or absent app_version bucket is filled from that fact.
	best, complete := self.addNative(providerConnectionsScopeKey(env.cfg.env, hosts, blocks), started, completed, total)
	return providerConnectionsNativeObservation{current: total, best: best, complete: complete, at: completed}
}

// Each host proves bounded stable enumeration and an executable/process start.
// All rows, including draining generations, are joined; equal starts fail closed.
func parseProviderConnectionsNative(raw, environment, host string, now time.Time, blocks []string) (providerConnectionsNativeFrame, bool) {
	var frame providerConnectionsNativeFrame
	if len(raw) > 65536 || !providerConnectionsJsonShape(raw) {
		return frame, false
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&frame) != nil || !frame.Complete || frame.Schema != 1 || frame.Host != host || frame.Environment != environment || (frame.Rows == nil || len(frame.Rows) > 64) {
		return frame, false
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return frame, false
	}
	validClock := func(value float64) bool {
		return !math.IsNaN(value) && !math.IsInf(value, 0) && value > 0 && math.Abs(value-float64(now.UnixMilli())/1000) <= 30
	}
	if !validClock(frame.Started) || !validClock(frame.Completed) || frame.Completed < frame.Started || frame.Completed-frame.Started > 20 {
		return frame, false
	}
	allowed, seen := map[string]bool{}, map[string]bool{}
	for _, block := range blocks {
		allowed[block] = true
	}
	for _, row := range frame.Rows {
		key := row.Block + "\x00" + strconv.FormatFloat(row.Start, 'g', -1, 64)
		if !allowed[row.Block] || row.Start <= 0 || math.IsNaN(row.Start) || math.IsInf(row.Start, 0) || row.Start > frame.Started+30 || seen[key] {
			return frame, false
		}
		seen[key] = true
	}
	return frame, true
}

// The reader speaks only to the local Docker Unix socket and bounded /proc
// metadata. Every exception emits a fixed incomplete frame; no argv or labels
// from unrelated containers, environment variables, logs or errors are returned.
func providerConnectionsNativeCommand(environment, host string, blocks []string) string {
	encoded, _ := json.Marshal(blocks)
	return "# monitor-provider-connections-native\npython3 - " + shellSingleQuote(environment) + " " + shellSingleQuote(host) + " " + shellSingleQuote(string(encoded)) + " <<'PROVIDER_NATIVE_PY'\n" + providerConnectionsNativeScript + "\nPROVIDER_NATIVE_PY"
}

const providerConnectionsNativeScript = `import json, math, os, re, selectors, signal, subprocess, sys, time
ENV, HOST, BLOCKS = sys.argv[1], sys.argv[2], set(json.loads(sys.argv[3]))
BEGIN = time.monotonic()
STARTED = time.time()
DOCKER = ['/usr/bin/docker', '-H', 'unix:///var/run/docker.sock']
LIST = '{"id":{{json .ID}},"name":{{json .Names}},"env":{{json (.Label "warp.env")}},"job":{{json (.Label "warp.service")}},"block":{{json (.Label "warp.block")}}}'
DETAIL = '{"id":{{json .Id}},"name":{{json .Name}},"pid":{{json .State.Pid}},"running":{{json .State.Running}},"started":{{json .State.StartedAt}},"path":{{json .Path}},"env":{{json (index .Config.Labels "warp.env")}},"job":{{json (index .Config.Labels "warp.service")}},"block":{{json (index .Config.Labels "warp.block")}}}'
def need(ok):
    if not ok: raise ValueError('unavailable')
def clock():
    need(time.monotonic()-BEGIN < 20)
def decode(raw):
    def unique(pairs):
        out = {}
        for key, value in pairs:
            need(key not in out)
            out[key] = value
        return out
    return json.loads(raw, object_pairs_hook=unique)
def command(args):
    clock()
    end = min(BEGIN+20, time.monotonic()+4)
    p = subprocess.Popen(DOCKER+args, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, env={'PATH':'/usr/bin:/bin','LANG':'C'}, start_new_session=True)
    result = bytearray()
    try:
        with selectors.DefaultSelector() as sel:
            sel.register(p.stdout, selectors.EVENT_READ)
            while True:
                left = end-time.monotonic()
                need(left > 0 and sel.select(left))
                part = os.read(p.stdout.fileno(), min(8192, 131073-len(result)))
                if not part: break
                result.extend(part)
                need(len(result) <= 131072)
        need(p.wait(timeout=max(.001,end-time.monotonic())) == 0)
        return bytes(result)
    finally:
        if p.poll() is None:
            try: os.killpg(p.pid, signal.SIGKILL)
            except ProcessLookupError: pass
        p.wait(timeout=1)
        p.stdout.close()
def read(path, cap):
    clock()
    with open(path, 'rb') as f: raw = f.read(cap+1)
    need(len(raw) <= cap)
    return raw.decode('ascii')
def selected():
    rows = [decode(line) for line in command(['ps','--no-trunc','--format',LIST]).splitlines()]
    need(len(rows) <= 256)
    selected = {}
    for row in rows:
        need(set(row) == {'id','name','env','job','block'} and all(isinstance(v,str) and len(v) <= 256 for v in row.values()))
        named = row['name'].startswith(ENV+'-connect-')
        labelled = (row['env'],row['job']) == (ENV,'connect')
        if not named and not labelled: continue
        need(named and labelled and row['block'] in BLOCKS and row['name'].startswith(ENV+'-connect-'+row['block']+'-') and re.fullmatch('[a-f0-9]{64}',row['id']) and row['id'] not in selected)
        selected[row['id']] = row
    need(len(selected) <= 64)
    return selected
def inspect(selected):
    if not selected: return {}
    rows = [decode(line) for line in command(['inspect','--type','container','--format',DETAIL]+sorted(selected)).splitlines()]
    need(len(rows) == len(selected))
    states = {}
    for row in rows:
        need(set(row) == {'id','name','pid','running','started','path','env','job','block'} and row['id'] in selected and row['id'] not in states)
        old = selected[row['id']]
        need(row['name'] == '/'+old['name'] and all(row[k] == old[k] for k in ('env','job','block')) and row['running'] is True and type(row['pid']) is int and 0 < row['pid'] <= 4194304 and row['path'] == '/usr/local/sbin/bringyour-connect')
        states[row['id']] = row
    return states
def boot():
    identity = read('/proc/sys/kernel/random/boot_id',128).strip()
    need(re.fullmatch('[a-f0-9-]{36}',identity))
    times = [int(line.split()[1]) for line in read('/proc/stat',131072).splitlines() if line.startswith('btime ')]
    need(len(times) == 1 and times[0] > 0)
    return identity,times[0]
def token(row,btime):
    pid = row['pid']
    base = '/proc/'+str(pid)
    raw = read(base+'/stat',16384)
    prefix,sep,suffix = raw.rpartition(')')
    fields = suffix.split()
    need(sep and prefix.startswith(str(pid)+' (') and len(fields) >= 22 and fields[0] not in ('Z','X','x') and re.fullmatch('[0-9]+',fields[19]))
    ticks = int(fields[19]); need(ticks > 0)
    own = os.readlink('/proc/self/ns/time')
    target = os.readlink(base+'/ns/time')
    need(os.readlink('/proc/self/ns/time_for_children') == own and os.readlink(base+'/ns/time_for_children') == target)
    def offsets(path):
        rows = [line.split() for line in read(path,256).splitlines()]
        need(len(rows) == 2 and {r[0] for r in rows} == {'monotonic','boottime'} and all(len(r) == 3 and re.fullmatch('-?[0-9]{1,12}',r[1]) and re.fullmatch('[0-9]{1,9}',r[2]) for r in rows))
        return tuple((r[0],int(r[1]),int(r[2])) for r in rows)
    own_offsets, target_offsets = offsets('/proc/self/timens_offsets'), offsets(base+'/timens_offsets')
    need(own == target or (all(sec == ns == 0 for _,sec,ns in own_offsets) and all(sec == ns == 0 for _,sec,ns in target_offsets)))
    executable, declared = os.stat(base+'/exe'),os.stat(base+'/root/usr/local/sbin/bringyour-connect')
    need((executable.st_dev,executable.st_ino) == (declared.st_dev,declared.st_ino))
    return ticks, own, target, own_offsets, target_offsets, executable.st_dev, executable.st_ino
result = {'schema':1,'host':HOST,'environment':ENV,'started':STARTED,'completed':STARTED,'complete':False,'rows':[]}
try:
    need(os.uname().nodename.split('.')[0] == HOST.split('.')[0] and 0 < len(BLOCKS) <= 64)
    first_boot = boot()
    before = selected(); states = inspect(before)
    tokens = {cid:token(row,first_boot[1]) for cid,row in states.items()}
    rows = []
    for cid,row in sorted(states.items()):
        need(tokens[cid] == token(row,first_boot[1]))
        rows.append({'block':row['block'],'start':float(first_boot[1])+float(tokens[cid][0])/100})
    after = selected(); terminal = inspect(after)
    need(before == after and states == terminal and first_boot == boot())
    clock()
    result.update(complete=True,rows=rows)
except Exception:
    pass
result['completed'] = time.time()
print(json.dumps(result,allow_nan=False,separators=(',',':')))
`

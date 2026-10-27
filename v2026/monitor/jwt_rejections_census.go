// Independently enumerate current Warp process generations for JWT coverage.
// Read only allowlisted Docker metadata and kernel start times, never argv/env.
package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"
)

const jwtRejectionsCensusTimeout = 15 * time.Second

// One source host's current generations remain transient and identity-free in
// findings. Error causes retain typed transport/admission provenance privately.
type jwtRejectionsCensus struct {
	starts   map[jwtRejectionsSlot][]float64
	complete bool
	err      error
}

// Four joined workers bound SSH concurrency inside the shared runner's own
// admission. Cancellation stops scheduling, and every admitted worker returns.
func readJwtRejectionsCensus(ctx context.Context, env *probeEnv, scope jwtRejectionsScope, now time.Time) jwtRejectionsCensus {
	result := jwtRejectionsCensus{starts: map[jwtRejectionsSlot][]float64{}, complete: true}
	if len(scope.hosts) > 16 {
		result.complete = false
		return result
	}
	jobs := make(chan *host, len(scope.hosts))
	for _, name := range scope.hosts {
		var target *host
		for _, h := range env.cfg.hosts {
			if h.name == name && !h.disabled {
				target = h
				break
			}
		}
		if target == nil {
			result.complete = false
			continue
		}
		jobs <- target
	}
	close(jobs)
	var stateLock sync.Mutex
	var workers sync.WaitGroup
	sourceErrors := map[string]error{}
	for range min(4, len(scope.hosts)) {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for target := range jobs {
				if ctx.Err() != nil {
					return
				}
				command := "timeout 14s python3 -c " + shellSingleQuote(jwtRejectionsCensusProgram(env.cfg.env, scope.services))
				out, err := env.runner.sshTimeout(ctx, target, command, "", jwtRejectionsCensusTimeout)
				starts, valid := parseJwtRejectionsCensus(out, target.name, now, scope)
				stateLock.Lock()
				if err != nil || !valid {
					result.complete = false
					if err != nil {
						sourceErrors[target.name] = err
					}
				} else {
					for slot, values := range starts {
						result.starts[slot] = append(result.starts[slot], values...)
					}
				}
				stateLock.Unlock()
			}
		}()
	}
	workers.Wait()
	for _, name := range scope.hosts {
		if err := sourceErrors[name]; err != nil {
			result.err = errors.Join(result.err, err)
		}
	}
	if ctx.Err() != nil {
		result.complete = false
	}
	return result
}

// Docker returns only identity/role/PID fields through a fixed formatter. The
// full container config and process environment never enter the reader.
// Stable beginning/end container sets fence turnover during this observation.
func jwtRejectionsCensusProgram(environment string, services []string) string {
	envJson, _ := json.Marshal(environment)
	servicesJson, _ := json.Marshal(services)
	return fmt.Sprintf(`import json,math,os,re,signal,socket,subprocess,sys,time
signal.signal(signal.SIGALRM,lambda *_: (_ for _ in ()).throw(TimeoutError()))
signal.alarm(13)
environment=%s
services=%s
def read_file(path,limit):
 with open(path,'rb') as source: raw=source.read(limit+1)
 if len(raw)>limit: raise ValueError()
 return raw.decode()
def command(argv,limit):
 child=subprocess.Popen(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL)
 try:
  raw=child.stdout.read(limit+1)
  if len(raw)>limit or child.wait(timeout=2)!=0: raise ValueError()
  return raw.decode()
 finally:
  if child.poll() is None: child.kill(); child.wait()
def ids():
 rows=command(['docker','ps','--no-trunc','--format','{{.ID}}'],32768).splitlines()
 if len(rows)>256 or len(rows)!=len(set(rows)) or any(not re.fullmatch('[a-f0-9]{64}',row) for row in rows): raise ValueError()
 return sorted(rows)
try:
 initial=ids()
 boot=read_file('/proc/sys/kernel/random/boot_id',128).strip()
 btimes=[int(line.split()[1]) for line in read_file('/proc/stat',1048576).splitlines() if line.startswith('btime ')]
 if len(btimes)!=1: raise ValueError()
 ticks=os.sysconf('SC_CLK_TCK')
 if not isinstance(ticks,int) or ticks<=0: raise ValueError()
 formatter='{"id":{{json .Id}},"name":{{json .Name}},"pid":{{.State.Pid}},"running":{{.State.Running}},"env":{{json (index .Config.Labels "warp.env")}},"service":{{json (index .Config.Labels "warp.service")}},"block":{{json (index .Config.Labels "warp.block")}}}'
 rows=[] if not initial else command(['docker','inspect','--format',formatter]+initial,131072).splitlines()
 if len(rows)!=len(initial): raise ValueError()
 seen=set(); processes=[]; proc_starts=[]
 for raw in rows:
  row=json.loads(raw)
  if row['id'] not in initial or row['id'] in seen or row['running'] is not True: raise ValueError()
  seen.add(row['id'])
  name=row.get('name')
  if not isinstance(name,str) or not name: raise ValueError()
  name=name.lstrip('/')
  named_services=[service for service in services if name.startswith(environment+'-'+service+'-')]
  # A scoped name with absent or contradictory labels cannot be unrelated.
  if named_services and (len(named_services)!=1 or row.get('env')!=environment or row.get('service')!=named_services[0]): raise ValueError()
  if row.get('env')!=environment: continue
  service,block=row.get('service'),row.get('block')
  if not isinstance(service,str) or not isinstance(block,str) or not service or not block: raise ValueError()
  # Warp creates the name and these labels together. Only a complete,
  # consistent out-of-scope identity may be skipped before reading its PID.
  if not name.startswith(environment+'-'+service+'-'+block+'-'): raise ValueError()
  if service not in services: continue
  pid=row['pid']
  if not isinstance(pid,int) or pid<=0: raise ValueError()
  executable=os.readlink('/proc/'+str(pid)+'/exe').removesuffix(' (deleted)')
  # An unsupported root process is a visibility gap, never an assumed app.
  if os.path.basename(executable)!='bringyour-'+service: raise ValueError()
  stat=read_file('/proc/'+str(pid)+'/stat',8192)
  fields=stat[stat.rfind(')')+2:].split()
  start_ticks=int(fields[19]); start=btimes[0]+start_ticks/ticks
  if start_ticks<=0 or not math.isfinite(start): raise ValueError()
  processes.append({'service':service,'block':block,'start':start})
  proc_starts.append((pid,start_ticks))
 if initial!=ids() or boot!=read_file('/proc/sys/kernel/random/boot_id',128).strip(): raise ValueError()
 for pid,previous in proc_starts:
  stat=read_file('/proc/'+str(pid)+'/stat',8192)
  if int(stat[stat.rfind(')')+2:].split()[19])!=previous: raise ValueError()
 print(json.dumps({'schema':1,'host':socket.gethostname().split('.')[0],'at':time.time(),'processes':processes},separators=(',',':')))
except Exception:
 raise SystemExit(1)
`, envJson, servicesJson)
}

// Strictly bound the census schema and join it only to permitted active slots.
// Any unexpected deployed slot, duplicate start or overfull generation set
// invalidates that host; no raw name or metadata reaches an error string.
func parseJwtRejectionsCensus(raw, hostName string, now time.Time, scope jwtRejectionsScope) (map[jwtRejectionsSlot][]float64, bool) {
	var source struct {
		Schema    int     `json:"schema"`
		Host      string  `json:"host"`
		At        float64 `json:"at"`
		Processes []struct {
			Service string  `json:"service"`
			Block   string  `json:"block"`
			Start   float64 `json:"start"`
		} `json:"processes"`
	}
	if len(raw) > 65536 {
		return nil, false
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&source) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	shortName, _, _ := strings.Cut(hostName, ".")
	age := float64(now.UnixNano())/1e9 - source.At
	if source.Schema != 1 || source.Host != shortName || math.IsNaN(source.At) || math.IsInf(source.At, 0) || age < -30 || age > 30 || len(source.Processes) > 256 {
		return nil, false
	}
	starts := map[jwtRejectionsSlot][]float64{}
	for _, process := range source.Processes {
		slot := jwtRejectionsSlot{service: process.Service, host: hostName, block: process.Block}
		if !scope.slots[slot] || math.IsNaN(process.Start) || math.IsInf(process.Start, 0) || process.Start <= 0 || process.Start > source.At || len(starts[slot]) >= jwtRejectionsMaxGenerations {
			return nil, false
		}
		for _, existing := range starts[slot] {
			if existing == process.Start {
				return nil, false
			}
		}
		starts[slot] = append(starts[slot], process.Start)
	}
	return starts, true
}

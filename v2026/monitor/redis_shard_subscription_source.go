// Bounded native producers for the canonical Proxy sharded-channel census.
// Raw Docker, HTTP and Redis replies never leave the remote producer. The
// versioned output contains only request indexes, counts, clocks and hashes.
package monitor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	redisShardSourceLimit  = 64 * 1024
	redisShardSourceBudget = 25 * time.Second
)

// Endpoints originate exclusively in the configured Redis host/port inventory.
// Address is the announced LAN address; native reads always use loopback on Host.
type redisShardEndpoint struct {
	Address string `json:"address"`
	Port    int    `json:"port"`
	host    *host
}

// Channels are generated locally with model.ProxyClientChannel after the
// native allocation has proved the exact RequireHost and RequireBlock values.
type redisShardChannel struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Slot  int    `json:"slot"`
	Owner int    `json:"owner"`
}

// The bounded request carries no credentials, arbitrary endpoint or Redis key.
type redisShardRequest struct {
	Phase       string               `json:"phase"`
	Nonce       string               `json:"nonce"`
	Environment string               `json:"environment"`
	Host        string               `json:"host"`
	HostNames   []string             `json:"host_names"`
	Blocks      []string             `json:"blocks"`
	Endpoints   []redisShardEndpoint `json:"endpoints"`
	Endpoint    int                  `json:"endpoint"`
	Channels    []redisShardChannel  `json:"channels"`
	Map         string               `json:"map"`
}

// The fixed row grammar avoids accepting duplicate JSON fields, extra rows,
// floating-point counters or an untrusted source label as a public identity.
type redisShardRow struct {
	index      int
	state      string
	hostIndex  int
	generation string
	slot       int
	owner      int
	count      int64
}

// Clock bounds describe the actual native observation, not evaluation time.
type redisShardFrame struct {
	started  time.Time
	finished time.Time
	mapping  string
	complete bool
	rows     []redisShardRow
}

// A native map cannot attest the configured host/block roster or exclusions.
// The existing seam compares the whole immutable effective settings generation.
var errRedisShardSettings = errors.New("sharded census settings generation unavailable")

// Keep parent cancellation authoritative and never expose loader errors.
func redisShardCurrentSettings(ctx context.Context, env *probeEnv) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if env.cfg.routerGenerationCheck == nil {
		return errRedisShardSettings
	}
	current, err := env.cfg.routerGenerationCheck(ctx)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if err != nil || !current {
		return errRedisShardSettings
	}
	return nil
}

// Each call is a single bounded attempt. Failed transport stdout is discarded;
// cancellation is returned intact before any further source contact.
func collectRedisShardFrame(ctx context.Context, env *probeEnv, target *host, request redisShardRequest) (redisShardFrame, error) {
	if err := redisShardCurrentSettings(ctx, env); err != nil {
		return redisShardFrame{}, err
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return redisShardFrame{}, fmt.Errorf("sharded census nonce unavailable")
	}
	request.Nonce = hex.EncodeToString(nonce)
	if request.HostNames == nil {
		request.HostNames = []string{}
	}
	if request.Blocks == nil {
		request.Blocks = []string{}
	}
	if request.Endpoints == nil {
		request.Endpoints = []redisShardEndpoint{}
	}
	if request.Channels == nil {
		request.Channels = []redisShardChannel{}
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > 96*1024 {
		return redisShardFrame{}, fmt.Errorf("sharded census request invalid")
	}
	command := "# monitor-redis-shard-subscriptions-v1\nexec timeout -k 2s 23s python3 -c " +
		shellSingleQuote(redisShardSourceProgram) + " " + shellSingleQuote(string(encoded))
	sourceCtx, cancel := context.WithTimeout(ctx, redisShardSourceBudget)
	defer cancel()
	started := env.now()
	out, err := env.runner.sshTimeout(sourceCtx, target, command, "", redisShardSourceBudget)
	finished := env.now()
	if generationErr := redisShardCurrentSettings(ctx, env); generationErr != nil {
		return redisShardFrame{}, generationErr
	}
	if err != nil || sourceCtx.Err() != nil {
		return redisShardFrame{}, fmt.Errorf("sharded census source unavailable")
	}
	return parseRedisShardFrame(out, request, started, finished)
}

// Require canonical unsigned decimal values, including a finite bound.
func redisShardUnsigned(value string, maximum int64) (int64, bool) {
	n, err := strconv.ParseInt(value, 10, 64)
	return n, err == nil && n >= 0 && n <= maximum && strconv.FormatInt(n, 10) == value
}

// Only locally generated lowercase SHA-256 digests are accepted as joins.
func redisShardDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

// Parse the complete envelope and every requested index before using any row.
func parseRedisShardFrame(out string, request redisShardRequest, localStart, localFinish time.Time) (redisShardFrame, error) {
	invalid := func() (redisShardFrame, error) {
		return redisShardFrame{}, fmt.Errorf("sharded census response invalid")
	}
	if (request.Phase != "allocations" && request.Phase != "resolve" && request.Phase != "counts") || len(out) == 0 || len(out) > redisShardSourceLimit || !strings.HasSuffix(out, "\n") || strings.Contains(out, "\r") {
		return invalid()
	}
	lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	if len(lines) < 2 {
		return invalid()
	}
	header := strings.Split(lines[0], " ")
	if len(header) != 9 || header[0] != "rsc1" || header[1] != request.Nonce || header[2] != request.Phase || (header[7] != "0" && header[7] != "1") {
		return invalid()
	}
	startNs, startOk := redisShardUnsigned(header[3], 1<<63-1)
	finishNs, finishOk := redisShardUnsigned(header[4], 1<<63-1)
	elapsedNs, elapsedOk := redisShardUnsigned(header[5], int64(21*time.Second))
	rows, rowsOk := redisShardUnsigned(header[6], 256)
	frame := redisShardFrame{
		started: time.Unix(0, startNs).UTC(), finished: time.Unix(0, finishNs).UTC(),
		mapping: header[8], complete: header[7] == "1",
	}
	if !startOk || !finishOk || !elapsedOk || !rowsOk || startNs == 0 || finishNs < startNs ||
		localFinish.Before(localStart) || localFinish.Sub(localStart) > redisShardSourceBudget ||
		frame.started.Before(localStart.Add(-5*time.Second)) || frame.finished.After(localFinish.Add(5*time.Second)) ||
		frame.finished.Sub(frame.started) > 21*time.Second ||
		redisShardAbsDuration(frame.finished.Sub(frame.started)-time.Duration(elapsedNs)) > time.Second ||
		len(lines) != int(rows)+2 || lines[len(lines)-1] != "end "+header[6] {
		return invalid()
	}
	if request.Phase == "allocations" {
		if frame.mapping != "-" || int(rows) != len(request.Blocks) {
			return invalid()
		}
	} else if !frame.complete || !redisShardDigest(frame.mapping) ||
		(request.Map != "" && request.Map != frame.mapping) || int(rows) != len(request.Channels) {
		return invalid()
	}
	wanted := map[int]redisShardChannel{}
	for _, channel := range request.Channels {
		wanted[channel.Index] = channel
	}
	seen := map[int]bool{}
	for _, line := range lines[1 : len(lines)-1] {
		fields := strings.Split(line, " ")
		if len(fields) < 2 {
			return invalid()
		}
		index, indexOk := redisShardUnsigned(fields[1], 255)
		if !indexOk || seen[int(index)] {
			return invalid()
		}
		seen[int(index)] = true
		row := redisShardRow{index: int(index), count: -1, hostIndex: -1}
		switch request.Phase {
		case "allocations":
			if len(fields) != 5 || fields[0] != "a" || int(index) >= len(request.Blocks) {
				return invalid()
			}
			row.state, row.generation = fields[2], fields[4]
			if row.state == "active" {
				hostIndex, hostOk := redisShardUnsigned(fields[3], int64(len(request.HostNames)-1))
				if !hostOk || !redisShardDigest(row.generation) {
					return invalid()
				}
				row.hostIndex = int(hostIndex)
			} else if (row.state != "inactive" && row.state != "unknown") || fields[3] != "-" || fields[4] != "-" {
				return invalid()
			}
		case "resolve", "counts":
			channel, present := wanted[int(index)]
			if !present || (request.Phase == "resolve" && (len(fields) != 4 || fields[0] != "r")) ||
				(request.Phase == "counts" && (len(fields) != 5 || fields[0] != "c")) {
				return invalid()
			}
			slot, slotOk := redisShardUnsigned(fields[2], 16383)
			owner, ownerOk := redisShardUnsigned(fields[3], int64(len(request.Endpoints)-1))
			if !slotOk || !ownerOk {
				return invalid()
			}
			row.slot, row.owner = int(slot), int(owner)
			if request.Phase == "counts" {
				if row.slot != channel.Slot || row.owner != channel.Owner || row.owner != request.Endpoint {
					return invalid()
				}
				if fields[4] != "unknown" {
					count, countOk := redisShardUnsigned(fields[4], 1<<63-1)
					if !countOk {
						return invalid()
					}
					row.count = count
				}
			}
		default:
			return invalid()
		}
		frame.rows = append(frame.rows, row)
	}
	return frame, nil
}

// Used for source clock consistency, never for a health threshold.
func redisShardAbsDuration(value time.Duration) time.Duration {
	if value < 0 {
		return -value
	}
	return value
}

// Each native command has bounded stdout/stderr, its actual exit is checked,
// and its new process group is joined on cap, timeout or parent cancellation.
// No shell evaluates native argv; no redirects, retries, PUBLISH or mutations.
const redisShardSourceProgram = `import binascii, hashlib, ipaddress, json, os, re, selectors, signal, subprocess, sys, time

class Unavailable(Exception):
    pass
class Cancelled(BaseException):
    pass

started_ns = time.time_ns()
started_mono = time.monotonic_ns()
deadline = time.monotonic() + 20.0

def cancel(signum, frame):
    raise Cancelled()
for signum in (signal.SIGTERM, signal.SIGINT, signal.SIGHUP):
    signal.signal(signum, cancel)

def require(value):
    if not value:
        raise Unavailable()

def digest(value):
    return hashlib.sha256(json.dumps(value, sort_keys=True, separators=(',', ':')).encode()).hexdigest()

def integer(value, maximum=(1 << 63)-1):
    require(isinstance(value, str) and re.fullmatch(r'0|[1-9][0-9]*', value) is not None)
    number = int(value)
    require(number <= maximum)
    return number

def capture(argv, limit=262144):
    require(time.monotonic() < deadline and sum(len(s) for s in argv) < 32768)
    child = None
    selector = selectors.DefaultSelector()
    output = bytearray()
    stderr_bytes = 0
    try:
        # Delay cancellation until the new session has an owned child handle.
        # This producer is single-threaded; restore the child's original mask
        # before exec so native commands do not inherit blocked cancellation.
        launch_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM, signal.SIGINT, signal.SIGHUP})
        try:
            child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True, close_fds=True,
                preexec_fn=lambda: signal.pthread_sigmask(signal.SIG_SETMASK, launch_mask))
        finally:
            signal.pthread_sigmask(signal.SIG_SETMASK, launch_mask)
        for stream in (child.stdout, child.stderr):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ)
        child_deadline = min(deadline, time.monotonic()+3.0)
        while selector.get_map():
            require(time.monotonic() < child_deadline)
            for key, events in selector.select(min(0.05, max(0, child_deadline-time.monotonic()))):
                chunk = os.read(key.fileobj.fileno(), 8192)
                if not chunk:
                    selector.unregister(key.fileobj)
                    continue
                if key.fileobj is child.stdout:
                    require(len(output)+len(chunk) <= limit)
                    output.extend(chunk)
                else:
                    stderr_bytes += len(chunk)
                    require(stderr_bytes <= 8192)
        # WNOWAIT retains the owned leader's PID until group cleanup, so a
        # later killpg cannot accidentally target a recycled process number.
        while True:
            outcome = os.waitid(os.P_PID, child.pid, os.WEXITED | os.WNOHANG | os.WNOWAIT)
            if outcome is not None:
                break
            require(time.monotonic() < child_deadline)
            time.sleep(min(0.01, max(0, child_deadline-time.monotonic())))
        require(outcome.si_code == os.CLD_EXITED and outcome.si_status == 0)
        return bytes(output).decode('utf-8', 'strict')
    finally:
        selector.close()
        if child is not None:
            blocked = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGTERM, signal.SIGINT, signal.SIGHUP})
            try:
                for signum in (signal.SIGTERM, signal.SIGKILL):
                    try:
                        os.killpg(child.pid, signum)
                    except ProcessLookupError:
                        pass
                child.wait(timeout=1.0)
                child.stdout.close()
                child.stderr.close()
            finally:
                signal.pthread_sigmask(signal.SIG_SETMASK, blocked)

def exact_lines(value):
    require(value.endswith('\n') and '\r' not in value)
    return value[:-1].split('\n')

def redis(*args):
    return capture(['redis-cli', '-2', '--raw', '-h', '127.0.0.1', '-p', str(request['endpoints'][request['endpoint']]['port'])] + list(args))

def info(section):
    values = {}
    raw = redis('INFO', section).replace('\r\n', '\n')
    for line in exact_lines(raw):
        if not line or line.startswith('#'):
            continue
        require(':' in line)
        key, value = line.split(':', 1)
        require(re.fullmatch(r'[A-Za-z0-9_]+', key) is not None and key not in values)
        values[key] = value
    return values

def topology():
    first_run = info('server')['run_id']
    require(re.fullmatch(r'[0-9a-f]{40}', first_run) is not None)
    cluster = {}
    for line in exact_lines(redis('CLUSTER', 'INFO').replace('\r\n', '\n').rstrip('\n')+'\n'):
        require(':' in line)
        key, value = line.split(':', 1)
        require(key not in cluster)
        cluster[key] = value
    require(cluster['cluster_state'] == 'ok' and cluster['cluster_slots_assigned'] == '16384' and cluster['cluster_slots_ok'] == '16384' and cluster['cluster_slots_pfail'] == '0' and cluster['cluster_slots_fail'] == '0')
    epoch = integer(cluster['cluster_current_epoch'])
    endpoint_indexes = {(str(ipaddress.ip_address(e['address'])), e['port']): i for i, e in enumerate(request['endpoints'])}
    require(len(endpoint_indexes) == len(request['endpoints']) and integer(cluster['cluster_known_nodes'], 256) == len(endpoint_indexes))
    owners = [-1]*16384
    nodes = []
    node_ids = set()
    endpoints_seen = set()
    self_count = 0
    for line in exact_lines(redis('CLUSTER', 'NODES').rstrip('\n')+'\n'):
        fields = line.split(' ')
        require(8 <= len(fields) <= 16392 and all(fields))
        node_id, address, flags, master, ping, pong, node_epoch, link = fields[:8]
        require(re.fullmatch(r'[0-9a-f]{40}', node_id) is not None and node_id not in node_ids)
        node_ids.add(node_id)
        announced = address.split(',', 1)[0]
        endpoint, bus = announced.split('@')
        ip, port = endpoint.rsplit(':', 1)
        integer(bus, 65535)
        endpoint_index = endpoint_indexes[(str(ipaddress.ip_address(ip.strip('[]'))), integer(port, 65535))]
        require(endpoint_index not in endpoints_seen)
        endpoints_seen.add(endpoint_index)
        flags = flags.split(',')
        require(len(flags) == len(set(flags)) and set(flags) <= {'myself','master','slave'} and ('master' in flags) != ('slave' in flags) and link == 'connected')
        if 'myself' in flags:
            self_count += 1
            require(endpoint_index == request['endpoint'])
        integer(ping); integer(pong)
        node_epoch = integer(node_epoch)
        require(node_epoch <= epoch)
        slots = []
        if 'slave' in flags:
            require(re.fullmatch(r'[0-9a-f]{40}', master) is not None and len(fields) == 8)
        else:
            require(master == '-')
            for token in fields[8:]:
                parts = token.split('-')
                require(1 <= len(parts) <= 2)
                low = integer(parts[0], 16383)
                high = low if len(parts) == 1 else integer(parts[1], 16383)
                require(low <= high)
                for slot in range(low, high+1):
                    require(owners[slot] == -1)
                    owners[slot] = endpoint_index
                slots.append([low, high])
        nodes.append([node_id, endpoint_index, sorted(set(flags)-{'myself'}), master, node_epoch, sorted(slots)])
    require(self_count == 1 and endpoints_seen == set(range(len(endpoint_indexes))) and -1 not in owners)
    require(all(n[3] == '-' or n[3] in node_ids for n in nodes))
    require(info('server')['run_id'] == first_run)
    return digest([epoch, sorted(nodes)]), first_run, owners

def keyslot(channel):
    raw = exact_lines(redis('CLUSTER', 'KEYSLOT', channel))
    require(len(raw) == 1)
    slot = integer(raw[0], 16383)
    require(slot == binascii.crc_hqx(channel.encode(), 0) % 16384)
    return slot

inspect_format = '{{json .Id}}\n{{json .Name}}\n{{json .State.Running}}\n{{json .State.Restarting}}\n{{json .State.Dead}}\n{{json .State.Pid}}\n{{json .State.StartedAt}}\n{{json .RestartCount}}\n{{range .Config.Env}}{{$p := split . "="}}{{if or (eq (index $p 0) "WARP_ENV") (eq (index $p 0) "WARP_SERVICE") (eq (index $p 0) "WARP_HOST") (eq (index $p 0) "WARP_BLOCK") (eq (index $p 0) "WARP_PORTS") (eq (index $p 0) "WARP_VERSION")}}{{println .}}{{end}}{{end}}'

def inspect(container_id):
    lines = exact_lines(capture(['docker','inspect','--format',inspect_format,container_id], 16384))
    require(len(lines) >= 8)
    metadata = [json.loads(value) for value in lines[:8]]
    require(metadata[0] == container_id and metadata[2] is True and metadata[3] is False and metadata[4] is False and type(metadata[5]) is int and metadata[5] > 0 and type(metadata[7]) is int and metadata[7] >= 0)
    require(isinstance(metadata[6], str) and re.fullmatch(r'[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9:.]+Z', metadata[6]) is not None)
    values = {}
    for line in lines[8:]:
        if line == '':
            continue
        key, value = line.split('=', 1)
        require(key in {'WARP_ENV','WARP_SERVICE','WARP_HOST','WARP_BLOCK','WARP_PORTS','WARP_VERSION'} and key not in values)
        values[key] = value
    require(set(values) == {'WARP_ENV','WARP_SERVICE','WARP_HOST','WARP_BLOCK','WARP_PORTS','WARP_VERSION'})
    return metadata, values

def allocations():
    rows = {i: ['a',str(i),'inactive','-','-'] for i in range(len(request['blocks']))}
    seen = set()
    complete = True
    unidentified = False
    prefix = request['environment']+'-proxy-'
    raw = capture(['docker','ps','--no-trunc','--format','{{.ID}} {{.Names}}'], 65536)
    lines = [] if raw == '' else exact_lines(raw)
    require(len(lines) <= 256)
    container_ids = set()
    for line in lines:
        parts = line.split(' ')
        require(len(parts) == 2 and re.fullmatch(r'[0-9a-f]{64}', parts[0]) is not None and parts[0] not in container_ids)
        container_ids.add(parts[0])
        if not parts[1].startswith(prefix):
            continue
        block_index = None
        try:
            metadata, values = inspect(parts[0])
            require(metadata[1] == '/'+parts[1] and values['WARP_ENV'] == request['environment'] and values['WARP_SERVICE'] == 'proxy')
            block_index = request['blocks'].index(values['WARP_BLOCK'])
            require(block_index not in seen)
            seen.add(block_index)
            host_index = request['host_names'].index(values['WARP_HOST'])
            ports = {}
            for pair in values['WARP_PORTS'].split(','):
                source, target = pair.split(':')
                source, target = integer(source,65535), integer(target,65535)
                require(source > 0 and target > 0 and source not in ports)
                ports[source] = target
            require(80 in ports)
            status_raw = capture(['curl','--silent','--show-error','--fail','--max-time','2','--noproxy','*','--proto','=http','http://127.0.0.1:'+str(ports[80])+'/status'], 8192)
            def unique_object(pairs):
                values = {}
                for key, value in pairs:
                    require(key not in values)
                    values[key] = value
                return values
            status = json.loads(status_raw, object_pairs_hook=unique_object)
            require(status['status'] == 'ok' and status['host'] == values['WARP_HOST'] and status['block'] == values['WARP_BLOCK'] and status['service'] == 'proxy' and status['version'] == values['WARP_VERSION'])
            require(inspect(parts[0]) == (metadata, values))
            rows[block_index] = ['a',str(block_index),'active',str(host_index),digest([metadata,values])]
        except (Unavailable, KeyError, ValueError, UnicodeError, OSError, subprocess.SubprocessError):
            complete = False
            if block_index is not None:
                rows[block_index] = ['a',str(block_index),'unknown','-','-']
            else:
                unidentified = True
    if unidentified:
        rows = {i: ['a',str(i),'unknown','-','-'] for i in rows}
    return '-', complete, [rows[i] for i in sorted(rows)]

def census():
    mapping, run_id, owners = topology()
    require(not request['map'] or mapping == request['map'])
    rows = []
    for channel in request['channels']:
        if request['phase'] == 'resolve':
            slot = keyslot(channel['name'])
            rows.append(['r', str(channel['index']), str(slot), str(owners[slot])])
        else:
            require(channel['owner'] == request['endpoint'] and owners[channel['slot']] == request['endpoint'])
            count = 'unknown'
            try:
                require(keyslot(channel['name']) == channel['slot'])
                values = exact_lines(redis('PUBSUB','SHARDNUMSUB',channel['name']))
                require(len(values) == 2 and values[0] == channel['name'])
                count = str(integer(values[1]))
            except (Unavailable, KeyError, ValueError, UnicodeError, OSError, subprocess.SubprocessError):
                pass
            rows.append(['c',str(channel['index']),str(channel['slot']),str(channel['owner']),count])
    final_map, final_run, final_owners = topology()
    require(mapping == final_map and run_id == final_run and owners == final_owners)
    return mapping, True, rows

try:
    require(len(sys.argv) == 2 and len(sys.argv[1]) <= 98304)
    request = json.loads(sys.argv[1])
    require(request['phase'] in {'allocations','resolve','counts'} and re.fullmatch(r'[0-9a-f]{32}',request['nonce']) is not None)
    require(0 < len(request['blocks']) <= 64 and len(set(request['blocks'])) == len(request['blocks']) and len(request['channels']) <= 256 and len(request['endpoints']) <= 256)
    require(all(re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.-]{0,127}', value) is not None for value in request['blocks']+request['host_names']+[request['environment'],request['host']]))
    require(os.uname().nodename.split('.',1)[0] == request['host'].split('.',1)[0])
    require(len({c['index'] for c in request['channels']}) == len(request['channels']))
    for channel in request['channels']:
        require(type(channel['index']) is int and 0 <= channel['index'] < 256 and re.fullmatch(r'proxy_client_[A-Za-z0-9_.-]+',channel['name']) is not None and len(channel['name']) <= 280)
    if request['phase'] == 'allocations':
        mapping, complete, rows = allocations()
    else:
        require(0 <= request['endpoint'] < len(request['endpoints']))
        mapping, complete, rows = census()
    elapsed_ns = time.monotonic_ns()-started_mono
    require(elapsed_ns <= 21000000000)
    finished_ns = time.time_ns()
    lines = ['rsc1 '+request['nonce']+' '+request['phase']+' '+str(started_ns)+' '+str(finished_ns)+' '+str(elapsed_ns)+' '+str(len(rows))+' '+str(int(complete))+' '+mapping]
    lines += [' '.join(row) for row in rows]
    lines += ['end '+str(len(rows))]
    result = '\n'.join(lines)+'\n'
    require(len(result) <= 65536)
    sys.stdout.write(result)
except Cancelled:
    sys.exit(130)
except BaseException:
    # No native diagnostic, environment value, channel or node identity escapes.
    sys.stderr.write('sharded census unavailable\n')
    sys.exit(1)
`

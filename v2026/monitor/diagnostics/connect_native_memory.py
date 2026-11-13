"""Bounded read-only Connect native process projection.

Invoked only by the separately admitted inventory/hostname SSH wrapper. This
module has no network endpoint, logs, process-memory/profile read, or mutation
API. Every live generation is retained; incomplete evidence remains explicit.
"""
import datetime
import hashlib
import json
import math
import os
import re
import selectors
import stat
import struct
import subprocess
import time

HOSTS = tuple('by-us-fmt-5-edge-' + str(n) for n in (0, 1, 3, 4))
BLOCKS = ('beta', 'g1', 'g2', 'g3', 'g4')
MAX_SECONDS = 30
MAX_CONTAINERS = 64
MAX_METADATA = 16 * 1024 * 1024
MAX_HASH_BYTES = 512 * 1024 * 1024
MAX_EXE_BYTES = 128 * 1024 * 1024
HEX = re.compile(r'[a-f0-9]{64}\Z')
IMAGE = re.compile(r'sha256:[a-f0-9]{64}\Z')
DOCKER = ['/usr/bin/docker', '-H', 'unix:///var/run/docker.sock']
LIST = '{"id":{{json .ID}},"name":{{json .Names}},"env":{{json (.Label "warp.env")}},"job":{{json (.Label "warp.service")}},"block":{{json (.Label "warp.block")}}}'
DETAIL = '{"id":{{json .Id}},"name":{{json .Name}},"image":{{json .Image}},"pid":{{json .State.Pid}},"running":{{json .State.Running}},"started":{{json .State.StartedAt}},"path":{{json .Path}},"env":{{json (index .Config.Labels "warp.env")}},"job":{{json (index .Config.Labels "warp.service")}},"block":{{json (index .Config.Labels "warp.block")}}}'
IMAGE_DETAIL = '{"id":{{json .Id}},"os":{{json .Os}},"architecture":{{json .Architecture}}}'


class Unavailable(Exception):
    pass


class HostProcessListBound(Unavailable):
    """Finite lower bounds from the entry already consumed at a list cutoff."""
    def __init__(self, limit, entries_observed, numeric_pids_seen):
        super().__init__('host-proc-list-bound')
        self.facts = {'limit': limit, 'entries_observed': entries_observed,
                      'numeric_pids_seen': numeric_pids_seen}


def require(condition, code):
    if not condition:
        raise Unavailable(code)


def decode(raw):
    def pairs(items):
        out = {}
        for key, value in items:
            require(key not in out, 'duplicate-field')
            out[key] = value
        return out
    return json.loads(raw, object_pairs_hook=pairs)


class Reader:
    def __init__(self):
        self.started = time.monotonic()
        self.metadata_bytes = 0
        self.hash_bytes = 0
        self.executables = {}

    def clock(self):
        require(time.monotonic() - self.started < MAX_SECONDS, 'total-time-bound')

    def command(self, arguments):
        self.clock()
        end = min(self.started + MAX_SECONDS, time.monotonic() + 4)
        p = subprocess.Popen(DOCKER + arguments, stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL,
                             env={'PATH': '/usr/bin:/bin', 'LANG': 'C'},
                             start_new_session=True)
        result = bytearray()
        try:
            with selectors.DefaultSelector() as sel:
                sel.register(p.stdout, selectors.EVENT_READ)
                while True:
                    left = end - time.monotonic()
                    require(left > 0 and sel.select(left), 'docker-command-time-bound')
                    part = os.read(p.stdout.fileno(), min(8192, 131073 - len(result)))
                    if not part:
                        break
                    result.extend(part)
                    require(len(result) <= 131072, 'docker-output-bound')
            require(p.wait(timeout=max(.001, end - time.monotonic())) == 0,
                    'docker-command-unavailable')
            return bytes(result)
        finally:
            if p.returncode is None:
                import signal
                try:
                    os.killpg(p.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            p.wait(timeout=1)
            p.stdout.close()

    def metadata(self, f, count):
        self.clock()
        require(0 <= count <= 1048576 and
                self.metadata_bytes + count <= MAX_METADATA, 'metadata-read-bound')
        data = f.read(count)
        self.metadata_bytes += len(data)
        require(len(data) == count, 'metadata-short-read')
        return data

    def text(self, path, cap=16384):
        self.clock()
        require(0 < cap <= 1048576, 'metadata-read-bound')
        with open(path, 'rb') as f:
            data = f.read(cap + 1)
        self.metadata_bytes += len(data)
        require(len(data) <= cap and self.metadata_bytes <= MAX_METADATA,
                'proc-output-bound')
        return data.decode('ascii')

    def executable(self, pid):
        path = '/proc/' + str(pid) + '/exe'
        self.clock()
        with open(path, 'rb') as f:
            info = os.fstat(f.fileno())
            identity = file_identity(info)
            require(stat.S_ISREG(info.st_mode) and 64 <= info.st_size <= MAX_EXE_BYTES,
                    'executable-bound')
            if identity not in self.executables:
                require(self.hash_bytes + info.st_size <= MAX_HASH_BYTES,
                        'total-executable-byte-bound')
                source = elf_buildinfo(self, f, info.st_size, 'connect')
                f.seek(0)
                digest = hashlib.sha256()
                left = info.st_size
                while left:
                    self.clock()
                    part = f.read(min(left, 1048576))
                    require(bool(part), 'executable-short-read')
                    digest.update(part)
                    self.hash_bytes += len(part)
                    left -= len(part)
                require(not f.read(1), 'executable-size-changed')
                self.executables[identity] = dict(source=source,
                    executable_sha256=digest.hexdigest(), executable_bytes=info.st_size)
            require(file_identity(os.fstat(f.fileno())) == identity and
                    file_identity(os.stat(path)) == identity, 'exe-changed')
        return self.executables[identity], identity


def file_identity(s):
    return (s.st_dev, s.st_ino, s.st_size, s.st_mtime_ns, s.st_ctime_ns)

def uvarint(data, pos):
    value = 0
    for shift in range(0, 70, 7):
        require(pos < len(data), 'buildinfo-varint')
        x = data[pos]
        pos += 1
        value |= (x & 127) << shift
        if x < 128:
            require(shift < 63 or x <= 1, 'buildinfo-varint')
            return value, pos
    raise Unavailable('buildinfo-varint')

def decode_buildinfo(data, expected_job):
    require(len(data) >= 32 and data[:14] == b'\xff Go buildinf:' and data[14] == 8 and data[15] & 2 and not data[15] & 1, 'buildinfo-format')
    n, pos = uvarint(data, 32)
    require(n <= 128 and pos + n <= len(data), 'buildinfo-go-version')
    version = data[pos:pos+n].decode('ascii')
    require(re.fullmatch(r'go[0-9A-Za-z._+-]{1,63}', version), 'buildinfo-go-version')
    n, pos = uvarint(data, pos+n)
    require(n <= 262144 and pos+n <= len(data), 'buildinfo-module-bound')
    module = data[pos:pos+n]
    if len(module) >= 32 and module[-17] == 10:
        module = module[16:-16]
    text = module.decode('utf-8')
    paths = re.findall(r'(?:^|\n)path\t([^\n]+)', text)
    revisions = re.findall(r'(?:^|\n)build\tvcs.revision=([0-9a-f]{40})(?=\n|$)', text)
    modified = re.findall(r'(?:^|\n)build\tvcs.modified=(true|false)(?=\n|$)', text)
    require(paths == ['github.com/urnetwork/server/cli/' + expected_job], 'service-main-path-unavailable')
    require(len(revisions) == 1 and len(modified) == 1, 'buildinfo-vcs-unavailable')
    return {'go_version': version, 'main_path': paths[0], 'revision': revisions[0], 'modified': modified[0] == 'true', 'buildinfo_sha256': hashlib.sha256(data).hexdigest()}

def elf_buildinfo(reader, f, size, job):
    f.seek(0)
    header = reader.metadata(f, 64)
    require(header[:6] == b'\x7fELF\x02\x01', 'elf-format')
    machine = struct.unpack_from('<H', header, 18)[0]
    require(machine in (62, 183), 'elf-architecture')
    offset = struct.unpack_from('<Q', header, 40)[0]
    width, count, names_index = struct.unpack_from('<HHH', header, 58)
    require(width == 64 and 0 < count <= 4096 and names_index < count and offset + width*count <= size, 'elf-section-bound')
    f.seek(offset)
    sections = reader.metadata(f, width*count)
    names_offset, names_size = struct.unpack_from('<QQ', sections, names_index*64+24)
    require(names_size <= 1048576 and names_offset + names_size <= size, 'elf-name-bound')
    f.seek(names_offset)
    names = reader.metadata(f, names_size)
    matches = []
    for i in range(count):
        name_offset = struct.unpack_from('<I', sections, i*64)[0]
        require(name_offset < len(names), 'elf-name-offset')
        if names[name_offset:].split(b'\0', 1)[0] == b'.go.buildinfo':
            matches.append(struct.unpack_from('<QQ', sections, i*64+24))
    require(len(matches) == 1, 'elf-buildinfo-cardinality')
    offset, length = matches[0]
    require(32 <= length <= 262144 and offset + length <= size, 'elf-buildinfo-bound')
    f.seek(offset)
    result = decode_buildinfo(reader.metadata(f, length), job)
    result['architecture'] = 'amd64' if machine == 62 else 'arm64'
    return result


def selected(reader):
    rows = [decode(line) for line in reader.command(
        ['ps', '--no-trunc', '--format', LIST]).splitlines()]
    require(len(rows) <= 256, 'container-list-bound')
    result = {}
    for row in rows:
        require(set(row) == {'id', 'name', 'env', 'job', 'block'}, 'list-schema')
        require(all(isinstance(row[k], str) and len(row[k]) <= 256
                    for k in ('id', 'name', 'env', 'job', 'block')), 'list-field-bound')
        named = row['name'].startswith('main-connect-')
        labelled = (row['env'], row['job']) == ('main', 'connect')
        if not named and not labelled:
            continue
        require(named and labelled and row['block'] in BLOCKS and
                row['name'].startswith('main-connect-' + row['block'] + '-') and
                HEX.fullmatch(row['id']) and row['id'] not in result,
                'slot-name-label-binding')
        result[row['id']] = row
    require(len(result) <= MAX_CONTAINERS, 'selected-container-bound')
    return result


def inspect(reader, selected_rows):
    if not selected_rows:
        return {}
    raw = reader.command(['inspect', '--type', 'container', '--format', DETAIL] +
                         sorted(selected_rows))
    rows = [decode(line) for line in raw.splitlines()]
    require(len(rows) == len(selected_rows), 'docker-inspect-cardinality')
    result = {}
    for row in rows:
        require(set(row) == {'id', 'name', 'image', 'pid', 'running', 'started',
                            'path', 'env', 'job', 'block'}, 'docker-inspect-schema')
        require(row['id'] in selected_rows and row['id'] not in result,
                'docker-inspect-identity')
        expected = selected_rows[row['id']]
        require(row['name'] == '/' + expected['name'] and
                all(row[k] == expected[k] for k in ('env', 'job', 'block')),
                'slot-name-label-binding')
        require(isinstance(row['image'], str) and IMAGE.fullmatch(row['image']) and
                type(row['pid']) is int and 0 < row['pid'] <= 4194304 and
                row['running'] is True, 'container-not-running')
        require(isinstance(row['path'], str) and len(row['path']) <= 4096 and
                isinstance(row['started'], str) and
                re.fullmatch(r'[0-9T:.Z+-]{10,40}', row['started']), 'container-state-schema')
        result[row['id']] = row
    return result


def process_stat(raw, pid):
    prefix, sep, suffix = raw.rpartition(')')
    require(sep and prefix.startswith(str(pid) + ' ('), 'process-stat-schema')
    fields = suffix.split()
    require(len(fields) >= 22 and fields[0] not in ('Z', 'X', 'x'), 'process-not-running')
    require(re.fullmatch(r'[0-9]+', fields[19]) and int(fields[19]) > 0,
            'process-start-schema')
    return int(fields[19])


STATUS_FIELDS = {'VmRSS': 'rss_bytes', 'RssAnon': 'anonymous_rss_bytes',
                 'RssFile': 'file_rss_bytes', 'RssShmem': 'shared_rss_bytes',
                 'VmSwap': 'swap_bytes'}


def process_status(raw, pid):
    fields = {}
    wanted = set(STATUS_FIELDS) | {'Pid', 'Threads'}
    for line in raw.splitlines():
        key, sep, value = line.partition(':')
        if sep and key in wanted:
            require(key not in fields, 'process-status-duplicate')
            fields[key] = value.split()
    require(set(fields) == wanted and fields['Pid'] == [str(pid)],
            'process-status-schema')
    result = {}
    for key, name in STATUS_FIELDS.items():
        value = fields[key]
        require(len(value) == 2 and value[1] == 'kB' and
                re.fullmatch(r'[0-9]{1,16}', value[0]) and int(value[0]) <= 2**43,
                'process-status-schema')
        result[name] = int(value[0]) * 1024
    require(len(fields['Threads']) == 1 and re.fullmatch(r'[0-9]{1,8}', fields['Threads'][0])
            and 0 < int(fields['Threads'][0]) <= 4194304, 'process-status-schema')
    result['threads'] = int(fields['Threads'][0])
    # Linux exports each counter from one status read, but its accounting is
    # approximate; do not reject a sample for tiny non-atomic component drift.
    return result


def boot_identity(reader):
    boot = reader.text('/proc/sys/kernel/random/boot_id', 128).strip()
    require(re.fullmatch(r'[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}', boot),
            'boot-identity-schema')
    raw = reader.text('/proc/stat', 1048576)
    times = [line.split()[1:] for line in raw.splitlines() if line.startswith('btime ')]
    require(len(times) == 1 and len(times[0]) == 1 and
            re.fullmatch(r'[0-9]{1,12}', times[0][0]), 'boot-time-schema')
    require(os.sysconf('SC_CLK_TCK') == 100, 'clock-tick-unsupported')
    return boot, int(times[0][0])


def policy_checked(policy):
    require(type(policy) is dict and set(policy) == {'images'} and
            type(policy['images']) is dict and 1 <= len(policy['images']) <= 16,
            'source-policy')
    for image, pin in policy['images'].items():
        require(isinstance(image, str) and IMAGE.fullmatch(image) and type(pin) is dict
                and set(pin) == {'architecture', 'revision', 'binary_sha256'} and
                pin['architecture'] in ('amd64', 'arm64') and
                re.fullmatch(r'[a-f0-9]{40}', pin['revision']) and
                HEX.fullmatch(pin['binary_sha256']), 'source-policy')
    return policy


def classify(error):
    if isinstance(error, Unavailable):
        return str(error)
    if isinstance(error, FileNotFoundError):
        return 'source-vanished'
    if isinstance(error, PermissionError):
        return 'permission-denied'
    if isinstance(error, (TimeoutError, subprocess.TimeoutExpired)):
        return 'timeout'
    if isinstance(error, OSError):
        return 'os-unavailable'
    return 'schema-unavailable'


def time_offsets(raw):
    # proc exports exactly two finite clocks; never interpret missing fields as
    # zero. Only boottime is needed for the procfs start-time conversion.
    fields = {}
    for line in raw.splitlines():
        parts = line.split()
        require(len(parts) == 3 and parts[0] in ('monotonic', 'boottime') and
                parts[0] not in fields and re.fullmatch(r'-?[0-9]{1,12}', parts[1]) and
                re.fullmatch(r'[0-9]{1,9}', parts[2]), 'time-offset-schema')
        seconds, nanos = int(parts[1]), int(parts[2])
        require(abs(seconds) <= 2**40 and 0 <= nanos < 1000000000, 'time-offset-schema')
        fields[parts[0]] = [seconds, nanos]
    require(set(fields) == {'monotonic', 'boottime'}, 'time-offset-schema')
    return fields['boottime']


def time_projection(reader, base, own_ns, time_ns):
    projection = {'relation': 'same_namespace' if own_ns == time_ns else 'unqualified',
                  'reader_boottime_offset': None, 'process_boottime_offset': None,
                  'offsets_bind_current_namespaces': False}
    try:
        # timens_offsets describes time_for_children. An unshared future
        # namespace is not evidence about the process's active clock view.
        require(os.readlink('/proc/self/ns/time_for_children') == own_ns and
                os.readlink(base + '/ns/time_for_children') == time_ns,
                'time-offset-namespace-unbound')
        projection['offsets_bind_current_namespaces'] = True
        own = time_offsets(reader.text('/proc/self/timens_offsets', 256))
        target = time_offsets(reader.text(base + '/timens_offsets', 256))
        projection.update(reader_boottime_offset=own, process_boottime_offset=target)
        # Linux proc stat adds the READER boottime offset to process start
        # ticks, and /proc/stat subtracts it from btime. Prometheus procfs sums
        # those two fields. Distinct namespaces with proved zero offsets have
        # exactly the same conversion. Keep all nonzero differences unknown;
        # byte caps do not justify a guessed fractional-clock correction.
        if own_ns != time_ns and own == target == [0, 0]:
            projection['relation'] = 'different_namespace_zero_offsets'
        return projection, None
    except Exception as error:
        return projection, classify(error)


def native_row(reader, state, boot, btime, policy):
    pid = state['pid']
    row = {'block': state['block'], 'container_sha256': hashlib.sha256(state['id'].encode()).hexdigest(),
           'container_short_sha256': hashlib.sha256(state['id'][:12].encode()).hexdigest(),
           'pid': pid, 'container_started_at': state['started'],
           'image_config_id': state['image'], 'native': None, 'artifact': None,
           'identity_stable': False, 'source_qualified': False,
           'metric_start_join_qualified': False, 'time_projection': None, 'causes': []}
    token = None
    try:
        base = '/proc/' + str(pid)
        ticks = process_stat(reader.text(base + '/stat'), pid)
        native = process_status(reader.text(base + '/status'), pid)
        native.update(boot_id=boot, start_ticks=ticks,
                      process_start_time_seconds=float(btime) + float(ticks) / 100,
                      observed_unix=time.time())
        row['native'] = native
        own_ns = os.readlink('/proc/self/ns/time')
        time_ns = os.readlink(base + '/ns/time')
        projection, offset_error = time_projection(reader, base, own_ns, time_ns)
        row['time_projection'] = projection
        row['metric_start_join_qualified'] = projection['relation'] != 'unqualified'
        if not row['metric_start_join_qualified']:
            row['causes'].append(offset_error or 'time-namespace-differs')
        token = (ticks, None, time_ns, own_ns, projection)
        require(state['path'] == '/usr/local/sbin/bringyour-connect', 'init-executable-path')
        artifact, exe_identity = reader.executable(pid)
        row['artifact'] = artifact
        token = (ticks, exe_identity, time_ns, own_ns, projection)
        pin = policy['images'].get(state['image'])
        if pin is None:
            row['causes'].append('image-unqualified')
        elif (artifact['executable_sha256'] != pin['binary_sha256'] or
              artifact['source']['revision'] != pin['revision'] or
              artifact['source']['architecture'] != pin['architecture']):
            row['causes'].append('image-executable-mismatch')
        else:
            row['source_qualified'] = True
    except Exception as error:
        row['causes'].append(classify(error))
    return row, token


def stable_process(reader, row, token):
    if token is None:
        return False
    pid = row['pid']
    base = '/proc/' + str(pid)
    ticks, exe_identity, time_ns, own_ns, projection = token
    require(process_stat(reader.text(base + '/stat'), pid) == ticks,
            'process-start-changed')
    require(os.readlink(base + '/ns/time') == time_ns and
            os.readlink('/proc/self/ns/time') == own_ns, 'time-namespace-changed')
    if projection['relation'] == 'different_namespace_zero_offsets':
        terminal, error = time_projection(reader, base, own_ns, time_ns)
        require(error is None and terminal == projection, 'time-offset-changed')
    if exe_identity is not None:
        require(file_identity(os.stat(base + '/exe')) == exe_identity, 'exe-changed')
    return True


HOST_MEM_FIELDS = ('MemTotal', 'MemFree', 'MemAvailable', 'Buffers', 'Cached',
                   'SwapTotal', 'SwapFree', 'Shmem', 'Slab', 'SReclaimable',
                   'SUnreclaim', 'KernelStack', 'PageTables')
MAX_HOST_PROCESSES = 1024
HOST_SECONDS = 5


def host_meminfo(raw):
    fields = {}
    for line in raw.splitlines():
        key, sep, value = line.partition(':')
        if sep and key in HOST_MEM_FIELDS:
            parts = value.split()
            require(key not in fields and len(parts) == 2 and parts[1] == 'kB'
                    and re.fullmatch(r'[0-9]{1,16}', parts[0])
                    and int(parts[0]) <= 2**43, 'host-meminfo-schema')
            fields[key] = int(parts[0]) * 1024
    require(set(fields) == set(HOST_MEM_FIELDS) and fields['MemTotal'] > 0
            and fields['MemFree'] <= fields['MemTotal']
            and fields['MemAvailable'] <= fields['MemTotal']
            and fields['SwapFree'] <= fields['SwapTotal'], 'host-meminfo-schema')
    return fields


def host_clock(reader, end):
    reader.clock()
    require(time.monotonic() < end, 'host-sample-time-bound')


def host_process_ids(reader, end):
    pids = set()
    with os.scandir('/proc') as entries:
        for count, entry in enumerate(entries):
            host_clock(reader, end)
            if count >= MAX_HOST_PROCESSES + 256:
                raise HostProcessListBound('entries', count + 1, len(pids))
            if entry.name.isascii() and entry.name.isdecimal():
                pid = int(entry.name)
                require(0 < pid <= 4194304 and pid not in pids, 'host-proc-list-schema')
                pids.add(pid)
                if len(pids) > MAX_HOST_PROCESSES:
                    raise HostProcessListBound('numeric_pids', count + 1, len(pids))
    return pids


def host_statm(raw, page_size):
    fields = raw.split()
    require(len(fields) == 7 and all(re.fullmatch(r'[0-9]{1,16}', x) for x in fields)
            and int(fields[1]) <= int(fields[0])
            and int(fields[1]) * page_size <= 2**53, 'host-statm-schema')
    return int(fields[1]) * page_size


def host_memory(reader, connect_rows, partition_qualified):
    # RSS sums double-count shared pages, omit kernel/cache ownership, and are
    # approximate procfs counters. MemAvailable is the headroom observation;
    # neither subtraction from MemTotal nor an RSS sum is a capacity forecast.
    end = min(reader.started + MAX_SECONDS, time.monotonic() + HOST_SECONDS)
    out = {'complete': False, 'meminfo_complete': False,
           'process_aggregate_complete': False,
           'connect_partition_qualified': False,
           'before': None, 'after': None, 'mem_available_min_bytes': None,
           'processes_listed': 0, 'processes_stable': 0, 'processes_unavailable': 0,
           # None means no qualified list-bound detail, not an empty host.
           # Counts in a refusal are lower bounds; processes_listed counts
           # only a completed initial enumeration.
           'process_list_refusal': None,
           'process_rss_lower_bound_bytes': 0,
           'connect_init_rss_lower_bound_bytes': None,
           'other_process_rss_lower_bound_bytes': None,
           'rss_shared_pages_may_be_counted_multiple_times': True,
           'rss_is_approximate': True, 'causes': [],
           'started_unix': time.time(), 'completed_unix': None}
    try:
        host_clock(reader, end)
        boot = reader.text('/proc/sys/kernel/random/boot_id', 128)
        require(re.fullmatch(r'[a-f0-9]{8}(-[a-f0-9]{4}){3}-[a-f0-9]{12}\n?', boot),
                'boot-identity-schema')
        out['before'] = host_meminfo(reader.text('/proc/meminfo', 16384))
        census_finished = False
        connect_rss = 0
        listing_phase = 'initial'
        try:
            page_size = os.sysconf('SC_PAGE_SIZE')
            require(type(page_size) is int and page_size in (4096, 16384, 65536),
                    'host-page-size-unsupported')
            pids = host_process_ids(reader, end)
            out['processes_listed'] = len(pids)
            connect = {r['pid']: r['native']['start_ticks'] for r in connect_rows
                       if r['identity_stable'] and r['native'] is not None}
            require(len(connect) == len(connect_rows) or not partition_qualified,
                    'host-connect-partition-unbound')
            connect_rss = 0
            for pid in sorted(pids):
                host_clock(reader, end)
                try:
                    base = '/proc/' + str(pid)
                    ticks = process_stat(reader.text(base + '/stat', 4096), pid)
                    rss = host_statm(reader.text(base + '/statm', 256), page_size)
                    require(process_stat(reader.text(base + '/stat', 4096), pid) == ticks,
                            'process-start-changed')
                    if pid in connect:
                        require(ticks == connect[pid], 'host-connect-partition-unbound')
                        connect_rss += rss
                    out['process_rss_lower_bound_bytes'] += rss
                    out['processes_stable'] += 1
                except Exception as error:
                    out['processes_unavailable'] += 1
                    out['causes'].append(classify(error))
                    if pid in connect:
                        out['causes'].append('host-connect-partition-unbound')
            listing_phase = 'terminal'
            terminal = host_process_ids(reader, end)
            if pids != terminal:
                out['causes'].append('host-process-set-changed')
            if not set(connect) <= pids:
                out['causes'].append('host-connect-partition-unbound')
            census_finished = True
        except Exception as error:
            out['causes'].append(classify(error))
            if isinstance(error, HostProcessListBound):
                out['process_list_refusal'] = {'phase': listing_phase, **error.facts}
        out['after'] = host_meminfo(reader.text('/proc/meminfo', 16384))
        require(reader.text('/proc/sys/kernel/random/boot_id', 128) == boot,
                'boot-identity-changed')
        require(out['before']['MemTotal'] == out['after']['MemTotal'],
                'host-memory-total-changed')
        host_clock(reader, end)
        out['meminfo_complete'] = True
        out['mem_available_min_bytes'] = min(out['before']['MemAvailable'], out['after']['MemAvailable'])
        partition = partition_qualified and 'host-connect-partition-unbound' not in out['causes'] and census_finished
        out['connect_partition_qualified'] = partition
        if partition:
            out['connect_init_rss_lower_bound_bytes'] = connect_rss
            out['other_process_rss_lower_bound_bytes'] = out['process_rss_lower_bound_bytes'] - connect_rss
        out['process_aggregate_complete'] = not out['causes'] and partition
        out['complete'] = out['meminfo_complete'] and out['process_aggregate_complete']
    except Exception as error:
        out['causes'].append(classify(error))
    if not partition_qualified:
        out['causes'].append('host-connect-partition-unbound')
    out['causes'] = sorted(set(out['causes']))
    out['completed_unix'] = time.time()
    return out


def collect(host, policy, reader=None):
    policy_checked(policy)
    require(host in HOSTS and os.geteuid() == 0 and
            os.uname().nodename.split('.')[0] == host, 'hostname-or-uid')
    reader = reader or Reader()
    result = {'schema': 1, 'kind': 'connect-native-memory', 'host': host,
              'read_only': True, 'complete': False, 'scope_stable': False,
              'source_complete': False, 'expected_blocks': list(BLOCKS),
              'missing_blocks': list(BLOCKS), 'rows': [], 'causes': [],
              'started_unix': time.time()}
    try:
        boot, btime = boot_identity(reader)
        before = selected(reader)
        states = inspect(reader, before)
        result['missing_blocks'] = sorted(set(BLOCKS) - {s['block'] for s in states.values()})
        if result['missing_blocks']:
            result['causes'].append('missing-slots')
        tokens = {}
        for cid, state in sorted(states.items()):
            row, tokens[cid] = native_row(reader, state, boot, btime, policy)
            result['rows'].append(row)
        after = selected(reader)
        terminal = inspect(reader, after)
        if before != after:
            result['causes'].append('container-set-changed')
        if states != terminal:
            result['causes'].append('container-state-changed')
        same_boot = boot_identity(reader) == (boot, btime)
        if not same_boot:
            result['causes'].append('boot-identity-changed')
        for (cid, state), row in zip(sorted(states.items()), result['rows']):
            try:
                row['identity_stable'] = (same_boot and terminal.get(cid) == state and
                                           stable_process(reader, row, tokens[cid]))
                if not row['identity_stable']:
                    row['causes'].append('terminal-identity-unavailable')
            except Exception as error:
                row['causes'].append(classify(error))
            if not row['identity_stable']:
                row['source_qualified'] = False
                row['metric_start_join_qualified'] = False
            row['causes'] = sorted(set(row['causes']))
        result['scope_stable'] = before == after and states == terminal and same_boot
        result['complete'] = (result['scope_stable'] and not result['missing_blocks'] and
                              all(r['native'] is not None and r['identity_stable']
                                  for r in result['rows']))
        result['source_complete'] = result['complete'] and all(r['source_qualified'] for r in result['rows'])
        reader.clock()
    except Exception as error:
        result['complete'] = False
        result['source_complete'] = False
        result['causes'].append(classify(error))
    for row in result['rows']:
        if not row['identity_stable']:
            row['source_qualified'] = False
            row['metric_start_join_qualified'] = False
    result['host_memory'] = host_memory(reader, result['rows'], result['scope_stable'] and
                                       all(r['identity_stable'] for r in result['rows']))
    result.update(completed_unix=time.time(), metadata_bytes=reader.metadata_bytes,
                  hash_bytes=reader.hash_bytes, elapsed_seconds=time.monotonic() - reader.started)
    require(len(json.dumps(result, allow_nan=False).encode()) <= 131072, 'result-output-bound')
    return result

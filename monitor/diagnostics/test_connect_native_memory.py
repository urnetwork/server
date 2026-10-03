import copy
import importlib.util
import io
import json
import os
import pathlib
import struct
import tempfile
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location('connect_native_memory', pathlib.Path(__file__).with_name('connect_native_memory.py'))
n = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(n)
HOST = n.HOSTS[0]
IMAGE = 'sha256:' + 'a' * 64
BOOT = '11111111-2222-3333-4444-555555555555'
POLICY = {'images': {IMAGE: {'architecture': 'amd64', 'revision': 'd' * 40,
                             'binary_sha256': 'b' * 64}}}


def proc_stat(pid=123, ticks=45678):
    return str(pid) + ' (service ) name) S ' + '0 ' * 18 + str(ticks) + ' 0 12\n'


def proc_status(pid=123, rss=100):
    return (f'Pid:\t{pid}\nThreads:\t17\nVmRSS:\t{rss} kB\nRssAnon:\t70 kB\n'
            'RssFile:\t20 kB\nRssShmem:\t10 kB\nVmSwap:\t3 kB\n')


def uvar(value):
    out = bytearray()
    while value >= 128:
        out.append((value & 127) | 128); value >>= 7
    return bytes(out + bytes([value]))


def elf(job='connect', modified='false'):
    names = b'\0.shstrtab\0.go.buildinfo\0'
    text = ('path\tgithub.com/urnetwork/server/cli/' + job +
            '\nbuild\tvcs.revision=' + 'd' * 40 + '\nbuild\tvcs.modified=' + modified + '\n').encode()
    module = b'a' * 16 + text + b'b' * 16
    info = bytearray(32); info[:14] = b'\xff Go buildinf:'; info[14:16] = bytes((8, 2))
    version = b'go1.26.7'; info += uvar(len(version)) + version + uvar(len(module)) + module
    data = bytearray(1216); data[:6] = b'\x7fELF\x02\x01'
    struct.pack_into('<H', data, 18, 62); struct.pack_into('<Q', data, 40, 1024)
    struct.pack_into('<HHH', data, 58, 64, 3, 1)
    data[64:64 + len(names)] = names; data[128:128 + len(info)] = info
    struct.pack_into('<I', data, 1088, names.index(b'.shstrtab'))
    struct.pack_into('<QQ', data, 1112, 64, len(names))
    struct.pack_into('<I', data, 1152, names.index(b'.go.buildinfo'))
    struct.pack_into('<QQ', data, 1176, 128, len(info))
    return bytes(data)


def listing(block='g1', number=1):
    return {'id': format(number, '064x'), 'name': 'main-connect-' + block + '-fixture-1',
            'env': 'main', 'job': 'connect', 'block': block}


def artifact():
    return {'source': n.elf_buildinfo(n.Reader(), io.BytesIO(elf()), len(elf()), 'connect'),
            'executable_sha256': 'b' * 64, 'executable_bytes': 1216}


class Fixture(n.Reader):
    def __init__(self, overlap=False):
        super().__init__()
        self.rows = [listing(block, i + 1) for i, block in enumerate(n.BLOCKS)]
        if overlap:
            self.rows.append(listing('g1', 6))
        self.list_calls = 0
        self.drift = False
        self.status_missing = False
        self.changed_ticks = False
        self.stat_calls = {}

    def command(self, args):
        if args[0] == 'ps':
            self.list_calls += 1
            rows = self.rows[:-1] if self.drift and self.list_calls > 1 else self.rows
        elif args[0] == 'inspect':
            rows = []
            for row in self.rows:
                if row['id'] not in args:
                    continue
                rows.append(dict(row, name='/' + row['name'], image=IMAGE,
                                 pid=int(row['id'], 16) + 100, running=True,
                                 started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect'))
        else:
            raise AssertionError('unexpected command')
        return b'\n'.join(json.dumps(row).encode() for row in rows)

    def text(self, path, cap=16384):
        if path.endswith('/boot_id'):
            return BOOT
        if path == '/proc/stat':
            return 'cpu 1 2 3 4\nbtime 1800000000\n'
        if path.endswith('/timens_offsets'):
            return 'monotonic 0 0\nboottime 0 0\n'
        pid = int(path.split('/')[2])
        if path.endswith('/status'):
            if self.status_missing and pid == 101:
                raise FileNotFoundError()
            return proc_status(pid, rss=pid * 1000)
        self.stat_calls[pid] = self.stat_calls.get(pid, 0) + 1
        ticks = pid * 100 + int(self.changed_ticks and self.stat_calls[pid] > 1)
        return proc_stat(pid, ticks)

    def executable(self, pid):
        return artifact(), None


def collect(fixture):
    with mock.patch.object(n.os, 'geteuid', return_value=0), \
         mock.patch.object(n.os, 'uname', return_value=type('U', (), {'nodename': HOST})()), \
         mock.patch.object(n.os, 'readlink', return_value='time:[42]'), \
         mock.patch.object(n.os, 'sysconf', return_value=100), \
         mock.patch.object(n, 'host_memory', side_effect=lambda r, rows, qualified: host_fixture(qualified)):
        return n.collect(HOST, POLICY, fixture)


def host_fixture(qualified=True):
    fields={k:0 for k in n.HOST_MEM_FIELDS};fields.update(MemTotal=1024*1024,MemAvailable=512*1024)
    return {'complete':qualified,'meminfo_complete':True,'process_aggregate_complete':qualified,
            'connect_partition_qualified':qualified,'before':fields,'after':fields,
            'mem_available_min_bytes':512*1024,'processes_listed':6,'processes_stable':6,
            'processes_unavailable':0,'process_rss_lower_bound_bytes':16384,
            'connect_init_rss_lower_bound_bytes':8192 if qualified else None,'other_process_rss_lower_bound_bytes':8192 if qualified else None,
            'rss_shared_pages_may_be_counted_multiple_times':True,'rss_is_approximate':True,
            'causes':[] if qualified else ['host-connect-partition-unbound'],'started_unix':1800000000.,'completed_unix':1800000001.}


class NativeControls(unittest.TestCase):
    def test_status_native_bytes_and_threads(self):
        value = n.process_status(proc_status(), 123)
        self.assertEqual(value['rss_bytes'], 102400)
        self.assertEqual(value['anonymous_rss_bytes'], 71680)
        self.assertEqual(value['threads'], 17)

    def test_missing_duplicate_bad_units_and_wrong_pid_are_unknown(self):
        for raw in (proc_status().replace('VmRSS:', 'unrelated:'), proc_status() + 'VmRSS: 1 kB\n',
                    proc_status().replace('100 kB', '100 MB'), proc_status(124)):
            with self.subTest(raw=raw), self.assertRaises(n.Unavailable):
                n.process_status(raw, 123)

    def test_stat_parentheses_and_exact_pid(self):
        self.assertEqual(n.process_stat(proc_stat(), 123), 45678)
        with self.assertRaises(n.Unavailable): n.process_stat(proc_stat(), 124)

    def test_all_live_generations_and_healthy_control_retained(self):
        result = collect(Fixture(overlap=True))
        self.assertTrue(result['complete']); self.assertTrue(result['source_complete'])
        self.assertEqual(len(result['rows']), 6)
        self.assertEqual(sum(row['block'] == 'g1' for row in result['rows']), 2)
        self.assertTrue(all(row['native']['rss_bytes'] > 0 for row in result['rows']))
        self.assertEqual(result['rows'][0]['native']['process_start_time_seconds'], 1800000101.)

    def test_empty_and_missing_slot_stay_incomplete(self):
        for rows in ([], [listing()]):
            fixture = Fixture(); fixture.rows = rows
            result = collect(fixture)
            self.assertFalse(result['complete']); self.assertIn('missing-slots', result['causes'])

    def test_changed_set_keeps_observed_rows_but_no_complete_claim(self):
        fixture = Fixture(overlap=True); fixture.drift = True
        result = collect(fixture)
        self.assertEqual(len(result['rows']), 6); self.assertFalse(result['complete'])
        self.assertIn('container-set-changed', result['causes'])
        self.assertFalse(result['rows'][-1]['identity_stable'])

    def test_source_vanishes_without_erasing_other_native_pressure(self):
        fixture = Fixture(); fixture.status_missing = True
        result = collect(fixture)
        self.assertFalse(result['complete']); self.assertEqual(len(result['rows']), 5)
        self.assertIn('source-vanished', result['rows'][0]['causes'])
        self.assertIsNone(result['rows'][0]['native'])
        self.assertEqual(sum(row['native'] is not None for row in result['rows']), 4)

    def test_same_pid_reuse_cannot_lend_old_sample_or_source(self):
        fixture = Fixture(); fixture.changed_ticks = True
        result = collect(fixture)
        self.assertFalse(result['complete'])
        for row in result['rows']:
            self.assertFalse(row['identity_stable']); self.assertFalse(row['source_qualified'])
            self.assertFalse(row['metric_start_join_qualified'])
            self.assertIn('process-start-changed', row['causes'])

    def test_unapproved_image_retains_rss_with_explicit_source_unknown(self):
        policy = copy.deepcopy(POLICY); policy['images'] = {'sha256:' + 'c' * 64: policy['images'][IMAGE]}
        with mock.patch.dict(POLICY, policy, clear=True): result = collect(Fixture())
        self.assertTrue(result['complete']); self.assertFalse(result['source_complete'])
        self.assertIn('image-unqualified', result['rows'][0]['causes'])

    def test_binary_mismatch_never_borrows_image_qualification(self):
        policy = copy.deepcopy(POLICY); policy['images'][IMAGE]['binary_sha256'] = 'f' * 64
        with mock.patch.dict(POLICY, policy, clear=True): result = collect(Fixture())
        self.assertFalse(result['source_complete'])
        self.assertIn('image-executable-mismatch', result['rows'][0]['causes'])

    def test_time_namespace_mismatch_retains_native_but_blocks_metric_join(self):
        fixture = Fixture(); state = dict(listing(), pid=123, image=IMAGE,
                                          started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect')
        original = fixture.text
        def text(path, cap=16384):
            if path.endswith('/timens_offsets') and '/self/' not in path:
                return 'monotonic 0 0\nboottime 1 1\n'
            return original(path, cap)
        fixture.text = text
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[1]', 'time:[2]', 'time:[1]', 'time:[2]']):
            row, token = n.native_row(fixture, state, BOOT, 1800000000, POLICY)
        self.assertIsNotNone(row['native']); self.assertFalse(row['metric_start_join_qualified'])
        self.assertIn('time-namespace-differs', row['causes'])
        self.assertEqual(row['time_projection']['process_boottime_offset'], [1, 1])

    def test_distinct_namespaces_with_proved_zero_offsets_match_conversion(self):
        fixture = Fixture(); state = dict(listing(), pid=123, image=IMAGE,
                                          started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect')
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[1]', 'time:[2]', 'time:[1]', 'time:[2]']):
            row, token = n.native_row(fixture, state, BOOT, 1800000000, POLICY)
        self.assertTrue(row['metric_start_join_qualified'])
        self.assertEqual(row['time_projection']['relation'], 'different_namespace_zero_offsets')
        self.assertTrue(row['source_qualified'])
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[2]', 'time:[1]', 'time:[1]', 'time:[2]']):
            self.assertTrue(n.stable_process(fixture, row, token))

    def test_time_offsets_unavailable_or_changed_preserve_unknown(self):
        for fault in ('missing', 'duplicate', 'negative_ns', 'overflow', 'other', 'unreadable'):
            with self.subTest(fault=fault):
                fixture = Fixture(); original = fixture.text
                values = {'missing':'boottime 0 0\n', 'duplicate':'monotonic 0 0\nboottime 0 0\nboottime 0 0\n',
                          'negative_ns':'monotonic 0 0\nboottime 0 -1\n', 'overflow':'monotonic 0 0\nboottime 99999999999999 0\n',
                          'other':'monotonic 0 0\nboottime 0 0\nrealtime 0 0\n'}
                def text(path, cap=16384):
                    if path.endswith('/timens_offsets'):
                        if fault == 'unreadable': raise PermissionError()
                        return values[fault]
                    return original(path, cap)
                fixture.text = text
                state = dict(listing(), pid=123, image=IMAGE, started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect')
                with mock.patch.object(n.os, 'readlink', side_effect=['time:[1]', 'time:[2]', 'time:[1]', 'time:[2]']):
                    row, token = n.native_row(fixture, state, BOOT, 1800000000, POLICY)
                self.assertIsNotNone(row['native']); self.assertTrue(row['source_qualified'])
                self.assertFalse(row['metric_start_join_qualified'])
        fixture = Fixture(); state = dict(listing(), pid=123, image=IMAGE, started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect')
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[1]', 'time:[2]', 'time:[1]', 'time:[2]']):
            row, token = n.native_row(fixture, state, BOOT, 1800000000, POLICY)
        original = fixture.text
        fixture.text = lambda path,cap=16384: 'monotonic 0 0\nboottime 1 0\n' if path.endswith('/timens_offsets') else original(path,cap)
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[2]', 'time:[1]', 'time:[1]', 'time:[2]']), self.assertRaisesRegex(n.Unavailable, 'time-offset-changed'):
            n.stable_process(fixture, row, token)

    def test_child_namespace_offsets_cannot_qualify_current_process(self):
        fixture = Fixture(); state = dict(listing(), pid=123, image=IMAGE,
                                          started='2026-10-03T01:00:00Z', path='/usr/local/sbin/bringyour-connect')
        with mock.patch.object(n.os, 'readlink', side_effect=['time:[1]', 'time:[2]', 'time:[1]', 'time:[3]']):
            row, token = n.native_row(fixture, state, BOOT, 1800000000, POLICY)
        self.assertFalse(row['metric_start_join_qualified'])
        self.assertFalse(row['time_projection']['offsets_bind_current_namespaces'])
        self.assertIn('time-offset-namespace-unbound', row['causes'])

    def test_native_no_names_no_environment_or_private_data(self):
        raw = json.dumps(collect(Fixture()))
        self.assertNotIn('fixture-1', raw); self.assertNotIn('WARP_', raw)
        self.assertNotIn('main-connect-g1-', raw)

    def test_actual_elf_preserves_modified_and_rejects_wrong_service(self):
        for modified in ('true', 'false'):
            data = elf(modified=modified)
            value = n.elf_buildinfo(n.Reader(), io.BytesIO(data), len(data), 'connect')
            self.assertEqual(value['modified'], modified == 'true')
        with self.assertRaises(n.Unavailable):
            data = elf('api'); n.elf_buildinfo(n.Reader(), io.BytesIO(data), len(data), 'connect')

    def test_executable_actual_read_hash_and_shared_inode_cache(self):
        with tempfile.NamedTemporaryFile() as f:
            f.write(elf()); f.flush(); original_open = open; original_stat = os.stat
            def opened(path, mode):
                self.assertEqual(path, '/proc/123/exe'); self.assertEqual(mode, 'rb')
                return original_open(f.name, mode)
            with mock.patch('builtins.open', side_effect=opened), \
                 mock.patch.object(n.os, 'stat', side_effect=lambda _: original_stat(f.name)):
                reader = n.Reader(); value, identity = reader.executable(123)
                self.assertEqual(value['executable_sha256'], n.hashlib.sha256(elf()).hexdigest())
                reader.executable(123); self.assertEqual(reader.hash_bytes, len(elf()))

    def test_clock_and_byte_budgets_fail_closed(self):
        reader = n.Reader(); reader.started -= 100
        with self.assertRaisesRegex(n.Unavailable, 'total-time-bound'): reader.clock()
        reader = n.Reader(); reader.metadata_bytes = n.MAX_METADATA
        with self.assertRaises(n.Unavailable): reader.metadata(io.BytesIO(b'a'), 1)

    def test_invalid_policy_duplicate_json_and_slot_binding(self):
        for policy in ({}, {'images': {}}, {'images': {'sha256:' + 'a' * 64: {}}}):
            with self.assertRaises(n.Unavailable): n.policy_checked(policy)
        with self.assertRaises(n.Unavailable): n.decode('{"a":1,"a":2}')
        fixture = Fixture(); fixture.rows[0]['block'] = 'g1'
        with self.assertRaises(n.Unavailable): n.selected(fixture)

    def test_actual_local_process_status(self):
        reader = n.Reader(); pid = os.getpid()
        value = n.process_status(reader.text('/proc/' + str(pid) + '/status'), pid)
        ticks = n.process_stat(reader.text('/proc/' + str(pid) + '/stat'), pid)
        self.assertGreater(value['rss_bytes'], 0); self.assertGreater(ticks, 0)


if __name__ == '__main__': unittest.main()

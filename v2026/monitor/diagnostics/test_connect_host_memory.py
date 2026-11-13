import copy
import json
import time
import unittest
from unittest import mock

from test_connect_native_memory import n, BOOT, Fixture, collect, proc_stat


def meminfo(available=400, total=1000):
    fields={k:0 for k in n.HOST_MEM_FIELDS}
    fields.update(MemTotal=total, MemAvailable=available, MemFree=100,
                  SwapTotal=200, SwapFree=150, Slab=80, SUnreclaim=60)
    return ''.join(f'{key}: {value} kB\n' for key,value in fields.items())


class HostFixture(n.Reader):
    def __init__(self):
        super().__init__()
        self.calls={}
        self.fail_pid=None
        self.reuse=False
        self.boot_changed=False
        self.total_changed=False

    def text(self,path,cap=16384):
        self.calls[path]=self.calls.get(path,0)+1
        count=self.calls[path]
        if path.endswith('/boot_id'):
            return ('f'+BOOT[1:]) if self.boot_changed and count>1 else BOOT
        if path=='/proc/meminfo':
            return meminfo(400 if count==1 else 350, 1001 if self.total_changed and count>1 else 1000)
        pid=int(path.split('/')[2])
        if pid==self.fail_pid:raise PermissionError('private process error')
        if path.endswith('/statm'):return f'1000 {pid} 0 0 0 0 0\n'
        return proc_stat(pid,pid*100+int(self.reuse and pid==10 and count>1))


ROWS=[{'pid':10,'identity_stable':True,'native':{'start_ticks':1000}}]


def sample(fixture=None, lists=None, qualified=True, rows=None):
    with mock.patch.object(n,'host_process_ids',side_effect=lists or [{10,20},{10,20}]), \
         mock.patch.object(n.os,'sysconf',return_value=4096):
        value=n.host_memory(fixture or HostFixture(), ROWS if rows is None else rows, qualified)
    return value


class HostControls(unittest.TestCase):
    def test_healthy_headroom_uses_native_available_and_preserves_other_processes(self):
        h=sample()
        self.assertTrue(h['complete'])
        self.assertEqual(h['mem_available_min_bytes'],350*1024)
        self.assertEqual(h['process_rss_lower_bound_bytes'],30*4096)
        self.assertEqual(h['connect_init_rss_lower_bound_bytes'],10*4096)
        self.assertEqual(h['other_process_rss_lower_bound_bytes'],20*4096)
        self.assertTrue(h['rss_shared_pages_may_be_counted_multiple_times'])

    def test_zero_available_is_real_pressure_not_unknown(self):
        f=HostFixture();original=f.text
        f.text=lambda p,cap=16384:meminfo(0) if p=='/proc/meminfo' else original(p,cap)
        h=sample(f);self.assertTrue(h['complete']);self.assertEqual(h['mem_available_min_bytes'],0)

    def test_missing_duplicate_units_negative_or_inconsistent_meminfo_refuses(self):
        for raw in (meminfo().replace('MemAvailable:','Unknown:'),meminfo()+'MemTotal: 1 kB\n',
                    meminfo().replace('400 kB','400 MB'),meminfo().replace('400 kB','-1 kB'),meminfo(1001)):
            with self.subTest(raw=raw),self.assertRaises(n.Unavailable):n.host_meminfo(raw)

    def test_permission_failure_is_partial_with_finite_cause_and_no_raw_identity(self):
        f=HostFixture();f.fail_pid=20;h=sample(f)
        self.assertFalse(h['complete']);self.assertTrue(h['meminfo_complete'])
        self.assertEqual(h['processes_unavailable'],1);self.assertEqual(h['processes_stable'],1)
        self.assertEqual(h['other_process_rss_lower_bound_bytes'],0)
        self.assertIn('permission-denied',h['causes']);self.assertNotIn('private',json.dumps(h))

    def test_pid_reuse_cannot_assign_old_connect_ownership(self):
        f=HostFixture();f.reuse=True;h=sample(f)
        self.assertFalse(h['complete']);self.assertEqual(h['processes_unavailable'],1)
        self.assertIn('process-start-changed',h['causes'])
        self.assertFalse(h['connect_partition_qualified'])
        self.assertIsNone(h['other_process_rss_lower_bound_bytes'])

    def test_missing_connect_and_unqualified_partition_never_assign_other_total(self):
        for lists,qualified in [([{20},{20}],True),([{10,20},{10,20}],False)]:
            h=sample(lists=lists,qualified=qualified)
            self.assertFalse(h['complete']);self.assertFalse(h['connect_partition_qualified'])
            self.assertIsNone(h['other_process_rss_lower_bound_bytes'])

    def test_process_churn_preserves_samples_but_rejects_full_aggregate(self):
        h=sample(lists=[{10,20},{10,20,30}])
        self.assertFalse(h['complete']);self.assertTrue(h['meminfo_complete'])
        self.assertIn('host-process-set-changed',h['causes'])

    def test_boot_or_memory_hotplug_change_cannot_prove_headroom(self):
        for field in ('boot_changed','total_changed'):
            f=HostFixture();setattr(f,field,True);h=sample(f)
            self.assertFalse(h['meminfo_complete']);self.assertIsNone(h['mem_available_min_bytes'])

    def test_exhausted_bound_is_unknown_not_zero_pressure(self):
        f=HostFixture();f.started=time.monotonic()-31;h=sample(f)
        self.assertFalse(h['complete']);self.assertIsNone(h['before'])
        self.assertIn('total-time-bound',h['causes'])

    def test_listing_cap_refuses_without_unbounded_materialization(self):
        entries=[type('Entry',(),{'name':str(i+1)})() for i in range(n.MAX_HOST_PROCESSES+1)]
        manager=mock.MagicMock();manager.__enter__.return_value=iter(entries)
        with mock.patch.object(n.os,'scandir',return_value=manager),self.assertRaises(n.Unavailable):
            n.host_process_ids(n.Reader(),time.monotonic()+5)

    def test_listing_bound_distinguishes_pid_and_entry_cutoffs_without_extra_read(self):
        for kind, names, expected_entries, expected_pids in (
            ('numeric_pids', (str(i+1) for i in range(2000)), 1025, 1025),
            ('entries', ('non-pid' for _ in range(2000)), 1281, 0),
        ):
            seen = []
            def entries():
                for name in names:
                    seen.append(1)
                    yield type('Entry', (), {'name': name})()
            manager = mock.MagicMock()
            manager.__enter__.return_value = entries()
            with mock.patch.object(n.os, 'scandir', return_value=manager), self.assertRaises(n.HostProcessListBound) as caught:
                n.host_process_ids(n.Reader(), time.monotonic()+5)
            self.assertEqual(len(seen), expected_entries)
            self.assertEqual(caught.exception.facts, {'limit': kind, 'entries_observed': expected_entries,
                                                       'numeric_pids_seen': expected_pids})
            self.assertEqual(n.classify(caught.exception), 'host-proc-list-bound')

    def test_refusal_phase_keeps_paired_meminfo_and_incomplete_census(self):
        refusal = n.HostProcessListBound('numeric_pids', 1100, 1025)
        for phase, steps, listed in (('initial', [refusal], 0), ('terminal', [{10,20}, refusal], 2)):
            h = sample(lists=steps)
            self.assertTrue(h['meminfo_complete'])
            self.assertEqual(h['mem_available_min_bytes'], 350*1024)
            self.assertFalse(h['process_aggregate_complete'])
            self.assertIsNone(h['other_process_rss_lower_bound_bytes'])
            self.assertEqual(h['processes_listed'], listed)
            self.assertEqual(h['process_list_refusal'], {'phase': phase, 'limit': 'numeric_pids',
                                                         'entries_observed': 1100, 'numeric_pids_seen': 1025})

    def test_time_or_permission_failure_does_not_invent_census_size(self):
        for error in (PermissionError('private'), n.Unavailable('host-sample-time-bound')):
            h = sample(lists=[error])
            self.assertTrue(h['meminfo_complete'])
            self.assertIsNone(h['process_list_refusal'])
            self.assertFalse(h['process_aggregate_complete'])

    def test_capped_or_failed_census_keeps_independent_paired_meminfo(self):
        for error in (n.Unavailable('host-proc-list-bound'), PermissionError('private detail')):
            with mock.patch.object(n,'host_process_ids',side_effect=error), \
                 mock.patch.object(n.os,'sysconf',return_value=4096):
                h=n.host_memory(HostFixture(),ROWS,True)
            self.assertFalse(h['complete']);self.assertFalse(h['process_aggregate_complete'])
            self.assertTrue(h['meminfo_complete']);self.assertIsNotNone(h['after'])
            self.assertEqual(h['mem_available_min_bytes'],350*1024)
            self.assertIsNone(h['other_process_rss_lower_bound_bytes'])
            self.assertFalse(h['connect_partition_qualified'])
            self.assertNotIn('private',json.dumps(h))

    def test_kernel_zero_statm_is_valid_but_malformed_or_impossible_fields_refuse(self):
        self.assertEqual(n.host_statm('0 0 0 0 0 0 0',4096),0)
        for raw in ('1 2 0 0 0 0 0','100 20 0','1 -1 0 0 0 0 0'):
            with self.assertRaises(n.Unavailable):n.host_statm(raw,4096)

    def test_connect_projection_remains_separate_from_capacity_completeness(self):
        v=collect(Fixture());v['host_memory']=sample(qualified=False)
        self.assertTrue(v['complete'])
        self.assertFalse(v['host_memory']['complete'])


if __name__=='__main__':unittest.main()

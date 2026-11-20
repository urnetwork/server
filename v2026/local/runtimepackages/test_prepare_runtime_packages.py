"""Real HTTP boundaries for locked payload preparation; no external services."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
from pathlib import Path
import tempfile
import threading
import time
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[2] / 'cli/prepare-runtime-packages.py'
spec = importlib.util.spec_from_file_location('runtime_fetch', SCRIPT)
fetcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fetcher)
BODY = b'locked package payload\n'


class FetchTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.cache = Path(self.temp.name)
        self.statuses = [200]
        self.body = BODY
        self.calls = 0
        self.cache_headers = []
        owner = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                owner.calls += 1
                owner.cache_headers.append(self.headers.get('Cache-Control'))
                status = owner.statuses[min(owner.calls - 1, len(owner.statuses) - 1)]
                body = owner.body if status == 200 else b'<html>503 Service Unavailable</html>'
                self.send_response(status)
                self.send_header('Content-Length', str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        self.server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        self.item = {'id': 'fixture', 'url': f'http://127.0.0.1:{self.server.server_port}/package',
                     'size': len(BODY), 'sha256': hashlib.sha256(BODY).hexdigest()}

    def fetch(self):
        return fetcher.fetch(self.item, self.cache, time.monotonic() + 5, sleep=lambda _: None)

    def test_success_and_reuse_rehash_bytes_without_network(self):
        path = self.fetch()
        self.assertEqual(BODY, path.read_bytes())
        self.assertEqual(path, self.fetch())
        self.assertEqual(1, self.calls)
        path.write_bytes(b'corrupt cache')
        self.assertEqual(BODY, self.fetch().read_bytes())
        self.assertEqual(2, self.calls)

    def test_transient_503_recovers_bounded_and_caches_only_verified_bytes(self):
        self.statuses = [503, 503, 200]
        self.assertEqual(BODY, self.fetch().read_bytes())
        self.assertEqual(3, self.calls)
        self.assertEqual(['no-cache'] * 3, self.cache_headers)
        self.assertEqual([self.item['sha256']], [p.name for p in self.cache.iterdir()])

    def test_terminal_503_stops_after_four_attempts_without_cached_error_body(self):
        self.statuses = [503]
        with self.assertRaisesRegex(RuntimeError, 'HTTP 503'):
            self.fetch()
        self.assertEqual(4, self.calls)
        self.assertEqual([], list(self.cache.iterdir()))

    def test_nontransient_http_does_not_retry(self):
        self.statuses = [404]
        with self.assertRaisesRegex(RuntimeError, 'HTTP 404'):
            self.fetch()
        self.assertEqual(1, self.calls)

    def test_wrong_200_bytes_are_rejected_without_retry_or_cache(self):
        self.body = b'x' * len(BODY)
        with self.assertRaisesRegex(ValueError, 'size/SHA256 mismatch'):
            self.fetch()
        self.assertEqual(1, self.calls)
        self.assertEqual([], list(self.cache.iterdir()))

    def test_oversize_and_truncated_payloads_fail_closed(self):
        for body in [BODY + b'extra', BODY[:-1]]:
            self.body = body
            with self.assertRaises(ValueError):
                self.fetch()
            self.assertEqual([], list(self.cache.iterdir()))

    def test_deadline_stops_before_network_or_retry(self):
        with self.assertRaises(TimeoutError):
            fetcher.fetch(self.item, self.cache, time.monotonic() - 1)
        self.assertEqual(0, self.calls)

    def test_cache_symlink_is_not_trusted_or_followed(self):
        victim = self.cache / 'victim'
        victim.write_bytes(BODY)
        target = self.cache / self.item['sha256']
        target.symlink_to(victim)
        self.fetch()
        self.assertFalse(target.is_symlink())
        self.assertEqual(BODY, victim.read_bytes())
        self.assertEqual(1, self.calls)

    def test_concurrent_downloads_atomically_publish_same_verified_identity(self):
        with ThreadPoolExecutor(max_workers=4) as pool:
            paths = list(pool.map(lambda _: self.fetch(), range(4)))
        self.assertEqual(1, len(set(paths)))
        self.assertEqual(BODY, paths[0].read_bytes())
        self.assertFalse(list(self.cache.glob('.download-*')))

    def test_preparation_stages_and_rechecks_only_locked_payloads(self):
        item = dict(self.item, destination='amd64/fixture.deb')
        destination = self.cache / 'staged'
        destination.mkdir()
        (destination / 'stale').write_text('must not survive')
        with patch.object(fetcher, 'inputs', return_value=[item]):
            fetcher.prepare('api', destination, self.cache / 'cache')
        self.assertEqual([destination / 'amd64'], list(destination.iterdir()))
        staged = destination / 'amd64/fixture.deb'
        self.assertEqual(BODY, staged.read_bytes())
        self.assertEqual(0o644, staged.stat().st_mode & 0o777)
        self.assertEqual(0o700, (self.cache / 'cache').stat().st_mode & 0o777)

    def test_preparation_rejects_a_symlink_cache(self):
        real = self.cache / 'real'
        real.mkdir()
        link = self.cache / 'link'
        link.symlink_to(real, target_is_directory=True)
        with patch.object(fetcher, 'inputs', return_value=[self.item]):
            with self.assertRaisesRegex(ValueError, 'cache is a symlink'):
                fetcher.prepare('api', cache=link)


class LockTest(unittest.TestCase):
    def test_all_services_and_both_archives_join_unchanged_lock(self):
        import json
        lock = json.loads(SCRIPT.with_name('runtime-packages.lock.json').read_text())
        all_inputs = fetcher.inputs(lock, '--all')
        self.assertEqual(107, len(all_inputs))
        self.assertEqual(72, len(fetcher.inputs(lock, 'taskworker')))
        selected = {item['id'] for role in lock['services'] for item in fetcher.inputs(lock, role)}
        self.assertEqual({item['id'] for item in all_inputs}, selected)


if __name__ == '__main__':
    unittest.main()

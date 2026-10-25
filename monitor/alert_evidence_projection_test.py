#!/usr/bin/python3
"""Deterministic offline regression controls for actual triage projection."""
import importlib.util
from pathlib import Path
import re
import unittest

BASE = Path(__file__).resolve().parent
SOURCE = BASE / 'alert_evidence_projection.py'
SPEC = importlib.util.spec_from_file_location('alert_evidence_projection', SOURCE)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)
STAMP = 'server/model.StampTopLevelClientContractTime'
GUIDANCE = 'For SQLSTATE 25006 at ' + STAMP + ', use the contained post-commit write discriminator.'


def alert(owner='server/controller.ConnectControlFrames', sqlstate='unknown', error_type='unknown'):
    return ('## [PAGE] synthetic retained Alert\n### Evidence\n'
            'sample: panic owner=' + owner + ' error_type=' + error_type + ' sqlstate=' + sqlstate
            + '\n### Action\n' + GUIDANCE + '\n### Verify\n' + GUIDANCE + '\n').encode()


class EvidenceProjectionControls(unittest.TestCase):
    def test_old_whole_object_predicate_RED(self):
        raw = alert()
        # Exact proven defective predicate class: whole-object token search.
        self.assertTrue('StampTopLevelClientContractTime' in raw.decode())
        self.assertIn('25006', re.findall(r'\b[0-9A-Z]{5}\b', raw.decode()))
        result = MODULE.project_alert(raw)
        self.assertFalse(MODULE.observed_match(result, STAMP, '25006'))

    def test_action_only_tokens_do_not_create_observation(self):
        result = MODULE.project_alert(('### Action\n' + GUIDANCE + '\n').encode())
        self.assertEqual(result['status'], 'cannot_observe')
        self.assertEqual(result['observed_function'], 'unknown')
        self.assertEqual(result['observed_sqlstate'], 'unknown')

    def test_Evidence_meaning_is_not_producer_sample(self):
        raw = ('### Evidence\nmeaning: ' + GUIDANCE
               + '\nmeaning: panic owner=' + STAMP
               + ' error_type=*pgconn.PgError sqlstate=25006\n'
               + 'panic owner=' + STAMP + ' error_type=*pgconn.PgError sqlstate=25006\n'
               + '### Action\n' + GUIDANCE + '\n').encode()
        result = MODULE.project_alert(raw)
        self.assertEqual(result['status'], 'cannot_observe')
        self.assertEqual(result['observed_sqlstate'], 'unknown')
        self.assertFalse(result['generated_meaning_used_as_observation'])

    def run_cli(self, path):
        import subprocess
        result = subprocess.run(['/usr/bin/python3', '-I', '-B', str(SOURCE), str(path)],
                                capture_output=True, timeout=3, check=False)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(result.stderr, b'')
        self.assertNotIn(b'SYNTHETIC_PRIVATE_MARKER', result.stdout)
        self.assertNotIn(str(path).encode(), result.stdout)
        projection = MODULE.json.loads(result.stdout)
        self.assertEqual(projection['status'], 'cannot_observe')
        self.assertEqual(projection['observed_sqlstate'], 'unknown')
        return projection

    def test_cli_invalid_utf8_is_fixed_private_failure(self):
        import tempfile
        with tempfile.TemporaryDirectory(dir=BASE) as folder:
            path = Path(folder) / 'SYNTHETIC_PRIVATE_MARKER.bin'
            path.write_bytes(b'SYNTHETIC_PRIVATE_MARKER' + bytes([255]))
            self.assertEqual(self.run_cli(path)['reason'], 'invalid_utf8')

    def test_cli_missing_input_is_fixed_private_failure(self):
        import tempfile
        with tempfile.TemporaryDirectory(dir=BASE) as folder:
            path = Path(folder) / 'SYNTHETIC_PRIVATE_MARKER.missing'
            self.assertEqual(self.run_cli(path)['reason'], 'input_unavailable')

    def test_cli_oversize_is_fixed_private_failure(self):
        import tempfile
        with tempfile.TemporaryDirectory(dir=BASE) as folder:
            path = Path(folder) / 'SYNTHETIC_PRIVATE_MARKER.large'
            path.write_bytes(b'SYNTHETIC_PRIVATE_MARKER' + b'x' * MODULE.MAX_ALERT_BYTES)
            self.assertEqual(self.run_cli(path)['reason'], 'invalid_or_oversize_input')

    def test_explicit_unknown_wins_over_guidance(self):
        result = MODULE.project_alert(alert(STAMP))
        self.assertTrue(MODULE.observed_match(result, STAMP))
        self.assertEqual(result['observed_error_type'], 'unknown')
        self.assertEqual(result['observed_sqlstate'], 'unknown')
        self.assertFalse(MODULE.observed_match(result, STAMP, '25006'))

    def test_real_Evidence_code_is_retained(self):
        result = MODULE.project_alert(alert(STAMP, '25006', '*pgconn.PgError'))
        self.assertTrue(MODULE.observed_match(result, STAMP, '25006'))
        self.assertFalse(result['shared_cause_established'])

    def test_different_functions_and_unknown_do_not_merge(self):
        first = MODULE.project_alert(alert())
        second = MODULE.project_alert(alert(STAMP))
        self.assertFalse(MODULE.same_known_observed_projection(first, second))
        self.assertFalse(MODULE.same_known_observed_projection(second, second))

    def test_non_model_public_owner_is_preserved(self):
        owner = 'server/session.validateByJwtStateRemembered'
        result = MODULE.project_alert(alert(owner))
        self.assertTrue(MODULE.observed_match(result, owner))

    def test_byte_provenance_only_points_inside_Evidence(self):
        raw = alert(STAMP)
        result = MODULE.project_alert(raw)
        provenance = result['producer_provenance']
        selected = raw[provenance['byte_start']:provenance['byte_end']]
        self.assertIn(b'sample: panic owner=', selected)
        self.assertNotIn(b'25006', selected)
        self.assertEqual(provenance['section'], 'Evidence')

    def test_duplicate_Evidence_fails_closed(self):
        raw = alert(STAMP) + alert(STAMP)
        self.assertEqual(MODULE.project_alert(raw)['status'], 'cannot_observe')



if __name__ == '__main__':
    unittest.main()

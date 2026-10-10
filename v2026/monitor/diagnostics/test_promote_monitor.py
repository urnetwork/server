import hashlib,json,pathlib,tempfile,unittest
from unittest import mock
import promote_monitor as p
from test_promotion_identity import OLD,NEW

class Promote(unittest.TestCase):
    def fixture(self,root):
        old=root/'monitor';old.write_bytes(b'old');candidate=root/'candidate';candidate.write_bytes(b'new');launcher=root/'launch.sh';launcher.write_bytes(b'launcher')
        unit=b'fixedunit';cadence={k:'unchanged' for k in ('last_attempted_at','last_terminal_at','next_eligible_at','last_completed_at')}
        plan={'old_authority':OLD,'candidate_path':str(candidate),'new_binary_sha256':hashlib.sha256(b'new').hexdigest(),'launcher_sha256':hashlib.sha256(b'launcher').hexdigest(),'unit_definition_sha256':hashlib.sha256(unit).hexdigest(),'new_source_commit':'c'*40}
        return old,launcher,plan,unit,cadence
    def exercise(self,fail_after=False,restart_failure=False):
        with tempfile.TemporaryDirectory() as d:
            root=pathlib.Path(d);old,launcher,plan,unit,cadence=self.fixture(root)
            replacement={'authority':NEW,'old_pid_retired':True}
            with mock.patch.object(p,'window'),mock.patch.object(p,'ACTIVE',old),mock.patch.object(p,'LAUNCHER',launcher),mock.patch.object(p,'preflight',return_value={'old':{'MainPID':'101'},'cadence_before':cadence}),mock.patch.object(p,'properties',return_value={'ActiveState':'active','MainPID':'202','NRestarts':'0'}),mock.patch.object(p,'state',return_value=cadence),mock.patch.object(p.subprocess,'check_output',return_value=unit),mock.patch.object(p.subprocess,'run',return_value=mock.Mock(returncode=1 if restart_failure else 0)) as restart,mock.patch.object(p.promotion_identity,'replacement',side_effect=PermissionError() if fail_after else None,return_value=replacement):
                first,path=p.execute(plan,root)
                self.assertTrue((root/'promotion-attempt.json').exists());self.assertEqual(restart.call_count,1)
                self.assertEqual(first['restart_exit'],1 if restart_failure else 0)
                self.assertEqual(first['completed'],not(fail_after or restart_failure))
                self.assertEqual(json.loads(path.read_text())['restart_attempted'],True)
                second,_=p.execute(plan,root)
                self.assertFalse(second['completed']);self.assertFalse(second['restart_attempted']);self.assertEqual(restart.call_count,1)
                self.assertEqual(json.loads(path.read_text()),first)
    def test_one_successful_same_unit_restart(self):self.exercise()
    def test_permission_after_success_retains_successful_restart_and_no_retry(self):self.exercise(fail_after=True)
    def test_unsuccessful_restart_never_retries(self):self.exercise(restart_failure=True)
    def test_preflight_refusal_never_restarts_or_installs(self):
        with tempfile.TemporaryDirectory() as d,mock.patch.object(p,'preflight',side_effect=PermissionError()),mock.patch.object(p.subprocess,'run',side_effect=AssertionError('restart')):
            rec,_=p.execute({},pathlib.Path(d));self.assertFalse(rec['restart_attempted']);self.assertFalse(rec['attempt_marker_written']);self.assertFalse((pathlib.Path(d)/'promotion-attempt.json').exists())
if __name__=='__main__':unittest.main()

import json,pathlib,tempfile,unittest
from unittest import mock
import process_identity,promotion_identity
OLD={'unit':process_identity.UNIT,'pid':101,'sha256':'a'*64,'start_ticks':20,'boot_id':'618def6d-48fb-46e3-b911-782480979dd7','n_restarts':0}
NEW=dict(OLD,pid=202,start_ticks=30,sha256='b'*64)
class Promotion(unittest.TestCase):
    def unit(self,**changes):
        d={'ActiveState':'active','SubState':'running','MainPID':'202','NRestarts':'0'};d.update(changes)
        return 0,''.join(k+'='+v+'\n' for k,v in d.items()).encode(),0
    def read(self,path,limit):
        if path==process_identity.BOOT_PATH:return OLD['boot_id'].encode()
        self.assertEqual(path,'/proc/202/stat')
        return b'202 (monitor) '+b' '.join([b'S',b'1']+[b'0']*17+[b'30'])
    def test_capture_uses_reviewed_exact_generation_verifier(self):
        with mock.patch.object(process_identity,'_run_bounded',return_value=self.unit()),mock.patch.object(process_identity,'_read',side_effect=self.read),mock.patch.object(process_identity,'verify',return_value={}) as v:
            self.assertEqual(promotion_identity.current('b'*64)[0],NEW);v.assert_called_once_with(NEW)
    def test_plain_proc_exe_permission_irrelevant(self):
        with mock.patch.object(process_identity,'_run_bounded',return_value=self.unit()),mock.patch.object(process_identity,'_read',side_effect=self.read),mock.patch.object(process_identity,'verify',return_value={}),mock.patch.object(pathlib.Path,'read_bytes',side_effect=PermissionError()):
            self.assertEqual(promotion_identity.current('b'*64)[0],NEW)
    def test_sudo_refusal_stays_unavailable(self):
        self.refused('sudo_or_hash_failed')
    def test_wrong_digest_stays_unavailable(self):self.refused('hash_mismatch')
    def test_generation_change_stays_unavailable(self):self.refused('generation_mismatch')
    def refused(self,cause):
        with mock.patch.object(process_identity,'_run_bounded',return_value=self.unit()),mock.patch.object(process_identity,'_read',side_effect=self.read),mock.patch.object(process_identity,'verify',side_effect=process_identity.AuditError(cause)):
            with self.assertRaises(process_identity.AuditError):promotion_identity.current('b'*64)
    def test_changed_unit_rejects(self):
        with mock.patch.object(process_identity,'_run_bounded',return_value=self.unit(NRestarts='1')):
            with self.assertRaises(process_identity.AuditError):promotion_identity.current('b'*64)
    def test_new_generation_old_absence(self):
        with mock.patch.object(promotion_identity,'current',return_value=(NEW,{})),mock.patch.object(pathlib.Path,'stat',side_effect=FileNotFoundError()):self.assertTrue(promotion_identity.replacement(OLD,'b'*64)['old_pid_retired'])
    def test_same_pid_rejects(self):
        with mock.patch.object(promotion_identity,'current',return_value=(OLD,{})):
            with self.assertRaises(process_identity.AuditError):promotion_identity.replacement(OLD,'b'*64)
    def test_old_still_present_rejects(self):
        with mock.patch.object(promotion_identity,'current',return_value=(NEW,{})),mock.patch.object(pathlib.Path,'stat',return_value=object()):
            with self.assertRaises(process_identity.AuditError):promotion_identity.replacement(OLD,'b'*64)
    def test_old_unreadable_not_retired(self):
        with mock.patch.object(promotion_identity,'current',return_value=(NEW,{})),mock.patch.object(pathlib.Path,'stat',side_effect=PermissionError()):
            with self.assertRaises(process_identity.AuditError):promotion_identity.replacement(OLD,'b'*64)
    def test_other_boot_rejects(self):
        with mock.patch.object(promotion_identity,'current',return_value=(dict(NEW,boot_id='0'*36),{})):
            with self.assertRaises(process_identity.AuditError):promotion_identity.replacement(OLD,'b'*64)
    def test_durable_marker_blocks_second_restart_attempt(self):
        with tempfile.TemporaryDirectory() as d:
            promotion_identity.begin_once(d,OLD,'b'*64);p=pathlib.Path(d)/'promotion-attempt.json';first=p.read_bytes()
            with self.assertRaises(FileExistsError):promotion_identity.begin_once(d,OLD,'b'*64)
            self.assertEqual(p.read_bytes(),first);self.assertFalse(json.loads(first)['automatic_retry']);self.assertEqual(p.stat().st_mode&0o777,0o600)
    def test_invalid_authority_does_not_spend_marker(self):
        with tempfile.TemporaryDirectory() as d:
            with self.assertRaises(process_identity.AuditError):promotion_identity.begin_once(d,dict(OLD,n_restarts=1),'b'*64)
            self.assertFalse(list(pathlib.Path(d).iterdir()))
    def test_marker_symlink_rejected(self):
        with tempfile.TemporaryDirectory() as d:
            p=pathlib.Path(d);(p/'target').write_text('keep');(p/'promotion-attempt.json').symlink_to(p/'target')
            with self.assertRaises(FileExistsError):promotion_identity.begin_once(d,OLD,'b'*64)
            self.assertEqual((p/'target').read_text(),'keep')
if __name__=='__main__':unittest.main()

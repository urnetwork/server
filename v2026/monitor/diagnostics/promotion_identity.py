"""Read-only generation verification and durable one-attempt promotion fence.

No function starts, stops, restarts, or signals a process. A verification
failure after restart requires read-only recovery, never another restart.
"""
import datetime
import json
import os
import pathlib
import re
import time
import process_identity


def current(expected_sha256):
    """Capture only the generation owned by the fixed monitor unit, then verify."""
    if not isinstance(expected_sha256,str) or not re.fullmatch(r'[a-f0-9]{64}',expected_sha256):
        raise process_identity.AuditError('invalid_authority')
    code,out,stderr_bytes=process_identity._run_bounded([
        '/usr/bin/systemctl','--user','show',process_identity.UNIT,
        '--property=ActiveState,SubState,MainPID,NRestarts'],time.monotonic()+1)
    if code!=0 or stderr_bytes: raise process_identity.AuditError('command_failed')
    try:
        pairs=[line.split('=',1) for line in out.decode('ascii').splitlines()]
        state=dict(pairs)
        if len(state)!=len(pairs) or set(state)!={'ActiveState','SubState','MainPID','NRestarts'}: raise ValueError()
        if state['ActiveState']!='active' or state['SubState']!='running' or state['NRestarts']!='0': raise ValueError()
        pid=int(state['MainPID'])
        if not 1<=pid<=4194304: raise ValueError()
        raw=process_identity._read(f'/proc/{pid}/stat',8192)
        end=raw.rfind(b')'); fields=raw[end+2:].split()
        if end<0 or int(raw[:raw.index(b' (')])!=pid or len(fields)<20: raise ValueError()
        ticks=int(fields[19]); boot=process_identity._read(process_identity.BOOT_PATH,64).decode('ascii').strip()
    except (ValueError,UnicodeError,IndexError):
        raise process_identity.AuditError('unit_invalid') from None
    authority={'unit':process_identity.UNIT,'pid':pid,'start_ticks':ticks,'boot_id':boot,'sha256':expected_sha256,'n_restarts':0}
    # Capture is not authority. The reviewed helper binds this exact generation
    # on both sides of its bounded, noninteractive privileged proc-exe hash.
    return authority,process_identity.verify(authority)


def replacement(old,expected_sha256):
    """Verify one replacement and predecessor absence without guessing a PID."""
    process_identity.validate_authority(old)
    authority,audit=current(expected_sha256)
    if authority['pid']==old['pid'] or authority['boot_id']!=old['boot_id']:
        raise process_identity.AuditError('generation_mismatch')
    try: pathlib.Path(f"/proc/{old['pid']}").stat()
    except FileNotFoundError: pass
    except OSError: raise process_identity.AuditError('proc_unavailable') from None
    else: raise process_identity.AuditError('generation_mismatch')
    return {'authority':authority,'audit':audit,'old_pid_retired':True}


def begin_once(directory,old,new_sha256):
    """Persist intent before replacement/restart; a spent marker cannot reopen."""
    process_identity.validate_authority(old)
    if not isinstance(new_sha256,str) or not re.fullmatch(r'[a-f0-9]{64}',new_sha256):
        raise process_identity.AuditError('invalid_authority')
    directory=pathlib.Path(directory)
    record={'schema':1,'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),
            'old_authority':old,'new_binary_sha256':new_sha256,'single_restart_attempt':True,
            'automatic_retry':False,'recovery':'A spent marker requires read-only reconciliation; never rerun to repair verification.'}
    data=(json.dumps(record,sort_keys=True,separators=(',',':'))+'\n').encode()
    fd=os.open(directory/'promotion-attempt.json',os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as f: f.write(data); f.flush(); os.fsync(f.fileno())
    fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    return record

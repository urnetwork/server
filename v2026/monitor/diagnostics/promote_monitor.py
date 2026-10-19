"""One explicitly authorized same-unit monitor replacement; never retries.

Stage this file and both identity helpers beside a reviewed plan.json. --check
is read-only. A failed --execute leaves an immutable attempt marker and receipt;
recover by observation, not by executing again or launching another watcher.
"""
import datetime
import hashlib
import json
import os
import pathlib
import signal
import subprocess
import sys
import time
import process_identity
import promotion_identity

UNIT=process_identity.UNIT
ACTIVE=pathlib.Path('/home/by/urnetwork/monitor/server-monitor.cap2-enabled-20260929T2232Z/monitor')
LAUNCHER=ACTIVE.parent/'launch.sh'
STATE=pathlib.Path('/home/by/.urnetwork-monitor/main/pg-query-sample/continuous.json')


def sha(path):return hashlib.sha256(pathlib.Path(path).read_bytes()).hexdigest()
def utc():return datetime.datetime.now(datetime.timezone.utc).isoformat()


def save(path,record):
    data=(json.dumps(record,sort_keys=True,indent=2)+'\n').encode()
    if len(data)>65536:raise ValueError('receipt_cap')
    fd=os.open(path,os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())
    fd=os.open(pathlib.Path(path).parent,os.O_RDONLY|os.O_DIRECTORY)
    try:os.fsync(fd)
    finally:os.close(fd)


def properties():
    raw=subprocess.check_output(['/usr/bin/systemctl','--user','show',UNIT,'--property=ActiveState,MainPID,NRestarts,KillMode,TimeoutStopUSec'],timeout=5)
    if len(raw)>4096:raise ValueError('unit_cap')
    pairs=[line.split('=',1) for line in raw.decode('ascii').splitlines()];state=dict(pairs)
    if len(state)!=len(pairs) or set(state)!={'ActiveState','MainPID','NRestarts','KillMode','TimeoutStopUSec'}:raise ValueError('unit_schema')
    return state


def state():
    raw=STATE.read_bytes()
    if len(raw)>65536:raise ValueError('state_cap')
    return json.loads(raw)


def window(plan):
    if plan['unit']!=UNIT or plan['authorized'] is not True:raise ValueError('authority')
    now=datetime.datetime.now(datetime.timezone.utc)
    start=datetime.datetime.fromisoformat(plan['start_not_before']);end=datetime.datetime.fromisoformat(plan['start_before'])
    if not start.tzinfo or not end.tzinfo or not datetime.timedelta(0)<end-start<=datetime.timedelta(minutes=10) or not start<=now<end:raise ValueError('window')


def preflight(plan,directory):
    window(plan)
    if set(plan['source_files'])!={'promote.py','promotion_identity.py','process_identity.py'}:raise ValueError('source_scope')
    for name,value in plan['source_files'].items():
        if pathlib.Path(name).name!=name or sha(directory/name)!=value:raise ValueError('source_changed')
    if sha(plan['candidate_path'])!=plan['new_binary_sha256'] or sha(ACTIVE)!=plan['old_authority']['sha256'] or sha(LAUNCHER)!=plan['launcher_sha256']:raise ValueError('artifact_changed')
    unit=subprocess.check_output(['/usr/bin/systemctl','--user','cat',UNIT],timeout=5)
    if hashlib.sha256(unit).hexdigest()!=plan['unit_definition_sha256']:raise ValueError('unit_changed')
    audit=process_identity.verify(plan['old_authority'])
    before=properties()
    if before['ActiveState']!='active' or before['MainPID']!=str(plan['old_authority']['pid']) or before['NRestarts']!='0':raise ValueError('unit_identity')
    cadence=state()
    if cadence!=plan['cadence']:raise ValueError('cadence_changed')
    return {'old':before,'old_audit':audit,'cadence_before':cadence}


def execute(plan,directory):
    os.umask(0o077);record={'started_utc':utc(),'completed':False,'restart_attempted':False,'restart_exit':None,'automatic_retry':False,'successful_sampler_claim':False,'overlap_claim':False}
    phase='preflight';spent=False
    try:
        record.update(preflight(plan,directory))
        phase='durable_attempt_marker'
        window(plan)
        promotion_identity.begin_once(directory,plan['old_authority'],plan['new_binary_sha256']);spent=True
        phase='atomic_binary_replacement'
        temp=ACTIVE.parent/'monitor.reviewed-promotion-candidate'
        with temp.open('xb') as f:
            f.write(pathlib.Path(plan['candidate_path']).read_bytes());f.flush();os.fsync(f.fileno())
        temp.chmod(0o700)
        if sha(temp)!=plan['new_binary_sha256']:raise ValueError('candidate_changed')
        os.replace(temp,ACTIVE)
        fd=os.open(ACTIVE.parent,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
        phase='single_restart';record['restart_attempted']=True
        with (directory/'restart.log').open('xb') as out:
            result=subprocess.run(['/usr/bin/systemctl','--user','restart',UNIT],stdout=out,stderr=subprocess.STDOUT,timeout=110)
        record['restart_exit']=result.returncode
        if result.returncode!=0:raise ValueError('restart_exit')
        phase='new_generation_verification'
        for _ in range(10):
            after=properties()
            if after['ActiveState']=='active' and after['MainPID'] not in ('0',str(plan['old_authority']['pid'])):break
            time.sleep(1)
        record['replacement']=promotion_identity.replacement(plan['old_authority'],plan['new_binary_sha256'])
        if sha(LAUNCHER)!=plan['launcher_sha256']:raise ValueError('launcher_changed')
        unit=subprocess.check_output(['/usr/bin/systemctl','--user','cat',UNIT],timeout=5)
        if hashlib.sha256(unit).hexdigest()!=plan['unit_definition_sha256']:raise ValueError('unit_changed')
        record['cadence_after']=state()
        for key in ('last_attempted_at','last_terminal_at','next_eligible_at','last_completed_at'):
            if record['cadence_before'][key]!=record['cadence_after'][key]:raise ValueError('cadence_changed')
        record.update(completed=True,new=after,new_binary_sha256=plan['new_binary_sha256'],source_commit=plan['new_source_commit'],first_active_startup_delay_seconds=900,first_actual_callback_unproven=True)
    except Exception as error:
        record['failure_class']=getattr(error,'cause',type(error).__name__)
        record['recovery']='Read-only reconciliation required. Do not rerun, roll back, or restart automatically.'
    finally:
        record['phase']=phase;record['attempt_marker_written']=spent;record['completed_utc']=utc()
        name='promotion-receipt.json' if spent else 'preflight-hold-'+datetime.datetime.now(datetime.timezone.utc).strftime('%Y%m%dT%H%M%S.%fZ')+'.json'
        path=directory/name;save(path,record)
    return record,path


def main():
    if sys.argv[1:] not in (['--check'],['--execute']):raise SystemExit(2)
    directory=pathlib.Path(__file__).resolve().parent;plan=json.loads((directory/'plan.json').read_text())
    def deadline(*_):raise TimeoutError('promotion_owner_timeout')
    signal.signal(signal.SIGALRM,deadline);signal.alarm(150)
    try:
        if sys.argv[1:]==['--check']:
            preflight(plan,directory);print(json.dumps({'source_preflight':True,'restart_attempted':False}));return 0
        record,path=execute(plan,directory)
        print(json.dumps({'complete':record['completed'],'restart_attempted':record['restart_attempted'],'restart_exit':record['restart_exit'],'receipt':str(path),'sha256':sha(path)}))
        return 0 if record['completed'] else 1
    finally:signal.alarm(0)
if __name__=='__main__':raise SystemExit(main())

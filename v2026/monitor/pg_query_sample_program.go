package monitor

// One subprocess and one absolute owner budget. Stderr is drained concurrently
// but only its first4KiB is kept in remote memory for finite classification.
// A psql-owned source line maps to our fixed SQL phase comments; it never
// exports that line number, text, SQL, credentials or addresses. No retries.
const pgQuerySampleProgram = `import json,os,re,selectors,subprocess,sys,time
p=None
phase='bootstrap'
stderr=bytearray()
stderr_truncated=False
sql=''
class Failure(Exception):
 def __init__(self,phase,cause):self.phase=phase;self.cause=cause
def sql_phase(raw):
 match=re.search(r'psql:(?:<stdin>|-):([0-9]+):',raw)
 if not match:return 'child_process'
 number=int(match.group(1));selected='child_process'
 phases={'identity','authority','history_start','sample_wait','activity','blockers','history_end'}
 for i,line in enumerate(sql.splitlines(),1):
  if i>number:break
  prefix='-- monitor_query_sample_phase:'
  if line.startswith(prefix) and line[len(prefix):] in phases:selected=line[len(prefix):]
 return selected
def child_failure():
 raw=stderr.decode('utf-8','replace');lower=raw.lower();stage=sql_phase(raw)
 if stage in {'history_start','history_end'} and 'pg_stat_statements' in lower and ('does not exist' in lower or 'must be loaded via shared_preload_libraries' in lower):return stage,'pgss_unavailable'
 causes=[('sample authority mismatch','authority_mismatch'),('statement timeout','statement_timeout'),('lock timeout','lock_timeout'),('password authentication failed','authentication'),('no pg_hba.conf entry','pg_hba'),('connection refused','connection_refused'),('timeout expired','connection_timeout'),('could not translate host name','name_resolution'),('network is unreachable','network_unreachable'),('no route to host','network_unreachable'),('certificate verify failed','tls'),('ssl error','tls'),('permission denied','permission_denied')]
 for text,cause in causes:
  if text in lower:
   if stage=='child_process' and cause in {'authentication','pg_hba','connection_refused','connection_timeout','name_resolution','network_unreachable','tls'}:stage='connection'
   return stage,cause
 if re.search(r'database .+ does not exist',lower):return 'connection','database_missing'
 if stage!='child_process':
  if 'does not exist' in lower or 'undefined' in lower:return stage,'schema_mismatch'
  return stage,'sql_error'
 return stage,'child_exit_unknown'
try:
 cfg=json.loads(sys.stdin.buffer.read(262145));assert set(cfg)=={'password','user','database','port','sql'}
 assert all(type(cfg[k]) is str for k in ('password','user','database','sql')) and type(cfg['port']) is int
 assert 0<len(cfg['sql'])<200000 and 0<cfg['port']<65536
 sql=cfg['sql']
 env=dict(os.environ,PGPASSWORD=cfg['password'],PGCONNECT_TIMEOUT='3',PGOPTIONS='-c statement_timeout=3000 -c lock_timeout=250 -c default_transaction_read_only=on -c idle_in_transaction_session_timeout=3000',LC_ALL='C')
 deadline=time.monotonic()+32
 phase='child_start'
 p=subprocess.Popen(['psql','-X','-q','-A','-t','-h','localhost','-p',str(cfg['port']),'-U',cfg['user'],'-d',cfg['database'],'-v','ON_ERROR_STOP=1','-f','-'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE,env=env)
 payload=memoryview(sql.encode());output=bytearray();sel=selectors.DefaultSelector();broken_stdin=False
 for f in (p.stdin,p.stdout,p.stderr):os.set_blocking(f.fileno(),False)
 sel.register(p.stdin,selectors.EVENT_WRITE);sel.register(p.stdout,selectors.EVENT_READ);sel.register(p.stderr,selectors.EVENT_READ)
 phase='child_process'
 while sel.get_map():
  left=deadline-time.monotonic()
  if left<=0:raise Failure('child_process','owner_deadline')
  for key,event in sel.select(min(left,0.2)):
   if key.fileobj is p.stdin:
    try:n=os.write(p.stdin.fileno(),payload[:65536]);payload=payload[n:]
    except BrokenPipeError:broken_stdin=True;payload=memoryview(b'')
    if not payload:sel.unregister(p.stdin);p.stdin.close()
   else:
    chunk=os.read(key.fileobj.fileno(),65536)
    if not chunk:sel.unregister(key.fileobj);key.fileobj.close()
    elif key.fileobj is p.stderr:
     room=4096-len(stderr)
     if len(chunk)>room:stderr_truncated=True
     stderr.extend(chunk[:room])
    else:
     output.extend(chunk)
     if len(output)>4194304:raise Failure('output','output_cap')
 status=p.wait(timeout=max(.001,deadline-time.monotonic()))
 if status!=0:raise Failure(*child_failure())
 if broken_stdin:raise Failure('stdin','broken_pipe')
 sys.stdout.buffer.write(output)
except BaseException as err:
 if p is not None and p.poll() is None:p.kill();p.wait()
 if isinstance(err,Failure):phase,cause=err.phase,err.cause
 elif phase=='bootstrap':cause='invalid_input'
 elif isinstance(err,FileNotFoundError):cause='missing_executable'
 elif isinstance(err,PermissionError):cause='permission_denied'
 elif isinstance(err,(TimeoutError,subprocess.TimeoutExpired)):cause='owner_deadline'
 elif isinstance(err,OSError):cause='io_error'
 else:cause='adapter_error'
 sys.stdout.write(json.dumps({'kind':'source_failure','schema':1,'phase':phase,'cause':cause,'stderr_truncated':stderr_truncated},separators=(',',':'))+'\n')
 sys.exit(76)
`

"""Execute one source-bound production Node/client cohort in a sealed container."""
import hashlib,json,os,pathlib,selectors,shutil,signal,stat,subprocess,sys,threading,time
ROOT=pathlib.Path(__file__).resolve().parents[1]
LOCK=json.loads((ROOT/'tools/connected-node-check/image.lock.json').read_bytes())
IMAGE=LOCK['repository']+'@'+LOCK['platformManifestDigest']
COMPLETE='CONNECTED-NODE-COMPLETE profiles=4 connectedBlocks=32 exactPlannedTransactions=64 completeStateRoots=32 indexedBlocks=32 persistedNodeAndClientRestarts=4 invalidPayloadRefusals=32 productionAccepted=false'

def bounded(argv,cap=128<<10,timeout=90):
    child=subprocess.Popen(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,start_new_session=True)
    output=bytearray();selector=selectors.DefaultSelector();selector.register(child.stdout,selectors.EVENT_READ)
    deadline=time.monotonic()+timeout
    try:
        while selector.get_map():
            assert time.monotonic()<deadline,'controller command deadline'
            for key,_ in selector.select(.1):
                chunk=os.read(key.fileobj.fileno(),8192)
                if not chunk:selector.unregister(key.fileobj);continue
                assert len(output)+len(chunk)<=cap,'controller command output limit'
                output.extend(chunk)
        code=child.wait(timeout=max(.1,deadline-time.monotonic()))
        sys.stdout.buffer.write(output);sys.stdout.buffer.flush();assert code==0,('controller command exit',code)
        return bytes(output)
    finally:
        selector.close()
        if child.poll() is None:os.killpg(child.pid,signal.SIGKILL);child.wait(timeout=15)

def binary_pin(path):
    path=pathlib.Path(path).resolve(strict=True);item=path.lstat()
    assert stat.S_ISREG(item.st_mode) and 0<item.st_size<=256<<20
    h=hashlib.sha256()
    with path.open('rb') as f:
        while chunk:=f.read(1<<20):h.update(chunk)
    return path,{'bytes':item.st_size,'sha256':h.hexdigest()}

assert len(sys.argv)==3
root=pathlib.Path(os.environ['RUNNER_TEMP'])/'connected-node-qualification';root.mkdir(mode=0o700)
assert shutil.disk_usage(root).free>=12<<30
head=os.environ['GITHUB_SHA'];assert len(head)==40 and set(head)<=set('0123456789abcdef')
assert bounded(['git','rev-parse','HEAD']).decode().strip()==head
assert bounded(['git','-C','zvm-core','rev-parse','HEAD']).decode().strip()=='acf39556a3f465a6885f23fc3d0cc8065891d76c'
bounded(['git','diff','--exit-code']);bounded(['git','-C','zvm-core','diff','--exit-code'])
bounded(['docker','pull','--platform','linux/amd64',IMAGE],timeout=120)
details=json.loads(bounded(['docker','image','inspect',IMAGE]));assert len(details)==1;details=details[0]
assert details['Id']==LOCK['configurationDigest'] and details['Architecture']=='amd64' and details['Os']=='linux'
assert details['RootFS']['Layers']==LOCK['uncompressedLayerDigests'] and details['Config']['Entrypoint']==['/usr/local/bin/op-reth']
print('CONNECTED-NODE-IMAGE '+json.dumps({'image':IMAGE,'configurationDigest':details['Id'],'platform':'linux/amd64',
    'compressedLayerBytes':LOCK['compressedLayerBytes'],'sourceCommitDeclaredByRelease':LOCK['sourceCommit'],'independentBuildProvenance':False},sort_keys=True),flush=True)
driver,driver_pin=binary_pin(sys.argv[1]);node,node_pin=binary_pin(sys.argv[2])
print('CONNECTED-NODE-DRIVER '+json.dumps(driver_pin,sort_keys=True),flush=True)
print('CONNECTED-NODE-PRODUCTION-BINARY '+json.dumps({**node_pin,'source':head},sort_keys=True),flush=True)
data=root/'data';data.mkdir(mode=0o700)
name='zvm-connected-node-'+os.environ['GITHUB_RUN_ID']+'-'+os.environ['GITHUB_RUN_ATTEMPT']
argv=['docker','run','--name',name,'--user',str(os.getuid())+':'+str(os.getgid()),'--network','none','--read-only','--cap-drop','ALL',
    '--security-opt','no-new-privileges','--pids-limit','256','--cpus','2','--memory','4g','--memory-swap','4g','--tmpfs','/tmp:rw,nosuid,nodev,size=256m',
    '--mount','type=bind,src='+str(ROOT/'zvm-core/chains')+',dst=/source/chains,readonly',
    '--mount','type=bind,src='+str(driver)+',dst=/qualification,readonly','--mount','type=bind,src='+str(node)+',dst=/qualified-node,readonly',
    '--mount','type=bind,src='+str(data)+',dst=/data','--workdir','/source','--env','GOMAXPROCS=2',
    '--env','XDG_DATA_HOME=/data/state','--env','XDG_CONFIG_HOME=/data/config','--env','XDG_CACHE_HOME=/data/cache',
    '--entrypoint','/qualification',IMAGE,'/data/chains',head]
process=subprocess.Popen(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.STDOUT)
captured=bytearray();failures=[]
def read():
    try:
        while True:
            chunk=process.stdout.read(4096)
            if not chunk:break
            if len(captured)+len(chunk)>1<<20:raise RuntimeError('production Node/client output limit')
            captured.extend(chunk);sys.stdout.buffer.write(chunk);sys.stdout.buffer.flush()
    except BaseException as error:failures.append(str(error))
reader=threading.Thread(target=read,daemon=True);reader.start();deadline=time.monotonic()+420
maximum_sampled_allocated=0;minimum_sampled_free=shutil.disk_usage(root).free
data_identity=(data.lstat().st_dev,data.lstat().st_ino);unstable_since=None;unstable_samples=0
def scan_error(error):raise error
try:
    while process.poll() is None:
        assert not failures,failures
        assert time.monotonic()<deadline,'production Node/client wall deadline'
        observed_free=shutil.disk_usage(root).free;minimum_sampled_free=min(minimum_sampled_free,observed_free)
        assert observed_free>=10<<30,'hosted free-space floor'
        info=data.lstat();assert stat.S_ISDIR(info.st_mode) and (info.st_dev,info.st_ino)==data_identity,'generated-data root identity changed'
        allocated=0
        try:
            for directory,dirs,names in os.walk(data,followlinks=False,onerror=scan_error):
                for part in dirs:assert stat.S_ISDIR((pathlib.Path(directory)/part).lstat().st_mode),'unexpected generated-data directory type'
                for part in names:
                    item=(pathlib.Path(directory)/part).lstat();assert stat.S_ISREG(item.st_mode),'unexpected generated-data file type'
                    allocated+=item.st_blocks*512
        except FileNotFoundError:
            unstable_samples+=1
            if unstable_since is None:unstable_since=time.monotonic()
            assert time.monotonic()-unstable_since<1,'generated-data scan remained unstable'
        else:
            unstable_since=None
            assert allocated<=512<<20,'generated Node/client allocation limit'
            maximum_sampled_allocated=max(maximum_sampled_allocated,allocated)
        time.sleep(.5)
    reader.join(timeout=5);assert not reader.is_alive() and not failures
    assert process.returncode==0,('actual production Node/client exit',process.returncode)
    assert bytes(captured).decode().count(COMPLETE)==1,'incomplete production Node/client cohort'
    assert bytes(captured).decode().count('CONSTRUCTOR-NODE-COMPLETE profiles=4 connectedBlocks=32 exactPlannedTransactions=64 completeStateRoots=32 indexedBlocks=32 persistedNodeAndClientRestarts=4 invalidPayloadRefusals=32 checkpoints=12 anchoredConstructorAccounts=48 statusSlotReads=12 getters=36 productionAccepted=false')==1,'incomplete constructor-state production Node cohort'
    print('CONNECTED-NODE-TRANSPORT '+json.dumps({'actualExit':process.returncode,'complete':True,'bytes':len(captured),
        'sha256':hashlib.sha256(captured).hexdigest(),'network':'none','positiveCohortExecutions':1,'incompleteDataSamplesNotUsedForSizeClaims':unstable_samples},sort_keys=True),flush=True)
    print('CONSTRUCTOR-NODE-RESOURCE '+json.dumps({'profileCohorts':2,'profiles':8,'maximumSampledGeneratedDataAllocatedBytes':maximum_sampled_allocated,
        'generatedDataAllocationCapBytes':512<<20,'minimumSampledFreeBytes':minimum_sampled_free,'minimumFreeBytes':10<<30,
        'sampleIntervalMilliseconds':500,'OSAggregateDiskQuotaClaimed':False,'containerPositiveExecutions':1,'driverBuilds':1,
        'productionNodeBuildsInThisJob':1,'incompleteDataSamplesNotUsedForSizeClaims':unstable_samples,'historicalOrLocalDataDeleted':False},sort_keys=True),flush=True)
finally:
    if process.poll() is None:
        subprocess.run(['docker','kill',name],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=20);process.wait(timeout=20)
    # Retain this ephemeral hosted container/data for the actual job's diagnostics.

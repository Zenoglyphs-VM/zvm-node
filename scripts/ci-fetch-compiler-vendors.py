#!/usr/bin/env python3
"""Fetch three selected vendor binaries once in owned CI scratch, with finite transport and source pins."""
import hashlib,json,os,re,signal,sys,time,urllib.request
from contextlib import contextmanager
from pathlib import Path

MAX_METADATA=256<<10
MAX_BINARY=32<<20
MAX_SECONDS=60
SOURCES=['https://binaries.soliditylang.org/linux-amd64/list.json',
         'https://raw.githubusercontent.com/ethereum/solc-bin/gh-pages/linux-amd64/list.json']
BASE='https://binaries.soliditylang.org/linux-amd64/'
VERSIONS=['0.5.16+commit.9c3226ce','0.6.6+commit.6c089d02','0.8.30+commit.73712a01']

class Refused(Exception):pass
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self,*args,**kwargs):raise Refused('redirect')
@contextmanager
def transport_deadline():
    if signal.getitimer(signal.ITIMER_REAL)!=(0.0,0.0):raise Refused('existing-timer')
    old=signal.getsignal(signal.SIGALRM)
    def expired(*args):raise Refused('elapsed-cap')
    signal.signal(signal.SIGALRM,expired)
    try:
        signal.setitimer(signal.ITIMER_REAL,MAX_SECONDS)
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL,0)
        signal.signal(signal.SIGALRM,old)
def unique(pairs):
    result={}
    for key,value in pairs:
        if key in result:raise Refused('duplicate-key')
        result[key]=value
    return result
def selected(raw):
    if not 0<len(raw)<=MAX_METADATA:raise Refused('manifest-size')
    rows=json.loads(raw,object_pairs_hook=unique)
    if not isinstance(rows,list) or len(rows)!=3:raise Refused('manifest-count')
    for number,row in enumerate(rows):
        if not isinstance(row,dict) or set(row)!={'path','version','longVersion','sha256'}:raise Refused('manifest-schema')
        if row['longVersion']!=VERSIONS[number] or row['version']!=VERSIONS[number].split('+')[0] or row['path']!='solc-linux-amd64-v'+row['longVersion']:raise Refused('manifest-identity')
        if not isinstance(row['sha256'],str) or not re.fullmatch('[0-9a-f]{64}',row['sha256']):raise Refused('manifest-digest')
    return rows
def copy_bounded(response,output,maximum,expected=None,clock=time.monotonic):
    if response.status!=200:raise Refused('HTTP-status')
    length=response.headers.get('Content-Length');declared=None
    if length is not None:
        if not re.fullmatch('[0-9]{1,10}',length):raise Refused('HTTP-length')
        declared=int(length)
        if not 0<declared<=maximum:raise Refused('HTTP-length-cap')
    count=0;h=hashlib.sha256();deadline=clock()+MAX_SECONDS
    while True:
        if clock()>deadline:raise Refused('elapsed-cap')
        block=response.read(min(64<<10,maximum-count+1))
        if not block:break
        if count+len(block)>maximum:raise Refused('stream-cap')
        output.write(block);count+=len(block);h.update(block)
    if count==0 or (declared is not None and declared!=count):raise Refused('incomplete-stream')
    if expected is not None and h.hexdigest()!=expected:raise Refused('digest-mismatch')
    return {'bytes':count,'sha256':h.hexdigest(),'actual_exit_complete':True}
def fetch(url,output,maximum,expected=None):
    if url not in SOURCES and not any(url==BASE+'solc-linux-amd64-v'+version for version in VERSIONS):raise Refused('URL-scope')
    opener=urllib.request.build_opener(NoRedirect())
    with transport_deadline():
        with opener.open(urllib.request.Request(url,headers={'User-Agent':'ZVM-compiler-qualification'}),timeout=15) as response:
            if response.geturl()!=url:raise Refused('response-URL')
            return copy_bounded(response,output,maximum,expected)
def verify_metadata(raw,rows):
    if not 0<len(raw)<=MAX_METADATA:raise Refused('metadata-size')
    actual=json.loads(raw,object_pairs_hook=unique)
    for row in rows:
        candidates=[item for item in actual['builds'] if item['longVersion']==row['longVersion']]
        if len(candidates)!=1:raise Refused('metadata-count')
        item=candidates[0]
        if any(item[key]!=row[key] for key in ['path','version','longVersion']) or item['sha256']!='0x'+row['sha256']:raise Refused('metadata-pin')
def main(args):
    if len(args)!=2:raise Refused('arguments')
    manifest=Path(__file__).resolve().parent.parent/'cmd/zvm-node/testdata/compiler_vendor_builds.json'
    rows=selected(manifest.read_bytes());destination=Path(args[1]);destination.mkdir(mode=0o700)
    import io
    proofs=[]
    for url in SOURCES:
        output=io.BytesIO();proof=fetch(url,output,MAX_METADATA);verify_metadata(output.getvalue(),rows)
        proofs.append({'source':url,**proof})
    print('N01_VENDOR_METADATA '+json.dumps({'sources':proofs,'selected_versions':VERSIONS,'same_publisher_metadata_not_independent_authority':True},sort_keys=True),flush=True)
    for row in rows:
        path=destination/('solc-'+row['version'])
        fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        with os.fdopen(fd,'wb') as output:proof=fetch(BASE+row['path'],output,MAX_BINARY,row['sha256'])
        with path.open('rb') as output:header=output.read(20)
        if len(header)!=20 or header[:6]!=b'\x7fELF\x02\x01' or header[18:20]!=b'\x3e\x00':raise Refused('ELF-platform')
        path.chmod(0o555)
        print('N01_VENDOR_TRANSPORT '+json.dumps({'version':row['version'],'long_version':row['longVersion'],**proof,'transport_attempts':1,'maximum_binary_bytes':MAX_BINARY,'maximum_elapsed_seconds':MAX_SECONDS,'automatic_retry_or_fallback':False},sort_keys=True),flush=True)
if __name__=='__main__':
    try:main(sys.argv)
    except Exception as error:
        print('N01_VENDOR_FETCH_FAILURE '+json.dumps({'type':type(error).__name__,'code':str(error) if isinstance(error,Refused) else 'transport-or-metadata-error','partial_outputs_preserved':True},sort_keys=True),file=sys.stderr)
        raise SystemExit(1)

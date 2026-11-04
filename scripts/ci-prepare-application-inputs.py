#!/usr/bin/env python3
"""Build complete literal compiler workloads from exact source archives, without installation or extraction."""
import base64
import gzip
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path, PurePosixPath
import re
import stat
import sys
import tarfile
import urllib.request

MAX_ARCHIVE=1<<20
MAX_EXPANDED=32<<20
MAX_MEMBERS=4096
MAX_BUNDLE=8<<20
GROUPS=('apps','bench','wznn','core','periphery')
PACKAGE_IDS={'openzeppelin','core','periphery','periphery-core','lib'}

class Refused(ValueError):pass
def sha(raw):return hashlib.sha256(raw).hexdigest()
def canonical(value):return json.dumps(value,ensure_ascii=False,sort_keys=True,separators=(',',':')).encode()
def unique(pairs):
    result={}
    for key,value in pairs:
        if key in result:raise Refused('duplicate-key')
        result[key]=value
    return result
def document(raw,maximum=MAX_BUNDLE):
    if not isinstance(raw,bytes) or not 0<len(raw)<=maximum:raise Refused('document-size')
    try:return json.loads(raw,object_pairs_hook=unique)
    except (ValueError,UnicodeError,RecursionError) as error:raise Refused('document-json') from error
def safe_path(name):
    if not isinstance(name,str) or not name or len(name)>240 or not re.fullmatch(r'[A-Za-z0-9_@+./-]+',name):raise Refused('path-name')
    if name.startswith('/') or any(part in ('','.','..') for part in name.split('/')):raise Refused('path-traversal-or-alias')
    return name
def regular(root,name,maximum=MAX_BUNDLE):
    safe_path(name);path=Path(root)
    for part in PurePosixPath(name).parts:
        path=path/part
        if stat.S_ISLNK(path.lstat().st_mode):raise Refused('source-link')
    info=path.lstat()
    if not stat.S_ISREG(info.st_mode) or not 0<info.st_size<=maximum:raise Refused('source-file-size-or-type')
    raw=path.read_bytes()
    if len(raw)!=info.st_size or path.lstat().st_ino!=info.st_ino:raise Refused('source-file-changed')
    return raw
def archive_files(raw,pin,maximum=MAX_EXPANDED):
    if not isinstance(raw,bytes) or len(raw)!=pin['bytes'] or len(raw)>MAX_ARCHIVE or sha(raw)!=pin['sha256']:
        raise Refused('archive-byte-or-SHA256-pin')
    if 'sha512-'+base64.b64encode(hashlib.sha512(raw).digest()).decode()!=pin['integrity']:raise Refused('archive-SHA512-pin')
    try:
        with gzip.GzipFile(fileobj=io.BytesIO(raw)) as compressed:expanded=compressed.read(maximum+1)
    except (OSError,EOFError) as error:raise Refused('archive-gzip') from error
    if len(expanded)>maximum or len(expanded)<1024 or len(expanded)%512:raise Refused('archive-expanded-limit-or-alignment')
    files={};seen=set();last=0;count=0
    try:
        with tarfile.open(fileobj=io.BytesIO(expanded),mode='r:') as archive:
            for member in archive:
                count+=1
                if count>MAX_MEMBERS or member.pax_headers or member.offset!=last:raise Refused('archive-member-bound-or-extension')
                header=expanded[member.offset:member.offset+512]
                if len(header)!=512 or header[156:157] not in (b'0',b'\0',b'5'):raise Refused('archive-special-member')
                if header[257:263] not in (b'ustar\0',b'ustar '):raise Refused('archive-header-format')
                name=member.name.rstrip('/') if member.isdir() else member.name
                safe_path(name)
                if not name.startswith('package/') and name!='package':raise Refused('archive-package-root')
                if name.casefold() in seen:raise Refused('archive-duplicate-or-case-alias')
                seen.add(name.casefold())
                if member.isdir():
                    if member.size!=0:raise Refused('archive-directory-size')
                elif member.isfile():
                    if not 0<=member.size<=MAX_EXPANDED:raise Refused('archive-member-size')
                    start=member.offset_data;end=start+member.size
                    if start!=member.offset+512 or end>len(expanded):raise Refused('archive-member-truncated')
                    files[name]=expanded[start:end]
                else:raise Refused('archive-member-type')
                last=member.offset+512+((member.size+511)//512)*512
                if any(expanded[member.offset_data+member.size:last]):raise Refused('archive-member-padding')
    except (tarfile.TarError,OSError) as error:raise Refused('archive-tar') from error
    if len(expanded)-last<1024 or any(expanded[last:]):raise Refused('archive-terminator-or-trailing-data')
    metadata=document(files.get('package/package.json',b''),1<<20)
    if metadata.get('name')!=pin['name'] or metadata.get('version')!=pin['version']:raise Refused('archive-package-identity')
    return files
def validate_manifest(manifest):
    if not isinstance(manifest,dict) or set(manifest)!={'schema','source_parent','packages','groups','artifact_files','provenance'} or manifest['schema']!=1:
        raise Refused('manifest-schema')
    if not re.fullmatch('[0-9a-f]{40}',manifest['source_parent']):raise Refused('manifest-source-parent')
    packages=manifest['packages'];groups=manifest['groups']
    if not isinstance(packages,list) or len(packages)!=5 or {row.get('id') for row in packages}!=PACKAGE_IDS:raise Refused('manifest-package-inventory')
    if sum(row['bytes'] for row in packages)>2<<20:raise Refused('manifest-total-archive-budget')
    for row in packages:
        if set(row)!={'id','name','version','url','bytes','sha256','integrity'}:raise Refused('manifest-package-schema')
        if not re.fullmatch('[0-9a-f]{64}',row['sha256']) or type(row['bytes']) is not int or not 0<row['bytes']<=MAX_ARCHIVE:raise Refused('manifest-package-pin')
        name=row['name'];version=row['version']
        if not re.fullmatch('@(?:openzeppelin|uniswap)/[A-Za-z0-9-]+',name) or not re.fullmatch(r'[0-9.]+(?:-beta\.[0-9]+)?',version):raise Refused('manifest-package-identity')
        expected='https://registry.npmjs.org/'+name+'/-/'+name.rsplit('/',1)[1]+'-'+version+'.tgz'
        if row['url']!=expected:raise Refused('manifest-package-URL')
    if not isinstance(groups,list) or tuple(row.get('name') for row in groups)!=GROUPS:raise Refused('manifest-complete-group-inventory')
    if sum(len(row['targets']) for row in groups)!=122 or sum(len(row['exports']) for row in groups)!=82:raise Refused('manifest-complete-target-inventory')
    expected_counts={'apps':48,'bench':34,'wznn':1,'core':12,'periphery':27}
    for group in groups:
        target_names=[row['name'] for row in group['targets']]
        export_names=[row['name'] for row in group['exports']]
        if len(target_names)!=expected_counts[group['name']] or len(set(target_names))!=len(target_names):raise Refused('manifest-exact-target-inventory')
        if len(set(export_names))!=len(export_names) or not set(export_names)<=set(target_names):raise Refused('manifest-export-target-binding')
        if group['name']=='apps':
            extra={'apps/ZVMFarmFeeStrategy.sol:'+name for name in ('IUniswapV2Factory','IUniswapV2Pair','IUniswapV2Router','IZVMFarm')}
            if set(target_names)-set(export_names)!=extra:raise Refused('manifest-non-exported-interface-inventory')
        targets={row['name']:row for row in group['targets']}
        for exported in group['exports']:
            if any(targets[exported['name']].get(key)!=value for key,value in exported.items() if key!='artifact_path'):raise Refused('manifest-export-material-binding')
        if len(group['sources'])>128 or len(group['targets'])>128 or not 0<group['input_bytes']<=16<<20:raise Refused('manifest-group-budget')
        if len({row['unit'] for row in group['sources']})!=len(group['sources']):raise Refused('manifest-source-duplicates')
        for row in group['sources']:
            safe_path(row['unit']);safe_path(row['path'])
            if row['origin'] not in PACKAGE_IDS|{'project'}:raise Refused('manifest-source-origin')
    return manifest
def bundles(manifest,packages,read_project):
    validate_manifest(manifest)
    if set(packages)!=PACKAGE_IDS:raise Refused('source-package-inventory')
    for row in manifest['artifact_files']:
        raw=read_project(row['path'])
        if len(raw)!=row['bytes'] or sha(raw)!=row['sha256']:raise Refused('tracked-artifact-drift')
    result=[]
    for group in manifest['groups']:
        sources={}
        for row in group['sources']:
            raw=read_project(row['path']) if row['origin']=='project' else packages[row['origin']].get(row['path'])
            if raw is None or len(raw)!=row['bytes'] or sha(raw)!=row['sha256']:raise Refused('literal-source-pin')
            try:content=raw.decode('utf-8')
            except UnicodeError as error:raise Refused('literal-source-encoding') from error
            sources[row['unit']]={'content':content}
        value={'language':'Solidity','sources':sources,'settings':group['settings']};raw=canonical(value)
        if len(raw)!=group['input_bytes'] or sha(raw)!=group['input_sha256']:raise Refused('complete-standard-JSON-input-pin')
        result.append({'name':group['name'],'input':value})
    return result
def fetch(pin,destination):
    path=Path(__file__).with_name('ci-fetch-compiler-vendors.py')
    spec=importlib.util.spec_from_file_location('bounded_vendor_transport',path)
    transport=importlib.util.module_from_spec(spec);spec.loader.exec_module(transport)
    opener=urllib.request.build_opener(transport.NoRedirect())
    path=destination/('source-'+pin['sha256']+'.tgz')
    fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    try:
        with os.fdopen(fd,'wb') as output:
            with transport.transport_deadline():
                with opener.open(urllib.request.Request(pin['url'],headers={'User-Agent':'ZVM-application-compiler-qualification'}),timeout=15) as response:
                    if response.geturl()!=pin['url']:raise Refused('source-transport-URL')
                    proof=transport.copy_bounded(response,output,pin['bytes'],pin['sha256'])
        if proof['bytes']!=pin['bytes']:raise Refused('source-transport-size')
    except Exception:
        with path.open('rb') as handle:prefix=handle.read(pin['bytes']+1)
        print('N01_APPLICATION_SOURCE_INTERRUPTED '+json.dumps({'id':pin['id'],'observed_prefix_bytes':len(prefix),
            'observed_prefix_sha256':sha(prefix),'expected_complete_bytes':pin['bytes'],'transport_complete':False,
            'partial_regular_file_preserved_in_job_owned_scratch':True,'qualification':'UNKNOWN_OR_FAILURE'},sort_keys=True),flush=True)
        raise
    return regular(destination,path.name,MAX_ARCHIVE),proof
def main(args):
    if len(args)!=2:raise Refused('arguments')
    root=Path(__file__).resolve().parent.parent
    raw_manifest=regular(root,'cmd/zvm-node/testdata/compiler_application_manifest.json',256<<10)
    manifest=validate_manifest(document(raw_manifest,256<<10))
    destination=Path(args[1]);destination.mkdir(mode=0o700)
    packages={}
    for pin in manifest['packages']:
        raw,proof=fetch(pin,destination);packages[pin['id']]=archive_files(raw,pin)
        print('N01_APPLICATION_SOURCE_TRANSPORT '+json.dumps({'id':pin['id'],**proof,'transport_attempts':1,
            'SHA512_integrity_matched':True,'archive_extracted_or_installed':False,'automatic_retry_or_fallback':False},sort_keys=True),flush=True)
    value={'schema':1,'manifest_sha256':sha(raw_manifest),'groups':bundles(manifest,packages,lambda name:regular(root,name))}
    encoded=canonical(value)
    if len(encoded)>MAX_BUNDLE:raise Refused('source-bundle-budget')
    path=destination/'compiler-applications.json';fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o444)
    with os.fdopen(fd,'wb') as handle:
        handle.write(encoded)
        os.fchmod(handle.fileno(),0o444)
    print('N01_APPLICATION_INPUTS '+json.dumps({'manifest_sha256':sha(raw_manifest),'bundle_sha256':sha(encoded),'bundle_bytes':len(encoded),
        'runtime_bundle_mode':stat.S_IMODE(path.stat().st_mode),'complete_groups':list(GROUPS),'target_records':122,'tracked_export_records':82,'compiler_executions_on_host':0,
        'package_installations':0,'archive_files_extracted_to_disk':0,'literal_source_files_copied_to_a_tree':0},sort_keys=True),flush=True)
if __name__=='__main__':
    try:main(sys.argv)
    except Exception as error:
        print('N01_APPLICATION_INPUT_FAILURE '+json.dumps({'type':type(error).__name__,'code':str(error) if isinstance(error,Refused) else 'source-input-error','partial_outputs_preserved_in_job_owned_scratch':True,'binary_payloads_qualified_locally':False},sort_keys=True),file=sys.stderr)
        raise SystemExit(1)

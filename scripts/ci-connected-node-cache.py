"""Manage only the fresh, explicitly disposable Go cache of this hosted job."""
import json,os,pathlib,shutil,stat,sys

temp=pathlib.Path(os.environ['RUNNER_TEMP']);assert temp.is_dir() and temp.resolve()==temp
name='zvm-owned-go-cache-'+os.environ['GITHUB_RUN_ID']+'-'+os.environ['GITHUB_RUN_ATTEMPT']
cache=temp/name;receipt=temp/(name+'.owner.json')
assert os.environ['GOCACHE']==str(cache)
action=sys.argv[1];assert action in ['prepare','release']
free=shutil.disk_usage(temp).free
if action=='prepare':
    print('OWNED-GO-CACHE-ADMISSION '+json.dumps({'freeBytes':free,'minimumFreeBytes':10<<30,'expectedGoBytes':3<<30},sort_keys=True),flush=True)
    assert free>=13<<30,'10 GiB floor plus 3 GiB monitored Go allocation is required'
    cache.mkdir(mode=0o700)
    info=cache.lstat();record={'schema':1,'run':os.environ['GITHUB_RUN_ID'],'attempt':os.environ['GITHUB_RUN_ATTEMPT'],
        'head':os.environ['GITHUB_SHA'],'name':name,'device':info.st_dev,'inode':info.st_ino,
        'disposable':True,'contains_historical_inputs_or_test_evidence':False}
    with receipt.open('x') as f:json.dump(record,f,sort_keys=True);f.write('\n')
    print('OWNED-GO-CACHE-PREPARED '+json.dumps({'disposable':True,'freshDirectory':True,'freeBytes':free,'minimumFreeBytes':10<<30,'expectedGoBytes':3<<30},sort_keys=True))
else:
    assert receipt.is_file() and not receipt.is_symlink()
    record=json.loads(receipt.read_bytes());info=cache.lstat()
    assert record=={'schema':1,'run':os.environ['GITHUB_RUN_ID'],'attempt':os.environ['GITHUB_RUN_ATTEMPT'],
        'head':os.environ['GITHUB_SHA'],'name':name,'device':info.st_dev,'inode':info.st_ino,
        'disposable':True,'contains_historical_inputs_or_test_evidence':False}
    assert stat.S_ISDIR(info.st_mode) and not cache.is_symlink()
    files=[];directories=[];logical=allocated=0
    for directory,subdirs,names in os.walk(cache,followlinks=False):
        parent=pathlib.Path(directory);assert not parent.is_symlink()
        for part in subdirs:
            path=parent/part;assert stat.S_ISDIR(path.lstat().st_mode) and not path.is_symlink();directories.append(path)
        for part in names:
            path=parent/part;item=path.lstat();assert stat.S_ISREG(item.st_mode)
            files.append((path,item.st_dev,item.st_ino,item.st_size));logical+=item.st_size;allocated+=item.st_blocks*512
        assert len(files)+len(directories)<=200000 and logical<=8<<30
    for path,device,inode,size in files:
        item=path.lstat();assert (item.st_dev,item.st_ino,item.st_size)==(device,inode,size) and stat.S_ISREG(item.st_mode);path.unlink()
    for path in reversed(directories):path.rmdir()
    info=cache.lstat();assert [info.st_dev,info.st_ino]==[record['device'],record['inode']];cache.rmdir()
    # The owner receipt and every test log remain outside the disposable cache.
    print('OWNED-GO-CACHE-RELEASED '+json.dumps({'files':len(files),'logicalBytes':logical,'allocatedBytesBefore':allocated,
        'freeBytesBefore':free,'freeBytesAfter':shutil.disk_usage(temp).free,'retainedOwnerReceipt':True,
        'testEvidenceOrHistoricalCachesDeleted':False},sort_keys=True))

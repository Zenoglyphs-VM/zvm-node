"""Finite memory-only source archive and complete-manifest controls; no network or native compiler."""
from copy import deepcopy
import base64
import gzip
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tarfile
import unittest

path=Path(__file__).with_name('ci-prepare-application-inputs.py')
spec=importlib.util.spec_from_file_location('application_inputs',path)
inputs=importlib.util.module_from_spec(spec);spec.loader.exec_module(inputs)
manifest=inputs.document((Path(__file__).resolve().parent.parent/'cmd/zvm-node/testdata/compiler_application_manifest.json').read_bytes())
def pin(raw):
    return {'id':'openzeppelin','name':'@openzeppelin/contracts','version':'5.6.1','bytes':len(raw),
        'sha256':hashlib.sha256(raw).hexdigest(),'integrity':'sha512-'+base64.b64encode(hashlib.sha512(raw).digest()).decode()}
def packed(extra=(),metadata=None,change=None):
    body=io.BytesIO()
    with tarfile.open(fileobj=body,mode='w:',format=tarfile.USTAR_FORMAT) as archive:
        rows=[('package/package.json',json.dumps(metadata or {'name':'@openzeppelin/contracts','version':'5.6.1'}).encode(),tarfile.REGTYPE),
            ('package/Test.sol',b'pragma solidity ^0.8.0; contract Test {}',tarfile.REGTYPE),*extra]
        for name,data,kind in rows:
            row=tarfile.TarInfo(name);row.type=kind;row.size=len(data)
            if kind in (tarfile.SYMTYPE,tarfile.LNKTYPE):row.linkname='outside'
            archive.addfile(row,io.BytesIO(data))
    raw=body.getvalue()
    if change is not None:raw=change(raw)
    return gzip.compress(raw,mtime=0)

class SourceTests(unittest.TestCase):
    def test_complete_actual_manifest_and_budgets(self):
        self.assertIs(inputs.validate_manifest(manifest),manifest)
        self.assertEqual([row['name'] for row in manifest['groups']],list(inputs.GROUPS))
        self.assertEqual(sum(len(row['targets']) for row in manifest['groups']),122)
        self.assertEqual(sum(len(row['exports']) for row in manifest['groups']),82)
        self.assertEqual(len(manifest['artifact_files']),6)

    def test_duplicate_or_replaced_complete_target_is_refused(self):
        for replacement in [deepcopy(manifest['groups'][0]['targets'][1]),{'name':'apps/Unexpected.sol:Unexpected'}]:
            changed=deepcopy(manifest);changed['groups'][0]['targets'][0]=replacement
            with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)

    def test_saved_export_material_cannot_be_rebound(self):
        for key,value in [('name','apps/Missing.sol:Missing'),('abiSha256','0'*64),('creationSha256','0'*64)]:
            changed=deepcopy(manifest);changed['groups'][0]['exports'][0][key]=value
            with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)

    def test_regular_ustar_and_literal_package_identity(self):
        raw=packed();files=inputs.archive_files(raw,pin(raw))
        self.assertEqual(files['package/Test.sol'],b'pragma solidity ^0.8.0; contract Test {}')
        self.assertEqual(len(files),2)

    def test_size_SHA256_and_SHA512_are_independently_required(self):
        raw=packed()
        for key,value in [('bytes',len(raw)+1),('sha256','0'*64),('integrity','sha512-'+base64.b64encode(b'0'*64).decode())]:
            record=pin(raw);record[key]=value
            with self.subTest(key=key):
                with self.assertRaises(inputs.Refused):inputs.archive_files(raw,record)

    def test_gzip_truncation_crc_and_expansion_bounds(self):
        raw=packed()
        for changed in [raw[:-1],raw[:-8]+b'12345678']:
            with self.assertRaises(inputs.Refused):inputs.archive_files(changed,pin(changed))
        with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw),maximum=1024)

    def test_nonregular_archive_members_are_not_filesystem_fixtures(self):
        for kind in [tarfile.SYMTYPE,tarfile.LNKTYPE,tarfile.FIFOTYPE,tarfile.CHRTYPE,tarfile.BLKTYPE]:
            raw=packed([('package/special',b'',kind)])
            with self.subTest(kind=kind):
                with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw))

    def test_path_traversal_and_package_escape(self):
        for name in ['package/../outside','/package/absolute','outside/file','package/./alias','package/double//slash']:
            raw=packed([(name,b'x',tarfile.REGTYPE)])
            with self.subTest(name=name):
                with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw))

    def test_duplicate_and_case_aliased_members(self):
        for name in ['package/Test.sol','package/test.sol']:
            raw=packed([(name,b'x',tarfile.REGTYPE)])
            with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw))

    def test_dirty_padding_and_nonzero_trailing_bytes(self):
        def padding(raw):return raw[:700]+b'x'+raw[701:]
        def trailing(raw):return raw+b'x'+b'\0'*511
        for change in [padding,trailing]:
            raw=packed(change=change)
            with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw))

    def test_package_identity_is_not_inferred_from_filename_or_digest(self):
        for metadata in [{'name':'other','version':'5.6.1'},{'name':'@openzeppelin/contracts','version':'0.0.0'}]:
            raw=packed(metadata=metadata)
            with self.assertRaises(inputs.Refused):inputs.archive_files(raw,pin(raw))

    def test_duplicate_json_missing_or_truncated_documents(self):
        for raw in [b'',b'{',b'{"a":1,"a":2}',b'{"a":1} trailing']:
            with self.assertRaises(inputs.Refused):inputs.document(raw)

    def test_missing_extra_reordered_or_incomplete_groups(self):
        for groups in [manifest['groups'][:-1],list(reversed(manifest['groups'])),manifest['groups']+[manifest['groups'][0]]]:
            changed=deepcopy(manifest);changed['groups']=deepcopy(groups)
            with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)
        changed=deepcopy(manifest);changed['groups'][0]['targets'].pop()
        with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)

    def test_package_URL_and_duplicate_source_inventory_refusals(self):
        changed=deepcopy(manifest);changed['packages'][0]['url']='https://example.invalid/archive.tgz'
        with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)
        changed=deepcopy(manifest);changed['groups'][0]['sources'].append(deepcopy(changed['groups'][0]['sources'][0]))
        with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)

    def test_source_origin_and_path_refusals(self):
        for key,value in [('origin','unknown'),('unit','../outside'),('path','/private/source')]:
            changed=deepcopy(manifest);changed['groups'][0]['sources'][0][key]=value
            with self.assertRaises(inputs.Refused):inputs.validate_manifest(changed)

    def test_missing_package_and_tracked_artifact_drift_stop_before_compilation(self):
        with self.assertRaises(inputs.Refused):inputs.bundles(manifest,{},lambda name:b'')
        with self.assertRaises(inputs.Refused):inputs.bundles(manifest,{name:{} for name in inputs.PACKAGE_IDS},lambda name:b'changed')

if __name__=='__main__':
    suite=unittest.defaultTestLoader.loadTestsFromTestCase(SourceTests);assert suite.countTestCases()==16
    result=unittest.TextTestRunner(verbosity=2,stream=sys.stdout).run(suite)
    if not result.wasSuccessful() or result.skipped:raise SystemExit(1)
    print('N01_APPLICATION_INPUT_CONTROLS '+json.dumps({'functions':16,'failures':0,'errors':0,'skips':0,
        'memory_only':True,'network_requests':0,'native_compiler_executions':0,'archives_extracted_to_disk':0},sort_keys=True))

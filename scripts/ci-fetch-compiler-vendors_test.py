#!/usr/bin/env python3
"""Memory-only refusal controls for the actual CI transport implementation."""
import hashlib,importlib.util,io,json,pathlib,sys,unittest
from unittest.mock import patch
path=pathlib.Path(__file__).with_name('ci-fetch-compiler-vendors.py')
spec=importlib.util.spec_from_file_location('vendor_fetch',path);vendor=importlib.util.module_from_spec(spec);spec.loader.exec_module(vendor)

class Response(io.BytesIO):
    def __init__(self,raw,status=200,**headers):super().__init__(raw);self.status=status;self.headers=headers
class VendorTransportControls(unittest.TestCase):
    def test_complete_exact_digest(self):
        output=io.BytesIO();raw=b'owned-input'
        result=vendor.copy_bounded(Response(raw,**{'Content-Length':str(len(raw))}),output,64,hashlib.sha256(raw).hexdigest())
        self.assertEqual(result['bytes'],len(raw));self.assertEqual(output.getvalue(),raw)
    def test_status_and_declared_length_refused(self):
        for response in [Response(b'x',status=302),Response(b'x',**{'Content-Length':'65'}),Response(b'x',**{'Content-Length':'invalid'}),Response(b'x',**{'Content-Length':'0'})]:
            with self.assertRaises(vendor.Refused):vendor.copy_bounded(response,io.BytesIO(),64)
    def test_actual_cap_preserves_bounded_prefix(self):
        output=io.BytesIO()
        with self.assertRaisesRegex(vendor.Refused,'stream-cap'):vendor.copy_bounded(Response(b'x'*65),output,64)
        self.assertLessEqual(len(output.getvalue()),64)
    def test_truncation_empty_and_digest_refused(self):
        for response,expected in [(Response(b'x',**{'Content-Length':'2'}),None),(Response(b''),None),(Response(b'x'),'0'*64)]:
            with self.assertRaises(vendor.Refused):vendor.copy_bounded(response,io.BytesIO(),64,expected)
    def test_elapsed_bound(self):
        with self.assertRaisesRegex(vendor.Refused,'elapsed-cap'):vendor.copy_bounded(Response(b'x'),io.BytesIO(),64,clock=iter([0,61]).__next__)
    def test_deadline_restored_and_existing_timer_refused(self):
        with patch.object(vendor.signal,'getitimer',return_value=(1.0,0.0)):
            with self.assertRaisesRegex(vendor.Refused,'existing-timer'):
                with vendor.transport_deadline():self.fail('must refuse existing timer')
        with patch.object(vendor.signal,'getitimer',return_value=(0.0,0.0)),patch.object(vendor.signal,'getsignal',return_value='original'),patch.object(vendor.signal,'signal') as handlers,patch.object(vendor.signal,'setitimer') as timers:
            with self.assertRaisesRegex(RuntimeError,'owned'):
                with vendor.transport_deadline():raise RuntimeError('owned')
            self.assertEqual(timers.call_args_list[-1].args,(vendor.signal.ITIMER_REAL,0))
            self.assertEqual(handlers.call_args_list[-1].args,(vendor.signal.SIGALRM,'original'))
    def test_redirect_has_no_followup(self):
        with self.assertRaisesRegex(vendor.Refused,'redirect'):vendor.NoRedirect().redirect_request(None,None,302,'',{},'https://example.invalid')
    def test_URL_scope_refused_before_open(self):
        with patch.object(vendor.urllib.request,'build_opener',side_effect=AssertionError('network must not be opened')):
            with self.assertRaisesRegex(vendor.Refused,'URL-scope'):vendor.fetch('https://example.invalid',io.BytesIO(),64)
    def test_duplicate_and_changed_manifest_refused(self):
        with self.assertRaisesRegex(vendor.Refused,'duplicate-key'):vendor.selected(b'{"a":1,"a":2}')
        for raw in [b'[]',b'{}',json.dumps([{'path':'../file'}]*3).encode()]:
            with self.assertRaises(vendor.Refused):vendor.selected(raw)
    def test_metadata_mismatch_or_duplicate_build_refused(self):
        rows=[{'path':'file','version':'v','longVersion':'long','sha256':'a'*64}]
        item={**rows[0],'sha256':'0x'+'b'*64}
        with self.assertRaisesRegex(vendor.Refused,'metadata-pin'):vendor.verify_metadata(json.dumps({'builds':[item]}).encode(),rows)
        with self.assertRaisesRegex(vendor.Refused,'metadata-count'):vendor.verify_metadata(json.dumps({'builds':[item,item]}).encode(),rows)
if __name__=='__main__':
    result=unittest.TextTestRunner(stream=sys.stdout,verbosity=1).run(unittest.defaultTestLoader.loadTestsFromTestCase(VendorTransportControls))
    print('N01_VENDOR_FETCH_CONTROLS '+json.dumps({'tests':result.testsRun,'pass':result.wasSuccessful(),'memory_only':True,'network_requests':0,'vendor_binary_downloads':0,'native_compiler_executions':0},sort_keys=True))
    raise SystemExit(0 if result.wasSuccessful() else 1)

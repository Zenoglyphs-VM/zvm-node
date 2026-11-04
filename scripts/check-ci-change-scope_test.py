"""Memory-only selection controls; no network, Git mutation or native build."""
from pathlib import Path
import importlib.util
import sys
import unittest

path = Path(__file__).with_name('check-ci-change-scope.py')
spec = importlib.util.spec_from_file_location('checked_ci_scope', path)
scope = importlib.util.module_from_spec(spec); spec.loader.exec_module(scope)


class ScopeTests(unittest.TestCase):
    def decide(self, changes, *, ancestor=None, current=None, previous=None, proven=True, workflow=None):
        paths = scope.IGNORED_AT_ANCHOR
        return scope.classify(changes, changes if ancestor is None else ancestor,
            {p:'100644' for p in paths} if current is None else current,
            {p:'100644' for p in paths} if previous is None else previous, proven,
            scope.EXPECTED_WORKFLOW_SHA256 if workflow is None else workflow)

    def test_guide_only(self):
        value = self.decide([('M', scope.GUIDE)])
        self.assertEqual((value['run_go'], value['run_integration']), (False, False))

    def test_declared_tests_keep_go_and_skip_unchanged_integrations(self):
        for path in sorted(scope.TESTS):
            with self.subTest(path=path):
                value = self.decide([('M', path)])
                self.assertEqual((value['run_go'], value['run_integration']), (True, False))

    def test_added_test_and_guide(self):
        path = 'indexer/nft/metadata_range_process_linux_test.go'
        value = self.decide([('A',path),('M',scope.GUIDE)], previous={scope.GUIDE:'100644'})
        self.assertEqual((value['run_go'], value['run_integration']), (True,False))

    def test_unknown_paths_and_controller_changes_require_full(self):
        for path in ['go.mod','go.sum','.gitmodules','core','indexer/nft/metadata_range.go',
                'orderbook/full_client_signed_recovery_test.go','scripts/market-navigation-support.mjs',
                'docs/UNKNOWN.md','indexer/nft/new_test.go',*sorted(scope.CONTROLLER)]:
            with self.subTest(path=path):
                value = self.decide([('M',path)])
                self.assertTrue(value['run_go'] and value['run_integration'])

    def test_changed_production_ancestor_cannot_be_hidden_by_test_only_tip(self):
        for path in ['indexer/nft/metadata_range.go','apps/apps.go','bench/artifacts.go','go.mod','orderbook/full_client_signed_recovery_test.go']:
            with self.subTest(path=path):
                self.assertTrue(self.decide([('M',scope.GUIDE)],ancestor=[('M',path)])['run_integration'])

    def test_deleted_renamed_or_type_changed_input_requires_full(self):
        for status in ['D','T','R100','C100','?']:
            with self.subTest(status=status):
                self.assertTrue(self.decide([(status,scope.GUIDE)])['run_integration'])

    def test_symlink_submodule_executable_or_unknown_modes_require_full(self):
        for mode in ['120000','160000','100755','tree','commit','']:
            for role in ['current','previous']:
                with self.subTest(mode=mode,role=role):
                    self.assertTrue(self.decide([('M',scope.GUIDE)],**{role:{scope.GUIDE:mode}})['run_integration'])

    def test_ancestor_mode_changes_require_full(self):
        for path in scope.CONTROLLER:
            with self.subTest(path=path):
                modes = {p:'100644' for p in scope.IGNORED_AT_ANCHOR}; modes[path]='120000'
                self.assertTrue(self.decide([('M',scope.GUIDE)],ancestor=[('M',path)],current=modes)['run_integration'])

    def test_missing_provenance_empty_changes_and_changed_workflow_require_full(self):
        for arguments in [{'proven':False},{'proven':None},{'proven':1},{'workflow':'0'*64}]:
            with self.subTest(arguments=arguments):
                value=self.decide([('M',scope.GUIDE)],**arguments)
                self.assertTrue(value['run_go'] and value['run_integration'])
        self.assertTrue(self.decide([])['run_integration'])

    def test_added_status_cannot_hide_existing_path(self):
        self.assertTrue(self.decide([('A',scope.GUIDE)])['run_integration'])

    def test_malformed_diff_is_unknown(self):
        for raw in [b'',b'M\0file',b'M\0',b'R100\0old\0new\0',b'M\0a\nfile\0',b'M\0\xff\0',b'M\0x\0M\0x\0']:
            with self.subTest(raw=raw):
                with self.assertRaises(scope.UnknownSelection): scope.parse_changes(raw)

    def test_exact_nul_delimited_diff(self):
        self.assertEqual(scope.parse_changes(b'M\0indexer/nft/metadata_range_index_test.go\0A\0docs/METADATA_INDEX_PROCESS_QUALIFICATION.md\0'),
            [('M','indexer/nft/metadata_range_index_test.go'),('A',scope.GUIDE)])


if __name__ == '__main__':
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(ScopeTests)
    assert suite.countTestCases() == 12
    result = unittest.TextTestRunner(verbosity=2, stream=sys.stdout).run(suite)
    if not result.wasSuccessful() or result.skipped:
        raise SystemExit(1)
    print('CI-SCOPE-CONTROLS functions=12 failures=0 errors=0 skips=0')

"""Memory-only CI dependency and source-graph controls; no network or native build."""
from copy import deepcopy
import importlib.util
from itertools import product
import json
from pathlib import Path
import sys
import unittest

path = Path(__file__).with_name('ci-market-result-gate.py')
spec = importlib.util.spec_from_file_location('market_gate', path)
gate = importlib.util.module_from_spec(spec); spec.loader.exec_module(gate)
WORKFLOW = Path('.github/workflows/ci.yml').read_text(encoding='utf-8')
MATRIX_CASES = 0
LEGACY_CASES = 0


def dependencies(selection='FULL_REQUIRED'):
    flags = gate.SELECTIONS[selection]
    value = {'ci-scope': {'result': 'success', 'outputs': dict(zip(('selection', 'run_go', 'run_integration'), (selection, *flags)))}}
    for name in gate.RUNTIME:
        selected = flags[0] == 'true' if name in ('go', 'compiler-isolation') else flags[1] == 'true'
        value[name] = {'result': 'success' if selected else 'skipped', 'outputs': {}}
    return value


class GateTests(unittest.TestCase):
    def test_complete_success_and_source_only_results(self):
        for selection in gate.SELECTIONS:
            with self.subTest(selection=selection):
                result = gate.decide(dependencies(selection))
                self.assertTrue(result['all_required_jobs_succeeded'])
                self.assertEqual(result['complete_runtime_qualified'], selection == 'FULL_REQUIRED')
                self.assertFalse(result['skipped_runtime_is_new_qualification'])

    def test_all_runtime_result_combinations_for_each_selection(self):
        global MATRIX_CASES, LEGACY_CASES
        states = ('success', 'failure', 'cancelled', 'skipped', 'neutral', 'timed_out', '')
        for selection in gate.SELECTIONS:
            expected = dependencies(selection)
            for results in product(states, repeat=5):
                value = deepcopy(expected)
                for name, result in zip(gate.RUNTIME, results):
                    value[name]['result'] = result
                if all(value[name]['result'] == expected[name]['result'] for name in gate.RUNTIME):
                    self.assertTrue(gate.decide(value)['all_required_jobs_succeeded'])
                else:
                    with self.assertRaises(gate.Refused): gate.decide(value)
                MATRIX_CASES += 1
                if value['connected-node']['result'] == expected['connected-node']['result']:
                    LEGACY_CASES += 1

    def test_selection_failure_cannot_be_hidden_by_other_success(self):
        for state in ('failure', 'cancelled', 'skipped', 'neutral', 'timed_out', '', None):
            value = dependencies(); value['ci-scope']['result'] = state
            with self.subTest(state=state):
                with self.assertRaises(gate.Refused): gate.decide(value)

    def test_missing_unknown_or_malformed_dependencies(self):
        for name in gate.NEEDS:
            value = dependencies(); del value[name]
            with self.assertRaises(gate.Refused): gate.decide(value)
            for bad in (None, [], 'success', {}, {'result': 'success'}, {'result': 'success', 'outputs': []}):
                value = dependencies(); value[name] = bad
                with self.assertRaises(gate.Refused): gate.decide(value)
        value = dependencies(); value['unexpected-job'] = {'result': 'success', 'outputs': {}}
        with self.assertRaises(gate.Refused): gate.decide(value)
        for value in (None, [], {}, True):
            with self.assertRaises(gate.Refused): gate.decide(value)

    def test_unknown_inconsistent_or_missing_selection_outputs(self):
        for selection in gate.SELECTIONS:
            for name in ('selection', 'run_go', 'run_integration'):
                value = dependencies(selection); del value['ci-scope']['outputs'][name]
                with self.assertRaises(gate.Refused): gate.decide(value)
                for bad in (None, True, False, 'TRUE', '', [], 'unknown'):
                    value = dependencies(selection); value['ci-scope']['outputs'][name] = bad
                    with self.assertRaises(gate.Refused): gate.decide(value)
            value = dependencies(selection); value['ci-scope']['outputs']['other'] = 'true'
            with self.assertRaises(gate.Refused): gate.decide(value)
        value = dependencies(); value['ci-scope']['outputs']['run_go'] = 'false'
        with self.assertRaises(gate.Refused): gate.decide(value)
        value = dependencies('DECLARED_GUIDE_ONLY'); value['ci-scope']['outputs']['run_integration'] = 'true'
        with self.assertRaises(gate.Refused): gate.decide(value)

    def test_unexpected_dependency_fields_or_outputs_are_not_accepted(self):
        for name in gate.NEEDS:
            value = dependencies(); value[name]['unexpected'] = True
            with self.assertRaises(gate.Refused): gate.decide(value)
        for name in gate.RUNTIME:
            value = dependencies(); value[name]['outputs'] = {'invented': 'success'}
            with self.assertRaises(gate.Refused): gate.decide(value)

    def test_bounded_unique_json_and_no_truncated_fallback(self):
        value = dependencies()
        self.assertEqual(gate.read_needs(json.dumps(value)), value)
        for raw in ('', ' ', '{', 'null trailing', 'x' * (gate.MAX_BYTES+1),
                    '{"go":{},"go":{}}', '{"go":{"result":"success","result":"failure"}}', '['*1100):
            with self.subTest(raw=raw[:30]):
                with self.assertRaises(gate.Refused): gate.read_needs(raw)

    def test_exact_actual_workflow_graph(self):
        self.assertTrue(gate.check_workflow(WORKFLOW))

    def test_changed_dependencies_conditions_names_or_bound_input_are_refused(self):
        mutations = [
            ('[ci-scope, go, full-client-recovery, compiler-isolation, market-browser-execution, connected-node]', '[ci-scope, go, full-client-recovery, market-browser-execution]'),
            ('[ci-scope, go, full-client-recovery, compiler-isolation, market-browser-execution, connected-node]', '[ci-scope]'),
            ('  market-browser-execution:\n    needs: [ci-scope]', '  market-browser-execution:\n    needs: [ci-scope, go]'),
            ('    if: ${{ always() }}', '    if: ${{ success() }}'),
            ('      selection: ${{ steps.select.outputs.selection }}\n', ''),
            ('          ZVM_CI_NEEDS: ${{ toJSON(needs) }}', '          ZVM_CI_NEEDS: "{}"'),
            ('run: python3 -B scripts/ci-market-result-gate.py', 'run: true'),
            ('  market-browser:\n', '  renamed-market-browser:\n'),
            ('  market-browser:\n', '  market-browser:\n    name: An unrelated context\n'),
            ('timeout-minutes: 3\n    steps:\n      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262\n', 'timeout-minutes: 15\n    steps:\n      - uses: actions/checkout@11d5960a326750d5838078e36cf38b85af677262\n'),
        ]
        for before, after in mutations:
            self.assertEqual(WORKFLOW.count(before), 1, before)
            with self.subTest(before=before):
                with self.assertRaises(gate.Refused): gate.check_workflow(WORKFLOW.replace(before, after, 1))
        for name in gate.RUNTIME:
            before = '  '+name+':\n    needs: [ci-scope]\n'
            self.assertEqual(WORKFLOW.count(before), 1)
            with self.assertRaises(gate.Refused): gate.check_workflow(WORKFLOW.replace(before, before.replace('[ci-scope]', '[]'), 1))


if __name__ == '__main__':
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(GateTests)
    assert suite.countTestCases() == 9
    result = unittest.TextTestRunner(verbosity=2, stream=sys.stdout).run(suite)
    if not result.wasSuccessful() or result.skipped:
        raise SystemExit(1)
    assert MATRIX_CASES == 50421 and LEGACY_CASES == 7203
    print('CI-MARKET-GATE-CONTROLS '+json.dumps({'functions': 9, 'result_combinations': MATRIX_CASES, 'preserved_original_result_combinations': LEGACY_CASES,
        'failures': 0, 'errors': 0, 'skips': 0, 'network_requests': 0, 'native_builds_or_tests': 0}, sort_keys=True))

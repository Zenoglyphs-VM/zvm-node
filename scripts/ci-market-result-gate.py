#!/usr/bin/env python3
"""Preserve the named market-browser result after independent runtime jobs."""
import json
import os
from pathlib import Path
import re

RUNTIME = ('go', 'full-client-recovery', 'compiler-isolation', 'market-browser-execution', 'connected-node')
NEEDS = ('ci-scope',) + RUNTIME
SELECTIONS = {
    'FULL_REQUIRED': ('true', 'true'),
    'DECLARED_NFT_TESTS_ONLY': ('true', 'false'),
    'DECLARED_GUIDE_ONLY': ('false', 'false'),
}
MAX_BYTES = 16384


class Refused(ValueError):
    pass


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise Refused('duplicate_dependency_field')
        result[key] = value
    return result


def read_needs(raw):
    if not isinstance(raw, str) or not raw or len(raw.encode('utf-8')) > MAX_BYTES:
        raise Refused('missing_or_oversized_dependencies')
    try:
        value = json.loads(raw, object_pairs_hook=unique_object)
    except (ValueError, RecursionError) as error:
        raise Refused('malformed_dependencies') from error
    return value


def decide(needs):
    if not isinstance(needs, dict) or set(needs) != set(NEEDS):
        raise Refused('missing_or_unknown_dependency')
    for row in needs.values():
        if not isinstance(row, dict) or set(row) != {'outputs', 'result'} or not isinstance(row['outputs'], dict):
            raise Refused('malformed_dependency')
    scope = needs['ci-scope']
    if scope['result'] != 'success':
        raise Refused('source_selection_did_not_succeed')
    outputs = scope['outputs']
    if set(outputs) != {'selection', 'run_go', 'run_integration'}:
        raise Refused('missing_or_unknown_source_selection')
    selection = outputs['selection']
    if not isinstance(selection, str) or selection not in SELECTIONS:
        raise Refused('unrecognized_source_selection')
    flags = (outputs['run_go'], outputs['run_integration'])
    if flags != SELECTIONS[selection]:
        raise Refused('inconsistent_source_selection')
    selected = set()
    if outputs['run_go'] == 'true':
        selected.update(('go', 'compiler-isolation'))
    if outputs['run_integration'] == 'true':
        selected.update(('full-client-recovery', 'market-browser-execution', 'connected-node'))
    for name in RUNTIME:
        expected = 'success' if name in selected else 'skipped'
        if needs[name]['result'] != expected or needs[name]['outputs']:
            raise Refused('dependency_result_does_not_match_selection:' + name)
    return {
        'selection': selection,
        'selected_jobs': sorted(selected),
        'skipped_jobs': sorted(set(RUNTIME) - selected),
        'all_required_jobs_succeeded': True,
        'complete_runtime_qualified': selection == 'FULL_REQUIRED',
        'skipped_runtime_is_new_qualification': False,
    }


def check_workflow(raw):
    """Check the declared job graph without network or an additional YAML dependency.

    This deliberately accepts only the reviewed literal job headers. It does not
    attempt to interpret arbitrary YAML. Semantic job results are checked above.
    """
    if not isinstance(raw, str) or len(raw.encode('utf-8')) > 65536:
        raise Refused('workflow_size_or_type')
    if len(re.findall(r'^jobs:\n', raw, re.M)) != 1:
        raise Refused('workflow_jobs_section_missing_or_duplicated')
    raw = raw.split('jobs:\n', 1)[1]
    headers = list(re.finditer(r'^  ([a-z][a-z0-9-]*):\n', raw, re.M))
    if [match.group(1) for match in headers] != [
            'ci-scope', 'go', 'market-browser-execution', 'full-client-recovery', 'compiler-isolation', 'connected-node', 'market-browser']:
        raise Refused('workflow_job_identity_or_order')
    jobs = {match.group(1): raw[match.end():headers[index+1].start() if index+1 < len(headers) else len(raw)]
            for index, match in enumerate(headers)}
    for name, block in jobs.items():
        if re.search(r'^    name:', block, re.M):
            raise Refused('workflow_job_display_context_changed')
        if name == 'ci-scope':
            if block.count('      selection: ${{ steps.select.outputs.selection }}\n') != 1:
                raise Refused('workflow_selection_output_missing')
            continue
        expected_needs = '[ci-scope, go, full-client-recovery, compiler-isolation, market-browser-execution, connected-node]' if name == 'market-browser' else '[ci-scope]'
        if re.findall(r'^    needs: (.+)$', block, re.M) != [expected_needs]:
            raise Refused('workflow_dependency_graph_changed')
        if name == 'market-browser':
            expected_if = '${{ always() }}'
            if 'timeout-minutes: 3\n' not in block or block.count('run: python3 -B scripts/ci-market-result-gate.py\n') != 1:
                raise Refused('workflow_gate_command_or_limit_changed')
            if block.count('          ZVM_CI_NEEDS: ${{ toJSON(needs) }}\n') != 1:
                raise Refused('workflow_gate_dependencies_not_bound')
        else:
            flag = 'run_go' if name in ('go', 'compiler-isolation') else 'run_integration'
            expected_if = "${{ always() && needs.ci-scope.outputs." + flag + " != 'false' }}"
        if re.findall(r'^    if: (.+)$', block, re.M) != [expected_if]:
            raise Refused('workflow_selection_or_always_condition_changed')
    return True


def main():
    try:
        check_workflow(Path('.github/workflows/ci.yml').read_text(encoding='utf-8'))
        result = decide(read_needs(os.environ.get('ZVM_CI_NEEDS', '')))
    except (Refused, OSError, UnicodeError) as error:
        print('CI-MARKET-GATE ' + json.dumps({'accepted': False, 'reason': str(error)}, sort_keys=True))
        raise SystemExit(1)
    print('CI-MARKET-GATE ' + json.dumps({'accepted': True, **result}, sort_keys=True))


if __name__ == '__main__':
    main()

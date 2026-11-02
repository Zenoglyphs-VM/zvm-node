#!/usr/bin/env python3
"""Conservative selection for declared NFT tests and their qualification guide.

Skipped jobs are selection, not new runtime qualification. The original runtime,
dependency, browser, service and fixture inputs must still equal the pinned
anchor. Every unknown input, classifier failure or unrelated change runs CI.
"""
from pathlib import Path
import hashlib
import json
import os
import re
import selectors
import subprocess
import sys
import time

ANCHOR = '86a254841276ede899b22379a9cdea4f9632465a'
EXPECTED_WORKFLOW_SHA256 = 'afe0565f5ebbffa8ba3b7bed64566cc3779b18ab19794796b89201ee51ce1f37'
GUIDE = 'docs/METADATA_INDEX_PROCESS_QUALIFICATION.md'
TESTS = frozenset(['indexer/nft/metadata_range_index_test.go',
    'indexer/nft/metadata_range_process_linux_test.go', 'indexer/nft/metadata_range_process_other_test.go'])
CONTROLLER = frozenset(['.github/workflows/ci.yml', 'scripts/check-ci-change-scope.py', 'scripts/check-ci-change-scope_test.py'])
IGNORED_AT_ANCHOR = TESTS | CONTROLLER | {GUIDE}


class UnknownSelection(Exception):
    pass


def full(reason):
    return {'selection': 'FULL_REQUIRED', 'reason': reason, 'run_go': True, 'run_integration': True}


def classify(changes, ancestor_changes, current_modes, previous_modes, proven, workflow_digest):
    if proven is not True:
        return full('source_or_reference_provenance_unavailable')
    if workflow_digest != EXPECTED_WORKFLOW_SHA256:
        return full('workflow_differs_from_reviewed_selection_and_job_commands')
    if not changes:
        return full('empty_or_unavailable_change_set')
    for status, path in ancestor_changes:
        if status not in ('A', 'M') or path not in IGNORED_AT_ANCHOR:
            return full('integration_inputs_differ_from_pinned_runtime_anchor')
        if current_modes.get(path) != '100644':
            return full('unreviewed_anchor_file_type')
    for status, path in changes:
        if status not in ('A', 'M') or path not in TESTS | {GUIDE}:
            return full('unreviewed_change')
        if current_modes.get(path) != '100644' or (status == 'M' and previous_modes.get(path) != '100644') or (status == 'A' and path in previous_modes):
            return full('unreviewed_file_type_or_status')
    if all(path == GUIDE for _, path in changes):
        return {'selection': 'DECLARED_GUIDE_ONLY', 'reason': 'runtime_and_all_test_inputs_unchanged', 'run_go': False, 'run_integration': False}
    return {'selection': 'DECLARED_NFT_TESTS_ONLY', 'reason': 'only_NFT_test_inputs_changed_integration_inputs_unchanged', 'run_go': True, 'run_integration': False}


def git(*args):
    # Read only local Git objects. Never fetch implicitly, invoke a pager or
    # allocate an unbounded stdout/stderr buffer on an unknown repository.
    env = {**os.environ, 'GIT_NO_LAZY_FETCH': '1', 'GIT_NO_REPLACE_OBJECTS': '1',
        'GIT_OPTIONAL_LOCKS': '0', 'GIT_TERMINAL_PROMPT': '0', 'GIT_PAGER': 'cat'}
    child = subprocess.Popen(['git', *args], stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    streams = selectors.DefaultSelector()
    streams.register(child.stdout, selectors.EVENT_READ, 'out')
    streams.register(child.stderr, selectors.EVENT_READ, 'err')
    captured = {'out': bytearray(), 'err': bytearray()}
    deadline = time.monotonic() + 10
    try:
        while streams.get_map():
            left = deadline - time.monotonic()
            if left <= 0:
                raise UnknownSelection('git_read_deadline')
            for key, _ in streams.select(min(left, 0.2)):
                chunk = os.read(key.fileobj.fileno(), 8192)
                if not chunk:
                    streams.unregister(key.fileobj)
                    continue
                captured[key.data].extend(chunk)
                if len(captured[key.data]) > 65536:
                    raise UnknownSelection('git_read_output_limit')
        if child.wait(timeout=max(0.01, deadline - time.monotonic())) != 0:
            raise UnknownSelection('git_reference_or_diff_unavailable')
        return bytes(captured['out'])
    finally:
        streams.close()
        if child.poll() is None:
            child.kill()
        child.wait()
        child.stdout.close(); child.stderr.close()


def parse_changes(raw):
    if not raw or not raw.endswith(b'\0'):
        raise UnknownSelection('empty_or_malformed_diff')
    parts = raw[:-1].split(b'\0')
    if len(parts) % 2:
        raise UnknownSelection('malformed_diff_pairs')
    rows = []
    for index in range(0, len(parts), 2):
        try:
            status, path = parts[index].decode('ascii'), parts[index+1].decode('utf-8')
        except UnicodeError:
            raise UnknownSelection('unsupported_diff_encoding')
        if status not in ('A', 'M', 'D', 'T') or not path or '\n' in path or '\r' in path or path in {row[1] for row in rows}:
            raise UnknownSelection('unrecognized_diff_record')
        rows.append((status, path))
    return rows


def tree_modes(oid):
    raw = git('ls-tree', '-r', '-z', oid, '--', *sorted(IGNORED_AT_ANCHOR))
    if not raw or not raw.endswith(b'\0'):
        raise UnknownSelection('unavailable_tree_modes')
    modes = {}
    for row in raw[:-1].split(b'\0'):
        metadata, path = row.split(b'\t', 1)
        mode, kind, _ = metadata.decode('ascii').split()
        modes[path.decode('utf-8')] = mode if kind == 'blob' else kind
    return modes


def observe():
    base, head = os.environ.get('ZVM_CHANGE_BASE', ''), os.environ.get('ZVM_CHANGE_HEAD', '')
    for oid in [base, head]:
        if not re.fullmatch('[a-f0-9]{40}', oid) or oid == '0'*40:
            raise UnknownSelection('missing_or_invalid_change_reference')
    if base == head or git('rev-parse', '--verify', 'HEAD').decode().strip() != head:
        raise UnknownSelection('empty_diff_or_checkout_mismatch')
    for oid in [base, head, ANCHOR]:
        git('cat-file', '-e', oid + '^{commit}')
    git('merge-base', '--is-ancestor', base, head)
    changes = parse_changes(git('diff', '--no-renames', '--name-status', '-z', base, head))
    ancestor_raw = git('diff', '--no-renames', '--name-status', '-z', ANCHOR, head)
    ancestor_changes = parse_changes(ancestor_raw) if ancestor_raw else []
    workflow = git('show', head + ':.github/workflows/ci.yml')
    result = classify(changes, ancestor_changes, tree_modes(head), tree_modes(base), True, hashlib.sha256(workflow).hexdigest())
    return {**result, 'base': base, 'head': head, 'changed_paths': len(changes),
        'runtime_anchor': ANCHOR, 'skipped_jobs_are_selection_not_new_runtime_evidence': True}


def main():
    if sys.argv[1:] not in [[], ['--github-output']]:
        raise SystemExit('unexpected selector arguments')
    try:
        result = observe()
    except UnknownSelection as error:
        result = {**full(str(error)), 'skipped_jobs_are_selection_not_new_runtime_evidence': True}
    print(json.dumps(result, sort_keys=True))
    if sys.argv[1:]:
        with open(os.environ['GITHUB_OUTPUT'], 'a', encoding='utf-8') as handle:
            for key in ['run_go', 'run_integration']:
                handle.write(key+'='+str(result[key]).lower()+'\n')
            handle.write('selection='+result['selection']+'\n')


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Map frozen upstream advisory aliases to a replayed fork import graph."""

import argparse
from collections import Counter
from datetime import datetime, timezone
import json
import math
from pathlib import Path
import re
import subprocess
import sys

import import_graph as graph_review
from bounded import ReviewError, file_bytes, save, sha, unique_object
from govulncheck_review import reject_constant, source_binding
from local_core import bind_core

NOMINAL = 'github.com/ethereum/go-ethereum'
EFFECTIVE = 'github.com/ethereum-optimism/op-geth'
OSV = 'https://api.osv.dev/v1/query'
LEGACY = 'https://geth.ethereum.org/docs/vulnerabilities/vulnerabilities.json'
PROVIDERS = {
    'osv-upstream': (OSV, NOMINAL), 'osv-effective': (OSV, EFFECTIVE),
    'osv-commit': (OSV, None),
    'github-upstream': ('https://api.github.com/repos/ethereum/go-ethereum/security-advisories', NOMINAL),
    'github-effective': ('https://api.github.com/repos/ethereum-optimism/op-geth/security-advisories', EFFECTIVE),
    'geth-legacy': (LEGACY, NOMINAL),
}
IDENTIFIER = re.compile(r'(?:GO-\d{4}-\d{4,8}|CVE-\d{4}-\d{4,8}|GHSA-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}|GETH-\d{4}-\d{2})')
SYMBOL = re.compile(r'[A-Za-z0-9_.*()\[\],~+-]{1,512}')
FIX_VERSION = re.compile(r'v?\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?(?:\+[A-Za-z0-9.-]+)?')
HEX64 = re.compile(r'[0-9a-f]{64}')
HEX40 = re.compile(r'[0-9a-f]{40}')
MAX_SNAPSHOT = 512 << 10
MAX_REVIEW = 2 << 20


def require(condition, message):
    if not condition:
        raise ReviewError(message)


def object_fields(value, fields):
    require(isinstance(value, dict) and set(value) == set(fields), 'unexpected object fields')


def count(value, low, high):
    require(type(value) is int and low <= value <= high, 'invalid bounded integer')


def text(value, pattern):
    require(isinstance(value, str) and pattern.fullmatch(value), 'invalid public identifier')


def strings(value, maximum, pattern):
    require(isinstance(value, list) and len(value) <= maximum, 'invalid bounded identifier list')
    for item in value:
        text(item, pattern)
    require(value == sorted(set(value)), 'duplicate or unordered identifiers')


def decode(raw, limit):
    require(isinstance(raw, bytes) and 0 < len(raw) <= limit, 'invalid JSON input size')
    return json.loads(raw, object_pairs_hook=unique_object, parse_constant=reject_constant)


def timestamp(value, *, date_allowed=False):
    if value is None:
        return
    require(isinstance(value, str) and len(value) <= 64, 'invalid record time')
    if date_allowed and re.fullmatch(r'\d{4}-\d{2}-\d{2}', value):
        datetime.fromisoformat(value)
        return
    match = re.fullmatch(r'(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d{1,9}))?(Z|\+00:00)', value)
    require(match is not None, 'record time must be UTC RFC 3339')
    # Python 3.9 accepts fewer fractional forms than RFC 3339. Normalize only
    # this calendar-validity check; retain the original nanosecond text in output.
    fraction = '.' + (match.group(2) + '000000')[:6] if match.group(2) else ''
    datetime.fromisoformat(match.group(1) + fraction + '+00:00')


def catalog(raw):
    value = decode(raw, MAX_SNAPSHOT)
    object_fields(value, ('schema', 'capturedAt', 'nominalModule', 'effectiveIdentityAtCapture', 'queries', 'records'))
    require(type(value['schema']) is int and value['schema'] == 1 and value['nominalModule'] == NOMINAL,
            'unsupported upstream snapshot scope')
    require(isinstance(value['capturedAt'], str) and len(value['capturedAt']) <= 64, 'invalid capture time')
    captured = datetime.fromisoformat(value['capturedAt'].replace('Z', '+00:00'))
    require(captured.tzinfo is not None and captured.utcoffset().total_seconds() == 0, 'capture time must be UTC')
    identity = value['effectiveIdentityAtCapture']
    object_fields(identity, ('module', 'version', 'commit'))
    require(identity['module'] == EFFECTIVE, 'unexpected effective module')
    text(identity['version'], graph_review.VERSION)
    text(identity['commit'], HEX40)
    queries = value['queries']
    require(isinstance(queries, list) and len(queries) == len(PROVIDERS), 'incomplete provider query set')
    provider_counts = {}
    for query in queries:
        object_fields(query, ('provider', 'url', 'subject', 'completeWithinSelectedQuery', 'responses', 'recordOccurrences'))
        provider = query['provider']
        require(isinstance(provider, str) and provider in PROVIDERS and provider not in provider_counts,
                'duplicate or unknown provider')
        url, subject = PROVIDERS[provider]
        require(query['url'] == url and query['subject'] == (identity['commit'] if subject is None else subject),
                'provider scope differs from the selected snapshot')
        require(query['completeWithinSelectedQuery'] is True, 'incomplete provider pagination')
        require(isinstance(query['responses'], list) and 1 <= len(query['responses']) <= 5, 'invalid response pin count')
        for pin in query['responses']:
            object_fields(pin, ('bytes', 'sha256'))
            count(pin['bytes'], 1, 2 << 20)
            text(pin['sha256'], HEX64)
        count(query['recordOccurrences'], 0, 2048)
        provider_counts[provider] = query['recordOccurrences']
    records = value['records']
    require(isinstance(records, list) and 0 < len(records) <= 2048, 'invalid advisory record count')
    seen = set()
    for record in records:
        object_fields(record, ('provider', 'id', 'aliases', 'recordSha256', 'recordURL', 'published', 'modified',
                              'withdrawnAt', 'upstreamFixedVersions', 'explicitUpstreamCommitReferences', 'packageAnnotations'))
        require(isinstance(record['provider'], str) and record['provider'] in PROVIDERS, 'unknown record provider')
        text(record['id'], IDENTIFIER)
        strings(record['aliases'], 64, IDENTIFIER)
        require(record['id'] not in record['aliases'], 'redundant self alias')
        pair = (record['provider'], record['id'])
        require(pair not in seen, 'duplicate provider advisory occurrence')
        seen.add(pair)
        text(record['recordSha256'], HEX64)
        expected_url = ('https://pkg.go.dev/vuln/' + record['id'] if record['id'].startswith('GO-') else
                        'https://github.com/advisories/' + record['id'] if record['id'].startswith('GHSA-') else LEGACY)
        require(record['recordURL'] == expected_url, 'unexpected advisory URL')
        for key in ('published', 'modified', 'withdrawnAt'):
            timestamp(record[key], date_allowed=key != 'withdrawnAt')
        strings(record['upstreamFixedVersions'], 64, FIX_VERSION)
        strings(record['explicitUpstreamCommitReferences'], 64, HEX40)
        annotations = record['packageAnnotations']
        require(isinstance(annotations, list) and len(annotations) <= 256, 'invalid package annotation count')
        annotated = set()
        for annotation in annotations:
            object_fields(annotation, ('path', 'goos', 'goarch', 'symbols'))
            path = annotation['path']
            require(graph_review.path(path) and (path == NOMINAL or path.startswith(NOMINAL + '/')),
                    'annotation outside the nominal upstream namespace')
            strings(annotation['goos'], 64, re.compile(r'[a-z0-9_]{1,32}'))
            strings(annotation['goarch'], 64, re.compile(r'[a-z0-9_]{1,32}'))
            strings(annotation['symbols'], 1024, SYMBOL)
            identity_key = (path, tuple(annotation['goos']), tuple(annotation['goarch']))
            require(identity_key not in annotated, 'duplicate package annotation profile')
            annotated.add(identity_key)
    observed = Counter(r['provider'] for r in records)
    require(all(observed[p] == expected for p, expected in provider_counts.items()), 'provider record accounting differs')
    return value


def summarize(raw, graph):
    """Data mapping only; the caller must replay and bind the graph producer."""
    snapshot = catalog(raw)
    require(isinstance(graph, dict), 'invalid graph summary')
    require(graph.get('status') == 'SELECTED_IMPORT_GRAPHS_RECORDED', 'incomplete graph summary')
    profile = graph.get('profile')
    object_fields(profile, ('GOVERSION', 'GOOS', 'GOARCH', 'CGO_ENABLED'))
    require(isinstance(profile['GOVERSION'], str) and re.fullmatch(r'go1\.\d+\.\d+', profile['GOVERSION']) and profile['GOOS'] == 'linux'
            and profile['GOARCH'] == 'amd64' and profile['CGO_ENABLED'] == '0', 'unexpected selected graph profile')
    modules = graph.get('selectedModules')
    require(isinstance(modules, list) and 0 < len(modules) <= 4096, 'invalid selected module inventory')
    selected = [graph_review.module(m, core_profile=graph.get('localCoreBinding')) for m in modules]
    require(all(m is not None for m in selected), 'missing selected module identity')
    require(len({m['Path'] for m in selected}) == len(selected), 'duplicate selected module identity')
    nominal = [m for m in selected if m['Path'] == NOMINAL]
    require(len(nominal) == 1 and nominal[0]['Replace'] is not None and nominal[0]['Replace']['Path'] == EFFECTIVE,
            'selected graph does not bind the expected replacement fork')
    closures = {}
    for key in ('entryImportClosure', 'testImportClosure'):
        ids = graph.get(key)
        require(isinstance(ids, list) and 0 < len(ids) <= graph_review.MAX_PACKAGES, 'invalid selected import closure')
        require(all(isinstance(i, str) for i in ids) and ids == sorted(set(ids)), 'duplicate or unordered package IDs')
        closures[key] = {graph_review.package_id(i) for i in ids}
    require(graph['entryPackage'] in graph['entryImportClosure'], 'entry absent from its closure')
    parent = {}

    def find(identifier):
        parent.setdefault(identifier, identifier)
        root = identifier
        while parent[root] != root:
            root = parent[root]
        while parent[identifier] != identifier:
            previous = parent[identifier]
            parent[identifier] = root
            identifier = previous
        return root

    for record in snapshot['records']:
        for identifier in (record['id'], *record['aliases']):
            left, right = find(record['id']), find(identifier)
            if left != right:
                parent[max(left, right)] = min(left, right)
    groups = {}
    for record in snapshot['records']:
        groups.setdefault(find(record['id']), []).append(record)
    mapped = []
    for group, records in sorted(groups.items()):
        annotations = []
        for record in records:
            for annotation in record['packageAnnotations']:
                path = annotation['path']
                cli = path in closures['entryImportClosure']
                tests = path in closures['testImportClosure']
                relation = ('PRESENT_IN_CLI_AND_TEST_IMPORTS' if cli and tests else 'PRESENT_IN_CLI_IMPORTS' if cli else
                            'PRESENT_ONLY_IN_TEST_IMPORTS' if tests else 'NOT_OBSERVED_IN_THESE_IMPORT_CLOSURES')
                annotations.append({**annotation, 'provider': record['provider'], 'recordID': record['id'],
                    'recordWithdrawn': record['withdrawnAt'] is not None, 'selectedPackageRelation': relation,
                    'annotationPlatformMatchesSelectedProfile':
                        (not annotation['goos'] or profile['GOOS'] in annotation['goos']) and
                        (not annotation['goarch'] or profile['GOARCH'] in annotation['goarch']),
                    'functionExecutionEstablished': False, 'forkFixPresenceEstablished': False})
        active = sum(record['withdrawnAt'] is None for record in records)
        mapped.append({'group': group, 'identifiers': sorted({i for r in records for i in (r['id'], *r['aliases'])}),
            'recordOccurrences': len(records), 'activeRecordOccurrences': active,
            'recordPins': [{k: r[k] for k in ('provider', 'id', 'recordURL', 'recordSha256', 'published', 'modified', 'withdrawnAt')}
                           for r in records],
            'packageAnnotations': annotations,
            'packageAnnotationCoverage': 'ANNOTATIONS_RECORDED' if annotations else 'UNKNOWN_NO_PACKAGE_ANNOTATIONS',
            'upstreamFixedVersionsInformationalOnly': sorted({v for r in records for v in r['upstreamFixedVersions']}),
            'explicitUpstreamCommitReferences': sorted({v for r in records for v in r['explicitUpstreamCommitReferences']}),
            'assessment': 'INDEPENDENT_FORK_REVIEW_REQUIRED' if active else 'WITHDRAWN_RECORDS_RETAINED',
            'forkFixOrRuntimeAcceptance': 'NOT_ESTABLISHED', 'waiverGenerated': False})
    return {'schema': 1, 'status': 'UPSTREAM_ALIAS_MAPPING_RECORDED_REVIEW_REQUIRED',
        'snapshotSha256': sha(raw), 'snapshotCapturedAt': snapshot['capturedAt'],
        'snapshotQueries': snapshot['queries'], 'effectiveIdentityAtCapture': snapshot['effectiveIdentityAtCapture'],
        'selectedNominalAndEffectiveModule': nominal[0], 'profile': profile,
        'effectiveVersionMatchesCapture': nominal[0]['Replace']['Version'] == snapshot['effectiveIdentityAtCapture']['version'],
        'recordOccurrences': len(snapshot['records']), 'aliasGroups': len(mapped),
        'groupsRequiringIndependentAssessment': sum(g['activeRecordOccurrences'] > 0 for g in mapped), 'groups': mapped,
        'graphSourceBindingVerified': False, 'snapshotLiveFeedsQueriedByThisConsumer': False,
        'snapshotFreshnessPolicyEstablished': False, 'independentFeedOriginAuthenticated': False,
        'upstreamVersionRangesAppliedToFork': False, 'missingAnnotationsMeanUnaffected': False,
        'emptyEffectiveNamespaceMeansUnaffected': False, 'functionExecutionOrExploitabilityEstablished': False,
        'fullForkAdvisoryCoverageEstablished': False, 'waiversGenerated': False, 'releaseAccepted': False}


def verified_graph(directory, source, go_version):
    expected_profile = {'GOVERSION': go_version, 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}
    execution = decode(file_bytes(directory / 'execution.json', MAX_REVIEW), MAX_REVIEW)
    require(isinstance(execution, dict), 'invalid graph execution receipt')
    require(execution.get('source') == source and execution.get('status') == 'SELECTED_IMPORT_GRAPHS_RECORDED'
            and execution.get('profile') == expected_profile and execution.get('moduleArchiveDownloadsEnabled') is False,
            'graph producer scope differs from the current source')
    commands = execution.get('commands')
    require(isinstance(commands, list) and all(isinstance(c, dict) for c in commands)
            and [c.get('label') for c in commands] == ['profile', 'local-core-module', 'packages'],
            'incomplete graph producer command set')
    outputs = {}
    for command in commands:
        object_fields(command, ('label', 'process'))
        label, process = command['label'], command['process']
        object_fields(process, ('actualExit', 'outputComplete', 'terminationReason', 'seconds', 'streams'))
        require(type(process['seconds']) in (int, float) and math.isfinite(process['seconds']) and
                0 <= process['seconds'] <= 125, 'invalid graph process duration')
        require(type(process['actualExit']) is int and process['actualExit'] == 0 and process['outputComplete'] is True
                and process['terminationReason'] is None, 'graph process failed or is incomplete')
        object_fields(process['streams'], ('stdout', 'stderr'))
        for name in ('stdout', 'stderr'):
            pin = process['streams'][name]
            object_fields(pin, ('path', 'bytes', 'sha256', 'eofObserved', 'limit'))
            limit = (16384 if label == 'profile' else 65536 if label == 'local-core-module' else graph_review.MAX_BYTES) if name == 'stdout' else 65536
            require(pin['path'] == label + '/' + name and pin['limit'] == limit and pin['eofObserved'] is True,
                    'unexpected producer stream binding')
            count(pin['bytes'], 0, limit)
            text(pin['sha256'], HEX64)
            raw = file_bytes(directory / label / name, limit)
            require(len(raw) == pin['bytes'] and sha(raw) == pin['sha256'], 'producer stream digest differs')
            if name == 'stdout':
                outputs[label] = raw
    require(decode(outputs['profile'], 16384) == expected_profile, 'raw profile differs')
    binding = bind_core(source, outputs['local-core-module'])
    require(binding == execution['localCoreBinding'], 'local Core graph identity differs')
    raw_graph = decode(file_bytes(directory / 'graph.json', MAX_REVIEW), MAX_REVIEW)
    replay = graph_review.summarize(outputs['packages'], entry=raw_graph['entryPackage'], core_profile=binding)
    replay.update(source=source, profile=expected_profile, producerOutputSha256=sha(outputs['packages']))
    require(replay == raw_graph, 'graph summary does not reproduce its exact producer output')
    return replay


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-commit', required=True)
    parser.add_argument('--go-version', required=True)
    parser.add_argument('--graph', type=Path, required=True)
    parser.add_argument('--snapshot', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    receipt = {'schema': 1, 'observedAt': datetime.now(timezone.utc).isoformat(),
        'status': 'UPSTREAM_ALIAS_MAPPING_FAILED_OR_INCOMPLETE', 'releaseAccepted': False,
        'nativeCommandsExecuted': False, 'liveFeedsQueried': False}
    created, phase, result = False, 'output_directory', 2
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = 'source_before'
        before = source_binding(args.source_commit)
        receipt['source'] = before
        phase = 'graph_replay'
        graph = verified_graph(args.graph, before, args.go_version)
        phase = 'snapshot_mapping'
        require(args.snapshot.resolve(strict=True) == Path(__file__).resolve().with_name('upstream-advisory-snapshot.json'),
                'snapshot must be the source-bound repository catalog')
        raw = file_bytes(args.snapshot, MAX_SNAPSHOT)
        review = summarize(raw, graph)
        review.update(source=before, graphSourceBindingVerified=True, producerOutputSha256=graph['producerOutputSha256'])
        phase = 'source_after'
        require(source_binding(args.source_commit) == before, 'source changed during advisory mapping')
        encoded = json.dumps(review, sort_keys=True, indent=2).encode()
        require(len(encoded) <= MAX_REVIEW, 'mapped review exceeds the output cap')
        save(args.output / 'review.json', review)
        receipt.update(status=review['status'], snapshotSha256=sha(raw), reviewSha256=sha((args.output / 'review.json').read_bytes()),
                       producerOutputSha256=graph['producerOutputSha256'], graphSourceBindingVerified=True)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, RecursionError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
    if created:
        save(args.output / 'execution.json', receipt)
    print(json.dumps({'status': receipt['status'], 'exit': result, 'releaseAccepted': False}))
    return result


if __name__ == '__main__':
    sys.exit(main())

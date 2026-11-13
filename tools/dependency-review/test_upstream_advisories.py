"""Frozen advisory accounting and exact-producer replay controls using data only."""

import copy
import json
from pathlib import Path
import unittest
from unittest.mock import patch

import upstream_advisories as review
import import_graph as graph_review
import local_core
from test_import_graph import ENTRY, encoded
from test_local_core import graph_rows, source, selection

PROFILE = {'GOVERSION': 'go1.26.11', 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}


def record(provider, identifier, aliases=(), annotations=()):
    return {'provider': provider, 'id': identifier, 'aliases': sorted(aliases), 'recordSha256': 'a' * 64,
        'recordURL': 'https://pkg.go.dev/vuln/' + identifier if identifier.startswith('GO-') else
            'https://github.com/advisories/' + identifier if identifier.startswith('GHSA-') else review.LEGACY,
        'published': '2026-01-01', 'modified': None, 'withdrawnAt': None,
        'packageAnnotations': list(annotations), 'upstreamFixedVersions': ['1.17.2'],
        'explicitUpstreamCommitReferences': ['b' * 40]}


def annotation(suffix, *, goos=(), goarch=()):
    return {'path': review.NOMINAL + '/' + suffix, 'symbols': ['Decode'], 'goos': list(goos), 'goarch': list(goarch)}


def snapshot():
    records = [record('osv-upstream', 'GO-2026-1001', ['CVE-2026-1001'],
                      [annotation('codec'), annotation('testonly'), annotation('absent')]),
        record('github-upstream', 'GHSA-aaaa-bbbb-cccc', ['CVE-2026-1001']),
        record('geth-legacy', 'GETH-2026-01', ['GHSA-aaaa-bbbb-cccc']),
        record('osv-upstream', 'GO-2026-1002', ['CVE-2026-1002'])]
    queries = [{'provider': p, 'url': url, 'subject': subject if subject is not None else 'c' * 40,
                'completeWithinSelectedQuery': True, 'responses': [{'bytes': 2, 'sha256': 'd' * 64}],
                'recordOccurrences': sum(r['provider'] == p for r in records)}
               for p, (url, subject) in sorted(review.PROVIDERS.items())]
    return {'schema': 1, 'capturedAt': '2026-11-13T00:00:00+00:00', 'nominalModule': review.NOMINAL,
        'effectiveIdentityAtCapture': {'module': review.EFFECTIVE, 'version': 'v1.101702.3', 'commit': 'c' * 40},
        'queries': queries, 'records': records}


def graph_input():
    rows = json.loads(json.dumps(graph_rows()).replace('example.org/upstream', review.NOMINAL)
                      .replace('example.org/fork', review.EFFECTIVE).replace('v1.100200.3', 'v1.101702.3'))
    return encoded(rows)


def selected_graph():
    raw = graph_input()
    result = graph_review.summarize(raw, entry=ENTRY, core_profile=local_core.expected_profile())
    result.update(source=source(), profile=PROFILE, producerOutputSha256=review.sha(raw))
    return result


def producer():
    directory = Path('/fixture/graph')
    stdout = {'profile': json.dumps(PROFILE).encode(), 'local-core-module': json.dumps(selection()).encode(),
              'packages': graph_input()}
    files, commands = {}, []
    for label, raw in stdout.items():
        streams = {}
        for name, payload in [('stdout', raw), ('stderr', b'')]:
            limit = (16384 if label == 'profile' else 65536 if label == 'local-core-module' else graph_review.MAX_BYTES) if name == 'stdout' else 65536
            streams[name] = {'path': label + '/' + name, 'bytes': len(payload), 'sha256': review.sha(payload),
                             'eofObserved': True, 'limit': limit}
            files[directory / label / name] = payload
        commands.append({'label': label, 'process': {'actualExit': 0, 'outputComplete': True,
            'terminationReason': None, 'seconds': 0.01, 'streams': streams}})
    execution = {'source': source(), 'profile': PROFILE, 'status': 'SELECTED_IMPORT_GRAPHS_RECORDED',
        'moduleArchiveDownloadsEnabled': False, 'localCoreBinding': local_core.expected_profile(), 'commands': commands}
    files[directory / 'execution.json'] = json.dumps(execution).encode()
    files[directory / 'graph.json'] = json.dumps(selected_graph()).encode()
    return directory, files, execution


class UpstreamAdvisoryTests(unittest.TestCase):
    def check(self, value=None, graph=None):
        return review.summarize(json.dumps(snapshot() if value is None else value).encode(), selected_graph() if graph is None else graph)

    def reject(self, value):
        with self.assertRaises((ValueError, KeyError, TypeError)):
            self.check(value)

    def replay(self, files, expected=None):
        with patch.object(review, 'file_bytes', side_effect=lambda path, limit: files[path]):
            return review.verified_graph(Path('/fixture/graph'), source() if expected is None else expected, 'go1.26.11')

    def test_transitive_aliases_preserve_every_provider_occurrence(self):
        result = self.check()
        self.assertEqual((result['recordOccurrences'], result['aliasGroups']), (4, 2))
        first = result['groups'][0]
        self.assertEqual(first['recordOccurrences'], 3)
        self.assertEqual(first['identifiers'], ['CVE-2026-1001', 'GETH-2026-01', 'GHSA-aaaa-bbbb-cccc', 'GO-2026-1001'])
        self.assertEqual(len(first['recordPins']), 3)

    def test_cli_test_only_and_absent_packages_remain_distinct(self):
        rows = self.check()['groups'][0]['packageAnnotations']
        self.assertEqual({r['path'].rsplit('/', 1)[1]: r['selectedPackageRelation'] for r in rows}, {
            'codec': 'PRESENT_IN_CLI_IMPORTS', 'testonly': 'PRESENT_ONLY_IN_TEST_IMPORTS',
            'absent': 'NOT_OBSERVED_IN_THESE_IMPORT_CLOSURES'})
        self.assertTrue(all(not r['functionExecutionEstablished'] and not r['forkFixPresenceEstablished'] for r in rows))

    def test_missing_annotations_and_empty_effective_feeds_never_clear_review(self):
        result = self.check()
        self.assertEqual(result['groups'][1]['packageAnnotationCoverage'], 'UNKNOWN_NO_PACKAGE_ANNOTATIONS')
        for field in ('emptyEffectiveNamespaceMeansUnaffected', 'missingAnnotationsMeanUnaffected', 'releaseAccepted',
                      'fullForkAdvisoryCoverageEstablished', 'waiversGenerated', 'graphSourceBindingVerified'):
            self.assertIs(result[field], False)
        self.assertEqual(result['groupsRequiringIndependentAssessment'], 2)

    def test_platform_annotations_are_kept_without_erasing_package_presence(self):
        value = snapshot(); value['records'][0]['packageAnnotations'][0]['goos'] = ['windows']
        row = self.check(value)['groups'][0]['packageAnnotations'][0]
        self.assertFalse(row['annotationPlatformMatchesSelectedProfile'])
        self.assertEqual(row['selectedPackageRelation'], 'PRESENT_IN_CLI_IMPORTS')

    def test_withdrawn_records_are_retained_without_suppressing_active_aliases(self):
        value = snapshot(); value['records'][0]['withdrawnAt'] = '2026-11-12T00:00:00Z'
        first = self.check(value)['groups'][0]
        self.assertEqual(first['activeRecordOccurrences'], 2)
        self.assertEqual(first['assessment'], 'INDEPENDENT_FORK_REVIEW_REQUIRED')
        for row in value['records']:
            row['withdrawnAt'] = '2026-11-12T00:00:00Z'
        result = self.check(value)
        self.assertEqual(result['groupsRequiringIndependentAssessment'], 0)
        self.assertTrue(all(g['assessment'] == 'WITHDRAWN_RECORDS_RETAINED' for g in result['groups']))
        self.assertFalse(result['waiversGenerated'])

    def test_fork_version_changes_do_not_relabel_old_namespace_queries(self):
        graph = selected_graph()
        next(m for m in graph['selectedModules'] if m['Path'] == review.NOMINAL)['Replace']['Version'] = 'v1.101702.4'
        result = self.check(graph=graph)
        self.assertFalse(result['effectiveVersionMatchesCapture'])
        self.assertFalse(result['upstreamVersionRangesAppliedToFork'])
        self.assertEqual(result['effectiveIdentityAtCapture']['version'], 'v1.101702.3')

    def test_invalid_withdrawal_times_cannot_suppress_active_review(self):
        for value in ('Z', '2026-99-99T00:00:00Z', '2026-11-13', '2026-11-13T00:00:00', '2026-11-13T00:00:00+03:00'):
            item = snapshot(); item['records'][0]['withdrawnAt'] = value
            self.reject(item)

    def test_rfc3339_fractional_precision_is_preserved_without_a_time_decision(self):
        for fraction in ('1', '12', '123', '123456', '1234567', '493797877'):
            value = snapshot(); timestamp = '2026-07-08T06:29:52.' + fraction + 'Z'
            value['records'][0]['modified'] = timestamp
            first = self.check(value)['groups'][0]
            self.assertEqual(next(r for r in first['recordPins'] if r['id'] == 'GO-2026-1001')['modified'], timestamp)
        value = snapshot(); value['records'][0]['modified'] = '2026-07-08T06:29:52.1234567890Z'
        self.reject(value)

    def test_partial_duplicate_and_oversize_json_are_refused(self):
        raw = json.dumps(snapshot()).encode()
        for bad in (b'', b'null', raw[:-1], raw.replace(b'"schema": 1', b'"schema":1,"schema":1'),
                    b' ' * (review.MAX_SNAPSHOT + 1), raw.replace(b'"schema": 1', b'"schema": NaN')):
            with self.subTest(size=len(bad)), self.assertRaises((ValueError, TypeError)):
                review.summarize(bad, selected_graph())

    def test_unknown_fields_wrong_schema_and_changed_scope_are_refused(self):
        variants = []
        for field, value in [('schema', True), ('nominalModule', 'example.org/other'), ('extra', True)]:
            item = snapshot(); item[field] = value; variants.append(item)
        item = snapshot(); item['effectiveIdentityAtCapture']['module'] = 'example.org/fork'; variants.append(item)
        item = snapshot(); item['capturedAt'] = '2026-11-13T00:00:00'; variants.append(item)
        for item in variants:
            self.reject(item)

    def test_missing_incomplete_or_miscounted_providers_are_refused(self):
        for change in ('missing', 'duplicate', 'incomplete', 'count', 'pins', 'URL', 'subject'):
            with self.subTest(change=change):
                value = snapshot(); row = value['queries'][0]
                if change == 'missing': value['queries'].pop()
                elif change == 'duplicate': value['queries'][-1] = copy.deepcopy(row)
                elif change == 'incomplete': row['completeWithinSelectedQuery'] = False
                elif change == 'count': row['recordOccurrences'] += 1
                elif change == 'pins': row['responses'] = []
                elif change == 'URL': row['url'] = 'https://example.org/advisories'
                else: row['subject'] = 'example.org/other'
                self.reject(value)

    def test_duplicate_records_and_unsafe_record_identifiers_are_refused(self):
        value = snapshot(); value['records'].append(copy.deepcopy(value['records'][0])); self.reject(value)
        for field, bad in [('id', '../source'), ('recordSha256', '0'), ('recordURL', 'https://example.org/record'),
                           ('aliases', ['CVE-2026-1001', 'CVE-2026-1001']), ('explicitUpstreamCommitReferences', ['bad'])]:
            value = snapshot(); value['records'][0][field] = bad; self.reject(value)

    def test_unsafe_and_duplicated_annotation_profiles_are_refused(self):
        for change in ('path', 'symbols', 'duplicate', 'extra'):
            value = snapshot(); row = value['records'][0]['packageAnnotations'][0]
            if change == 'path': row['path'] = '/fixture/private'
            elif change == 'symbols': row['symbols'] = ['bad\nline']
            elif change == 'extra': row['localPath'] = '/fixture/source'
            else: value['records'][0]['packageAnnotations'].append(copy.deepcopy(row))
            self.reject(value)

    def test_wrong_replacement_and_missing_nominal_module_are_refused(self):
        for change in ('missing', 'other', 'unversioned'):
            graph = selected_graph(); row = next(m for m in graph['selectedModules'] if m['Path'] == review.NOMINAL)
            if change == 'missing': graph['selectedModules'].remove(row)
            elif change == 'other': row['Replace']['Path'] = 'example.org/other'
            else: row['Replace']['Version'] = ''
            with self.assertRaises(ValueError): self.check(graph=graph)

    def test_invalid_closures_and_profile_are_refused(self):
        for change in ('private', 'duplicate', 'entry', 'profile'):
            graph = selected_graph()
            if change == 'private': graph['entryImportClosure'] = ['/fixture/source']
            elif change == 'duplicate': graph['testImportClosure'] += graph['testImportClosure']
            elif change == 'entry': graph['entryPackage'] = 'example.org/missing'
            else: graph['profile'] = {**PROFILE, 'CGO_ENABLED': '1'}
            with self.assertRaises(ValueError): self.check(graph=graph)

    def test_exact_graph_producer_is_independently_replayed(self):
        directory, files, execution = producer()
        self.assertEqual(self.replay(files), selected_graph())

    def test_changed_raw_stream_and_changed_summary_are_refused(self):
        directory, files, execution = producer()
        for name in ('packages/stdout', 'graph.json'):
            changed = dict(files)
            if name == 'graph.json':
                graph = selected_graph(); graph['entryImportClosure'] = [ENTRY]
                changed[directory / name] = json.dumps(graph).encode()
            else: changed[directory / name] += b'\n'
            with self.assertRaises(ValueError): self.replay(changed)

    def test_incomplete_failed_traversing_or_mispinned_streams_are_refused(self):
        for change in ('exit', 'booleanExit', 'complete', 'EOF', 'path', 'limit', 'digest'):
            directory, files, execution = producer(); process = execution['commands'][-1]['process']
            pin = process['streams']['stdout']
            if change == 'exit': process['actualExit'] = 1
            elif change == 'booleanExit': process['actualExit'] = False
            elif change == 'complete': process['outputComplete'] = False
            elif change == 'EOF': pin['eofObserved'] = False
            elif change == 'path': pin['path'] = '../source'
            elif change == 'limit': pin['limit'] += 1
            else: pin['sha256'] = 'f' * 64
            files[directory / 'execution.json'] = json.dumps(execution).encode()
            with self.assertRaises(ValueError): self.replay(files)

    def test_changed_source_or_core_profile_cannot_reuse_the_producer(self):
        directory, files, execution = producer()
        with self.assertRaises(ValueError): self.replay(files, {**source(), 'root': {'rootLockOverrides': {}, 'tree': 'changed'}})
        execution['localCoreBinding'] = {**local_core.expected_profile(), 'sourceTree': 'f' * 40}
        files[directory / 'execution.json'] = json.dumps(execution).encode()
        with self.assertRaises(ValueError): self.replay(files)

    def test_missing_commands_and_raw_profile_mismatch_are_refused(self):
        directory, files, execution = producer(); execution['commands'].pop()
        files[directory / 'execution.json'] = json.dumps(execution).encode()
        with self.assertRaises(ValueError): self.replay(files)
        directory, files, execution = producer(); files[directory / 'profile/stdout'] = b'{}'
        pin = execution['commands'][0]['process']['streams']['stdout']; pin.update(bytes=2, sha256=review.sha(b'{}'))
        files[directory / 'execution.json'] = json.dumps(execution).encode()
        with self.assertRaises(ValueError): self.replay(files)

    def test_committed_fact_projection_has_the_same_conservative_contract(self):
        raw = Path(__file__).with_name('upstream-advisory-snapshot.json').read_bytes()
        result = review.summarize(raw, selected_graph())
        self.assertGreater(result['recordOccurrences'], 0)
        self.assertGreater(result['aliasGroups'], 0)
        self.assertFalse(result['fullForkAdvisoryCoverageEstablished'])
        self.assertTrue(all(g['forkFixOrRuntimeAcceptance'] == 'NOT_ESTABLISHED' and not g['waiverGenerated'] for g in result['groups']))


if __name__ == '__main__':
    unittest.main()

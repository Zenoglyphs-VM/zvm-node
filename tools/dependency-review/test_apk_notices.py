"""Data-only controls for source-bound installed APK notice candidates."""

import base64
import copy
import json
import unittest

from bounded import ReviewError, sha
import apk_notices as apk


def installed(name, origin='demo', commit='c' * 40):
    return {'name': name, 'version': '1.0-r0', 'architecture': 'x86_64', 'installedChecksum': 'Q1' + 'a' * 28,
        'origin': origin, 'declaredLicense': 'MIT', 'declaredBuildCommit': commit, 'declaredInstalledBytes': 42,
        'licenseTextOrLegalQualificationEstablished': False}


def fixture():
    notice = b'Original source notice\nCopyright Example\n'
    digest = sha(notice)
    packages = [installed('alpine-release', 'alpine-base'), installed('demo'), installed('demo-extra')]
    origins = []
    for name in ('alpine-base', 'demo'):
        origins.append({'origin': name, 'recipe': {'repository': 'https://github.com/alpinelinux/aports',
            'commit': 'c' * 40, 'path': 'main/' + name + '/APKBUILD', 'gitBlob': 'd' * 40, 'sha256': 'e' * 64, 'bytes': 100},
            'sources': [], 'notices': [], 'referenceDocuments': [], 'unresolvedSourceInputs': []})
    origins[1]['sources'] = [
        {'name': 'demo-1.0.tar.gz', 'kind': 'SOURCE_ARCHIVE', 'url': 'https://distfiles.alpinelinux.org/distfiles/v3.22/demo-1.0.tar.gz', 'sha512': 'a' * 128, 'sha256': 'b' * 64, 'bytes': 1024},
        {'name': 'helper.c', 'kind': 'APORTS_SOURCE_FILE', 'url': 'https://raw.githubusercontent.com/alpinelinux/aports/' + 'c' * 40 + '/main/demo/helper.c', 'sha512': 'f' * 128, 'sha256': '1' * 64, 'bytes': 300}]
    origins[1]['notices'] = [
        {'source': 'demo-1.0.tar.gz', 'path': 'demo-1.0/COPYING', 'bytes': len(notice), 'sha256': digest, 'selectionScope': 'ARCHIVE_NOTICE_FILE'},
        {'source': 'helper.c', 'path': 'helper.c', 'bytes': len(notice), 'sha256': digest, 'selectionScope': 'INITIAL_SOURCE_NOTICE'}]
    value = {'schema': 1, 'status': apk.STATUS, 'scope': apk.SCOPE, 'installedPackages': packages,
        'origins': origins, 'objects': {digest: len(notice)}, 'referenceDocuments': [], **{key: False for key in apk.FLAGS}}
    return value, {digest: notice}


def files_for(value, objects):
    # Inline fixture notices become separately pinned occurrence pages on disk.
    manifest = copy.deepcopy(value)
    declared_objects = manifest.pop('objects')
    manifest['noticeObjectCount'] = len(declared_objects)
    manifest['noticePayloadBytes'] = sum(declared_objects.values())
    files = {apk.README: apk.README_BYTES, **{apk.PREFIX + 'objects/' + key: payload for key, payload in objects.items()}}
    for origin in manifest['origins']:
        notices = origin.pop('notices'); origin['noticePages'] = []
        for index, offset in enumerate(range(0, len(notices), 128)):
            path = apk.PAGES + origin['origin'] + '-' + str(index).zfill(3) + '.json'
            raw = json.dumps({'schema': 1, 'origin': origin['origin'], 'notices': notices[offset:offset + 128]}, sort_keys=True).encode()
            files[path] = raw; origin['noticePages'].append({'path': path, 'bytes': len(raw), 'sha256': sha(raw)})
    for document in manifest['referenceDocuments']:
        files[document['path']] = document.pop('payload')
    files[apk.MANIFEST] = json.dumps(manifest, sort_keys=True).encode()
    return files


def packet(files, main=True, owner=apk.ROOT):
    pins = [{'path': path, 'bytes': len(raw), 'sha256': sha(raw), 'selectionScope': 'MODULE_NOTICE_DIRECTORY'} for path, raw in sorted(files.items())]
    go = {'modules': [{'identity': {'path': owner, 'version': '', 'effectivePath': owner, 'effectiveVersion': '', 'main': main}, 'notices': pins}]}
    items = [('M', 'manifest.json', json.dumps(go, sort_keys=True).encode()), ('R', 'README.txt', b'Go notice packet fixture\n')]
    objects = {sha(raw): raw for raw in files.values()}
    items.extend(('O', key, raw) for key, raw in sorted(objects.items()))
    wire = b'ZVM-NOTICES-1\n' + b''.join((kind + '\t' + key + '\t' + str(len(raw)) + '\t1\t').encode() + base64.b64encode(raw) + b'\n' for kind, key, raw in items)
    return wire + ('END\t' + str(len(objects)) + '\n').encode()


def check(value, objects, selected=None, source=None, copied=None):
    files = files_for(value, objects)
    checked_source = files if source is None else source
    return apk.summarize(packet(files if copied is None else copied), value['installedPackages'] if selected is None else selected,
        lambda path, limit: checked_source[path])


class APKSourceNoticeTests(unittest.TestCase):
    def test_original_bytes_deduplicate_and_missing_notices_remain_explicit(self):
        value, objects = fixture(); result = check(value, objects)
        self.assertEqual((result['installedPackageCount'], result['sourceOriginCount'], result['recipePinnedSourceInputReferences']), (3, 2, 2))
        self.assertEqual((result['selectedNoticeOccurrences'], result['distinctNoticeObjects']), (2, 1))
        self.assertEqual(result['originsWithoutObservedNoticeCandidates'], ['alpine-base'])
        self.assertFalse(result['allOriginsHaveObservedCandidateNotices'])
        self.assertTrue(result['allRecordedSourceInputsPresent'])
        self.assertTrue(all(result[key] is False for key in apk.FLAGS))
        value['origins'][1]['notices'][0]['selectionScope'] = 'ARCHIVE_INITIAL_SOURCE_NOTICE'
        self.assertEqual(check(value, objects)['selectedNoticeOccurrences'], 2)

    def test_installed_checksums_versions_source_commits_and_licenses_cannot_be_relabelled(self):
        for field, replacement in [('installedChecksum', 'Q1' + 'z' * 28), ('version', '2.0-r0'),
                ('declaredBuildCommit', 'a' * 40), ('declaredLicense', 'GPL-2.0-only'), ('architecture', 'aarch64')]:
            with self.subTest(field=field):
                value, objects = fixture(); selected = copy.deepcopy(value['installedPackages']); selected[1][field] = replacement
                with self.assertRaises(ReviewError): check(value, objects, selected=selected)

    def test_recipe_must_match_the_actual_installed_origin_and_commit(self):
        for field, replacement in [('commit', 'a' * 40), ('path', 'main/other/APKBUILD'),
                ('repository', 'https://github.com/example/aports'), ('gitBlob', 'x' * 40)]:
            value, objects = fixture(); value['origins'][1]['recipe'][field] = replacement
            with self.assertRaises(ReviewError): check(value, objects)
        value, objects = fixture(); value['installedPackages'][2]['declaredBuildCommit'] = 'a' * 40
        with self.assertRaises(ReviewError): check(value, objects)

    def test_rewritten_copied_payload_cannot_be_hidden_by_new_wire_hashes(self):
        value, objects = fixture(); source = files_for(value, objects); copied = dict(source)
        key = next(path for path in source if '/objects/' in path); copied[key] += b'rewritten\n'
        with self.assertRaises(ReviewError): check(value, objects, source=source, copied=copied)
        copied = dict(source); copied[apk.MANIFEST] += b' '
        with self.assertRaises(ReviewError): check(value, objects, source=source, copied=copied)

    def test_catalog_and_source_input_claims_do_not_establish_legal_acceptance(self):
        for field in apk.FLAGS:
            for replacement in (True, 0, None):
                value, objects = fixture(); value[field] = replacement
                with self.assertRaises(ReviewError): check(value, objects)

    def test_duplicate_missing_and_foreign_origins_are_refused(self):
        for action in ('duplicate', 'missing', 'foreign'):
            value, objects = fixture()
            if action == 'duplicate': value['origins'][0] = copy.deepcopy(value['origins'][1])
            elif action == 'missing': value['origins'].pop()
            else: value['origins'][0]['origin'] = 'other'
            with self.assertRaises(ReviewError): check(value, objects)
        value, objects = fixture(); value['installedPackages'][2] = copy.deepcopy(value['installedPackages'][1])
        with self.assertRaises(ReviewError): check(value, objects)

    def test_source_checksum_url_and_size_boundaries_are_checked(self):
        for field, replacement in [('sha512', 'a' * 127), ('sha256', 'b' * 63), ('bytes', True), ('bytes', 81 << 20),
                ('url', 'http://distfiles.alpinelinux.org/a'), ('url', 'https://127.0.0.1/a'),
                ('url', 'https://github.com/.private/a'), ('url', 'https://user:password@github.com/a'),
                ('url', 'https://github.com/a?token=private'), ('name', '../source'), ('kind', 'EXECUTED_SCRIPT')]:
            value, objects = fixture(); value['origins'][1]['sources'][0][field] = replacement
            with self.assertRaises(ReviewError): check(value, objects)

    def test_notice_requires_the_same_source_input_scope_and_payload(self):
        for field, replacement in [('source', 'missing.tar.gz'), ('selectionScope', 'INITIAL_SOURCE_NOTICE'),
                ('path', '../COPYING'), ('bytes', 1), ('sha256', 'f' * 64)]:
            value, objects = fixture(); value['origins'][1]['notices'][0][field] = replacement
            with self.assertRaises(ReviewError): check(value, objects)
        value, objects = fixture(); value['origins'][1]['notices'].append(copy.deepcopy(value['origins'][1]['notices'][0]))
        with self.assertRaises(ReviewError): check(value, objects)

    def test_unreferenced_payloads_and_unexpected_checked_files_are_refused(self):
        value, objects = fixture(); extra = b'unused notice\n'; objects[sha(extra)] = extra; value['objects'][sha(extra)] = len(extra)
        with self.assertRaises(ReviewError): check(value, objects)
        value, objects = fixture(); files = files_for(value, objects); files[apk.PREFIX + 'unexpected.txt'] = b'extra\n'
        with self.assertRaises(ReviewError): check(value, objects, copied=files, source=files)

    def test_unresolved_source_input_stays_unknown_and_is_not_silently_omitted(self):
        value, objects = fixture(); value['origins'][1]['unresolvedSourceInputs'] = ['missing.patch']
        result = check(value, objects)
        self.assertEqual(result['unresolvedSourceInputs'], ['demo/missing.patch'])
        self.assertFalse(result['allRecordedSourceInputsPresent'])
        value['origins'][1]['unresolvedSourceInputs'] = ['helper.c']
        with self.assertRaises(ReviewError): check(value, objects)

    def test_missing_catalog_boundary_text_and_wrong_root_owner_are_refused(self):
        value, objects = fixture(); files = files_for(value, objects); files[apk.README] = b'Approved for release\n'
        with self.assertRaises(ReviewError): check(value, objects, copied=files, source=files)
        files = files_for(value, objects)
        for main, owner in [(False, apk.ROOT), (True, 'github.com/example/other')]:
            with self.assertRaises(ReviewError): apk.summarize(packet(files, main=main, owner=owner), value['installedPackages'], lambda path, limit: files[path])

    def test_wire_links_truncation_and_unknown_schema_are_refused(self):
        value, objects = fixture(); files = files_for(value, objects); raw = packet(files)
        for changed in (raw[:-1], raw.replace(b'\t1\t', b'\t2\t', 1)):
            with self.assertRaises(ReviewError): apk.summarize(changed, value['installedPackages'], lambda path, limit: files[path])
        value['schema'] = True
        with self.assertRaises(ReviewError): check(value, objects)

    def test_occurrence_pages_cannot_be_reassigned_emptied_or_reordered(self):
        for field, replacement in [('schema', True), ('origin', 'other'), ('notices', [])]:
            value, objects = fixture(); files = files_for(value, objects)
            manifest = json.loads(files[apk.MANIFEST]); pin = manifest['origins'][1]['noticePages'][0]
            page = json.loads(files[pin['path']]); page[field] = replacement
            raw = json.dumps(page).encode(); files[pin['path']] = raw; pin.update(bytes=len(raw), sha256=sha(raw))
            files[apk.MANIFEST] = json.dumps(manifest).encode()
            with self.assertRaises(ReviewError): apk.summarize(packet(files), value['installedPackages'], lambda path, limit: files[path])
        value, objects = fixture(); files = files_for(value, objects); manifest = json.loads(files[apk.MANIFEST])
        manifest['origins'][1]['noticePages'].reverse()
        manifest['origins'][1]['noticePages'][0]['path'] = apk.PAGES + 'demo-001.json'
        files[apk.MANIFEST] = json.dumps(manifest).encode()
        with self.assertRaises(ReviewError): apk.summarize(packet(files), value['installedPackages'], lambda path, limit: files[path])

    def test_versioned_license_reference_is_separate_from_recipe_inputs_and_acceptance(self):
        value, objects = fixture(); reference = b'License reference fixture\n'
        value['referenceDocuments'] = [{'identifier': 'MPL-2.0', 'url': apk.REFERENCE_URLS['MPL-2.0'],
            'path': apk.REFERENCES + 'MPL-2.0.txt', 'bytes': len(reference), 'sha256': sha(reference), 'payload': reference}]
        value['origins'][1]['referenceDocuments'] = ['MPL-2.0']
        with self.assertRaises(ReviewError): check(value, objects)
        for row in value['installedPackages']:
            if row['origin'] == 'demo': row['declaredLicense'] = 'MPL-2.0 AND MIT'
        result = check(value, objects)
        self.assertEqual((result['referenceDocumentCount'], result['recipePinnedSourceInputReferences']), (1, 2))
        self.assertFalse(result['legalReviewComplete'])
        value['referenceDocuments'][0]['url'] = 'https://example.com/changed.txt'
        with self.assertRaises(ReviewError): check(value, objects)


if __name__ == '__main__':
    unittest.main()

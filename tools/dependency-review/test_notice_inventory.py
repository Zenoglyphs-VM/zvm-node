"""Image notice framing, attribution and source-binding controls using data only."""

import base64
import copy
import hashlib
import json
import unittest

import notice_inventory as notice
from bounded import sha


def fixture():
    files = {'LICENSE': b'Copyright Public Authors\nOriginal license\n', 'NOTICE.md': b'Original third-party notice\n',
        'zvm-core/LICENSE': b'Original Core license\n', 'zvm-core/NOTICE.md': b'Original Core notice\n',
        'cmd/zvm-node/main.go': b'// Copyright Public Authors\npackage main\n',
        'cmd/zvm-node/artifact.json': b'{"fixture":true}', 'zvm-core/amount/amount.go': b'package amount\n'}
    objects = {sha(raw): raw for path, raw in files.items() if path.endswith(('LICENSE', 'NOTICE.md'))}
    objects[sha(b'SDK license\n')] = b'SDK license\n'
    header = b'// Copyright Public Authors\n'; objects[sha(header)] = header
    source = {'root': {'commit': '1' * 40, 'goModSha256': 'a' * 64, 'goSumSha256': 'b' * 64},
        'core': {'commit': '2' * 40, 'goModSha256': 'c' * 64, 'goSumSha256': 'd' * 64}}
    modules = []
    for key, identity in [(notice.ROOT, {'path': notice.ROOT, 'version': '', 'effectivePath': notice.ROOT, 'effectiveVersion': '', 'main': True}),
        (notice.CORE, {'path': notice.CORE, 'version': 'v0.0.0', 'effectivePath': './zvm-core', 'effectiveVersion': '', 'main': False}),
        ('std', {'path': 'std', 'version': 'go1.26.11', 'effectivePath': 'std', 'effectiveVersion': 'go1.26.11', 'main': False})]:
        names = ['LICENSE'] if key == 'std' else ['LICENSE', 'NOTICE.md']
        pins = []
        for name in names:
            raw = b'SDK license\n' if key == 'std' else files[('zvm-core/' if key == notice.CORE else '') + name]
            pins.append({'path': name, 'bytes': len(raw), 'sha256': sha(raw), 'selectionScope': 'SELECTED_PACKAGE_ANCESTOR'})
        modules.append({'identity': identity, 'notices': pins, 'reviewRequired': True, 'noCandidateNoticeFileObserved': False})
    modules.sort(key=lambda m: m['identity']['path'])

    def file_pin(path, data, group, header=None):
        value = {'path': path, 'bytes': len(data), 'sha256': sha(data), 'spdxIdentifiersObserved': [], 'selectionGroup': group}
        if header is not None: value['initialNoticeObject'] = sha(header)
        return value

    packages = [{'path': notice.ROOT + '/cmd/zvm-node', 'module': notice.ROOT,
        'selectedSourceFiles': [file_pin('cmd/zvm-node/main.go', files['cmd/zvm-node/main.go'], 'GoFiles', header)],
        'embeddedFiles': [file_pin('cmd/zvm-node/artifact.json', files['cmd/zvm-node/artifact.json'], 'EmbedFiles')]},
        {'path': notice.CORE + '/amount', 'module': notice.CORE,
         'selectedSourceFiles': [file_pin('amount/amount.go', files['zvm-core/amount/amount.go'], 'GoFiles')], 'embeddedFiles': []},
        {'path': 'unsafe', 'module': 'std', 'selectedSourceFiles': [], 'embeddedFiles': []}]
    packages.sort(key=lambda p: p['path'])
    commands = [{'label': label, 'actualExit': 0, 'outputComplete': True,
        'stdout': {'bytesCaptured': 1, 'sha256': sha(b'x'), 'limit': 8 << 20, 'truncated': False},
        'stderr': {'bytesCaptured': 0, 'sha256': sha(b''), 'limit': 256 << 10, 'truncated': False}}
        for label in ('profile', 'verify_before', 'selected_packages', 'verify_after')]
    manifest = {'schema': 2, 'status': 'SOURCE_NOTICE_BUNDLE_RECORDED_REVIEW_REQUIRED', 'entryPackage': notice.ROOT + '/cmd/zvm-node',
        'profile': {'GOVERSION': 'go1.26.11', 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'},
        'callerSourceCommit': source['root']['commit'], 'callerCoreCommit': source['core']['commit'], 'sourceCommitAuthenticated': False,
        'rootLockSha256': {'go.mod': 'a' * 64, 'go.sum': 'b' * 64}, 'coreLockSha256': {'go.mod': 'c' * 64, 'go.sum': 'd' * 64},
        'binarySha256': 'e' * 64, 'binaryBytes': 1024, 'modules': modules, 'packages': packages,
        'objects': {key: len(raw) for key, raw in objects.items()}, 'commands': commands, 'privatePathsExported': False,
        'legalReviewComplete': False, 'correspondingSourceOrRelinkingReview': 'NOT_ASSESSED', 'runtimeAPKLicenseTextReview': 'NOT_ASSESSED',
        'completeSBOM': False, 'releaseAccepted': False, 'binaryModuleIdentitiesMatched': True, 'sourceSelectionScope': notice.SOURCE_SCOPE}
    return manifest, objects, source, files


README = b'Source notice review remains required.\n'


def encode(manifest, objects, *, readme=README):
    def line(kind, key, raw):
        return b'\t'.join((kind.encode(), key.encode(), str(len(raw)).encode(), b'1', base64.b64encode(raw)))
    lines = [b'ZVM-NOTICES-1', line('M', 'manifest.json', json.dumps(manifest, sort_keys=True).encode()), line('R', 'README.txt', readme)]
    lines += [line('O', key, raw) for key, raw in sorted(objects.items())]
    lines.append(b'END\t' + str(len(objects)).encode())
    return b'\n'.join(lines) + b'\n'


def upstream_pin(raw):
    return {'bytes': len(raw), 'sha256': sha(raw),
        'gitBlob': hashlib.sha1(b'blob ' + str(len(raw)).encode() + b'\0' + raw).hexdigest()}


def supplement_fixture():
    values = fixture(); manifest, objects, _, files = values
    module = 'example.org/library'
    identity = {'path': module, 'version': 'v1.2.3', 'effectivePath': module, 'effectiveVersion': 'v1.2.3', 'main': False}
    text = b'Copyright Public Upstream Authors\nOriginal upstream notice\n'
    retained = 'tools/noticebundle/supplements/fixture/MIT-LICENSE.txt'
    source = b'package library\n'
    entry = {'module': copy.deepcopy(identity), 'repository': 'https://github.com/example/library', 'commit': '3' * 40,
        'moduleSubdirectory': 'v2', 'moduleGoMod': upstream_pin(b'module example.org/library\n\ngo 1.18\n'),
        'selectedFiles': {'library.go': upstream_pin(source), 'empty.dat': upstream_pin(b'')},
        'notice': {'path': 'MIT-LICENSE.txt', 'sourcePath': retained, **upstream_pin(text)},
        'status': notice.SUPPLEMENT_STATUS, 'legalReviewComplete': False,
        'independentOriginAuthenticated': False, 'completeSourceCoverage': False}
    files[retained] = text
    files[notice.REGISTRY_PATH] = json.dumps({'schema': 1, 'entries': [entry]}, sort_keys=True).encode()
    origin = {'registryPath': notice.REGISTRY_PATH, 'registrySha256': sha(files[notice.REGISTRY_PATH]), 'entry': entry}
    pin = {'path': 'MIT-LICENSE.txt', 'bytes': len(text), 'sha256': sha(text),
        'selectionScope': 'PINNED_UPSTREAM_SUPPLEMENT', 'upstreamSupplement': origin}
    manifest['modules'].append({'identity': identity, 'notices': [pin], 'reviewRequired': True, 'noCandidateNoticeFileObserved': False})
    manifest['modules'].sort(key=lambda row: row['identity']['path'])
    files_pin = lambda path, raw, group: {'path': path, 'bytes': len(raw), 'sha256': sha(raw),
        'spdxIdentifiersObserved': [], 'selectionGroup': group}
    manifest['packages'].append({'path': module, 'module': module,
        'selectedSourceFiles': [files_pin('library.go', source, 'GoFiles')],
        'embeddedFiles': [files_pin('empty.dat', b'', 'EmbedFiles')]})
    manifest['packages'].sort(key=lambda row: row['path'])
    objects[sha(text)] = text; manifest['objects'][sha(text)] = len(text)
    return values


def supplement_origin(values):
    return next(row for row in values[0]['modules'] if row['identity']['path'] == 'example.org/library')['notices'][0]['upstreamSupplement']


def sync_registry(values):
    origin = supplement_origin(values)
    raw = json.dumps({'schema': 1, 'entries': [origin['entry']]}, sort_keys=True).encode()
    values[3][notice.REGISTRY_PATH] = raw; origin['registrySha256'] = sha(raw)


class NoticeInventoryTests(unittest.TestCase):
    def check(self, values=None, raw=None):
        manifest, objects, source, files = fixture() if values is None else values
        return notice.summarize_export(encode(manifest, objects) if raw is None else raw, source, 'e' * 64, README,
            lambda path, limit: files[path])

    def reject(self, values):
        with self.assertRaises((ValueError, KeyError, TypeError)):
            self.check(values)

    def test_original_notice_objects_and_embedded_inputs_are_bound(self):
        result = self.check()
        self.assertEqual((result['runtimePackageCount'], result['runtimeModuleCount']), (3, 3))
        self.assertEqual(result['embeddedFiles'][0]['path'], 'cmd/zvm-node/artifact.json')
        self.assertEqual(result['checkoutFilesIndependentlyMatched'], 7)
        self.assertFalse(result['legalReviewComplete'])
        self.assertFalse(result['releaseAccepted'])

    def test_partial_or_duplicate_export_cannot_qualify(self):
        values = fixture(); raw = encode(values[0], values[1]); lines = raw.splitlines()
        variants = [raw[:-1], b'\n'.join(lines[:-1]) + b'\n', raw + b'extra\n',
            b'\n'.join(lines[:-1] + [lines[3], lines[-1]]) + b'\n']
        for bad in variants:
            with self.subTest(size=len(bad)), self.assertRaises((ValueError, TypeError)):
                self.check(values, raw=bad)

    def test_export_links_paths_sizes_and_digests_are_checked(self):
        values = fixture(); raw = encode(values[0], values[1])
        variants = [raw.replace(b'M\tmanifest.json', b'M\t../manifest.json', 1),
            raw.replace(b'\t1\t', b'\t2\t', 1), raw.replace(b'ZVM-NOTICES-1', b'ZVM-NOTICES-2', 1),
            raw.replace(b'END\t6', b'END\t999', 1)]
        # Change one payload while leaving its digest key unchanged.
        key = sorted(values[1])[0].encode(); lines = raw.splitlines()
        for i, line in enumerate(lines):
            if line.startswith(b'O\t' + key + b'\t'):
                parts = line.split(b'\t'); parts[-1] = base64.b64encode(b'changed'); parts[2] = b'7'
                lines[i] = b'\t'.join(parts)
        variants.append(b'\n'.join(lines) + b'\n')
        for bad in variants:
            with self.assertRaises(ValueError): self.check(values, raw=bad)

    def test_source_revisions_locks_profile_and_binary_cannot_be_relabelled(self):
        for field, value in [('callerSourceCommit', '0' * 40), ('callerCoreCommit', '0' * 40),
                             ('binarySha256', '0' * 64), ('rootLockSha256', {}), ('coreLockSha256', {})]:
            values = fixture(); values[0][field] = value; self.reject(values)
        values = fixture(); values[0]['profile']['CGO_ENABLED'] = '1'; self.reject(values)

    def test_legal_approval_and_origin_cannot_be_inferred(self):
        for field in ('sourceCommitAuthenticated', 'privatePathsExported', 'legalReviewComplete', 'completeSBOM', 'releaseAccepted'):
            values = fixture(); values[0][field] = True; self.reject(values)
        values = fixture(); values[0]['correspondingSourceOrRelinkingReview'] = 'APPROVED'; self.reject(values)

    def test_changed_project_notice_is_not_hidden_by_a_new_digest(self):
        values = fixture(); manifest, objects, _, _ = values
        module = next(m for m in manifest['modules'] if m['identity']['path'] == notice.ROOT)
        pin = next(p for p in module['notices'] if p['path'] == 'LICENSE')
        old = pin['sha256']; data = b'Rewritten notice\n'; new = sha(data)
        del objects[old]; del manifest['objects'][old]; objects[new] = data; manifest['objects'][new] = len(data)
        pin.update(sha256=new, bytes=len(data)); self.reject(values)

    def test_missing_required_notice_or_payload_is_refused(self):
        values = fixture(); module = next(m for m in values[0]['modules'] if m['identity']['path'] == notice.CORE)
        module['notices'].pop(); self.reject(values)
        values = fixture(); values[1].pop(sorted(values[1])[0]); self.reject(values)

    def test_initial_header_payload_and_spdx_fields_are_checked(self):
        values = fixture(); source = next(p for p in values[0]['packages'] if p['module'] == notice.ROOT)['selectedSourceFiles'][0]
        source['spdxIdentifiersObserved'] = ['MIT\nprivate']; self.reject(values)
        values = fixture(); source = next(p for p in values[0]['packages'] if p['module'] == notice.ROOT)['selectedSourceFiles'][0]
        source['initialNoticeObject'] = sha(b'SDK license\n'); self.reject(values)

    def test_selected_file_hash_and_private_paths_are_checked(self):
        values = fixture(); values[3]['cmd/zvm-node/main.go'] = b'changed'; self.reject(values)
        values = fixture(); values[0]['packages'][0]['path'] = '/fixture/private'; self.reject(values)

    def test_unversioned_dependency_or_changed_local_core_is_refused(self):
        values = fixture(); core = next(m for m in values[0]['modules'] if m['identity']['path'] == notice.CORE)
        core['identity']['effectivePath'] = '../core'; self.reject(values)
        values = fixture(); core = next(m for m in values[0]['modules'] if m['identity']['path'] == notice.CORE)
        core['identity']['version'] = 'v0.0.1'; self.reject(values)

    def test_failed_incomplete_or_truncated_metadata_is_refused(self):
        for change in ('exit', 'complete', 'truncated', 'commands'):
            values = fixture(); row = values[0]['commands'][0]
            if change == 'exit': row['actualExit'] = 1
            elif change == 'complete': row['outputComplete'] = False
            elif change == 'truncated': row['stdout']['truncated'] = True
            else: values[0]['commands'].pop()
            self.reject(values)

    def test_review_readme_must_match_checked_source(self):
        values = fixture()
        with self.assertRaises(ValueError): self.check(values, raw=encode(values[0], values[1], readme=b'approved'))

    def test_package_namespace_cannot_be_assigned_to_another_module(self):
        values = fixture(); row = next(p for p in values[0]['packages'] if p['module'] == notice.ROOT)
        row['module'] = notice.CORE; self.reject(values)

    def test_same_source_path_cannot_have_conflicting_observations(self):
        values = fixture(); row = next(p for p in values[0]['packages'] if p['module'] == notice.ROOT)
        pin = copy.deepcopy(row['selectedSourceFiles'][0]); pin['selectionGroup'] = 'EmbedFiles'; pin['sha256'] = 'f' * 64
        row['embeddedFiles'].append(pin); row['embeddedFiles'].sort(key=lambda p: p['path']); self.reject(values)

    def test_duplicate_module_package_or_unreferenced_object_is_refused(self):
        values = fixture(); values[0]['modules'].append(copy.deepcopy(values[0]['modules'][0])); self.reject(values)
        values = fixture(); values[0]['packages'].append(copy.deepcopy(values[0]['packages'][0])); self.reject(values)
        values = fixture(); extra = b'extra'; values[1][sha(extra)] = extra; values[0]['objects'][sha(extra)] = len(extra); self.reject(values)

    def test_historical_manifest_schema_is_not_relabelled(self):
        values = fixture(); values[0]['schema'] = 1; self.reject(values)

    def test_upstream_supplement_binds_registry_notice_and_selected_files(self):
        result = self.check(supplement_fixture())
        self.assertEqual(result['upstreamSourceSupplementCount'], 1)
        self.assertEqual(result['upstreamSourceSupplementModules'], ['example.org/library'])
        self.assertEqual(result['modulesWithOnlySupplementalNoticeFiles'], ['example.org/library'])
        self.assertEqual(result['checkoutFilesIndependentlyMatched'], 9)
        self.assertFalse(result['legalReviewComplete'])

    def test_supplement_registry_and_entry_cannot_be_substituted(self):
        for change in ('registry_digest', 'revision', 'module_version', 'registry_path', 'duplicate'):
            values = supplement_fixture(); origin = supplement_origin(values)
            if change == 'registry_digest': origin['registrySha256'] = 'a' * 64
            elif change == 'revision': origin['entry']['commit'] = '4' * 40
            elif change == 'module_version': origin['entry']['module']['version'] = 'v1.2.4'; sync_registry(values)
            elif change == 'registry_path': origin['registryPath'] = 'other.json'
            else:
                entry = origin['entry']; raw = json.dumps({'schema': 1, 'entries': [entry, entry]}).encode()
                values[3][notice.REGISTRY_PATH] = raw; origin['registrySha256'] = sha(raw)
            self.reject(values)

    def test_supplement_requires_exact_source_set_hashes_and_empty_embeds(self):
        for change in ('missing_file', 'extra_file', 'digest', 'bytes', 'blob'):
            values = supplement_fixture(); entry = supplement_origin(values)['entry']
            if change == 'missing_file': del entry['selectedFiles']['empty.dat']
            elif change == 'extra_file': entry['selectedFiles']['other.go'] = upstream_pin(b'package other\n')
            elif change == 'digest': entry['selectedFiles']['library.go']['sha256'] = 'a' * 64
            elif change == 'bytes': entry['selectedFiles']['empty.dat']['bytes'] = 1
            else: entry['selectedFiles']['library.go']['gitBlob'] = 'invalid'
            sync_registry(values); self.reject(values)

    def test_supplement_legal_origin_and_completeness_flags_stay_false(self):
        for field in ('legalReviewComplete', 'independentOriginAuthenticated', 'completeSourceCoverage'):
            values = supplement_fixture(); supplement_origin(values)['entry'][field] = True
            sync_registry(values); self.reject(values)
        values = supplement_fixture(); del supplement_origin(values)['entry']['legalReviewComplete']
        sync_registry(values); self.reject(values)

    def test_supplement_rewritten_notice_and_blob_are_refused(self):
        values = supplement_fixture(); entry = supplement_origin(values)['entry']; values[3][entry['notice']['sourcePath']] = b'changed'; self.reject(values)
        values = supplement_fixture(); entry = supplement_origin(values)['entry']; entry['notice']['gitBlob'] = 'a' * 40; sync_registry(values); self.reject(values)

    def test_supplement_scope_requires_origin_on_the_same_notice(self):
        values = supplement_fixture()
        row = next(row for row in values[0]['modules'] if row['identity']['path'] == 'example.org/library')['notices'][0]
        del row['upstreamSupplement']; self.reject(values)
        values = supplement_fixture()
        next(row for row in values[0]['modules'] if row['identity']['path'] == 'example.org/library')['notices'][0]['selectionScope'] = 'SELECTED_PACKAGE_ANCESTOR'
        self.reject(values)

    def test_supplement_rejects_private_paths_unpinned_origins_and_extra_fields(self):
        for change in ('source_path', 'repository', 'commit', 'subdirectory', 'unknown_field', 'schema'):
            values = supplement_fixture(); entry = supplement_origin(values)['entry']
            if change == 'source_path': entry['notice']['sourcePath'] = '../outside'
            elif change == 'repository': entry['repository'] = 'https://example.invalid/private'
            elif change == 'commit': entry['commit'] = 'main'
            elif change == 'subdirectory': entry['moduleSubdirectory'] = '../outside'
            elif change == 'unknown_field': entry['extra'] = True
            else: values[0]['schema'] = 3
            sync_registry(values); self.reject(values)


APK = b'ZVM-APK-LICENSES-2\nP:fixture\nV:1.0-r0\nA:x86_64\nC:Q1' + b'a' * 27 + b'\nL:MIT\no:fixture\nc:' + b'1' * 40 + b'\nI:4096\n\nEND\t1\n'


class APKNoticeTests(unittest.TestCase):
    def test_installed_identity_and_declared_license_remain_separate_from_approval(self):
        rows = notice.apk_licenses(APK, ['fixture-1.0-r0'])
        self.assertEqual(rows[0]['declaredLicense'], 'MIT')
        self.assertFalse(rows[0]['licenseTextOrLegalQualificationEstablished'])
        self.assertEqual(rows[0]['declaredBuildCommit'], '1' * 40)
        self.assertEqual(rows[0]['declaredInstalledBytes'], 4096)

    def test_missing_license_stays_unresolved(self):
        rows = notice.apk_licenses(APK.replace(b'L:MIT\n', b''), ['fixture-1.0-r0'])
        self.assertIsNone(rows[0]['declaredLicense'])

    def test_mismatched_or_duplicate_installed_records_are_refused(self):
        for raw, packages in ((APK, ['other-1.0-r0']), (APK + APK, ['fixture-1.0-r0']), (APK.replace(b'V:1.0-r0', b'V:../version'), ['fixture-1.0-r0'])):
            with self.assertRaises(ValueError): notice.apk_licenses(raw, packages)

    def test_unrequested_metadata_and_partial_records_are_refused(self):
        for raw in (APK + b'm:unrequested\n', APK[:-1], APK.replace(b'P:fixture\n', b'P:fixture\nP:other\n'), APK.replace(b'C:Q1' + b'a' * 27 + b'\n', b'')):
            with self.assertRaises(ValueError): notice.apk_licenses(raw, ['fixture-1.0-r0'])

    def test_missing_source_commit_stays_unknown_without_omitting_package(self):
        raw = APK.replace(b'c:' + b'1' * 40 + b'\n', b'')
        self.assertIsNone(notice.apk_licenses(raw, ['fixture-1.0-r0'])[0]['declaredBuildCommit'])

    def test_invalid_commit_size_and_historical_framing_are_refused(self):
        for raw in (APK.replace(b'c:' + b'1' * 40, b'c:main'), APK.replace(b'I:4096', b'I:-1'),
            APK.replace(b'I:4096', b'I:04096'), APK.replace(b'I:4096\n', b''),
            APK.replace(b'ZVM-APK-LICENSES-2', b'ZVM-APK-LICENSES-1')):
            with self.assertRaises(ValueError): notice.apk_licenses(raw, ['fixture-1.0-r0'])


if __name__ == '__main__':
    unittest.main()

"""Data-only refusal controls for the bounded image comparison consumers."""

import copy
import json
import unittest

import container_footprint as footprint
from bounded import sha
from test_notice_inventory import fixture, encode


def image(identifier='1', size=10000):
    return {'id': 'sha256:' + identifier * 64, 'os': 'linux', 'architecture': 'amd64',
        'entrypoint': ['zvm-node'], 'user': '', 'reportedSizeBytes': size}


def package(name):
    return {'name': name, 'version': '1.0-r0', 'architecture': 'x86_64', 'installedChecksum': 'Q1' + 'a' * 27,
        'origin': name, 'declaredLicense': 'MIT', 'declaredBuildCommit': 'a' * 40, 'declaredInstalledBytes': 512,
        'licenseTextOrLegalQualificationEstablished': False}


def comparison_fixture():
    candidate = {'image': image('1', 10000), 'cliBinarySha256': 'b' * 64,
        'recipe': {'path': 'Dockerfile', 'sha256': 'c' * 64, 'sourceCommit': 'd' * 40},
        'runtimeAPKLicenseDeclarations': [package('busybox'), package('gcompat')]}
    control = copy.deepcopy(candidate); control.update(image=image('2', 15000),
        recipe={'path': footprint.CONTROL_RECIPE, 'sha256': footprint.CONTROL_RECIPE_SHA, 'sourceCommit': footprint.PARENT})
    control['runtimeAPKLicenseDeclarations'] += [package(name) for name in sorted(footprint.REMOVED_PACKAGES)]
    projection = {'status': 'CONTROL_NOTICE_BYTES_MATCH_CANDIDATE'}
    health = {role: {'status': 'VERIFIER_LOOPBACK_HEALTH_AND_TEARDOWN_PASSED', 'ownedContainerRemoved': True,
        'imageId': value['image']['id']} for role, value in [('candidate', candidate), ('control', control)]}
    return candidate, control, projection, health


def projection_fixture():
    manifest, objects, _, _ = fixture(); exported = encode(manifest, objects)
    from notice_inventory import decode_export
    payloads = decode_export(exported)
    rows = [b'ZVM-NOTICE-DIGESTS-1']
    keys = ['manifest.json', 'README.txt'] + sorted(objects)
    for index, key in enumerate(keys):
        raw = payloads[key]
        rows.append(b'\t'.join((b'M' if index == 0 else b'R' if index == 1 else b'O', key.encode(),
            str(len(raw)).encode(), b'1', sha(raw).encode())))
    rows.append(b'END\t' + str(len(objects)).encode())
    return b'\n'.join(rows) + b'\n', exported


def health_fixture(role='candidate'):
    identifier = 'c' * 64
    labels = [role + '-' + name for name in ('create', 'start', 'inspect', 'provider', 'ready-0',
        'healthy-pipeline', 'unhealthy-pipeline', 'logs', 'stop', 'stopped', 'remove')]
    process = {label: {'actualExit': 1 if label.endswith('-unhealthy-pipeline') else 0,
        'outputComplete': True, 'terminationReason': None} for label in labels}
    inspected = {'id': identifier, 'image': 'sha256:' + '1' * 64, 'running': True, 'user': '65534:65534',
        'network': 'none', 'readonly': True, 'capDrop': ['ALL'], 'securityOpt': ['no-new-privileges'],
        'mounts': [], 'ports': {}}
    raw = {label: b'' for label in labels}
    for name in ('create', 'start', 'remove'): raw[role + '-' + name] = (identifier + '\n').encode()
    raw[role + '-inspect'] = json.dumps(inspected).encode()
    raw[role + '-provider'] = b'/usr/bin/wget\n' + (b'/bin/busybox\n' if role == 'candidate' else b'/usr/bin/wget\n')
    raw[role + '-ready-0'] = b'{"ok":true,"solcDir":"/solc"}\n'
    raw[role + '-stopped'] = json.dumps({'id': identifier, 'running': False, 'exitCode': 0}).encode()
    return process, raw


class ImageComparisonTests(unittest.TestCase):
    def test_same_source_reduction_preserves_shared_inputs_and_false_approval_flags(self):
        result = footprint.compare(*comparison_fixture())
        self.assertEqual(result['dockerReportedImageBytesReduction'], 5000)
        self.assertEqual(result['removedPackages'], sorted(footprint.REMOVED_PACKAGES))
        self.assertFalse(result['physicalDiskReclaimMeasured'])
        self.assertFalse(result['legalReviewComplete'])
        self.assertFalse(result['productionCompatibilityEstablished'])

    def test_unexpected_added_removed_and_changed_packages_are_refused(self):
        for change in ('addition', 'critical_removal', 'version', 'checksum', 'commit', 'duplicate'):
            values = comparison_fixture(); after = values[0]['runtimeAPKLicenseDeclarations']
            if change == 'addition': after.append(package('unexpected'))
            elif change == 'critical_removal': after.pop()
            elif change == 'duplicate': after.append(copy.deepcopy(after[0]))
            else: after[0][{'version': 'version', 'checksum': 'installedChecksum', 'commit': 'declaredBuildCommit'}[change]] = 'changed'
            with self.assertRaises(ValueError): footprint.compare(*values)

    def test_changed_binary_recipe_and_cross_image_health_are_refused(self):
        for change in ('binary', 'recipe', 'same_image', 'health', 'projection'):
            values = comparison_fixture()
            if change == 'binary': values[0]['cliBinarySha256'] = 'e' * 64
            elif change == 'recipe': values[1]['recipe']['sha256'] = 'e' * 64
            elif change == 'same_image': values[1]['image']['id'] = values[0]['image']['id']
            elif change == 'health': values[3]['candidate']['imageId'] = values[1]['image']['id']
            else: values[2]['status'] = 'UNKNOWN'
            with self.assertRaises(ValueError): footprint.compare(*values)

    def test_invalid_and_non_decreasing_size_claims_are_refused(self):
        for size in (True, '10000', 0, -1, 15000, 20000):
            values = comparison_fixture(); values[0]['image']['reportedSizeBytes'] = size
            with self.assertRaises((TypeError, ValueError)): footprint.compare(*values)

    def test_complete_control_digest_projection_reuses_candidate_payloads(self):
        raw, exported = projection_fixture(); result = footprint.digest_projection(raw, exported)
        self.assertEqual(result['status'], 'CONTROL_NOTICE_BYTES_MATCH_CANDIDATE')
        self.assertFalse(result['controlNoticePayloadBytesDuplicatedInEvidence'])

    def test_partial_duplicate_changed_and_linked_digest_rows_are_refused(self):
        raw, exported = projection_fixture(); rows = raw.splitlines()
        variants = [raw[:-1], b'\n'.join(rows[:-1]) + b'\n', raw.replace(b'\t1\t', b'\t2\t', 1),
            b'\n'.join(rows[:-1] + [rows[3], rows[-1]]) + b'\n',
            raw.replace(b'M\tmanifest.json', b'M\t../manifest.json', 1)]
        parts = rows[3].split(b'\t'); parts[-1] = b'f' * 64; changed = rows[:]; changed[3] = b'\t'.join(parts)
        variants.append(b'\n'.join(changed) + b'\n')
        for bad in variants:
            with self.assertRaises(ValueError): footprint.digest_projection(bad, exported)


class HealthComparisonTests(unittest.TestCase):
    def test_actual_status_positive_and_negative_pipelines_and_teardown_bind(self):
        for role in ('candidate', 'control'):
            process, raw = health_fixture(role)
            result = footprint.health_summary(role, process, raw.__getitem__)
            self.assertTrue(result['ownedContainerRemoved'])
            self.assertEqual(result['unhealthyPipelineExit'], 1)

    def test_root_network_mount_and_port_drift_are_refused(self):
        for key, value in [('user', '0'), ('network', 'host'), ('readonly', False), ('capDrop', []),
            ('securityOpt', []), ('mounts', [{'Type': 'bind'}]), ('ports', {'8600/tcp': []})]:
            process, raw = health_fixture(); row = json.loads(raw['candidate-inspect']); row[key] = value
            raw['candidate-inspect'] = json.dumps(row).encode()
            with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)

    def test_incomplete_accepted_negative_and_unremoved_service_are_refused(self):
        for change in ('truncated', 'positive', 'negative', 'exit', 'remove', 'provider'):
            process, raw = health_fixture()
            if change == 'truncated': process['candidate-ready-0']['outputComplete'] = False
            elif change == 'positive': process['candidate-healthy-pipeline']['actualExit'] = 1
            elif change == 'negative': process['candidate-unhealthy-pipeline']['actualExit'] = 0
            elif change == 'exit': raw['candidate-stopped'] = json.dumps({'id': 'c' * 64, 'running': False, 'exitCode': 137}).encode()
            elif change == 'remove': raw['candidate-remove'] = b'other\n'
            else: raw['candidate-provider'] = b'/usr/bin/wget\n/usr/bin/wget\n'
            with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)

    def test_readiness_attempts_remain_bounded_and_original_failures_are_preserved(self):
        process, raw = health_fixture(); ready = process.pop('candidate-ready-0'); body = raw.pop('candidate-ready-0')
        process['candidate-ready-0'] = {'actualExit': 1, 'outputComplete': True, 'terminationReason': None}; raw['candidate-ready-0'] = b''
        process['candidate-ready-1'] = ready; raw['candidate-ready-1'] = body
        self.assertEqual(footprint.health_summary('candidate', process, raw.__getitem__)['readyAttempts'], 2)
        for i in range(2, 9): process['candidate-ready-' + str(i)] = ready; raw['candidate-ready-' + str(i)] = body
        with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)

    def test_json_numbers_and_booleans_are_not_interchangeable_in_health_evidence(self):
        process, raw = health_fixture(); raw['candidate-ready-0'] = b'{"ok":1,"solcDir":"/solc"}'
        with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)
        process, raw = health_fixture(); raw['candidate-stopped'] = json.dumps({'id': 'c' * 64, 'running': 0, 'exitCode': False}).encode()
        with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)

    def test_unknown_readiness_sequence_and_missing_command_are_refused(self):
        process, raw = health_fixture(); process['candidate-ready-x'] = process.pop('candidate-ready-0')
        raw['candidate-ready-x'] = raw.pop('candidate-ready-0')
        with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)
        process, raw = health_fixture(); del process['candidate-stop']
        with self.assertRaises(ValueError): footprint.health_summary('candidate', process, raw.__getitem__)


if __name__ == '__main__':
    unittest.main()

"""Pure consumers for a same-source image comparison and loopback health checks."""

import json
import re

from bounded import ReviewError, sha, unique_object
from notice_inventory import decode_export

CONTROL_RECIPE = 'tools/dependency-review/fixtures/with-gnu-wget.Dockerfile'
CONTROL_RECIPE_SHA = '2797696d8ff61a1cbed4f40f69325e4660a3211a000f8ce32329fc3589b05ff2'
PARENT = '7e190b62461fc6645ed1f95a1d65630e4047b70e'
HEALTH_COMMAND = 'wget -qO- http://127.0.0.1:8600/status | grep -q ok'
UNHEALTHY_COMMAND = 'wget -qO- http://127.0.0.1:8600/status/missing | grep -q ok'
REMOVED_PACKAGES = {'wget', 'libidn2', 'libunistring', 'pcre2'}

DIGEST_SCRIPT = r'''
set -eu
root=/usr/share/doc/zvm-node/notices
emit() {
  kind=$1; key=$2; file=$3
  test -f "$file" && test ! -L "$file"
  set -- $(/bin/busybox stat -c '%s %h' "$file")
  test "$2" = 1
  sum=$(/bin/busybox sha256sum "$file"); sum=${sum%% *}
  printf '%s\t%s\t%s\t%s\t%s\n' "$kind" "$key" "$1" "$2" "$sum"
}
printf 'ZVM-NOTICE-DIGESTS-1\n'
emit M manifest.json "$root/manifest.json"
emit R README.txt "$root/README.txt"
count=0
for file in "$root"/objects/*; do
  key=${file##*/}; emit O "$key" "$file"; count=$((count+1))
done
printf 'END\t%s\n' "$count"
'''


def require(condition, message):
    if not condition:
        raise ReviewError(message)


def image(value):
    require(isinstance(value, dict) and set(value) == {'id', 'os', 'architecture', 'entrypoint', 'user', 'reportedSizeBytes'}, 'unexpected image projection')
    require(isinstance(value['id'], str) and re.fullmatch('sha256:[0-9a-f]{64}', value['id']) and
        value['os'] == 'linux' and value['architecture'] == 'amd64' and value['entrypoint'] == ['zvm-node'] and
        type(value['reportedSizeBytes']) is int and 0 < value['reportedSizeBytes'] <= 16 << 30, 'invalid selected image')
    return value


def digest_projection(raw, exported):
    require(isinstance(raw, bytes) and 0 < len(raw) <= 2 << 20 and raw.endswith(b'\n'), 'incomplete notice digest projection')
    rows = raw.splitlines(); require(4 <= len(rows) <= 8196 and rows[0] == b'ZVM-NOTICE-DIGESTS-1', 'unknown notice digest framing')
    observed = {}
    for i, row in enumerate(rows[1:-1]):
        fields = row.split(b'\t'); require(len(fields) == 5, 'malformed notice digest row')
        kind, key, size, links, digest = fields
        require(links == b'1' and re.fullmatch(rb'0|[1-9][0-9]{0,8}', size) and re.fullmatch(rb'[0-9a-f]{64}', digest), 'unsafe notice digest row')
        require(kind == (b'M' if i == 0 else b'R' if i == 1 else b'O'), 'notice digest row order differs')
        require(key == b'manifest.json' if i == 0 else key == b'README.txt' if i == 1 else re.fullmatch(rb'[0-9a-f]{64}', key), 'unsafe notice digest key')
        key = key.decode(); require(key not in observed, 'duplicate notice digest row')
        observed[key] = {'bytes': int(size), 'sha256': digest.decode()}
    count = len(observed) - 2
    require(count > 0 and rows[-1] == b'END\t' + str(count).encode(), 'notice digest end marker differs')
    payloads = decode_export(exported)
    expected = {key: {'bytes': len(payload), 'sha256': sha(payload)} for key, payload in payloads.items()}
    require(observed == expected, 'control notice files differ from the candidate payloads')
    return {'status': 'CONTROL_NOTICE_BYTES_MATCH_CANDIDATE', 'manifestSha256': observed['manifest.json']['sha256'],
        'readmeSha256': observed['README.txt']['sha256'], 'noticeObjects': count,
        'controlNoticePayloadBytesDuplicatedInEvidence': False}


def health_summary(role, process, raw):
    require(role in ('candidate', 'control'), 'unknown health role')
    required = {role + '-' + value for value in ('create', 'start', 'inspect', 'provider', 'healthy-pipeline',
        'unhealthy-pipeline', 'logs', 'stop', 'stopped', 'remove')}
    require(required <= set(process), 'health command evidence missing')
    for label in required | {key for key in process if key.startswith(role + '-ready-')}:
        value = process[label]
        allowed = (1,) if label == role + '-unhealthy-pipeline' else (0, 1, 8) if '-ready-' in label else (0,)
        require(type(value['actualExit']) is int and value['actualExit'] in allowed and
            value['outputComplete'] is True and value['terminationReason'] is None, 'health command failed or incomplete')
    identifier = raw(role + '-create').decode().strip()
    require(re.fullmatch('[0-9a-f]{64}', identifier), 'unknown owned container identifier')
    require(raw(role + '-start').decode().strip() == identifier, 'started container differs')
    inspected = json.loads(raw(role + '-inspect'), object_pairs_hook=unique_object)
    require(set(inspected) == {'id', 'image', 'running', 'user', 'network', 'readonly', 'capDrop', 'securityOpt', 'mounts', 'ports'}, 'unexpected health isolation projection')
    require(inspected['id'] == identifier and inspected['running'] is True and inspected['user'] == '65534:65534'
        and inspected['network'] == 'none' and inspected['readonly'] is True and inspected['capDrop'] == ['ALL']
        and inspected['securityOpt'] in (['no-new-privileges'], ['no-new-privileges:true'])
        and inspected['mounts'] == [] and inspected['ports'] in (None, {}), 'health container isolation differs')
    provider = raw(role + '-provider').decode().splitlines()
    require(len(provider) == 2 and provider[0] in ('/usr/bin/wget', '/bin/wget') and
        provider[1] == ('/bin/busybox' if role == 'candidate' else provider[0]), 'unexpected wget command provider')
    ready = [label for label in process if label.startswith(role + '-ready-')]
    require(1 <= len(ready) <= 8, 'unbounded or missing readiness attempts')
    require(ready == [role + '-ready-' + str(index) for index in range(len(ready))], 'readiness sequence differs')
    success = [label for label in ready if process[label]['actualExit'] == 0]
    require(len(success) == 1 and success[0] == ready[-1], 'readiness attempt outcomes differ')
    body = json.loads(raw(success[0]), object_pairs_hook=unique_object)
    require(isinstance(body, dict) and set(body) == {'ok', 'solcDir'} and body['ok'] is True and
        body['solcDir'] == '/solc', 'unexpected verifier status body')
    require(process[role + '-healthy-pipeline']['actualExit'] == 0 and process[role + '-unhealthy-pipeline']['actualExit'] == 1,
        'health command accepted a missing route or rejected status')
    stopped = json.loads(raw(role + '-stopped'), object_pairs_hook=unique_object)
    require(isinstance(stopped, dict) and set(stopped) == {'id', 'running', 'exitCode'} and stopped['id'] == identifier
        and stopped['running'] is False and type(stopped['exitCode']) is int and stopped['exitCode'] == 0,
        'verifier did not stop cleanly')
    require(raw(role + '-remove').decode().strip() == identifier, 'owned container removal differs')
    return {'status': 'VERIFIER_LOOPBACK_HEALTH_AND_TEARDOWN_PASSED', 'imageId': inspected['image'],
        'wgetCommandPath': provider[0], 'wgetResolvedPath': provider[1], 'readyAttempts': len(ready),
        'healthyPipelineExit': 0, 'unhealthyPipelineExit': 1, 'serviceExit': 0, 'ownedContainerRemoved': True,
        'scope': 'Verifier /status and missing-route transport only; no compilation, Engine, L1 or L2 execution.'}


def compare(candidate, control, projection, health):
    image(candidate['image']); image(control['image'])
    require(all(isinstance(v['cliBinarySha256'], str) and re.fullmatch('[0-9a-f]{64}', v['cliBinarySha256'])
        for v in (candidate, control)), 'invalid CLI digest in image comparison')
    require(candidate['image']['id'] != control['image']['id'], 'comparison images are not distinct')
    require(candidate['cliBinarySha256'] == control['cliBinarySha256'], 'CLI binary differs between recipes')
    require(control['recipe'] == {'path': CONTROL_RECIPE, 'sha256': CONTROL_RECIPE_SHA, 'sourceCommit': PARENT}, 'control recipe is not the frozen parent')
    require(candidate['recipe']['path'] == 'Dockerfile' and candidate['recipe']['sha256'] != CONTROL_RECIPE_SHA, 'candidate recipe did not change')
    before = {r['name']: r for r in control['runtimeAPKLicenseDeclarations']}
    after = {r['name']: r for r in candidate['runtimeAPKLicenseDeclarations']}
    require(len(before) == len(control['runtimeAPKLicenseDeclarations']) and len(after) == len(candidate['runtimeAPKLicenseDeclarations']), 'duplicate APK package identity')
    removed = set(before) - set(after)
    require(removed == REMOVED_PACKAGES and not set(after) - set(before), 'unexpected package-set change')
    require(all(after[key] == before[key] for key in after), 'shared APK identity or metadata changed')
    for rows in (before, after):
        require(all(type(r['declaredInstalledBytes']) is int and r['declaredInstalledBytes'] >= 0 and
            r['licenseTextOrLegalQualificationEstablished'] is False for r in rows.values()), 'invalid package size or legal claim')
    for role, value in health.items():
        require(value['status'] == 'VERIFIER_LOOPBACK_HEALTH_AND_TEARDOWN_PASSED' and value['ownedContainerRemoved']
            and value['imageId'] == (candidate if role == 'candidate' else control)['image']['id'], 'health evidence is for another image')
    require(set(health) == {'candidate', 'control'} and projection['status'] == 'CONTROL_NOTICE_BYTES_MATCH_CANDIDATE', 'comparison controls incomplete')
    size_before, size_after = control['image']['reportedSizeBytes'], candidate['image']['reportedSizeBytes']
    require(size_before > size_after, 'reported image footprint did not decrease')
    return {'schema': 1, 'status': 'SAME_SOURCE_IMAGE_RECIPE_REDUCTION_OBSERVED',
        'controlScope': 'Current source under the frozen parent recipe; not qualification of a historical parent image.',
        'packageCountBefore': len(before), 'packageCountAfter': len(after), 'removedPackages': sorted(removed),
        'commonPackageMetadataUnchanged': True, 'cliBinaryUnchanged': True, 'noticeFilesUnchanged': True,
        'dockerReportedImageBytesBefore': size_before, 'dockerReportedImageBytesAfter': size_after,
        'dockerReportedImageBytesReduction': size_before - size_after,
        'apkDeclaredInstalledBytesBefore': sum(r['declaredInstalledBytes'] for r in before.values()),
        'apkDeclaredInstalledBytesAfter': sum(r['declaredInstalledBytes'] for r in after.values()),
        'missingDeclaredSourceCommits': sorted(r['name'] for r in after.values() if r['declaredBuildCommit'] is None),
        'physicalDiskReclaimMeasured': False, 'completeSBOM': False, 'legalReviewComplete': False,
        'productionCompatibilityEstablished': False, 'releaseAccepted': False}

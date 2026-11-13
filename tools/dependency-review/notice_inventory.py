"""Check one runtime image's source-notice packet without legal approval."""

import base64
import hashlib
import json
from pathlib import Path
import re

from bounded import ReviewError, file_bytes, sha, unique_object
from govulncheck_review import reject_constant
from import_graph import path as public_path, VERSION

ROOT = 'github.com/Zenoglyphs-VM/zvm-node'
CORE = 'github.com/Zenoglyphs-VM/zvm-core'
PREFIX = '/usr/share/doc/zvm-node/notices'
HEX = re.compile(r'[0-9a-f]{64}')
MAX_MANIFEST = 8 << 20
MAX_PAYLOAD = 8 << 20
MAX_EXPORT = 24 << 20
GROUPS = {'GoFiles', 'CgoFiles', 'CFiles', 'CXXFiles', 'MFiles', 'HFiles', 'FFiles', 'SFiles', 'SwigFiles', 'SwigCXXFiles', 'SysoFiles'}
SCOPES = {'SELECTED_PACKAGE_ANCESTOR', 'MODULE_NOTICE_DIRECTORY', 'REQUIRED_PROJECT_NOTICE', 'REQUIRED_SDK_NOTICE', 'PINNED_UPSTREAM_SUPPLEMENT'}
SOURCE_SCOPE = 'Selected non-test CLI source inputs and embedded files; compiler intermediates are not exported.'
REGISTRY_PATH = 'tools/noticebundle/notice-supplements.json'
SUPPLEMENT_STATUS = 'UPSTREAM_SOURCE_BYTES_MATCHED_REVIEW_REQUIRED'
NOTICE_FILE = re.compile(r'(?:[a-z0-9.+_-]+[-_.])?(licen[cs]es?|copying|notice|copyright|authors|contributors|patents|legal|unlicense)(?:[._-].*)?', re.I)

# Only fixed image paths and digest-named payloads are read. No tar extraction,
# host bind mount, service, network or additional image publication is involved.
EXPORT_SCRIPT = r'''
set -eu
root=/usr/share/doc/zvm-node/notices
emit() {
  kind=$1; key=$2; file=$3
  test -f "$file" && test ! -L "$file"
  links=$(/bin/busybox stat -c %h "$file")
  size=$(/bin/busybox stat -c %s "$file")
  test "$links" = 1
  printf '%s\t%s\t%s\t%s\t' "$kind" "$key" "$size" "$links"
  /bin/busybox base64 "$file" | /bin/busybox tr -d '\n'
  printf '\n'
}
printf 'ZVM-NOTICES-1\n'
emit M manifest.json "$root/manifest.json"
emit R README.txt "$root/README.txt"
count=0
for file in "$root"/objects/*; do
  key=${file##*/}
  emit O "$key" "$file"
  count=$((count+1))
done
printf 'END\t%s\n' "$count"
'''

APK_SCRIPT = r'''/bin/busybox awk 'BEGIN { print "ZVM-APK-LICENSES-1" } /^P:/ { count++ } /^[PVACLo]:/ { print } /^$/ { print "" } END { printf "END\t%d\n", count }' /lib/apk/db/installed'''


def require(condition, message):
    if not condition:
        raise ReviewError(message)


def fields(value, names):
    require(isinstance(value, dict) and set(value) == set(names), 'unexpected notice object fields')


def integer(value, low, high):
    require(type(value) is int and low <= value <= high, 'invalid bounded notice integer')


def digest(value):
    require(isinstance(value, str) and HEX.fullmatch(value), 'invalid notice digest')


def records(value, limit):
    require(isinstance(value, list) and len(value) <= limit, 'invalid bounded notice list')
    return value


def checked_json(raw):
    return json.loads(raw, object_pairs_hook=unique_object, parse_constant=reject_constant)


def decode_export(raw):
    require(isinstance(raw, bytes) and 0 < len(raw) <= MAX_EXPORT and raw.endswith(b'\n'), 'notice export incomplete or oversized')
    lines = raw.splitlines()
    require(4 <= len(lines) <= 8196 and lines[0] == b'ZVM-NOTICES-1', 'unknown notice export framing')
    result = {}
    object_count, total = 0, 0
    for index, line in enumerate(lines[1:-1]):
        parts = line.split(b'\t')
        require(len(parts) == 5, 'invalid notice export record')
        kind, key, size, links, encoded = parts
        require(re.fullmatch(rb'0|[1-9][0-9]{0,8}', size) and links == b'1', 'invalid notice file size or link count')
        size = int(size)
        expected_kind = b'M' if index == 0 else b'R' if index == 1 else b'O'
        require(kind == expected_kind, 'notice export record order differs')
        if kind == b'M':
            require(key == b'manifest.json' and size <= MAX_MANIFEST, 'invalid manifest record')
        elif kind == b'R':
            require(key == b'README.txt' and size <= 65536, 'invalid review readme record')
        else:
            require(re.fullmatch(rb'[0-9a-f]{64}', key) and size <= 128 << 10, 'invalid notice object key or cap')
            object_count += 1; total += size
            require(total <= MAX_PAYLOAD, 'notice payload total exceeds cap')
        key = key.decode('ascii')
        require(key not in result, 'duplicate notice export key')
        require(len(encoded) == 4 * ((size + 2) // 3), 'notice base64 length differs from declared size')
        payload = base64.b64decode(encoded, validate=True)
        require(len(payload) == size, 'notice export payload size differs')
        if kind == b'O':
            require(sha(payload) == key, 'notice object digest differs')
        result[key] = payload
    require(lines[-1] == b'END\t' + str(object_count).encode(), 'notice export end marker differs')
    require(object_count > 0, 'empty notice object selection')
    return result


def expected_readme():
    raw = file_bytes(Path('tools/noticebundle/main.go'), 128 << 10)
    values = re.findall(rb'const readme = `([^`]+)`', raw)
    require(len(values) == 1, 'source readme contract differs')
    return values[0]


def upstream_pin(value, limit, *, minimum=0):
    fields(value, ('bytes', 'sha256', 'gitBlob'))
    integer(value['bytes'], minimum, limit); digest(value['sha256'])
    require(isinstance(value['gitBlob'], str) and re.fullmatch(r'[0-9a-f]{40}', value['gitBlob']), 'invalid upstream Git blob identifier')


def supplement_entry(entry):
    fields(entry, ('module', 'repository', 'commit', 'moduleSubdirectory', 'moduleGoMod', 'selectedFiles', 'notice',
        'status', 'legalReviewComplete', 'independentOriginAuthenticated', 'completeSourceCoverage'))
    identity = entry['module']; fields(identity, ('path', 'version', 'effectivePath', 'effectiveVersion', 'main'))
    require(public_path(identity['path']) and public_path(identity['effectivePath']) and identity['main'] is False
        and identity['path'] not in (ROOT, CORE, 'std') and isinstance(identity['version'], str)
        and VERSION.fullmatch(identity['version']) and isinstance(identity['effectiveVersion'], str)
        and VERSION.fullmatch(identity['effectiveVersion']), 'supplement module is not versioned')
    require(isinstance(entry['repository'], str) and re.fullmatch(r'https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+', entry['repository']), 'unsafe upstream repository reference')
    require(isinstance(entry['commit'], str) and re.fullmatch(r'[0-9a-f]{40}', entry['commit']), 'unpinned supplement revision')
    require(entry['moduleSubdirectory'] == '' or public_path(entry['moduleSubdirectory']), 'unsafe upstream module subdirectory')
    require(entry['status'] == SUPPLEMENT_STATUS and all(entry[k] is False for k in
        ('legalReviewComplete', 'independentOriginAuthenticated', 'completeSourceCoverage')), 'supplement overstates qualification')
    upstream_pin(entry['moduleGoMod'], 65536, minimum=1)
    selected = entry['selectedFiles']
    require(isinstance(selected, dict) and 0 < len(selected) <= 4096, 'missing or oversized supplement source set')
    for path, pin in selected.items():
        require(public_path(path), 'unsafe supplemented source path'); upstream_pin(pin, 32 << 20)
    require(sum(p['bytes'] for p in selected.values()) <= 256 << 20, 'supplement source budget exceeded')
    notice = entry['notice']; fields(notice, ('path', 'sourcePath', 'bytes', 'sha256', 'gitBlob'))
    upstream_pin({k: notice[k] for k in ('bytes', 'sha256', 'gitBlob')}, 128 << 10, minimum=1)
    require(public_path(notice['path']) and NOTICE_FILE.fullmatch(notice['path'].split('/')[-1]), 'unsafe upstream notice reference')
    require(public_path(notice['sourcePath']) and notice['sourcePath'].startswith('tools/noticebundle/supplements/'), 'unsafe checked-in supplement path')


def upstream_supplement(origin, pin, identity, payload, read_source):
    fields(origin, ('registryPath', 'registrySha256', 'entry'))
    require(origin['registryPath'] == REGISTRY_PATH, 'unknown source supplement registry')
    digest(origin['registrySha256'])
    raw = read_source(REGISTRY_PATH, 1 << 20)
    require(sha(raw) == origin['registrySha256'], 'supplement registry differs from checked source')
    registry = checked_json(raw); fields(registry, ('schema', 'entries'))
    require(type(registry['schema']) is int and registry['schema'] == 1, 'unknown supplement registry schema')
    entries = records(registry['entries'], 64)
    keys = []
    for entry in entries:
        supplement_entry(entry); keys.append((entry['module']['path'], entry['notice']['path']))
    require(len(set(keys)) == len(keys), 'duplicate registry supplement')
    entry = origin['entry']; supplement_entry(entry)
    require(sum(row == entry for row in entries) == 1 and entry['module'] == identity, 'supplement identity differs from selected registry entry')
    notice = entry['notice']
    require(all(pin[k] == notice[k] for k in ('path', 'bytes', 'sha256')), 'notice does not match its upstream supplement')
    retained = read_source(notice['sourcePath'], 128 << 10)
    require(retained == payload and sha(retained) == notice['sha256'] and len(retained) == notice['bytes'], 'retained upstream notice was rewritten')
    require(hashlib.sha1(b'blob ' + str(len(retained)).encode() + b'\0' + retained).hexdigest() == notice['gitBlob'], 'upstream notice Git blob differs')
    return entry


def summarize_export(raw, source, binary_sha, readme, read_source):
    payloads = decode_export(raw)
    require(payloads['README.txt'] == readme, 'review limitations readme differs from source')
    value = checked_json(payloads['manifest.json'])
    fields(value, ('schema', 'status', 'entryPackage', 'profile', 'callerSourceCommit', 'callerCoreCommit', 'sourceCommitAuthenticated',
        'rootLockSha256', 'coreLockSha256', 'binarySha256', 'binaryBytes', 'modules', 'packages', 'objects', 'commands',
        'privatePathsExported', 'legalReviewComplete', 'correspondingSourceOrRelinkingReview', 'runtimeAPKLicenseTextReview',
        'completeSBOM', 'releaseAccepted', 'binaryModuleIdentitiesMatched', 'sourceSelectionScope'))
    require(type(value['schema']) is int and value['schema'] == 2 and value['status'] == 'SOURCE_NOTICE_BUNDLE_RECORDED_REVIEW_REQUIRED', 'unknown notice manifest status')
    require(value['entryPackage'] == ROOT + '/cmd/zvm-node' and value['profile'] == {
        'GOVERSION': 'go1.26.11', 'GOOS': 'linux', 'GOARCH': 'amd64', 'CGO_ENABLED': '0'}, 'notice profile differs')
    require(value['callerSourceCommit'] == source['root']['commit'] and value['callerCoreCommit'] == source['core']['commit'], 'caller revisions differ from checked source')
    require(value['rootLockSha256'] == {'go.mod': source['root']['goModSha256'], 'go.sum': source['root']['goSumSha256']} and
        value['coreLockSha256'] == {'go.mod': source['core']['goModSha256'], 'go.sum': source['core']['goSumSha256']}, 'notice locks differ')
    require(all(value[key] is False for key in ('sourceCommitAuthenticated', 'privatePathsExported', 'legalReviewComplete', 'completeSBOM', 'releaseAccepted')), 'notice packet overstates qualification')
    require(value['correspondingSourceOrRelinkingReview'] == value['runtimeAPKLicenseTextReview'] == 'NOT_ASSESSED', 'legal or APK text review was inferred')
    require(value['binaryModuleIdentitiesMatched'] is True and value['sourceSelectionScope'] == SOURCE_SCOPE, 'binary or source selection scope differs')
    digest(value['binarySha256']); integer(value['binaryBytes'], 1, 128 << 20)
    require(value['binarySha256'] == binary_sha, 'notice packet is for a different binary')
    objects = value['objects']
    require(isinstance(objects, dict) and 0 < len(objects) <= 8192 and set(objects) == set(payloads) - {'manifest.json', 'README.txt'}, 'object inventory differs from export')
    for key, size in objects.items():
        digest(key); integer(size, 0, 128 << 10)
        require(len(payloads[key]) == size, 'notice object size differs from manifest')
    commands = records(value['commands'], 4)
    require([c['label'] for c in commands] == ['profile', 'verify_before', 'selected_packages', 'verify_after'], 'metadata command set incomplete')
    for command in commands:
        fields(command, ('label', 'actualExit', 'outputComplete', 'stdout', 'stderr'))
        require(type(command['actualExit']) is int and command['actualExit'] == 0 and command['outputComplete'] is True, 'Go metadata command failed or incomplete')
        for name, limit in [('stdout', 8 << 20), ('stderr', 256 << 10)]:
            pin = command[name]; fields(pin, ('bytesCaptured', 'sha256', 'limit', 'truncated'))
            integer(pin['bytesCaptured'], 0, limit); digest(pin['sha256'])
            require(pin['limit'] == limit and pin['truncated'] is False, 'metadata stream cap or completeness differs')
    modules, used_objects, checked_files, supplements = {}, set(), set(), []

    def checkout_file(module, relative, pin, notice_payload=None):
        if module not in (ROOT, CORE):
            return
        path = ('zvm-core/' if module == CORE else '') + relative
        content = read_source(path, 32 << 20)
        require(len(content) == pin['bytes'] and sha(content) == pin['sha256'], 'selected checkout file differs from packet')
        if notice_payload is not None:
            require(content == notice_payload, 'copied project notice was rewritten')
        checked_files.add(path)

    for module in records(value['modules'], 1024):
        fields(module, ('identity', 'notices', 'reviewRequired', 'noCandidateNoticeFileObserved'))
        identity = module['identity']; fields(identity, ('path', 'version', 'effectivePath', 'effectiveVersion', 'main'))
        key = identity['path']; require(public_path(key) and key not in modules, 'invalid or duplicate notice module')
        if key == ROOT:
            require(identity == {'path': ROOT, 'version': '', 'effectivePath': ROOT, 'effectiveVersion': '', 'main': True}, 'main module notice identity differs')
        elif key == CORE:
            require(identity == {'path': CORE, 'version': 'v0.0.0', 'effectivePath': './zvm-core', 'effectiveVersion': '', 'main': False}, 'local Core notice identity differs')
        elif key == 'std':
            require(identity == {'path': 'std', 'version': 'go1.26.11', 'effectivePath': 'std', 'effectiveVersion': 'go1.26.11', 'main': False}, 'SDK notice identity differs')
        else:
            require(identity['main'] is False and isinstance(identity['version'], str) and VERSION.fullmatch(identity['version'])
                and public_path(identity['effectivePath']) and isinstance(identity['effectiveVersion'], str)
                and VERSION.fullmatch(identity['effectiveVersion']), 'dependency notice identity is unversioned')
        require(module['reviewRequired'] is True and type(module['noCandidateNoticeFileObserved']) is bool, 'owner review flag missing')
        notices = records(module['notices'], 8192)
        require(module['noCandidateNoticeFileObserved'] == (not notices), 'missing notice accounting differs')
        paths = []
        for pin in notices:
            fields(pin, {'path', 'bytes', 'sha256', 'selectionScope'} | ({'upstreamSupplement'} if 'upstreamSupplement' in pin else set()))
            require(public_path(pin['path']) and pin['selectionScope'] in SCOPES, 'unsafe notice reference')
            integer(pin['bytes'], 0, 128 << 10); digest(pin['sha256'])
            require(pin['sha256'] in objects and pin['bytes'] == objects[pin['sha256']], 'notice payload missing')
            used_objects.add(pin['sha256']); paths.append(pin['path'])
            require(('upstreamSupplement' in pin) == (pin['selectionScope'] == 'PINNED_UPSTREAM_SUPPLEMENT'), 'supplement origin or scope is missing')
            if 'upstreamSupplement' in pin:
                entry = upstream_supplement(pin['upstreamSupplement'], pin, identity, payloads[pin['sha256']], read_source)
                supplements.append(entry); checked_files.update((REGISTRY_PATH, entry['notice']['sourcePath']))
            else:
                checkout_file(key, pin['path'], pin, payloads[pin['sha256']])
        require(paths == sorted(set(paths)), 'notice paths duplicate or unordered')
        if key in (ROOT, CORE): require({'LICENSE', 'NOTICE.md'} <= set(paths), 'required project source notices missing')
        if key == 'std': require('LICENSE' in paths, 'SDK license missing')
        modules[key] = module
    require({ROOT, CORE, 'std'} <= set(modules), 'required source modules missing')
    package_names, embedded, source_count, source_bytes = [], [], 0, 0
    source_pins = {}
    for package in records(value['packages'], 4096):
        fields(package, ('path', 'module', 'selectedSourceFiles', 'embeddedFiles'))
        name, module = package['path'], package['module']
        require(public_path(name) and module in modules, 'invalid package/module reference')
        require(module == 'std' or name == module or name.startswith(module + '/'), 'package is outside its nominal source module')
        package_names.append(name)
        for field, allowed in [('selectedSourceFiles', GROUPS - {'CgoFiles'}), ('embeddedFiles', {'EmbedFiles'})]:
            paths = []
            for pin in records(package[field], 4096):
                require(isinstance(pin, dict), 'invalid source pin')
                fields(pin, {'path', 'bytes', 'sha256', 'spdxIdentifiersObserved', 'selectionGroup'} | ({'initialNoticeObject'} if 'initialNoticeObject' in pin else set()))
                require(public_path(pin['path']) and pin['selectionGroup'] in allowed, 'invalid selected source path or kind')
                integer(pin['bytes'], 0, 32 << 20); digest(pin['sha256']); paths.append(pin['path'])
                key = (module, pin['path']); selected_pin = (pin['bytes'], pin['sha256'])
                require(key not in source_pins or source_pins[key] == selected_pin, 'conflicting selected source file observations')
                if key not in source_pins:
                    source_pins[key] = selected_pin; source_bytes += pin['bytes']
                    require(len(source_pins) <= 32768 and source_bytes <= 256 << 20, 'selected source aggregate budget exceeded')
                ids = records(pin['spdxIdentifiersObserved'], 128)
                require(all(isinstance(i, str) and re.fullmatch(r'[A-Za-z0-9.+() -]{1,256}', i) for i in ids)
                    and ids == sorted(set(ids)), 'invalid observed SPDX identifier')
                if 'initialNoticeObject' in pin:
                    ref = pin['initialNoticeObject']; digest(ref)
                    require(ref in objects, 'source header notice missing')
                    used_objects.add(ref)
                    if module in (ROOT, CORE):
                        path = ('zvm-core/' if module == CORE else '') + pin['path']
                        require(read_source(path, 32 << 20).startswith(payloads[ref]), 'initial source notice was rewritten')
                checkout_file(module, pin['path'], pin)
                if field == 'embeddedFiles': embedded.append({'package': name, 'module': module, **pin})
                else: source_count += 1
            require(paths == sorted(set(paths)), 'source paths duplicate or unordered')
    require(package_names == sorted(set(package_names)) and ROOT + '/cmd/zvm-node' in package_names, 'runtime package set incomplete or duplicate')
    require(used_objects == set(objects), 'unreferenced notice payload exported')
    for entry in supplements:
        observed = {path: {'bytes': size, 'sha256': digest} for (module, path), (size, digest) in source_pins.items() if module == entry['module']['path']}
        expected = {path: {k: pin[k] for k in ('bytes', 'sha256')} for path, pin in entry['selectedFiles'].items()}
        require(observed == expected, 'upstream supplement does not match the exact selected file set')
    return {'schema': 2, 'status': 'IMAGE_SOURCE_NOTICES_OBSERVED_REVIEW_REQUIRED',
        'manifestSha256': sha(payloads['manifest.json']), 'manifestBytes': len(payloads['manifest.json']),
        'readmeSha256': sha(readme), 'binarySha256': binary_sha, 'profile': value['profile'],
        'callerSourceCommit': value['callerSourceCommit'], 'callerCoreCommit': value['callerCoreCommit'],
        'rootLockSha256': value['rootLockSha256'], 'coreLockSha256': value['coreLockSha256'],
        'runtimePackageCount': len(package_names), 'runtimeModuleCount': len(modules),
        'selectedSourceFileCount': source_count, 'embeddedFiles': embedded,
        'modules': [modules[k] for k in sorted(modules)], 'noticeObjectCount': len(objects), 'noticePayloadBytes': sum(objects.values()),
        'modulesWithoutCandidateNoticeFiles': sorted(k for k, v in modules.items() if v['noCandidateNoticeFileObserved']),
        'upstreamSourceSupplementCount': len(supplements),
        'upstreamSourceSupplementModules': sorted({entry['module']['path'] for entry in supplements}),
        'modulesWithOnlySupplementalNoticeFiles': sorted(k for k, v in modules.items() if v['notices'] and
            all(p['selectionScope'] == 'PINNED_UPSTREAM_SUPPLEMENT' for p in v['notices'])),
        'checkoutFilesIndependentlyMatched': len(checked_files), 'metadataCommands': commands,
        'privatePathsExported': False, 'rawPrivateGoMetadataExported': False,
        'legalReviewComplete': False, 'completeSBOM': False, 'releaseAccepted': False}


def apk_licenses(raw, installed):
    require(isinstance(raw, bytes) and 0 < len(raw) <= 2 << 20 and raw.endswith(b'\n'), 'APK declaration output incomplete')
    lines = raw.decode('utf-8').splitlines()
    require(lines and lines[0] == 'ZVM-APK-LICENSES-1' and re.fullmatch(r'END\t[1-9][0-9]{0,3}', lines[-1]), 'APK export framing incomplete')
    declared_count = int(lines[-1].split('\t')[1])
    rows = []
    for block in '\n'.join(lines[1:-1]).split('\n\n'):
        if not block.strip(): continue
        record = {}
        for line in block.splitlines():
            key, separator, value = line.partition(':')
            require(separator and key in {'P', 'V', 'A', 'C', 'L', 'o'} and key not in record and len(value) <= 4096, 'APK declaration record ambiguous')
            record[key] = value
        require({'P', 'V', 'A', 'C'} <= set(record), 'APK identity fields missing')
        require(all(re.fullmatch(r'[A-Za-z0-9_.+~-]+', record[k]) for k in ('P', 'V', 'A')), 'invalid APK public identity')
        require(re.fullmatch(r'Q[12][A-Za-z0-9+/=]{20,128}', record['C']), 'invalid installed APK checksum')
        if 'o' in record: require(re.fullmatch(r'[A-Za-z0-9_.+~-]+', record['o']), 'invalid APK origin name')
        if 'L' in record: require(re.fullmatch(r'[A-Za-z0-9.+()_ /:-]{1,4096}', record['L']), 'invalid APK license declaration')
        rows.append({'name': record['P'], 'version': record['V'], 'architecture': record['A'],
            'installedChecksum': record['C'], 'origin': record.get('o'), 'declaredLicense': record.get('L'),
            'licenseTextOrLegalQualificationEstablished': False})
    require(0 < len(rows) <= 512 and len({r['name'] for r in rows}) == len(rows), 'APK inventory duplicate or oversized')
    require(len(rows) == declared_count, 'APK end marker record count differs')
    require({r['name'] + '-' + r['version'] for r in rows} == set(installed), 'APK declarations differ from installed packages')
    return sorted(rows, key=lambda r: r['name'])

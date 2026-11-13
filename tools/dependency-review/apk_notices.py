"""Bind retained APK notice candidates to installed metadata and checked source."""

import re
from urllib.parse import urlsplit

from bounded import ReviewError, sha
from notice_inventory import ROOT, checked_json, decode_export, fields, integer, digest
from import_graph import path as public_path

PREFIX = 'LICENSES/apk/'
MANIFEST = PREFIX + 'manifest.json'
README = PREFIX + 'README.txt'
PAGES = PREFIX + 'pages/'
REFERENCES = 'LICENSES/apk-references/'
REFERENCE_URLS = {'MPL-2.0': 'https://www.mozilla.org/MPL/2.0/index.txt'}
STATUS = 'APK_SOURCE_NOTICE_CANDIDATES_REVIEW_REQUIRED'
SCOPE = 'Installed APK declarations and selected original source notices; corresponding source and legal acceptance remain unqualified.'
FLAGS = ('legalReviewComplete', 'sourceOriginAuthenticated', 'completeCorrespondingSource',
    'completeLicenseCoverage', 'completeSBOM', 'releaseAccepted')
SOURCE_KINDS = {'SOURCE_ARCHIVE', 'APORTS_SOURCE_FILE', 'REMOTE_SOURCE_FILE'}
NOTICE_SCOPES = {'ARCHIVE_NOTICE_FILE', 'ARCHIVE_INITIAL_SOURCE_NOTICE', 'INITIAL_SOURCE_NOTICE'}
SOURCE_HOSTS = {'distfiles.alpinelinux.org', 'raw.githubusercontent.com', 'gitlab.alpinelinux.org',
    'busybox.net', 'git.adelielinux.org', 'distfiles.ariadne.space', 'musl.libc.org', 'github.com',
    'dev.gentoo.org', 'zlib.net', 'salsa.debian.org'}
README_BYTES = b'''APK source notice candidates

This catalog records exact installed APK metadata, declared aports recipe pins,
recipe-checksummed source inputs and selected original notice bytes. The Go
notice collector retains this directory through its existing module notice
directory support; identical payloads are stored once per digest in the image.
Bounded occurrence pages retain archive notice and initial-comment references.
Separate reference documents retain their own source and byte pins.

The APK consumer checks this catalog against installed package records, copied
payloads and the reviewed Node checkout. Source URLs and recipe commits remain
provider-selected references. They do not authenticate the binary's origin.

Missing source inputs and origins without observed notice candidates remain
explicit. Selected notices are not a complete copyright inventory, corresponding
source distribution, source offer, relinking kit, legal review or release
approval. Original upstream attribution and notice bytes are preserved.
'''


def require(condition, message):
    if not condition:
        raise ReviewError(message)


def filename(value):
    return isinstance(value, str) and 0 < len(value) <= 256 and re.fullmatch(r'[A-Za-z0-9_+@.-]+', value) and value not in ('.', '..')


def url(value):
    require(isinstance(value, str) and len(value) <= 2048 and value.isascii(), 'unsafe APK source URL')
    parsed = urlsplit(value)
    require(parsed.scheme == 'https' and parsed.hostname in SOURCE_HOSTS and parsed.netloc == parsed.hostname
        and not parsed.query and not parsed.fragment and parsed.path.startswith('/')
        and not {'Users', 'Volumes', '.private'}.intersection(parsed.path.split('/')), 'unversioned or private APK source URL')


def summarize(raw, installed, read_source):
    """Consume the same exported packet after its complete Go-notice validation."""
    payloads = decode_export(raw)
    go = checked_json(payloads['manifest.json'])
    roots = [row for row in go['modules'] if row['identity']['path'] == ROOT]
    require(len(roots) == 1 and roots[0]['identity']['main'] is True, 'APK notice catalog has no unique main-module owner')
    pins = {}
    for pin in roots[0]['notices']:
        if pin['path'].startswith((PREFIX, REFERENCES)):
            fields(pin, ('path', 'bytes', 'sha256', 'selectionScope'))
            require(public_path(pin['path']) and pin['path'] not in pins and pin['selectionScope'] == 'MODULE_NOTICE_DIRECTORY', 'APK source notice path or ownership differs')
            digest(pin['sha256']); integer(pin['bytes'], 1, 128 << 10)
            require(pin['sha256'] in payloads, 'APK source notice payload missing')
            data = payloads[pin['sha256']]
            require(len(data) == pin['bytes'] and sha(data) == pin['sha256'] and read_source(pin['path'], 128 << 10) == data,
                'APK source notice bytes differ from checked source')
            pins[pin['path']] = pin

    def copied(path):
        require(path in pins, 'required APK source notice file missing')
        return payloads[pins[path]['sha256']]

    require(copied(README) == README_BYTES, 'APK notice review boundary text differs')
    manifest_raw = copied(MANIFEST); value = checked_json(manifest_raw)
    fields(value, ('schema', 'status', 'scope', 'installedPackages', 'origins', 'noticeObjectCount', 'noticePayloadBytes', 'referenceDocuments', *FLAGS))
    require(type(value['schema']) is int and value['schema'] == 1 and value['status'] == STATUS and value['scope'] == SCOPE, 'unknown APK notice catalog')
    require(all(value[key] is False for key in FLAGS), 'APK notice candidates cannot establish legal or release acceptance')
    require(isinstance(installed, list) and 0 < len(installed) <= 512 and value['installedPackages'] == installed, 'APK notice catalog differs from installed package metadata')
    require(len({row['name'] for row in installed}) == len(installed), 'duplicate installed APK identity')
    selected = {}
    for row in installed:
        require(isinstance(row['origin'], str) and filename(row['origin']) and re.fullmatch('[0-9a-f]{40}', row['declaredBuildCommit'] or ''), 'missing APK origin or declared source commit')
        require(row['licenseTextOrLegalQualificationEstablished'] is False, 'installed declaration cannot establish legal acceptance')
        selected.setdefault(row['origin'], set()).add(row['declaredBuildCommit'])
    require(all(len(commits) == 1 for commits in selected.values()), 'one APK origin selects conflicting source commits')
    integer(value['noticeObjectCount'], 0, 4096); integer(value['noticePayloadBytes'], 0, 8 << 20)
    objects = {}; expected_paths = {MANIFEST, README}
    references = {}; reference_bytes = 0
    require(isinstance(value['referenceDocuments'], list) and len(value['referenceDocuments']) <= 32, 'APK reference document budget exceeded')
    for document in value['referenceDocuments']:
        fields(document, ('identifier', 'url', 'path', 'bytes', 'sha256'))
        key = document['identifier']
        require(key in REFERENCE_URLS and key not in references and document['url'] == REFERENCE_URLS[key]
            and document['path'] == REFERENCES + key + '.txt', 'unknown or unversioned APK license reference')
        digest(document['sha256']); integer(document['bytes'], 1, 128 << 10)
        payload = copied(document['path'])
        require(len(payload) == document['bytes'] and sha(payload) == document['sha256'], 'APK license reference bytes differ')
        expected_paths.add(document['path']); references[key] = document; reference_bytes += len(payload)
    origins, used, used_references, missing_notices, unresolved = [], set(), set(), [], []
    page_count = 0
    source_count, notice_count = 0, 0
    require(isinstance(value['origins'], list) and len(value['origins']) == len(selected), 'APK origin coverage differs')
    for origin in value['origins']:
        fields(origin, ('origin', 'recipe', 'sources', 'noticePages', 'referenceDocuments', 'unresolvedSourceInputs'))
        name = origin['origin']; require(name in selected and name not in origins, 'unknown or duplicate APK source origin')
        recipe = origin['recipe']
        fields(recipe, ('repository', 'commit', 'path', 'gitBlob', 'sha256', 'bytes'))
        require(recipe['repository'] == 'https://github.com/alpinelinux/aports' and recipe['commit'] in selected[name]
            and recipe['path'] == 'main/' + name + '/APKBUILD' and re.fullmatch('[0-9a-f]{40}', recipe['gitBlob']), 'APK recipe does not bind the declared installed source')
        digest(recipe['sha256']); integer(recipe['bytes'], 1, 128 << 10)
        require(isinstance(origin['sources'], list) and len(origin['sources']) <= 256, 'APK source input budget exceeded')
        sources = {}
        for source in origin['sources']:
            fields(source, ('name', 'kind', 'url', 'sha512', 'sha256', 'bytes'))
            key = source['name']; require(filename(key) and key not in sources and source['kind'] in SOURCE_KINDS, 'invalid APK source selection')
            url(source['url']); digest(source['sha256']); integer(source['bytes'], 1, 80 << 20)
            require(isinstance(source['sha512'], str) and re.fullmatch('[0-9a-f]{128}', source['sha512']), 'APK source archive checksum is not pinned')
            sources[key] = source
        gaps = origin['unresolvedSourceInputs']
        require(isinstance(gaps, list) and len(gaps) <= 256 and gaps == sorted(set(gaps)) and all(filename(gap) and gap not in sources for gap in gaps), 'unresolved APK source input accounting differs')
        unresolved.extend(name + '/' + gap for gap in gaps)
        declared = set().union(*(set(re.findall(r'[A-Za-z0-9.+-]+', row['declaredLicense'] or '')) for row in installed if row['origin'] == name))
        reference_ids = origin['referenceDocuments']
        require(isinstance(reference_ids, list) and reference_ids == sorted(set(reference_ids))
            and all(key in references and key in declared for key in reference_ids), 'APK license reference does not match the declared origin license')
        used_references.update(reference_ids)
        pages = origin['noticePages']; notices = []
        require(isinstance(pages, list) and len(pages) <= 256, 'APK notice page budget exceeded')
        for index, page in enumerate(pages):
            fields(page, ('path', 'bytes', 'sha256'))
            require(page['path'] == PAGES + name + '-' + str(index).zfill(3) + '.json', 'APK notice page sequence or origin differs')
            digest(page['sha256']); integer(page['bytes'], 1, 128 << 10)
            payload = copied(page['path'])
            require(len(payload) == page['bytes'] and sha(payload) == page['sha256'], 'APK notice page bytes differ')
            decoded = checked_json(payload); fields(decoded, ('schema', 'origin', 'notices'))
            require(type(decoded['schema']) is int and decoded['schema'] == 1 and decoded['origin'] == name
                and isinstance(decoded['notices'], list) and 0 < len(decoded['notices']) <= 256, 'APK notice page contents differ')
            notices.extend(decoded['notices']); expected_paths.add(page['path']); page_count += 1
        require(len(notices) <= 32768, 'APK notice selection budget exceeded')
        seen = set()
        for notice in notices:
            fields(notice, ('source', 'path', 'bytes', 'sha256', 'selectionScope'))
            pair = (notice['source'], notice['path'])
            require(notice['source'] in sources and public_path(notice['path']) and pair not in seen
                and notice['selectionScope'] in NOTICE_SCOPES, 'APK notice lacks a selected source input')
            require((sources[notice['source']]['kind'] == 'SOURCE_ARCHIVE') == (notice['selectionScope'] in {'ARCHIVE_NOTICE_FILE', 'ARCHIVE_INITIAL_SOURCE_NOTICE'}), 'APK notice selection scope differs from source input kind')
            digest(notice['sha256']); integer(notice['bytes'], 1, 128 << 10)
            key = notice['sha256']; path = PREFIX + 'objects/' + key; payload = copied(path)
            require(len(payload) == notice['bytes'] and sha(payload) == key, 'APK notice reference differs from payload')
            require(key not in objects or objects[key] == notice['bytes'], 'APK notice payload has conflicting sizes')
            objects[key] = notice['bytes']; expected_paths.add(path)
            require(len(objects) <= 4096 and sum(objects.values()) <= 8 << 20, 'APK notice payload budget exceeded')
            seen.add(pair); used.add(key)
        if not notices:
            missing_notices.append(name)
        origins.append(name); source_count += len(sources); notice_count += len(notices)
        require(notice_count <= 65536, 'APK aggregate notice occurrence budget exceeded')
    require(origins == sorted(selected) and used == set(objects) and used_references == set(references), 'APK source origin ordering or notice object reachability differs')
    require(value['noticeObjectCount'] == len(objects) and value['noticePayloadBytes'] == sum(objects.values()), 'APK unique notice count or byte total differs')
    require(set(pins) == expected_paths, 'unexpected APK catalog file copied')
    return {'schema': 1, 'status': STATUS, 'manifestSha256': sha(manifest_raw),
        'installedPackageCount': len(installed), 'sourceOriginCount': len(origins), 'recipePinnedSourceInputReferences': source_count,
        'selectedNoticeOccurrences': notice_count, 'distinctNoticeObjects': len(objects), 'noticePayloadBytes': sum(objects.values()),
        'noticeOccurrencePages': page_count, 'referenceDocumentCount': len(references), 'referenceDocumentBytes': reference_bytes,
        'originsWithoutObservedNoticeCandidates': missing_notices, 'unresolvedSourceInputs': sorted(unresolved),
        'sourceFilesIndependentlyMatched': len(pins), 'allOriginsHaveObservedCandidateNotices': not missing_notices,
        'allRecordedSourceInputsPresent': not unresolved,
        **{key: False for key in FLAGS}}

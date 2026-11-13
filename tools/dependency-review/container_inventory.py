#!/usr/bin/env python3
"""Retain exact-image inputs and a same-source recipe/health comparison."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import subprocess
import sys
import time
import uuid

from bounded import ReviewError, file_bytes, run_bounded, save, sha
from source_closure import source_closure
from notice_inventory import EXPORT_SCRIPT, APK_SCRIPT, MAX_EXPORT, summarize_export, expected_readme, apk_licenses
from container_footprint import (CONTROL_RECIPE, CONTROL_RECIPE_SHA, PARENT, DIGEST_SCRIPT, HEALTH_COMMAND,
    UNHEALTHY_COMMAND, image as checked_image, digest_projection, health_summary, compare)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--source-commit', required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    receipt = {'schema': 2, 'observedAt': datetime.now(timezone.utc).isoformat(),
        'status': 'IMAGE_OBSERVATION_FAILED_OR_INCOMPLETE', 'commands': [],
        'completeSBOM': False, 'vulnerabilityScanPerformed': False, 'imageOriginAuthenticated': False,
        'legalReviewComplete': False, 'releaseAccepted': False, 'healthLaunches': []}
    created = False
    phase = 'output_directory'
    raw_outputs, processes = {}, {}
    try:
        args.output.mkdir(mode=0o700); created = True
        phase = 'source_before'
        before = source_closure(args.source_commit); receipt['source'] = before

        def run(label, argv, stdout_limit=65536, expected=(0,), timeout=30):
            nonlocal phase
            phase = label
            if label in processes:
                raise ReviewError('duplicate image command label')
            directory = args.output / label; directory.mkdir()
            result = run_bounded(argv, directory, timeout=timeout, stdout_limit=stdout_limit, stderr_limit=65536)
            for name, pin in result['streams'].items():
                (directory / pin['path']).rename(directory / name); pin['path'] = label + '/' + name
            receipt['commands'].append({'label': label, 'expectedExits': list(expected), 'process': result})
            processes[label] = result
            raw = file_bytes(directory / 'stdout', stdout_limit); raw_outputs[label] = raw
            if result['actualExit'] not in expected or not result['outputComplete']:
                raise ReviewError('image observation command failed or is incomplete')
            return raw

        control_recipe = file_bytes(Path(CONTROL_RECIPE), 65536)
        if sha(control_recipe) != CONTROL_RECIPE_SHA:
            raise ReviewError('frozen comparison recipe differs')
        origin = json.loads(file_bytes(Path('tools/dependency-review/fixtures/with-gnu-wget-origin.json'), 16384))
        if origin['sourceCommit'] != PARENT or origin['sourcePin']['sha256'] != CONTROL_RECIPE_SHA:
            raise ReviewError('frozen recipe source reference differs')
        recipe = {'path': 'Dockerfile', 'sha256': sha(file_bytes(Path('Dockerfile'), 65536)), 'sourceCommit': args.source_commit}
        template = ('{"id":{{json .Id}},"os":{{json .Os}},"architecture":{{json .Architecture}},'
            '"entrypoint":{{json .Config.Entrypoint}},"user":{{json .Config.User}},"reportedSizeBytes":{{json .Size}}}')
        common = ['docker', 'run', '--rm', '--network=none', '--read-only', '--user=65534:65534',
            '--cap-drop=ALL', '--security-opt=no-new-privileges']

        def observe(role, tag):
            prefix = '' if role == 'candidate' else 'control-'
            image = checked_image(json.loads(run(prefix + 'image', ['docker', 'image', 'inspect', '--format', template, tag])))
            packages = run(prefix + 'packages', common + ['--entrypoint=/sbin/apk', image['id'], 'info', '-v']).decode().splitlines()
            if not 1 <= len(packages) <= 512 or len(set(packages)) != len(packages) or any(
                not re.fullmatch(r'[A-Za-z0-9_.+~-]+', value) for value in packages):
                raise ReviewError('invalid installed package inventory')
            if not {'ca-certificates-20261020-r0', 'gcompat-1.1.0-r4'} <= set(packages):
                raise ReviewError('required runtime packages are missing')
            if ('wget-1.25.0-r1' in packages) != (role == 'control'):
                raise ReviewError('unexpected GNU wget package selection')
            binary = run(prefix + 'binary', common + ['--entrypoint=/bin/busybox', image['id'],
                'sha256sum', '/usr/local/bin/zvm-node']).decode().split()
            if len(binary) != 2 or not re.fullmatch('[0-9a-f]{64}', binary[0]) or binary[1] != '/usr/local/bin/zvm-node':
                raise ReviewError('invalid CLI digest')
            declarations = run(prefix + 'apk-licenses', common + ['--entrypoint=/bin/sh', image['id'], '-euc', APK_SCRIPT], 2 << 20)
            result = {'image': image, 'installedPackages': sorted(packages), 'cliBinarySha256': binary[0],
                'runtimeAPKLicenseDeclarations': apk_licenses(declarations, packages),
                'recipe': recipe if role == 'candidate' else {'path': CONTROL_RECIPE, 'sha256': CONTROL_RECIPE_SHA, 'sourceCommit': PARENT}}
            if role == 'candidate':
                exported = run('notices', common + ['--entrypoint=/bin/sh', image['id'], '-euc', EXPORT_SCRIPT], MAX_EXPORT)
                result['sourceNotices'] = summarize_export(exported, before, binary[0], expected_readme(),
                    lambda path, limit: file_bytes(Path(path), limit))
            else:
                projected = run('control-notice-digests', common + ['--entrypoint=/bin/sh', image['id'], '-euc', DIGEST_SCRIPT], 2 << 20)
                result['sourceNoticeDigestProjection'] = digest_projection(projected, raw_outputs['notices'])
            return result

        def health(role, image):
            nonlocal phase
            owned = None
            name = 'zvm-review-' + role + '-' + uuid.uuid4().hex
            launch = {'role': role, 'name': name, 'imageId': image['id'], 'ownership': 'CREATED_BY_THIS_INVOCATION', 'state': 'LAUNCH_PENDING'}
            receipt['healthLaunches'].append(launch)
            failed_phase = None
            try:
                raw = run(role + '-create', ['docker', 'create', '--name', name, '--network=none', '--read-only',
                    '--user=65534:65534', '--cap-drop=ALL', '--security-opt=no-new-privileges', '--pids-limit=64',
                    '--memory=256m', '--cpus=1', '--label', 'org.edgepillar.zvm-review=' + name,
                    image['id'], 'solcd', '-listen=127.0.0.1:8600', '-solc-dir=/solc'])
                identifier = raw.decode().strip()
                if not re.fullmatch('[0-9a-f]{64}', identifier):
                    raise ReviewError('owned container identifier is unknown')
                owned = identifier; launch.update(state='CREATED', id=owned)
                run(role + '-start', ['docker', 'start', owned])
                inspect_template = ('{"id":{{json .Id}},"image":{{json .Image}},"running":{{json .State.Running}},'
                    '"user":{{json .Config.User}},"network":{{json .HostConfig.NetworkMode}},'
                    '"readonly":{{json .HostConfig.ReadonlyRootfs}},"capDrop":{{json .HostConfig.CapDrop}},'
                    '"securityOpt":{{json .HostConfig.SecurityOpt}},"mounts":{{json .Mounts}},"ports":{{json .HostConfig.PortBindings}}}')
                run(role + '-inspect', ['docker', 'inspect', '--format', inspect_template, owned])
                run(role + '-provider', ['docker', 'exec', owned, '/bin/sh', '-euc',
                    'path=$(command -v wget); printf "%s\\n" "$path"; /bin/busybox readlink -f "$path"'])
                ready = False
                for attempt in range(8):
                    label = role + '-ready-' + str(attempt)
                    run(label, ['docker', 'exec', owned, 'wget', '-qO-', 'http://127.0.0.1:8600/status'],
                        stdout_limit=8192, expected=(0, 1, 8), timeout=10)
                    if processes[label]['actualExit'] == 0:
                        ready = True; break
                    time.sleep(0.15)
                if not ready:
                    raise ReviewError('verifier did not become ready within bounded probes')
                run(role + '-healthy-pipeline', ['docker', 'exec', owned, '/bin/sh', '-ec', HEALTH_COMMAND], 8192)
                run(role + '-unhealthy-pipeline', ['docker', 'exec', owned, '/bin/sh', '-ec', UNHEALTHY_COMMAND],
                    8192, expected=(1,))
                run(role + '-logs', ['docker', 'logs', owned])
            except BaseException:
                failed_phase = phase
                raise
            finally:
                if owned is not None:
                    try:
                        run(role + '-stop', ['docker', 'stop', '--time=2', owned], timeout=10)
                        run(role + '-stopped', ['docker', 'inspect', '--format',
                            '{"id":{{json .Id}},"running":{{json .State.Running}},"exitCode":{{json .State.ExitCode}}}', owned])
                    finally:
                        run(role + '-remove', ['docker', 'rm', '--force', owned], timeout=10)
                        launch['state'] = 'REMOVED'
                if failed_phase:
                    phase = failed_phase
            return health_summary(role, processes, raw_outputs.__getitem__)

        candidate = observe('candidate', 'zvm-node:ci')
        receipt.update(candidate)
        control = observe('control', 'zvm-node:wget-control')
        receipt['control'] = control
        health_results = {}; receipt['verifierHealth'] = health_results
        for role, value in [('candidate', candidate), ('control', control)]:
            health_results[role] = health(role, value['image'])
        comparison = compare(candidate, control, control['sourceNoticeDigestProjection'], health_results)
        phase = 'source_after'
        if source_closure(args.source_commit) != before:
            raise ReviewError('source changed during image observations')
        receipt.update(status='IMAGE_AND_PACKAGE_INPUTS_OBSERVED', footprintComparison=comparison,
            verifierHealth=health_results, hostPathsOrConfigValuesRecorded=False)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    if created:
        save(args.output / 'inventory.json', receipt)
    print(json.dumps({'status': receipt['status'], 'exit': result, 'completeSBOM': False, 'releaseAccepted': False}))
    return result


if __name__ == '__main__':
    sys.exit(main())

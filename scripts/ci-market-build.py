"""Overlap independent market inputs; join every owned group before testing."""
from __future__ import annotations
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import subprocess
import sys
import time

FLOOR = 10 << 30
RESERVATION = 8 << 30
OUTPUT_CAP = 2 << 20
TIMEOUT = 240

class BuildFailure(RuntimeError):
    pass

def plans(binary):
    return [
        [('relayer', ['go', '-C', '.market-browser/zvm-relayer', 'build', '-p=2', '-trimpath', '-buildvcs=true', '-o', str(binary), './cmd/zvm-relayer'])],
        [('nft-install', ['npm', 'ci', '--ignore-scripts', '--include=dev', '--prefix', '.market-browser/zvm-nft']),
         ('karum-install', ['npm', 'ci', '--ignore-scripts', '--include=dev', '--prefix', '.market-browser/zvm-karum']),
         ('nft-build', ['npm', 'run', 'build', '--prefix', '.market-browser/zvm-nft']),
         ('karum-build', ['npm', 'run', 'build', '--prefix', '.market-browser/zvm-karum'])],
    ]

def run(lanes, backend, *, timeout=TIMEOUT, floor=FLOOR, reservation=RESERVATION, output_cap=OUTPUT_CAP):
    if len(lanes) != 2 or any(not commands for commands in lanes):
        raise BuildFailure('Exactly two nonempty independent build lanes are required.')
    roles = [role for commands in lanes for role, _ in commands]
    if len(set(roles)) != len(roles):
        raise BuildFailure('Command roles must be unique.')
    started = backend.now()
    initial_free = backend.free()
    if initial_free < floor + reservation:
        raise BuildFailure('The free-space floor plus the declared build reservation is unavailable.')
    pending = [list(commands) for commands in lanes]
    active = {}
    completed = []
    output = {role: 0 for role in roles}
    maximum = 0
    minimum_free = initial_free
    maximum_decrease = 0
    try:
        while active or any(pending):
            if backend.now() - started >= timeout:
                raise BuildFailure('The independent build phase exceeded its bounded deadline.')
            free = backend.free()
            minimum_free = min(minimum_free, free)
            maximum_decrease = max(maximum_decrease, initial_free - free)
            if free < floor or maximum_decrease > reservation:
                raise BuildFailure('The sampled free-space floor or build reservation was exceeded.')
            for number, commands in enumerate(pending):
                if commands and number not in active:
                    role, argv = commands.pop(0)
                    token = backend.start(role, argv)
                    active[number] = (token, role)
            maximum = max(maximum, len(active))
            for token, kind, value in backend.events():
                found = [(number, role) for number, (current, role) in active.items() if current == token]
                if len(found) != 1:
                    raise BuildFailure('An unowned or duplicate command event was observed.')
                number, role = found[0]
                if kind == 'output':
                    output[role] += len(value)
                    if output[role] > output_cap:
                        raise BuildFailure('A build command exceeded its explicit output cap.')
                    backend.emit(role, value)
                elif kind == 'exit':
                    if type(value) is not int or value != 0:
                        raise BuildFailure('An independent build command failed: ' + role)
                    completed.append(role)
                    del active[number]
                else:
                    raise BuildFailure('An unknown command event was observed.')
        if set(completed) != set(roles) or len(completed) != len(roles):
            raise BuildFailure('The complete original build plan was not executed exactly once.')
        backend.assert_joined()
    except BaseException:
        backend.cancel()
        raise
    return {'schema': 1, 'complete': True, 'commandRoles': completed, 'commandCount': len(completed),
            'maximumActiveCommandGroups': maximum, 'allOwnedGroupsJoined': True,
            'elapsedMilliseconds': round((backend.now() - started) * 1000),
            'outputBytesByRole': output, 'outputCapPerCommandBytes': output_cap,
            'freeSpaceFloorBytes': floor, 'initialFreeBytes': initial_free, 'minimumSampledFreeBytes': minimum_free,
            'reservedBuildBytes': reservation, 'maximumSampledWholeFilesystemFreeSpaceDecreaseBytes': maximum_decrease,
            'sampledFilesystemObservationIsNotAnOSAggregateQuota': True,
            'phaseTimeoutSeconds': timeout, 'originalFinancialProfilesOrDeadlinesChanged': False,
            'newDependencyVersionsOrDownloadsAdded': False}

class Processes:
    def __init__(self):
        self.selector = selectors.DefaultSelector()
        self.children = {}
        self.serial = 0

    def now(self):
        return time.monotonic()

    def free(self):
        return shutil.disk_usage(Path.cwd()).free

    def start(self, role, argv):
        environment = os.environ.copy()
        if role == 'relayer':
            environment.update(GOMAXPROCS='2', GOMEMLIMIT='3GiB')
        else:
            environment['NODE_OPTIONS'] = '--max-old-space-size=1536'
        child = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 start_new_session=True, env=environment)
        self.serial += 1
        token = self.serial
        self.children[token] = {'process': child, 'eof': False, 'registered': False, 'role': role, 'bytes': 0}
        os.set_blocking(child.stdout.fileno(), False)
        self.selector.register(child.stdout, selectors.EVENT_READ, token)
        self.children[token]['registered'] = True
        return token

    @staticmethod
    def group_exists(pid):
        try:
            os.killpg(pid, 0)
            return True
        except ProcessLookupError:
            return False

    def events(self):
        result = []
        for key, _ in self.selector.select(0.1):
            token = key.data
            raw = os.read(key.fileobj.fileno(), 16384)
            if raw:
                result.append((token, 'output', raw))
            else:
                self.selector.unregister(key.fileobj)
                key.fileobj.close()
                self.children[token]['eof'] = True
                self.children[token]['registered'] = False
        for token, row in list(self.children.items()):
            child = row['process']
            code = child.poll()
            if code is None:
                continue
            if code != 0:
                result.append((token, 'exit', code))
            elif row['eof'] and not self.group_exists(child.pid):
                assert child.wait() == 0
                del self.children[token]
                result.append((token, 'exit', 0))
        return result

    def emit(self, role, raw):
        # Prefix every chunk so build output cannot impersonate later Go test events.
        for row in self.children.values():
            if row['role'] == role:
                row['bytes'] += len(raw)
        text = raw.decode('utf-8', errors='replace')
        for line in text.splitlines():
            print('MARKET-BUILD-OUTPUT ' + role + ' ' + line, flush=True)

    def assert_joined(self):
        if self.children or self.selector.get_map():
            raise BuildFailure('An owned process group or output pipe remains open.')
        self.selector.close()

    def cancel(self):
        for row in self.children.values():
            try:
                os.killpg(row['process'].pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
        deadline = self.now() + 5
        while self.now() < deadline and any(self.group_exists(row['process'].pid) for row in self.children.values()):
            for key, _ in self.selector.select(0.05):
                row = self.children[key.data]
                raw = os.read(key.fileobj.fileno(), 16384)
                if raw:
                    self.emit(row['role'], raw[:max(0, OUTPUT_CAP - row['bytes'])])
                else:
                    self.selector.unregister(key.fileobj)
                    key.fileobj.close()
                    row['eof'] = True
                    row['registered'] = False
            for row in self.children.values():
                row['process'].poll()
        for row in self.children.values():
            child = row['process']
            try:
                os.killpg(child.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            try:
                child.wait(timeout=3)
            except subprocess.TimeoutExpired as error:
                raise BuildFailure('An owned build leader could not be joined.') from error
            if self.group_exists(child.pid):
                raise BuildFailure('An owned build descendant remains after cancellation.')
            if not row['eof']:
                if row['registered']:
                    self.selector.unregister(child.stdout)
                while True:
                    try:
                        raw = os.read(child.stdout.fileno(), 16384)
                    except BlockingIOError:
                        break
                    if not raw:
                        break
                    self.emit(row['role'], raw[:max(0, OUTPUT_CAP - row['bytes'])])
                child.stdout.close()
        self.children.clear()
        self.selector.close()

def main():
    binary = Path(os.environ['RUNNER_TEMP']) / 'zvm-market-relayer'
    if not binary.parent.is_dir() or binary.exists():
        raise BuildFailure('A fresh output under the existing runner temporary directory is required.')
    def interrupted(signum, frame):
        raise BuildFailure('The owned build phase was interrupted.')
    signal.signal(signal.SIGTERM, interrupted)
    signal.signal(signal.SIGINT, interrupted)
    result = run(plans(binary), Processes())
    print('MARKET-INDEPENDENT-BUILD ' + json.dumps(result, sort_keys=True), flush=True)

if __name__ == '__main__':
    main()

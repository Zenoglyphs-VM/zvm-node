"""Bounded capture/file primitives reused from the reviewed Core implementation."""

import hashlib
import json
import os
from pathlib import Path
import selectors
import signal
import stat
import subprocess
import time

MAX_STDOUT = 8 << 20
MAX_STDERR = 256 << 10

class ReviewError(ValueError):
    """An input cannot support the selected review contract."""


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ReviewError("duplicate JSON member")
        result[key] = value
    return result


def run_bounded(argv, output, *, cwd=None, env=None, timeout=600, stdout_limit=MAX_STDOUT, stderr_limit=MAX_STDERR):
    """POSIX foreground capture with caps, a deadline and process-group teardown."""
    if os.name != "posix" or timeout <= 0 or stdout_limit <= 0 or stderr_limit <= 0:
        raise ReviewError("unsupported process budget")
    output = Path(output)
    names = {"stdout": "govulncheck.json", "stderr": "govulncheck.stderr"}
    files, streams = {}, {}
    proc = selector = None
    reason = None
    complete = {"stdout": False, "stderr": False}
    counts = {"stdout": 0, "stderr": 0}
    hashes = {name: hashlib.sha256() for name in names}
    caps = {"stdout": stdout_limit, "stderr": stderr_limit}
    started = time.monotonic()
    try:
        for name, path in names.items():
            files[name] = (output / path).open("xb")
        proc = subprocess.Popen(argv, cwd=cwd, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True)
        selector = selectors.DefaultSelector()
        for name, stream in (("stdout", proc.stdout), ("stderr", proc.stderr)):
            os.set_blocking(stream.fileno(), False)
            selector.register(stream, selectors.EVENT_READ, name)
            streams[name] = stream
        deadline = started + timeout
        stopped_at = parent_exited_at = None
        while selector.get_map() or proc.poll() is None:
            now = time.monotonic()
            if proc.poll() is not None and parent_exited_at is None:
                parent_exited_at = now
            if reason is None and now >= deadline:
                reason = "deadline_exceeded"
            if reason is None and parent_exited_at is not None and now - parent_exited_at >= 2 and selector.get_map():
                reason = "incomplete_inherited_streams"
            if reason is not None and stopped_at is None:
                try:
                    os.killpg(proc.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                stopped_at = now
            if stopped_at is not None and now - stopped_at >= 3:
                break
            for key, _ in selector.select(0.05):
                name = key.data
                try:
                    block = os.read(key.fileobj.fileno(), 65536)
                except BlockingIOError:
                    continue
                if not block:
                    complete[name] = True
                    selector.unregister(key.fileobj)
                    continue
                remaining = max(0, caps[name] - counts[name])
                prefix = block[:remaining]
                if prefix:
                    files[name].write(prefix)
                    hashes[name].update(prefix)
                    counts[name] += len(prefix)
                if len(block) > remaining and reason is None:
                    reason = name + "_limit_exceeded"
        try:
            actual_exit = proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            actual_exit = None
            reason = reason or "process_termination_unknown"
        return {"actualExit": actual_exit, "terminationReason": reason,
            "outputComplete": reason is None and all(complete.values()),
            "seconds": round(time.monotonic() - started, 3),
            "streams": {name: {"path": names[name], "bytes": counts[name], "sha256": hashes[name].hexdigest(),
                                "eofObserved": complete[name], "limit": caps[name]} for name in names}}
    finally:
        if proc is not None and proc.poll() is None:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                pass
            proc.wait(timeout=3)
        if selector is not None:
            selector.close()
        for stream in streams.values():
            stream.close()
        for stream in files.values():
            stream.flush()
            os.fsync(stream.fileno())
            stream.close()


def file_bytes(path, limit):
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as source:
        info = os.fstat(source.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1 or info.st_size > limit:
            raise ReviewError("selected file is not an independent bounded regular file")
        raw = source.read(limit + 1)
        after = os.fstat(source.fileno())
        identity = lambda item: (item.st_dev, item.st_ino, item.st_mode, item.st_nlink, item.st_size, item.st_mtime_ns, item.st_ctime_ns)
        if len(raw) != info.st_size or identity(info) != identity(after) or identity(after) != identity(path.lstat()):
            raise ReviewError("selected file changed during observation")
        return raw


def save(path, value):
    with path.open("x") as output:
        json.dump(value, output, indent=2, sort_keys=True)
        output.write("\n")

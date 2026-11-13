#!/usr/bin/env python3
"""Verify the Node checkout and its one explicitly pinned Core source closure."""

import argparse
import json
from pathlib import Path, PurePosixPath
import re
import subprocess
import sys

from bounded import ReviewError, file_bytes, sha

CORE_COMMIT = "29e6d9b30be94de3772c850c7a1e9811585a8833"
CORE_URL = "https://github.com/Zenoglyphs-VM/zvm-core.git"
AUTH_KEYS = r"^(http\..*\.extraheader|include\.path|includeif\..*\.path|core\.sshcommand|credential\..*)$"


def git(root, *args, absent=False):
    done = subprocess.run(["git", "--no-optional-locks", "--no-replace-objects", "-C", str(root), *args],
                          stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=15)
    if len(done.stdout) > 1 << 20 or len(done.stderr) > 65536:
        raise ReviewError("source metadata exceeds its bound")
    if absent:
        if done.returncode != 1 or done.stdout or done.stderr:
            raise ReviewError("repository-local authentication config is present or unknown")
    elif done.returncode:
        raise ReviewError("source metadata command did not complete")
    return done.stdout


def tree_entries(raw, *, core_link=False):
    rows, names = [], set()
    for entry in raw.decode().split("\0"):
        if not entry:
            continue
        metadata, name = entry.split("\t", 1)
        mode, kind, oid = metadata.split()
        path = PurePosixPath(name)
        if (path.as_posix() != name or path.is_absolute() or any(p in (".", "..", ".git") for p in path.parts)
                or name in names or not re.fullmatch(r"[0-9a-f]{40}", oid)):
            raise ReviewError("unsupported source path or identity")
        names.add(name)
        if mode == "160000":
            if not core_link or kind != "commit" or name != "zvm-core" or oid != CORE_COMMIT:
                raise ReviewError("unreviewed Gitlink profile")
        elif mode not in ("100644", "100755") or kind != "blob":
            raise ReviewError("source needs a separately reviewed file profile")
        rows.append((mode, oid, name))
        if len(rows) > 4096:
            raise ReviewError("source file-count budget exceeded")
    links = [row for row in rows if row[0] == "160000"]
    if core_link and len(links) != 1:
        raise ReviewError("the required Core Gitlink is missing")
    return rows


def snapshot(root, commit, *, core_link=False, allow_root_locks=False):
    if not re.fullmatch(r"[0-9a-f]{40}", commit) or root.is_symlink() or not root.is_dir():
        raise ReviewError("invalid source root or revision")
    if git(root, "rev-parse", "HEAD").decode().strip() != commit:
        raise ReviewError("checkout differs from selected source")
    if Path(git(root, "rev-parse", "--show-toplevel").decode().strip()).resolve() != root.resolve():
        raise ReviewError("unexpected nested checkout root")
    git(root, "diff", "--cached", "--exit-code", "--name-only", "HEAD", "--")
    if git(root, "ls-files", "--others", "--exclude-standard") or git(root, "ls-files", "--others", "--ignored", "--exclude-standard"):
        raise ReviewError("untracked or ignored source inputs require review")
    git(root, "config", "--local", "--name-only", "--get-regexp", AUTH_KEYS, absent=True)
    rows = tree_entries(git(root, "ls-tree", "-r", "-z", "HEAD"), core_link=core_link)
    files = total = 0
    overrides = {}
    for mode, oid, name in rows:
        if mode == "160000":
            continue
        selected = root / name
        for ancestor in selected.parents:
            if ancestor == root:
                break
            if ancestor.is_symlink():
                raise ReviewError("linked source ancestor")
        raw = file_bytes(selected, 16 << 20)
        actual = __import__("hashlib").sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()
        if bool(selected.stat().st_mode & 0o111) != (mode == "100755"):
            raise ReviewError("tracked source mode changed")
        if actual != oid:
            if not allow_root_locks or name not in ("go.mod", "go.sum"):
                raise ReviewError("tracked source bytes changed")
            overrides[name] = {"bytes": len(raw), "sha256": sha(raw)}
        files += 1
        total += len(raw)
        if total > 128 << 20:
            raise ReviewError("source byte budget exceeded")
    return {"commit": commit, "tree": git(root, "rev-parse", "HEAD^{tree}").decode().strip(),
            "regularFilesVerified": files, "regularBytesObserved": total, "rootLockOverrides": overrides,
            "goModSha256": sha(file_bytes(root / "go.mod", 1 << 20)),
            "goSumSha256": sha(file_bytes(root / "go.sum", 8 << 20))}


def source_closure(commit, *, allow_root_locks=False):
    root = Path.cwd()
    parent = snapshot(root, commit, core_link=True, allow_root_locks=allow_root_locks)
    expected = {"submodule.zvm-core.path": "zvm-core", "submodule.zvm-core.url": CORE_URL}
    config = git(root, "config", "--file", ".gitmodules", "--get-regexp", r"^submodule\.").decode().splitlines()
    if len(config) != 2 or dict(line.split(" ", 1) for line in config) != expected:
        raise ReviewError("Core URL or submodule configuration changed")
    core = snapshot(root / "zvm-core", CORE_COMMIT)
    if git(root / "zvm-core", "config", "--local", "--get", "remote.origin.url").decode().strip() != CORE_URL:
        raise ReviewError("Core checkout remote differs from its declared URL")
    return {"schema": 1, "root": parent, "core": {**core, "path": "zvm-core", "url": CORE_URL},
            "authenticationConfigValuesPrinted": False, "sourceProfile": "NODE_AND_EXPLICIT_CORE_GITLINK"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    args = parser.parse_args()
    try:
        observed = source_closure(args.source_commit)
    except (ReviewError, OSError, ValueError, subprocess.SubprocessError):
        print(json.dumps({"status": "SOURCE_CLOSURE_NOT_VERIFIED", "authenticationConfigValuesPrinted": False}))
        return 1
    print(json.dumps({"status": "NODE_AND_CORE_SOURCE_CLOSURE_VERIFIED", "source": observed}))
    return 0


if __name__ == "__main__":
    sys.exit(main())

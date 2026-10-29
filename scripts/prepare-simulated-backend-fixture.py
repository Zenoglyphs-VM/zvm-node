#!/usr/bin/env python3
"""Create a bounded, explicit CI-only copy of the pinned simulator dependency.

Tracked modules, the Go module cache, and other CI jobs remain unmodified.
The native red and green controls use the same actual upstream regression.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import stat
import subprocess

VERSION = "v1.101702.2"
SOURCE_COMMIT = "e8800cffe53d459cde8a07c8e8f1de9d86e79e07"
ORIGINAL = "483679155fe79a9bd486b9cbba173064afe2a0c3d9f3bede532c627b2af0211d"
MAX_BYTES = 256 << 20
MAX_FILES = 20000
OLD = "func (payload *Payload) WaitFull() {\n\tpayload.lock.Lock()\n\tdefer payload.lock.Unlock()\n\tpayload.cond.Wait()\n}"
NEW = "func (payload *Payload) WaitFull() {\n\tpayload.lock.Lock()\n\tdefer payload.lock.Unlock()\n\tfor payload.full == nil {\n\t\tpayload.cond.Wait()\n\t}\n}"
SUBJECT = "miner/payload_building.go"
TEST = "miner/zvm_wait_full_regression_test.go"
TEST_NAME = "TestZVMWaitFullCompletedBeforeWait"


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def corrected(raw):
    if digest(raw) != ORIGINAL or raw.count(OLD.encode()) != 1:
        raise ValueError("Pinned upstream WaitFull source differs")
    return raw.replace(OLD.encode(), NEW.encode(), 1)


def regular(path):
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode):
        raise ValueError("Expected an owned regular source file")
    return info


def read_manifest(root):
    regular(root / "manifest.json")
    value = json.loads((root / "manifest.json").read_text())
    if value["version"] != VERSION or value["source_commit"] != SOURCE_COMMIT:
        raise ValueError("Unexpected fixture manifest")
    return value


def emit(phase, manifest):
    print("SIMULATOR-WAIT-FULL-FIXTURE " + json.dumps({
        "phase": phase, "version": VERSION, "source_commit": SOURCE_COMMIT,
        "original_source_sha256": ORIGINAL,
        "patched_source_sha256": manifest.get("patched_source_sha256"),
        "copied_bytes": manifest["copied_bytes"],
        "copied_files": len(manifest["files"]),
        "module_cache_modified": False, "tracked_module_files_modified": False,
        "test_only_workspace": True, "native_results_qualified_by_metadata": False,
    }, sort_keys=True))


def prepare(root):
    env = dict(os.environ, GOWORK="off", GOPROXY="off")
    result = subprocess.run(["go", "list", "-mod=readonly", "-m", "-json",
        "github.com/ethereum/go-ethereum"], env=env, check=True,
        stdout=subprocess.PIPE, timeout=45)
    module = json.loads(result.stdout)["Replace"]
    if module["Path"] != "github.com/ethereum-optimism/op-geth" or module["Version"] != VERSION:
        raise ValueError("Unexpected pinned simulator dependency")
    source = Path(module["Dir"]).resolve(strict=True)
    if digest((source / SUBJECT).read_bytes()) != ORIGINAL:
        raise ValueError("Original module cache source differs")
    files = {}
    total = 0
    for directory, dirs, names in os.walk(source, followlinks=False):
        for name in dirs:
            if not stat.S_ISDIR((Path(directory) / name).lstat().st_mode):
                raise ValueError("Non-directory module source entry")
        for name in sorted(names):
            path = Path(directory) / name
            size = regular(path).st_size
            total += size
            files[path.relative_to(source).as_posix()] = size
            if total > MAX_BYTES or len(files) > MAX_FILES:
                raise ValueError("Simulator copy exceeds its explicit bound")
    if shutil.disk_usage(root.parent).free < (10 << 30) + total:
        raise ValueError("Simulator copy would violate the free-space floor")
    root.mkdir(mode=0o700, exist_ok=False)
    destination = root / "op-geth"
    manifest = {"version": VERSION, "source_commit": SOURCE_COMMIT,
        "source_directory": str(source), "copied_bytes": total, "files": {},
        "tracked_inputs": {name: digest(Path(name).read_bytes())
            for name in ["go.mod", "go.sum", "zvm-core/go.mod", "zvm-core/go.sum"]}}
    for name, size in sorted(files.items()):
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        actual = 0
        sha = hashlib.sha256()
        with (source / name).open("rb") as src, target.open("xb") as out:
            while chunk := src.read(1 << 20):
                actual += len(chunk)
                if actual > size:
                    raise ValueError("Module source grew during its copy")
                sha.update(chunk)
                out.write(chunk)
        if actual != size:
            raise ValueError("Module source copy is incomplete")
        manifest["files"][name] = sha.hexdigest()
    if digest((destination / SUBJECT).read_bytes()) != ORIGINAL:
        raise ValueError("Copied upstream source differs")
    regression = Path("scripts/testdata/simulated-backend-wait-full_test.go").read_bytes()
    with (destination / TEST).open("xb") as handle:
        handle.write(regression)
    manifest["regression_sha256"] = digest(regression)
    workspace = "go 1.25.0\nuse " + json.dumps(str(Path.cwd())) + "\nreplace github.com/ethereum/go-ethereum => " + json.dumps(str(destination)) + "\n"
    (root / "go.work").write_text(workspace)
    (root / "manifest.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")
    emit("ORIGINAL_COPY_PREPARED", manifest)


def check_red(root, log, actual_exit):
    manifest = read_manifest(root)
    if actual_exit != 1 or regular(log).st_size > 1 << 20:
        raise ValueError("Unexpected original native control outcome")
    if digest((root / "op-geth" / SUBJECT).read_bytes()) != ORIGINAL:
        raise ValueError("Red control did not select the original source")
    events = [json.loads(line) for line in log.read_text().splitlines() if line.startswith("{")]
    terminal = [row for row in events if row.get("Action") in ["pass", "fail", "skip"]]
    if len(terminal) != 2 or any(row["Action"] != "fail" for row in terminal):
        raise ValueError("Original native control failed outside the expected scope")
    if [row.get("Test") for row in terminal] != [TEST_NAME, None]:
        raise ValueError("Missing exact original regression failure")
    if any(row.get("Package") != "github.com/ethereum/go-ethereum/miner" for row in events):
        raise ValueError("Unexpected original regression package")
    output = "".join(row.get("Output", "") for row in events)
    if "late WaitFull missed an already completed payload" not in output or "panic:" in output or "original condition waiter did not join after its rescue" in output:
        raise ValueError("Original failure did not reproduce the declared wait defect")
    manifest["original_native_control"] = {"actual_exit": actual_exit,
        "test": TEST_NAME, "result": "EXPECTED_FAILURE_PRESERVED", "log_sha256": digest(log.read_bytes())}
    (root / "manifest.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")
    emit("ORIGINAL_NATIVE_FAILURE_PRESERVED", manifest)


def apply(root):
    manifest = read_manifest(root)
    if manifest.get("original_native_control", {}).get("actual_exit") != 1:
        raise ValueError("Missing exact original native regression failure")
    path = root / "op-geth" / SUBJECT
    regular(path)
    body = corrected(path.read_bytes())
    path.write_bytes(body)
    manifest["patched_source_sha256"] = digest(body)
    (root / "manifest.json").write_text(json.dumps(manifest, sort_keys=True, indent=2) + "\n")
    emit("EXACT_CONDITION_PREDICATE_APPLIED", manifest)


def verify(root):
    manifest = read_manifest(root)
    source = Path(manifest["source_directory"])
    found = set()
    for directory, dirs, names in os.walk(root / "op-geth", followlinks=False):
        for name in dirs:
            if not stat.S_ISDIR((Path(directory) / name).lstat().st_mode):
                raise ValueError("Unexpected linked module-copy directory")
        for name in names:
            path = Path(directory) / name
            regular(path)
            found.add(path.relative_to(root / "op-geth").as_posix())
            if len(found) > MAX_FILES + 1:
                raise ValueError("Unexpected module-copy file count")
    if found != set(manifest["files"]) | {TEST}:
        raise ValueError("Unexpected files in the declared two-file delta")
    for name, sha in manifest["files"].items():
        path = root / "op-geth" / name
        regular(path)
        expected = manifest["patched_source_sha256"] if name == SUBJECT else sha
        if digest(path.read_bytes()) != expected:
            raise ValueError("Test-only module copy differs from its declared delta")
    if digest((source / SUBJECT).read_bytes()) != ORIGINAL:
        raise ValueError("Original module cache source changed")
    if digest((root / "op-geth" / TEST).read_bytes()) != manifest["regression_sha256"]:
        raise ValueError("Upstream regression source changed")
    for name, sha in manifest["tracked_inputs"].items():
        if digest(Path(name).read_bytes()) != sha:
            raise ValueError("Tracked module input changed")
    emit("DECLARED_TEST_ONLY_SOURCE_DELTA_VERIFIED", manifest)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("mode", choices=["prepare", "check-red", "apply", "verify"])
    parser.add_argument("root", type=Path)
    parser.add_argument("--log", type=Path)
    parser.add_argument("--actual-exit", type=int)
    args = parser.parse_args()
    parent = Path(os.environ["RUNNER_TEMP"]).resolve(strict=True)
    root = args.root.absolute()
    if root.parent.resolve(strict=True) != parent or root.is_symlink():
        raise ValueError("Fixture must be one owned directory beneath RUNNER_TEMP")
    if args.mode == "check-red":
        check_red(root, args.log, args.actual_exit)
    else:
        globals()[args.mode](root)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Retain bounded exact-source Node build, integrity and test evidence in CI."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from bounded import ReviewError, file_bytes, run_bounded, save, unique_object
from source_closure import source_closure


def test_summary(raw):
    """Count complete Go JSON terminal events, preserving failures and skips."""
    if not raw or not raw.endswith(b"\n"):
        raise ReviewError("incomplete Go test event stream")
    terminals, packages, seen = [], {}, set()
    for line in raw.splitlines():
        row = json.loads(line, object_pairs_hook=unique_object)
        if not isinstance(row, dict) or not isinstance(row.get("Action"), str):
            raise ReviewError("invalid Go test event")
        if row["Action"] not in ("pass", "fail", "skip"):
            continue
        package = row.get("Package")
        if not isinstance(package, str) or not package:
            raise ReviewError("missing Go test package identity")
        name = row.get("Test")
        if name is None:
            if package in packages:
                raise ReviewError("duplicate terminal package event")
            packages[package] = row["Action"]
        else:
            if not isinstance(name, str) or not name or (package, name) in seen:
                raise ReviewError("duplicate or invalid terminal test identity")
            seen.add((package, name))
            terminals.append({"package": package, "test": name, "action": row["Action"]})
    if not packages or not terminals or any(row["package"] not in packages for row in terminals):
        raise ReviewError("missing terminal package or test evidence")
    return {"testAndSubtestEvents": {action: sum(row["action"] == action for row in terminals)
                                      for action in ("pass", "fail", "skip")},
            "failedTests": [row for row in terminals if row["action"] == "fail"],
            "skippedTests": [row for row in terminals if row["action"] == "skip"],
            "packages": dict(sorted(packages.items())),
            "countsIncludeParentTestsAndSubtests": True}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(),
               "status": "NATIVE_QUALIFICATION_FAILED_OR_INCOMPLETE", "commands": [],
               "vulnerabilityScanExecuted": False, "releaseAccepted": False}
    created = False
    phase = "output_directory"
    started = time.monotonic()
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_before"
        before = source_closure(args.source_commit)
        receipt["source"] = before
        env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOTELEMETRY="off",
                   GOFLAGS="-mod=readonly", GOMAXPROCS="2", GOMEMLIMIT="1536MiB")

        def run(label, argv):
            nonlocal phase
            phase = label
            remaining = 900 - (time.monotonic() - started)
            if remaining <= 0:
                raise ReviewError("aggregate qualification deadline exceeded")
            directory = args.output / label
            directory.mkdir()
            result = run_bounded(argv, directory, env=env, timeout=remaining,
                                 stdout_limit=8 << 20, stderr_limit=256 << 10)
            for name, pin in result["streams"].items():
                (directory / pin["path"]).rename(directory / name)
                pin["path"] = label + "/" + name
            command = {"label": label, "argv": argv, "process": result}
            receipt["commands"].append(command)
            raw = file_bytes(directory / "stdout", 8 << 20)
            if label in ("normal-tests", "race-tests") and result["outputComplete"]:
                command["testSummary"] = test_summary(raw)
            if result["actualExit"] != 0 or not result["outputComplete"]:
                raise ReviewError("native command did not complete successfully")
            if "testSummary" in command and (command["testSummary"]["failedTests"] or
                    "fail" in command["testSummary"]["packages"].values()):
                raise ReviewError("Go test failure events were observed")
            return raw

        actual = json.loads(run("sdk", ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOTOOLCHAIN"]))
        receipt["goEnvironment"] = actual
        if (actual["GOVERSION"] != "go1.26.11" or actual["GOTOOLCHAIN"] != "local" or
                (actual["GOOS"], actual["GOARCH"]) != ("linux", "amd64")):
            raise ReviewError("unexpected native qualification platform or SDK")
        run("tidy", ["go", "mod", "tidy", "-diff"])
        run("verify", ["go", "mod", "verify"])
        run("build", ["go", "build", "./..."])
        run("vet", ["go", "vet", "./..."])
        run("normal-tests", ["go", "test", "-count=1", "-timeout=180s", "-json", "./..."])
        run("race-tests", ["go", "test", "-race", "-count=1", "-timeout=300s", "-json", "./..."])
        phase = "source_after"
        if source_closure(args.source_commit) != before:
            raise ReviewError("source closure changed during native qualification")
        receipt.update(status="NODE_NATIVE_INTEGRITY_BUILD_VET_AND_TESTS_PASSED",
                       sourceClosureUnchanged=True, testResultCacheDisabled=True)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    if created:
        save(args.output / "qualification.json", receipt)
    print(json.dumps({"status": receipt["status"], "exit": result,
                      "failureStage": receipt.get("failureStage"), "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

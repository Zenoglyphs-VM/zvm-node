#!/usr/bin/env python3
"""Prepare exact dependency locks in a disposable CI checkout; never publish them."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import subprocess
import sys
import time

from bounded import ReviewError, file_bytes, run_bounded, save, sha, unique_object
from source_closure import source_closure

MODULE_TEMPLATE = (r'{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}},'
                   r'"Main":{{.Main}},"Replace":{{with .Replace}}'
                   r'{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}}}'
                   r'{{else}}null{{end}}}')


def objects(raw):
    """Decode the complete concatenated JSON stream emitted by go list."""
    decoder = json.JSONDecoder(object_pairs_hook=unique_object)
    text, result = raw.decode(), []
    while text.strip():
        value, end = decoder.raw_decode(text.lstrip())
        if not isinstance(value, dict) or len(result) >= 512:
            raise ReviewError("invalid or excessive module inventory")
        result.append(value)
        text = text.lstrip()[end:]
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(),
               "status": "LOCK_PREPARATION_FAILED_OR_INCOMPLETE", "commands": [],
               "buildOrTestsExecuted": False, "vulnerabilityScanExecuted": False,
               "releaseAccepted": False, "repositoryWritebackPerformed": False}
    phase = "output_directory"
    created = False
    started = time.monotonic()
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_binding_before"
        before = source_closure(args.source_commit)
        receipt["source"] = before
        raw = file_bytes(Path("tools/dependency-review/remediation-lock-inputs.json"), 8192)
        config = json.loads(raw, object_pairs_hook=unique_object)
        if config["schema"] != 1 or config["goVersion"] != "go1.26.11" or config["minimumGoVersion"] != "1.26.11":
            raise ReviewError("unsupported SDK recipe")
        if (config["goModSha256"], config["goSumSha256"]) != (before["root"]["goModSha256"], before["root"]["goSumSha256"]):
            raise ReviewError("recipe no longer matches the original module locks")
        modules = config["modules"]
        if not isinstance(modules, dict) or len(modules) != 8 or any(
                not isinstance(key, str) or not isinstance(value, str) or "@" in key or not value.startswith("v")
                for key, value in modules.items()):
            raise ReviewError("invalid exact module selections")
        receipt.update(recipeSha256=sha(raw), requestedModules=modules, selectedGoVersion=config["goVersion"])
        env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOTELEMETRY="off", GOFLAGS="",
                   GOPROXY="https://proxy.golang.org", GOSUMDB="sum.golang.org", GOPRIVATE="",
                   GONOPROXY="", GONOSUMDB="", GOMAXPROCS="2", GOMEMLIMIT="1536MiB")

        def run(label, argv, limit=4 << 20):
            nonlocal phase
            phase = label
            remaining = 540 - (time.monotonic() - started)
            if remaining <= 0:
                raise ReviewError("aggregate command deadline exceeded")
            directory = args.output / label
            directory.mkdir()
            result = run_bounded(argv, directory, env=env, timeout=remaining,
                                 stdout_limit=limit, stderr_limit=256 << 10)
            for stream, pin in result["streams"].items():
                (directory / pin["path"]).rename(directory / stream)
                pin["path"] = label + "/" + stream
            receipt["commands"].append({"label": label, "argv": argv, "process": result})
            if result["actualExit"] != 0 or not result["outputComplete"]:
                raise ReviewError("module preparation command did not complete successfully")
            return file_bytes(directory / "stdout", limit)

        actual = json.loads(run("sdk", ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH",
                                      "GOTOOLCHAIN", "GOPROXY", "GOSUMDB"], 65536))
        receipt["goEnvironment"] = actual
        if actual["GOVERSION"] != config["goVersion"] or actual["GOTOOLCHAIN"] != "local":
            raise ReviewError("unexpected SDK")
        original = json.loads(run("original-module", ["go", "mod", "edit", "-json"], 65536))
        run("select-sdk", ["go", "mod", "edit", "-go=" + config["minimumGoVersion"],
                           "-toolchain=none"])
        run("resolve", ["go", "get"] + [key + "@" + value for key, value in sorted(modules.items())])
        run("tidy", ["go", "mod", "tidy"])
        run("verify", ["go", "mod", "verify"])
        final = json.loads(run("candidate-module", ["go", "mod", "edit", "-json"], 65536))
        graph = objects(run("module-inventory", ["go", "list", "-m", "-f", MODULE_TEMPLATE, "all"]))
        inventory = {row["Path"]: row for row in graph}
        if len(inventory) != len(graph) or any(inventory.get(key, {}).get("Version") != value for key, value in modules.items()):
            raise ReviewError("resolved graph differs from exact module selections")
        if (final["Module"] != original["Module"] or final["Replace"] != original["Replace"] or
                final["Go"] != config["minimumGoVersion"] or final.get("Toolchain", "") not in ("", config["goVersion"])):
            raise ReviewError("module identity, replacement or SDK declaration changed unexpectedly")
        if any(not row.get("Indirect", False) and row["Path"] not in modules and row not in final["Require"]
               for row in original["Require"]):
            raise ReviewError("unselected direct dependency changed")
        run("unchanged-source", ["git", "--no-optional-locks", "--no-replace-objects", "diff", "--exit-code", "--",
                                 ".", ":(exclude)go.mod", ":(exclude)go.sum"])
        run("no-untracked-source", ["git", "--no-optional-locks", "ls-files", "--others", "--exclude-standard"])
        if file_bytes(args.output / "no-untracked-source/stdout", 4 << 20):
            raise ReviewError("unexpected files appeared in the checkout")
        after = source_closure(args.source_commit, allow_root_locks=True)
        if after["core"] != before["core"]:
            raise ReviewError("Core source changed during lock preparation")
        receipt["sourceAfter"] = after
        pins = {}
        for name, limit in (("go.mod", 1 << 20), ("go.sum", 8 << 20)):
            content = file_bytes(Path(name), limit)
            with (args.output / ("candidate." + name)).open("xb") as output:
                output.write(content)
            pins[name] = {"bytes": len(content), "sha256": sha(content)}
        receipt.update(status="LOCK_CANDIDATE_GENERATED_NOT_BUILD_OR_RUNTIME_QUALIFIED", candidateLocks=pins,
                       replacementUnchanged=True, selectedModulesVerified=True)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    if created:
        save(args.output / "preparation.json", receipt)
    print(json.dumps({"status": receipt["status"], "exit": result, "failureStage": receipt.get("failureStage"), "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

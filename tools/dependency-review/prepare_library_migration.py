#!/usr/bin/env python3
"""Prepare one reviewed library replacement and observe bounded compatibility controls."""

import argparse
import copy
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import time

from bounded import ReviewError, file_bytes, run_bounded, save, sha, unique_object
from local_core import MODULE_TEMPLATE
from native_qualification import test_summary
from source_closure import source_closure

NOMINAL = "github.com/ethereum/go-ethereum"
FORK = "github.com/ethereum-optimism/op-geth"
OLD = "v1.101702.2"
NEW = "v1.101702.3"


def objects(raw):
    decoder = json.JSONDecoder(object_pairs_hook=unique_object)
    text, rows = raw.decode(), []
    while text.strip():
        row, end = decoder.raw_decode(text.lstrip())
        if not isinstance(row, dict) or len(rows) >= 512:
            raise ReviewError("invalid module projection")
        rows.append(row)
        text = text.lstrip()[end:]
    return rows


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(),
               "status": "MIGRATION_PREPARATION_FAILED_OR_INCOMPLETE", "commands": [],
               "repositoryWritebackPerformed": False, "vulnerabilityScanExecuted": False,
               "fullNodeTestsExecuted": False, "engineOrLiveChainExecuted": False, "releaseAccepted": False}
    phase = "output_directory"
    created = workspace_created = False
    workspace = args.output.parent / "library-migration-control-workspace"
    started = time.monotonic()
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_before"
        before = source_closure(args.source_commit)
        receipt["source"] = before
        config_raw = file_bytes(Path("tools/dependency-review/library-migration-inputs.json"), 8192)
        config = json.loads(config_raw, object_pairs_hook=unique_object)
        if (config["schema"] != 1 or config["goVersion"] != "go1.26.11" or
                config["oldReplacement"] != OLD or config["candidateReplacement"] != NEW or
                (config["goModSha256"], config["goSumSha256"]) !=
                (before["root"]["goModSha256"], before["root"]["goSumSha256"])):
            raise ReviewError("migration recipe differs from the selected source")
        receipt["recipeSha256"] = sha(config_raw)
        fixtures = {}
        for name, pin in config["fixtures"].items():
            raw = file_bytes(Path("tools/dependency-review") / name, 65536)
            if sha(raw) != pin["sha256"] or len(raw) != pin["bytes"]:
                raise ReviewError("control fixture differs from its recipe")
            fixtures[name] = raw
        if set(fixtures) != {"library_compatibility.go.fixture", "stun_parser_test.go.fixture"}:
            raise ReviewError("unexpected control fixture set")
        workspace.mkdir(mode=0o700)
        workspace_created = True
        compat_source = workspace / "compatibility.go"
        compat_source.write_bytes(fixtures["library_compatibility.go.fixture"])
        new_test = workspace / "stun_new_test.go"
        new_test.write_bytes(fixtures["stun_parser_test.go.fixture"])
        old_test = workspace / "stun_old_test.go"
        if fixtures["stun_parser_test.go.fixture"].count(b'"github.com/pion/stun/v3"') != 1:
            raise ReviewError("legacy control import substitution is ambiguous")
        old_test.write_bytes(fixtures["stun_parser_test.go.fixture"].replace(
            b'"github.com/pion/stun/v3"', b'"github.com/pion/stun/v2"'))
        env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOTELEMETRY="off", GOFLAGS="",
                   GOPROXY="https://proxy.golang.org", GOSUMDB="sum.golang.org", GOPRIVATE="",
                   GONOPROXY="", GONOSUMDB="", CGO_ENABLED="0", GOMAXPROCS="2", GOMEMLIMIT="1536MiB")

        def run(label, argv, *, expected=0, limit=4 << 20):
            nonlocal phase
            phase = label
            remaining = 900 - (time.monotonic() - started)
            if remaining <= 0:
                raise ReviewError("aggregate migration command deadline exceeded")
            directory = args.output / label
            directory.mkdir()
            process = run_bounded(argv, directory, env=env, timeout=remaining,
                                  stdout_limit=limit, stderr_limit=256 << 10)
            for name, pin in process["streams"].items():
                (directory / pin["path"]).rename(directory / name)
                pin["path"] = label + "/" + name
            public_args = [str(arg).replace(str(workspace), "$CONTROL_WORKSPACE") for arg in argv]
            command = {"label": label, "argvProjection": public_args, "expectedExit": expected, "process": process}
            receipt["commands"].append(command)
            raw = file_bytes(directory / "stdout", limit)
            if label in ("legacy-parser", "candidate-parser") and process["outputComplete"]:
                command["testSummary"] = test_summary(raw)
            if process["actualExit"] != expected or not process["outputComplete"]:
                raise ReviewError("migration command did not complete with its expected outcome")
            return raw

        actual = json.loads(run("sdk", ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOTOOLCHAIN", "CGO_ENABLED"], limit=65536))
        if actual != {"GOVERSION": "go1.26.11", "GOOS": "linux", "GOARCH": "amd64", "GOTOOLCHAIN": "local", "CGO_ENABLED": "0"}:
            raise ReviewError("unexpected migration SDK or platform")
        receipt["goEnvironment"] = actual
        original = json.loads(run("original-module", ["go", "mod", "edit", "-json"], limit=65536))
        run("format-control-sources", ["gofmt", "-w", str(compat_source), str(old_test), str(new_test)])
        canonical = {"library_compatibility.go": file_bytes(compat_source, 65536),
                     "stun_parser_test.go": file_bytes(new_test, 65536)}
        if file_bytes(old_test, 65536) != canonical["stun_parser_test.go"].replace(
                b'"github.com/pion/stun/v3"', b'"github.com/pion/stun/v2"'):
            raise ReviewError("old and new parser controls differ beyond the declared import")
        canonical_pins = {}
        for name, content in canonical.items():
            with (args.output / ("canonical-" + name)).open("xb") as output:
                output.write(content)
            canonical_pins[name] = {"bytes": len(content), "sha256": sha(content)}
        receipt["canonicalControlSources"] = canonical_pins
        # A legacy failure is an explicit negative control, not a successful test.
        run("legacy-parser", ["go", "test", "-mod=readonly", "-trimpath", "-count=1", "-timeout=30s", "-json", str(old_test)], expected=1)
        legacy_command = receipt["commands"][-1]
        if not legacy_command["testSummary"]["failedTests"] or b"short attribute caused a panic" not in file_bytes(args.output / "legacy-parser/stdout", 4 << 20):
            raise ReviewError("legacy parser did not demonstrate the intended bounded regression")
        old_binary = workspace / "compatibility-old"
        run("legacy-build", ["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(old_binary), str(compat_source)])
        old_snapshot = json.loads(run("legacy-snapshot", [str(old_binary)]))
        if old_snapshot["effectiveOpGethVersion"] != OLD or old_snapshot["SDK"] != "go1.26.11":
            raise ReviewError("legacy compatibility binary has an unexpected embedded version")
        old_binary_sha = sha(file_bytes(old_binary, 64 << 20))
        run("select-library", ["go", "mod", "edit", "-replace=" + NOMINAL + "=" + FORK + "@" + NEW])
        run("tidy", ["go", "mod", "tidy"])
        run("verify", ["go", "mod", "verify"])
        final = json.loads(run("candidate-module", ["go", "mod", "edit", "-json"], limit=65536))
        expected_replacements = copy.deepcopy(original["Replace"])
        changed = 0
        for row in expected_replacements:
            if row["Old"]["Path"] == NOMINAL and row["New"] == {"Path": FORK, "Version": OLD}:
                row["New"]["Version"] = NEW
                changed += 1
        order = lambda values: sorted(json.dumps(row, sort_keys=True) for row in values)
        if (changed != 1 or final["Module"] != original["Module"] or final["Go"] != original["Go"] or
                final.get("Toolchain") != original.get("Toolchain") or order(final["Replace"]) != order(expected_replacements)):
            raise ReviewError("an unreviewed module identity, SDK or replacement changed")
        if any(not row.get("Indirect", False) and row not in final["Require"] for row in original["Require"]):
            raise ReviewError("an original direct dependency changed")
        module_rows = objects(run("module-inventory", ["go", "list", "-m", "-f", MODULE_TEMPLATE, "all"]))
        modules = {row["Path"]: row for row in module_rows}
        if len(modules) != len(module_rows) or modules[NOMINAL]["Replace"] != {"Path": FORK, "Version": NEW}:
            raise ReviewError("candidate replacement was not actually selected")
        for module, version in config["requiredResolved"].items():
            if modules.get(module, {}).get("Version") != version:
                raise ReviewError("candidate networking version differs from the reviewed selection")
        new_binary = workspace / "compatibility-new"
        run("candidate-build", ["go", "build", "-mod=readonly", "-trimpath", "-buildvcs=false", "-o", str(new_binary), str(compat_source)])
        new_snapshot = json.loads(run("candidate-snapshot", [str(new_binary)]))
        if new_snapshot["effectiveOpGethVersion"] != NEW or new_snapshot["SDK"] != "go1.26.11":
            raise ReviewError("candidate compatibility binary has an unexpected embedded version")
        payload = workspace / "candidate-payload.json"
        payload.write_text(json.dumps(new_snapshot["payload"], sort_keys=True))
        old_decoded_new = json.loads(run("legacy-decodes-candidate", [str(old_binary), "decode-payload", str(payload)]))
        phase = "compatibility_comparison"
        stable = {name: old_snapshot[name] == new_snapshot[name] for name in ("genesis", "transactions", "legacyHeader")}
        payload_keys = sorted(set(new_snapshot["payload"]) - set(old_snapshot["payload"]))
        if (not all(stable.values()) or old_decoded_new != old_snapshot["payload"] or
                new_snapshot["payload"] != {**old_snapshot["payload"], "targetGasLimit": None}):
            raise ReviewError("a selected chain-321 or legacy payload-codec compatibility vector changed")
        receipt["compatibility"] = {"stableVectors": stable, "oldCodecAcceptsCandidatePayload": True,
            "candidateAdditionalPayloadKeys": payload_keys,
            "slotfulHeaderChanged": old_snapshot["slotfulHeader"] != new_snapshot["slotfulHeader"],
            "oldBinarySha256": old_binary_sha, "candidateBinarySha256": sha(file_bytes(new_binary, 64 << 20)),
            "binariesRetainedOrPublished": False,
            "unqualifiedProfiles": ["Slotful headers and Amsterdam", "PostExec transaction/receipt changes", "Lagoon activation", "Active Engine API execution", "Live chain or operator state"]}
        run("candidate-parser", ["go", "test", "-mod=readonly", "-trimpath", "-count=1", "-timeout=30s", "-json", str(new_test)])
        if receipt["commands"][-1]["testSummary"]["testAndSubtestEvents"] != {"pass": 5, "fail": 0, "skip": 0}:
            raise ReviewError("candidate parser regression controls did not all pass")
        phase = "source_after"
        after = source_closure(args.source_commit, allow_root_locks=True)
        if after["core"] != before["core"]:
            raise ReviewError("Core source changed during migration preparation")
        receipt["sourceAfter"] = after
        pins = {}
        for name, limit in (("go.mod", 1 << 20), ("go.sum", 8 << 20)):
            raw = file_bytes(Path(name), limit)
            with (args.output / ("candidate." + name)).open("xb") as output:
                output.write(raw)
            pins[name] = {"bytes": len(raw), "sha256": sha(raw)}
        receipt.update(status="LIBRARY_MIGRATION_CANDIDATE_GENERATED_WITH_BOUNDED_COMPATIBILITY", candidateLocks=pins,
                       originalCoreSourceUnchanged=True, originalDirectDependenciesPreserved=True,
                       selectedReplacement=NEW, legacyParserFailurePreserved=True)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    finally:
        if workspace_created:
            shutil.rmtree(workspace)
    if created:
        save(args.output / "preparation.json", receipt)
    print(json.dumps({"status": receipt["status"], "exit": result, "failureStage": receipt.get("failureStage"), "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

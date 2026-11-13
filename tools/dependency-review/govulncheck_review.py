#!/usr/bin/env python3
"""Bound govulncheck execution and retain an explicitly scoped review result."""

import argparse
from datetime import datetime, timezone
import json
import math
import os
from pathlib import Path
import re
import subprocess
import sys

from bounded import ReviewError, file_bytes, run_bounded, save, sha, unique_object
from source_closure import source_closure as source_binding
from local_core import CORE_MODULE, CORE_REPLACEMENT, MODULE_TEMPLATE, bind_core, validate_profile

MAX_STDOUT = 8 << 20
MAX_STDERR = 256 << 10
MAX_MESSAGES = 20000
PROTOCOL = "v1.0.0"
SCANNER_VERSION = "v1.9.0"
DATABASE = "https://vuln.go.dev"
GO_ID = re.compile(r"GO-\d{4}-\d{4,}")
PORTABLE = re.compile(r"[A-Za-z0-9._~!+/@-]{1,512}")


def integer(raw):
    if len(raw) > 20:
        raise ReviewError("JSON integer exceeds its bound")
    return int(raw)


def reject_constant(_):
    raise ReviewError("non-finite JSON number")


def real(raw):
    value = float(raw)
    if not math.isfinite(value):
        raise ReviewError("non-finite JSON number")
    return value


def messages(raw):
    if not raw or len(raw) > MAX_STDOUT:
        raise ReviewError("empty or oversized stream")
    decoder = json.JSONDecoder(object_pairs_hook=unique_object, parse_int=integer, parse_float=real,
                               parse_constant=reject_constant)
    text = raw.decode("utf-8")
    offset, count = 0, 0
    while offset < len(text):
        while offset < len(text) and text[offset] in " \t\r\n":
            offset += 1
        if offset == len(text):
            break
        value, offset = decoder.raw_decode(text, offset)
        count += 1
        if count > MAX_MESSAGES or not isinstance(value, dict) or len(value) != 1:
            raise ReviewError("invalid bounded protocol message")
        yield value


def text_field(value, name, required=False, maximum=512):
    result = value.get(name, "")
    if not isinstance(result, str) or len(result) > maximum or (required and not result):
        raise ReviewError("invalid protocol string")
    return result


def timestamp(value):
    if not isinstance(value, str) or not re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z", value):
        raise ReviewError("missing database observation time")
    datetime.fromisoformat(value.split(".")[0].removesuffix("Z") + "+00:00")
    return value


def portable(value):
    return isinstance(value, str) and PORTABLE.fullmatch(value) and not value.startswith(("/", "~")) and not any(
        part in (".", "..") for part in value.split("/"))


def summarize(raw, *, go_version, mode="source", scanner_version=SCANNER_VERSION, database=DATABASE, core_profile=None):
    """Classify scanner reports, without independently asserting reachability."""
    if mode not in ("source", "binary"):
        raise ReviewError("unsupported review mode")
    core_profile = validate_profile(core_profile) if core_profile is not None else None
    local_core_occurrences = 0
    config = sbom = None
    advisories, findings = {}, []
    count = 0
    for message in messages(raw):
        count += 1
        kind, value = next(iter(message.items()))
        if not isinstance(value, dict) or (count == 1 and kind != "config"):
            raise ReviewError("missing initial scanner configuration")
        if kind == "config":
            allowed = {"protocol_version", "scanner_name", "scanner_version", "db", "db_last_modified",
                       "go_version", "scan_level", "scan_mode"}
            if config is not None or set(value) - allowed:
                raise ReviewError("ambiguous scanner configuration")
            expected = {"protocol_version": PROTOCOL, "scanner_name": "govulncheck",
                        "scanner_version": scanner_version, "db": database,
                        "scan_level": "symbol", "scan_mode": mode}
            if any(value.get(key) != wanted for key, wanted in expected.items()):
                raise ReviewError("scanner contract differs from independent expectations")
            if mode == "source" and value.get("go_version") != go_version:
                raise ReviewError("source analysis Go version differs")
            timestamp(value.get("db_last_modified"))
            config = value
        elif kind == "SBOM":
            if sbom is not None or set(value) - {"go_version", "modules", "roots"}:
                raise ReviewError("ambiguous scan inventory")
            if value.get("go_version") != go_version:
                raise ReviewError("scan inventory Go version differs")
            roots, modules = value.get("roots"), value.get("modules")
            if not isinstance(roots, list) or not roots or len(roots) > 4096:
                raise ReviewError("missing bounded scan roots")
            if not all(portable(root) for root in roots):
                raise ReviewError("invalid scan root")
            if not isinstance(modules, list) or not modules or len(modules) > 4096:
                raise ReviewError("missing bounded module inventory")
            selected = set()
            for module in modules:
                if not isinstance(module, dict) or set(module) - {"path", "version"}:
                    raise ReviewError("invalid inventory module")
                path = text_field(module, "path", required=True)
                version = text_field(module, "version", maximum=256)
                if path == CORE_REPLACEMENT:
                    if core_profile is None or version != "":
                        raise ReviewError("unbound or versioned local Core scan identity")
                    local_core_occurrences += 1
                elif not portable(path):
                    raise ReviewError("invalid inventory module")
                selected.add((path, version))
            sbom = value
        elif kind == "osv":
            identifier = text_field(value, "id", required=True)
            if not GO_ID.fullmatch(identifier):
                raise ReviewError("invalid advisory")
            canonical = json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True).encode()
            digest = sha(canonical)
            if identifier in advisories and advisories[identifier] != digest:
                raise ReviewError("conflicting advisory revisions")
            advisories[identifier] = digest
        elif kind == "finding":
            if set(value) - {"osv", "fixed_version", "trace"}:
                raise ReviewError("unknown finding fields")
            identifier = text_field(value, "osv", required=True)
            if not GO_ID.fullmatch(identifier):
                raise ReviewError("invalid finding identifier")
            fixed = text_field(value, "fixed_version", maximum=256)
            if fixed and not portable(fixed):
                raise ReviewError("invalid fixed version")
            trace = value.get("trace")
            if not isinstance(trace, list) or not trace or len(trace) > 256:
                raise ReviewError("missing bounded finding trace")
            for frame in trace:
                if not isinstance(frame, dict) or set(frame) - {"module", "version", "package", "function", "receiver", "position"}:
                    raise ReviewError("invalid finding frame")
                for key in ("module", "version", "package"):
                    item = text_field(frame, key, required=(key == "module"))
                    if key == "module" and item == CORE_REPLACEMENT:
                        if core_profile is None or frame.get("version", "") != "":
                            raise ReviewError("unbound local Core finding identity")
                    elif item and not portable(item):
                        raise ReviewError("nonportable finding identifier")
                if frame["module"] == CORE_REPLACEMENT and frame.get("package") and not (
                        frame["package"] == CORE_MODULE or frame["package"].startswith(CORE_MODULE + "/")):
                    raise ReviewError("local Core finding package namespace differs")
                text_field(frame, "function", maximum=1024)
                text_field(frame, "receiver", maximum=1024)
                if "position" in frame:
                    pos = frame["position"]
                    if not isinstance(pos, dict) or set(pos) - {"filename", "offset", "line", "column"}:
                        raise ReviewError("invalid source position")
                    text_field(pos, "filename", maximum=4096)
                    if any(type(pos.get(k, 0)) is not int or pos.get(k, 0) < 0 for k in ("offset", "line", "column")):
                        raise ReviewError("invalid source coordinates")
            first = trace[0]
            package, function = first.get("package", ""), first.get("function", "")
            if function and not package:
                raise ReviewError("symbol without a package")
            if not function and (len(trace) != 1 or first.get("receiver") or first.get("position")):
                raise ReviewError("ambiguous lower-level finding")
            if mode == "binary" and (len(trace) != 1 or any("position" in frame for frame in trace)):
                raise ReviewError("binary result cannot establish a source call trace")
            level = ("source_call_trace_reported" if mode == "source" else "compiled_symbol_reported") if function else (
                "package_reported" if package else "module_reported")
            findings.append({"id": identifier, "level": level, "module": first["module"],
                             "version": first.get("version", ""), "package": package,
                             "fixedVersion": fixed or None})
        elif kind == "progress":
            if set(value) - {"time", "message"}:
                raise ReviewError("unknown progress fields")
        else:
            raise ReviewError("unsupported protocol message")
    if config is None or sbom is None or any(row["id"] not in advisories for row in findings):
        raise ReviewError("incomplete configuration, inventory or advisory references")
    if core_profile is not None and not local_core_occurrences:
        raise ReviewError("the selected local Core is missing from the scan inventory")
    levels = {name: 0 for name in ("module_reported", "package_reported", "source_call_trace_reported", "compiled_symbol_reported")}
    distinct = {}
    for row in findings:
        levels[row["level"]] += 1
        key = json.dumps(row, sort_keys=True)
        distinct[key] = row
    return {"schema": 1, "status": "REVIEW_REQUIRED" if findings else "NO_FINDINGS_REPORTED_IN_THIS_SCAN",
        "scannerVersion": scanner_version, "scanMode": mode, "scanLevel": "symbol", "analysisGoVersion": go_version,
        "database": database, "databaseModifiedObserved": config["db_last_modified"],
        "databaseSnapshotAuthenticated": False, "localCoreBinding": core_profile,
        "localCoreModuleOccurrences": local_core_occurrences, "messages": count, "moduleRecords": len(selected),
        "moduleRecordOccurrences": len(sbom["modules"]),
        "duplicateModuleRecordOccurrences": len(sbom["modules"]) - len(selected),
        "roots": sorted(sbom["roots"]), "advisoryRecords": len(advisories), "advisoryDigests": dict(sorted(advisories.items())),
        "uniqueFindingAdvisories": len({row["id"] for row in findings}), "findingOccurrencesByLevel": levels,
        "findings": sorted(distinct.values(), key=lambda row: (row["id"], row["module"], row["package"], row["level"])),
        "runtimeExploitabilityEstablished": False, "unreportedMeansUnaffected": False,
        "replacementForkAdvisoryCoverageEstablished": False, "licenseReviewComplete": False, "releaseAccepted": False}


def file_digest(path, limit):
    return sha(file_bytes(path, limit))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--scanner", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--go-version", required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(), "scanMode": "source",
               "patterns": ["./..."], "testPackagesIncluded": True, "nestedCoreTestSuiteIncluded": False, "scannerVersionExpected": SCANNER_VERSION,
               "analysisGoVersionExpected": args.go_version, "releaseAccepted": False}
    created = False
    phase = "output_directory"
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_binding_before"
        before = source_binding(args.source_commit)
        receipt["source"] = before
        env = dict(os.environ, GOTELEMETRY="off", GOTOOLCHAIN="local", GOWORK="off", CGO_ENABLED="0",
                   GOFLAGS="-mod=readonly", GOMAXPROCS="2", GOMEMLIMIT="1536MiB",
                   GOPROXY="https://proxy.golang.org", GOSUMDB="sum.golang.org", GOPRIVATE="",
                   GONOPROXY="", GONOSUMDB="")
        phase = "local_core_module_binding"
        module_dir = args.output / "local-core-module"
        module_dir.mkdir()
        module_process = run_bounded(["go", "list", "-m", "-f", MODULE_TEMPLATE, CORE_MODULE], module_dir,
                                     env=env, timeout=60, stdout_limit=65536, stderr_limit=65536)
        for name, pin in module_process["streams"].items():
            (module_dir / pin["path"]).rename(module_dir / name)
            pin["path"] = "local-core-module/" + name
        receipt["localCoreModuleProcess"] = module_process
        if module_process["actualExit"] != 0 or not module_process["outputComplete"]:
            raise ReviewError("local Core module observation did not complete")
        core_binding = bind_core(before, file_bytes(module_dir / "stdout", 65536))
        receipt["localCoreBinding"] = core_binding
        scanner = args.scanner.resolve(strict=True)
        scanner_sha = file_digest(scanner, 64 << 20)
        receipt.update(scannerBinarySha256=scanner_sha, scannerOriginAuthenticated=False)
        argv = [str(scanner), "-json", "-scan=symbol", "-mode=source", "-test", "-db=" + DATABASE, "./..."]
        phase = "scanner_capture"
        process = run_bounded(argv, args.output, env=env)
        receipt["process"] = process
        phase = "source_binding_after"
        if source_binding(args.source_commit) != before or file_digest(scanner, 64 << 20) != scanner_sha:
            raise ReviewError("source or selected scanner changed")
        if process["actualExit"] != 0 or not process["outputComplete"]:
            raise ReviewError("scanner did not complete successfully")
        raw = (args.output / "govulncheck.json").read_bytes()
        if sha(raw) != process["streams"]["stdout"]["sha256"]:
            raise ReviewError("retained output changed")
        phase = "protocol_review"
        summary = summarize(raw, go_version=args.go_version, core_profile=core_binding)
        summary["source"] = before
        summary["processReceiptRequiredAndVerified"] = True
        save(args.output / "review.json", summary)
        receipt["status"] = summary["status"]
        result = 1 if summary["uniqueFindingAdvisories"] else 0
    except (ReviewError, OSError, ValueError, RecursionError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(status="SCAN_FAILED_OR_INCOMPLETE", errorType=type(error).__name__, failureStage=phase)
        result = 2
    if created:
        try:
            save(args.output / "execution.json", receipt)
        except FileExistsError:
            result = 2
    print(json.dumps({"status": receipt.get("status"), "exit": result, "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

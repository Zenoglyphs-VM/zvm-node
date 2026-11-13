#!/usr/bin/env python3
"""Record selected package import graphs, not exercised vulnerability reachability."""

import argparse
from datetime import datetime, timezone
import json
import os
from pathlib import Path
import re
import subprocess
import sys

from govulncheck_review import (ReviewError, file_bytes, integer, real, reject_constant,
                               run_bounded, save, sha, source_binding, unique_object)

from local_core import CORE_MODULE, CORE_VERSION, CORE_REPLACEMENT, MODULE_TEMPLATE, bind_core, validate_profile

MAX_BYTES = 4 << 20
MAX_PACKAGES = 4096
MAX_EDGES = 65536
PATH = re.compile(r"[A-Za-z0-9._~!+/-]{1,512}")
VERSION = re.compile(r"v\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?")
FIELDS = {"ImportPath", "Name", "Standard", "ForTest", "DepOnly", "Incomplete", "Imports", "Module"}

# -json=Fields specifies required fields; it is not a field-exclusion contract.
# Render only these public identifiers, never Dir, Root, GoMod or generated-file paths.
TEMPLATE = (r'{"ImportPath":{{printf "%q" .ImportPath}},"Name":{{printf "%q" .Name}},'
            r'"Standard":{{.Standard}},"ForTest":{{printf "%q" .ForTest}},'
            r'"DepOnly":{{.DepOnly}},"Incomplete":{{.Incomplete}},"Imports":['
            r'{{range $i,$v := .Imports}}{{if $i}},{{end}}{{printf "%q" $v}}{{end}}],'
            r'"Module":{{with .Module}}{"Path":{{printf "%q" .Path}},'
            r'"Version":{{printf "%q" .Version}},"Main":{{.Main}},"Replace":'
            r'{{with .Replace}}{"Path":{{printf "%q" .Path}},"Version":{{printf "%q" .Version}}}'
            r'{{else}}null{{end}}}{{else}}null{{end}}}')


def path(value):
    return (isinstance(value, str) and PATH.fullmatch(value) and not value.startswith(("/", ".", "~"))
            and not any(p in ("", ".", "..") for p in value.split("/")))


def package_id(value):
    if not isinstance(value, str) or len(value) > 1024:
        raise ReviewError("invalid package identifier")
    parts = value.split(" [")
    if len(parts) == 1 and path(value):
        return value
    if (len(parts) == 2 and path(parts[0]) and parts[1].endswith(".test]")
            and path(parts[1][:-1])):
        return parts[0]
    raise ReviewError("invalid package or test-variant identifier")


def module(value, *, core_profile=None):
    if value is None:
        return None
    if not isinstance(value, dict) or set(value) != {"Path", "Version", "Main", "Replace"}:
        raise ReviewError("invalid module projection")
    if not path(value["Path"]) or type(value["Main"]) is not bool:
        raise ReviewError("invalid module identity")
    version, replacement = value["Version"], value["Replace"]
    if value["Main"]:
        if version != "" or replacement is not None:
            raise ReviewError("unsupported main module profile")
    elif not isinstance(version, str) or not VERSION.fullmatch(version):
        raise ReviewError("unknown or non-versioned dependency")
    if isinstance(replacement, dict) and replacement.get("Path") == CORE_REPLACEMENT:
        wanted = {"Path": CORE_MODULE, "Version": CORE_VERSION, "Main": False,
                  "Replace": {"Path": CORE_REPLACEMENT, "Version": ""}}
        if core_profile is None or value != wanted:
            raise ReviewError("local Core import identity differs from the bound source profile")
        validate_profile(core_profile)
        return value
    if replacement is not None:
        if (not isinstance(replacement, dict) or set(replacement) != {"Path", "Version"}
                or not path(replacement["Path"]) or not isinstance(replacement["Version"], str)
                or not VERSION.fullmatch(replacement["Version"])):
            raise ReviewError("local or unversioned replacement needs separate review")
    return value


def summarize(raw, *, entry, core_profile=None):
    core_profile = validate_profile(core_profile) if core_profile is not None else None
    if not path(entry) or not raw or len(raw) > MAX_BYTES:
        raise ReviewError("invalid entry or stream budget")
    decoder = json.JSONDecoder(object_pairs_hook=unique_object, parse_int=integer,
                               parse_float=real, parse_constant=reject_constant)
    text = raw.decode("utf-8")
    offset = edges = 0
    packages, modules = {}, {}
    while offset < len(text):
        while offset < len(text) and text[offset] in " \t\r\n":
            offset += 1
        if offset == len(text):
            break
        row, offset = decoder.raw_decode(text, offset)
        if not isinstance(row, dict) or set(row) != FIELDS:
            raise ReviewError("unexpected or missing projected fields")
        identity = row["ImportPath"]
        package_id(identity)
        if identity in packages or len(packages) >= MAX_PACKAGES:
            raise ReviewError("duplicate package or package budget exceeded")
        if (not isinstance(row["Name"], str) or not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", row["Name"])
                or any(type(row[key]) is not bool for key in ("Standard", "DepOnly", "Incomplete"))
                or row["Incomplete"] or not isinstance(row["ForTest"], str)
                or (row["ForTest"] and not path(row["ForTest"]))):
            raise ReviewError("incomplete or invalid package profile")
        if " [" in identity and identity.split(" [", 1)[1] != row["ForTest"] + ".test]":
            raise ReviewError("test variant disagrees with ForTest")
        imports = row["Imports"]
        if (not isinstance(imports, list) or len(imports) > 2048
                or any(not isinstance(v, str) for v in imports)):
            raise ReviewError("invalid import edges")
        for imported in imports:
            package_id(imported)
        edges += len(imports)
        if edges > MAX_EDGES:
            raise ReviewError("import edge budget exceeded")
        selected = module(row["Module"], core_profile=core_profile)
        if row["Standard"] and selected is not None:
            raise ReviewError("standard package has unexpected module metadata")
        if not row["Standard"] and selected is None and not identity.endswith(".test"):
            raise ReviewError("non-standard package has no selected module identity")
        if selected is not None:
            base_path = package_id(identity)
            if base_path != selected["Path"] and not base_path.startswith(selected["Path"] + "/"):
                raise ReviewError("package and nominal module namespaces disagree")
            previous = modules.setdefault(selected["Path"], selected)
            if previous != selected:
                raise ReviewError("conflicting selected module versions or replacements")
        packages[identity] = row
    if not packages or entry not in packages:
        raise ReviewError("selected entry is absent")
    selected = packages[entry]
    if (selected["Name"] != "main" or selected["ForTest"] or selected["Standard"]
            or selected["DepOnly"] or not selected["Module"] or not selected["Module"]["Main"]):
        raise ReviewError("selected entry is not an ordinary main-module command")
    if core_profile is not None and CORE_MODULE not in modules:
        raise ReviewError("the selected local Core is missing from the import graph")
    if any(imported not in packages for row in packages.values() for imported in row["Imports"]):
        raise ReviewError("import graph is not closed")
    # A successful Go package graph is acyclic. Reject an ambiguous cyclic input.
    remaining = {name: len(row["Imports"]) for name, row in packages.items()}
    reverse = {name: [] for name in packages}
    for name, row in packages.items():
        for imported in row["Imports"]:
            reverse[imported].append(name)
    ready = [name for name, count in remaining.items() if count == 0]
    visited = 0
    while ready:
        visited += 1
        for parent in reverse[ready.pop()]:
            remaining[parent] -= 1
            if remaining[parent] == 0:
                ready.append(parent)
    if visited != len(packages):
        raise ReviewError("cyclic import graph")

    def closure(roots):
        result, pending = set(), list(roots)
        while pending:
            name = pending.pop()
            if name not in result:
                result.add(name)
                pending.extend(packages[name]["Imports"])
        return result

    test_roots = sorted(name for name, row in packages.items()
                        if row["ForTest"] or name.endswith(".test"))
    if not test_roots:
        raise ReviewError("test-package profile did not report any test graph")
    ordinary_roots = sorted(name for name, row in packages.items()
                            if not row["DepOnly"] and not row["ForTest"] and not name.endswith(".test"))
    cli = closure([entry])
    tests = closure(test_roots)
    distinct_edges = sum(len(set(row["Imports"])) for row in packages.values())
    return {"schema": 1, "status": "SELECTED_IMPORT_GRAPHS_RECORDED", "entryPackage": entry,
            "packageRecords": len(packages), "importEdges": distinct_edges,
            "importEdgeOccurrences": edges, "duplicateImportEdgeOccurrences": edges - distinct_edges,
            "ordinaryRoots": ordinary_roots,
            "testRoots": test_roots, "entryImportClosure": sorted(cli),
            "testImportClosure": sorted(tests), "testOnlyImportIDs": sorted(tests - cli),
            "selectedModules": [modules[name] for name in sorted(modules)],
            "localCoreBinding": core_profile,
            "exercisedFunctionOrExploitReachabilityEstablished": False,
            "replacementForkAdvisoryCoverageEstablished": False, "waiversGenerated": False,
            "releaseAccepted": False}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--go-version", required=True)
    parser.add_argument("--entry-package", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(),
               "status": "IMPORT_GRAPH_FAILED_OR_INCOMPLETE", "commands": [],
               "moduleArchiveDownloadsEnabled": False, "releaseAccepted": False}
    env = dict(os.environ, GOTOOLCHAIN="local", GOWORK="off", GOTELEMETRY="off",
               GOFLAGS="-mod=readonly", CGO_ENABLED="0", GOPROXY="off", GOSUMDB="off",
               GONOPROXY="none", GONOSUMDB="none", GOPRIVATE="", GOMAXPROCS="2", GOMEMLIMIT="1536MiB")
    created = False
    phase = "output_directory"
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_before"
        before = source_binding(args.source_commit)
        receipt["source"] = before

        def run(label, argv, limit):
            nonlocal phase
            phase = label
            directory = args.output / label
            directory.mkdir()
            result = run_bounded(argv, directory, env=env, timeout=120, stdout_limit=limit, stderr_limit=65536)
            for name, pin in result["streams"].items():
                (directory / pin["path"]).rename(directory / name)
                pin["path"] = label + "/" + name
            receipt["commands"].append({"label": label, "process": result})
            if result["actualExit"] != 0 or not result["outputComplete"]:
                raise ReviewError("package metadata command did not complete")
            return file_bytes(directory / "stdout", limit)

        profile = json.loads(run("profile", ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "CGO_ENABLED"], 16384))
        expected = {"GOVERSION": args.go_version, "GOOS": "linux", "GOARCH": "amd64", "CGO_ENABLED": "0"}
        if not re.fullmatch(r"go1\.\d+\.\d+", args.go_version) or profile != expected:
            raise ReviewError("unexpected source-analysis SDK or build profile")
        receipt["profile"] = profile
        core_selection = run("local-core-module", ["go", "list", "-m", "-f", MODULE_TEMPLATE, CORE_MODULE], 65536)
        core_binding = bind_core(before, core_selection)
        receipt["localCoreBinding"] = core_binding
        raw = run("packages", ["go", "list", "-deps", "-test", "-f", TEMPLATE, "./..."], MAX_BYTES)
        phase = "source_after"
        if source_binding(args.source_commit) != before:
            raise ReviewError("source changed during package observation")
        phase = "graph_review"
        summary = summarize(raw, entry=args.entry_package, core_profile=core_binding)
        summary["source"] = before
        summary["profile"] = profile
        summary["producerOutputSha256"] = sha(raw)
        save(args.output / "graph.json", summary)
        receipt["status"] = summary["status"]
        result = 0
    except (ReviewError, OSError, ValueError, RecursionError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    if created:
        save(args.output / "execution.json", receipt)
    print(json.dumps({"status": receipt["status"], "exit": result, "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

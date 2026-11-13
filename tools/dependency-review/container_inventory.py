#!/usr/bin/env python3
"""Retain bounded observations of one local CI image; never publish an image."""

import argparse
from datetime import datetime, timezone
import json
from pathlib import Path
import re
import subprocess
import sys

from bounded import ReviewError, file_bytes, run_bounded, save
from source_closure import source_closure
from notice_inventory import EXPORT_SCRIPT, APK_SCRIPT, MAX_EXPORT, summarize_export, expected_readme, apk_licenses


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--source-commit", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    receipt = {"schema": 1, "observedAt": datetime.now(timezone.utc).isoformat(),
               "status": "IMAGE_OBSERVATION_FAILED_OR_INCOMPLETE", "commands": [],
               "completeSBOM": False, "vulnerabilityScanPerformed": False,
               "imageOriginAuthenticated": False, "releaseAccepted": False}
    created = False
    phase = "output_directory"
    try:
        args.output.mkdir(mode=0o700)
        created = True
        phase = "source_before"
        before = source_closure(args.source_commit)
        receipt["source"] = before

        def run(label, argv, stdout_limit=65536):
            nonlocal phase
            phase = label
            directory = args.output / label
            directory.mkdir()
            result = run_bounded(argv, directory, timeout=30,
                                 stdout_limit=stdout_limit, stderr_limit=65536)
            for name, pin in result["streams"].items():
                (directory / pin["path"]).rename(directory / name)
                pin["path"] = label + "/" + name
            receipt["commands"].append({"label": label, "process": result})
            if result["actualExit"] != 0 or not result["outputComplete"]:
                raise ReviewError("image observation command did not complete")
            return file_bytes(directory / "stdout", stdout_limit)

        template = ('{"id":{{json .Id}},"os":{{json .Os}},'
                    '"architecture":{{json .Architecture}},'
                    '"entrypoint":{{json .Config.Entrypoint}},"user":{{json .Config.User}}}')
        image = json.loads(run("image", ["docker", "image", "inspect", "--format", template, "zvm-node:ci"]))
        if (not re.fullmatch(r"sha256:[0-9a-f]{64}", image["id"]) or image["os"] != "linux" or
                image["entrypoint"] != ["zvm-node"]):
            raise ReviewError("unexpected CI image identity or entrypoint")
        # Pin the observed immutable local image ID for every subsequent command.
        common = ["docker", "run", "--rm", "--network=none", "--read-only", "--user=65534:65534",
                  "--cap-drop=ALL", "--security-opt=no-new-privileges"]
        packages = run("packages", common + ["--entrypoint=/sbin/apk", image["id"], "info", "-v"]).decode().splitlines()
        if (not 1 <= len(packages) <= 512 or len(set(packages)) != len(packages) or
                not {"ca-certificates-20261020-r0", "wget-1.25.0-r1", "gcompat-1.1.0-r4"} <= set(packages) or
                any(not re.fullmatch(r"[A-Za-z0-9_.+~-]+", item) for item in packages)):
            raise ReviewError("unexpected installed package inventory")
        digest = run("binary", common + ["--entrypoint=/bin/busybox", image["id"], "sha256sum", "/usr/local/bin/zvm-node"]).decode().split()
        if len(digest) != 2 or not re.fullmatch(r"[0-9a-f]{64}", digest[0]) or digest[1] != "/usr/local/bin/zvm-node":
            raise ReviewError("invalid CLI binary digest")
        declarations = run("apk-licenses", common + ["--entrypoint=/bin/sh", image["id"], "-euc", APK_SCRIPT], stdout_limit=2 << 20)
        apk_license_records = apk_licenses(declarations, packages)
        exported = run("notices", common + ["--entrypoint=/bin/sh", image["id"], "-euc", EXPORT_SCRIPT], stdout_limit=MAX_EXPORT)
        notice_summary = summarize_export(exported, before, digest[0], expected_readme(),
            lambda path, limit: file_bytes(Path(path), limit))
        phase = "source_after"
        if source_closure(args.source_commit) != before:
            raise ReviewError("source changed during image observation")
        receipt.update(status="IMAGE_AND_PACKAGE_INPUTS_OBSERVED", image=image,
                       installedPackages=sorted(packages), cliBinarySha256=digest[0],
                       hostPathsOrConfigValuesRecorded=False, sourceNotices=notice_summary,
                       runtimeAPKLicenseDeclarations=apk_license_records, legalReviewComplete=False)
        result = 0
    except (ReviewError, OSError, ValueError, KeyError, TypeError, subprocess.SubprocessError) as error:
        receipt.update(failureStage=phase, errorType=type(error).__name__)
        result = 2
    if created:
        save(args.output / "inventory.json", receipt)
    print(json.dumps({"status": receipt["status"], "exit": result, "completeSBOM": False,
                      "releaseAccepted": False}))
    return result


if __name__ == "__main__":
    sys.exit(main())

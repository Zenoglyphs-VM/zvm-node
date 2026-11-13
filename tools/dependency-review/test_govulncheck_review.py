"""Protocol controls and hosted POSIX process-budget regressions."""

import copy
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

import govulncheck_review as review


def stream(*extra, mode="source"):
    config = {"protocol_version": "v1.0.0", "scanner_name": "govulncheck", "scanner_version": "v1.9.0",
              "db": "https://vuln.go.dev", "db_last_modified": "2026-11-01T12:34:56.123456789Z",
              "go_version": "go1.25.14", "scan_level": "symbol", "scan_mode": mode}
    sbom = {"go_version": "go1.25.14", "modules": [{"path": "example.org/project", "version": "v1.0.0"},
             {"path": "example.org/module", "version": "v1.0.0"}],
            "roots": ["example.org/project"]}
    return [{"config": config}, {"SBOM": sbom}, *extra]


def encode(rows):
    return ("\n".join(json.dumps(row, indent=2) for row in rows) + "\n").encode()


def advisory():
    return {"osv": {"id": "GO-2026-0001", "modified": "2026-11-01T12:00:00Z",
                    "affected": [{"package": {"ecosystem": "Go", "name": "example.org/module"}}]}}


def finding(level="module"):
    frame = {"module": "example.org/module", "version": "v1.0.0"}
    if level != "module":
        frame["package"] = "example.org/module/parser"
    if level == "symbol":
        frame["function"] = "Parse"
    return {"finding": {"osv": "GO-2026-0001", "fixed_version": "v1.0.1", "trace": [frame]}}


class ProtocolTests(unittest.TestCase):
    def summary(self, rows, **kw):
        return review.summarize(encode(rows), go_version="go1.25.14", **kw)

    def reject(self, rows, **kw):
        with self.assertRaises((ValueError, RecursionError)):
            self.summary(rows, **kw)

    def test_no_finding_does_not_claim_release_or_absence(self):
        result = self.summary(stream())
        self.assertEqual(result["status"], "NO_FINDINGS_REPORTED_IN_THIS_SCAN")
        for key in ("releaseAccepted", "unreportedMeansUnaffected", "replacementForkAdvisoryCoverageEstablished"):
            self.assertIs(result[key], False)

    def test_advisory_without_affected_version_finding(self):
        result = self.summary(stream(advisory()))
        self.assertEqual(result["advisoryRecords"], 1)
        self.assertEqual(result["uniqueFindingAdvisories"], 0)

    def test_module_only_still_requires_review(self):
        result = self.summary(stream(advisory(), finding()))
        self.assertEqual(result["status"], "REVIEW_REQUIRED")
        self.assertEqual(result["findingOccurrencesByLevel"]["module_reported"], 1)

    def test_levels_and_repeated_occurrences_remain_distinct(self):
        result = self.summary(stream(advisory(), finding(), finding("package"), finding("symbol"), finding("symbol")))
        self.assertEqual(result["uniqueFindingAdvisories"], 1)
        self.assertEqual(len(result["findings"]), 3)
        self.assertEqual(result["findingOccurrencesByLevel"]["source_call_trace_reported"], 2)

    def test_binary_symbol_is_not_a_source_call(self):
        result = self.summary(stream(advisory(), finding("symbol"), mode="binary"), mode="binary")
        self.assertEqual(result["findingOccurrencesByLevel"]["compiled_symbol_reported"], 1)
        self.assertEqual(result["findingOccurrencesByLevel"]["source_call_trace_reported"], 0)

    def test_message_order_after_configuration_is_not_assumed(self):
        rows = stream(finding("symbol"), advisory())
        rows.append(rows.pop(1))
        self.assertEqual(self.summary(rows)["uniqueFindingAdvisories"], 1)

    def test_wrong_independent_config_refused(self):
        changes = {"protocol_version": "v2", "scanner_name": "other", "scanner_version": "v0",
                   "db": "https://other.invalid", "go_version": "go1.25.0", "scan_level": "module", "scan_mode": "query"}
        for key, value in changes.items():
            with self.subTest(field=key):
                rows = stream(); rows[0]["config"][key] = value
                self.reject(rows)

    def test_configuration_must_be_first_and_unique(self):
        rows = stream()
        for bad in (rows[1:], list(reversed(rows)), rows + [copy.deepcopy(rows[0])]):
            with self.subTest(shape=len(bad)):
                self.reject(bad)

    def test_missing_or_ambiguous_inventory_refused(self):
        self.reject(stream()[:1])
        self.reject(stream() + [copy.deepcopy(stream()[1])])
        for key, value in (("go_version", "go1.25.0"), ("roots", []), ("modules", []),
                           ("roots", ["/private-fixture/project"]), ("modules", [{"path": "../private"}])):
            with self.subTest(field=key):
                rows = stream(); rows[1]["SBOM"][key] = value
                self.reject(rows)

    def test_repeated_effective_replacement_record_is_counted_once(self):
        # This pair was repeated in the actual v1.9.0 test-package scan.
        rows = stream()
        effective = {"path": "github.com/ethereum-optimism/op-geth", "version": "v1.101702.2"}
        rows[1]["SBOM"]["modules"] = [effective, copy.deepcopy(effective)]
        result = self.summary(rows)
        self.assertEqual(result["moduleRecords"], 1)
        self.assertEqual(result["moduleRecordOccurrences"], 2)
        self.assertEqual(result["duplicateModuleRecordOccurrences"], 1)

    def test_distinct_effective_versions_remain_separate(self):
        rows = stream()
        rows[1]["SBOM"]["modules"] = [{"path": "example.org/module", "version": "v1.0.0"},
            {"path": "example.org/module", "version": "v2.0.0"}]
        result = self.summary(rows)
        self.assertEqual(result["moduleRecords"], 2)
        self.assertEqual(result["moduleRecordOccurrences"], 2)
        self.assertEqual(result["duplicateModuleRecordOccurrences"], 0)

    def test_missing_advisory_reference_refused(self):
        self.reject(stream(finding()))

    def test_identical_advisory_repeat_allowed_conflicting_revision_refused(self):
        self.assertEqual(self.summary(stream(advisory(), advisory()))["advisoryRecords"], 1)
        changed = advisory(); changed["osv"]["modified"] = "2026-11-02T00:00:00Z"
        self.reject(stream(advisory(), changed))

    def test_invalid_or_empty_trace_refused(self):
        for trace in ([], None, [{"module": "../private"}], [{"module": "x", "function": "Parse"}]):
            with self.subTest(trace=trace):
                bad = finding(); bad["finding"]["trace"] = trace
                self.reject(stream(advisory(), bad))

    def test_lower_level_cannot_carry_a_source_trace(self):
        bad = finding(); bad["finding"]["trace"].append({"module": "example.org/project"})
        self.reject(stream(advisory(), bad))

    def test_binary_cannot_carry_source_positions(self):
        bad = finding("symbol"); bad["finding"]["trace"][0]["position"] = {"filename": "x.go", "line": 1}
        self.reject(stream(advisory(), bad, mode="binary"), mode="binary")

    def test_private_source_positions_are_omitted_from_summary(self):
        bad = finding("symbol")
        bad["finding"]["trace"][0]["position"] = {"filename": "/private-fixture/project.go", "line": 42}
        result = self.summary(stream(advisory(), bad))
        self.assertNotIn("private-fixture", json.dumps(result))
        self.assertNotIn("project.go", json.dumps(result))

    def test_unknown_fixed_version_remains_unknown(self):
        row = finding(); row["finding"].pop("fixed_version")
        self.assertIsNone(self.summary(stream(advisory(), row))["findings"][0]["fixedVersion"])

    def test_invalid_database_dates_refused(self):
        for value in (None, "", "2026-11-01Z", "2026-02-30T12:00:00Z"):
            with self.subTest(date=value):
                rows = stream(); rows[0]["config"]["db_last_modified"] = value
                self.reject(rows)

    def test_duplicate_members_trailing_garbage_and_truncation_refused(self):
        raw = encode(stream())
        inputs = [raw + b"garbage", raw[:-3], raw.replace(b'"symbol"', b'"symbol", "scan_level": "symbol"', 1),
                  raw.replace(b'"version": "v1.0.0"', b'"version": "v1.0.0", "version": "v2"', 1)]
        for value in inputs:
            with self.subTest(bytes=len(value)):
                with self.assertRaises(ValueError):
                    review.summarize(value, go_version="go1.25.14")

    def test_empty_oversized_nonfinite_or_excessive_messages_refused(self):
        for raw in (b"", b" " * (review.MAX_STDOUT + 1), b'{"config":NaN}', b'{"config":1e999}',
                    encode(stream()) + b'{"progress":{}}\n' * review.MAX_MESSAGES):
            with self.subTest(bytes=len(raw)):
                with self.assertRaises(ValueError):
                    review.summarize(raw, go_version="go1.25.14")

    def test_unknown_message_or_field_refused(self):
        self.reject(stream({"success": {}}))
        rows = stream(); rows[0]["config"]["extra"] = True
        self.reject(rows)


class ProcessTests(unittest.TestCase):
    """Run in the hosted POSIX lane; local qualification selects ProtocolTests."""

    def capture(self, code, **kw):
        with tempfile.TemporaryDirectory() as folder:
            result = review.run_bounded([sys.executable, "-c", code], folder, timeout=kw.pop("timeout", 3), **kw)
            stdout = (Path(folder) / "govulncheck.json").read_bytes()
            stderr = (Path(folder) / "govulncheck.stderr").read_bytes()
            self.assertEqual(len(stdout), result["streams"]["stdout"]["bytes"])
            self.assertEqual(review.sha(stdout), result["streams"]["stdout"]["sha256"])
            self.assertEqual(review.sha(stderr), result["streams"]["stderr"]["sha256"])
            return result, stdout, stderr

    def test_zero_exit_retains_both_complete_streams(self):
        result, out, err = self.capture("import sys;print('out');print('err',file=sys.stderr)")
        self.assertEqual((out, err), (b"out\n", b"err\n"))
        self.assertEqual(result["actualExit"], 0)
        self.assertTrue(result["outputComplete"])

    def test_nonzero_exit_is_preserved(self):
        result, _, _ = self.capture("raise SystemExit(7)")
        self.assertEqual(result["actualExit"], 7)
        self.assertTrue(result["outputComplete"])

    def test_stdout_flood_is_bounded(self):
        result, out, _ = self.capture("import os;os.write(1,b'x'*65536)", stdout_limit=1024)
        self.assertEqual(len(out), 1024)
        self.assertEqual(result["terminationReason"], "stdout_limit_exceeded")
        self.assertFalse(result["outputComplete"])

    def test_stderr_flood_is_bounded(self):
        result, _, err = self.capture("import os;os.write(2,b'x'*65536)", stderr_limit=1024)
        self.assertEqual(len(err), 1024)
        self.assertEqual(result["terminationReason"], "stderr_limit_exceeded")

    def test_deadline_is_not_a_success_even_if_output_is_valid(self):
        result, _, _ = self.capture("import time;print('{}',flush=True);time.sleep(30)", timeout=0.2)
        self.assertEqual(result["terminationReason"], "deadline_exceeded")
        self.assertFalse(result["outputComplete"])

    def test_existing_output_files_are_preserved(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "govulncheck.json"; path.write_bytes(b"original")
            with self.assertRaises(FileExistsError):
                review.run_bounded([sys.executable, "-c", "pass"], folder)
            self.assertEqual(path.read_bytes(), b"original")

    def test_existing_output_directory_is_not_adopted_by_cli(self):
        with tempfile.TemporaryDirectory() as folder:
            path = Path(folder) / "existing"; path.mkdir(); (path / "original").write_text("keep")
            done = subprocess.run([sys.executable, "-B", str(Path(review.__file__)), "--scanner", sys.executable,
                "--output", str(path), "--source-commit", "1" * 40, "--go-version", "go1.25.14"], capture_output=True)
            self.assertEqual(done.returncode, 2)
            self.assertEqual([item.name for item in path.iterdir()], ["original"])


if __name__ == "__main__":
    unittest.main()

"""Data-only refusal controls for complete and honestly counted Go test streams."""

import json
import unittest

from bounded import ReviewError
from native_qualification import test_summary


def stream(*rows):
    return b"".join(json.dumps(row).encode() + b"\n" for row in rows)


class TestEventTests(unittest.TestCase):
    def test_failures_and_skips_remain_visible_with_parent_and_subtest_counts(self):
        raw = stream({"Action": "pass", "Package": "example", "Test": "TestParent/child"},
                     {"Action": "skip", "Package": "example", "Test": "TestOptional"},
                     {"Action": "fail", "Package": "example", "Test": "TestParent"},
                     {"Action": "fail", "Package": "example"})
        result = test_summary(raw)
        self.assertEqual(result["testAndSubtestEvents"], {"pass": 1, "fail": 1, "skip": 1})
        self.assertEqual(result["packages"], {"example": "fail"})
        self.assertEqual(result["failedTests"][0]["test"], "TestParent")
        self.assertEqual(result["skippedTests"][0]["test"], "TestOptional")

    def test_truncated_duplicate_and_non_object_json_are_refused(self):
        for raw in (b'{"Action":"pass"}', b'{"Action":"pass","Action":"skip"}\n', b'[]\n'):
            with self.subTest(raw=raw), self.assertRaises((ReviewError, ValueError)):
                test_summary(raw)

    def test_duplicate_terminal_test_and_package_events_are_refused(self):
        terminal = {"Action": "pass", "Package": "example", "Test": "TestOnce"}
        package = {"Action": "pass", "Package": "example"}
        for rows in ((terminal, terminal, package), (terminal, package, package)):
            with self.subTest(rows=rows), self.assertRaises(ReviewError):
                test_summary(stream(*rows))

    def test_missing_package_completion_cannot_qualify_partial_output(self):
        for rows in (({"Action": "pass", "Package": "example", "Test": "TestOnce"},),
                     ({"Action": "pass", "Package": "example"},)):
            with self.subTest(rows=rows), self.assertRaises(ReviewError):
                test_summary(stream(*rows))


if __name__ == "__main__":
    unittest.main()

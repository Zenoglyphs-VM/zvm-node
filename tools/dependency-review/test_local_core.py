"""Data controls for exact local-source binding without advisory or graph waivers."""

import copy
import json
import unittest

from bounded import ReviewError
import govulncheck_review as review
import import_graph as graph
import local_core as local
from test_govulncheck_review import advisory, encode, finding, stream
from test_import_graph import ENTRY, encoded, fixture, package


def source():
    return {"sourceProfile": "NODE_AND_EXPLICIT_CORE_GITLINK", "authenticationConfigValuesPrinted": False,
            "root": {"rootLockOverrides": {}},
            "core": {"commit": local.CORE_COMMIT, "tree": local.CORE_TREE, "path": "zvm-core",
                     "url": local.CORE_URL, "goModSha256": local.CORE_MOD_SHA256,
                     "goSumSha256": local.CORE_SUM_SHA256, "rootLockOverrides": {}}}


def selection():
    return {"Path": local.CORE_MODULE, "Version": local.CORE_VERSION, "Main": False,
            "Replace": {"Path": local.CORE_REPLACEMENT, "Version": ""}}


def scan_rows(*extra):
    rows = stream(*extra)
    rows[0]["config"]["go_version"] = rows[1]["SBOM"]["go_version"] = "go1.26.11"
    rows[1]["SBOM"]["modules"].append({"path": local.CORE_REPLACEMENT})
    return rows


def graph_rows():
    rows = fixture()
    core_package = local.CORE_MODULE + "/amount"
    rows[0]["Imports"].append(core_package)
    rows.append(package(core_package, selected=selection()))
    return rows


class CoreBindingTests(unittest.TestCase):
    def test_exact_source_and_module_metadata_require_independent_source_evidence(self):
        profile = local.bind_core(source(), json.dumps(selection()).encode())
        self.assertEqual(profile, local.expected_profile())
        self.assertIs(profile["independentSourceEvidenceRequired"], True)

    def test_changed_tree_commit_locks_and_url_are_refused(self):
        for key in ("commit", "tree", "goModSha256", "goSumSha256", "url", "path"):
            with self.subTest(field=key), self.assertRaises(ReviewError):
                value = source(); value["core"][key] = "changed"
                local.bind_core(value, json.dumps(selection()).encode())

    def test_unknown_source_profile_and_lock_overrides_are_refused(self):
        values = [None, {}, {**source(), "root": None}, {**source(), "sourceProfile": "OTHER"}]
        value = source(); value["root"]["rootLockOverrides"] = {"go.mod": {}}
        values.append(value)
        value = source(); value["core"]["rootLockOverrides"] = {"go.sum": {}}
        values.append(value)
        for value in values:
            with self.subTest(value=value), self.assertRaises(ReviewError):
                local.bind_core(value, json.dumps(selection()).encode())

    def test_changed_module_identity_local_path_or_added_fields_are_refused(self):
        variants = []
        for key, value in (("Path", "example.org/other"), ("Version", "v0.0.1"), ("Main", 0)):
            item = selection(); item[key] = value; variants.append(item)
        for replacement in ({"Path": "../zvm-core", "Version": ""},
                            {"Path": "./zvm-core", "Version": "v1.0.0"},
                            {"Path": "./zvm-core", "Version": "", "Dir": "/fixture/source"}):
            item = selection(); item["Replace"] = replacement; variants.append(item)
        for value in variants:
            with self.subTest(value=value), self.assertRaises(ReviewError):
                local.bind_core(source(), json.dumps(value).encode())

    def test_profile_changes_extra_fields_and_boolean_schema_are_refused(self):
        variants = [None, {}, {**local.expected_profile(), "schema": True},
                    {**local.expected_profile(), "sourceCommit": "0" * 40},
                    {**local.expected_profile(), "extra": True}]
        for value in variants:
            with self.subTest(value=value), self.assertRaises(ReviewError):
                local.validate_profile(value)


class LocalScanTests(unittest.TestCase):
    def check(self, rows, **kwargs):
        return review.summarize(encode(rows), go_version="go1.26.11", **kwargs)

    def test_bound_local_occurrences_remain_in_the_full_inventory(self):
        rows = scan_rows()
        rows[1]["SBOM"]["modules"].append({"path": local.CORE_REPLACEMENT, "version": ""})
        result = self.check(rows, core_profile=local.expected_profile())
        self.assertEqual(result["localCoreModuleOccurrences"], 2)
        self.assertEqual(result["moduleRecordOccurrences"], 4)
        self.assertEqual(result["moduleRecords"], 3)
        self.assertEqual(result["localCoreBinding"], local.expected_profile())
        self.assertFalse(result["replacementForkAdvisoryCoverageEstablished"])

    def test_unbound_missing_or_versioned_local_record_is_refused(self):
        with self.assertRaises(ReviewError):
            self.check(scan_rows())
        rows = scan_rows(); rows[1]["SBOM"]["modules"].pop()
        with self.assertRaises(ReviewError):
            self.check(rows, core_profile=local.expected_profile())
        rows = scan_rows(); rows[1]["SBOM"]["modules"][-1]["version"] = "v0.0.0"
        with self.assertRaises(ReviewError):
            self.check(rows, core_profile=local.expected_profile())

    def test_other_local_absolute_and_parent_paths_are_refused_with_a_valid_binding(self):
        for name in ("./other", "../zvm-core", "/fixture/zvm-core", "C:/fixture/zvm-core"):
            with self.subTest(path=name), self.assertRaises(ReviewError):
                rows = scan_rows(); rows[1]["SBOM"]["modules"].append({"path": name})
                self.check(rows, core_profile=local.expected_profile())

    def test_local_source_call_finding_is_preserved_and_requires_review(self):
        item = finding("symbol")
        item["finding"]["trace"][0] = {"module": local.CORE_REPLACEMENT,
            "package": local.CORE_MODULE + "/parser", "function": "Parse"}
        result = self.check(scan_rows(advisory(), item), core_profile=local.expected_profile())
        self.assertEqual(result["status"], "REVIEW_REQUIRED")
        self.assertEqual(result["findingOccurrencesByLevel"]["source_call_trace_reported"], 1)
        self.assertEqual(result["findings"][0]["module"], local.CORE_REPLACEMENT)
        self.assertFalse(result["runtimeExploitabilityEstablished"])

    def test_wrong_local_finding_namespace_and_version_are_refused(self):
        for changes in ({"package": "example.org/other"}, {"version": "v0.0.0"}):
            with self.subTest(changes=changes), self.assertRaises(ReviewError):
                item = finding("symbol")
                item["finding"]["trace"][0] = {"module": local.CORE_REPLACEMENT,
                    "package": local.CORE_MODULE + "/parser", "function": "Parse", **changes}
                self.check(scan_rows(advisory(), item), core_profile=local.expected_profile())


class LocalGraphTests(unittest.TestCase):
    def check(self, rows, **kwargs):
        return graph.summarize(encoded(rows), entry=ENTRY, **kwargs)

    def test_exact_local_module_preserves_nominal_effective_and_source_identities(self):
        result = self.check(graph_rows(), core_profile=local.expected_profile())
        self.assertIn(selection(), result["selectedModules"])
        self.assertIn(local.CORE_MODULE + "/amount", result["entryImportClosure"])
        self.assertEqual(result["localCoreBinding"], local.expected_profile())
        self.assertFalse(result["waiversGenerated"])

    def test_local_module_requires_binding_and_presence_in_the_graph(self):
        with self.assertRaises(ReviewError):
            self.check(graph_rows())
        with self.assertRaises(ReviewError):
            self.check(fixture(), core_profile=local.expected_profile())

    def test_changed_nominal_version_replacement_or_namespace_is_refused(self):
        variants = []
        for key, value in (("Path", "example.org/other"), ("Version", "v0.0.1"), ("Main", True)):
            item = selection(); item[key] = value; variants.append(item)
        item = selection(); item["Replace"]["Version"] = "v1.0.0"; variants.append(item)
        for item in variants:
            with self.subTest(module=item), self.assertRaises(ReviewError):
                rows = graph_rows(); rows[-1]["Module"] = item
                self.check(rows, core_profile=local.expected_profile())

    def test_binding_does_not_authorize_other_local_replacements(self):
        rows = graph_rows(); rows[1]["Module"]["Replace"] = {"Path": "./other", "Version": ""}
        with self.assertRaises(ReviewError):
            self.check(rows, core_profile=local.expected_profile())

    def test_changed_source_profile_cannot_pass_an_otherwise_valid_graph(self):
        value = copy.deepcopy(local.expected_profile()); value["sourceTree"] = "0" * 40
        with self.assertRaises(ReviewError):
            self.check(graph_rows(), core_profile=value)


if __name__ == "__main__":
    unittest.main()

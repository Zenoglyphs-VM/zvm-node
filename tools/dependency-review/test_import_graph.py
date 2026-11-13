"""Closed-graph, test-variant and replacement-identity controls using data only."""

import copy
import json
import unittest
from unittest.mock import patch

import import_graph as graph

ENTRY = "example.org/core/cmd/tool"
NOMINAL = "example.org/upstream"
MAIN = {"Path": "example.org/core", "Version": "", "Main": True, "Replace": None}
REPLACED = {"Path": NOMINAL, "Version": "v0.0.0-00010101000000-000000000000", "Main": False,
            "Replace": {"Path": "example.org/fork", "Version": "v1.100200.3"}}


def package(name, imports=(), *, test="", selected=None, dep=True, standard=False):
    return {"ImportPath": name, "Name": "p", "Standard": standard, "ForTest": test,
            "DepOnly": dep, "Incomplete": False, "Imports": list(imports), "Module": selected}


def fixture():
    entry = package(ENTRY, [NOMINAL + "/codec"], selected=copy.deepcopy(MAIN), dep=False)
    entry["Name"] = "main"
    ordinary = package(NOMINAL + "/codec", ["unsafe"], selected=copy.deepcopy(REPLACED))
    variant = package(ENTRY + " [" + ENTRY + ".test]", [NOMINAL + "/testonly"],
                      test=ENTRY, selected=copy.deepcopy(MAIN))
    variant["Name"] = "main"
    binary = package(ENTRY + ".test", [variant["ImportPath"]], selected=copy.deepcopy(MAIN), dep=False)
    binary["Name"] = "main"
    return [entry, ordinary, package("unsafe", standard=True), variant, binary,
            package(NOMINAL + "/testonly", selected=copy.deepcopy(REPLACED))]


def encoded(rows):
    return ("\n".join(json.dumps(row) for row in rows) + "\n").encode()


class ImportGraphTests(unittest.TestCase):
    def check(self, rows):
        return graph.summarize(encoded(rows), entry=ENTRY)

    def reject(self, rows):
        with self.assertRaises((ValueError, RecursionError)):
            self.check(rows)

    def test_cli_and_test_dependencies_remain_separate(self):
        result = self.check(fixture())
        self.assertEqual(result["entryImportClosure"], [ENTRY, NOMINAL + "/codec", "unsafe"])
        self.assertIn(NOMINAL + "/testonly", result["testOnlyImportIDs"])
        self.assertNotIn(NOMINAL + "/codec", result["testOnlyImportIDs"])
        self.assertFalse(result["exercisedFunctionOrExploitReachabilityEstablished"])
        self.assertFalse(result["replacementForkAdvisoryCoverageEstablished"])
        self.assertFalse(result["releaseAccepted"])

    def test_nominal_and_effective_module_identities_preserved(self):
        result = self.check(fixture())
        self.assertIn(REPLACED, result["selectedModules"])
        self.assertIn(MAIN, result["selectedModules"])

    def test_source_record_order_does_not_change_graph(self):
        rows = fixture()
        self.assertEqual(self.check(rows), self.check(list(reversed(rows))))

    def test_partial_or_non_object_stream_cannot_pass(self):
        raw = encoded(fixture())
        for bad in (b"", raw[:-3], raw + b"null", raw + b"[]", raw + b"garbage"):
            with self.subTest(length=len(bad)), self.assertRaises(ValueError):
                graph.summarize(bad, entry=ENTRY)

    def test_duplicate_json_members_and_package_ids_refused(self):
        raw = encoded(fixture()).replace(b'"Name": "main"', b'"Name":"p","Name":"main"', 1)
        with self.assertRaises(ValueError):
            graph.summarize(raw, entry=ENTRY)
        rows = fixture(); rows.append(copy.deepcopy(rows[0])); self.reject(rows)

    def test_missing_import_record_and_cycles_refused(self):
        rows = fixture(); rows[0]["Imports"] = ["example.org/missing"]; self.reject(rows)
        rows = fixture(); rows[1]["Imports"] = [ENTRY]; self.reject(rows)

    def test_entry_must_be_ordinary_main_module_command(self):
        for field, value in (("Name", "library"), ("Module", None), ("DepOnly", True), ("Standard", True)):
            with self.subTest(field=field):
                rows = fixture(); rows[0][field] = value; self.reject(rows)
        with self.assertRaises(ValueError):
            graph.summarize(encoded(fixture()), entry="example.org/missing")

    def test_incomplete_and_mis_typed_records_refused(self):
        for field, value in (("Incomplete", True), ("DepOnly", "false"), ("Standard", 0), ("Imports", "unsafe")):
            with self.subTest(field=field):
                rows = fixture(); rows[0][field] = value; self.reject(rows)

    def test_filesystem_paths_and_unexpected_fields_refused(self):
        for name in ("/private/source", "../source", "C:/source", "example.org//source", "example.org/../source"):
            with self.subTest(name=name):
                rows = fixture(); rows[0]["ImportPath"] = name; self.reject(rows)
        rows = fixture(); rows[0]["Dir"] = "/private/source"; self.reject(rows)

    def test_replacement_needs_versioned_portable_identity(self):
        for replacement in ({"Path": "../local", "Version": ""}, {"Path": "example.org/fork", "Version": ""},
                            {"Path": "example.org/fork", "Version": "v1.2.3", "Dir": "/private/source"}):
            with self.subTest(replacement=replacement):
                rows = fixture(); rows[1]["Module"]["Replace"] = replacement; self.reject(rows)

    def test_conflicting_module_selections_refused(self):
        rows = fixture(); rows[-1]["Module"]["Replace"]["Version"] = "v1.100200.4"; self.reject(rows)

    def test_unknown_or_wrong_module_namespace_refused(self):
        rows = fixture(); rows[1]["Module"] = None; self.reject(rows)
        rows = fixture(); rows[1]["Module"]["Path"] = "example.org/unrelated"; self.reject(rows)

    def test_test_variants_must_match_their_context(self):
        rows = fixture(); rows[3]["ForTest"] = "example.org/other"; self.reject(rows)
        rows = [r for r in fixture() if not r["ForTest"] and not r["ImportPath"].endswith(".test")]
        rows = [r for r in rows if r["ImportPath"] != NOMINAL + "/testonly"]
        self.reject(rows)

    def test_edge_and_record_budgets_refused(self):
        with patch.object(graph, "MAX_PACKAGES", 2):
            self.reject(fixture())
        with patch.object(graph, "MAX_EDGES", 1):
            self.reject(fixture())
        with patch.object(graph, "MAX_BYTES", 16), self.assertRaises(ValueError):
            self.check(fixture())

    def test_repeated_import_edges_preserve_occurrences_and_closure(self):
        # The retained Go 1.26.11 test projection repeated identical import IDs.
        rows = fixture(); baseline = self.check(rows)
        rows[0]["Imports"] *= 2
        rows[3]["Imports"] *= 2
        result = self.check(rows)
        self.assertEqual(result["entryImportClosure"], baseline["entryImportClosure"])
        self.assertEqual(result["testImportClosure"], baseline["testImportClosure"])
        self.assertEqual(result["importEdges"], baseline["importEdges"])
        self.assertEqual(result["importEdgeOccurrences"], baseline["importEdgeOccurrences"] + 2)
        self.assertEqual(result["duplicateImportEdgeOccurrences"], 2)


if __name__ == "__main__":
    unittest.main()

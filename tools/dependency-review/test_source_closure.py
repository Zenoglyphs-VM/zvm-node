"""Data controls for the explicit Node/Core Git-tree profile."""

import unittest
from source_closure import CORE_COMMIT, ReviewError, tree_entries


def entry(mode="100644", kind="blob", name="go.mod", oid="a" * 40):
    return f"{mode} {kind} {oid}\t{name}\0".encode()


class SourceTreeTests(unittest.TestCase):
    def test_regular_files_and_one_pinned_core_are_accepted(self):
        raw = entry() + entry("160000", "commit", "zvm-core", CORE_COMMIT)
        self.assertEqual(len(tree_entries(raw, core_link=True)), 2)

    def test_gitlinks_are_rejected_without_explicit_profile(self):
        with self.assertRaises(ReviewError):
            tree_entries(entry("160000", "commit", "zvm-core", CORE_COMMIT))

    def test_missing_extra_and_changed_gitlinks_are_rejected(self):
        for raw in (entry(), entry("160000", "commit", "zvm-core"),
                    entry("160000", "commit", "other", CORE_COMMIT)):
            with self.subTest(raw=raw), self.assertRaises(ReviewError):
                tree_entries(raw, core_link=True)

    def test_links_modes_and_non_blob_files_are_rejected(self):
        for mode, kind in (("120000", "blob"), ("040000", "tree"), ("100644", "commit")):
            with self.subTest(mode=mode), self.assertRaises(ReviewError):
                tree_entries(entry(mode, kind))

    def test_absolute_parent_alias_and_git_paths_are_rejected(self):
        for name in ("/private/fixture", "../fixture", "a/../fixture", "a//fixture", ".git/config"):
            with self.subTest(name=name), self.assertRaises(ReviewError):
                tree_entries(entry(name=name))

    def test_duplicate_paths_and_bad_object_ids_are_rejected(self):
        for raw in (entry() + entry(), entry(oid="not-a-commit")):
            with self.subTest(raw=raw), self.assertRaises(ReviewError):
                tree_entries(raw)


if __name__ == "__main__":
    unittest.main()

"""Tests for check-docs.py: how headings become anchors and what is ignored."""
import importlib.util
import os
import unittest

spec = importlib.util.spec_from_file_location("cd", os.path.join(os.path.dirname(__file__), "check-docs.py"))
cd = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cd)


class Slugs(unittest.TestCase):
    def test_github_rules(self):
        self.assertEqual(cd.slug("Rotating the master key"), "rotating-the-master-key")
        self.assertEqual(cd.slug("Names and releasing them"), "names-and-releasing-them")
        self.assertEqual(cd.slug("Authentication and roles"), "authentication-and-roles")
        self.assertEqual(cd.slug("What `make stack` does (and why?)"), "what-make-stack-does-and-why")
        self.assertEqual(cd.slug("Becoming a maintainer"), "becoming-a-maintainer")
        self.assertEqual(cd.slug("1. Correction"), "1-correction")

    def test_code_is_ignored(self):
        text = "see [real](a.md)\n```\n[fake](nope.md)\n```\nand `[also fake](nope.md)`"
        stripped = cd.strip_code(text)
        self.assertIn("real", stripped)
        self.assertNotIn("nope.md", stripped)

    def test_links_are_found_in_both_styles(self):
        self.assertEqual(cd.LINK.findall("![img](a.png) and [t](b.md#x \"title\")"), ["a.png", "b.md#x"])
        self.assertEqual(cd.REFDEF.findall("[home]: https://example.test\n[x]: ./y.md"), ["https://example.test", "./y.md"])


if __name__ == "__main__":
    unittest.main()

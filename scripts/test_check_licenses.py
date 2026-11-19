"""Tests for check-licenses.py: the rules that decide what may ship."""
import importlib.util
import os
import unittest

spec = importlib.util.spec_from_file_location("cl", os.path.join(os.path.dirname(__file__), "check-licenses.py"))
cl = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cl)


class Verdicts(unittest.TestCase):
    def state(self, lic, name="x"):
        return cl.verdict(name, lic)[0]

    def test_permissive_licences_pass(self):
        for lic in ("MIT", "Apache-2.0", "BSD-3-Clause", "ISC", "BSD-2-Clause", "MIT-0", "BlueOak-1.0.0"):
            self.assertEqual(self.state(lic), "ok", lic)

    def test_copyleft_fails(self):
        for lic in ("GPL", "AGPL-3.0", "LGPL", "GPL-3.0-only", "LGPL-2.1", "AGPL-3.0-or-later"):
            self.assertEqual(self.state(lic), "fail", lic)

    def test_unknown_or_missing_fails_so_a_person_looks(self):
        for lic in ("UNRECOGNISED", "NO-LICENSE-FILE", "NO-LICENSE-FIELD", "SEE LICENSE IN LICENSE.txt"):
            self.assertEqual(self.state(lic), "fail", lic)

    def test_fonts_and_weak_copyleft_are_allowed_with_a_note(self):
        self.assertEqual(self.state("OFL-1.1"), "note")
        self.assertEqual(self.state("MPL-2.0"), "note")

    def test_dual_licences_pass_when_one_choice_is_permissive(self):
        self.assertEqual(self.state("(MIT OR GPL-3.0)"), "ok")
        self.assertEqual(self.state("MIT AND OFL-1.1"), "note")
        self.assertEqual(self.state("GPL-3.0 AND MIT"), "fail")

    def test_a_reviewed_exception_passes(self):
        cl.ALLOWED["example.com/reviewed"] = "checked by a person"
        try:
            self.assertEqual(cl.verdict("example.com/reviewed", "UNRECOGNISED")[0], "ok")
        finally:
            del cl.ALLOWED["example.com/reviewed"]

    def test_licence_text_is_recognised(self):
        self.assertEqual(cl.classify_text("Apache License\nVersion 2.0, January 2004"), "Apache-2.0")
        self.assertEqual(cl.classify_text("Permission is hereby granted, free of charge, to any person"), "MIT")
        self.assertEqual(cl.classify_text("GNU AFFERO GENERAL PUBLIC LICENSE Version 3"), "AGPL-3.0")
        self.assertEqual(cl.classify_text("GNU GENERAL PUBLIC LICENSE Version 2"), "GPL")
        self.assertEqual(cl.classify_text("SIL OPEN FONT LICENSE Version 1.1"), "OFL-1.1")
        self.assertEqual(cl.classify_text("Redistribution and use in source and binary forms ... Neither the name of"), "BSD-3-Clause")
        self.assertEqual(cl.classify_text("all rights reserved, no licence"), "UNRECOGNISED")


if __name__ == "__main__":
    unittest.main()

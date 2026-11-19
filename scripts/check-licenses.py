#!/usr/bin/env python3
"""Check the licences of what Plinth ships.

Go: every module that provides a package linked into the shipped programs (cmd/plinth and cmd/devidp), read
from the module cache (run `go mod download` first).
Web: every package that is not dev-only in web/package-lock.json (these end up in the binary).

Fails on a copyleft licence (GPL, AGPL, LGPL), on a licence it cannot recognise, and on a module
without a licence file, so that something unusual is reviewed by a person before it ships. Weak
copyleft (MPL, EPL) and the SIL Open Font License are reported and allowed. Anything reviewed and
accepted goes in ALLOWED below, with the reason.

    python3 scripts/check-licenses.py            # check, exit 1 on a problem
    python3 scripts/check-licenses.py --list     # also print every dependency with its licence
"""
import collections
import json
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))

# Reviewed exceptions: dependency -> why it is acceptable.
ALLOWED = {
    # "example.com/module": "reason",
}

COPYLEFT = ("GPL", "AGPL", "LGPL")
OK = {"Apache-2.0", "MIT", "MIT-0", "BSD-2-Clause", "BSD-3-Clause", "BSD", "ISC", "0BSD", "CC0-1.0", "BlueOak-1.0.0", "Unlicense", "Python-2.0"}
NOTE = {"OFL-1.1": "font licence, permits bundling with the notice (see THIRD_PARTY_NOTICES.md)",
        "MPL-2.0": "weak copyleft, file-level: fine unless modified files are distributed", "EPL-2.0": "weak copyleft"}


def classify_text(text: str) -> str:
    t = " ".join(text.split())[:8000]
    if "GNU AFFERO GENERAL PUBLIC" in t:
        return "AGPL-3.0"
    if "GNU LESSER GENERAL PUBLIC" in t:
        return "LGPL"
    if "GNU GENERAL PUBLIC LICENSE" in t:
        return "GPL"
    if "Mozilla Public License" in t:
        return "MPL-2.0"
    if "SIL OPEN FONT LICENSE" in t.upper():
        return "OFL-1.1"
    if "Apache License" in t and "Version 2.0" in t:
        return "Apache-2.0"
    if "Permission is hereby granted, free of charge" in t:
        return "MIT"
    if "Redistribution and use in source and binary forms" in t:
        return "BSD-3-Clause" if "Neither the name" in t or "endorse or promote" in t else "BSD-2-Clause"
    if "Permission to use, copy, modify, and/or distribute" in t or "Permission to use, copy, modify, and distribute" in t:
        return "ISC"
    return "UNRECOGNISED"


def go_modules():
    """Modules that provide packages linked into the programs Plinth ships (not test or tool dependencies)."""
    out = subprocess.run(
        ["go", "list", "-deps", "-f", "{{with .Module}}{{.Path}}|{{.Dir}}|{{.Main}}{{end}}", "./cmd/plinth", "./cmd/devidp"],
        cwd=ROOT, capture_output=True, text=True, check=True).stdout
    seen = set()
    for line in out.splitlines():
        if not line:
            continue
        path, d, main = line.split("|")
        if main == "true" or path in seen:
            continue
        seen.add(path)
        lic = "NO-LICENSE-FILE"
        if d and os.path.isdir(d):
            f = next((os.path.join(d, n) for n in sorted(os.listdir(d)) if re.match(r"(?i)(licen[cs]e|copying)", n)), None)
            if f:
                lic = classify_text(open(f, errors="ignore").read())
        yield path, lic


def web_packages():
    lock = json.load(open(os.path.join(ROOT, "web", "package-lock.json")))
    for key, v in lock["packages"].items():
        if not key or v.get("dev") or v.get("devOptional"):
            continue
        yield key.split("node_modules/")[-1], str(v.get("license", "NO-LICENSE-FIELD"))


def verdict(name: str, lic: str):
    if name in ALLOWED:
        return "ok", f"allowed: {ALLOWED[name]}"
    parts = [p.strip(" ()") for p in re.split(r"\s+(?:OR|AND)\s+", lic)] if re.search(r"\s(OR|AND)\s", lic) else [lic]
    if any(any(c in p for c in COPYLEFT) for p in parts) and not (" OR " in lic and any(p in OK for p in parts)):
        return "fail", "copyleft licence"
    if all(p in OK for p in parts) or (" OR " in lic and any(p in OK for p in parts)):
        return "ok", ""
    if all(p in OK or p in NOTE for p in parts):
        return "note", "; ".join(NOTE[p] for p in parts if p in NOTE)
    return "fail", "licence not recognised: review it, then add it to ALLOWED with the reason"


def main() -> int:
    listing = "--list" in sys.argv
    problems, notes, counts = [], [], collections.Counter()
    for source, items in (("go", go_modules()), ("web", web_packages())):
        for name, lic in items:
            counts[(source, lic)] += 1
            state, why = verdict(name, lic)
            if listing:
                print(f"{source:3} {lic:20} {name}")
            if state == "fail":
                problems.append(f"{source}: {name}: {lic}: {why}")
            elif state == "note":
                notes.append(f"{source}: {name}: {lic}: {why}")
    print("licences of what ships:")
    for (source, lic), n in sorted(counts.items()):
        print(f"  {source:3} {lic:22} {n}")
    for n in notes:
        print("note:", n)
    for p in problems:
        print("PROBLEM:", p, file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())

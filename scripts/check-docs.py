#!/usr/bin/env python3
"""Check the links between the Markdown files of the repository.

Every relative link must point at a file that exists, and a #fragment must match a heading in the file it points at
(using GitHub's rules for turning headings into anchors). External links are not fetched. Code blocks are ignored.

    python3 scripts/check-docs.py
"""
import os
import re
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
# GitHub resolves links in these relative to the web page they are shown on, not to the file.
SKIP_FILES = {".github/PULL_REQUEST_TEMPLATE.md"}
LINK = re.compile(r"!?\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")
REFDEF = re.compile(r"^\[[^\]]+\]:\s*(\S+)", re.M)


def strip_code(text: str) -> str:
    text = re.sub(r"^(```|~~~).*?^\1[^\n]*$", "", text, flags=re.M | re.S)
    return re.sub(r"`[^`\n]*`", "", text)


def slug(heading: str) -> str:
    h = re.sub(r"[`*_]|\[([^\]]*)\]\([^)]*\)", lambda m: m.group(1) or "", heading.strip().lower())
    h = re.sub(r"[^\w\- ]", "", h, flags=re.UNICODE)
    return h.replace(" ", "-")


def anchors(path: str) -> set:
    text = strip_code(open(path, encoding="utf-8").read())
    seen, out = {}, set()
    for m in re.finditer(r"^#{1,6}\s+(.+?)\s*#*\s*$", text, re.M):
        s = slug(m.group(1))
        n = seen.get(s, 0)
        out.add(s if n == 0 else f"{s}-{n}")
        seen[s] = n + 1
    return out


def main() -> int:
    files = subprocess.run(["git", "ls-files", "*.md"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.split()
    files += [f for f in subprocess.run(["git", "ls-files", "--others", "--exclude-standard", "*.md"], cwd=ROOT, capture_output=True, text=True).stdout.split()]
    problems, checked = [], 0
    cache = {}
    for rel in sorted(set(files)):
        if rel in SKIP_FILES or not os.path.exists(os.path.join(ROOT, rel)):
            continue
        text = strip_code(open(os.path.join(ROOT, rel), encoding="utf-8").read())
        for target in LINK.findall(text) + REFDEF.findall(text):
            if re.match(r"^[a-z][a-z0-9+.-]*:", target, re.I) or target.startswith("//"):
                continue
            checked += 1
            path, _, frag = target.partition("#")
            dest = os.path.normpath(os.path.join(ROOT, os.path.dirname(rel), path)) if path else os.path.join(ROOT, rel)
            if not os.path.exists(dest):
                problems.append(f"{rel}: {target}: no such file")
                continue
            if frag and dest.endswith(".md"):
                if dest not in cache:
                    cache[dest] = anchors(dest)
                if frag.lower() not in cache[dest]:
                    problems.append(f"{rel}: {target}: no heading with that anchor")
    for p in problems:
        print("BROKEN:", p, file=sys.stderr)
    print(f"checked {checked} links in {len(set(files))} Markdown files: {len(problems)} broken")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())

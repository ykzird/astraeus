#!/usr/bin/env python3
"""Fail if a Markdown file links to a relative path that is not there.

The release archive ships the server and the pages a user needs, not the
repository's own working documents, so a shipped page that reaches for something
outside the archive has to point at it by URL. Run over an extracted archive this
is what keeps that true; run over a checkout it catches a link left behind by a
deletion.

Usage: scripts/check-doc-links.py <directory>
"""

import os
import re
import sys

# [text](target) with an optional title; the target is what gets resolved.
LINK = re.compile(r"\[[^\]]*\]\(([^)\s]+)(?:\s+\"[^\"]*\")?\)")

# Schemes and fragments that are not files inside the tree.
EXTERNAL = ("http://", "https://", "mailto:", "#")

# Directories that hold a checkout's own machinery or scratch state rather than
# documents: walking them finds extracted copies and dependency trees, not links
# anyone wrote.
SKIP = {".git", ".tmp", "node_modules", "dist"}


def markdown_files(root):
    for base, dirs, names in os.walk(root):
        dirs[:] = sorted(d for d in dirs if d not in SKIP)
        for name in sorted(names):
            if name.endswith(".md"):
                yield os.path.join(base, name)


def dangling(root):
    for path in markdown_files(root):
        with open(path, encoding="utf-8") as handle:
            text = handle.read()
        for target in LINK.findall(text):
            if target.startswith(EXTERNAL):
                continue
            bare = target.split("#", 1)[0]
            if not bare:
                continue
            resolved = os.path.normpath(os.path.join(os.path.dirname(path), bare))
            if not os.path.exists(resolved):
                yield os.path.relpath(path, root), target


def main(argv):
    if len(argv) != 2:
        print(f"usage: {argv[0]} <directory>", file=sys.stderr)
        return 2

    root = argv[1]
    found = list(dangling(root))
    for source, target in found:
        print(f"dangling link: {source} -> {target}")
    checked = sum(1 for _ in markdown_files(root))
    print(f"{checked} Markdown file(s) checked, {len(found)} dangling link(s)")
    return 1 if found else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))

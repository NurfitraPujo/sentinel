#!/usr/bin/env python3
"""Print `KEY<TAB>/ssm/path` for every entry under externalSecrets.secrets.

STDLIB ONLY, ON PURPOSE. The first version of the CI job ran
`pip install pyyaml` and the step failed on the self-hosted runner, which took
the gate down with it -- a checker that cannot run is worse than no checker,
because it reports red for a reason unrelated to what it checks.

It is a scoped reader, not a grep: it finds the `externalSecrets:` block, then
`secrets:` inside it, then reads mapping lines only while they stay indented
deeper than `secrets:`. A plain grep for `KEY: /path` also matches comments and
identically-shaped keys in other blocks, which is the mistake this avoids.
"""

import sys


def secrets_of(path):
    with open(path, encoding="utf-8") as fh:
        lines = fh.read().splitlines()

    def indent(s):
        return len(s) - len(s.lstrip(" "))

    out = {}
    i = 0
    n = len(lines)
    while i < n:
        line = lines[i]
        if line.lstrip().startswith("#") or not line.strip():
            i += 1
            continue
        # top-level externalSecrets:
        if indent(line) == 0 and line.rstrip() == "externalSecrets:":
            i += 1
            # find `secrets:` within this block
            while i < n:
                cur = lines[i]
                if not cur.strip() or cur.lstrip().startswith("#"):
                    i += 1
                    continue
                if indent(cur) == 0:          # block ended
                    break
                if cur.strip() == "secrets:":
                    sec_indent = indent(cur)
                    i += 1
                    while i < n:
                        m = lines[i]
                        if not m.strip() or m.lstrip().startswith("#"):
                            i += 1
                            continue
                        if indent(m) <= sec_indent:   # dedent: mapping ended
                            break
                        key, sep, val = m.strip().partition(":")
                        if not sep:
                            i += 1
                            continue
                        val = val.strip().strip('"').strip("'")
                        if val:
                            out[key.strip()] = val
                        i += 1
                    break
                i += 1
            break
        i += 1
    return out


if __name__ == "__main__":
    for f in sys.argv[1:]:
        for k, v in sorted(secrets_of(f).items()):
            print(f"{k}\t{v}")

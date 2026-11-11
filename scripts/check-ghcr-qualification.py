#!/usr/bin/env python3
"""Read-only visibility guard for the explicitly approved qualification packages."""

import argparse
import json
import re
import subprocess


def check(name, allow_absent=False):
    if name not in {"stoke-qualification-tools", "stoke-qualification-cache", "stoke-qualification-results"}:
        raise ValueError("package is outside the qualification allowlist")
    response = subprocess.run(
        ["gh", "api", "--include", f"orgs/strio-dev/packages/container/{name}"],
        check=False, capture_output=True, text=True, timeout=30,
    )
    headers, separator, body = response.stdout.partition("\n\n")
    status = re.match(r"HTTP/\S+ (\d{3})\b", headers)
    if not separator or status is None:
        raise ValueError("package metadata request did not return an HTTP response")
    if status[1] == "404" and allow_absent:
        print(json.dumps({"package": name, "state": "absent-or-inaccessible"}))
        return
    if status[1] != "200" or response.returncode:
        raise ValueError(f"package metadata request failed: HTTP {status[1]}")
    package = json.loads(body)
    if (package.get("name") != name or package.get("package_type") != "container"
            or package.get("visibility") != "private"
            or (package.get("repository") or {}).get("full_name") != "strio-dev/stoke"):
        raise ValueError("existing package is not the expected private Stoke container package")
    print(json.dumps({"package": name, "id": package["id"], "visibility": "private"}))


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("name")
    parser.add_argument("--allow-absent", action="store_true",
                        help="bootstrap only after an authorized org-wide collision check")
    args = parser.parse_args()
    try:
        check(args.name, args.allow_absent)
    except (ValueError, KeyError, subprocess.TimeoutExpired) as error:
        parser.exit(1, f"{error}\n")

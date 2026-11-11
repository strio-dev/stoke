#!/usr/bin/env python3
"""Exercise the private-package gate without credentials or network requests."""

from contextlib import redirect_stdout
import importlib.util
import io
import json
from pathlib import Path
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location(
    "qualification", Path(__file__).resolve().parents[1] / "check-ghcr-qualification.py")
qualification = importlib.util.module_from_spec(spec)
spec.loader.exec_module(qualification)
NAME = "stoke-qualification-tools"
PRIVATE = {"id": 7, "name": NAME, "package_type": "container", "visibility": "private",
           "repository": {"full_name": "strio-dev/stoke"}}


def response(status, package=None):
    return SimpleNamespace(returncode=0 if status == 200 else 1,
                           stdout=f"HTTP/2.0 {status}\n\n" + json.dumps(package))


class PackageVisibilityTests(unittest.TestCase):
    def test_private_repository_and_explicit_bootstrap(self):
        for result, bootstrap, expected in [(response(200, PRIVATE), False, "private"),
                                            (response(404), True, "absent-or-inaccessible")]:
            with self.subTest(expected=expected), patch.object(
                    qualification.subprocess, "run", return_value=result), redirect_stdout(io.StringIO()) as out:
                qualification.check(NAME, bootstrap)
                self.assertIn(expected, out.getvalue())

    def test_unsafe_or_unknown_metadata_fails_closed(self):
        cases = [response(404), response(403), response(500),
                 SimpleNamespace(returncode=0, stdout="not an HTTP response")]
        for change in [{"visibility": "public"}, {"visibility": "internal"},
                       {"repository": None}, {"repository": {"full_name": "other/repo"}},
                       {"name": "another-package"}, {"package_type": "npm"}]:
            cases.append(response(200, PRIVATE | change))
        for result in cases:
            with self.subTest(response=result.stdout), patch.object(
                    qualification.subprocess, "run", return_value=result):
                with self.assertRaises(ValueError):
                    qualification.check(NAME)
        with patch.object(qualification.subprocess, "run") as request:
            with self.assertRaises(ValueError):
                qualification.check("unapproved-package", allow_absent=True)
            request.assert_not_called()


if __name__ == "__main__":
    unittest.main()

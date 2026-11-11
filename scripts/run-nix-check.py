#!/usr/bin/env python3
"""Run one real flake check from an ordinary, fresh generic host task.

Nix owns derivations, dependencies and substitution. This example observes
Nix's internal JSON activity records; it does not change Nix configuration.
Protocol evidence (upstream Nix 2.34.2): logging.hh ActivityType105/108,
logging.cc JSONLogger, derivation-building-goal.cc build fields and
substitution-goal.cc substitution fields. Unknown schemas remain unknown.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

MAX_JSON = 8 * 1024 * 1024
MAX_LOG = 64 * 1024 * 1024
MAX_LINE = 1024 * 1024


def query(nix, arguments, cwd=None):
    with tempfile.TemporaryFile() as output, tempfile.TemporaryFile() as errors:
        result = subprocess.run([nix, "--max-jobs", "1", "--cores", "2", "--option", "allow-import-from-derivation", "false", *arguments], cwd=cwd, stdout=output, stderr=errors, check=False)
        if output.tell() > MAX_JSON or errors.tell() > MAX_JSON:
            raise ValueError("Nix query exceeds the 8 MiB output bound")
        output.seek(0)
        errors.seek(0)
        stdout = output.read().decode()
        stderr = errors.read().decode(errors="replace")
        if result.returncode:
            raise ValueError("Nix query failed: " + stderr.strip())
        return stdout


def attribute(name):
    # Nix attribute selectors are quoted separately from the process argv.
    return json.dumps(name, ensure_ascii=False).replace("${", "\\${")


def local_valid(nix, output):
    try:
        return query(nix, ["path-info", "--offline", output]).strip() == output
    except (OSError, ValueError):
        return False


class Activities:
    def __init__(self, drv, outputs):
        self.drv = drv
        self.outputs = set(outputs)
        self.started = {}
        self.finished = set()
        self.remote_builds = set()
        self.malformed = 0
        self.recognized_activity_schema = False

    def observe(self, raw):
        if not raw.startswith("@nix "):
            return raw.rstrip("\n")
        try:
            event = json.loads(raw[5:])
            if not isinstance(event, dict):
                raise ValueError("not an activity object")
        except (ValueError, json.JSONDecodeError):
            self.malformed += 1
            return None
        action = event.get("action")
        fields = event.get("fields", [])
        if action == "start":
            kind = event.get("type")
            identity = event.get("id")
            if (type(kind) is not int or type(identity) is not int
                    or not isinstance(fields, list)):
                self.malformed += 1
                return None
            if kind in range(100, 113):
                self.recognized_activity_schema = True
            if kind == 105:
                if (len(fields) != 4 or not isinstance(fields[0], str)
                        or not isinstance(fields[1], str)
                        or any(type(value) is not int or value != 1 for value in fields[2:])):
                    self.malformed += 1
                    return None
            elif kind == 108:
                if len(fields) != 2 or not all(isinstance(value, str) for value in fields):
                    self.malformed += 1
                    return None
            if not fields:
                return None
            target = fields[0]
            if ((kind == 105 and target == self.drv)
                    or (kind == 108 and isinstance(target, str) and target in self.outputs)):
                if identity in self.started:
                    self.malformed += 1
                    return None
                if len(self.started) >= 8192:
                    raise ValueError("Nix check activity count exceeds the 8192-record bound")
                self.started[identity] = (kind, target)
                if kind == 105 and fields[1]:
                    self.remote_builds.add(identity)
        elif action == "stop":
            identity = event.get("id")
            if type(identity) is not int:
                self.malformed += 1
            elif identity in self.started:
                self.finished.add(identity)
        if action == "msg" and isinstance(event.get("msg"), str):
            return event["msg"]
        if (action == "result" and event.get("type") == 101
                and isinstance(fields, list) and fields and isinstance(fields[0], str)):
            return fields[0]
        return None

    def classify(self, returncode, valid_before):
        builds = [identity for identity, (kind, _) in self.started.items()
                  if kind == 105 and identity not in self.remote_builds]
        substitutions = {target for identity, (kind, target) in self.started.items()
                         if kind == 108 and identity in self.finished}
        if returncode:
            realization = "failed"
        elif self.malformed:
            realization = "unknown"
        elif builds and all(identity in self.finished for identity in builds):
            realization = "built"
        elif builds or self.remote_builds:
            realization = "unknown"
        elif substitutions and self.outputs and all(
                valid_before.get(output, False) or output in substitutions for output in self.outputs):
            realization = "substituted"
        elif (self.recognized_activity_schema and self.outputs
              and all(valid_before.get(output, False) for output in self.outputs)):
            realization = "already-valid"
        else:
            realization = "unknown"
        fresh = True if realization == "built" else (
            False if realization in {"substituted", "already-valid"} else None)
        return {"realization": realization, "freshSuccessfulCheckBuilder": fresh,
                "targetBuilderActivityObserved": bool(builds),
                "targetRemoteHookActivityObserved": bool(self.remote_builds),
                "targetBuildActivitiesCompleted": len([i for i in builds if i in self.finished]),
                "targetOutputsSubstituted": sorted(substitutions),
                "malformedInternalJSONRecords": self.malformed}


def run_build(nix, selector, reports, activities):
    command = [nix, "build", "--max-jobs", "1", "--cores", "2", "--no-link",
               "--no-update-lock-file", "--option", "allow-import-from-derivation", "false", "--json", "--log-format", "internal-json", selector]
    with (reports / "nix.stdout.json").open("xb") as output, (reports / "nix.stderr.log").open("xb") as error_log:
        child = subprocess.Popen(command, stdout=output, stderr=subprocess.PIPE)
        total = 0
        try:
            while True:
                raw = child.stderr.readline(MAX_LINE + 1)
                if not raw:
                    break
                total += len(raw)
                if len(raw) > MAX_LINE or total > MAX_LOG:
                    raise ValueError("Nix check exceeds the 1 MiB line/64 MiB stderr bound")
                error_log.write(raw)
                display = activities.observe(raw.decode(errors="replace"))
                if display:
                    print(display, file=sys.stderr, flush=True)
            returncode = child.wait()
        finally:
            child.stderr.close()
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
        if output.tell() > MAX_JSON:
            raise ValueError("Nix result JSON exceeds the 8 MiB bound")
    return command, returncode


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--nix", default="nix", help="already-provisioned Nix executable")
    parser.add_argument("--flake", default="path:source")
    parser.add_argument("--system", default=os.environ.get("STOKE_MATRIX_SYSTEM"))
    parser.add_argument("--check", default=os.environ.get("STOKE_MATRIX_CHECK"))
    parser.add_argument("--reports", type=Path, default=Path("reports"))
    args = parser.parse_args(argv)
    try:
        for value, label in [(args.system, "system"), (args.check, "check")]:
            if not isinstance(value, str) or not value or len(value) > 512 or re.search(r"[\x00-\x1f\x7f]", value):
                raise ValueError(f"provide {label} through its flag or STOKE_MATRIX environment")
        if not args.flake.startswith("path:") or "#" in args.flake:
            raise ValueError("this example requires an explicit local path: flake reference without a fragment")
        flake_source = Path(args.flake[len("path:"):]).resolve(strict=True)
        if not flake_source.is_dir():
            raise ValueError("flake source must be a directory")
        reports = args.reports.absolute()
        temporary = Path(os.environ.get("TMPDIR", tempfile.gettempdir())).resolve()
        for location, label in [(reports, "reports"), (temporary, "TMPDIR")]:
            if location == flake_source or flake_source in location.parents:
                raise ValueError(f"{label} must remain outside the flake source tree")
        args.reports.mkdir(mode=0o700, exist_ok=False)
        selector = args.flake + "#checks." + attribute(args.system) + "." + attribute(args.check)
        metadata = json.loads(query(args.nix, ["eval", "--json", "--no-update-lock-file", selector,
            "--apply", "check: { drvPath = check.drvPath; outputs = builtins.listToAttrs (map (name: { inherit name; value = check.${name}.outPath; }) check.outputs); }"]))
        drv = metadata.get("drvPath")
        outputs = metadata.get("outputs")
        if not isinstance(drv, str) or not drv.startswith("/nix/store/") or not drv.endswith(".drv"):
            raise ValueError("flake check did not report a canonical derivation path")
        if not isinstance(outputs, dict) or not outputs or not all(
                isinstance(output, str) and output.startswith("/nix/store/") for output in outputs.values()):
            raise ValueError("this example requires statically named flake-check outputs")
        before = {output: local_valid(args.nix, output) for output in outputs.values()}
        activities = Activities(drv, outputs.values())
        command, returncode = run_build(args.nix, selector, args.reports, activities)
        provenance = activities.classify(returncode, before)
        result = {"version": 1, "system": args.system, "check": args.check,
                  "status": "passed" if returncode == 0 else "failed",
                  "hostCommand": "executed", "nixExitCode": returncode,
                  "drvPath": drv, "outputs": outputs, "outputsValidBeforeRun": before,
                  "command": command, "provenance": provenance,
                  "nixResultJSONSHA256": hashlib.sha256((args.reports / "nix.stdout.json").read_bytes()).hexdigest(),
                  "provenanceSchema": "Nix 2.34.2 internal-json Activity105/108; exact local builder or substitution; remote-only/unknown stays unknown"}
        (args.reports / "nix.json").write_text(json.dumps(result, indent=2, sort_keys=True) + "\n")
        fresh = provenance["freshSuccessfulCheckBuilder"]
        print(f"Nix check {args.check} ({args.system}): {result['status']}; "
              f"realization={provenance['realization']}; fresh-check-builder="
              + ("unknown" if fresh is None else str(fresh).lower()), flush=True)
        return 0 if returncode == 0 else 1
    except (OSError, ValueError, json.JSONDecodeError) as error:
        print("run-nix-check:", error, file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

#!/usr/bin/env python3
"""Collect and evaluate fixed end-to-end cache workloads on a rootless host."""

import argparse
import datetime
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shutil
import statistics
import subprocess
import sys
import uuid


SCENARIOS = {"cold", "local-warm", "registry-warm", "registry-rw", "changed-source"}
WARM = {"local-warm", "registry-warm", "registry-rw"}
SAMPLES = 5


def require(condition, message):
    if not condition:
        raise ValueError(message)


def digest(value):
    return isinstance(value, str) and re.fullmatch(r"(?:sha256:)?[a-f0-9]{64}", value)


def boolean_proofs(value, name):
    require(isinstance(value, dict) and value, f"{name}: missing correctness proofs")
    require(all(v is True for v in value.values()), f"{name}: a correctness proof failed")


def validate_report(report):
    require(report.get("schema") == 1, "unsupported report schema")
    require(report.get("samples") == SAMPLES, "evaluation requires five samples")
    require(digest(report.get("fixtureHash")), "invalid fixture hash")
    require(digest(report.get("seedHash")), "invalid frozen seed hash")
    require(digest(report.get("engineSHA")), "invalid engine hash")
    require(digest(report.get("stokeSHA")), "invalid Stoke hash")
    require(isinstance(report.get("resources"), dict) and report["resources"], "missing resource contract")
    boolean_proofs(report.get("proofs"), "report")
    groups = {}
    iterations = {}
    for case in report.get("cases", []):
        key = (case.get("workload"), case.get("scenario"))
        require(isinstance(key[0], str) and key[0], "missing workload")
        require(key[1] in SCENARIOS, f"unknown scenario {key[1]}")
        require(case.get("status") == "passed", f"{key}: unsuccessful run")
        boolean_proofs(case.get("proofs"), str(key))
        seconds = case.get("wallSeconds")
        require(type(seconds) in (int, float) and math.isfinite(seconds) and seconds > 0, f"{key}: invalid wall time")
        transferred = case.get("registryBlobGetBytes")
        require(type(transferred) is int and transferred >= 0, f"{key}: invalid transferred bytes")
        requests = case.get("registryRequests")
        require(isinstance(requests, list), f"{key}: missing registry observations")
        counted = 0
        for request in requests:
            for field in ("readBytes", "writtenBytes"):
                require(type(request.get(field)) is int and request[field] >= 0, f"{key}: invalid HTTP byte count")
            if request.get("method") == "GET" and "/blobs/" in request.get("path", "") and "/uploads/" not in request["path"] and 200 <= request.get("status", 0) < 300:
                counted += request["writtenBytes"]
        require(counted == transferred, f"{key}: blob byte counter differs from actual response observations")
        iteration = case.get("iteration")
        require(type(iteration) is int and iteration >= 0, f"{key}: invalid iteration")
        require(iteration not in iterations.setdefault(key, set()), f"{key}: duplicate sample")
        iterations[key].add(iteration)
        groups.setdefault(key, []).append(case)
    workloads = {key[0] for key in groups}
    require(len(workloads) == 2, "evaluation requires both fixed workload families")
    require(set(groups) == {(w, s) for w in workloads for s in SCENARIOS}, "missing fixed workload scenario")
    require(all(len(cases) == SAMPLES for cases in groups.values()), "a workload scenario lacks five samples")
    return groups


def evaluate(baseline, candidate):
    old, new = validate_report(baseline), validate_report(candidate)
    require(baseline.get("label") == "baseline" and candidate.get("label") == "candidate", "report labels must be baseline and candidate")
    for field in ("fixtureHash", "seedHash", "resources", "stokeSHA"):
        require(baseline[field] == candidate[field], f"frozen {field} changed")
    require(baseline["engineSHA"] != candidate["engineSHA"], "candidate must identify the rebuilt engine")
    require(set(old) == set(new), "workload identities changed")
    require(set(baseline["proofs"]) == set(candidate["proofs"]), "correctness proof set changed")
    results, gains, regressions = [], [], []
    for key in sorted(old):
        before, after = old[key], new[key]
        before_by_iteration = {c["iteration"]: c for c in before}
        after_by_iteration = {c["iteration"]: c for c in after}
        require(set(before_by_iteration) == set(after_by_iteration), f"{key}: sample identities changed")
        for iteration, case in before_by_iteration.items():
            require(set(case["proofs"]) == set(after_by_iteration[iteration]["proofs"]), f"{key}: per-run proofs changed")
        btime = statistics.median(c["wallSeconds"] for c in before)
        ctime = statistics.median(c["wallSeconds"] for c in after)
        bbytes = statistics.median(c["registryBlobGetBytes"] for c in before)
        cbytes = statistics.median(c["registryBlobGetBytes"] for c in after)
        row = {
            "workload": key[0], "scenario": key[1], "samples": SAMPLES,
            "baselineWallSeconds": btime, "candidateWallSeconds": ctime,
            "wallChangePercent": 100 * (ctime / btime - 1),
            "baselineBlobGetBytes": bbytes, "candidateBlobGetBytes": cbytes,
            "blobByteReductionPercent": 100 * (1 - cbytes / bbytes) if bbytes else None,
            "baselineWallSamples": [c["wallSeconds"] for c in before],
            "candidateWallSamples": [c["wallSeconds"] for c in after],
        }
        results.append(row)
        if ctime > btime * 1.10:
            regressions.append({"workload": key[0], "scenario": key[1], "percent": row["wallChangePercent"]})
        if key[1] in WARM and ctime <= btime * 0.85:
            gains.append({"kind": "warm-wall-time", "workload": key[0], "scenario": key[1], "percent": -row["wallChangePercent"]})
        if key[1] in {"registry-warm", "registry-rw"} and bbytes > 0 and cbytes <= bbytes * 0.75:
            gains.append({"kind": "registry-blob-bytes", "workload": key[0], "scenario": key[1], "percent": row["blobByteReductionPercent"]})
    return {
        "status": "pass" if gains and not regressions else "fail", "schema": 1,
        "fixtureHash": baseline["fixtureHash"], "seedHash": baseline["seedHash"],
        "gains": gains, "regressions": regressions, "comparisons": results,
        "limits": ["Five-sample medians; no p95 claim", "Packaging and cache I/O fixtures; no compilation or general CI speed claim", "A registry-byte gain alone does not establish an elapsed-time gain"],
    }


def collect(args):
    repo = Path(__file__).resolve().parent.parent
    root = Path(args.root).resolve()
    require(root.name.startswith("stoke-cache-performance-"), "use a dedicated stoke-cache-performance-* work directory")
    root.mkdir(parents=True, exist_ok=True)
    output = Path(args.output).resolve()
    require(not output.exists(), "report already exists; keep earlier collection evidence")
    go = shutil.which("go")
    require(go, "run collection in the pinned Stoke Nix development shell")
    version = subprocess.check_output([go, "version"], text=True).strip()
    require("go1.27.3 " in version, f"frozen Go version changed: {version}")
    selected = {}
    for name, setting in (("STOKE_BINARY", args.stoke), ("STOKE_COOPR_BINARY", args.engine)):
        path = Path(setting).resolve()
        require(path.is_file() and os.access(path, os.X_OK), f"missing executable: {path}")
        selected[name] = str(path)
    require(os.environ.get("STOKE_SMOKE_BUSYBOX"), "the pinned shell must supply STOKE_SMOKE_BUSYBOX")
    selected.update({
        "STOKE_CACHE_PERF_ROOT": str(root), "STOKE_CACHE_PERF_REPORT": str(output),
        "STOKE_CACHE_PERF_LABEL": args.label, "STOKE_CACHE_PERF_SAMPLES": str(args.samples),
        "STOKE_CACHE_PERF_SOURCE_STOKE": str(root / "source/stoke"),
        "STOKE_CACHE_PERF_SOURCE_COOPR": str(root / "source/coopr"),
        "GOMAXPROCS": "4", "CGO_ENABLED": "0", "GOTOOLCHAIN": "local",
        "STOKE_SMOKE_BUSYBOX": str(Path(os.environ["STOKE_SMOKE_BUSYBOX"]).resolve()),
        "PATH": os.environ["PATH"],
    })
    command = [
        "systemd-run", "--user", "--quiet", "--wait", "--pipe", "--collect",
        "--unit=stoke-cacheperf-" + args.label + "-" + uuid.uuid4().hex[:8],
        "-p", "MemoryMax=8G", "-p", "MemorySwapMax=0", "-p", "TasksMax=256",
        "-p", "RuntimeMaxSec=2700", "-p", "CPUQuota=400%", "-p", "OOMPolicy=kill",
        "--working-directory=" + str(repo), "/usr/bin/env",
    ] + [f"{key}={value}" for key, value in selected.items()] + [
        go, "test", "-p=2", "./internal/artifact", "-run", "^TestCachePerformanceCollector$",
        "-v", "-count=1", "-timeout=45m",
    ]
    output.parent.mkdir(parents=True, exist_ok=True)
    log = output.with_suffix(".log")
    started = datetime.datetime.now(datetime.timezone.utc).isoformat()
    with log.open("xb") as stream:
        result = subprocess.run(command, stdout=stream, stderr=subprocess.STDOUT)
    record = {"argv": command, "goVersion": version, "startedAt": started,
              "completedAt": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "exitCode": result.returncode, "log": str(log),
              "selectedBinaryHashes": {name: hashlib.sha256(Path(value).read_bytes()).hexdigest() for name, value in selected.items() if name in {"STOKE_BINARY", "STOKE_COOPR_BINARY"}}}
    output.with_suffix(".command.json").write_text(json.dumps(record, indent=2) + "\n")
    require(result.returncode == 0, f"collector failed; see {log}")
    require(output.is_file(), "collector exited without a report")
    print(output)
    return 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    collector = sub.add_parser("collect")
    for name in ("engine", "stoke", "root", "output"):
        collector.add_argument("--" + name, required=True)
    collector.add_argument("--label", choices=["baseline", "candidate", "preflight"], required=True)
    collector.add_argument("--samples", type=int, choices=[1, SAMPLES], default=SAMPLES)
    comparator = sub.add_parser("evaluate")
    comparator.add_argument("--baseline", required=True)
    comparator.add_argument("--candidate", required=True)
    comparator.add_argument("--report")
    args = parser.parse_args()
    try:
        if args.command == "collect":
            return collect(args)
        result = evaluate(json.loads(Path(args.baseline).read_text()), json.loads(Path(args.candidate).read_text()))
        if args.report:
            destination = Path(args.report).resolve()
            require(destination not in {Path(args.baseline).resolve(), Path(args.candidate).resolve()}, "evaluation output would replace an input report")
            destination.write_text(json.dumps(result, indent=2) + "\n")
        print(json.dumps(result, indent=2))
        return 0 if result["status"] == "pass" else 1
    except (ValueError, OSError, KeyError, TypeError) as error:
        print(f"cache performance evaluation failed: {error}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())

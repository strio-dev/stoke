#!/usr/bin/env python3
"""Exercise native Coopr YAML tasks with local rootless inputs and transfers.

Run inside the Nix development shell with a selected API-capable Coopr. Uses
only scratch, static BusyBox and private signature policy; retains evidence and
caller-owned native stores under the selected temporary directory. Never publishes to an external registry.
"""

import hashlib
import json
import os
from pathlib import Path
import selectors
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time


TIMEOUT = 180


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def descendants(pid):
    """Go can fork native children from any OS thread, not only its main one."""
    pending = [pid]
    found = set()
    while pending:
        current = pending.pop()
        try:
            tasks = list((Path("/proc") / str(current) / "task").iterdir())
        except FileNotFoundError:
            continue
        for task in tasks:
            try:
                children = map(int, (task / "children").read_text().split())
                for child in children:
                    if child not in found:
                        found.add(child)
                        pending.append(child)
            except FileNotFoundError:
                pass
    return found


def live(pid):
    try:
        # Reaped work and zombies cannot keep executing workloads.
        return (Path("/proc") / str(pid) / "stat").read_text().rsplit(")", 1)[1].split()[0] != "Z"
    except FileNotFoundError:
        return False


def main():
    binary = Path(os.environ.get("STOKE_BINARY", "result-stoke/bin/stoke")).resolve()
    busybox_setting = os.environ.get("STOKE_SMOKE_BUSYBOX")
    require(binary.is_file(), f"Stoke binary does not exist: {binary}")
    require(busybox_setting, "STOKE_SMOKE_BUSYBOX must name a static BusyBox binary")
    busybox = Path(busybox_setting).resolve()
    require(busybox.is_file(), f"BusyBox does not exist: {busybox}")
    root = Path(tempfile.mkdtemp(prefix="stoke-native-"))
    print(f"Retained native workflow evidence: {root}", file=sys.stderr, flush=True)
    context = root / "context"
    context.mkdir()
    shutil.copy2(busybox, context / "busybox")
    (context / "payload.txt").write_text("native component payload\n")
    (context / "derived.txt").write_text("native image argument\n")
    (context / "literal.txt").write_text("local literal payload\n")
    (context / "component.coopr").write_text(
        'package as="pkg" {\n    copy "payload.txt" "/payload"\n}\n'
        'extend as="installed" {\n    copy "/payload" "/installed" from="pkg"\n}\n'
    )
    native_command = "printf '%s\\n' \"$NATIVE_MODE\" > /mode.txt; echo STOKE_NATIVE_RUN_EXECUTED"
    (context / "image.coopr").write_text(
        'arg "COMPONENT"\narg "MODE" "standard"\n'
        'from "scratch" as="image" {\n    arg "COMPONENT"\n    component "${COMPONENT}"\n'
        '    copy "busybox" "/bin/busybox"\n    arg "MODE"\n'
        '    env NATIVE_MODE="${MODE}"\n    run {\n'
        '        exec "/bin/busybox" "sh" "-eu" "-c" ' + json.dumps(native_command) + '\n'
        '    }\n}\n'
    )
    (context / "derived.coopr").write_text(
        'arg "BASE"\nfrom "${BASE}" as="derived" {\n'
        '    copy "derived.txt" "/derived.txt"\n}\n'
    )
    cancel_command = "echo STOKE_NATIVE_CANCEL_READY; /bin/busybox sleep 120"
    (context / "cancel.coopr").write_text(
        'from "scratch" {\n    copy "busybox" "/bin/busybox"\n    run {\n'
        '        exec "/bin/busybox" "sh" "-eu" "-c" ' + json.dumps(cancel_command) + '\n'
        '    }\n}\n'
    )
    policy = root / "policy.json"
    policy.write_text(json.dumps({"default": [{"type": "reject"}], "transports": {
        "oci": {"": [{"type": "insecureAcceptAnything"}]},
        "oci-archive": {"": [{"type": "insecureAcceptAnything"}]},
        "containers-storage": {"": [{"type": "insecureAcceptAnything"}]},
    }}))
    env = os.environ.copy()
    env.update(XDG_STATE_HOME=str(root / "state"), XDG_CACHE_HOME=str(root / "cache"))
    # Publication destinations vary between lanes. Keep this harness definition
    # outside the native input tree so a warm lane has identical build inputs.
    workflow = root / "stoke.yaml"
    invocation = [str(binary), "run", "--network", "none", "--context", str(context),
                  "--signature-policy", str(policy), "-f", str(workflow)]
    reports = {}
    checks = []

    def context_identity():
        return {str(path.relative_to(context)): {
            "sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "mode": path.stat().st_mode, "mtimeNs": path.stat().st_mtime_ns,
        } for path in sorted(context.rglob("*")) if path.is_file()}

    original_context = context_identity()

    def write(tasks):
        # JSON is a YAML subset and needs no extra fixture dependency.
        workflow.write_text(json.dumps({"version": 1, "tasks": tasks}, indent=2))

    def definition(label, fail=False):
        image_destination = root / (label + ".published-image")
        component_destination = root / (label + ".published-component.tar")
        test_command = (
            "test \"$(/bin/busybox cat /installed)\" = 'native component payload'; "
            "test \"$(/bin/busybox cat /mode.txt)\" = standard; echo STOKE_NATIVE_TEST_EXECUTED"
        )
        if fail:
            test_command = "echo STOKE_NATIVE_EXPECTED_FAILURE; exit 17"
        write({
            "component": {"coopr": {"kind": "component", "files": ["component.coopr"], "target": "installed"}},
            "build": {"coopr": {"files": ["image.coopr"], "target": "image", "args": {
                "COMPONENT": {"from": "component"}, "MODE": "standard",
            }}},
            "literal": {"image": "scratch", "steps": [
                {"copy": {"sources": ["busybox"], "destination": "/bin/busybox"}},
                {"copy": {"sources": ["literal.txt"], "destination": "/literal.txt"}},
                {"run": ["/bin/busybox", "sh", "-eu", "-c",
                         "/bin/busybox cp /literal.txt /literal-result.txt; echo STOKE_LITERAL_RUN_EXECUTED"]},
            ]},
            "derived": {"coopr": {"files": ["derived.coopr"], "args": {"BASE": {"from": "build"}}},
                        "outputs": {"filesystem": False}},
            "test": {"from": "build", "cache": False, "steps": [
                {"run": ["/bin/busybox", "sh", "-eu", "-c", test_command]},
            ]},
            "verify": {"from": "derived", "cache": False, "steps": [
                {"run": ["/bin/busybox", "sh", "-eu", "-c",
                         "test \"$(/bin/busybox cat /derived.txt)\" = 'native image argument'"]},
            ]},
            "artifact": {"image": "scratch", "needs": ["test"], "steps": [
                {"copy": {"from": "build", "sources": ["/installed"], "destination": "/artifact.txt"}},
                {"copy": {"from": "literal", "sources": ["/literal-result.txt"], "destination": "/literal-result.txt"}},
            ]},
            "publish-image": {"publish": {"from": "derived", "destinations": ["oci:" + str(image_destination)]},
                              "needs": ["test", "verify", "artifact"]},
            "publish-component": {"publish": {"from": "component", "destinations": ["oci-archive:" + str(component_destination)]},
                                  "needs": ["test"]},
        })
        return image_destination, component_destination

    def save(label, completed, success):
        (root / (label + ".stdout")).write_text(completed.stdout)
        (root / (label + ".stderr")).write_text(completed.stderr)
        require((completed.returncode == 0) == success,
                f"{label}: exit={completed.returncode}\n{completed.stderr}")
        report = json.loads(completed.stdout)
        require(report["status"] == ("passed" if success else "failed"), f"{label}: report status mismatch")
        require(json.loads((Path(report["directory"]) / "run.json").read_text()) == report,
                f"{label}: persisted report differs")
        reports[label] = report
        return report

    def execute(label, extra=(), success=True):
        destinations = definition(label, fail=not success)
        require(context_identity() == original_context,
                f"{label}: harness changed the native build context")
        completed = subprocess.run(invocation + list(extra) + ["publish-image", "publish-component"],
                                   env=env, capture_output=True, text=True, timeout=TIMEOUT)
        require(context_identity() == original_context,
                f"{label}: native build changed its caller context")
        report = save(label, completed, success)
        jobs = {job["name"]: job for job in report["tasks"]}
        if not success:
            require(all(jobs[name]["status"] == "skipped" for name in ["publish-image", "publish-component"]), "failed test scheduled publication")
            require(not any(path.exists() for path in destinations), "failed test published an artifact")
            require(jobs["artifact"]["status"] == "skipped" and not (Path(report["output"]) / "artifact").exists(), "failed test produced a workflow artifact")
            return report, completed
        require((Path(jobs["build"]["filesystem"]) / "installed").read_text() == "native component payload\n",
                f"{label}: native component was not consumed")
        require((Path(jobs["build"]["filesystem"]) / "mode.txt").read_text() == "standard\n",
                f"{label}: native argument changed")
        require((Path(jobs["artifact"]["filesystem"]) / "artifact.txt").read_text() == "native component payload\n",
                f"{label}: literal COPY did not consume native image")
        for name in ("literal", "artifact"):
            require((Path(jobs[name]["filesystem"]) / "literal-result.txt").read_text() == "local literal payload\n", f"{label}: literal output bytes changed")
        require(jobs["derived"].get("filesystem", "") == "", f"{label}: disabled rootfs was reported")
        require(not (Path(report["output"]) / "derived/rootfs").exists(), f"{label}: disabled rootfs was exported")
        require(Path(jobs["component"]["oci"]).is_file(), f"{label}: component export is not an archive")
        with tarfile.open(jobs["component"]["oci"]) as archive:
            component_index = json.load(archive.extractfile("index.json"))
            require(len(component_index["manifests"]) == 1 and component_index["manifests"][0]["digest"] == jobs["component"]["digest"],
                    f"{label}: component digest does not identify exported archive")
        for name in ("component", "build", "derived", "publish-image", "publish-component"):
            require("cache" not in jobs[name], f"{label}: {name} fabricated unavailable cache counters")
        for name in ("literal", "test", "verify", "artifact"):
            require(isinstance(jobs[name].get("cache", {}).get("Hits"), int), f"{label}: {name} lost available cache counters")
        require("STOKE_NATIVE_TEST_EXECUTED" in completed.stderr.splitlines(), f"{label}: test was reused")
        image_index = json.loads((destinations[0] / "index.json").read_text())
        original_index = json.loads((Path(jobs["derived"]["oci"]) / "index.json").read_text())
        require(image_index["manifests"] == original_index["manifests"], f"{label}: publication changed image identity")
        for item in image_index["manifests"]:
            manifest = json.loads((destinations[0] / "blobs" / item["digest"].replace(":", "/")).read_text())
            require(manifest["config"]["digest"] == "sha256:" + jobs["derived"]["imageID"],
                    f"{label}: published image config differs from tested image")
        with tarfile.open(destinations[1]) as archive:
            require(json.load(archive.extractfile("index.json"))["manifests"] == component_index["manifests"],
                    f"{label}: publication changed component identity")
        for name in ("publish-image", "publish-component"):
            delivered = jobs[name].get("destinations", [])
            require(len(delivered) == 1 and delivered[0]["status"] == "passed",
                    f"{label}: destination outcome not reported: {delivered}")
        return report, completed

    cold_report, cold = execute("cold")
    require("STOKE_NATIVE_RUN_EXECUTED" in cold.stderr.splitlines(), "cold native RUN did not execute")
    require("STOKE_LITERAL_RUN_EXECUTED" in cold.stderr.splitlines(), "cold literal RUN did not execute")
    checks.extend(["native-component-image-bindings", "literal-native-image-consumers", "optional-rootfs", "gated-local-publication"])
    warm_report, warm = execute("warm")
    require("STOKE_NATIVE_RUN_EXECUTED" not in warm.stderr.splitlines(),
            "warm native RUN executed instead of reusing its controlled fixture result")
    require(cold_report["directory"] != warm_report["directory"], "warm run did not use fresh native stores")
    literal_cold = next(job for job in cold_report["tasks"] if job["name"] == "literal")
    literal_warm = next(job for job in warm_report["tasks"] if job["name"] == "literal")
    require("STOKE_LITERAL_RUN_EXECUTED" not in warm.stderr.splitlines() and literal_warm["cache"]["Hits"] > 0, "warm literal RUN did not reuse the local cache")
    require(literal_cold["digest"] == literal_warm["digest"], "warm literal output identity changed")
    (root / "context-identity.json").write_text(json.dumps({
        "workflowOutsideContext": not workflow.is_relative_to(context),
        "coldAndWarmInputsIdentical": True, "files": original_context,
    }, indent=2) + "\n")
    identities = {}
    for label, report in (("cold", cold_report), ("warm", warm_report)):
        job = next(job for job in report["tasks"] if job["name"] == "build")
        layout = Path(job["oci"])
        index = json.loads((layout / "index.json").read_text())
        require(len(index["manifests"]) == 1, f"{label}: expected one native image manifest")
        descriptor = index["manifests"][0]
        require(descriptor["digest"] == job["digest"], f"{label}: report digest differs from exported native image")
        manifest = json.loads((layout / "blobs" / descriptor["digest"].replace(":", "/")).read_text())
        identities[label] = {"runDirectory": report["directory"], "manifestDigest": job["digest"],
                             "configDigest": manifest["config"]["digest"], "layers": manifest["layers"]}
    require(identities["cold"]["layers"] == identities["warm"]["layers"],
            "warm native RUN did not preserve its output layers")
    # Fresh native builds may refresh empty history timestamps even when layers
    # are reused. Do not mistake that metadata change for workload execution or
    # promise a stable whole-image digest across native frontend rebuilds.
    (root / "warm-cache.json").write_text(json.dumps({"scope": "controlled native fixture with fresh run stores and a shared cache",
        "nativeRunExecuted": {"cold": True, "warm": False}, "identities": identities,
        "identicalLayers": True, "identicalManifestDigest": identities["cold"]["manifestDigest"] == identities["warm"]["manifestDigest"]}, indent=2) + "\n")
    checks.append("native-warm-cache")
    checks.append("cache-statistics-availability")
    uncached_report, uncached = execute("no-cache", ["--no-cache"])
    require("STOKE_NATIVE_RUN_EXECUTED" in uncached.stderr.splitlines(), "--no-cache reused native RUN")
    require("STOKE_LITERAL_RUN_EXECUTED" in uncached.stderr.splitlines() and all(job.get("cache", {}).get("Hits", 0) == 0 for job in uncached_report["tasks"]), "--no-cache reused literal results")
    checks.append("native-no-cache")
    execute("failed-test", success=False)
    checks.append("failed-test-blocks-publication")

    # Existing local destinations belong to the caller. A publication failure
    # must preserve them rather than replace an archive or merge an image tree.
    for kind in ("image", "component"):
        label = "preserve-existing-" + kind
        destinations = definition(label)
        destination = destinations[0 if kind == "image" else 1]
        marker = b"caller-owned publication destination\x00must remain unchanged\n"
        if kind == "image":
            destination.mkdir()
            protected = destination / "keep.txt"
        else:
            protected = destination
        protected.write_bytes(marker)
        original_destination = destination.stat()
        original_protected = protected.stat()
        target = "publish-" + kind
        completed = subprocess.run(invocation + [target], env=env, capture_output=True,
                                   text=True, timeout=TIMEOUT)
        report = save(label, completed, False)
        publication = next((job for job in report["tasks"] if job["name"] == target), None)
        require(publication is not None and publication["status"] == "failed",
                f"{label}: existing destination was not rejected by publication")
        require(all(job["status"] == "passed" for job in report["tasks"] if job["name"] != target),
                f"{label}: fixture failed before publication")
        require(not any(item["status"] == "passed" for item in publication.get("destinations", [])),
                f"{label}: rejected destination was reported as successful")
        require(protected.read_bytes() == marker, f"{label}: caller-owned bytes changed")
        require((destination.stat().st_dev, destination.stat().st_ino) ==
                (original_destination.st_dev, original_destination.st_ino),
                f"{label}: caller-owned destination was replaced")
        require((protected.stat().st_dev, protected.stat().st_ino) ==
                (original_protected.st_dev, original_protected.st_ino),
                f"{label}: caller-owned marker was replaced")
        if kind == "image":
            require([path.name for path in destination.iterdir()] == ["keep.txt"],
                    f"{label}: publication merged new entries into the caller-owned directory")
        (root / (label + ".preservation.json")).write_text(json.dumps({
            "destination": str(destination), "publicationStatus": publication["status"],
            "bytesPreserved": True, "destinationInodePreserved": True,
            "markerInodePreserved": True, "destinations": publication.get("destinations", []),
        }, indent=2) + "\n")
        checks.append("local-publication-preserves-existing-" + kind)

    write({
        "component": {"coopr": {"kind": "component", "files": ["component.coopr"]}},
        "matrix": {"coopr": {"files": ["image.coopr"], "args": {
            "COMPONENT": {"from": "component"}, "MODE": {"matrix": "mode"},
        }}, "strategy": {"matrix": {"mode": ["debug", "release"]}, "max-parallel": 2}},
    })
    completed = subprocess.run(invocation + ["matrix"], env=env, capture_output=True, text=True, timeout=TIMEOUT)
    matrix = save("matrix", completed, True)
    variants = [job for job in matrix["tasks"] if job.get("task") == "matrix"]
    require(len(variants) == 2, "native matrix did not run both variants")
    for job in variants:
        require((Path(job["filesystem"]) / "mode.txt").read_text() == job["matrix"]["mode"] + "\n",
                "native matrix argument/output crossed variants")
        require("cache" not in job, "native matrix fabricated cache counters")
    require(variants[0]["digest"] != variants[1]["digest"], "native matrix outputs did not differ")
    checks.append("native-matrix-arguments")

    destination = root / "cancel.published-image"
    write({
        "build": {"coopr": {"files": ["cancel.coopr"]}, "cache": False},
        "publish": {"publish": {"from": "build", "destinations": ["oci:" + str(destination)]}},
    })
    process = subprocess.Popen(invocation + ["publish"], env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    captured = bytearray()
    try:
        selector = selectors.DefaultSelector()
        selector.register(process.stderr, selectors.EVENT_READ)
        deadline = time.monotonic() + TIMEOUT
        while b"STOKE_NATIVE_CANCEL_READY" not in captured.splitlines():
            require(time.monotonic() < deadline, "native cancellation readiness timed out")
            require(process.poll() is None, f"native cancellation exited before readiness: {captured.decode(errors='replace')}")
            if selector.select(timeout=1):
                chunk = os.read(process.stderr.fileno(), 65536)
                require(chunk, "native cancellation stderr closed before readiness")
                captured.extend(chunk)
        selector.close()
        known = descendants(process.pid)
        require(known, "no native workload descendants observed")
        process.send_signal(signal.SIGTERM)
        stdout, stderr = process.communicate(timeout=45)
        captured.extend(stderr)
        completed = subprocess.CompletedProcess(invocation, process.returncode, stdout.decode(), captured.decode())
        canceled = save("cancel", completed, False)
        require(any(job["name"] == "publish" and job["status"] == "skipped" for job in canceled["tasks"]), "canceled publication was not recorded as skipped")
        require(not destination.exists(), "canceled native task published")
        survivors = [pid for pid in known if live(pid)]
        require(not survivors, f"native workload descendants survived: {survivors}")
        (root / "cancel.lifetime.json").write_text(json.dumps({"stokePID": process.pid, "knownDescendants": sorted(known), "survivors": survivors}, indent=2) + "\n")
        checks.append("native-cancellation-cleanup")
    finally:
        if process.poll() is None:
            process.terminate()
            try:
                process.communicate(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.communicate(timeout=10)

    source = context / "literal.txt"
    stamp = source.stat()
    source.write_text("fresh literal payload\n")
    os.utime(source, ns=(stamp.st_atime_ns, stamp.st_mtime_ns))
    expected_changed = context_identity()
    require({name: value for name, value in expected_changed.items() if name != "literal.txt"} ==
            {name: value for name, value in original_context.items() if name != "literal.txt"}, "source mutation changed unrelated fixture inputs")
    definition("changed-literal")
    completed = subprocess.run(invocation + ["artifact"], env=env, capture_output=True, text=True, timeout=TIMEOUT)
    changed = save("changed-literal", completed, True)
    require(context_identity() == expected_changed, "source-change build modified caller inputs")
    jobs = {job["name"]: job for job in changed["tasks"]}
    require("STOKE_LITERAL_RUN_EXECUTED" in completed.stderr.splitlines() and jobs["literal"]["cache"]["Misses"] > 0, "same-size/restored-mtime source mutation reused the old RUN")
    require(jobs["literal"]["digest"] != literal_warm["digest"], "changed source did not change literal output identity")
    for name in ("literal", "artifact"):
        require((Path(jobs[name]["filesystem"]) / "literal-result.txt").read_text() == "fresh literal payload\n", "source-change result was not propagated")
    require("STOKE_NATIVE_TEST_EXECUTED" in completed.stderr.splitlines() and jobs["test"]["cache"]["Hits"] == 0, "source-change run reused fresh-only tests")
    checks.append("literal-local-cache-forced-execution-and-source-invalidation")

    summary = {"passed": True, "evidence": str(root), "checks": checks,
               "reports": {label: report["directory"] for label, report in reports.items()}}
    (root / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary, indent=2))


if __name__ == "__main__":
    main()

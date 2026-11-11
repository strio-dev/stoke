#!/usr/bin/env python3
"""Qualify bounded workflow scheduling with local rootless container workloads.

A host-local HTTP fixture controls branch release, so overlap and dependency
ordering are observable without timing sleeps. State and logs remain under the selected temporary directory.
"""

import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import urllib.parse
import uuid


TIMEOUT = 180


def require(condition, message):
    if not condition:
        raise AssertionError(message)


class Lane:
    def __init__(self, mode, expected=("native", "literal", "slow")):
        self.mode = mode
        self.expected = set(expected)
        self.lock = threading.Lock()
        self.arrivals = set()
        self.completed = set()
        self.ready = threading.Event()
        self.release = threading.Event()
        self.joined = threading.Event()
        self.trace = []
        self.errors = []

    def record(self, path, task):
        with self.lock:
            self.trace.append({"path": path, "task": task})


def workload_processes(token):
    found = []
    for entry in Path("/proc").iterdir():
        if not entry.name.isdigit():
            continue
        try:
            command = (entry / "cmdline").read_bytes().replace(b"\x00", b" ")
        except (FileNotFoundError, ProcessLookupError, PermissionError):
            continue
        if token.encode() in command:
            found.append({"pid": int(entry.name), "command": command.decode(errors="replace")})
    return found


def main():
    binary = Path(os.environ.get("STOKE_BINARY", "result-stoke/bin/stoke")).resolve()
    busybox_setting = os.environ.get("STOKE_SMOKE_BUSYBOX")
    require(binary.is_file(), f"Stoke executable missing: {binary}")
    require(busybox_setting, "STOKE_SMOKE_BUSYBOX must name the static BusyBox fixture")
    busybox = Path(busybox_setting).resolve()
    require(busybox.is_file(), f"BusyBox fixture missing: {busybox}")
    root = Path(tempfile.mkdtemp(prefix="stoke-dag-"))
    print(f"Retained workflow scheduling evidence: {root}", file=sys.stderr, flush=True)
    context = root / "context"
    context.mkdir()
    shutil.copy2(busybox, context / "busybox")
    workflow = context / "stoke.yaml"
    policy = root / "policy.json"
    policy.write_text(json.dumps({"default": [{"type": "reject"}], "transports": {
        "oci": {"": [{"type": "insecureAcceptAnything"}]},
        "containers-storage": {"": [{"type": "insecureAcceptAnything"}]},
    }}))
    network = root / "containers.conf"
    network.write_text('[network]\npasta_options = ["--map-host-loopback=169.254.1.3"]\n')
    env = os.environ.copy()
    env.update(XDG_STATE_HOME=str(root / "state"), XDG_CACHE_HOME=str(root / "cache"),
               XDG_CONFIG_HOME=str(root / "config"), XDG_DATA_HOME=str(root / "data"),
               CONTAINERS_CONF_OVERRIDE=str(network))
    current = {"lane": Lane("initial")}
    reports, checks = {}, []

    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            lane = current["lane"]
            query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
            path = urllib.parse.urlsplit(self.path).path
            task = query.get("task", [""])[0]
            lane.record(path, task)
            ok = True
            if path in {"/arrive", "/stop"}:
                with lane.lock:
                    lane.arrivals.add(task)
                    if lane.expected.issubset(lane.arrivals):
                        lane.ready.set()
                ok = lane.ready.wait(60)
                if path == "/stop" and (lane.mode == "cancel" or task != "a"):
                    ok = lane.release.wait(90) and ok
            elif path == "/slow":
                with lane.lock:
                    lane.arrivals.add("slow")
                    if lane.mode == "warm" or {"native", "literal"}.issubset(lane.arrivals):
                        lane.ready.set()
                ok = lane.joined.wait(75)
            elif path == "/done":
                with lane.lock:
                    lane.completed.add(task)
            elif path == "/join":
                with lane.lock:
                    if lane.mode != "warm" and lane.completed != {"native", "literal"}:
                        lane.errors.append("join started before both prerequisite workloads completed")
                    if "slow" not in lane.arrivals:
                        lane.errors.append("join did not overlap the independent blocked slow branch")
                    ok = not lane.errors
                lane.joined.set()
            else:
                ok = False
            try:
                self.send_response(200 if ok else 503)
                self.end_headers()
                self.wfile.write(b"ready\n" if ok else b"fixture gate failed\n")
            except (BrokenPipeError, ConnectionResetError):
                pass

        def log_message(self, *_args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    server.daemon_threads = True
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    host = os.environ.get("STOKE_SMOKE_HOST", "169.254.1.3")
    url = f"http://{host}:{server.server_port}"
    proxy_env = {"http_proxy": "", "HTTP_PROXY": "", "https_proxy": "", "HTTPS_PROXY": "", "ALL_PROXY": "", "NO_PROXY": "*"}

    def get(path):
        return f'/bin/busybox wget -T 90 -q -O /ready "{url}{path}"'

    def literal(command, cached=True, identity="literal"):
        task_env = dict(proxy_env, DAG_FIXTURE_BRANCH=identity)
        return {"image": "scratch", "cache": cached, "env": task_env, "steps": [
            {"copy": {"sources": ["busybox"], "destination": "/bin/busybox"}},
            {"run": ["/bin/busybox", "sh", "-eu", "-c", command]},
        ]}

    def write_parallel():
        native_command = get("/arrive?task=native") + '; printf "native\\n" > /native.txt; ' + get("/done?task=native")
        native = 'from "scratch" {\n    copy "busybox" "/bin/busybox"\n    run {\n        exec "/bin/busybox" "sh" "-eu" "-c" ' + json.dumps(native_command) + '\n    }\n}\n'
        (context / "native.coopr").write_text(native)
        workflow.write_text(json.dumps({"version": 1, "tasks": {
            "a_native": {"coopr": {"files": ["native.coopr"]}},
            "b_literal": literal(get("/arrive?task=literal") + '; printf "literal\\n" > /literal.txt; ' + get("/done?task=literal")),
            "c_slow": literal(get("/slow"), cached=False, identity="slow"),
            "join": {"from": "b_literal", "needs": ["a_native"], "cache": False, "env": dict(proxy_env), "steps": [
                {"copy": {"from": "a_native", "sources": ["/native.txt"], "destination": "/native.txt"}},
                {"run": ["/bin/busybox", "sh", "-eu", "-c", 'test "$(/bin/busybox cat /native.txt)" = native; test "$(/bin/busybox cat /literal.txt)" = literal; ' + get("/join")]},
            ]},
            "complete": {"image": "scratch", "needs": ["join", "c_slow"]},
        }}))

    def command():
        return [str(binary), "run", "--signature-policy", str(policy), "-f", str(workflow), "complete"]

    def save(label, completed, successful):
        (root / f"{label}.stdout").write_text(completed.stdout)
        (root / f"{label}.stderr").write_text(completed.stderr)
        require((completed.returncode == 0) == successful, f"{label}: exit={completed.returncode}\n{completed.stderr}")
        report = json.loads(completed.stdout)
        require(report["status"] == ("passed" if successful else "failed"), f"{label}: report status")
        persisted = json.loads((Path(report["directory"]) / "run.json").read_text())
        require(report == persisted, f"{label}: persisted report differs")
        planned = subprocess.run([str(binary), "plan", "-f", str(workflow), "complete"], env=env, capture_output=True, text=True, timeout=30)
        require(planned.returncode == 0, f"{label}: plan failed: {planned.stderr}")
        expected = [line.split("\t", 1)[0] for line in planned.stdout.splitlines()]
        require([item["name"] for item in report["tasks"]] == expected, f"{label}: report order differs from deterministic plan")
        require(not current["lane"].errors, f"{label}: fixture errors: {current['lane'].errors}")
        reports[label] = report
        (root / f"{label}.trace.json").write_text(json.dumps(current["lane"].trace, indent=2) + "\n")
        return report

    def execute(label):
        completed = subprocess.run(command(), env=env, capture_output=True, text=True, timeout=TIMEOUT)
        return save(label, completed, True)

    try:
        write_parallel()
        current["lane"] = Lane("cold")
        cold = execute("cold")
        require(current["lane"].arrivals == {"native", "literal", "slow"}, "independent native/literal RUNs never overlapped the third branch")
        require(current["lane"].joined.is_set(), "join did not release while an unrelated branch was blocked")
        checks.extend(["independent-native-literal-run-overlap", "join-waits-for-all-prerequisites-without-level-barrier", "deterministic-plan-order-reports"])
        current["lane"] = Lane("warm")
        warm = execute("warm")
        require(current["lane"].arrivals == {"slow"}, "warm native/literal branches contacted server instead of reusing cache")
        literal_result = next(item for item in warm["tasks"] if item["task"] == "b_literal")
        require(literal_result["cache"]["Hits"] > 0 and literal_result["cache"]["Misses"] == 0, "warm literal branch did not retain available cache hits")
        require(cold["directory"] != warm["directory"], "warm run did not use fresh stores")
        checks.append("coopr-native-literal-cache-reuse-across-fresh-run-stores")

        matrix_command = get("/arrive?task=$STOKE_MATRIX_MODE") + '; printf "%s\\n" "$STOKE_MATRIX_MODE" > /variant.txt'
        workflow.write_text(json.dumps({"version": 1, "tasks": {
            "base": {"image": "scratch", "steps": [
                {"copy": {"sources": ["busybox"], "destination": "/bin/busybox"}},
            ]},
            "matrix": {"from": "base", "strategy": {"matrix": {"mode": ["a", "b"]}, "max-parallel": 2},
                       "env": dict(proxy_env), "steps": [{"run": ["/bin/busybox", "sh", "-eu", "-c", matrix_command]}]},
            "complete": {"image": "scratch", "needs": ["matrix"]},
        }}))
        current["lane"] = Lane("matrix-cold", ("a", "b"))
        matrix_cold = execute("matrix-cold")
        require(current["lane"].arrivals == {"a", "b"}, "matrix RUNs did not overlap at their rendezvous")
        current["lane"] = Lane("matrix-warm", ("a", "b"))
        matrix_warm = execute("matrix-warm")
        require(not current["lane"].trace, "warm matrix contacted the server instead of reusing cache")
        originals = [job for job in matrix_cold["tasks"] if job["task"] == "matrix"]
        restored = [job for job in matrix_warm["tasks"] if job["task"] == "matrix"]
        require(len(originals) == len(restored) == 2, "expected two matrix variants")
        require(originals[0]["digest"] != originals[1]["digest"], "variant outputs have the same digest")
        for original, warm_job in zip(originals, restored):
            require(original["matrix"] == warm_job["matrix"] and original["digest"] == warm_job["digest"], "warm variant identity changed")
            require(warm_job["cache"]["Hits"] > 0, "variant had no warm cache hit")
            for job in (original, warm_job):
                require((Path(job["filesystem"]) / "variant.txt").read_text() == job["matrix"]["mode"] + "\n", "matrix output crossed variants")
        checks.append("matrix-native-overlap-distinct-outputs-and-warm-cache-restoration")

        for mode, matrix_case in (("failure", False), ("cancel", False), ("failure", True), ("cancel", True)):
            label = ("matrix-" if matrix_case else "") + mode
            active = ("a", "b") if matrix_case else ("a", "b", "c", "d")
            lane = Lane(mode, active)
            current["lane"] = lane
            token = "STOKE_DAG_CHILD_" + uuid.uuid4().hex
            child = f'/bin/busybox sh -c "/bin/busybox sleep 300 & wait" {token} & child=$!; '
            def blocked(task):
                command = child + get("/stop?task=" + task)
                if mode == "failure":
                    command += f'; if [ "{task}" = a ]; then exit 17; fi'
                return literal(command + '; wait "$child"', cached=False, identity=task)

            if matrix_case:
                tasks = {"matrix": blocked("$STOKE_MATRIX_MODE")}
                tasks["matrix"]["strategy"] = {"matrix": {"mode": ["a", "b"]}, "max-parallel": 2}
            else:
                tasks = {task: blocked(task) for task in active}
                tasks["e_queued"] = {"image": "scratch"}
            tasks["complete"] = {"image": "scratch", "needs": list(tasks)}
            workflow.write_text(json.dumps({"version": 1, "tasks": tasks}))
            process = subprocess.Popen(command(), env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
            try:
                require(lane.ready.wait(90), f"{label}: active workloads did not all reach the cancellation gate")
                if mode == "cancel":
                    process.send_signal(signal.SIGTERM)
                stdout, stderr = process.communicate(timeout=30)
                completed = subprocess.CompletedProcess(process.args, process.returncode, stdout, stderr)
                report = save(label, completed, False)
                statuses = {item.get("matrix", {}).get("mode", item["task"]): item["status"] for item in report["tasks"]}
                require(statuses["complete"] == "skipped", f"{label}: downstream work executed: {statuses}")
                if not matrix_case:
                    require(statuses["e_queued"] == "skipped", f"{label}: queued task executed: {statuses}")
                    require(not (Path(report["output"]) / "e_queued").exists(), f"{label}: queued output was created")
                require(all(statuses[task] == "canceled" for task in active[1:]), f"{label}: active siblings were not canceled: {statuses}")
                require(statuses["a"] == ("failed" if mode == "failure" else "canceled"), f"{mode}: initiating task classification: {statuses}")
                remaining = workload_processes(token)
                (root / f"{label}.remaining-processes.json").write_text(json.dumps(remaining, indent=2) + "\n")
                require(not remaining, f"{mode}: canceled workload descendants survived: {remaining}")
                checks.append(label + "-drains-active-work-and-descendants")
            finally:
                lane.release.set()
                if process.poll() is None:
                    process.terminate()
                    try:
                        process.communicate(timeout=10)
                    except subprocess.TimeoutExpired:
                        process.kill()
                        process.communicate(timeout=10)
        summary = {"passed": True, "evidence": str(root), "checks": checks,
                   "networkConfig": str(network), "reports": {name: report["directory"] for name, report in reports.items()}}
        (root / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
        print(json.dumps(summary, indent=2))
    finally:
        current["lane"].release.set()
        current["lane"].joined.set()
        current["lane"].ready.set()
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)


if __name__ == "__main__":
    main()

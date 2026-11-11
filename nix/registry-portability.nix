{
  pkgs,
  stoke,
  coopr,
}:
let
  inherit (pkgs) lib;
  # This path must be absent from the consumer's host store. Its complete
  # runtime closure reaches that guest only inside registry-carried image data.
  producerTool = pkgs.writeShellApplication {
    name = "stoke-portability-tool";
    text = "echo NIX_CLOSURE_EXECUTED";
  };
  input =
    (coopr.buildImage {
      name = "stoke-portability-input";
      packages = [
        pkgs.bash
        pkgs.coreutils
        pkgs.diffutils
        producerTool
      ];
    }).inputImage.oci;
  fixture = pkgs.runCommand "stoke-registry-portability-source" { } ''
    mkdir -p "$out"
    cat > "$out/build.coopr" <<'EOF'
    from "oci:base:image" {
      run {
        exec "/bin/bash" "-euc" "/bin/stoke-portability-tool > /tool-proof; /bin/head -c 16 /dev/urandom | /bin/od -An -tx1 > /value; echo EXACT_BUILD_RAN"
      }
    }
    EOF
    cat > "$out/stoke.yaml" <<'EOF'
    version: 1
    tasks:
      build:
        cache: false
        coopr: {files: [build.coopr]}
      derive:
        from: build
        steps:
          - run: [/bin/bash, -euc, '/bin/cp /value /derived; echo DERIVE_RAN']
      tests:
        from: derive
        cache: false
        strategy:
          max-parallel: 2
          fail-fast: false
          matrix: {mode: [a, b]}
        steps:
          - run: [/bin/bash, -euc, '/bin/cmp /value /derived; /bin/stoke-portability-tool; /bin/cp /derived /result; echo EXACT_TEST_RAN']
      package:
        needs: [tests]
        publish:
          from: derive
          destinations: [producer:5000/published/exact:tested]
    EOF
  '';
  commonPackages = [
    stoke
    coopr
    pkgs.crun
    pkgs.netavark
    pkgs.aardvark-dns
    pkgs.passt
    pkgs.slirp4netns
    pkgs.coreutils
    pkgs.gnutar
    pkgs.gzip
    pkgs.curl
    (pkgs.python3.withPackages (ps: [ ps.pyyaml ]))
  ];
  node = uid: {
    virtualisation = {
      memorySize = 2304;
      cores = 1;
      diskSize = 8192;
      useNixStoreImage = true;
      mountHostNixStore = false;
      podman.enable = true;
      additionalPaths = [ fixture ];
      containers.storage.settings.storage = {
        graphroot = lib.mkForce "";
        runroot = lib.mkForce "";
      };
      containers.containersConf.settings.engine.helper_binaries_dir = [
        "${pkgs.netavark}/bin"
        "${pkgs.aardvark-dns}/bin"
      ];
    };
    systemd.services."user@".serviceConfig.Delegate = "pids memory cpu cpuset";
    users.users.ci = {
      isNormalUser = true;
      inherit uid;
      autoSubUidGidRange = true;
      linger = true;
    };
    environment.systemPackages = commonPackages;
  };
in
assert lib.assertMsg (
  coopr ? buildImage
) "registry-portability requires a local Coopr derivation exposing buildImage";
pkgs.testers.runNixOSTest {
  name = "stoke-registry-portability";
  requiredFeatures.kvm = true;
  qemu.forceAccel = true;
  globalTimeout = 15 * 60;
  # Without this, every derivation named in testScript is added to EVERY guest,
  # making a producer-only closure absence assertion meaningless.
  includeTestScriptReferences = false;
  nodes = {
    producer = lib.recursiveUpdate (node 1000) {
      virtualisation.additionalPaths = [
        fixture
        input
      ];
      services.dockerRegistry = {
        enable = true;
        listenAddress = "0.0.0.0";
        openFirewall = true;
        enableDelete = true;
      };
    };
    consumer = node 2000;
  };
  testScript = ''
    import json
    import shlex

    start_all()
    fixture_nodes = [(producer, 1000), (consumer, 2000)]
    guest_identity = {}
    handwritten = "name: Handwritten\non: workflow_dispatch\njobs: {keep: {runs-on: ubuntu-latest, steps: [{run: 'echo retained'}]}}\n"
    for machine, uid in fixture_nodes:
        machine.wait_for_unit("default.target")
        machine.wait_for_unit(f"user@{uid}.service")
        assert machine.succeed("id -u ci").strip() == str(uid)
        assert machine.succeed("su - ci -c 'id -u'").strip() == str(uid)
        store_types = machine.succeed("findmnt -n -o FSTYPE /nix/store").splitlines()
        assert store_types and all(value not in ["9p", "virtiofs"] for value in store_types), store_types
        guest_identity[machine.name] = {
            "uid": uid, "storeFilesystem": store_types,
            "bootID": machine.succeed("cat /proc/sys/kernel/random/boot_id").strip(),
            "kernel": machine.succeed("uname -r").strip(),
        }
        machine.succeed("install -d -o ci -g users /home/ci/checkout /home/ci/evidence")
        machine.succeed("cp -RL ${fixture}/. /home/ci/checkout/; chown -R ci:users /home/ci/checkout")
        machine.succeed("install -d -o ci -g users /home/ci/checkout/.github/workflows")
        machine.succeed("printf '%s' " + shlex.quote(handwritten) + " > /home/ci/checkout/.github/workflows/manual.yml; chown ci:users /home/ci/checkout/.github/workflows/manual.yml")
        machine.succeed("printf '%s' " + shlex.quote(json.dumps({
            "default": [{"type": "reject"}],
            "transports": {name: {"": [{"type": "insecureAcceptAnything"}]}
                           for name in ["oci", "containers-storage", "docker"]}
        })) + " > /home/ci/policy.json; chown ci:users /home/ci/policy.json")
    producer.wait_for_unit("docker-registry.service")
    assert len({value["bootID"] for value in guest_identity.values()}) == 2
    producer.wait_for_open_port(5000)
    consumer.succeed("curl -fsS http://producer:5000/v2/")
    producer.succeed("cp -RL ${input} /home/ci/checkout/base; chown -R ci:users /home/ci/checkout/base")
    consumer.succeed("test ! -e ${producerTool}; test ! -e /home/ci/checkout/base")

    def user_command(machine, uid, command):
        prefix = "cd /home/ci/checkout && export " + " ".join([
            f"XDG_RUNTIME_DIR=/run/user/{uid}",
            "STOKE_COOPR_BINARY=${coopr}/bin/coopr",
            "GOMAXPROCS=1", "GOMEMLIMIT=768MiB",
        ]) + " && "
        return "su - ci -c " + shlex.quote(prefix + command)

    generation = ["${stoke}/bin/stoke", "ci", "generate", "-f", "stoke.yaml",
                  "--context", ".", "--network", "none", "--plain-http",
                  "--cache-from", "producer:5000/cache", "--cache-to", "producer:5000/cache",
                  "--results-repository", "producer:5000/results", "package"]
    plans = []
    for machine, uid in fixture_nodes:
        machine.succeed(user_command(machine, uid, shlex.join(generation)))
        machine.succeed(user_command(machine, uid, shlex.join(generation[:3] + ["--check"] + generation[3:])))
        assert machine.succeed("cat /home/ci/checkout/.github/workflows/manual.yml") == handwritten
        plans.append(json.loads(machine.succeed("cat /home/ci/checkout/.stoke/ci-plan.json")))
    plan = plans[0]
    assert plan == plans[1], "fresh checkout changed the generated plan"
    groups = {group["name"]: group for group in plan["groups"]}
    assert set(groups) == {"build", "derive", "tests", "package"}
    assert groups["tests"]["matrix"] and len(groups["tests"]["jobs"]) == 2
    assert groups["tests"]["maxParallel"] == 2 and not groups["tests"]["failFast"]
    assert len({job["outputKey"] for job in groups["tests"]["jobs"]}) == 2
    assert groups["tests"]["id"] in groups["package"]["needs"]
    render_check = "import json,yaml; print(json.dumps(yaml.safe_load(open('/home/ci/checkout/.github/workflows/stoke.generated.yml'))['jobs']))"
    rendered = json.loads(producer.succeed("python3 -c " + shlex.quote(render_check)))
    for group in groups.values():
        assert group["id"] in rendered
        needs = rendered[group["id"]].get("needs", [])
        if isinstance(needs, str):
            needs = [needs]
        assert set(group.get("needs", [])).issubset(needs)
    generated = producer.succeed("cat /home/ci/checkout/.github/workflows/stoke.generated.yml")
    assert "actions/upload-artifact" not in generated and "actions/download-artifact" not in generated

    github = "env GITHUB_SERVER_URL=https://github.com GITHUB_REPOSITORY=strio-dev/stoke GITHUB_RUN_ID=1 GITHUB_SHA=" + "1" * 40
    scope = json.loads(producer.succeed(user_command(producer, 1000, github + " ${stoke}/bin/stoke ci scope --plan-id " + plan["id"])))
    records = {}
    reports = {}

    def native_needs(job, overrides=None):
        group = next(group for group in plan["groups"] if any(row["id"] == job["id"] for row in group["jobs"]))
        result = {}
        for parent in group.get("needs", []):
            parent_group = next(group for group in plan["groups"] if group["id"] == parent)
            result[parent] = {"result": "success", "outputs": {
                row["outputKey"]: json.dumps(records[row["id"]], separators=(",", ":"))
                for row in parent_group["jobs"]
            }}
        if overrides:
            overrides(result)
        return result

    def run_job(machine, uid, label, job, attempt=1, expect=True, overrides=None, empty_cache=False):
        work = "/home/ci/evidence/" + label
        machine.succeed("install -d -o ci -g users " + shlex.quote(work))
        command = ["${stoke}/bin/stoke", "ci", "run", "--plan", ".stoke/ci-plan.json",
                   "--plan-id", plan["id"], "--job", job["id"], "--attempt", str(attempt),
                   "--scope", json.dumps(scope, separators=(",", ":")),
                   "--needs", json.dumps(native_needs(job, overrides), separators=(",", ":")),
                   "--coopr-binary", "${coopr}/bin/coopr", "--runtime", "${pkgs.crun}/bin/crun",
                   "--signature-policy", "/home/ci/policy.json", "--plain-http", "--tls-verify=false",
                   "--storage-driver", "vfs", "--output", work + "/output", "--cache-dir", work + "/cache",
                   "--github-output", work + "/native-output"]
        if empty_cache:
            command += ["--cache-from", "producer:5000/cache-miss", "--cache-to", "producer:5000/cache-miss"]
        shell = "touch " + shlex.quote(work + "/native-output") + "; "
        shell += "env XDG_STATE_HOME=" + shlex.quote(work + "/state") + " " + shlex.join(command)
        shell += " >" + shlex.quote(work + "/report.json") + " 2>" + shlex.quote(work + "/stderr.log")
        wrapped = user_command(machine, uid, shell)
        status, _ = machine.execute(wrapped)
        log = machine.succeed("cat " + shlex.quote(work + "/stderr.log"))
        raw = machine.succeed("cat " + shlex.quote(work + "/report.json"))
        if (status == 0) != expect:
            for suffix, source in [("stderr.log", "stderr.log"), ("report.json", "report.json")]:
                destination = "/home/ci/evidence/failure-" + label + "-" + suffix
                machine.succeed("cp " + shlex.quote(work + "/" + source) + " " + shlex.quote(destination))
                machine.copy_from_machine(destination)
            raise Exception(f"job {label} exited {status}; expected success={expect}\n{log}\n{raw}")
        report = json.loads(raw) if raw.strip() else None
        if expect:
            assert report is not None, "successful job returned no report"
            assert report["status"] == "passed", report
            native = machine.succeed("cat " + shlex.quote(work + "/native-output"))
            outputs = [line.split("=", 1)[1] for line in native.splitlines() if line.startswith(job["outputKey"] + "=")]
            assert len(outputs) == 1, native
            records[job["id"]] = json.loads(outputs[0])
            assert records[job["id"]] == report["ci"], "native output differs from report"
        reports[label] = {"report": report, "log": log, "guest": machine.name, "uid": uid}
        return report, log

    def row(task):
        return groups[task]["jobs"][0]

    def task(report, name):
        return next(item for item in report["tasks"] if item["name"] == name)

    def file(machine, report, name, relative):
        return machine.succeed("cat " + shlex.quote(task(report, name)["filesystem"] + "/" + relative))

    def markers(log, marker):
        return sum(line.strip() == marker for line in log.splitlines())

    built, log = run_job(producer, 1000, "build", row("build"))
    assert markers(log, "EXACT_BUILD_RAN") == 1
    value = file(producer, built, "build", "value")
    assert value.strip()
    derived, log = run_job(consumer, 2000, "derive", row("derive"))
    assert markers(log, "EXACT_BUILD_RAN") == 0 and markers(log, "DERIVE_RAN") == 1
    assert file(consumer, derived, "derive", "derived") == value
    consumer.succeed("test ! -e ${producerTool}; test ! -e /home/ci/checkout/base")
    tested_records = dict(records)
    for index, variant in enumerate(groups["tests"]["jobs"]):
        checked, log = run_job(consumer, 2000, f"test-{index}", variant)
        assert markers(log, "EXACT_TEST_RAN") == 1 and markers(log, "EXACT_BUILD_RAN") == 0
        assert file(consumer, checked, variant["id"], "result") == value
        assert (task(checked, variant["id"]).get("cache") or {}).get("Hits", 0) == 0
    tested_records.update(records)
    packaged, log = run_job(producer, 1000, "package", row("package"))
    assert not any(markers(log, marker) for marker in ["EXACT_BUILD_RAN", "DERIVE_RAN", "EXACT_TEST_RAN"])

    # Reading objects here is verification, not cross-job data transport. Jobs
    # only received small native metadata; their payloads came from the registry.
    accept = "Accept: application/vnd.oci.image.manifest.v1+json,application/vnd.oci.image.index.v1+json"

    def registry_json(path):
        return json.loads(producer.succeed("curl -fsS -H " + shlex.quote(accept) + " " + shlex.quote("http://producer:5000/v2/" + path)))

    def object_config(reference):
        name, digest = reference.split("@", 1)
        repository = name.split("/", 1)[1]
        manifest = registry_json(repository + "/manifests/" + digest)
        return registry_json(repository + "/blobs/" + manifest["config"]["digest"])

    bindings = object_config(tested_records[row("derive")["id"]]["inputs"])["bindings"]
    exact = object_config(bindings["derive"])
    published = producer.succeed("curl -fsS -H " + shlex.quote(accept) + " http://producer:5000/v2/published/exact/manifests/tested | sha256sum").split()[0]
    assert "sha256:" + published == exact["output"]["digest"]

    # A second guest user/store can reuse the same declared cache inputs. Its
    # new execution envelope is deliberately not combined with the old tests.
    warm, log = run_job(producer, 1000, "warm-derive", row("derive"), attempt=2)
    assert markers(log, "DERIVE_RAN") == 0 and file(producer, warm, "derive", "derived") == value
    assert (task(warm, "derive").get("cache") or {}).get("Hits", 0) > 0
    records = dict(tested_records)
    retried, log = run_job(producer, 1000, "fresh-test-retry", groups["tests"]["jobs"][0], attempt=2, empty_cache=True)
    assert markers(log, "EXACT_TEST_RAN") == 1 and markers(log, "EXACT_BUILD_RAN") == 0
    assert file(producer, retried, groups["tests"]["jobs"][0]["id"], "result") == value

    def failed_gate(needs):
        needs[groups["tests"]["id"]]["result"] = "failure"

    _, log = run_job(producer, 1000, "failed-native-gate", row("package"), attempt=2, expect=False, overrides=failed_gate)
    assert "native prerequisite is not successful" in log
    # Deliberate loss injection only in this disposable registry. This is not
    # an automatic retention policy or a recommendation to delete live retries.
    missing = tested_records[row("derive")["id"]]["inputs"].split("@", 1)[1]
    producer.succeed("curl -fsS -X DELETE " + shlex.quote("http://producer:5000/v2/results/manifests/" + missing))
    _, log = run_job(consumer, 2000, "missing-tested-input", groups["tests"]["jobs"][0], attempt=3, expect=False, empty_cache=True)
    assert markers(log, "EXACT_BUILD_RAN") == 0 and markers(log, "EXACT_TEST_RAN") == 0
    assert "manifest unknown" in log.lower() or "not found" in log.lower()
    summary = {
        "status": "PASS", "users": [1000, 2000], "separateGuestKernels": True,
        "guestIdentity": guest_identity,
        "hostStoreShared": False, "producerPathAbsentOnConsumerHost": "${producerTool}",
        "registryOnlyPayloadTransport": True, "hostedGitHubActions": False,
        "plan": plan, "reports": reports,
    }
    producer.succeed("printf '%s' " + shlex.quote(json.dumps(summary)) + " > /home/ci/evidence/summary.json")
    # Optional qualification evidence is extracted only after every job and
    # failure injection has finished. No consumer uses this report as data.
    producer.copy_from_machine("/home/ci/evidence/summary.json")
  '';
}

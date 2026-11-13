#!/usr/bin/env bash
set -euo pipefail
umask 077
: "${RUNNER_TEMP:?}" "${GITHUB_RUN_ID:?}" "${GITHUB_RUN_ATTEMPT:?}" "${APPLICATION_PLATFORM:?}" "${STOKE_GHCR_AUTH_DIR:?}"
context=$(pwd -P)
[[ $(git rev-parse HEAD) = 22f540863e3e3bf854bbfb37530de5c0d5305e88 ]]
[[ -z $(git status --porcelain --untracked-files=all --ignored=matching) ]]
[[ $(id -u):$(id -g) = 1001:1001 ]]
case "$APPLICATION_PLATFORM:$(uname -m)" in linux/amd64:x86_64|linux/arm64:aarch64) ;; *) exit 1 ;; esac
auth="$STOKE_GHCR_AUTH_DIR/auth.json"
[[ -f "$auth" && $(stat -c %a "$auth") = 600 ]]
repo=ghcr.io/strio-dev/stoke-qualification-tools
work="$RUNNER_TEMP/engine-comparison-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-${APPLICATION_PLATFORM#linux/}"
mkdir "$work"
tag="buildkit-benchmark-$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT-${APPLICATION_PLATFORM#linux/}"
# Both cold writers receive the same snapshot. Keep its immutable descriptors as
# direct seeding evidence; the additional local export perturbs COLD timing only.
verify_coopr_seed() {
  python3 - "$work/coopr-cold" "$auth" "$repo" "$1" <<'PY'
import hashlib,json,pathlib,subprocess,sys
cold=pathlib.Path(sys.argv[1]); layout=cold/'cold-seed'
def verified(descriptor):
    algorithm,encoded=descriptor['digest'].split(':')
    assert algorithm=='sha256'
    data=(layout/'blobs'/algorithm/encoded).read_bytes()
    assert len(data)==descriptor['size'] and hashlib.sha256(data).hexdigest()==encoded
    return json.loads(data)
image=json.loads((cold/'metadata.json').read_text())['containerimage.digest']
roots=[d for d in json.loads((layout/'index.json').read_text())['manifests']
       if d.get('annotations',{}).get('org.opencontainers.image.ref.name','').startswith('graph-v3-')]
assert roots, 'cold build did not publish an OCI cache seed'
matched=False; descriptors=[]
for root in roots:
    tag=root['annotations']['org.opencontainers.image.ref.name']
    assert tag.startswith('graph-v3-'), tag
    index=verified(root)
    assert index['mediaType']=='application/vnd.oci.image.index.v1+json'
    metadata=[m for m in (verified(d) for d in index['manifests']) if m.get('artifactType')=='application/vnd.coopr.strio.dev.graph-cache.v3+json']
    assert len(metadata)==1
    manifest=metadata[0]; assert manifest['layers']==[]
    assert manifest['config']['mediaType']=='application/vnd.coopr.strio.dev.graph-cache.config.v3+json'
    record=verified(manifest['config'])
    assert record['version']=='coopr.strio.dev/graph-cache-record/v3'
    assert tag=='graph-v3-'+record['root'].split(':')[1]
    selected={r['ID'] for node in record['metadata']['nodes'] if node['id'] in record['metadata']['roots'] for r in node.get('results',[])}
    matched=matched or any(r['id'] in selected and r['image']['digest']==image for r in record['results'])
    remote=cold/('remote-'+tag+'.json')
    subprocess.run(['benchmark-rootless','bash','-c',
        'exec coopr --root "$1/root" --runroot "$1/run" --storage-driver vfs manifest inspect --authfile "$2" "$3" > "$4"',
        'seed-check',str(cold/'seed-inspection'),sys.argv[2],sys.argv[3]+':'+tag,str(remote)],check=True,stdout=subprocess.DEVNULL)
    # Inspect preserves the remote index fields, including the exact immutable
    # metadata/config binding, while formatting JSON for CLI output.
    assert json.loads(remote.read_text())==index, 'remote seed changed: '+tag
    descriptors.append(root['digest'])
assert matched, 'cold cache seed does not bind the built image'
print(json.dumps({'phase':'cold-seed-proof','checkpoint':sys.argv[4],'graphDescriptors':sorted(descriptors),'coldImage':image,'passed':True}))
PY
}
for engine in buildkit coopr; do
  for phase in cold local-warm registry-warm; do
    dir="$work/$engine-$phase"; mkdir "$dir"
    state="$work/$engine-local-state"
    [[ "$phase" != registry-warm ]] || state="$work/$engine-registry-state"
    if [[ "$engine:$phase" = coopr:registry-warm ]]; then verify_coopr_seed before-fresh-store; fi
    printf 'BENCHMARK_CASE engine=%s phase=%s platform=%s\n' "$engine" "$phase" "$APPLICATION_PLATFORM"
    if [[ "$engine" = buildkit ]]; then
      args=(--context "$context" --state "$state" --output "$dir/fs" --platform "$APPLICATION_PLATFORM" --authfile "$auth" --runtime /bin/crun)
      [[ "$phase" != registry-warm ]] || args+=(--cache-from "$repo:$tag-cold")
      args+=(--cache-to "$repo:$tag-$phase")
      benchmark-rootless buildkit-reference "${args[@]}" 2>&1 | tee "$dir/run.log"
    else
      # The child shell expands its positional arguments inside the private namespace.
      # shellcheck disable=SC2016
      benchmark-rootless bash -c '
        set -euo pipefail
        dir=$1; state=$2; context=$3; platform=$4; auth=$5; repo=$6; phase=$7
        base=(coopr --root "$state/root" --runroot "$state/run" --storage-driver vfs)
        args=(--cache-to "$repo")
        [[ "$phase" != cold ]] || args+=(--cache-to "type=local,dest=$dir/cold-seed")
        [[ "$phase" != registry-warm ]] || args+=(--cache-from "$repo")
        start=$(python3 -c "import time; print(time.monotonic_ns())")
        build_started=$EPOCHREALTIME
        "${base[@]}" build --file "$context/examples/ci/build.coopr" "$context" \
          --platform "$platform" --runtime /bin/crun --network ns:/proc/self/ns/net \
          --timestamp=0 --rewrite-timestamp --format oci --authfile "$auth" \
          --metadata-file "$dir/metadata.json" \
          --iidfile "$dir/id" --output "type=local,dest=$dir/fs" "${args[@]}"
        build_completed=$EPOCHREALTIME
        "${base[@]}" push --platform "$platform" --authfile "$auth" --format oci \
          --digestfile "$dir/digest" "$(cat "$dir/id")" "oci-archive:$dir/output.oci.tar:binary"
        python3 - "$start" <<PY
import json,sys,time
print(json.dumps({"phase":"engine-core-total","seconds":(time.monotonic_ns()-int(sys.argv[1]))/1e9,"engine":"coopr","includes":"build+cache-export+push+both-CLI-bootstrap","coldIncludesAdditionalLocalSeedWriter":"$phase"=="cold"}))
PY
        python3 - "$dir/metadata.json" "$build_started" "$build_completed" <<PY
import datetime,json,sys
metadata=json.load(open(sys.argv[1]))
events=metadata.get("coopr.operation.progress", [])
exports=[{key:event[key] for key in ("instruction","phase","stage","cache","state","durationNanos") if key in event} for event in events if event.get("phase")=="export" and event.get("state")=="completed"]
completed=[datetime.datetime.fromisoformat(event["completed"].replace("Z", "+00:00")).timestamp() for event in events if event.get("completed")]
print(json.dumps({"phase":"native-build-phase-profile","buildSeconds":float(sys.argv[3])-float(sys.argv[2]),"exportPhases":exports,"progressDropped":metadata.get("coopr.operation.progress.dropped",0),"lastObservedPhaseToBuildReturnSeconds":float(sys.argv[3])-max(completed) if completed else None}))
PY
        # Native archive ingestion and filesystem verification are OUTSIDE core time.
        proof=(coopr --root "$dir/proof/root" --runroot "$dir/proof/run" --storage-driver vfs)
        printf "BENCHMARK_PROOF_STEP=archive-load\n"
        "${proof[@]}" pull --quiet --platform "$platform" --authfile "$auth" "oci-archive:$dir/output.oci.tar:binary" > "$dir/imported-id"
        printf "BENCHMARK_PROOF_STEP=archive-inspect\n"
        "${proof[@]}" image inspect "$(cat "$dir/imported-id")" > "$dir/inspect.json"
        mkdir "$dir/proof-input"
        ln "$dir/output.oci.tar" "$dir/proof-input/output.oci.tar"
        printf "from \"oci-archive:output.oci.tar:binary\" {}\n" > "$dir/proof.coopr"
        printf "BENCHMARK_PROOF_STEP=archive-materialize\n"
        "${proof[@]}" build --file "$dir/proof.coopr" "$dir/proof-input" --platform "$platform" \
          --runtime /bin/crun --network none --authfile "$auth" --output "type=local,dest=$dir/archive-fs" 2>&1 | tee "$dir/proof.log"
      ' coopr "$dir" "$state" "$context" "$APPLICATION_PLATFORM" "$auth" "$repo" "$phase" 2>&1 | tee "$dir/run.log"
      if [[ "$phase" = cold || "$phase" = registry-warm ]]; then verify_coopr_seed "after-$phase"; fi
    fi
  done
done
python3 - "$work" "$APPLICATION_PLATFORM" <<'PY'
import hashlib,json,pathlib,stat,sys
root=pathlib.Path(sys.argv[1]); rows=[]
for engine in ('coopr','buildkit'):
    for phase in ('cold','local-warm','registry-warm'):
        d=root/f'{engine}-{phase}'; log=(d/'run.log').read_text(); events=[]
        for line in log.splitlines():
            try: events.append(json.loads(line))
            except json.JSONDecodeError: pass
        f=d/'fs/stoke'; s=f.lstat(); assert stat.S_ISREG(s.st_mode) and stat.S_IMODE(s.st_mode)==0o755
        binary='sha256:'+hashlib.file_digest(f.open('rb'),'sha256').hexdigest()
        if engine=='coopr':
            run_count=sum(line.rstrip('\r')=='STOKE_BUILD_EXECUTED' for line in log.splitlines())
            imported=json.loads((d/'inspect.json').read_text())[0]; cfg=imported['Config']
            image=(d/'digest').read_text().strip()
            assert image==imported['Digest']
            assert imported['Os']+'/'+imported['Architecture']==sys.argv[2]
            archived=d/'archive-fs/stoke'; a=archived.lstat()
            assert stat.S_ISREG(a.st_mode) and stat.S_IMODE(a.st_mode)==0o755
            assert hashlib.file_digest(archived.open('rb'),'sha256').hexdigest()==binary[7:]
        else:
            core=next(e for e in events if e.get('phase')=='engine-core-total'); run_count=core['runCalls']
            p=next(e for e in events if e.get('phase')=='output-proof'); cfg=p['functionalConfig']; image=p['imageDigest']
            assert p['binarySHA256']==binary and p['layerBinaryMatchesFilesystem'] and p['binaryMode']=='0755'
        assert run_count==(1 if phase=='cold' else 0),(engine,phase,'actual RUN',run_count)
        config={'env':sorted(cfg.get('Env') or []),'workdir':cfg.get('WorkingDir') or '/', 'user':cfg.get('User') or '', 'cmd':cfg.get('Cmd') or [],'entrypoint':cfg.get('Entrypoint') or [],'labels':cfg.get('Labels') or {},'ports':sorted(cfg.get('ExposedPorts') or {}),'volumes':sorted(cfg.get('Volumes') or {}),'stopSignal':cfg.get('StopSignal') or ''}
        row={'engine':engine,'case':phase,'platform':sys.argv[2],'binarySHA256':binary,'binaryMode':'0755','imageDigest':image,'functionalConfig':config,'runCalls':run_count,'timings':[e for e in events if e.get('phase') in ('engine-core-total','outer-rootlesskit-total')]}
        rows.append(row)
        print(json.dumps(row,sort_keys=True))
for engine in ('coopr','buildkit'):
    own=[r for r in rows if r['engine']==engine]; assert len({r['imageDigest'] for r in own})==1,(engine,'own cold/warm OCI identity')
assert len({r['binarySHA256'] for r in rows})==1
assert all(r['functionalConfig']==rows[0]['functionalConfig'] for r in rows)
print(json.dumps({'phase':'comparison-proof','passed':True,'cacheSeedBoundary':'Coopr cold local/registry writers share the same immutable graph snapshot; remote descriptors verified before/after fresh-store reuse. Requires no concurrent writer for these keys. BuildKit unique own-cold tag.','timerBoundary':'Engine core excludes output proof; Coopr cold includes an additional local cache seed writer and is diagnostic, not a fair cold performance comparison. Warm paths have only their normal registry writer. RootlessKit outer includes setup and engine-specific proof.'},sort_keys=True))
PY
[[ -z $(git status --porcelain --untracked-files=all --ignored=matching) ]]

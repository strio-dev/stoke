# shellcheck shell=bash
set -euo pipefail
umask 077
[[ "$(id -u):$(id -g)" = 1001:1001 ]]
[[ $# -gt 0 ]]
engine=$(basename -- "$1")
printf 'BENCHMARK_OUTER_MACHINE=%s AVAILABLE_CPUS=%s\n' "$(uname -m)" "$(nproc)"
grep '^MemTotal:' /proc/meminfo
for resource in /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max; do
  if [[ -r "$resource" ]]; then printf '%s=' "$resource"; cat "$resource"; fi
done
started=$(python3 -c 'import time; print(time.monotonic_ns())')
outer_netns=$(stat -Lc '%d:%i' /proc/self/ns/net)
[[ "$outer_netns" =~ ^[0-9]+:[0-9]+$ ]]
export BENCHMARK_OUTER_NETNS="$outer_netns"
rk_state=$(mktemp -d "${TMPDIR:-/tmp}/stoke-benchmark-rootless.XXXXXXXX")
child=""
interrupted=0
trap 'interrupted=143; if [[ -n "$child" ]]; then kill -TERM "$child" 2>/dev/null || true; fi' TERM
trap 'interrupted=130; if [[ -n "$child" ]]; then kill -INT "$child" 2>/dev/null || true; fi' INT
# The child shell expands the private-namespace checks and command arguments.
# shellcheck disable=SC2016
rootlesskit --state-dir="$rk_state" --subid-source=static \
  --net=slirp4netns --disable-host-loopback --copy-up=/etc --copy-up=/run \
  bash -c '
    set -euo pipefail
    [[ "$(id -u):$(id -g)" = 0:0 ]]
    inner_netns=$(stat -Lc "%d:%i" /proc/self/ns/net)
    [[ "$inner_netns" =~ ^[0-9]+:[0-9]+$ ]]
    [[ "$inner_netns" != "$BENCHMARK_OUTER_NETNS" ]]
    printf "BENCHMARK_NETWORK_NAMESPACE outer=%s inner=%s\n" "$BENCHMARK_OUTER_NETNS" "$inner_netns"
    printf "BENCHMARK_INNER_UID_MAP\n"
    cat /proc/self/uid_map
    printf "BENCHMARK_INNER_GID_MAP\n"
    cat /proc/self/gid_map
    exec "$@"
  ' benchmark-child "$@" &
child=$!
status=0
wait "$child" || status=$?
if kill -0 "$child" 2>/dev/null; then wait "$child" || status=$?; fi
if [[ "$interrupted" != 0 ]]; then status=$interrupted; fi
# Stock RootlessKit removes its own state, including its private api.sock.
# On early startup failure, remove only the empty directory created above.
cleanup_status=0
if [[ -d "$rk_state" ]]; then rmdir -- "$rk_state" || cleanup_status=$?; fi
python3 - "$started" "$status" "$cleanup_status" "$engine" <<'PYTHON'
import json, sys, time
print(json.dumps({"phase":"outer-rootlesskit-total","seconds":(time.monotonic_ns()-int(sys.argv[1]))/1e9,"engine":sys.argv[4],"exitStatus":int(sys.argv[2]),"stateCleanupStatus":int(sys.argv[3])},sort_keys=True))
PYTHON
if [[ "$status" = 0 && "$cleanup_status" != 0 ]]; then status=$cleanup_status; fi
exit "$status"

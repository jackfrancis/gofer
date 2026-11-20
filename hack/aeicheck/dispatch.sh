#!/usr/bin/env bash
# The dispatch half of `make aei-check`: before gofer is deployed, confirm that the
# aei-controller Service serves AEI's dispatch listener on the TCP port gofer dials
# (AEI_DISPATCH_PORT, read from AEI_DISPATCH_ENDPOINT in the base gofer-config).
# gofer dispatches there with a token projected for the aei-dispatch audience; an AEI
# from before that listener (agent-execution-interface 34d0951) has neither, and the
# web pod would still turn Ready, since /healthz does not call AEI.
#
# Keep kubectl and its cluster-selection flags as separate arguments; never eval a
# command string.
set -euo pipefail

port=${AEI_DISPATCH_PORT:-}
[[ $port =~ ^[0-9]+$ ]] || {
    echo "ERROR: could not read the dispatch port from AEI_DISPATCH_ENDPOINT in deploy/k8s/base/configmap.yaml." >&2
    exit 1
}
[[ $# -gt 0 ]] || { echo "ERROR: pass kubectl and its cluster-selection flags." >&2; exit 1; }
kubectl=("$@")

errfile=$(mktemp)
trap 'rm -f "$errfile"' EXIT

# One line per Service port: name, port, protocol.
if ! ports=$("${kubectl[@]}" -n aei-system get service aei-controller \
    -o jsonpath='{range .spec.ports[*]}{.name}{" "}{.port}{" "}{.protocol}{"\n"}{end}' 2>"$errfile"); then
    echo "ERROR: could not read the aei-controller Service in aei-system:" >&2
    sed 's/^/         /' "$errfile" >&2
    echo "       gofer checks that AEI serves its dispatch listener before it deploys. The" >&2
    echo "       deploying identity needs 'get' on services/aei-controller in aei-system" >&2
    echo "       (README, \"Deploy permissions\"). If the Service is missing, install AEI first." >&2
    exit 1
fi

# The protocol defaults to TCP, and the API server fills it in; an empty one is TCP.
if awk -v port="$port" '$1 == "dispatch" && $2 == port && ($3 == "TCP" || $3 == "") { found = 1 } END { exit !found }' <<<"$ports"; then
    exit 0
fi
echo "ERROR: the aei-controller Service in aei-system does not serve the dispatch listener on" >&2
echo "       TCP $port, the port gofer dials (AEI_DISPATCH_ENDPOINT). Its ports (name port protocol):" >&2
if [[ -n $ports ]]; then sed 's/^/         /' <<<"$ports" >&2; else echo "         (none)" >&2; fi
echo "       gofer dispatches to AEI's dispatch listener with an aei-dispatch token. Upgrade AEI" >&2
echo "       to a release that serves it (agent-execution-interface 34d0951 or later), then re-run." >&2
exit 1

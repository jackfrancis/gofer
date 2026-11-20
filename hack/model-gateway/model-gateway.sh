#!/usr/bin/env bash
# The cluster half of gofer's optional model gateway (docs/adr/0008-model-gateway.md),
# behind `make model-gateway-apply | model-gateway-down | model-gateway-check` and
# make deploy's gateway steps. Keep kubectl and its cluster-selection flags as separate
# arguments; never eval a command string. Every write is checked, and the steps run
# in an order that never loses the only copy of the model key or of the provider
# endpoints:
#
#   apply:  write the gateway Secret (read back) -> roll out the gateway -> point
#           gofer-config at it, saving the original AI_CONNECTIONS once -> only then
#           remove AI_TOKEN from gofer-secrets (read back) -> restart gofer.
#   down:   put the key back in gofer-secrets (read back) -> restore the saved
#           AI_CONNECTIONS verbatim and drop AI_GATEWAY_URL in one patch -> restart
#           gofer -> only then delete the gateway.
#   preflight / sync:  make deploy's steps. Never touch a key: preflight refuses a
#           deploy that would hand gofer AI_TOKEN beside the gateway, and sync points
#           the freshly re-applied base config back at the gateway.
#   check:  read-only check of a deployed profile.
set -euo pipefail

cmd=${1:?usage: model-gateway.sh apply|down|preflight|sync|check kubectl [flags...]}
shift
[[ $# -gt 0 ]] || { echo "ERROR: pass kubectl and its cluster-selection flags." >&2; exit 1; }
kubectl=("$@")

ns=${KUBE_NS:-gofer}
gw_url=${MODEL_GATEWAY_URL:-http://gofer-model-gateway.$ns.svc.cluster.local}
default_upstream=https://api.githubcopilot.com
upstream_annotation=gofer/model-gateway-upstream
rollout_timeout=${ROLLOUT_TIMEOUT:-180s}
# The original AI_CONNECTIONS, saved at the first rewrite and restored verbatim by
# down. It names the provider each model went to, so rollback never guesses it.
saved_key=AI_CONNECTIONS_BEFORE_GATEWAY

fail() { echo "ERROR: $*" >&2; exit 1; }
kube() { "${kubectl[@]}" "$@"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# secret_get NAME KEY prints the base64 value, or nothing when the Secret or key is
# absent. Any other failure (forbidden, unreachable) stops the script.
secret_get() {
    local out
    out=$(kube -n "$ns" get secret "$1" --ignore-not-found -o json) || fail "could not read Secret $ns/$1."
    [[ -n $out ]] || return 0
    jq -r --arg k "$2" '.data[$k] // empty' <<<"$out"
}

config_get() {
    kube -n "$ns" get configmap gofer-config -o json || fail "could not read ConfigMap $ns/gofer-config."
}

# patch_config FILE applies a JSON merge patch to gofer-config (null removes a key).
patch_config() {
    kube -n "$ns" patch configmap gofer-config --type=merge --patch-file "$1" >/dev/null ||
        fail "could not update ConfigMap $ns/gofer-config; nothing after this step ran."
}

# origin of a URL, lowercased: scheme://host[:port].
jq_origin='def origin: (capture("^(?<o>[A-Za-z][A-Za-z0-9+.-]*://[^/]+)").o // "") | ascii_downcase;'

# routing_patch prints the merge patch that points gofer-config at the gateway, or
# fails without writing anything. Every endpoint must be on the upstream (the first
# time) or every endpoint already on the gateway (a repeat); a mix is refused.
routing_patch() {
    local cfg conns
    cfg=$(config_get)
    conns=$(jq -r '.data.AI_CONNECTIONS // empty' <<<"$cfg")
    [[ -n $conns ]] || fail "gofer-config has no AI_CONNECTIONS to route through the gateway."
    jq -ce --arg gw "$gw_url" --arg up "$upstream" --arg saved "$saved_key" --argjson cfg "$cfg" "$jq_origin"'
        ($gw | origin) as $g | ($up | origin) as $u |
        ([.[].endpoint | origin == $g] | all) as $done |
        ([.[].endpoint | origin == $g] | any) as $some |
        if length == 0 then error("AI_CONNECTIONS is empty")
        elif $done then
            if ($cfg.data[$saved] // "") == "" then
                error("AI_CONNECTIONS already points at the gateway but the original (\($saved)) is not saved; restore AI_CONNECTIONS to the provider endpoints and re-run")
            else {data: {AI_GATEWAY_URL: $gw}} end
        elif $some then error("AI_CONNECTIONS mixes gateway and provider endpoints; restore it to the provider endpoints and re-run")
        else
            (map(.endpoint |= (
                if origin != $u then error("AI_CONNECTIONS endpoint \(.) is not on MODEL_GATEWAY_UPSTREAM \($up)")
                elif test("/chat/completions/?$") then $gw + "/v1/chat/completions"
                elif test("/responses/?$") then $gw + "/v1/responses"
                else error("AI_CONNECTIONS endpoint \(.) is neither .../chat/completions nor .../responses") end))) as $new |
            {data: {AI_CONNECTIONS: ($new | tojson), AI_GATEWAY_URL: $gw, ($saved): $cfg.data.AI_CONNECTIONS}}
        end' <<<"$conns" || fail "could not point AI_CONNECTIONS at the gateway (above); nothing was changed."
}

# route points gofer-config at the gateway and confirms it took.
route() {
    routing_patch >"$tmp/routing.json"
    patch_config "$tmp/routing.json"
    local got
    got=$(config_get | jq -r '.data.AI_GATEWAY_URL // empty')
    [[ $got == "$gw_url" ]] || fail "gofer-config did not keep AI_GATEWAY_URL; nothing after this step ran."
    echo "model gateway: AI_CONNECTIONS = $(config_get | jq -r '.data.AI_CONNECTIONS')"
}

# write_gateway_key B64 writes the gateway Secret from a file (the key is never on a
# command line) and reads it back.
write_gateway_key() {
    jq -n --arg ns "$ns" --arg v "$1" \
        '{apiVersion: "v1", kind: "Secret", metadata: {name: "gofer-model-gateway", namespace: $ns}, type: "Opaque", data: {AI_TOKEN: $v}}' \
        >"$tmp/gateway-secret.json"
    kube -n "$ns" apply -f "$tmp/gateway-secret.json" >/dev/null ||
        fail "could not write Secret $ns/gofer-model-gateway; gofer-secrets is unchanged."
    [[ $(secret_get gofer-model-gateway AI_TOKEN) == "$1" ]] ||
        fail "Secret $ns/gofer-model-gateway does not hold the key just written; gofer-secrets is unchanged."
}

restart_gofer() {
    kube -n "$ns" rollout restart statefulset/gofer
    kube -n "$ns" rollout status statefulset/gofer --timeout="$rollout_timeout"
}

gateway_installed() { kube -n "$ns" get deployment gofer-model-gateway >/dev/null 2>&1; }

# resolve_upstream sets upstream: the provider the installed gateway fronts, else
# MODEL_GATEWAY_UPSTREAM, else GitHub Copilot. Asking an installed gateway for a
# different provider is refused: its key and the saved endpoints belong to the old one.
resolve_upstream() {
    local out installed requested=${MODEL_GATEWAY_UPSTREAM:-}
    out=$(kube -n "$ns" get deployment gofer-model-gateway --ignore-not-found -o json) || fail "could not read the gateway Deployment."
    installed=
    [[ -z $out ]] || installed=$(jq -r --arg a "$upstream_annotation" '.metadata.annotations[$a] // empty' <<<"$out")
    if [[ -n $installed && -n $requested && $requested != "$installed" ]]; then
        fail "the gateway is installed for $installed. To change the provider, run 'make model-gateway-down' first, then apply with MODEL_GATEWAY_UPSTREAM=$requested."
    fi
    upstream=${installed:-${requested:-$default_upstream}}
}

do_apply() {
    : "${MODEL_GATEWAY_BURST:?}" "${MODEL_GATEWAY_REFILL:?}" "${MODEL_GATEWAY_INTERVAL:?}"
    kube -n "$ns" get statefulset gofer >/dev/null 2>&1 || fail "gofer is not deployed here; deploy it first."
    resolve_upstream
    # Fail on a bad AI_CONNECTIONS before anything is written.
    routing_patch >/dev/null

    local web gw want
    web=$(secret_get gofer-secrets AI_TOKEN)
    gw=$(secret_get gofer-model-gateway AI_TOKEN)
    if [[ -n ${AI_TOKEN:-} ]]; then
        # An explicit key: install or rotate it.
        want=$(printf '%s' "$AI_TOKEN" | base64 | tr -d '\n')
    elif [[ -n $gw ]]; then
        want=$gw
        [[ -z $web || $web == "$gw" ]] ||
            fail "gofer-secrets and gofer-model-gateway hold different AI_TOKENs. Pass the one you want as AI_TOKEN, or remove the other, and re-run."
    elif [[ -n $web ]]; then
        want=$web
    else
        fail "no upstream key. Set AI_TOKEN in your shell, or create Secret $ns/gofer-model-gateway with key AI_TOKEN."
    fi
    if [[ $gw != "$want" ]]; then
        write_gateway_key "$want"
        echo "model gateway: upstream key written to $ns/gofer-model-gateway"
    fi

    kube kustomize deploy/k8s/model-gateway >"$tmp/gateway.yaml" || fail "could not render deploy/k8s/model-gateway."
    sed -e "s|@UPSTREAM@|$upstream|" \
        -e "s|@AUTHZ_HOST@|gofer.$ns.svc.cluster.local:8080|" \
        -e "s|@BURST@|$MODEL_GATEWAY_BURST|" \
        -e "s|@REFILL@|$MODEL_GATEWAY_REFILL|" \
        -e "s|@INTERVAL@|$MODEL_GATEWAY_INTERVAL|" "$tmp/gateway.yaml" >"$tmp/gateway.rendered.yaml"
    kube -n "$ns" apply -f "$tmp/gateway.rendered.yaml" >/dev/null || fail "could not apply the gateway; gofer is unchanged."
    # The config's name does not change with the values filled in, so restart to load them.
    kube -n "$ns" rollout restart deployment/gofer-model-gateway >/dev/null
    kube -n "$ns" rollout status deployment/gofer-model-gateway --timeout="$rollout_timeout" ||
        fail "the gateway did not become ready; gofer is unchanged."

    route

    # Only now, with the key safe in the gateway and gofer's config pointing at it,
    # remove gofer's copy. The running web pod keeps its environment until restarted.
    if [[ -n $web ]]; then
        printf '%s' '[{"op":"remove","path":"/data/AI_TOKEN"}]' >"$tmp/remove-key.json"
        kube -n "$ns" patch secret gofer-secrets --type=json --patch-file "$tmp/remove-key.json" >/dev/null ||
            fail "could not remove AI_TOKEN from gofer-secrets; gofer was not restarted. Remove it and restart gofer."
        [[ -z $(secret_get gofer-secrets AI_TOKEN) ]] ||
            fail "gofer-secrets still holds AI_TOKEN; gofer was not restarted. Remove it and restart gofer."
        echo "model gateway: removed AI_TOKEN from gofer-secrets (the gateway holds it)"
    fi
    restart_gofer
}

do_down() {
    local cfg before key
    cfg=$(config_get)
    before=${MODEL_GATEWAY_RESTORE_CONNECTIONS:-$(jq -r --arg s "$saved_key" '.data[$s] // empty' <<<"$cfg")}
    [[ -n $before ]] || fail "gofer-config has no saved $saved_key, so the provider endpoints are unknown. Pass them as MODEL_GATEWAY_RESTORE_CONNECTIONS and re-run."
    jq -e --arg gw "$gw_url" "$jq_origin"' ($gw | origin) as $g | type == "array" and length > 0 and all(.[]; (.endpoint | origin) != $g)' \
        <<<"$before" >/dev/null 2>&1 || fail "the AI_CONNECTIONS to restore is not a list of provider endpoints: $before"
    key=$(secret_get gofer-model-gateway AI_TOKEN)
    [[ -n $key ]] || fail "Secret $ns/gofer-model-gateway holds no AI_TOKEN to give back to gofer; nothing was changed."

    jq -n --arg v "$key" '{data: {AI_TOKEN: $v}}' >"$tmp/key.json"
    kube -n "$ns" patch secret gofer-secrets --type=merge --patch-file "$tmp/key.json" >/dev/null ||
        fail "could not put AI_TOKEN back in gofer-secrets; nothing else was changed."
    [[ $(secret_get gofer-secrets AI_TOKEN) == "$key" ]] ||
        fail "gofer-secrets does not hold the key just written; nothing else was changed."

    # One patch: the original endpoints, and no gateway.
    jq -n --arg c "$before" --arg s "$saved_key" '{data: {AI_CONNECTIONS: $c, AI_GATEWAY_URL: null, ($s): null}}' >"$tmp/restore.json"
    patch_config "$tmp/restore.json"
    cfg=$(config_get)
    jq -e --arg c "$before" '.data.AI_CONNECTIONS == $c and (.data.AI_GATEWAY_URL // "") == ""' <<<"$cfg" >/dev/null ||
        fail "gofer-config was not restored; the gateway is still running."
    echo "AI_CONNECTIONS = $before"

    restart_gofer
    kube kustomize deploy/k8s/model-gateway >"$tmp/gateway.yaml" || fail "could not render deploy/k8s/model-gateway."
    kube -n "$ns" delete --ignore-not-found -f "$tmp/gateway.yaml"
    echo "model gateway removed. Secret $ns/gofer-model-gateway is kept; delete it when you no longer need it."
}

do_preflight() {
    gateway_installed || return 0
    [[ -z $(secret_get gofer-secrets AI_TOKEN) ]] ||
        fail "the model gateway is installed but gofer-secrets holds AI_TOKEN, and gofer refuses to start with both. To change the gateway's key, run 'make model-gateway-apply AI_TOKEN=...'; then remove AI_TOKEN from gofer-secrets."
}

do_sync() {
    gateway_installed || return 0
    resolve_upstream
    route
}

do_check() {
    local port=${MODEL_GATEWAY_LOCAL_PORT:-18480} cfg pf code auth
    kube -n "$ns" rollout status deployment/gofer-model-gateway --timeout=10s >/dev/null ||
        fail "the gateway is not running (make model-gateway-apply)."
    cfg=$(config_get)
    [[ $(jq -r '.data.AI_GATEWAY_URL // empty' <<<"$cfg") == "$gw_url" ]] || fail "gofer-config AI_GATEWAY_URL is not $gw_url."
    jq -e --arg gw "$gw_url" '.data.AI_CONNECTIONS | fromjson | length > 0 and all(.[]; .endpoint | startswith($gw + "/"))' <<<"$cfg" >/dev/null ||
        fail "an AI_CONNECTIONS endpoint is not on the gateway."
    [[ -n $(jq -r --arg s "$saved_key" '.data[$s] // empty' <<<"$cfg") ]] || fail "gofer-config has no saved $saved_key, so model-gateway-down could not restore the provider endpoints."
    [[ -z $(secret_get gofer-secrets AI_TOKEN) ]] || fail "gofer-secrets still holds AI_TOKEN."
    [[ -n $(secret_get gofer-model-gateway AI_TOKEN) ]] || fail "the gateway has no upstream key."
    kube -n "$ns" port-forward svc/gofer-model-gateway "$port:80" >/dev/null 2>&1 &
    pf=$!
    trap 'kill $pf 2>/dev/null; rm -rf "$tmp"' EXIT
    for _ in 1 2 3 4 5 6 7 8 9 10; do curl -s -o /dev/null "http://127.0.0.1:$port/" && break; sleep 1; done
    for auth in "" "Authorization: Bearer forged.credential"; do
        code=$(curl -s -o /dev/null -w '%{http_code}' -H "Content-Type: application/json" ${auth:+-H "$auth"} \
            -d '{"model":"any","messages":[{"role":"user","content":"hi"}]}' "http://127.0.0.1:$port/v1/chat/completions")
        case $code in 401|403) ;; *) fail "the gateway answered $code to a caller with '${auth:-no credential}', want 401 or 403." ;; esac
    done
    echo "OK: the gateway holds the model key, gofer holds none, and callers without a run credential are refused."
}

case $cmd in
    apply) do_apply ;;
    down) do_down ;;
    preflight) do_preflight ;;
    sync) do_sync ;;
    check) do_check ;;
    *) fail "unknown command: $cmd" ;;
esac

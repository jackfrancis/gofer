#!/usr/bin/env bash
# The cluster half of gofer's optional model gateway (docs/adr/0008-model-gateway.md),
# behind `make model-gateway-apply | model-gateway-down | model-gateway-check` and
# make deploy's gateway steps. Keep kubectl and its cluster-selection flags as separate
# arguments; never eval a command string. The rules:
#
# - Every read is assigned before it is tested (v=$(...)), so a failed read stops the
#   script (set -e) instead of looking like "absent". Discovery uses
#   --ignore-not-found, so absent and unreadable stay distinct.
# - Every write is read back: a ConfigMap patch must leave every key it names at the
#   value written, and a Secret must hold the key just written.
# - The model key never appears in a process's arguments: it reaches jq on stdin and
#   kubectl in a file.
# - The steps run in an order that never loses the only copy of the key or of the
#   provider endpoints:
#
#   apply:   write the gateway Secret -> roll out the gateway -> point gofer-config at
#            it, saving the provider AI_CONNECTIONS -> only then remove AI_TOKEN from
#            gofer-secrets -> restart gofer.
#   down:    put the key back in gofer-secrets -> restore the saved AI_CONNECTIONS
#            verbatim and drop AI_GATEWAY_URL -> restart gofer -> delete the gateway ->
#            only then forget the saved AI_CONNECTIONS. Each step is skipped when it
#            is already done, so a down that stopped part-way can simply be re-run.
#   make deploy (never touches a key):
#     preflight  refuse a deploy that would hand gofer AI_TOKEN beside the gateway.
#     apply-base render deploy/k8s/base with IMAGE, then render and apply it from this
#                invocation's own temporary directory.
#     render     stdin -> stdout: the rendered base with gofer-config's AI_CONNECTIONS
#                set to its final value (the AI_CONNECTIONS override, if given; and
#                behind the gateway, the gateway endpoints built from the saved provider
#                connections, with AI_GATEWAY_URL), so one apply writes it whole.
#     sync       after the apply: confirm gofer-config routes every connection through
#                the gateway, before gofer restarts.
#   check:   read-only check of a deployed profile.
#   gateway-installed:  prints yes or no (a failed read stops it); dev-secrets uses it.
#   validate:  exits 0 when gofer, with the model enabled, would accept $AI_CONNECTIONS.
set -euo pipefail

cmd=${1:?usage: model-gateway.sh apply|down|preflight|render|apply-base|sync|check|gateway-installed kubectl [flags...]}
shift
[[ $# -gt 0 ]] || { echo "ERROR: pass kubectl and its cluster-selection flags." >&2; exit 1; }
kubectl=("$@")

ns=${KUBE_NS:-gofer}
gw_url=${MODEL_GATEWAY_URL:-http://gofer-model-gateway.$ns.svc.cluster.local}
default_upstream=https://api.githubcopilot.com
upstream_annotation=gofer/model-gateway-upstream
rollout_timeout=${ROLLOUT_TIMEOUT:-180s}
# The provider AI_CONNECTIONS, saved when gofer is pointed at the gateway and restored
# verbatim by down. It names the provider each model goes to, so neither a deploy nor
# a rollback ever guesses it.
saved_key=AI_CONNECTIONS_BEFORE_GATEWAY

fail() { echo "ERROR: $*" >&2; exit 1; }
kube() { "${kubectl[@]}" "$@"; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

# secret_get NAME KEY prints the base64 value, or nothing when the Secret or key is
# absent. A failed read stops the script.
secret_get() {
    local out
    out=$(kube -n "$ns" get secret "$1" --ignore-not-found -o json) || fail "could not read Secret $ns/$1."
    [[ -n $out ]] || return 0
    jq -r --arg k "$2" '.data[$k] // empty' <<<"$out"
}

# web_secret_get prints gofer-secrets as JSON, or nothing when it is absent. A failed
# read stops the script.
web_secret_get() {
    kube -n "$ns" get secret gofer-secrets --ignore-not-found -o json || fail "could not read Secret $ns/gofer-secrets."
}

config_get() {
    kube -n "$ns" get configmap gofer-config -o json || fail "could not read ConfigMap $ns/gofer-config."
}

# config_value JSON KEY prints one gofer-config value, or nothing.
config_value() { jq -r --arg k "$2" '.data[$k] // empty' <<<"$1"; }

# gateway_deployment prints the gateway Deployment as JSON, or nothing when it is not
# installed. A failed read stops the script.
gateway_deployment() {
    kube -n "$ns" get deployment gofer-model-gateway --ignore-not-found -o json ||
        fail "could not read Deployment $ns/gofer-model-gateway."
}

# patch_config FILE WHAT applies a JSON merge patch to gofer-config (null removes a
# key), then confirms every key the patch names now holds the value written.
patch_config() {
    local live
    kube -n "$ns" patch configmap gofer-config --type=merge --patch-file "$1" >/dev/null ||
        fail "could not update ConfigMap $ns/gofer-config ($2); nothing after this step ran."
    live=$(config_get)
    jq -e --slurpfile p "$1" '.data as $l | $p[0].data | to_entries |
        all(if .value == null then ($l[.key] == null) else $l[.key] == .value end)' <<<"$live" >/dev/null ||
        fail "ConfigMap $ns/gofer-config does not hold what was just written ($2); nothing after this step ran."
}

# jq_lib: definitions shared by every jq program here.
#   field(n)   a connection's value for field n as gofer reads it: names match
#              case-insensitively and the last match wins, as encoding/json does.
#   endpoint   a connection's effective endpoint (field "endpoint", trimmed, as gofer
#              trims it). Every check and rewrite reads the endpoint through this, so
#              the script routes exactly the endpoint gofer will use.
#   origin     a URL's scheme://host[:port], lowercased.
#   la, la_path, la_has_token   a Secret's kubectl.kubernetes.io/last-applied-configuration
#              annotation (client-side kubectl apply keeps the applied manifest there),
#              its JSON Patch path, and whether it still names a non-empty AI_TOKEN.
jq_lib='def trim: gsub("^\\s+|\\s+$"; "");
def field($n): [to_entries[] | select(.key | ascii_downcase == $n) | .value] | last;
def endpoint: ((if type == "object" then field("endpoint") else null end) // "") | if type == "string" then trim else "" end;
def origin: (capture("^(?<o>[A-Za-z][A-Za-z0-9+.-]*://[^/]+)").o // "") | ascii_downcase;
def la: .metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"]? // null;
def la_path: "/metadata/annotations/kubectl.kubernetes.io~1last-applied-configuration";
def la_has_token: la as $la | $la != null and ((($la | fromjson?) // {}) |
    type == "object" and (((.data | objects | .AI_TOKEN) // "") != "" or ((.stringData | objects | .AI_TOKEN) // "") != ""));
'

# form JSON prints "gateway" when every endpoint is on the gateway, "provider" when
# none is, and fails on a mix or a malformed list.
form() {
    jq -re --arg gw "$gw_url" "$jq_lib"'
        ($gw | origin) as $g | [.[] | endpoint | origin == $g] as $on |
        if type != "array" or length == 0 then error("not a non-empty list of connections")
        elif ($on | all) then "gateway" elif ($on | any) then error("it mixes gateway and provider endpoints")
        else "provider" end' <<<"$1" 2>/dev/null || fail "AI_CONNECTIONS is not usable here: $1"
}

# jq_valid mirrors how gofer reads AI_CONNECTIONS at startup with the model enabled
# (config.LoadConnections: encoding/json into []AIConnection, then its checks), so a
# config gofer would refuse is never written. hack/model-gateway's tests hold the two
# together on a corpus of cases. In order:
#   - exactly one JSON value; null or [] means no connections (refused);
#   - each element an object, or null (a zero connection);
#   - field names match case-insensitively, the last match winning, as encoding/json;
#   - endpoint a string or null; models a list or null of strings or nulls;
#   - endpoint non-blank, and at least one non-blank model.
jq_valid=$jq_lib'def valid:
    if length != 1 then error("AI_CONNECTIONS is not one JSON value") else .[0] end |
    if type == "null" then [] elif type != "array" then error("AI_CONNECTIONS is not a list of {endpoint, models}") else . end |
    if length == 0 then error("AI_CONNECTIONS has no connections") else . end |
    to_entries[] | .key as $i | .value |
    (if . == null then {} elif type != "object" then error("AI_CONNECTIONS[\($i)] is not an object") else . end) |
    field("endpoint") as $e | field("models") as $m |
    if ($e | type | . != "string" and . != "null") then error("AI_CONNECTIONS[\($i)]: endpoint is not a string")
    elif ($m | type | . != "array" and . != "null") then error("AI_CONNECTIONS[\($i)]: models is not a list")
    elif any($m[]?; type != "string" and type != "null") then error("AI_CONNECTIONS[\($i)]: every model must be a string")
    elif (($e // "") | trim) == "" then error("AI_CONNECTIONS[\($i)]: endpoint is required")
    elif ([$m[]? | strings | trim | select(length > 0)] | length) == 0 then error("AI_CONNECTIONS[\($i)] (\($e)): at least one model is required")
    else empty end;'

# validate CONNS fails unless gofer, with the model enabled, would accept CONNS at
# startup.
validate() {
    jq -se "$jq_valid"' [valid] | true' <<<"$1" >/dev/null ||
        fail "AI_CONNECTIONS would not start gofer (above); nothing was changed: $1"
}

# rewrite PROVIDER prints the gateway form of a provider AI_CONNECTIONS (compact
# JSON), or fails. Each connection is first normalized to exactly the two fields gofer
# reads, {endpoint, models}, with their effective values, so no other spelling of
# "endpoint" can survive to name the provider. Each endpoint must be exactly the
# upstream (origin and base path) plus an API the gateway serves, /chat/completions or
# /responses: that is the only place the gateway forwards to, so anything else would
# silently reach a different backend. The result is checked by gofer's gateway rule
# before it is printed.
rewrite() {
    local new
    validate "$1"
    new=$(jq -ce --arg gw "$gw_url" --arg up "$upstream" "$jq_lib"'
        def split_url: capture("^(?<o>[A-Za-z][A-Za-z0-9+.-]*://[^/?#]+)(?<p>[^?#]*)$") // error("\(.) is not a URL with no query") |
            {o: (.o | ascii_downcase), p: (.p | sub("/+$"; ""))};
        ($up | split_url) as $u |
        map({endpoint: endpoint, models: field("models")}) |
        map(.endpoint |= (
            (split_url) as $e |
            if $e.o != $u.o then error("AI_CONNECTIONS endpoint \(.) is not on the gateway upstream \($up)")
            elif $e.p == $u.p + "/chat/completions" then $gw + "/v1/chat/completions"
            elif $e.p == $u.p + "/responses" then $gw + "/v1/responses"
            else error("AI_CONNECTIONS endpoint \(.) is not \($up)/chat/completions or \($up)/responses, the only places the gateway forwards to") end))' \
        <<<"$1") || fail "could not route AI_CONNECTIONS through the gateway (above); nothing was changed."
    check_routed "$new"
    printf '%s' "$new"
}

# check_routed CONNS fails unless gofer, with AI_GATEWAY_URL set to the gateway, would
# start with CONNS (config.LoadConnections): valid, and every effective endpoint an
# http(s) URL without userinfo on the gateway's origin. It runs on every routed config
# before it is written, and on the live config before gofer restarts.
check_routed() {
    validate "$1"
    jq -e --arg gw "$gw_url" "$jq_lib"'
        def gorigin: (capture("^(?<s>[Hh][Tt][Tt][Pp][Ss]?)://(?<h>[^/?#]+)") // null) |
            if . == null or (.h | contains("@")) then null else (.s + "://" + .h | ascii_downcase) end;
        ($gw | gorigin) as $g | $g != null and all(.[]; (endpoint | gorigin) == $g)' <<<"$1" >/dev/null ||
        fail "AI_CONNECTIONS does not route every connection through the gateway ($gw_url), so gofer would refuse to start; nothing was changed: $1"
}

# resolve_upstream sets upstream: the provider the installed gateway fronts, else
# MODEL_GATEWAY_UPSTREAM, else GitHub Copilot. Asking an installed gateway for a
# different provider is refused: its key and the saved endpoints belong to the old one.
resolve_upstream() {
    local out installed requested=${MODEL_GATEWAY_UPSTREAM:-}
    out=$(gateway_deployment)
    installed=
    if [[ -n $out ]]; then
        installed=$(jq -r --arg a "$upstream_annotation" '.metadata.annotations[$a] // empty' <<<"$out")
    fi
    if [[ -n $installed && -n $requested && $requested != "$installed" ]]; then
        fail "the gateway is installed for $installed. To change the provider, run 'make model-gateway-down' first, then apply with MODEL_GATEWAY_UPSTREAM=$requested."
    fi
    upstream=${installed:-${requested:-$default_upstream}}
}

# plan_apply writes the routing patch for model-gateway-apply to FILE, or fails without
# writing anything. The provider connections are gofer-config's AI_CONNECTIONS when it
# still names the provider, else the saved ones.
plan_apply() {
    local cfg conns provider shape
    cfg=$(config_get)
    conns=$(config_value "$cfg" AI_CONNECTIONS)
    [[ -n $conns ]] || fail "gofer-config has no AI_CONNECTIONS to route through the gateway."
    shape=$(form "$conns")
    case $shape in
        provider) provider=$conns ;;
        gateway)
            provider=$(config_value "$cfg" "$saved_key")
            [[ -n $provider ]] || fail "AI_CONNECTIONS already points at the gateway but the provider connections ($saved_key) are not saved; restore AI_CONNECTIONS to the provider endpoints and re-run."
            ;;
    esac
    routing_patch "$provider" >"$1"
}

# routing_patch PROVIDER prints the merge patch that routes PROVIDER through the gateway.
routing_patch() {
    local new
    new=$(rewrite "$1")
    jq -n --arg c "$new" --arg gw "$gw_url" --arg s "$saved_key" --arg p "$1" \
        '{data: {AI_CONNECTIONS: $c, AI_GATEWAY_URL: $gw, ($s): $p}}'
}

# write_gateway_key B64 writes the gateway Secret and reads it back. The key reaches
# jq on stdin and kubectl in a file.
write_gateway_key() {
    printf '%s' "$1" | jq -R --arg ns "$ns" \
        '{apiVersion: "v1", kind: "Secret", metadata: {name: "gofer-model-gateway", namespace: $ns}, type: "Opaque", data: {AI_TOKEN: .}}' \
        >"$tmp/gateway-secret.json"
    kube -n "$ns" apply -f "$tmp/gateway-secret.json" >/dev/null ||
        fail "could not write Secret $ns/gofer-model-gateway; gofer-secrets is unchanged."
    local got
    got=$(secret_get gofer-model-gateway AI_TOKEN)
    [[ $got == "$1" ]] || fail "Secret $ns/gofer-model-gateway does not hold the key just written; gofer-secrets is unchanged."
}

restart_gofer() {
    kube -n "$ns" rollout restart statefulset/gofer
    kube -n "$ns" rollout status statefulset/gofer --timeout="$rollout_timeout"
}

do_apply() {
    : "${MODEL_GATEWAY_BURST:?}" "${MODEL_GATEWAY_REFILL:?}" "${MODEL_GATEWAY_INTERVAL:?}"
    local sts websecret web gw want ops after
    sts=$(kube -n "$ns" get statefulset gofer --ignore-not-found -o name) || fail "could not read StatefulSet $ns/gofer."
    [[ -n $sts ]] || fail "gofer is not deployed here; deploy it first."
    resolve_upstream
    # Fail on a bad AI_CONNECTIONS before anything is written.
    plan_apply "$tmp/routing.json"

    # gofer-secrets as read now: the removal at the end tests against this.
    websecret=$(web_secret_get)
    web=
    [[ -z $websecret ]] || web=$(jq -r '.data.AI_TOKEN // empty' <<<"$websecret")
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

    patch_config "$tmp/routing.json" "routing through the gateway"
    echo "model gateway: AI_CONNECTIONS = $(jq -r '.data.AI_CONNECTIONS' "$tmp/routing.json")"

    # Only now, with the key safe in the gateway and gofer's config pointing at it,
    # remove gofer's copies: the data field, and the copy client-side kubectl apply
    # keeps in the last-applied-configuration annotation (readable by anyone who can
    # read the Secret). The running web pod keeps its environment until restarted.
    # One JSON Patch tests each value read at the start and removes or rewrites it, so
    # a key rotated in, or a manifest re-applied, meanwhile is refused, never lost.
    if [[ -n $websecret ]]; then
        jq -c "$jq_lib"'
            (.data.AI_TOKEN? // null) as $tok | la as $la |
            (if $tok != null then [{op: "test", path: "/data/AI_TOKEN", value: $tok}, {op: "remove", path: "/data/AI_TOKEN"}] else [] end) +
            (if la_has_token then [{op: "test", path: la_path, value: $la},
                {op: "replace", path: la_path, value: ($la | fromjson | del(.data.AI_TOKEN, .stringData.AI_TOKEN) | tojson)}] else [] end)' \
            <<<"$websecret" >"$tmp/remove-key.json"
        ops=$(jq -r '[.[] | select(.op != "test") | .path] | join(" ")' "$tmp/remove-key.json")
        if [[ -n $ops ]]; then
            kube -n "$ns" patch secret gofer-secrets --type=json --patch-file "$tmp/remove-key.json" >/dev/null ||
                fail "could not remove AI_TOKEN from gofer-secrets: it changed since it was read at the start (key rotated, or the manifest re-applied, meanwhile?), or the write was refused. gofer was not restarted. Re-run make model-gateway-apply (with AI_TOKEN=... to install a new key)."
            after=$(web_secret_get)
            jq -e "$jq_lib"'(.data.AI_TOKEN // "") == "" and (la_has_token | not)' <<<"$after" >/dev/null ||
                fail "gofer-secrets still holds AI_TOKEN; gofer was not restarted. Remove it and restart gofer."
            echo "model gateway: removed AI_TOKEN from gofer-secrets (the gateway holds it)"
            if [[ $ops == *last-applied* ]]; then
                echo "model gateway: also removed it from gofer-secrets' last-applied configuration. Remove AI_TOKEN from the manifest you apply gofer-secrets from, too, or the next apply puts it back."
            fi
        fi
    fi
    restart_gofer
}

do_down() {
    local cfg saved before gwset conns dep key web shape
    cfg=$(config_get)
    saved=$(config_value "$cfg" "$saved_key")
    before=${MODEL_GATEWAY_RESTORE_CONNECTIONS:-$saved}
    gwset=$(config_value "$cfg" AI_GATEWAY_URL)
    conns=$(config_value "$cfg" AI_CONNECTIONS)
    dep=$(gateway_deployment)
    if [[ -z $before ]]; then
        if [[ -z $dep && -z $gwset ]]; then
            echo "model gateway: not installed; nothing to undo."
            return 0
        fi
        fail "gofer-config has no saved $saved_key, so the provider endpoints are unknown. Pass them as MODEL_GATEWAY_RESTORE_CONNECTIONS and re-run."
    fi
    # gofer gets its key back with these connections, so they must start it.
    validate "$before"
    shape=$(form "$before")
    [[ $shape == provider ]] || fail "the AI_CONNECTIONS to restore points at the gateway, not a provider: $before"

    # 1. The key back in gofer-secrets (skipped when it is already there).
    key=$(secret_get gofer-model-gateway AI_TOKEN)
    web=$(secret_get gofer-secrets AI_TOKEN)
    if [[ -n $key && $web != "$key" ]]; then
        printf '%s' "$key" | jq -R '{data: {AI_TOKEN: .}}' >"$tmp/key.json"
        kube -n "$ns" patch secret gofer-secrets --type=merge --patch-file "$tmp/key.json" >/dev/null ||
            fail "could not put AI_TOKEN back in gofer-secrets; nothing else was changed."
        web=$(secret_get gofer-secrets AI_TOKEN)
        [[ $web == "$key" ]] || fail "gofer-secrets does not hold the key just written; nothing else was changed."
    elif [[ -z $key && -z $web ]]; then
        fail "neither gofer-model-gateway nor gofer-secrets holds AI_TOKEN; nothing was changed."
    fi

    # 2. The provider endpoints, verbatim, and no gateway. The saved copy stays until
    #    the end, so a down that stops after this can be re-run.
    if [[ $conns != "$before" || -n $gwset ]]; then
        jq -n --arg c "$before" '{data: {AI_CONNECTIONS: $c, AI_GATEWAY_URL: null}}' >"$tmp/restore.json"
        patch_config "$tmp/restore.json" "restoring the provider endpoints; the gateway is still running"
    fi
    echo "AI_CONNECTIONS = $before"

    # 3. gofer on per-run vending, then the gateway gone, then the saved copy.
    restart_gofer
    if [[ -n $dep ]]; then
        kube kustomize deploy/k8s/model-gateway >"$tmp/gateway.yaml" || fail "could not render deploy/k8s/model-gateway."
        kube -n "$ns" delete --ignore-not-found -f "$tmp/gateway.yaml" >/dev/null ||
            fail "could not delete the gateway; gofer already runs without it. Re-run make model-gateway-down."
    fi
    if [[ -n $saved ]]; then
        jq -n --arg s "$saved_key" '{data: {($s): null}}' >"$tmp/forget.json"
        patch_config "$tmp/forget.json" "forgetting the saved provider endpoints; everything else is done"
    fi
    echo "model gateway removed. Secret $ns/gofer-model-gateway is kept; delete it when you no longer need it."
}

do_preflight() {
    local dep web cfg
    dep=$(gateway_deployment)
    [[ -n $dep ]] || return 0
    web=$(secret_get gofer-secrets AI_TOKEN)
    [[ -n $web ]] || return 0
    cfg=$(config_get)
    if [[ -z $(config_value "$cfg" AI_GATEWAY_URL) ]]; then
        fail "a 'make model-gateway-down' did not finish (gofer-secrets holds AI_TOKEN again and the gateway is still installed). Re-run it, then deploy."
    fi
    fail "the model gateway is installed but gofer-secrets holds AI_TOKEN, and gofer refuses to start with both. To change the gateway's key, run 'make model-gateway-apply AI_TOKEN=...'; then remove AI_TOKEN from gofer-secrets."
}

# deploy_provider prints the provider connections a deploy routes through the gateway:
# the AI_CONNECTIONS override if given, else the saved ones.
deploy_provider() {
    local cfg provider
    cfg=$(config_get)
    provider=${AI_CONNECTIONS:-$(config_value "$cfg" "$saved_key")}
    [[ -n $provider ]] || fail "the model gateway is installed but gofer-config has no saved provider connections ($saved_key). Pass them as AI_CONNECTIONS, or re-run make model-gateway-apply."
    printf '%s' "$provider"
}

# yaml_set FILE KEY=VALUE... replaces gofer-config's AI_CONNECTIONS line in the
# rendered manifests FILE with one line per KEY (values as YAML double-quoted
# scalars, which JSON strings are) and prints the result. It fails unless exactly one
# such line is found, in the gofer-config ConfigMap.
yaml_set() {
    local file=$1 lines= kv k v
    shift
    for kv in "$@"; do
        k=${kv%%=*}
        v=${kv#*=}
        lines+="  $k: $(printf '%s' "$v" | jq -Rs .)"$'\n'
    done
    REPLACEMENT=$lines awk '
        function flush() {
            if (doc != "") {
                if (has_cm && has_name && idx > 0) {
                    m = split(doc, l, "\n"); out = ""
                    for (i = 1; i < m; i++) out = out (i == idx ? ENVIRON["REPLACEMENT"] : l[i] "\n")
                    doc = out; n++
                } else if (idx > 0) bad++
                printf "%s", doc
            }
            doc = ""; has_cm = has_name = 0; idx = 0; line = 0
        }
        /^---$/ { flush(); print; next }
        {
            line++; doc = doc $0 "\n"
            if ($0 == "kind: ConfigMap") has_cm = 1
            if ($0 == "  name: gofer-config") has_name = 1
            if ($0 ~ /^  AI_CONNECTIONS: /) { if (idx > 0) bad++; idx = line }
        }
        END { flush(); if (n != 1 || bad) exit 3 }
    ' "$file" || fail "the rendered base has no single AI_CONNECTIONS line in the gofer-config ConfigMap to set."
}

do_render() {
    local dep provider new
    cat >"$tmp/base.yaml"
    dep=$(gateway_deployment)
    if [[ -z $dep ]]; then
        if [[ -n ${AI_CONNECTIONS:-} ]]; then
            # gofer reads AI_CONNECTIONS only with the model enabled (AI_TOKEN set);
            # with it off, any value (even []) is accepted and ignored.
            local web
            web=$(secret_get gofer-secrets AI_TOKEN)
            [[ -z $web ]] || validate "$AI_CONNECTIONS"
            yaml_set "$tmp/base.yaml" "AI_CONNECTIONS=$AI_CONNECTIONS"
        else
            cat "$tmp/base.yaml"
        fi
        return 0
    fi
    resolve_upstream
    provider=$(deploy_provider)
    new=$(rewrite "$provider")
    yaml_set "$tmp/base.yaml" "AI_CONNECTIONS=$new" "AI_GATEWAY_URL=$gw_url" "$saved_key=$provider"
}

# apply-base: make deploy's base apply. It renders deploy/k8s/base with IMAGE, sets
# gofer-config's AI_CONNECTIONS (render), and applies the result, all in this
# invocation's own temporary directory, so concurrent deploys never read each other's
# files. A failed render applies nothing.
do_apply_base() {
    : "${IMAGE:?IMAGE must be set}"
    kube kustomize deploy/k8s/base >"$tmp/kustomized.yaml" || fail "could not render deploy/k8s/base."
    IMAGE=$IMAGE awk '{
        s = "image: localhost/gofer:dev"; n = length($0) - length(s) + 1
        if (n > 0 && substr($0, n) == s) $0 = substr($0, 1, n - 1) "image: " ENVIRON["IMAGE"]
        print
    }' "$tmp/kustomized.yaml" >"$tmp/imaged.yaml"
    (do_render <"$tmp/imaged.yaml") >"$tmp/base.rendered.yaml"
    kube apply -f "$tmp/base.rendered.yaml"
}

do_sync() {
    local dep provider new cfg live
    dep=$(gateway_deployment)
    [[ -n $dep ]] || return 0
    resolve_upstream
    provider=$(deploy_provider)
    new=$(rewrite "$provider")
    cfg=$(config_get)
    live=$(jq -c --arg s "$saved_key" '.data | {c: (.AI_CONNECTIONS // "null" | fromjson? // null), g: .AI_GATEWAY_URL, p: (.[$s] // "null" | fromjson? // null)}' <<<"$cfg")
    jq -e --argjson l "$live" --argjson c "$new" --argjson p "$provider" --arg g "$gw_url" -n \
        '$l.c == $c and $l.g == $g and $l.p == $p' >/dev/null ||
        fail "gofer-config does not route every connection through the gateway (want AI_CONNECTIONS $new); gofer was not restarted. Re-run make deploy."
    echo "model gateway: gofer-config routes through the gateway"
}

do_check() {
    local port=${MODEL_GATEWAY_LOCAL_PORT:-18480} cfg pf code auth v
    kube -n "$ns" rollout status deployment/gofer-model-gateway --timeout=10s >/dev/null ||
        fail "the gateway is not running (make model-gateway-apply)."
    cfg=$(config_get)
    [[ $(config_value "$cfg" AI_GATEWAY_URL) == "$gw_url" ]] || fail "gofer-config AI_GATEWAY_URL is not $gw_url."
    check_routed "$(config_value "$cfg" AI_CONNECTIONS)"
    [[ -n $(config_value "$cfg" "$saved_key") ]] || fail "gofer-config has no saved $saved_key, so model-gateway-down could not restore the provider endpoints."
    v=$(secret_get gofer-secrets AI_TOKEN)
    [[ -z $v ]] || fail "gofer-secrets still holds AI_TOKEN."
    v=$(web_secret_get)
    [[ -z $v ]] || jq -e "$jq_lib"'la_has_token | not' <<<"$v" >/dev/null ||
        fail "gofer-secrets' last-applied configuration (kubectl.kubernetes.io/last-applied-configuration) still holds AI_TOKEN; re-run make model-gateway-apply, and remove it from the manifest you apply gofer-secrets from."
    v=$(secret_get gofer-model-gateway AI_TOKEN)
    [[ -n $v ]] || fail "the gateway has no upstream key."
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
    render) do_render ;;
    apply-base) do_apply_base ;;
    gateway-installed) dep=$(gateway_deployment); [[ -n $dep ]] && echo yes || echo no ;;
    validate) validate "${AI_CONNECTIONS:-}" ;;
    sync) do_sync ;;
    check) do_check ;;
    *) fail "unknown command: $cmd" ;;
esac

#!/usr/bin/env bash
# Own one bounded broker/container/network/fence. All request bytes are data.
# Install only after independent qualification; no main/vault parent is mounted.
set -Eeuo pipefail
umask 077
export PATH=/usr/sbin:/usr/bin:/sbin:/bin
readonly configuration=/etc/urnetwork/competition-review.json
readonly state_root=/var/lib/urnetwork/competition-review
readonly input_limit=4194304 output_limit=1048576 error_limit=65536
readonly invocation_limit_seconds=1800 cleanup_limit_seconds=20 command_limit_seconds=10
[[ "$(id -u)" == 0 ]] || exit 1
[[ $# == 0 ]] || { printf 'review broker accepts stdin only\n' >&2; exit 2; }
[[ -f "$configuration" && ! -L "$configuration" ]] || exit 1
[[ "$(stat -c %u "$configuration")" == 0 ]] || exit 1
permissions="$(stat -c %a "$configuration")"
(( (8#$permissions & 8#022) == 0 )) || exit 1
image="$(jq -er '.image_id' "$configuration")"
[[ "$image" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 1
provider_kind=legacy
provider_settings="" provider_settings_sha256=""
if jq -e '(keys|sort)==["image_id","model_key_file"]' "$configuration" >/dev/null; then
    key="$(jq -er '.model_key_file' "$configuration")"
    [[ "$key" == /var/lib/urnetwork/competition-review/model.key ]] || exit 1
    [[ -f "$key" && ! -L "$key" ]] || exit 1
    [[ "$(stat -c %a "$key")" == 400 ]] || exit 1
    [[ "$(stat -c %u /var/lib/urnetwork/competition-review)" == 0 ]] || exit 1
    [[ "$(stat -c %a /var/lib/urnetwork/competition-review)" == 700 ]] || exit 1
    [[ "$(realpath -e "$key")" == "$key" ]] || exit 1
    key_identity="$(stat -c %d:%i:%u:%g:%a:%s:%h "$key")"
    readonly key_identity
else
    jq -e '(keys|sort)==["image_id","provider_settings_file","provider_settings_sha256"]' "$configuration" >/dev/null
    provider_kind=snow
    provider_settings="$(jq -er '.provider_settings_file' "$configuration")"
    provider_settings_sha256="$(jq -er '.provider_settings_sha256' "$configuration")"
    [[ "$provider_settings" == /var/lib/urnetwork/competition-review/snow-provider.json && "$provider_settings_sha256" =~ ^[0-9a-f]{64}$ ]]
    [[ -f "$provider_settings" && ! -L "$provider_settings" && "$(realpath -e "$provider_settings")" == "$provider_settings" ]]
    [[ "$(stat -c %u:%g:%a:%h "$provider_settings")" == 0:0:444:1 ]]
    (( $(stat -c %s "$provider_settings") > 0 && $(stat -c %s "$provider_settings") <= input_limit ))
    current="$provider_settings"
    while :; do
        [[ ! -L "$current" && "$(stat -c %u "$current")" == 0 ]]
        permissions="$(stat -c %a "$current")"
        (( (8#$permissions & 8#022) == 0 ))
        [[ "$current" != / ]] || break
        current="$(dirname "$current")"
        [[ -d "$current" ]]
    done
    [[ "$(sha256sum "$provider_settings" | cut -d ' ' -f1)" == "$provider_settings_sha256" ]]
    jq -e '(keys|sort)==["endpoint","kind","members","schema"] and .schema==1 and .kind=="snow-review-provider" and
        .endpoint=="http://192.168.1.162:8000/v1/responses" and (.members|length)==3 and
        [.members[].name]==["qwen","gemma","muse"] and
        [.members[].model]==["qwen3.8-27b","gemma-4-26b-a4b","muse-glimmer-30b"] and
        all(.members[]; (keys|sort)==["model","name","reasoning","revision"] and (.revision|type)=="string" and (.revision|test("^[0-9a-f]{40}$")) and
            (.reasoning|type)=="string" and (.reasoning|length)>0)' "$provider_settings" >/dev/null
    provider_settings_identity="$(stat -c %d:%i:%u:%g:%a:%s:%h "$provider_settings")"
    readonly provider_settings_identity
fi
readonly provider_kind provider_settings provider_settings_sha256
[[ "$(stat -c %u:%a "$state_root")" == 0:700 ]] || exit 1
[[ "$(awk '/Cpus_allowed_list/ {print $2}' /proc/self/status)" == 20,22 ]] || exit 1
readonly docker=(/usr/bin/docker --host=unix:///var/run/docker.sock)
bounded() { timeout --signal=TERM --kill-after=1s "${command_limit_seconds}s" "$@"; }
[[ "$(bounded "${docker[@]}" image inspect --format '{{.Id}}' "$image")" == "$image" ]] || exit 1
bounded /usr/sbin/iptables --wait 1 --list DOCKER-USER >/dev/null
[[ ! -L "$state_root/attempts" && ! -L "$state_root/broker.lock" ]] || exit 1
if [[ ! -d "$state_root/attempts" ]]; then mkdir -m 0700 "$state_root/attempts"; fi
[[ "$(stat -c %u:%a "$state_root/attempts")" == 0:700 ]] || exit 1
exec 8<>"$state_root/broker.lock"
flock -n 8
attempt_directory="$(mktemp -d "$state_root/attempts/invocation.XXXXXXXX")"
member_state="$attempt_directory/member-state"
if [[ "$provider_kind" == snow ]]; then mkdir -m 0700 "$member_state"; fi
owner="$(openssl rand -hex 32)"
[[ "$owner" =~ ^[0-9a-f]{64}$ ]] || exit 1
container_name="urnetwork-review-$owner"
network_name="urnetwork-review-$owner"
chain="URRV_${owner:0:16}"
comment="urnetwork-review-$owner"
container_id="" network_id="" subnet="" active_pid="" input_pid="" stdout_pid="" stderr_pid=""
network_may_exist=false container_may_exist=false chain_created=false forward_added=false input_added=false
broker_status=1
broker_exit_observed=false
provider_input_released=false

# Cleanup never trusts a name alone, including lost create acknowledgments.
# A residual network keeps its deny fence; unrelated chains are never flushed.
finish() {
    local status=$? cleanup_verified=true network_removed=true
    trap - EXIT INT TERM
    set +e
    local cleanup_deadline=$((SECONDS+cleanup_limit_seconds))
    cleanup_command() {
        local remaining=$((cleanup_deadline-SECONDS))
        ((remaining>0)) || return 1
        local bound=$((remaining<3?remaining:3))
        timeout --signal=TERM --kill-after=1s "${bound}s" "$@"
    }
    # review_broker_stop_program_begin
    # Fence egress before pipe teardown can release a blocked stdin reader.
    if $chain_created; then
        cleanup_command /usr/sbin/iptables --wait 1 --insert "$chain" 1 --jump DROP || cleanup_verified=false
    fi
    if $container_may_exist; then
        if [[ -z "$container_id" ]]; then container_id="$(cleanup_command "${docker[@]}" inspect --format '{{.Id}}' "$container_name")"; fi
        if [[ "$container_id" =~ ^[0-9a-f]{64}$ ]] &&
           [[ "$(cleanup_command "${docker[@]}" inspect --format '{{index .Config.Labels "com.urnetwork.review.owner"}}' "$container_id")" == "$owner" ]]; then
            cleanup_command "${docker[@]}" rm --force --volumes "$container_id" >/dev/null || cleanup_verified=false
            residual="$(cleanup_command "${docker[@]}" container ls --all --no-trunc --quiet --filter "id=$container_id")"
            [[ $? == 0 && -z "$residual" ]] || cleanup_verified=false
        else cleanup_verified=false; fi
    fi
    # Stop the owned broker before closing its attach/input processes. Closing
    # those first could release EOF and begin provider work during cleanup.
    if [[ -n "$active_pid" ]]; then kill -TERM "$active_pid" 2>/dev/null; fi
    if [[ -n "$input_pid" ]]; then kill -TERM "$input_pid" 2>/dev/null; fi
    # review_broker_stop_program_end
    for reader_pid in "$stdout_pid" "$stderr_pid"; do
        [[ -z "$reader_pid" ]] || { kill -TERM "$reader_pid" 2>/dev/null; wait "$reader_pid" 2>/dev/null; }
    done
    if $network_may_exist; then
        if [[ -z "$network_id" ]]; then network_id="$(cleanup_command "${docker[@]}" network inspect --format '{{.Id}}' "$network_name")"; fi
        if [[ "$network_id" =~ ^[0-9a-f]{64}$ ]] &&
           [[ "$(cleanup_command "${docker[@]}" network inspect --format '{{index .Labels "com.urnetwork.review.owner"}}' "$network_id")" == "$owner" ]] &&
           [[ "$(cleanup_command "${docker[@]}" network inspect --format '{{len .Containers}}' "$network_id")" == 0 ]]; then
            cleanup_command "${docker[@]}" network rm "$network_id" >/dev/null || { cleanup_verified=false; network_removed=false; }
            residual="$(cleanup_command "${docker[@]}" network ls --quiet --no-trunc --filter "id=$network_id")"
            [[ $? == 0 && -z "$residual" ]] || { cleanup_verified=false; network_removed=false; }
        else cleanup_verified=false; network_removed=false; fi
    fi
    if $network_removed; then
        if $input_added; then cleanup_command /usr/sbin/iptables --wait 1 --delete INPUT --source "$subnet" --match comment --comment "$comment" --jump DROP || cleanup_verified=false; fi
        if $forward_added; then cleanup_command /usr/sbin/iptables --wait 1 --delete DOCKER-USER --source "$subnet" --match comment --comment "$comment" --jump "$chain" || cleanup_verified=false; fi
        if $chain_created; then
            cleanup_command /usr/sbin/iptables --wait 1 --flush "$chain" || cleanup_verified=false
            cleanup_command /usr/sbin/iptables --wait 1 --delete-chain "$chain" || cleanup_verified=false
        fi
    fi
    [[ -z "$active_pid" ]] || wait "$active_pid" 2>/dev/null
    [[ -z "$input_pid" ]] || wait "$input_pid" 2>/dev/null
    exec 7>&-
    local output_bytes=0 error_bytes=0 evidence_verified=true
    [[ ! -f "$attempt_directory/broker.stdout" ]] || output_bytes="$(stat -c %s "$attempt_directory/broker.stdout")"
    [[ ! -f "$attempt_directory/broker.stderr" ]] || error_bytes="$(stat -c %s "$attempt_directory/broker.stderr")"
    ((output_bytes<=output_limit && error_bytes<=error_limit)) || status=1
    $cleanup_verified || status=1
    if [[ "$provider_kind" == snow && -n "${mapped_uid:-}" ]] && $cleanup_verified; then
        shopt -s nullglob dotglob
        state_files=("$member_state"/*)
        ((${#state_files[@]}<=6)) || evidence_verified=false
        for source in "${state_files[@]}"; do
            name="${source##*/}"
            if [[ ! "$name" =~ ^member-0[123]\.(started|complete)\.json$ || -L "$source" || ! -f "$source" ]]; then evidence_verified=false; continue; fi
            [[ "$(stat -c %u:%a:%h "$source")" == "$mapped_uid:600:1" ]] || { evidence_verified=false; continue; }
            (($(stat -c %s "$source")<=1048576)) || { evidence_verified=false; continue; }
            head -c 1048577 "$source" > "$attempt_directory/$name" || evidence_verified=false
            [[ "$(sha256sum "$source" | cut -d ' ' -f1)" == "$(sha256sum "$attempt_directory/$name" | cut -d ' ' -f1)" ]] || evidence_verified=false
        done
    fi
    $evidence_verified || status=1
    jq -n --argjson status "$status" --argjson broker_status "$broker_status" --argjson observed "$broker_exit_observed" --argjson cleanup "$cleanup_verified" --argjson released "$provider_input_released" \
        --arg owner "$owner" --arg image "$image" --arg container "$container_id" --arg network "$network_id" \
        '{schema:1,kind:"winner-review-container-terminal",owner:$owner,image_id:$image,container_id:$container,
          network_id:$network,wrapper_exit:$status,broker_exit:(if $observed then $broker_status else null end),broker_exit_observed:$observed,cleanup_verified:$cleanup,provider_input_released:$released}' > "$attempt_directory/terminal.json" || evidence_verified=false
    (
        set -Eeuo pipefail
        cd "$attempt_directory" || exit 1
        for file in input.json image-inspect.json network-inspect.json container-inspect.json container-state.json cgroup.json uid-map.txt key-ownership.json provider-settings.json provider-settings-ownership.json member-state-ownership.json attempt-claim.json member-01.started.json member-01.complete.json member-02.started.json member-02.complete.json member-03.started.json member-03.complete.json provider-address.txt fence.json fence-forward.txt fence-input.txt fence-chain.txt broker.stdout broker.stderr terminal.json; do
            [[ ! -f "$file" ]] || jq -cn --arg path "$file" --arg sha256 "$(sha256sum "$file" | cut -d ' ' -f1)" --argjson bytes "$(stat -c %s "$file")" '{path:$path,sha256:$sha256,bytes:$bytes}'
        done | jq -s '{schema:1,kind:"winner-review-container-evidence",artifacts:.}'
    ) > "$attempt_directory/evidence-manifest.json" || evidence_verified=false
    sync -f "$attempt_directory" || evidence_verified=false
    $evidence_verified || status=1
    jq -cn --arg owner "$owner" --arg directory "$attempt_directory" --arg manifest "$(sha256sum "$attempt_directory/evidence-manifest.json" | cut -d ' ' -f1)" \
        --argjson status "$status" --argjson cleanup "$cleanup_verified" \
        '{schema:1,kind:"winner-review-container-receipt",owner:$owner,attempt_directory:$directory,evidence_manifest_sha256:$manifest,wrapper_exit:$status,cleanup_verified:$cleanup}' >&2
    if $cleanup_verified && ((output_bytes>0 && output_bytes<=output_limit)); then
        if ((status==0)) || jq -e '.schema==1 and .kind=="review_provider_refusal" and .provider_refusal.http_status>=300' "$attempt_directory/broker.stdout" >/dev/null 2>&1; then
            cat "$attempt_directory/broker.stdout"
        fi
    fi
    exit "$status"
}
trap finish EXIT
trap 'exit 143' TERM
trap 'exit 130' INT

# The parent sends a finite bounded JSON document; capture it before any model
# request. No key bytes are read by this wrapper or included in its receipts.
# Explicit stdin redirection prevents non-interactive Bash from substituting
# /dev/null for this asynchronous reader.
# review_input_capture_program_begin
head -c "$((input_limit+1))" <&0 > "$attempt_directory/input.json" &
active_pid=$!
wait "$active_pid"
active_pid=""
(( $(stat -c %s "$attempt_directory/input.json") <= input_limit ))
# review_input_capture_program_end
deadline="$(jq -er '.request.deadline | strings' "$attempt_directory/input.json")"
deadline_epoch="$(date -u -d "$deadline" +%s)"
remaining=$((deadline_epoch-$(date -u +%s)))
((remaining>0 && remaining<=invocation_limit_seconds))

# Historical HTTPS resolves once on the host. Snow is one fixed unauthenticated
# LAN HTTP frontend: no DNS, TLS, key or public-provider assertion is implied.
provider_ip=192.168.1.162 provider_host=192.168.1.162 provider_port=8000
provider_network_arguments=()
provider_command_arguments=("--review-provider-settings=/run/review/provider.json" "--review-provider-settings-sha256=$provider_settings_sha256" "--review-state-directory=/run/review/state")
provider_state_arguments=(--ulimit fsize=1048576:1048576 --mount "type=bind,source=$member_state,target=/run/review/state")
provider_entrypoint='["/review-broker","review-broker"]'
provider_extra_hosts='[]'
provider_mount_source="$provider_settings" provider_mount_target=/run/review/provider.json
if [[ "$provider_kind" == legacy ]]; then
    resolution="$(bounded getent ahostsv4 api.openai.com)"
    provider_ip="$(awk '{print $1}' <<<"$resolution" | sort -u | head -n 1)"
    provider_host=api.openai.com provider_port=443
    provider_network_arguments=(--add-host "api.openai.com:$provider_ip")
    provider_command_arguments=()
    provider_state_arguments=()
    provider_entrypoint='["/review-broker","review-broker","--model-key-file=/run/secrets/model-key"]'
    provider_extra_hosts="$(jq -cn --arg host "api.openai.com:$provider_ip" '[$host]')"
    provider_mount_source="$key" provider_mount_target=/run/secrets/model-key
fi
public_ipv4() {
    local value="$1" a b c d
    [[ "$value" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    IFS=. read -r a b c d <<<"$value"
    a=$((10#$a)); b=$((10#$b)); c=$((10#$c)); d=$((10#$d))
    ((a>0 && a<224 && a!=10 && a!=127 && b<256 && c<256 && d<256)) || return 1
    ! (( (a==100 && b>=64 && b<=127) || (a==169 && b==254) || (a==172 && b>=16 && b<=31) ||
         (a==192 && (b==168 || b==0 || (b==88 && c==99))) || (a==198 && (b==18 || b==19 || (b==51 && c==100))) || (a==203 && b==0 && c==113) ))
}
if [[ "$provider_kind" == legacy ]]; then public_ipv4 "$provider_ip"; fi
printf '%s\n' "$provider_ip" > "$attempt_directory/provider-address.txt"
bounded "${docker[@]}" image inspect "$image" > "$attempt_directory/image-inspect.json"
network_may_exist=true
network_id="$(bounded "${docker[@]}" network create --driver bridge --ipv6=false --opt com.docker.network.bridge.enable_icc=false --label "com.urnetwork.review.owner=$owner" "$network_name")"
[[ "$network_id" =~ ^[0-9a-f]{64}$ ]]
bounded "${docker[@]}" network inspect "$network_id" > "$attempt_directory/network-inspect.json"
jq -e --arg owner "$owner" --arg id "$network_id" 'length==1 and .[0].Id==$id and .[0].Driver=="bridge" and .[0].Scope=="local" and .[0].Internal==false and .[0].EnableIPv6==false and .[0].Attachable==false and .[0].Labels["com.urnetwork.review.owner"]==$owner and (.[0].IPAM.Config|length)==1 and .[0].Options["com.docker.network.bridge.enable_icc"]=="false"' "$attempt_directory/network-inspect.json" >/dev/null
subnet="$(jq -er '.[0].IPAM.Config[0].Subnet' "$attempt_directory/network-inspect.json")"
[[ "$subnet" =~ ^(10\.|172\.(1[6-9]|2[0-9]|3[01])\.|192\.168\.)[0-9.]+/(1[6-9]|2[0-9]|30)$ ]]

# Install source-scoped host-input denial and the sole provider destination before
# creating/starting a child. These are actual kernel rules, not app claims.
bounded /usr/sbin/iptables --wait 1 --new-chain "$chain"
chain_created=true
bounded /usr/sbin/iptables --wait 1 --append "$chain" --destination "$provider_ip" --protocol tcp --dport "$provider_port" --jump ACCEPT
bounded /usr/sbin/iptables --wait 1 --append "$chain" --jump DROP
bounded /usr/sbin/iptables --wait 1 --insert DOCKER-USER 1 --source "$subnet" --match comment --comment "$comment" --jump "$chain"
forward_added=true
bounded /usr/sbin/iptables --wait 1 --insert INPUT 1 --source "$subnet" --match comment --comment "$comment" --jump DROP
input_added=true
bounded /usr/sbin/iptables --wait 1 --check "$chain" --destination "$provider_ip" --protocol tcp --dport "$provider_port" --jump ACCEPT
bounded /usr/sbin/iptables --wait 1 --check "$chain" --jump DROP
bounded /usr/sbin/iptables --wait 1 --list-rules DOCKER-USER > "$attempt_directory/fence-forward.txt"
bounded /usr/sbin/iptables --wait 1 --list-rules INPUT > "$attempt_directory/fence-input.txt"
bounded /usr/sbin/iptables --wait 1 --list-rules "$chain" > "$attempt_directory/fence-chain.txt"
[[ "$(awk '$1=="-A" {print;exit}' "$attempt_directory/fence-forward.txt" | tr -d '"')" == "-A DOCKER-USER -s $subnet -m comment --comment $comment -j $chain" ]]
[[ "$(awk '$1=="-A" {print;exit}' "$attempt_directory/fence-input.txt" | tr -d '"')" == "-A INPUT -s $subnet -m comment --comment $comment -j DROP" ]]
[[ "$(awk '$1=="-A" {n++} END {print n+0}' "$attempt_directory/fence-chain.txt")" == 2 ]]
jq -n --arg owner "$owner" --arg chain "$chain" --arg subnet "$subnet" --arg ip "$provider_ip" --arg host "$provider_host" --argjson port "$provider_port" '{schema:1,kind:"winner-review-provider-egress-fence",owner:$owner,chain:$chain,source_subnet:$subnet,provider_host:$host,provider_ipv4:$ip,provider_port:$port,external_dns:false,host_input_denied:true,default_deny:true,installed_before_start:true}' > "$attempt_directory/fence.json"

container_may_exist=true
container_id="$(bounded "${docker[@]}" create --interactive --init --pull=never --name "$container_name" --label "com.urnetwork.review.owner=$owner" \
    --network "$network_id" --dns 127.0.0.1 "${provider_network_arguments[@]}" --read-only --user 65532:65532 --cap-drop ALL \
    --security-opt no-new-privileges:true --cgroupns private --sysctl net.ipv6.conf.all.disable_ipv6=1 --sysctl net.ipv6.conf.default.disable_ipv6=1 \
    --pids-limit 64 --memory 2g --memory-swap 2g --cpus 1 --cpuset-cpus 20,22 --ulimit nofile=1024:1024 --ulimit core=0:0 \
    --mount "type=bind,source=$provider_mount_source,target=$provider_mount_target,readonly" "${provider_state_arguments[@]}" "$image" "${provider_command_arguments[@]}")"
[[ "$container_id" =~ ^[0-9a-f]{64}$ ]]
bounded "${docker[@]}" inspect "$container_id" > "$attempt_directory/container-inspect.json"
provider_command_json="$(jq -cn --args '$ARGS.positional' -- "${provider_command_arguments[@]}")"
jq -e --arg id "$container_id" --arg image "$image" --arg owner "$owner" --arg network "$network_id" --arg leaf "$provider_mount_source" --arg target "$provider_mount_target" --arg state "$member_state" --argjson snow "$([[ "$provider_kind" == snow ]] && printf true || printf false)" --argjson hosts "$provider_extra_hosts" --argjson entrypoint "$provider_entrypoint" --argjson command "$provider_command_json" '
 length==1 and .[0].Id==$id and .[0].Image==$image and .[0].Config.User=="65532:65532" and
 .[0].Config.Entrypoint==$entrypoint and
 (.[0].Config.Cmd//[])==$command and .[0].Config.Labels["com.urnetwork.review.owner"]==$owner and
 .[0].HostConfig.NetworkMode==$network and .[0].HostConfig.ReadonlyRootfs==true and .[0].HostConfig.Privileged==false and
 .[0].HostConfig.CgroupnsMode=="private" and .[0].HostConfig.CpusetCpus=="20,22" and .[0].HostConfig.NanoCpus==1000000000 and
 .[0].HostConfig.Memory==2147483648 and .[0].HostConfig.MemorySwap==2147483648 and .[0].HostConfig.PidsLimit==64 and
 .[0].HostConfig.CapDrop==["ALL"] and ((.[0].HostConfig.CapAdd//[])|length)==0 and .[0].HostConfig.Init==true and
 (.[0].HostConfig.SecurityOpt|index("no-new-privileges:true")!=null) and .[0].HostConfig.Dns==["127.0.0.1"] and
 (.[0].HostConfig.ExtraHosts//[])==$hosts and .[0].HostConfig.Sysctls["net.ipv6.conf.all.disable_ipv6"]=="1" and
 .[0].HostConfig.Sysctls["net.ipv6.conf.default.disable_ipv6"]=="1" and
 (.[0].Mounts|length)==(if $snow then 2 else 1 end) and
 any(.[0].Mounts[]; .Type=="bind" and .Source==$leaf and .Destination==$target and .RW==false) and
 (if $snow then
    any(.[0].Mounts[]; .Type=="bind" and .Source==$state and .Destination=="/run/review/state" and .RW==true) and
    any(.[0].HostConfig.Ulimits[]; .Name=="fsize" and .Soft==1048576 and .Hard==1048576)
  else true end) and
 .[0].HostConfig.PidMode=="" and .[0].HostConfig.IpcMode=="private" and .[0].HostConfig.UTSMode=="" and
 ((.[0].HostConfig.Devices//[])|length)==0 and ((.[0].HostConfig.DeviceRequests//[])|length)==0 and
 ((.[0].HostConfig.Binds//[])|length)==0 and ((.[0].HostConfig.VolumesFrom//[])|length)==0 and
 ((.[0].HostConfig.PortBindings//{})|length)==0 and ((.[0].HostConfig.Tmpfs//{})|length)==0 and
 all((.[0].Config.Env//[])[]; type=="string" and test("^(PATH|HOSTNAME|HOME)="))' "$attempt_directory/container-inspect.json" >/dev/null
mkfifo -m 0600 "$attempt_directory/stdin.pipe" "$attempt_directory/stdout.pipe" "$attempt_directory/stderr.pipe"
exec 7<>"$attempt_directory/stdin.pipe"
# review_output_capture_program_begin
{ head -c "$((output_limit+1))"; cat >/dev/null; } < "$attempt_directory/stdout.pipe" > "$attempt_directory/broker.stdout" 7>&- & stdout_pid=$!
{ head -c "$((error_limit+1))"; cat >/dev/null; } < "$attempt_directory/stderr.pipe" > "$attempt_directory/broker.stderr" 7>&- & stderr_pid=$!
# review_output_capture_program_end
remaining=$((deadline_epoch-$(date -u +%s)))
((remaining>0))
timeout --signal=TERM --kill-after=5s "${remaining}s" "${docker[@]}" start --attach --interactive "$container_id" < "$attempt_directory/stdin.pipe" 7>&- > "$attempt_directory/stdout.pipe" 2> "$attempt_directory/stderr.pipe" &
active_pid=$!
# No input/EOF reaches the actual broker until its running cgroup is observed.
# Its ReadAll(stdin) is the barrier; no model request is made before this proof.
ready_deadline=$((SECONDS+command_limit_seconds))
while true; do
    running="$(bounded "${docker[@]}" inspect --format '{{.State.Running}} {{.State.Pid}}' "$container_id")"
    if [[ "$running" =~ ^true\ ([1-9][0-9]*)$ ]]; then broker_pid="${BASH_REMATCH[1]}"; break; fi
    ((SECONDS<ready_deadline))
    sleep 0.05
done
cgroup_relative="$(awk -F: '$1=="0" {print $3}' "/proc/$broker_pid/cgroup")"
[[ "$cgroup_relative" == /* && "$cgroup_relative" != *..* ]]
cgroup="/sys/fs/cgroup$cgroup_relative"
memory_max="$(<"$cgroup/memory.max")"; swap_max="$(<"$cgroup/memory.swap.max")"
pids_max="$(<"$cgroup/pids.max")"; cpu_max="$(<"$cgroup/cpu.max")"; cpu_set="$(<"$cgroup/cpuset.cpus.effective")"
[[ "$memory_max" == 2147483648 && "$swap_max" == 0 && "$pids_max" == 64 && "$cpu_max" == '100000 100000' && "$cpu_set" == 20,22 ]]
jq -n --arg path "$cgroup_relative" --arg memory "$memory_max" --arg swap "$swap_max" --arg pids "$pids_max" --arg cpu "$cpu_max" --arg cpuset "$cpu_set" \
    --argjson pid "$broker_pid" \
    '{schema:1,kind:"winner-review-running-cgroup",path:$path,container_pid:$pid,memory_max:$memory,memory_swap_max:$swap,pids_max:$pids,cpu_max:$cpu,cpuset_cpus:$cpuset,observed_before_provider_input:true}' > "$attempt_directory/cgroup.json"
# Read the running child mapping, not a daemon-name guess or host UID literal.
# The root-private parent denies host traversal; only the bind-mounted leaf is
# readable by its mapped container owner. Never read or save key contents.
head -c 16385 "/proc/$broker_pid/uid_map" > "$attempt_directory/uid-map.txt"
(( $(stat -c %s "$attempt_directory/uid-map.txt") <= 16384 ))
# review_uid_map_program_begin
readonly review_uid_map_program='
BEGIN { limit=4294967295; target=65532; count=0; found=0; bad=0 }
$0~/[^0-9 \t]/ { bad=1; exit 1 }
NF!=3 || $1!~/^(0|[1-9][0-9]*)$/ || $2!~/^(0|[1-9][0-9]*)$/ || $3!~/^(0|[1-9][0-9]*)$/ { bad=1; exit 1 }
{
    start=$1+0; host=$2+0; span=$3+0;
    if (start>=limit || host>=limit || span<=0 || span>limit || start+span>limit || host+span>limit || count>=340) { bad=1; exit 1 }
    for (i=0;i<count;i++) {
        if ((start<ends[i] && starts[i]<start+span) || (host<hostends[i] && hosts[i]<host+span)) { bad=1; exit 1 }
    }
    starts[count]=start; ends[count]=start+span; hosts[count]=host; hostends[count]=host+span; count++;
    if (start<=target && target<start+span) { mapped=host+target-start; found++ }
}
END { if (bad || count==0 || found!=1) exit 1; printf "%.0f\n",mapped }
'
# review_uid_map_program_end
mapped_uid="$(awk "$review_uid_map_program" "$attempt_directory/uid-map.txt")"
[[ "$mapped_uid" =~ ^[0-9]+$ ]]
if [[ "$provider_kind" == legacy ]]; then
[[ ! -L "$key" && -f "$key" && "$(stat -c %d:%i:%u:%g:%a:%s:%h "$key")" == "$key_identity" ]]
IFS=: read -r key_device key_inode key_uid key_gid key_mode key_bytes key_links <<<"$key_identity"
jq -n --arg owner "$owner" --arg container "$container_id" --argjson pid "$broker_pid" --arg path "$key" \
    --arg map_sha256 "$(sha256sum "$attempt_directory/uid-map.txt" | cut -d ' ' -f1)" \
    --argjson host_uid "$key_uid" --argjson host_gid "$key_gid" --argjson mapped_uid "$mapped_uid" \
    --arg device "$key_device" --arg inode "$key_inode" --argjson bytes "$key_bytes" --argjson links "$key_links" \
    '{schema:1,kind:"winner-review-key-ownership",owner:$owner,container_id:$container,container_pid:$pid,
      path:$path,container_uid:65532,mapped_host_uid:$mapped_uid,host_uid:$host_uid,host_gid:$host_gid,
      mode:"0400",bytes:$bytes,links:$links,device:$device,inode:$inode,uid_map_sha256:$map_sha256,
      parent_owner_uid:0,parent_mode:"0700",observed_before_provider_input:true}' > "$attempt_directory/key-ownership.json"
[[ "$key_mode" == 400 && "$key_uid" == "$mapped_uid" && "$key_links" == 1 && "$mapped_uid" != 0 ]]
((key_bytes>0 && key_bytes<=4096))
else
    [[ "$mapped_uid" != 0 && ! -L "$provider_settings" && -f "$provider_settings" ]]
    [[ "$(stat -c %d:%i:%u:%g:%a:%s:%h "$provider_settings")" == "$provider_settings_identity" ]]
    head -c "$((input_limit+1))" "$provider_settings" > "$attempt_directory/provider-settings.json"
    [[ "$(sha256sum "$attempt_directory/provider-settings.json" | cut -d ' ' -f1)" == "$provider_settings_sha256" ]]
    IFS=: read -r settings_device settings_inode settings_uid settings_gid settings_mode settings_bytes settings_links <<<"$provider_settings_identity"
    jq -n --arg owner "$owner" --arg container "$container_id" --argjson pid "$broker_pid" --arg path "$provider_settings" \
        --arg map_sha256 "$(sha256sum "$attempt_directory/uid-map.txt" | cut -d ' ' -f1)" --arg settings_sha256 "$provider_settings_sha256" \
        --argjson mapped_uid "$mapped_uid" --arg device "$settings_device" --arg inode "$settings_inode" --argjson bytes "$settings_bytes" \
        '{schema:1,kind:"winner-review-provider-settings-ownership",owner:$owner,container_id:$container,container_pid:$pid,
          path:$path,container_uid:65532,mapped_host_uid:$mapped_uid,host_uid:0,host_gid:0,mode:"0444",bytes:$bytes,links:1,
          device:$device,inode:$inode,uid_map_sha256:$map_sha256,provider_settings_sha256:$settings_sha256,
          observed_before_provider_input:true}' > "$attempt_directory/provider-settings-ownership.json"
    chown "$mapped_uid":0 "$member_state"
    [[ ! -L "$member_state" && -d "$member_state" && "$(stat -c %u:%g:%a "$member_state")" == "$mapped_uid:0:700" ]]
    jq -n --arg owner "$owner" --arg container "$container_id" --argjson pid "$broker_pid" --arg path "$member_state" \
        --arg map_sha256 "$(sha256sum "$attempt_directory/uid-map.txt" | cut -d ' ' -f1)" --argjson mapped_uid "$mapped_uid" \
        --arg device "$(stat -c %d "$member_state")" --arg inode "$(stat -c %i "$member_state")" \
        '{schema:1,kind:"winner-review-member-state-ownership",owner:$owner,container_id:$container,container_pid:$pid,
          path:$path,container_uid:65532,mapped_host_uid:$mapped_uid,host_uid:$mapped_uid,host_gid:0,mode:"0700",
          device:$device,inode:$inode,uid_map_sha256:$map_sha256,observed_before_provider_input:true}' > "$attempt_directory/member-state-ownership.json"
fi
[[ "$(stat -c %u:%a "$state_root")" == 0:700 ]]
if [[ "$(jq -er '.operation' "$attempt_directory/input.json")" == qualification_cancel_before_provider ]]; then exit 143; fi
# review_snow_attempt_program_begin
claim_snow_attempt() {
    [[ "$provider_kind" == snow ]]
    if [[ "$(jq -er '.operation' "$attempt_directory/input.json")" == qualification_cancel_before_provider ]]; then return 0; fi
    idempotency_key="$(jq -er '.template.idempotency_key | strings | select(length>0 and length<=512)' "$attempt_directory/input.json")"
    idempotency_sha256="$(printf '%s' "$idempotency_key" | sha256sum | cut -d ' ' -f1)"
    claim_directory="$state_root/snow-review-claims"
    [[ ! -L "$claim_directory" ]]
    if [[ ! -d "$claim_directory" ]]; then mkdir -m 0700 "$claim_directory"; fi
    [[ "$(stat -c %u:%g:%a "$claim_directory")" == 0:0:700 ]]
    model_set="$(jq -cS '.members' "$attempt_directory/provider-settings.json")"
    model_set_sha256="$(printf '%s' "$model_set" | sha256sum | cut -d ' ' -f1)"
    claim_path="$claim_directory/$idempotency_sha256.json"
    (
        set -o noclobber
        jq -n --arg key_sha256 "$idempotency_sha256" --arg input_sha256 "$(sha256sum "$attempt_directory/input.json" | cut -d ' ' -f1)" \
            --arg settings_sha256 "$provider_settings_sha256" --arg model_set_sha256 "$model_set_sha256" --arg owner "$owner" --arg attempt "$attempt_directory" \
            '{schema:1,kind:"snow-review-single-attempt-claim",idempotency_key_sha256:$key_sha256,input_sha256:$input_sha256,
              provider_settings_sha256:$settings_sha256,model_set_sha256:$model_set_sha256,owner:$owner,attempt_directory:$attempt,
              automatic_retries:false}' > "$claim_path"
    )
    bounded sync -f "$claim_path"
    bounded sync -f "$claim_directory"
    cp -- "$claim_path" "$attempt_directory/attempt-claim.json"
}
# review_snow_attempt_program_end
if [[ "$provider_kind" == snow ]]; then claim_snow_attempt; fi
provider_input_released=true
timeout --signal=TERM --kill-after=1s "${command_limit_seconds}s" cat "$attempt_directory/input.json" >&7 7>&- &
input_pid=$!
wait "$input_pid"
input_pid=""
exec 7>&-
set +e
wait "$active_pid"
attach_status=$?
set -e
active_pid=""
wait "$stdout_pid" "$stderr_pid"
stdout_pid="" stderr_pid=""
bounded "${docker[@]}" inspect --format '{{json .State}}' "$container_id" > "$attempt_directory/container-state.json"
jq -e '.Status=="exited" and .Running==false and .OOMKilled==false and (.ExitCode|type)=="number"' "$attempt_directory/container-state.json" >/dev/null
broker_status="$(jq -er '.ExitCode' "$attempt_directory/container-state.json")"
broker_exit_observed=true
((attach_status==0 || attach_status==broker_status))
exit "$broker_status"

#!/usr/bin/env bash

# List local Go test directories for test.sh. Acceptance-owned trees and the
# scratch/build/profile directories ignored by this repository are artifacts,
# not source packages; prune them before looking for test files.
set -o pipefail

for command_name in find dirname sort go; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        printf 'test-dirs.sh: missing prerequisite: %s\n' "$command_name" >&2
        exit 1
    fi
done

script_path="${BASH_SOURCE[0]}"
if [[ "$script_path" == */* ]]; then
    script_dir="${script_path%/*}"
else
    script_dir="."
fi
server_dir="$(cd -- "$script_dir" >/dev/null 2>&1 && pwd)" || exit $?
cd "$server_dir" || exit $?

# The Connect wrapper owns only its own subtree. The canonical server
# wrapper keeps the same complete tree, including explicit testdata packages.
search_directory=.
if [[ "${1:-}" == --connect-only ]]; then
    search_directory=./connect
    shift
fi

# Replay Go flags that affect the active source set and the owner module.
# GOFLAGS is already applied by Go. Skip test-flag values before looking for
# more build options so a selection pattern cannot be consumed as a flag.
arguments=("$@")
build_args=()
for ((index = 0; index < ${#arguments[@]}; index++)); do
    argument=${arguments[index]}
    if [[ "$argument" == -args || "$argument" == --args || "$argument" == -- ]]; then
        break
    fi
    option=${argument%%=*}
    option=${option/#--/-}
    case "$option" in
        -race|-msan|-asan)
            build_args+=("$argument")
            ;;
        -tags|-mod|-modfile|-overlay|-compiler|-buildmode|-installsuffix|-pkgdir)
            build_args+=("$argument")
            if [[ "$argument" != *=* ]]; then
                ((index++))
                if [[ "$index" -ge "${#arguments[@]}" ]]; then
                    printf 'test-dirs.sh: missing value for %s\n' "$argument" >&2
                    exit 2
                fi
                build_args+=("${arguments[index]}")
            fi
            ;;
        *)
            case "${option/#-test./-}" in
                -run|-skip|-list|-bench|-benchtime|-count|-cpu|-parallel|-timeout|-shuffle|\
                -fuzz|-fuzztime|-fuzzminimizetime|-exec|\
                -blockprofile|-blockprofilerate|-coverprofile|-covermode|-coverpkg|\
                -cpuprofile|-memprofile|-memprofilerate|-mutexprofile|-mutexprofilefraction|\
                -outputdir|-trace|-vet|-o|-p|-asmflags|-gcflags|-gccgoflags|-ldflags|-pgo|-toolexec)
                    if [[ "$argument" != *=* ]]; then
                        ((index++))
                    fi
                    ;;
            esac
            ;;
    esac
done

# The vendored modules' tests (./third_party: go-redis, pgx, utls) are upstream
# suites that dial localhost's default Redis and Postgres ports directly, not
# the endpoints test-env.sh attests. On 2026-11-25 a developer machine forwarded
# localhost's Redis ports to a production cluster, and a vendored go-redis
# suite's FLUSHALL landed there. They're left out unless
# URNETWORK_TEST_THIRD_PARTY=1 asks for them, on a machine where those ports
# are disposable test servers.
third_party_prune=(-path './third_party' -prune -o)
if [[ "${URNETWORK_TEST_THIRD_PARTY:-}" == 1 ]]; then
    third_party_prune=()
elif [[ "$search_directory" == . && -d ./third_party ]]; then
    printf '%s\n' "test-dirs.sh: ./third_party is left out: its vendored tests dial localhost's default Redis and Postgres ports; URNETWORK_TEST_THIRD_PARTY=1 includes it" >&2
fi

candidates="$(find "$search_directory" \
    -type d \( \
        -iname '*acceptance*' -o \
        -path './temp' -o -path './.vscode' -o -path './.direnv' -o \
        -name '.git' -o -name 'bin' -o -name 'build' -o -name 'profile' \
    \) -prune -o \
    "${third_party_prune[@]}" \
    -path './connect/sim-latency/eval-*' -prune -o \
    -path './connect/sim-latency/baseline' -prune -o \
    -iname '*_test.go' -print | \
    while IFS= read -r test_file; do dirname "$test_file" || exit $?; done | sort -u)" || exit $?

# A literal package from its own directory retains nested modules, vendor
# resolution and file-specific flags. Omit only a build-excluded package with
# no active Go files. In particular, keep active test-only and ordinary sources
# and package diagnostics so the original go test reports build and vet errors.
template='{{if and .Error .IgnoredGoFiles (not (or .GoFiles .CgoFiles .TestGoFiles .XTestGoFiles .InvalidGoFiles)) (eq (printf "%v" .Error.Err) (printf "build constraints exclude all Go files in %s" .Dir))}}inactive{{else}}applicable{{end}}'
# GOFLAGS also applies to go list. Own discovery's operation and output
# mode while leaving the caller's test environment alone.
metadata_args=(
    -find -e -compiled=false -deps=false -test=false -export=false
    -m=false -u=false -versions=false -retracted=false -reuse= -json=false
    -f "$template"
)
directories="$(
    while IFS= read -r directory; do
        [[ -n "$directory" ]] || continue
        applicability="$(cd -- "$directory" && go list "${metadata_args[@]}" "${build_args[@]}" .)" || exit $?
        if [[ "$applicability" == applicable ]]; then
            printf '%s\n' "$directory"
        elif [[ "$applicability" != inactive ]]; then
            printf 'test-dirs.sh: unexpected Go package metadata for %s\n' "$directory" >&2
            exit 65
        fi
    done <<< "$candidates"
)" || exit $?
printf '%s\n' "$directories"

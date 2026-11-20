#!/usr/bin/env zsh

# The transport and edge integration tests exercise NGINX 1.31.4 UDP upstream
# PROXY protocol v2. Build the same pinned source used by warp/lb, then make the
# capability path explicit for every child `go test` process.
connect_dir=${0:A:h}
workspace_root=${URNETWORK_ROOT:-${WARP_HOME:-${connect_dir:h}}}
network_test_gate="$workspace_root/tests/network-intensive-suite-lock.sh"
if [[ ! -x "$network_test_gate" ]]; then
    echo "connect test suite gate is missing or not executable: $network_test_gate" >&2
    exit 127
fi
if [[ "${URNETWORK_NETWORK_TEST_LOCK_HELD:-}" != 1 ]]; then
    exec "$network_test_gate" run-all run-all-connect -- "$connect_dir/test.sh" "$@"
fi
if ! "$network_test_gate" --verify-held run-all; then
    echo "connect test suite inherited an invalid network-intensive lock" >&2
    exit 70
fi
warp_lb_dir=${connect_dir:h}/warp/lb
make -C "$warp_lb_dir" nginx_local
nginx_build_status=$?
if [[ $nginx_build_status != 0 ]]; then
    exit $nginx_build_status
fi
export NGINX_UDP_PROXY_V2_BINARY="$warp_lb_dir/build/nginx-local/sbin/nginx"

# A failed output filter is the causal pipeline status. Reporting only an
# upstream SIGPIPE would incorrectly attribute the failure to the test process.
test_pipeline_status() {
    local test_status="$1"
    local filter_status="$2"
    if [[ "$filter_status" != 0 ]]; then
        echo "test output filter failed with status $filter_status (upstream test status $test_status)" >&2
        return "$filter_status"
    fi
    return "$test_status"
}

# Some tests are timing-sensitive: they pass in a fresh process but stall or
# fail late in the full -race suite, once accumulated GC/race overhead slows
# delivery. Run each such group first, in its own process, so it executes under
# the same unpressured conditions as an isolated run, and skip them from the
# main run below.
#   - TestPtDns*: real-time QUIC-over-DNS transfers with tight per-stream deadlines.
#   - TestWebRtc*: pion/ice WebRTC transports whose ICE timing flakes under load.
#   - TestTunTCPThroughput: a sustained gvisor tun transfer judged against a
#     throughput floor, run once per ip family, whose per-family budget only
#     closes while the host is not also running the rest of the -race suite.
pt_filter='TestPtDnsEncodeDecode|TestPtDnsPumpEncodeDecode'
webrtc_filter='TestWebRtc'
tun_throughput_filter='TestTunTCPThroughput'
skip_filter="$pt_filter|$webrtc_filter|$tun_throughput_filter"

# run the WebRTC tests first, in their own process
match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -run "$webrtc_filter" "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
pipeline_status=("${pipestatus[@]}")
test_pipeline_status "${pipeline_status[1]}" "${pipeline_status[2]}" || exit $?

# run the packet-translation tests next, in their own process
match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -run "$pt_filter" "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
pipeline_status=("${pipestatus[@]}")
test_pipeline_status "${pipeline_status[1]}" "${pipeline_status[2]}" || exit $?

# run the tun throughput measurement next, in its own process
match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -run "$tun_throughput_filter" "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
pipeline_status=("${pipestatus[@]}")
test_pipeline_status "${pipeline_status[1]}" "${pipeline_status[2]}" || exit $?

# Match package discovery to the test command's race/build flags. Consume test
# flag values so a pattern such as `-run -tags` is not mistaken for a build flag.
# Go applies GOFLAGS itself, including test-only flags ignored by go list.
list_args=(-race)
for ((arg_index = 1; arg_index <= $#; arg_index++)); do
    argument=$argv[arg_index]
    option=${argument%%=*}
    option=${option/#--/-}
    option=${option/#-test./-}
    case "$option" in
        -race|-msan|-asan)
            list_args+=("$argument")
            ;;
        -tags|-mod|-modfile|-overlay|-compiler|-buildmode|-installsuffix)
            list_args+=("$argument")
            if [[ "$argument" != *=* ]]; then
                ((arg_index++))
                list_args+=("$argv[arg_index]")
            fi
            ;;
        -args) break ;;
        -run|-skip|-list|-bench|-benchtime|-count|-cpu|-parallel|-timeout|-shuffle|\
        -fuzz|-fuzztime|-fuzzminimizetime|\
        -blockprofile|-blockprofilerate|-coverprofile|-covermode|-coverpkg|\
        -cpuprofile|-memprofile|-memprofilerate|-mutexprofile|-mutexprofilefraction|\
        -outputdir|-trace|-vet|-o|-p|-asmflags|-gcflags|-gccgoflags|-ldflags|-pgo|-toolexec)
            if [[ "$argument" != *=* ]]; then
                ((arg_index++))
            fi
            ;;
    esac
done

# testdata contains pinned replay inputs that need a different root package;
# they are not standalone packages. Go also honors target/build constraints,
# hidden/vendor directories and nested module boundaries. Discovery errors
# must stop the suite before a partial package list can appear successful.
test_directories=$(go list "${list_args[@]}" \
    -f '{{if or .TestGoFiles .XTestGoFiles}}{{.Dir}}{{end}}' ./...) || exit $?
while IFS= read -r d; do
    [[ -n "$d" ]] || continue
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd "$d" || exit $?
        # highlight source files in this dir
        match="/${PWD##*/}/\\S*\.go\|^\\S*_test.go"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -skip "$skip_filter" -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --binary-files=text --line-buffered --color=always -e "^" -e "$match"
        # -trace profile/trace -coverprofile profile/cover
        pipeline_status=("${pipestatus[@]}")
        test_pipeline_status "${pipeline_status[1]}" "${pipeline_status[2]}" || exit $?
        popd
    # fi
done <<< "$test_directories"
# stdbuf -i0 -o0 -e0

# to turn on logging e.g.
# go test -args -v 2 -logtostderr true

# go tool trace profile/trace
# PPROF_BINARY_PATH=. go tool pprof profile/cpu

# store default.pgo
# https://go.dev/doc/pgo

#!/usr/bin/env zsh

# Some tests are timing-sensitive: they pass in a fresh process but stall or
# fail late in the full -race suite, once accumulated GC/race overhead slows
# delivery. Run each such group first, in its own process, so it executes under
# the same unpressured conditions as an isolated run, and skip them from the
# main run below.
#   - TestPtDns*: real-time QUIC-over-DNS transfers with tight per-stream deadlines.
#   - TestWebRtc*: pion/ice WebRTC transports whose ICE timing flakes under load.
pt_filter='TestPtDnsEncodeDecode|TestPtDnsPumpEncodeDecode'
webrtc_filter='TestWebRtc'
skip_filter="$pt_filter|$webrtc_filter"

# run the WebRTC tests first, in their own process
match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -run "$webrtc_filter" "$@" | grep --color=always -e "^" -e "$match"
if [[ ${pipestatus[1]} != 0 ]]; then
    exit ${pipestatus[1]}
fi

# run the packet-translation tests next, in their own process
match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -run "$pt_filter" "$@" | grep --color=always -e "^" -e "$match"
if [[ ${pipestatus[1]} != 0 ]]; then
    exit ${pipestatus[1]}
fi

for d in `find . -iname '*_test.go' | xargs -n 1 dirname | sort | uniq | paste -sd ' ' -`; do
    # if [[ $1 == "" || $1 == `basename $d` ]]; then
        pushd $d
        # highlight source files in this dir
        match="/$(basename $(pwd))/\\S*\.go\|^\\S*_test.go"
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -skip "$skip_filter" -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --color=always -e "^" -e "$match"
        # -trace profile/trace -coverprofile profile/cover
        if [[ ${pipestatus[1]} != 0 ]]; then
            exit ${pipestatus[1]}
        fi
        popd
    # fi
done
# stdbuf -i0 -o0 -e0

# to turn on logging e.g.
# go test -args -v 2 -logtostderr true

# go tool trace profile/trace
# PPROF_BINARY_PATH=. go tool pprof profile/cpu

# store default.pgo
# https://go.dev/doc/pgo

#!/usr/bin/env zsh

# The packet-translation tests (TestPtDns*) drive real-time QUIC-over-DNS
# transfers with tight per-stream deadlines. They pass in a fresh process but
# stall and time out late in the full -race suite, once accumulated GC/race
# overhead slows UDP delivery. Run them first, in their own process, so they
# execute under the same unpressured conditions as an isolated run, and skip
# them from the main run below.
pt_filter='TestPtDnsEncodeDecode|TestPtDnsPumpEncodeDecode'

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
        GORACE="log_path=profile/race.out halt_on_error=1" go test -timeout 0 -v -race -skip "$pt_filter" -cpuprofile profile/cpu -memprofile profile/memory "$@" | grep --color=always -e "^" -e "$match"
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

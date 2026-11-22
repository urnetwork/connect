#!/usr/bin/env bash
# go test -exec for GOOS=js GOARCH=wasm that keeps net/http on fetch.
#
# Go's js net/http uses the global fetch only when process.argv0 does not
# start with "node" (net/http/roundtrip_js.go jsFetchDisabled); under the
# stock go_js_wasm_exec every request falls back to the socket transport,
# which a browser never uses. Starting node under another argv0 lets the js
# tests drive the browser path with a fake fetch (net_http_platform_js_test.go):
#
#   GOOS=js GOARCH=wasm go test -exec "$PWD/tools/go_js_wasm_exec_fetch.sh" .
set -euo pipefail
goroot="$(go env GOROOT)"
glue="$goroot/lib/wasm/wasm_exec_node.js"
if [[ ! -f "$glue" ]]; then
    glue="$goroot/misc/wasm/wasm_exec_node.js"
fi
exec -a wasm-browser-test node --stack-size=8192 "$glue" "$@"

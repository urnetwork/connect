# Pinned HTTP mutation endpoints

`WithHttpRedirectsDisabled` scopes refusal to requests derived from one operation
context. HTTP serial, parallel and hello attempts use a shallow per-attempt client
copy with `CheckRedirect` returning `http.ErrUseLastResponse`. The shared client,
transport, jar, timeout and unrelated redirect behavior remain unchanged. The
original completed 3xx response reaches its caller; a 307/308 cannot move a replayable
mutation body to another path or origin. Physical failure retries keep the original
URL/method/body/headers and the existing finite budgets.

The js/wasm browser path additionally sets Go's internal `js.fetch:redirect=error`
option on a cloned request header. Browser fetch follows redirects inside its
RoundTrip unless that option is set; a Client callback alone would be insufficient.
The option is consumed by Go's browser transport and is not a wire header. Native
transport never receives it. Native fallback still uses the scoped Client policy.
Browser execution is not claimed by native tests; the helper is covered for header
ownership and the js build is compile-qualified separately.

Only the SDK's versioned client-registration operation selects this policy. It is
not a global route change or an idempotency claim for legacy clients. Qualification
must exercise actual local same-origin and cross-origin 307/308 responses, unchanged
unscoped cached-client behavior, exact original retry after a physical body break,
and the SDK's completed hard refusal before enabling registration rollout.

The current-main integration on 2026-10-01 passes the affected normal/race suites
in Connect (75 top-level tests) and SDK (100), including these local endpoint and
ownership checks. The merge's auth-observation regression also rejects a control
that restores the unscoped serial client. Browser execution remains unqualified:
both clean current main and this merge fail `GOOS=js GOARCH=wasm go build .` on
native `syscall.MSG_PEEK` references in the unchanged UDP return-read files.

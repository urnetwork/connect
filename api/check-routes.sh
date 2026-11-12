#!/usr/bin/env zsh
#
# Pin bringyour.yml to the server's route table.
#
#   ./check-routes.sh /path/to/urnetwork/server
#
# Extracts every router.NewRoute("METHOD", "/path", ...) and
# router.NewStreamingRoute("METHOD", "/path", ...) registration the api
# service serves (api/api.go plus the oauth authorization server routes it
# appends) and every method+path the spec documents, normalizes the two into
# the same shape, and exits non-zero if either side has something the other
# does not. A streaming route (one whose request body the server reads at the
# client's pace) is a route like any other here.
#
# Two groups of routes are documented elsewhere, and bringyour.yml must NOT
# list them: a path of theirs found there is drift. Each is checked against
# its own document instead, looked up beside the server checkout; when that
# document is not reachable its half of the check is skipped with a note
# rather than failed, so the script still works in a checkout without the sn
# repo beside the server.
#
#   - /competition/* is a separate security domain this api process serves:
#     <server>/../sn/api/competition.yml.
#   - /sn/ctf, /sn/ctf/* and /sn/openapi.yml are the operator's public SN
#     routes: <server>/../sn/api/operator.yml, which the server serves at
#     GET /sn/openapi.yml. That document also lists GET /sn/ledger/account,
#     which bringyour.yml lists too; every operation it lists must be a
#     registered route.
#
# Deliberately dependency-free: zsh, grep, sed, sort, comm. No yaml parser and
# no go toolchain, so it runs in a bare checkout and in CI.
#
# Normalization:
#   route  ([^/]+)   -> *      spec  {name} -> *
#   route  \.        -> .      (the route table escapes dots for its regex)
#
# That second rule matters for the three /.well-known/ discovery documents,
# which are the only routes registered with escaped dots. The server's own
# api/spec_conformance_test.go normalizePath() unescapes them the same way.
#
# Test files are excluded: they register routes that are not served.

set -u
emulate -L zsh
setopt no_nomatch pipe_fail

script_dir=${0:A:h}
spec=$script_dir/bringyour.yml

if [[ $# -ne 1 ]]; then
    print -u2 "usage: ${0:t} <path to the urnetwork server checkout>"
    exit 2
fi

server_dir=${~1}
if [[ ! -d $server_dir ]]; then
    print -u2 "${0:t}: not a directory: $server_dir"
    exit 2
fi
if [[ ! -f $server_dir/api/api.go ]]; then
    print -u2 "${0:t}: $server_dir does not look like the server checkout (no api/api.go)"
    exit 2
fi
if [[ ! -f $spec ]]; then
    print -u2 "${0:t}: spec not found: $spec"
    exit 2
fi

competition_spec=${server_dir:A:h}/sn/api/competition.yml
operator_spec=${server_dir:A:h}/sn/api/operator.yml

# the "METHOD /path" lines sn/api/operator.yml alone documents
operator_owned='^[A-Z]+ /sn/(ctf(/.*)?|openapi[.]yml)$'

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# spec_paths <file> -- the "METHOD /path" operations an OpenAPI document
# declares. Paths are the two-space keys under `paths:`; methods are their
# four-space children. Stops at the next top-level key (components:).
spec_paths() {
    awk '
        /^paths:[[:space:]]*$/ { inpaths = 1; next }
        /^[A-Za-z]/            { inpaths = 0 }
        !inpaths               { next }
        /^  \/[^ ]*:[[:space:]]*$/ { path = $1; sub(/:$/, "", path); next }
        /^    (get|put|post|patch|delete|head|options|trace):[[:space:]]*$/ {
            method = $1
            sub(/:$/, "", method)
            print toupper(method), path
        }
    ' "$1" | sed -e 's/{[^}]*}/*/g' | sort -u
}

# --- the route table -------------------------------------------------------
# Every NewRoute( and NewStreamingRoute( occurrence, not every line: some
# source lines hold two.
route_files=()
for f in $server_dir/api/api.go $server_dir/oauth/handlers.go; do
    [[ -f $f ]] && route_files+=$f
done

grep -ohE 'New(Streaming)?Route\("[A-Z*]*", "[^"]*"' $route_files \
    | sed -e 's/.*Route("//' -e 's/", "/ /' -e 's/"$//' \
    | sed -e 's/\\\\\./\./g' -e 's/(\[^\/\]+)/*/g' \
    | sort -u > $work/routes.all

grep -v ' /competition/' $work/routes.all | grep -Ev "$operator_owned" > $work/routes
grep ' /competition/' $work/routes.all > $work/routes.competition
grep -E "$operator_owned" $work/routes.all > $work/routes.operator

# --- the specs -------------------------------------------------------------
spec_paths $spec > $work/spec.all
grep -v ' /competition/' $work/spec.all | grep -Ev "$operator_owned" > $work/spec
grep ' /competition/' $work/spec.all > $work/spec.competition
grep -E "$operator_owned" $work/spec.all > $work/spec.operator_listed

drifted=0

report() {
    print -u2 "$1:"
    print -u2 -- "$2" | sed 's/^/  /' >&2
    drifted=1
}

# --- bringyour.yml ---------------------------------------------------------
missing_from_spec=$(comm -23 $work/routes $work/spec)
missing_from_server=$(comm -13 $work/routes $work/spec)

[[ -n $missing_from_spec ]] && report "registered but NOT in bringyour.yml" "$missing_from_spec"
[[ -n $missing_from_server ]] && report "in bringyour.yml but NOT registered" "$missing_from_server"

# a competition path in bringyour.yml is drift on its own: they belong to
# sn/api/competition.yml and must not be dual-listed
listed_competition=$(cat $work/spec.competition)
[[ -n $listed_competition ]] && \
    report "in bringyour.yml but belongs in sn/api/competition.yml" "$listed_competition"

# likewise the routes sn/api/operator.yml owns
listed_operator=$(cat $work/spec.operator_listed)
[[ -n $listed_operator ]] && \
    report "in bringyour.yml but belongs in sn/api/operator.yml" "$listed_operator"

# --- sn/api/competition.yml ------------------------------------------------
route_count=$(grep -c . $work/routes)
spec_count=$(grep -c . $work/spec)
competition_route_count=$(grep -c . $work/routes.competition)

if [[ -f $competition_spec ]]; then
    spec_paths $competition_spec > $work/spec.sn
    competition_spec_count=$(grep -c . $work/spec.sn)

    competition_missing=$(comm -23 $work/routes.competition $work/spec.sn)
    competition_extra=$(comm -13 $work/routes.competition $work/spec.sn)

    [[ -n $competition_missing ]] && \
        report "registered but NOT in sn/api/competition.yml" "$competition_missing"
    [[ -n $competition_extra ]] && \
        report "in sn/api/competition.yml but NOT registered" "$competition_extra"

    competition_summary="$competition_route_count competition routes, $competition_spec_count documented in sn/api/competition.yml"
else
    competition_summary="$competition_route_count competition routes NOT CHECKED (no $competition_spec)"
fi

# --- sn/api/operator.yml ---------------------------------------------------
operator_route_count=$(grep -c . $work/routes.operator)

if [[ -f $operator_spec ]]; then
    spec_paths $operator_spec > $work/spec.operator.all
    grep -E "$operator_owned" $work/spec.operator.all > $work/spec.operator
    operator_spec_count=$(grep -c . $work/spec.operator.all)

    operator_missing=$(comm -23 $work/routes.operator $work/spec.operator)
    # every operation the document lists, its own and the shared ledger
    # route alike, is served
    operator_extra=$(comm -13 $work/routes.all $work/spec.operator.all)

    [[ -n $operator_missing ]] && \
        report "registered but NOT in sn/api/operator.yml" "$operator_missing"
    [[ -n $operator_extra ]] && \
        report "in sn/api/operator.yml but NOT registered" "$operator_extra"

    operator_summary="$operator_route_count operator routes, $operator_spec_count operations documented in sn/api/operator.yml"
else
    operator_summary="$operator_route_count operator routes NOT CHECKED (no $operator_spec)"
fi

if (( drifted == 0 )); then
    print "ok: $route_count registered routes, $spec_count documented operations; $competition_summary; $operator_summary; no drift"
else
    print -u2 "drift: $route_count registered routes, $spec_count documented operations; $competition_summary; $operator_summary"
fi

exit $drifted

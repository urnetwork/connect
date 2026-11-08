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
# The /competition/* routes are one exception. This api process serves
# them, but they are a separate security domain with their own document,
# sn/api/competition.yml, so bringyour.yml must NOT list them: a competition
# path found there is drift. They are checked against that document instead,
# looked up at <server>/../sn/api/competition.yml; when it is not reachable
# that half of the check is skipped with a note rather than failed, so the
# script still works in a checkout without the sn repo beside the server.
#
# The /gkm/* routes are the other exception, the other way round: bringyour.yml
# documents them, but server/cli/gkm serves them on its own host from sidecar's
# gkmservice package, so api must not register them: a gkm path in its table is
# drift. They are checked against that package's route table instead, looked up
# at <server>/../sidecar/gkmservice/routes.go, where each route is a net/http
# pattern in one string literal, as in "POST /gkm/groups/{group_id}/gen/{g}".
# A planned gkm operation may be missing from that table, as one may be from
# api's (below), but a table that names no route at all fails, so a table in
# another shape cannot pass by matching nothing. Until that file exists, or in a
# checkout without the sidecar repo beside the server, that half of the check is
# skipped with a note rather than failed.
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
#
# An operation marked `x-status: planned` documents a route a later change
# lands, so the spec can lead the implementation. It may be missing from the
# route table, as the server's api/spec_conformance_test.go allows.

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

# The siblings are found beside the path given, never beside a symlink's
# target: a track's server is a symlink to the main checkout's, and its own
# sidecar and sn sit beside the link. :a makes the path absolute and resolves
# . and .. as written; :A would follow the link.
competition_spec=${server_dir:a:h}/sn/api/competition.yml
gkm_routes=${server_dir:a:h}/sidecar/gkmservice/routes.go

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

# planned_paths <file> -- the "METHOD /path" operations marked planned. The mark
# is an operation key, at six spaces, under the method it follows.
planned_paths() {
    awk '
        /^paths:[[:space:]]*$/ { inpaths = 1; next }
        /^[A-Za-z]/            { inpaths = 0 }
        !inpaths               { next }
        /^  \/[^ ]*:[[:space:]]*$/ { path = $1; sub(/:$/, "", path); method = ""; next }
        /^    (get|put|post|patch|delete|head|options|trace):[[:space:]]*$/ {
            method = $1
            sub(/:$/, "", method)
            next
        }
        /^      x-status:[[:space:]]*planned[[:space:]]*$/ {
            if (method != "") print toupper(method), path
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

grep -v -e ' /competition/' -e ' /gkm/' $work/routes.all > $work/routes
grep ' /competition/' $work/routes.all > $work/routes.competition

# --- the specs -------------------------------------------------------------
spec_paths $spec > $work/spec.all
grep -v -e ' /competition/' -e ' /gkm/' $work/spec.all > $work/spec
grep ' /competition/' $work/spec.all > $work/spec.competition
planned_paths $spec > $work/planned

drifted=0

report() {
    print -u2 "$1:"
    print -u2 -- "$2" | sed 's/^/  /' >&2
    drifted=1
}

# --- bringyour.yml ---------------------------------------------------------
missing_from_spec=$(comm -23 $work/routes $work/spec)
missing_from_server=$(comm -13 $work/routes $work/spec | comm -23 - $work/planned)

[[ -n $missing_from_spec ]] && report "registered but NOT in bringyour.yml" "$missing_from_spec"
[[ -n $missing_from_server ]] && report "in bringyour.yml but NOT registered" "$missing_from_server"

# a competition path in bringyour.yml is drift on its own: they belong to
# sn/api/competition.yml and must not be dual-listed
listed_competition=$(cat $work/spec.competition)
[[ -n $listed_competition ]] && \
    report "in bringyour.yml but belongs in sn/api/competition.yml" "$listed_competition"

# --- gkmservice's route table ----------------------------------------------
grep ' /gkm/' $work/spec.all > $work/spec.gkm
gkm_spec_count=$(grep -c . $work/spec.gkm)

# a gkm path in api's table is drift on its own, whatever gkmservice holds
registered_gkm=$(grep ' /gkm/' $work/routes.all)
[[ -n $registered_gkm ]] && report "registered by api but served by gkm" "$registered_gkm"

if [[ -f $gkm_routes ]]; then
    grep -ohE '"[A-Z]+ /gkm/[^"]*"' $gkm_routes \
        | sed -e 's/^"//' -e 's/"$//' -e 's/{[^}]*}/*/g' \
        | sort -u > $work/routes.gkm
    gkm_route_count=$(grep -c . $work/routes.gkm)
    gkm_planned_count=$(grep -c ' /gkm/' $work/planned)

    (( gkm_route_count == 0 )) && report "gkmservice's route table names no route" "$gkm_routes"

    gkm_missing=$(comm -23 $work/routes.gkm $work/spec.gkm)
    gkm_extra=$(comm -13 $work/routes.gkm $work/spec.gkm | comm -23 - $work/planned)

    [[ -n $gkm_missing ]] && report "in gkmservice's route table but NOT in bringyour.yml" "$gkm_missing"
    [[ -n $gkm_extra ]] && report "in bringyour.yml but NOT in gkmservice's route table" "$gkm_extra"

    print "gkm: $gkm_route_count routes in gkmservice's table, $gkm_spec_count documented operations ($gkm_planned_count planned)"
else
    print "gkm: $gkm_spec_count documented operations NOT CHECKED (no $gkm_routes)"
fi

# --- sn/api/competition.yml ------------------------------------------------
route_count=$(grep -c . $work/routes)
spec_count=$(grep -c . $work/spec)
planned_count=$(grep -c . $work/planned)
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

if (( drifted == 0 )); then
    print "ok: $route_count registered routes, $spec_count documented operations ($planned_count planned); $competition_summary; no drift"
else
    print -u2 "drift: $route_count registered routes, $spec_count documented operations ($planned_count planned); $competition_summary"
fi

exit $drifted

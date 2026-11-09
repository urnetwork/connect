"""Regression checks for the published OpenAPI document (requires PyYAML)."""

import re
import shutil
import subprocess
import tempfile
import unittest
from pathlib import Path
from urllib.parse import urlparse

import yaml


class SpecTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.spec = yaml.safe_load(Path(__file__).with_name("bringyour.yml").read_text())

    def test_sn_top_replica_score_distinguishes_unavailable_evidence(self):
        fleet = self.spec["components"]["schemas"]["SnTopFleet"]
        self.assertIn("replica_score", fleet["required"])
        self.assertEqual(
            ["number", "null"], fleet["properties"]["replica_score"]["type"]
        )

    def test_paths_and_shared_responses_stay_in_their_sections(self):
        paths = self.spec["paths"]
        for path in ("/privacy.txt", "/terms.txt", "/vdp.txt"):
            with self.subTest(path=path):
                self.assertIn("303", paths[path]["get"]["responses"])
                self.assertNotIn("200", paths[path]["get"]["responses"])
        status = paths["/status"]["get"]["responses"]["200"]
        self.assertEqual(
            "#/components/schemas/WarpStatusResult",
            status["content"]["application/json"]["schema"]["$ref"],
        )
        for section in self.spec["components"].values():
            self.assertFalse(any(key.startswith("/") for key in section))
        for path, method in (
            ("/network/sessions", "get"),
            ("/network/revoke-session", "post"),
            ("/network/revoke-other-sessions", "post"),
            ("/network/session-operations/{operation_id}", "get"),
        ):
            with self.subTest(path=path):
                responses = paths[path][method]["responses"]
                self.assertIn("200", responses)
                self.assertEqual(
                    "#/components/responses/ClientJwtRefused",
                    responses["403"]["$ref"],
                )

    def test_unique_exit_names_match_the_server(self):
        # model/proxy_unique_exit_model.go and model/network_client_model.go
        schemas = self.spec["components"]["schemas"]
        proxy_config = schemas["AuthNetworkClientArgs"]["properties"]["proxy_config"]
        self.assertEqual("boolean", proxy_config["properties"]["unique_exit"]["type"])
        self.assertEqual(
            ["auto", "manual"],
            [o["const"] for o in proxy_config["properties"]["unique_exit_replace"]["oneOf"]],
        )
        result = schemas["AuthNetworkClientResult"]["properties"]
        self.assertEqual(
            "#/components/schemas/ProxyExit",
            result["proxy_config_result"]["allOf"][0]["properties"]["exit"]["$ref"],
        )
        self.assertEqual("boolean", result["error"]["properties"]["no_unique_supply"]["type"])
        item = schemas["NetworkClientsResult"]["properties"]["clients"]["items"]
        self.assertEqual("#/components/schemas/ProxyExit", item["properties"]["exit"]["$ref"])
        exit = schemas["ProxyExit"]
        self.assertEqual(
            {"exit_id", "country_code", "region", "city", "exit_since", "state", "state_reason", "replace_mode"},
            set(exit["properties"]),
        )
        self.assertEqual(
            ["assigned", "replacing", "no_supply"],
            [o["const"] for o in exit["properties"]["state"]["oneOf"]],
        )
        rotate = self.spec["paths"]["/network/proxy/rotate-exit"]["post"]
        self.assertEqual(
            "#/components/schemas/RotateProxyExitArgs",
            rotate["requestBody"]["content"]["application/json"]["schema"]["$ref"],
        )
        self.assertEqual(
            "#/components/schemas/RotateProxyExitResult",
            rotate["responses"]["200"]["content"]["application/json"]["schema"]["$ref"],
        )
        self.assertEqual("#/components/responses/ClientJwtRefused", rotate["responses"]["403"]["$ref"])
        self.assertEqual(["client_id"], schemas["RotateProxyExitArgs"]["required"])
        self.assertEqual(
            {"no_unique_supply", "rate_limited", "message"},
            set(schemas["RotateProxyExitResult"]["properties"]["error"]["properties"]),
        )

    # the routes of sidecar DESIGN.md §7, §8 and §11 a service serves, out of the
    # planned list: api the authorization log's and §8.9's acks, and evs's host
    # its engine's routes (EVS1-M2-21). Each route's own task adds it here when it
    # lifts the planned mark
    served = {
        ("/network/config/projection", "get"),
        ("/network/config/projection", "post"),
        ("/network/config/delta", "post"),
        ("/acl/ack", "post"),
        ("/evs/v1/repos/{repo_id}", "put"),
        ("/evs/v1/repos/{repo_id}/segments/{segment_id}", "put"),
    }

    def test_sidecar_routes_are_documented_and_planned(self):
        # the routes of sidecar DESIGN.md §7, §8 and §11. Each stays planned and
        # out of the public api until a service serves it; the owner of a route
        # moves it to served when it does.
        expected = {
            "/network/enroll": {"post"},
            "/network/config/projection": {"get", "post"},
            "/network/config/delta": {"post"},
            "/network/config/reseed": {"post"},
            "/acl/ack": {"post"},
            "/acl/invite": {"post"},
            "/acl/accept-invite": {"post"},
            "/evs/v1/repos/{repo_id}": {"put", "get", "delete"},
            "/evs/v1/repos/{repo_id}/restore": {"post"},
            "/evs/v1/repos/{repo_id}/activate": {"post"},
            "/evs/v1/repos/{repo_id}/reseed": {"post"},
            "/evs/v1/repos/{repo_id}/segments/{segment_id}": {"put"},
            "/evs/v1/repos/{repo_id}/push": {"post"},
            "/evs/v1/repos/{repo_id}/push/{push_id}/{part}": {"post"},
            "/evs/v1/repos/{repo_id}/push/{push_id}/lease": {"post"},
            "/evs/v1/repos/{repo_id}/path-epoch": {"post"},
            "/evs/v1/repos/{repo_id}/path-epoch/block": {"post"},
            "/evs/v1/repos/{repo_id}/read": {"post"},
            "/evs/v1/repos/{repo_id}/refs": {"get"},
            "/evs/v1/repos/{repo_id}/log": {"get"},
            "/evs/v1/repos/{repo_id}/slots/delete": {"post"},
            "/evs/v1/repos/{repo_id}/drops/preview": {"post"},
            "/evs/v1/repos/{repo_id}/drops/preview/{preview_id}": {"get"},
            "/evs/v1/repos/{repo_id}/drops": {"get"},
            "/evs/v1/subscriptions": {"post", "get", "delete"},
        }
        methods = {"get", "put", "post", "delete", "patch"}
        paths = self.spec["paths"]
        evs_hosts = set()
        for path, want in expected.items():
            with self.subTest(path=path):
                self.assertIn(path, paths)
                item = paths[path]
                self.assertEqual(want, methods & set(item))
                for method in want:
                    operation = item[method]
                    self.assertTrue(operation["x-internal"])
                    if (path, method) in self.served:
                        self.assertNotIn("x-status", operation)
                    else:
                        self.assertEqual("planned", operation["x-status"])
                if path.startswith("/evs/"):
                    evs_hosts.add(item["servers"][0]["url"])
                    for method in want:
                        self.assertEqual([{"ServiceTokenAuth": []}], item[method]["security"])
                else:
                    self.assertNotIn("servers", item)
        # evs is one host of its own, so no path falls back to api's
        self.assertEqual(1, len(evs_hosts))
        self.assertFalse(evs_hosts & {server["url"] for server in self.spec["servers"]})
        self.assertLessEqual({path for path, _ in self.served}, set(expected))
        enroll = paths["/network/enroll"]["post"]
        self.assertEqual([], enroll["security"])
        args = self.spec["components"]["schemas"]["NetworkEnrollArgs"]
        self.assertNotIn("bundle", args["properties"])

    def test_tls_routes_are_documented_and_planned(self):
        # sidecar DESIGN.md §13.15's certificate routes, each with its class: the
        # order and its status take a service token for audience certs, the rest
        # a bearer. Each stays planned and out of the public api until certs
        # serves it, on api's own host. POST /tls/suspend stays a model call, so
        # it is no path here.
        expected = {
            ("/tls/cert-order", "post"): "ServiceTokenAuth",
            ("/tls/cert-order/{cert_id}", "get"): "ServiceTokenAuth",
            ("/tls/cert-register", "post"): "BearerAuth",
            ("/tls/certs", "get"): "BearerAuth",
            ("/tls/cert-revoke", "post"): "BearerAuth",
        }
        methods = {"get", "put", "post", "delete", "patch"}
        paths = self.spec["paths"]
        self.assertEqual(
            {path for path, _ in expected},
            {path for path in paths if path.startswith("/tls/")},
        )
        for (path, method), scheme in expected.items():
            with self.subTest(path=path, method=method):
                item = paths[path]
                self.assertEqual({method}, methods & set(item))
                self.assertNotIn("servers", item)
                operation = item[method]
                self.assertTrue(operation["x-internal"])
                self.assertEqual("planned", operation["x-status"])
                self.assertEqual([{scheme: []}], operation["security"])
        # the bodies' fields are sidecar tls_api.go's JSON names, which its
        # TestTlsBodiesKeepTheirWireNames pins on that side
        schemas = self.spec["components"]["schemas"]
        fields = {
            "TlsCertOrderArgs": {"csr", "attestation", "replaces"},
            "TlsCertOrderResult": {"cert_id", "state", "retry_after_ms"},
            "TlsCertOrderStatus": {"state", "retry_after_ms", "chain", "ari_window_start", "ari_window_end"},
            "TlsCertsResult": {"certs", "queued_csrs"},
            "TlsCertRegisterArgs": {"cert_id", "attestation", "chain", "account_uri"},
            "TlsCertRevokeArgs": {"cert_id", "key_compromise"},
            "TlsCertRecord": {
                "cert_id", "network_id", "client_id", "sidecar_id", "identifier_set_sha256",
                "spki_sha256", "attestation", "csr", "ca", "account_uri", "profile", "serial",
                "not_before", "not_after", "ari_window_start", "ari_window_end", "state",
                "successor_cert_id", "chain",
            },
        }
        for name, want in fields.items():
            with self.subTest(schema=name):
                self.assertEqual(want, set(schemas[name]["properties"]))
        self.assertEqual(
            ["queued", "ordering", "waiting_dns", "issued", "failed", "revoking", "revoked", "expired"],
            schemas["TlsCertState"]["enum"],
        )

    def test_socket_routes_are_documented_and_planned(self):
        # sidecar DESIGN.md §9.15's routes, each with its class: the three
        # Client routes take a client token, and GET /socket/keys is public.
        # sockets serves them on one host of its own, never api's, so each
        # stays planned and out of the public api until it does.
        expected = {
            ("/socket/registrations", "post"): [{"BearerAuth": []}],
            ("/socket/find", "post"): [{"BearerAuth": []}],
            ("/socket/list", "get"): [{"BearerAuth": []}],
            ("/socket/keys", "get"): [],
        }
        methods = {"get", "put", "post", "delete", "patch"}
        paths = self.spec["paths"]
        self.assertEqual(
            {path for path, _ in expected},
            {path for path in paths if path.startswith("/socket/")},
        )
        hosts = set()
        for (path, method), security in expected.items():
            with self.subTest(path=path, method=method):
                item = paths[path]
                self.assertEqual({method}, methods & set(item))
                hosts.add(item["servers"][0]["url"])
                operation = item[method]
                self.assertTrue(operation["x-internal"])
                self.assertEqual("planned", operation["x-status"])
                self.assertEqual(security, operation["security"])
        # one host of its own, so no path falls back to api's
        self.assertEqual(1, len(hosts))
        self.assertFalse(hosts & {server["url"] for server in self.spec["servers"]})
        # the bodies' fields are sidecar socket_api.go's JSON names, which its
        # TestSocketRouteBodyNames pins on that side
        schemas = self.spec["components"]["schemas"]
        fields = {
            "SocketRegistrationsArgs": {"set_version", "key_start", "key_end", "registrations"},
            "SocketRegistration": {
                "name", "name_token", "sealed_name", "listen_id", "protocol", "version", "alpn",
                "app_mode", "weight", "max_streams", "exclusive", "affinity", "serving", "draining",
                "inflight", "claim",
            },
            "SocketRegistrationsResult": {
                "statuses", "set_version", "server_time", "ticket_keys", "urnetwork_edge",
                "streams_stopped", "error",
            },
            "SocketError": {"code", "retry_after_ms"},
            "SocketTicketKey": {"key_id", "public_key", "expire_time_millis"},
            "SocketFindArgs": {
                "name", "name_token", "network_id", "affinity_key", "failed_tickets", "client_ids",
            },
            "SocketFailedTicket": {"ticket", "reason"},
            "SocketFindTicket": {"ticket", "claim"},
            "SocketFindResult": {"primary", "backup", "error"},
            "SocketListName": {"name", "name_token", "sealed_name"},
            "SocketListResult": {"names", "next_cursor", "error"},
            "SocketKeysResult": {"keys"},
        }
        for name, want in fields.items():
            with self.subTest(schema=name):
                self.assertEqual(want, set(schemas[name]["properties"]))
        enums = {
            "SocketRegistrationStatus": ["ok", "revoked", "denied", "taken", "invalid"],
            "SocketErrorCode": [
                "no_listener", "busy", "denied", "quota", "unavailable", "stale", "invalid",
            ],
            "SocketFailedReason": ["timeout", "refused"],
        }
        for name, want in enums.items():
            with self.subTest(schema=name):
                self.assertEqual(want, schemas[name]["enum"])
        # a dial's protocol, version and ALPN stay off the wire until the
        # metadata list names them for Find (plan-SOCK-metadata-rows)
        self.assertFalse(
            {"protocol", "min_version", "alpn"} & set(schemas["SocketFindArgs"]["properties"])
        )

    def test_served_evs_routes_answer_in_evs_messages(self):
        # plan-EVS1-format-bytes #8: evs's bodies are evs.proto messages as
        # application/x-protobuf, but a segment's raw bytes, and every answer but
        # a 200 is an EvsRefusalReply (connect-m1-c11-route-scope #2: the task
        # that serves a route fixes its encoding as it lifts planned)
        paths = self.spec["paths"]
        refusal = {"$ref": "#/components/schemas/EvsRefusalReply"}
        for path, method in sorted(self.served):
            if not path.startswith("/evs/"):
                continue
            operation = paths[path][method]
            with self.subTest(path=path, method=method):
                self.assertEqual([{"ServiceTokenAuth": []}], operation["security"])
                for status, response in operation["responses"].items():
                    if status == "200":
                        continue
                    self.assertEqual({"application/x-protobuf": {"schema": refusal}}, response["content"], status)
        create = paths["/evs/v1/repos/{repo_id}"]["put"]
        self.assertNotIn("requestBody", create)
        self.assertNotIn("content", create["responses"]["200"])
        upload = paths["/evs/v1/repos/{repo_id}/segments/{segment_id}"]["put"]
        self.assertEqual({"application/octet-stream"}, set(upload["requestBody"]["content"]))
        self.assertEqual(
            {"application/x-protobuf": {"schema": {"$ref": "#/components/schemas/EvsUploadReceipt"}}},
            upload["responses"]["200"]["content"],
        )
        schemas = self.spec["components"]["schemas"]
        for name in ("EvsRefusalReply", "EvsUploadReceipt"):
            self.assertEqual({"type": "string", "format": "binary"}, {key: schemas[name][key] for key in ("type", "format")})

    def test_auth_key_routes_are_served(self):
        # TEAM-M1-03 serves §7.8's auth key routes: out of the planned list,
        # the network credential's (403 for a client token), and the bundle never
        # a field of any of them
        paths = self.spec["paths"]
        schemas = self.spec["components"]["schemas"]
        for path, methods in {"/network/auth-keys": {"get", "post"}, "/network/auth-key-revoke": {"post"}}.items():
            for method in methods:
                with self.subTest(path=path, method=method):
                    operation = paths[path][method]
                    self.assertNotIn("x-status", operation)
                    self.assertNotIn("x-internal", operation)
                    self.assertEqual([{"BearerAuth": []}], operation["security"])
                    self.assertEqual("#/components/responses/ClientJwtRefused", operation["responses"]["403"]["$ref"])
        self.assertEqual({"bundle_still_admits", "note", "error"}, set(schemas["NetworkAuthKeyRevokeResult"]["properties"]))
        for name in ("NetworkAuthKey", "NetworkAuthKeyCreateArgs", "NetworkAuthKeyCreateResult", "NetworkAuthKeyRevokeResult"):
            self.assertNotIn("bundle", schemas[name]["properties"])
        self.assertLessEqual({"scopes", "duration_seconds"}, set(schemas["CreateApiKeyArgs"]["properties"]))

    def test_local_references_resolve(self):
        def visit(value):
            if isinstance(value, dict):
                if "$ref" in value and value["$ref"].startswith("#/"):
                    reference = value["$ref"]
                    with self.subTest(reference=reference):
                        target = self.spec
                        for part in reference[2:].split("/"):
                            part = part.replace("~1", "/").replace("~0", "~")
                            self.assertIn(part, target)
                            target = target[part]
                for child in value.values():
                    visit(child)
            elif isinstance(value, list):
                for child in value:
                    visit(child)

        visit(self.spec)


class GkmRoutesTest(unittest.TestCase):
    """The routes of sidecar DESIGN.md §10.14, and check-routes.sh's hold on them."""

    methods = {"get", "put", "post", "delete", "patch"}
    expected = {
        "/gkm/key-packages": {"post"},
        "/gkm/key-packages/claim": {"post"},
        "/gkm/reader-key": {"post"},
        "/gkm/reader-key/{sidecar_id}": {"get"},
        "/gkm/reader-keys": {"post"},
        "/gkm/groups": {"post"},
        "/gkm/groups/{group_id}": {"get"},
        "/gkm/repos/{repo_id}/forms": {"get", "post"},
        "/gkm/repos/{repo_id}/drop-key": {"get"},
        "/gkm/groups/{group_id}/epochs": {"get"},
        "/gkm/groups/{group_id}/epochs/{n}": {"post"},
        "/gkm/groups/{group_id}/gen": {"get"},
        "/gkm/groups/{group_id}/gen/{g}": {"post"},
        "/gkm/groups/{group_id}/wraps/{g}": {"get"},
        "/gkm/groups/{group_id}/wraps/{g}/{c8}": {"post"},
        "/gkm/groups/{group_id}/recovery-wraps/{g}/{c8}": {"post"},
        "/gkm/groups/{group_id}/ladder": {"post"},
        "/gkm/groups/{group_id}/welcome/{n}": {"post"},
        "/gkm/groups/{group_id}/group-info": {"get", "post"},
        "/gkm/groups/{group_id}/ack": {"post"},
        "/gkm/welcomes": {"get"},
        "/gkm/recovery/{member_id}/wraps": {"get", "post", "delete"},
    }

    @classmethod
    def setUpClass(cls):
        cls.script = Path(__file__).with_name("check-routes.sh")
        cls.spec = yaml.safe_load(Path(__file__).with_name("bringyour.yml").read_text())

    def operations(self, gkm):
        """The spec's operations on one side of the carve-out, as (method, path)."""
        return [
            (method, path)
            for path, item in self.spec["paths"].items()
            if path.startswith("/gkm/") == gkm
            for method in self.methods & set(item)
        ]

    def on_api(self, path):
        """Whether api serves path: it names no servers of its own, or its own name
        one of the document's."""
        own = self.spec["paths"][path].get("servers")
        if not own:
            return True
        return bool({server["url"] for server in own} & {server["url"] for server in self.spec["servers"]})

    def table(self):
        """gkmservice's routes.go, holding every documented route."""
        lines = [f'\t"{method.upper()} {path}",' for method, path in self.operations(True)]
        return "\n".join(["var routes = []string{", *lines, "}"])

    def table_without(self, route):
        """table() less one "METHOD /path" route."""
        table = self.table()
        line = f'\t"{route}",\n'
        self.assertIn(line, table)
        return table.replace(line, "")

    def unplanned(self, method, path):
        """bringyour.yml's text with one operation's planned mark lifted, as the
        task that serves it lifts it."""
        text = Path(__file__).with_name("bringyour.yml").read_text()
        start = text.index(f"\n  {path}:\n")
        end = text.index("\n  /", start + 1)
        operation = text.index(f"\n    {method}:\n", start, end)
        mark = "\n      x-status: planned\n"
        at = text.index(mark, operation, end)
        return text[:at] + text[at + len(mark) - 1 :]

    def check_routes(self, table=None, api_routes=(), spec=None, symlinked=False, competition=None):
        """check-routes.sh on a fixture beside a routes.go of table, or none, whose
        api serves the rest of the spec and api_routes. A spec given is the
        bringyour.yml the script reads in place of this one. A symlinked server
        links to a checkout elsewhere, as a track's does, its siblings still
        beside the link, and competition is the sn/api/competition.yml there."""
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        script = self.script
        if spec is not None:
            (root / "connect/api").mkdir(parents=True)
            script = root / "connect/api" / self.script.name
            shutil.copy(self.script, script)
            (root / "connect/api/bringyour.yml").write_text(spec)
        server = root / "checkout/server" if symlinked else root / "server"
        (server / "api").mkdir(parents=True)
        if symlinked:
            (root / "server").symlink_to(server, target_is_directory=True)
        registered = [
            (method.upper(), re.sub(r"{[^}]*}", "([^/]+)", path))
            for method, path in self.operations(False)
            if self.on_api(path)
        ]
        (server / "api/api.go").write_text(
            "\n".join(
                f'router.NewRoute("{method}", "{path}", handler)'
                for method, path in [*registered, *api_routes]
            )
        )
        if table is not None:
            (root / "sidecar/gkmservice").mkdir(parents=True)
            (root / "sidecar/gkmservice/routes.go").write_text(table)
        if competition is not None:
            (root / "sn/api").mkdir(parents=True)
            (root / "sn/api/competition.yml").write_text(competition)
        return subprocess.run(
            ["zsh", str(script), str(root / "server")], capture_output=True, text=True
        )

    def test_every_route_is_documented_planned_and_on_gkms_own_host(self):
        paths = {path: item for path, item in self.spec["paths"].items() if path.startswith("/gkm/")}
        self.assertEqual(set(self.expected), set(paths))
        api_host = urlparse(self.spec["servers"][0]["url"]).hostname
        gkm_host = "gkm." + api_host.split(".", 1)[1]
        for path, want in self.expected.items():
            with self.subTest(path=path):
                item = paths[path]
                self.assertEqual(want, self.methods & set(item))
                self.assertEqual(gkm_host, urlparse(item["servers"][0]["url"]).hostname)
                for method in want:
                    operation = item[method]
                    self.assertTrue(operation["x-internal"])
                    self.assertEqual("planned", operation["x-status"])
                    self.assertEqual([{"ServiceTokenAuth": []}], operation["security"])

    def test_every_body_is_binary(self):
        # the messages of sidecar's protocol/gkm.proto, which a schema here would restate
        for method, path in self.operations(True):
            operation = self.spec["paths"][path][method]
            with self.subTest(path=path, method=method):
                bodies = [operation.get("requestBody", {}), *operation["responses"].values()]
                for body in bodies:
                    self.assertLessEqual(set(body.get("content", {})), {"application/octet-stream"})

    def description(self, method, path):
        """An operation's description on one line."""
        return " ".join(self.spec["paths"][path][method]["description"].split())

    def test_only_drop_key_takes_a_deposit_only_token(self):
        # QD-177: a token whose only repo scopes are deposit reaches GET drop-key
        # and nothing else at gkm, so every other route asks for r, rw or admin
        for method, path in self.operations(True):
            description = self.description(method, path)
            with self.subTest(path=path, method=method):
                if (method, path) == ("get", "/gkm/repos/{repo_id}/drop-key"):
                    self.assertIn("`deposit`", description)
                else:
                    self.assertNotIn("`deposit`", description)
                    self.assertRegex(description, r"`(r|rw|admin)`")

    def test_a_reform_files_its_epoch_zero_with_its_form(self):
        # 10.6's checks: a re-form writes the GroupForm, epoch 0, ladder and first
        # wraps. Only the forms route can carry that epoch 0: POST /gkm/groups is
        # creation's, and an epoch POST needs the epoch before it
        description = self.description("post", "/gkm/repos/{repo_id}/forms")
        self.assertTrue(description.startswith("A `GkmGroupReformRequest`"), description)
        self.assertIn("epoch 0", description)

    def test_a_deleted_reseed_takes_the_form_and_the_ladder(self):
        # 10.8: a repo re-seeded as deleted takes only its GroupForm, with its
        # epoch 0 and ladder. QD-074 opens the ladder to every re-form, a stalled
        # group's and an IANA renumbering's too, and to that one in re-seed mode
        taking = {
            (method, path)
            for method, path in self.operations(True)
            if "re-seeded as deleted" in self.description(method, path)
        }
        self.assertEqual(
            {("post", "/gkm/repos/{repo_id}/forms"), ("post", "/gkm/groups/{group_id}/ladder")}, taking
        )
        self.assertIn("Open to every re-form", self.description("post", "/gkm/groups/{group_id}/ladder"))

    def test_a_recovery_wraps_412_names_its_unsigned_winner(self):
        # QD-468: a RecoveryWrap is unsigned, bare enc || ct, so the winner its
        # 412 carries cannot be the wraps route's Signed delivery
        responses = self.spec["paths"]["/gkm/groups/{group_id}/recovery-wraps/{g}/{c8}"]["post"]["responses"]
        self.assertIn("bare ciphertext", responses["200"]["description"])

    def test_statuses_are_the_designs(self):
        # a wrap's own 201, 412 and 409 ride its batch's 200
        for path, want in {
            "/gkm/groups/{group_id}/epochs/{n}": {"200", "403", "409", "429"},
            "/gkm/groups/{group_id}/gen/{g}": {"201", "403", "412", "429"},
            "/gkm/groups/{group_id}/wraps/{g}/{c8}": {"200", "403", "429"},
            "/gkm/groups/{group_id}/recovery-wraps/{g}/{c8}": {"200", "403", "429"},
        }.items():
            with self.subTest(path=path):
                self.assertEqual(want, set(self.spec["paths"][path]["post"]["responses"]))

    def test_a_conflict_carries_the_winner(self):
        for path, status in (
            ("/gkm/groups/{group_id}/epochs/{n}", "409"),
            ("/gkm/groups/{group_id}/gen/{g}", "412"),
        ):
            with self.subTest(path=path):
                response = self.spec["paths"][path]["post"]["responses"][status]
                self.assertIn("application/octet-stream", response["content"])

    def test_check_routes_says_when_it_could_not_read_the_table(self):
        result = self.check_routes()
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn("NOT CHECKED", result.stdout)

    def test_check_routes_passes_a_whole_table(self):
        result = self.check_routes(self.table())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertNotIn("NOT CHECKED (no", result.stdout.split("ok:")[0])
        count = len(self.operations(True))
        self.assertIn(f"gkm: {count} routes in gkmservice's table, {count} documented", result.stdout)

    def test_check_routes_lets_a_planned_route_wait(self):
        # gkmservice's table grows a task at a time, as api's does
        result = self.check_routes(self.table_without("POST /gkm/groups/{group_id}/ack"))
        self.assertEqual(0, result.returncode, result.stderr)
        count = len(self.operations(True))
        self.assertIn(f"gkm: {count - 1} routes in gkmservice's table, {count} documented", result.stdout)

    def test_check_routes_refuses_a_served_route_the_table_lacks(self):
        result = self.check_routes(
            self.table_without("POST /gkm/groups/{group_id}/ack"),
            spec=self.unplanned("post", "/gkm/groups/{group_id}/ack"),
        )
        self.assertEqual(1, result.returncode)
        self.assertIn("in bringyour.yml but NOT in gkmservice's route table", result.stderr)
        self.assertIn("POST /gkm/groups/*/ack", result.stderr)

    def test_check_routes_refuses_a_table_route_the_spec_lacks(self):
        table = self.table().removesuffix("}") + '\t"GET /gkm/undocumented",\n}'
        result = self.check_routes(table)
        self.assertEqual(1, result.returncode)
        self.assertIn("in gkmservice's route table but NOT in bringyour.yml", result.stderr)
        self.assertIn("GET /gkm/undocumented", result.stderr)
        self.assertNotIn("in bringyour.yml but NOT in gkmservice's route table", result.stderr)

    def test_check_routes_refuses_a_table_it_cannot_read(self):
        # a table in another shape must not pass by naming no route
        result = self.check_routes("var routes = []route{}")
        self.assertEqual(1, result.returncode)
        self.assertIn("gkmservice's route table names no route", result.stderr)

    def test_check_routes_reads_the_table_beside_a_symlinked_server(self):
        # a track's server links to the main checkout, and the track's own sidecar
        # sits beside the link, not beside its target
        result = self.check_routes("var routes = []route{}", symlinked=True)
        self.assertEqual(1, result.returncode, result.stdout)
        self.assertIn("gkmservice's route table names no route", result.stderr)
        result = self.check_routes(self.table(), symlinked=True)
        self.assertEqual(0, result.returncode, result.stderr)
        count = len(self.operations(True))
        self.assertIn(f"gkm: {count} routes in gkmservice's table, {count} documented", result.stdout)

    def test_check_routes_reads_competition_beside_a_symlinked_server(self):
        # the same lookup finds sn/api/competition.yml
        competition = "paths:\n  /competition/info:\n    get:\n      summary: Read\n"
        result = self.check_routes(self.table(), symlinked=True, competition=competition)
        self.assertEqual(1, result.returncode, result.stdout)
        self.assertIn("in sn/api/competition.yml but NOT registered", result.stderr)
        self.assertIn("GET /competition/info", result.stderr)

    def test_check_routes_refuses_a_gkm_route_api_registers(self):
        result = self.check_routes(self.table(), api_routes=[("GET", "/gkm/welcomes")])
        self.assertEqual(1, result.returncode)
        self.assertIn("registered by api but served by gkm", result.stderr)
        self.assertIn("GET /gkm/welcomes", result.stderr)

    def test_check_routes_leaves_other_hosts_routes_to_them(self):
        # evs's and sockets' paths name servers of their own: api serves none of
        # them, and a route evs serves, out of the planned list, is not one api
        # lacks
        other = [(method, path) for method, path in self.operations(False) if not self.on_api(path)]
        self.assertTrue(any(path.startswith("/evs/") for _, path in other))
        self.assertTrue(any(path.startswith("/socket/") for _, path in other))
        served = [(method, path) for method, path in other if "x-status" not in self.spec["paths"][path][method]]
        self.assertTrue(served)
        result = self.check_routes(self.table())
        self.assertEqual(0, result.returncode, result.stderr)
        self.assertIn(f"other hosts: {len(other)} documented operations", result.stdout)

    def test_check_routes_refuses_an_other_hosts_route_api_registers(self):
        result = self.check_routes(self.table(), api_routes=[("PUT", "/evs/v1/repos/([^/]+)")])
        self.assertEqual(1, result.returncode)
        self.assertIn("registered by api but served on another host", result.stderr)
        self.assertIn("PUT /evs/v1/repos/*", result.stderr)
        self.assertNotIn("registered but NOT in bringyour.yml", result.stderr)

    def test_check_routes_holds_a_path_whose_servers_name_api(self):
        # a path whose own servers name one of api's hosts is still api's
        text = Path(__file__).with_name("bringyour.yml").read_text()
        at = text.index("\n  /evs/v1/repos/{repo_id}:\n")
        extra = (
            "\n  /network/v4-only:\n"
            "    servers:\n"
            f"      - url: {self.spec['servers'][1]['url']}\n"
            "    get:\n"
            "      summary: Read\n"
        )
        result = self.check_routes(self.table(), spec=text[:at] + extra + text[at:])
        self.assertEqual(1, result.returncode)
        self.assertIn("in bringyour.yml but NOT registered", result.stderr)
        self.assertIn("GET /network/v4-only", result.stderr)

    def test_check_routes_still_refuses_api_drift(self):
        # the carve-out must not take the rest of the spec out of the check
        result = self.check_routes(self.table(), api_routes=[("GET", "/undocumented")])
        self.assertEqual(1, result.returncode)
        self.assertIn("registered but NOT in bringyour.yml", result.stderr)


if __name__ == "__main__":
    unittest.main()

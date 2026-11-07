"""Regression checks for the published OpenAPI document (requires PyYAML)."""

import re
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
        "/gkm/groups": {"post"},
        "/gkm/groups/{group_id}": {"get"},
        "/gkm/repos/{repo_id}/forms": {"post"},
        "/gkm/groups/{group_id}/epochs": {"get"},
        "/gkm/groups/{group_id}/epochs/{n}": {"post"},
        "/gkm/groups/{group_id}/gen": {"get"},
        "/gkm/groups/{group_id}/gen/{g}": {"post"},
        "/gkm/groups/{group_id}/wraps/{g}": {"get"},
        "/gkm/groups/{group_id}/wraps/{g}/{c8}": {"post"},
        "/gkm/groups/{group_id}/welcome/{n}": {"post"},
        "/gkm/groups/{group_id}/group-info": {"get", "post"},
        "/gkm/groups/{group_id}/ack": {"post"},
        "/gkm/groups/{group_id}/drop-key": {"get"},
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

    def table(self):
        """gkmservice's routes.go, holding every documented route."""
        lines = [f'\t"{method.upper()} {path}",' for method, path in self.operations(True)]
        return "\n".join(["var routes = []string{", *lines, "}"])

    def check_routes(self, table=None, api_routes=()):
        """check-routes.sh on a fixture beside a routes.go of table, or none, whose
        api serves the rest of the spec and api_routes."""
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = Path(directory.name)
        (root / "server/api").mkdir(parents=True)
        registered = [
            (method.upper(), re.sub(r"{[^}]*}", "([^/]+)", path))
            for method, path in self.operations(False)
        ]
        (root / "server/api/api.go").write_text(
            "\n".join(
                f'router.NewRoute("{method}", "{path}", handler)'
                for method, path in [*registered, *api_routes]
            )
        )
        if table is not None:
            (root / "sidecar/gkmservice").mkdir(parents=True)
            (root / "sidecar/gkmservice/routes.go").write_text(table)
        return subprocess.run(
            ["zsh", str(self.script), str(root / "server")], capture_output=True, text=True
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

    def test_statuses_are_the_designs(self):
        for path, want in {
            "/gkm/groups/{group_id}/epochs/{n}": {"200", "403", "409", "429"},
            "/gkm/groups/{group_id}/gen/{g}": {"201", "403", "412", "429"},
            "/gkm/groups/{group_id}/wraps/{g}/{c8}": {"201", "403", "409", "412", "429"},
        }.items():
            with self.subTest(path=path):
                self.assertEqual(want, set(self.spec["paths"][path]["post"]["responses"]))

    def test_a_conflict_carries_the_winner(self):
        for path, status in (
            ("/gkm/groups/{group_id}/epochs/{n}", "409"),
            ("/gkm/groups/{group_id}/gen/{g}", "412"),
            ("/gkm/groups/{group_id}/wraps/{g}/{c8}", "412"),
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

    def test_check_routes_refuses_a_documented_route_the_table_lacks(self):
        lines = self.table().splitlines()
        dropped = lines.pop(1)
        result = self.check_routes("\n".join(lines))
        self.assertEqual(1, result.returncode)
        self.assertIn("in bringyour.yml but NOT in gkmservice's route table", result.stderr)
        self.assertIn(re.sub(r"{[^}]*}", "*", dropped.strip(' \t",')), result.stderr)

    def test_check_routes_refuses_a_table_route_the_spec_lacks(self):
        table = self.table().replace("}", '\t"GET /gkm/undocumented",\n}')
        result = self.check_routes(table)
        self.assertEqual(1, result.returncode)
        self.assertIn("in gkmservice's route table but NOT in bringyour.yml", result.stderr)
        self.assertIn("GET /gkm/undocumented", result.stderr)

    def test_check_routes_refuses_a_table_it_cannot_read(self):
        # a table in another shape must not pass by naming no route
        result = self.check_routes("var routes = []route{}")
        self.assertEqual(1, result.returncode)
        self.assertIn("in bringyour.yml but NOT in gkmservice's route table", result.stderr)

    def test_check_routes_refuses_a_gkm_route_api_registers(self):
        result = self.check_routes(self.table(), api_routes=[("GET", "/gkm/welcomes")])
        self.assertEqual(1, result.returncode)
        self.assertIn("registered by api but served by gkm", result.stderr)
        self.assertIn("GET /gkm/welcomes", result.stderr)

    def test_check_routes_still_refuses_api_drift(self):
        # the carve-out must not take the rest of the spec out of the check
        result = self.check_routes(self.table(), api_routes=[("GET", "/undocumented")])
        self.assertEqual(1, result.returncode)
        self.assertIn("registered but NOT in bringyour.yml", result.stderr)


if __name__ == "__main__":
    unittest.main()

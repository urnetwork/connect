"""Regression checks for the published OpenAPI document (requires PyYAML)."""

import unittest
from pathlib import Path

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

    def test_sidecar_routes_are_documented_and_planned(self):
        # the routes of sidecar DESIGN.md §7, §8 and §11. Each stays planned and
        # out of the public api until a service serves it; the owner of a route
        # moves it out of this list when it does.
        expected = {
            "/network/enroll": {"post"},
            "/network/auth-keys": {"get", "post"},
            "/network/auth-key-revoke": {"post"},
            "/network/config/projection": {"get", "post"},
            "/network/config/delta": {"post"},
            "/network/config/reseed": {"post"},
            "/acl/ack": {"post"},
            "/acl/invite": {"post"},
            "/acl/accept-invite": {"post"},
            "/evs/v1/repos/{repo_id}": {"put", "get", "delete"},
            "/evs/v1/repos/{repo_id}/restore": {"post"},
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
        for path, want in expected.items():
            with self.subTest(path=path):
                self.assertIn(path, paths)
                item = paths[path]
                self.assertEqual(want, methods & set(item))
                for method in want:
                    operation = item[method]
                    self.assertTrue(operation["x-internal"])
                    self.assertEqual("planned", operation["x-status"])
                if path.startswith("/evs/"):
                    self.assertEqual("https://evs.bringyour.com", item["servers"][0]["url"])
                    for method in want:
                        self.assertEqual([{"ServiceTokenAuth": []}], item[method]["security"])
                else:
                    self.assertNotIn("servers", item)
        enroll = paths["/network/enroll"]["post"]
        self.assertEqual([], enroll["security"])
        args = self.spec["components"]["schemas"]["NetworkEnrollArgs"]
        self.assertNotIn("bundle", args["properties"])

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


if __name__ == "__main__":
    unittest.main()

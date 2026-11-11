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

    def test_256gib_pack_is_sold_over_x402_and_gamma_only(self):
        # config/all/pro.yml: data_256gib, 256 GiB for $0.75, the pack for
        # clients with a $1 per-payment cap, rails [x402, gamma] (the owner,
        # 2026-11-11). The server refuses it on every other path.
        pack = "data_256gib"
        schemas = self.spec["components"]["schemas"]
        paths = self.spec["paths"]

        for name, field in (
            ("GammaTermsItem", "item_id"),
            ("CreateGammaPaymentIntentArgs", "item_id"),
            ("GammaSettingsArgs", "topup_item_id"),
        ):
            with self.subTest(schema=name):
                self.assertIn(pack, schemas[name]["properties"][field]["enum"])
        terms = paths["/pay/gamma/terms"]["get"]["responses"]["200"]
        for name, example in terms["content"]["application/json"]["examples"].items():
            with self.subTest(example=name):
                items = {
                    item["item_id"]: item
                    for item in example["value"]["items"]
                    if "item_id" in item
                }
                self.assertEqual("0.75", items[pack]["price_gamma"])
                self.assertEqual(0.75, items[pack]["price_usd"])
                self.assertEqual("0.75", items[pack]["min_daily_cap_gamma"])
        self.assertIn(pack, paths["/pay/gamma/intent"]["post"]["description"])

        skus = paths["/x402/skus"]["get"]["description"]
        self.assertIn(pack, skus)
        self.assertIn("$0.75", skus)
        self.assertIn("$1 per-payment cap", skus)
        self.assertIn(pack, schemas["X402Sku"]["properties"]["sku_id"]["description"])
        self.assertIn(pack, paths["/x402/purchase"]["post"]["description"])

        # card, the crypto invoices and USDC by Solana Pay never offer it
        for name in (
            "PayDataCheckoutArgs",
            "PayDataSolanaIntentArgs",
            "PayDataSolanaStatusResult",
            "CryptoPaymentQuoteArgs",
            "CreateCryptoPaymentIntentArgs",
            "CreateDataCryptoPaymentIntentArgs",
        ):
            with self.subTest(schema=name):
                enum = schemas[name]["properties"]["item_id"]["enum"]
                self.assertEqual(["data_1tib", "data_10tib"], enum)
        card = schemas["StripeCreateCheckoutSessionArgs"]["properties"]["item_id"]
        self.assertIn(pack, card["description"])
        self.assertIn("item_unavailable", card["description"])

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

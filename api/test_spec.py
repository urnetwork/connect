"""Regression checks for the published OpenAPI document (requires PyYAML)."""

import unittest
from pathlib import Path

import yaml


class SpecTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.spec = yaml.safe_load(Path(__file__).with_name("bringyour.yml").read_text())

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

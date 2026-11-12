import copy
import unittest
from build_http_expiry_catalog import build, BASE_SOURCE, SOURCE


class CatalogTest(unittest.TestCase):
    def setUp(self):
        self.base = {"schema": "thingspanel.plugin-catalog.v1", "version": "1.2.0",
                     "release_tag": "plugin-http-v1.2.0", "target_platform": "linux/amd64",
                     "agent_sha256": "agent", "published_at": 1, "expires_at": 9999999999,
                     "plugins": [{"id": "homeassistant", "image_archive_sha256": "ha",
                                  "identity": {"token_env": "TP_PLUGIN_RUNTIME_TOKEN"}, "version": "1.0.0"},
                                 {"id": "http-adapter", "version": "1.2.0", "source_commit": BASE_SOURCE,
                                  "entrypoint": "/app/tp-plugin-http", "args": ["-c", "/app/config/config.yaml"],
                                  "identity": {"token_env": "P_PLATFORM_API_TOKEN"},
                                  "env_from_stack": {"P_PLATFORM_MQTT_PASSWORD": "TP_HTTP_MQTT_PASSWORD"},
                                  "platform": {"network_alias": "tp-plugin-http-adapter"}}]}
        self.image = {"Os": "linux", "Architecture": "amd64", "Id": "sha256:" + "a" * 64,
                      "Config": {"Entrypoint": ["/app/tp-plugin-http", "-c", "/app/config/config.yaml"]}}

    def render(self):
        return build(self.base, self.image, "amd64", "agent", "ha", "newarchive", "newinstaller", 123)

    def test_only_http_version_and_image_change(self):
        before = copy.deepcopy(self.base)
        result = self.render()
        self.assertEqual(self.base, before)
        self.assertEqual(result["plugins"][0], before["plugins"][0])
        self.assertEqual(result["agent_sha256"], before["agent_sha256"])
        self.assertEqual(result["version"], "1.2.1")
        self.assertEqual(result["release_tag"], "plugin-http-v1.2.1")
        new, old = result["plugins"][1], before["plugins"][1]
        self.assertEqual(new["source_commit"], SOURCE)
        self.assertEqual(new["image_archive_sha256"], "newarchive")
        self.assertEqual(new["image_id"], self.image["Id"])
        for key in ("identity", "env_from_stack", "platform", "entrypoint", "args"):
            self.assertEqual(new[key], old[key])

    def test_archive_mismatch_rejected(self):
        for agent, ha in (("wrong", "ha"), ("agent", "wrong")):
            with self.subTest(agent=agent, ha=ha), self.assertRaises(ValueError):
                build(self.base, self.image, "amd64", agent, ha, "new", "commit", 123)

    def test_wrong_base_and_extra_plugin_rejected(self):
        for change in (lambda b: b.update(version="1.2.1"),
                       lambda b: b["plugins"][1].update(source_commit="unreviewed"),
                       lambda b: b["plugins"].append({"id": "unexpected"})):
            original = copy.deepcopy(self.base)
            change(self.base)
            with self.assertRaises(ValueError):
                self.render()
            self.base = original

    def test_wrong_image_contract_rejected(self):
        for change in (lambda i: i.update(Architecture="arm64"),
                       lambda i: i["Config"].update(Labels={"unexpected": "label"}),
                       lambda i: i["Config"].update(Entrypoint=["/unexpected"]),
                       lambda i: i.update(Id="mutable-tag")):
            self.image = copy.deepcopy(self.image)
            saved = copy.deepcopy(self.image)
            change(self.image)
            with self.assertRaises(ValueError):
                self.render()
            self.image = saved


if __name__ == "__main__":
    unittest.main()

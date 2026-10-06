import json
import tempfile
import unittest
from pathlib import Path

from scrub_fixtures import Scrubber, scrub_dir

TOKEN = "0123456789abcdef0123456789abcdef"
USER = "fedcba9876543210fedcba9876543210"


class ScrubberTest(unittest.TestCase):
    def run_scrub(self, doc, literals=None):
        s = Scrubber(literals)
        s.discover(doc)
        text = s.scrub_text(json.dumps(s.redact_structured(doc)), s.replacements())
        return s, text

    def test_same_shape_and_all_spellings(self):
        dashed_user = f"{USER[:8]}-{USER[8:12]}-{USER[12:16]}-{USER[16:20]}-{USER[20:]}"
        doc = {"AccessToken": TOKEN, "User": {"Id": USER, "Name": "alice", "Policy": {}},
               "path": f"/Users/{dashed_user}/Items?api_key={TOKEN.upper()}"}
        s, text = self.run_scrub(doc)
        out = json.loads(text)
        self.assertEqual(out["AccessToken"], "aaaa" + "0" * 27 + "1")
        self.assertEqual(out["User"]["Id"], "bbbb" + "0" * 27 + "1")
        self.assertEqual(out["User"]["Name"], "user1")
        self.assertIn("/Users/bbbb0000-", out["path"])
        self.assertNotIn(TOKEN.upper(), text)
        self.assertEqual(s.leaks(text), [])

    def test_password_redacted(self):
        _, text = self.run_scrub({"Username": "alice", "Pw": "hunter22"})
        self.assertNotIn("hunter22", text)
        self.assertIn('"Pw": "REDACTED"', text)

    def test_ips_hosts_sig_debrid(self):
        doc = {"Path": "http://mediabox:8097/stream/realdebrid/ABCDEFGHIJKLM/1?sig=_xYz9", "Remote": "100.64.12.34",
               "Local": "http://10.89.1.2:8096", "H": "mediabox.tail1234.ts.net", "Abi": "12.1.0.0"}
        _, text = self.run_scrub(doc, literals=["mediabox"])
        out = json.loads(text)
        self.assertEqual(out["Path"], "http://redacted1:8097/stream/realdebrid/TORRENTID/1?sig=REDACTED")
        self.assertTrue(out["Remote"].startswith("192.0.2."))
        self.assertEqual(out["Abi"], "12.1.0.0")
        self.assertEqual(out["H"], "host.example.ts.net")
        self.assertNotIn("mediabox", text)

    def test_android_user_agent(self):
        ua = "Dalvik/2.1.0 (Linux; U; Android 14; moto g power 5G - 2023 Build/U1TOS34.1-157-5-25)"
        s, text = self.run_scrub({"User-Agent": ua})
        self.assertEqual(json.loads(text)["User-Agent"], "Dalvik/2.1.0 (Linux; U; Android 14; Device Build/REDACTED)")
        self.assertEqual(s.leaks(text), [])
        self.assertEqual(s.leaks(ua), ["android-ua"])

    def test_placeholder_tokens_ignored(self):
        doc = {"request": {"auth": {"Token": "null", "DeviceId": "undefined"}}, "body": None, "x": "nullable"}
        s, text = self.run_scrub(doc)
        self.assertEqual(json.loads(text)["body"], None)
        self.assertIn("nullable", text)
        self.assertEqual(s.maps.get("token", {}), {})

    def test_item_ids_are_kept(self):
        item = "84088e11eb6351255c08507602d79a0f"
        _, text = self.run_scrub({"Items": [{"Id": item, "Name": "Luca"}]})
        self.assertIn(item, text)
        self.assertIn("Luca", text)

    def test_leak_check_flags_survivors(self):
        s = Scrubber(["mediabox"])
        s.add("token", TOKEN)
        self.assertEqual(sorted(s.leaks(f"x {TOKEN} 192.168.1.4 mediabox sig=abc")),
                         ["literal:me…", "private-ip", "sig", "token:0123…"])

    def test_leak_check_aborts_and_removes_output(self):
        with tempfile.TemporaryDirectory() as d:
            src, dst = Path(d, "raw"), Path(d, "out")
            src.mkdir()
            (src / "0001-GET-x.json").write_text(json.dumps({"AccessToken": TOKEN}))
            orig = Scrubber.scrub_text
            Scrubber.scrub_text = lambda self, text, pairs: text  # simulate a replacement bug
            try:
                with self.assertRaises(SystemExit):
                    scrub_dir(src, dst, [])
            finally:
                Scrubber.scrub_text = orig
            self.assertFalse(dst.exists())

    def test_literal_list_scrubbed(self):
        with tempfile.TemporaryDirectory() as d:
            src, dst = Path(d, "raw"), Path(d, "out")
            src.mkdir()
            # A token in a key we don't know about can't be discovered, but the literal list catches it.
            (src / "0001-GET-x.json").write_text(json.dumps({"odd": "secret-literal"}))
            self.assertEqual(scrub_dir(src, dst, ["secret-literal"]), 1)
            self.assertNotIn("secret-literal", (dst / "0001-GET-x.json").read_text())


if __name__ == "__main__":
    unittest.main()

import unittest

import re

from mitm2fixtures import keep_flow, normalize_path, parse_auth, slug


class NormalizePath(unittest.TestCase):
    def test_ids_and_query(self):
        cases = {
            "/Items/84088e11-eb63-5125-5c08-507602d79a0f/PlaybackInfo?UserId=x": "/Items/{id}/PlaybackInfo",
            "/items/84088e11eb6351255c08507602d79a0f/Images/Primary": "/items/{id}/Images/Primary",
            "/Users/Public": "/Users/Public",
            "/Videos/551ae82823de5533467c046377c55afb/hls1/main/12.ts": "/Videos/{id}/hls1/main/{n}.ts",
            "/Videos/551ae82823de5533467c046377c55afb/{id}/Subtitles/3/0/Stream.vtt": "/Videos/{id}/{id}/Subtitles/{n}/{n}/Stream.vtt",
            "/items/551ae82823de5533467c046377c55afb/Images/Backdrop/0": "/items/{id}/Images/Backdrop/{n}",
            "/Shows/NextUp": "/Shows/NextUp",
        }
        for raw, want in cases.items():
            self.assertEqual(normalize_path(raw), want, raw)

    def test_does_not_eat_longer_hex(self):
        self.assertEqual(normalize_path("/x/" + "a" * 40), "/x/" + "a" * 40)


class ParseAuth(unittest.TestCase):
    def test_mediabrowser_header(self):
        h = {"Authorization": 'MediaBrowser Client="Findroid", Device="Pixel 8", DeviceId="abc", Version="1.1.0", Token="tok"'}
        a = parse_auth(h, {})
        self.assertEqual(a["Client"], "Findroid")
        self.assertEqual(a["Token"], "tok")
        self.assertEqual(a["_source"], "authorization")

    def test_emby_header_and_token_header(self):
        a = parse_auth({"X-Emby-Authorization": 'Emby Client="Kodi", DeviceId="d"', "X-Emby-Token": "t2"}, {})
        self.assertEqual((a["Client"], a["Token"]), ("Kodi", "t2"))

    def test_query_api_key_case_insensitive(self):
        self.assertEqual(parse_auth({}, {"ApiKey": ["q1"]})["Token"], "q1")
        self.assertEqual(parse_auth({}, {"api_key": ["q2"]})["_source"], "query:api_key")

    def test_none(self):
        self.assertIsNone(parse_auth({"Accept": "*/*"}, {}))


class KeepFlow(unittest.TestCase):
    def test_split_by_client_then_ua(self):
        kc, ku = re.compile("^Jellyfin for Android$"), re.compile(r"; wv\)|^Jellyfin for Android/")
        self.assertTrue(keep_flow({"Client": "Jellyfin%20for%20Android"}, "Mozilla/5.0 (Linux; wv)", kc, ku))
        self.assertTrue(keep_flow({"Client": "Jellyfin+for+Android"}, "okhttp/4.12.0", kc, ku))
        self.assertFalse(keep_flow({"Client": "Streamyfin"}, "Mozilla/5.0 (Linux; wv)", kc, ku))
        self.assertTrue(keep_flow({"Token": "t"}, "Mozilla/5.0 (Linux; Android 14; x; wv) AppleWebKit", kc, ku))
        self.assertFalse(keep_flow(None, "libmpv", kc, ku))
        self.assertTrue(keep_flow(None, "anything", None, None))


class Slug(unittest.TestCase):
    def test_slug(self):
        self.assertEqual(slug("/Items/{id}/PlaybackInfo"), "Items-id-PlaybackInfo")
        self.assertEqual(slug("/"), "root")


if __name__ == "__main__":
    unittest.main()

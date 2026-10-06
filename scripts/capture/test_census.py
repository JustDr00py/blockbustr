import unittest

from census import collapse_static, design_routes, expand, matches

DESIGN = """### 3.5 MVP endpoints
- **Items:** `GET /Items` (+ `/Users/{id}/Items`), `/Items/{id}`, `/Items/Filters`
- **Playback:** `POST|GET /Items/{id}/PlaybackInfo`, `GET|HEAD /Videos/{id}/stream[.{container}]`
- **Images:** `GET|HEAD /Items/{id}/Images/{type}[/{index}]` with `maxWidth,maxHeight`
- **QC:** `/QuickConnect/Enabled|Initiate`, `/Plugins` (`[]`)
- **User data:** `POST/DELETE /UserPlayedItems/{id}`

## 4. Data model
`/NotARoute`
"""


class Census(unittest.TestCase):
    def setUp(self):
        self.routes = design_routes(DESIGN)

    def test_expand(self):
        self.assertEqual(sorted(expand("/V/{id}/stream[.{c}]")), ["/V/{id}/stream", "/V/{id}/stream.{c}"])
        self.assertEqual(expand("/QC/Enabled|Initiate"), ["/QC/Enabled", "/QC/Initiate"])

    def test_matching(self):
        m = lambda meth, p: matches(self.routes, meth, p)
        self.assertEqual(m("GET", "/items/{id}/Images/Primary"), "/Items/{id}/Images/{type}")
        self.assertEqual(m("GET", "/Items/{id}/Images/Backdrop/0"), "/Items/{id}/Images/{type}/{index}")
        self.assertEqual(m("POST", "/Items/{id}/PlaybackInfo"), "/Items/{id}/PlaybackInfo")
        self.assertIsNone(m("DELETE", "/Items/{id}/PlaybackInfo"))  # method not listed
        self.assertEqual(m("GET", "/Videos/{id}/stream.mkv"), "/Videos/{id}/stream.{container}")
        self.assertEqual(m("DELETE", "/UserPlayedItems/{id}"), "/UserPlayedItems/{id}")
        self.assertEqual(m("GET", "/QuickConnect/Initiate"), "/QuickConnect/Initiate")
        self.assertIsNone(m("GET", "/Items/Suggestions"))

    def test_collapse_static(self):
        self.assertEqual(collapse_static("GET /web/main.jellyfin.bundle.js"), "GET /web/{file}")
        self.assertEqual(collapse_static("GET /web/config.json"), "GET /web/config.json")
        self.assertEqual(collapse_static("GET /web/ConfigurationPages"), "GET /web/ConfigurationPages")

    def test_only_section_3_5(self):
        self.assertNotIn("/NotARoute", {d for _, d, _ in self.routes})
        self.assertNotIn("[]", {d for _, d, _ in self.routes})


if __name__ == "__main__":
    unittest.main()

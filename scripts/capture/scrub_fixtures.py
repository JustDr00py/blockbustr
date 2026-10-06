#!/usr/bin/env python3
"""Scrub raw fixtures into committable ones (TASKS P0.5). Stdlib only.

  scripts/capture/scrub_fixtures.py findroid-1.1.0 [scenario]
    reads  testdata/jellyfin/_raw/<client>/<scenario>/   (gitignored, has real tokens)
    writes testdata/jellyfin/<client>/<scenario>/         (safe to commit)

Secrets are replaced with stable placeholders of the SAME SHAPE (32-hex stays
32-hex, dashed GUIDs stay dashed), and the same value always maps to the same
placeholder, so cross-fixture references keep working for contract tests.
Item IDs are kept: they are path hashes of the test library, not personal data.

Extra literal strings (hostnames, names…) can be listed one per line in
scripts/capture/scrub.local.txt (gitignored). The run fails if any original
secret or a sensitive pattern survives (leak check).
"""
from __future__ import annotations

import argparse
import json
import re
import shutil
import sys
from pathlib import Path
from urllib.parse import quote

ROOT = Path(__file__).resolve().parents[2]
LOCAL_LITERALS = Path(__file__).with_name("scrub.local.txt")

HEX32 = re.compile(r"^[0-9a-fA-F]{32}$")
GUID = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
# Private, CGNAT/Tailscale and link-local IPv4 only: public-looking dotted strings
# such as "12.1.0.0" ABI versions must survive.
PRIVATE_IP = re.compile(
    r"(?<![\d.])(?:10\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])|192\.168|100\.(?:6[4-9]|[7-9]\d|1[01]\d|12[0-7])|169\.254)"
    r"\.\d{1,3}\.\d{1,3}(?![\d.])"
)
TS_NET = re.compile(r"(?<![A-Za-z0-9.-])(?!host\.example\.ts\.net)[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.ts\.net")
SIG = re.compile(r"(?i)(\bsig=)(?!REDACTED)[^&\"\s\\]+")
# Android User-Agents carry the device model and firmware build ("Android 14; moto g … Build/U1TOS34…").
ANDROID_UA = re.compile(r"(Android [\d.]+; )(?!Device Build/REDACTED)[^;)\"]*?Build/[^;)\"]+")
DEBRID_PATH = re.compile(r"(/stream/(?:realdebrid|torbox|alldebrid|premiumize)/)(?!TORRENTID)[A-Za-z0-9]+")

# key (lower-case) → secret class
KEY_CLASS = {
    "accesstoken": "token", "token": "token", "api_key": "token", "apikey": "token",
    "userid": "user", "serverid": "server", "sessionid": "session",
    "deviceid": "device", "devicename": "devname", "device": "devname",
    "username": "username",
}
REDACT_KEYS = {"pw", "password", "newpw", "currentpw"}
HEX_PREFIX = {"token": "aaaa", "user": "bbbb", "server": "cccc", "session": "eeee", "device": "dddd"}
MIN_SECRET_LEN = 4
# Clients send these as "tokens" before login (Streamyfin sends Token="null"); never treat them as secrets.
NOT_SECRETS = {"null", "undefined", "none", "true", "false", "nan"}


def dashed(h: str) -> str:
    return f"{h[:8]}-{h[8:12]}-{h[12:16]}-{h[16:20]}-{h[20:]}"


def is_id(value: str) -> bool:
    return bool(HEX32.match(value) or GUID.match(value))


class Scrubber:
    def __init__(self, literals: list[str] | None = None):
        self.maps: dict[str, dict[str, str]] = {}
        self.literals = [s for s in (literals or []) if len(s) >= MIN_SECRET_LEN and s.lower() not in NOT_SECRETS]
        self.ips: dict[str, str] = {}

    # ---- placeholder allocation -------------------------------------------------
    def placeholder(self, cls: str, value: str) -> str:
        """Return the placeholder for value, allocating one on first sight.
        IDs are keyed in canonical form (lower-case, no dashes) so every spelling
        of the same GUID maps to the same placeholder."""
        m = self.maps.setdefault(cls, {})
        key = value.lower().replace("-", "") if is_id(value) else value
        if key not in m:
            n = len(m) + 1
            if is_id(value):
                m[key] = HEX_PREFIX.get(cls, "ffff") + f"{n:028x}"
            elif cls == "devname":
                m[key] = f"Device {n}"
            elif cls == "username":
                m[key] = f"user{n}"
            elif cls == "literal":
                m[key] = f"redacted{n}"
            else:
                m[key] = f"{cls}-{n:04d}"
        return m[key]

    def add(self, cls: str, value) -> None:
        if isinstance(value, str) and len(value) >= MIN_SECRET_LEN and value.lower() not in NOT_SECRETS:
            self.placeholder(cls, value)

    # ---- discovery --------------------------------------------------------------
    def discover(self, node, parent_key: str = "") -> None:
        if isinstance(node, dict):
            looks_user = "Policy" in node or "HasPassword" in node
            looks_server = "ProductName" in node and "Version" in node
            looks_session = "DeviceId" in node and "Client" in node and "PlayState" in node
            for k, v in node.items():
                kl = k.lower()
                if kl in KEY_CLASS:
                    for item in v if isinstance(v, list) else [v]:
                        self.add(KEY_CLASS[kl], item)
                if kl == "id" and isinstance(v, str):
                    if looks_user or parent_key == "user":
                        self.add("user", v)
                    elif looks_server:
                        self.add("server", v)
                    elif looks_session or parent_key == "sessioninfo":
                        self.add("session", v)
                if kl == "name" and isinstance(v, str) and (looks_user or parent_key == "user"):
                    self.add("username", v)
                if kl == "servername" and isinstance(v, str):
                    self.add("literal", v)
                self.discover(v, kl)
        elif isinstance(node, list):
            for item in node:
                self.discover(item, parent_key)

    # ---- replacement ------------------------------------------------------------
    def redact_structured(self, node):
        if isinstance(node, dict):
            return {k: ("REDACTED" if k.lower() in REDACT_KEYS and v else self.redact_structured(v))
                    for k, v in node.items()}
        if isinstance(node, list):
            return [self.redact_structured(v) for v in node]
        return node

    def replacements(self) -> list[tuple[str, str]]:
        """Every textual spelling of every secret → its placeholder, longest first."""
        pairs: dict[str, str] = {}
        for m in self.maps.values():
            for key, ph in m.items():
                spellings = {key: ph}
                if HEX32.match(key) and HEX32.match(ph):
                    spellings |= {dashed(key): dashed(ph), key.upper(): ph.upper(), dashed(key).upper(): dashed(ph).upper()}
                for orig, out in list(spellings.items()):
                    spellings.setdefault(quote(orig, safe=""), out)
                    spellings.setdefault(quote(orig), out)
                pairs.update(spellings)
        for lit in self.literals:
            pairs.setdefault(lit, self.placeholder("literal", lit))
        return sorted(pairs.items(), key=lambda p: -len(p[0]))

    def ip(self, match: re.Match) -> str:
        return self.ips.setdefault(match.group(0), f"192.0.2.{len(self.ips) + 1}")

    def scrub_text(self, text: str, pairs: list[tuple[str, str]]) -> str:
        for orig, ph in pairs:
            text = text.replace(orig, ph)
        text = PRIVATE_IP.sub(self.ip, text)
        text = TS_NET.sub("host.example.ts.net", text)
        text = SIG.sub(r"\1REDACTED", text)
        text = ANDROID_UA.sub(r"\1Device Build/REDACTED", text)
        return DEBRID_PATH.sub(r"\1TORRENTID", text)

    def leaks(self, text: str) -> list[str]:
        found = []
        for cls, m in self.maps.items():
            for key in m:
                spellings = [key] + ([dashed(key), key.upper()] if HEX32.match(key) else [])
                if any(s in text for s in spellings):
                    found.append(f"{cls}:{key[:4]}…")
        found += [f"literal:{lit[:2]}…" for lit in self.literals if lit in text]
        for name, rx in (("private-ip", PRIVATE_IP), ("sig", SIG), ("ts.net", TS_NET), ("debrid-id", DEBRID_PATH), ("android-ua", ANDROID_UA)):
            if rx.search(text):
                found.append(name)
        return found


def load_literals() -> list[str]:
    if not LOCAL_LITERALS.exists():
        return []
    return [ln.strip() for ln in LOCAL_LITERALS.read_text().splitlines() if ln.strip() and not ln.startswith("#")]


def scrub_dir(src: Path, dst: Path, literals: list[str]) -> int:
    files = sorted(p for p in src.glob("*.json") if p.name != "scrub-map.json")
    if not files:
        sys.exit(f"no fixtures in {src}")
    s = Scrubber(literals)
    docs = {p.name: json.loads(p.read_text()) for p in files}
    for doc in docs.values():
        s.discover(doc)
    pairs = s.replacements()

    if dst.exists():
        shutil.rmtree(dst)
    dst.mkdir(parents=True)
    leaks: dict[str, list[str]] = {}
    for name, doc in docs.items():
        text = json.dumps(s.redact_structured(doc), indent=2, ensure_ascii=False) + "\n"
        text = s.scrub_text(text, pairs)
        json.loads(text)  # replacements must keep the file valid JSON
        if found := s.leaks(text):
            leaks[name] = found
        (dst / name).write_text(text)

    # The map holds original secrets, so it stays next to the raw (gitignored) input.
    (src / "scrub-map.json").write_text(json.dumps({"maps": s.maps, "ips": s.ips}, indent=2) + "\n")
    summary = {cls: len(m) for cls, m in s.maps.items()} | {"ip": len(s.ips)}
    print(f"{src} → {dst}: {len(docs)} files, replaced {summary}")
    if leaks:
        shutil.rmtree(dst)
        for name, found in list(leaks.items())[:20]:
            print(f"  LEAK {name}: {', '.join(found)}", file=sys.stderr)
        sys.exit(f"leak check failed in {len(leaks)} files; output removed")
    return len(docs)


def main(argv: list[str] | None = None) -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("client")
    ap.add_argument("scenario", nargs="?", default="full")
    ap.add_argument("--root", type=Path, default=ROOT / "testdata/jellyfin")
    args = ap.parse_args(argv)
    scrub_dir(args.root / "_raw" / args.client / args.scenario, args.root / args.client / args.scenario, load_literals())


if __name__ == "__main__":
    main()

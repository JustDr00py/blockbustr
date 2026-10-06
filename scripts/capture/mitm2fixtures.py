#!/usr/bin/env python3
"""Convert a mitmproxy capture of real Jellyfin traffic into fixture JSON (TASKS P0.4).

Runs anywhere the `mitmproxy` Python package is importable (normally inside the
mitmproxy image, via convert.sh). Output is RAW: it still contains access tokens
and device IDs, so it goes to the gitignored testdata/jellyfin/_raw/ tree.
scrub-fixtures (P0.5) produces the committed copies.

Usage:
  mitm2fixtures.py CAPTURE.mitm --client findroid-1.1.0 --scenario full --out testdata/jellyfin/_raw
"""
from __future__ import annotations

import argparse
import datetime as dt
import json
import re
import sys
from collections import OrderedDict
from pathlib import Path
from urllib.parse import parse_qs, unquote_plus, urlsplit

ID_RE = re.compile(
    r"(?<![0-9a-f])(?:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}|[0-9a-f]{32})(?![0-9a-f])",
    re.IGNORECASE,
)
AUTH_FIELD_RE = re.compile(r'(\w+)\s*=\s*"([^"]*)"')
AUTH_HEADERS = ("authorization", "x-emby-authorization")
TOKEN_HEADERS = ("x-emby-token", "x-mediabrowser-token")
TOKEN_QUERY = ("api_key", "apikey")
WS_MESSAGE_LIMIT = 200


NUM_SEGMENT_RE = re.compile(r"(?<=/)\d+(?=(?:\.[A-Za-z0-9]+)?(?:/|$))")


def normalize_path(path: str) -> str:
    """Strip the query, replace item/user/session IDs with {id} and purely numeric
    segments (HLS segment numbers, stream/image indexes) with {n}."""
    return NUM_SEGMENT_RE.sub("{n}", ID_RE.sub("{id}", urlsplit(path).path))


def parse_auth(headers: dict[str, str], query: dict[str, list[str]]) -> dict[str, str] | None:
    """Collect MediaBrowser auth fields from every place Jellyfin accepts them (DESIGN §3.2)."""
    lower = {k.lower(): v for k, v in headers.items()}
    auth: dict[str, str] = {}
    for name in AUTH_HEADERS:
        value = lower.get(name)
        if value:
            auth.update(AUTH_FIELD_RE.findall(value))
            auth.setdefault("_source", name)
    for name in TOKEN_HEADERS:
        if lower.get(name) and "Token" not in auth:
            auth["Token"] = lower[name]
            auth.setdefault("_source", name)
    lq = {k.lower(): v for k, v in query.items()}
    for name in TOKEN_QUERY:
        if lq.get(name) and "Token" not in auth:
            auth["Token"] = lq[name][0]
            auth.setdefault("_source", "query:" + name)
    return auth or None


def decode_body(message, streamed: bool) -> tuple[object | None, str | None]:
    """Return (json_body, omitted_note). Non-JSON bodies are described, not stored."""
    ctype = message.headers.get("content-type", "")
    if streamed or message.raw_content is None:
        return None, f"{ctype or 'unknown'} streamed (not captured)"
    try:
        content = message.get_content(strict=False) or b""
    except ValueError:
        content = message.raw_content or b""
    if not content:
        return None, None
    stripped = content.lstrip()[:1]
    if "json" in ctype or stripped in (b"{", b"["):
        try:
            return json.loads(content), None
        except ValueError:
            pass
    if ctype.startswith("text/") or "xml" in ctype or "mpegurl" in ctype:
        return {"_text": content.decode("utf-8", "replace")}, None
    return None, f"{ctype or 'unknown'} {len(content)} B"


def iso(ts: float | None) -> str | None:
    if ts is None:
        return None
    return dt.datetime.fromtimestamp(ts, dt.timezone.utc).strftime("%Y-%m-%dT%H:%M:%S.%f")[:-3] + "Z"


def slug(endpoint: str) -> str:
    s = re.sub(r"[{}]", "", endpoint)
    s = re.sub(r"[^A-Za-z0-9]+", "-", s).strip("-")
    return s[:80] or "root"


def flow_to_fixture(seq: int, flow) -> dict:
    req = flow.request
    query = parse_qs(urlsplit(req.path).query, keep_blank_values=True)
    req_headers = {k: v for k, v in req.headers.items(multi=False)}
    req_body, req_omitted = decode_body(req, streamed=False)
    endpoint = f"{req.method} {normalize_path(req.path)}"

    fixture: dict = OrderedDict(
        seq=seq,
        endpoint=endpoint,
        time=iso(req.timestamp_start),
        request=OrderedDict(
            method=req.method,
            path=urlsplit(req.path).path,
            query=query,
            headers=req_headers,
            auth=parse_auth(req_headers, query),
            body=req_body,
        ),
    )
    if req_omitted:
        fixture["request"]["body_omitted"] = req_omitted

    resp = flow.response
    if resp is not None:
        streamed = bool(getattr(resp, "stream", False))
        body, omitted = decode_body(resp, streamed)
        fixture["response"] = OrderedDict(
            status=resp.status_code,
            headers={k: v for k, v in resp.headers.items(multi=False)},
            body=body,
        )
        if omitted:
            fixture["response"]["body_omitted"] = omitted
        fixture["duration_ms"] = (
            round((resp.timestamp_end - req.timestamp_start) * 1000)
            if resp.timestamp_end and req.timestamp_start
            else None
        )
    elif flow.error:
        fixture["error"] = str(flow.error.msg)

    ws = getattr(flow, "websocket", None)
    if ws is not None:
        msgs = []
        for m in ws.messages[:WS_MESSAGE_LIMIT]:
            text = m.content.decode("utf-8", "replace") if isinstance(m.content, bytes) else str(m.content)
            try:
                payload = json.loads(text)
            except ValueError:
                payload = {"_text": text}
            msgs.append({"from_client": m.from_client, "time": iso(m.timestamp), "data": payload})
        fixture["websocket"] = {"messages": msgs, "truncated": len(ws.messages) > WS_MESSAGE_LIMIT}
    return fixture


def keep_flow(auth: dict | None, ua: str, keep_client: re.Pattern | None, keep_ua: re.Pattern | None) -> bool:
    """Attribute a flow to the client being captured when several apps share one capture.
    Flows with a MediaBrowser Client field are matched on it (URL-decoded); flows without one
    (images, media segments, web assets) are matched on their User-Agent."""
    if keep_client is None and keep_ua is None:
        return True
    client = unquote_plus((auth or {}).get("Client", ""))
    if client:
        return bool(keep_client and keep_client.search(client))
    return bool(keep_ua and keep_ua.search(ua))


def convert(capture: Path, client: str, scenario: str, out_root: Path, exclude_ua: list[str],
            keep_client: re.Pattern | None = None, keep_ua: re.Pattern | None = None) -> Path:
    from mitmproxy import http, io  # imported here so unit tests run without mitmproxy

    out_dir = out_root / client / scenario
    if out_dir.exists() and any(out_dir.iterdir()):
        sys.exit(f"refusing to overwrite non-empty {out_dir}; remove it first")
    out_dir.mkdir(parents=True, exist_ok=True)

    census: "OrderedDict[str, dict]" = OrderedDict()
    seq = skipped = other = 0
    with capture.open("rb") as fh:
        for flow in io.FlowReader(fh).stream():
            if not isinstance(flow, http.HTTPFlow):
                continue
            ua = flow.request.headers.get("user-agent", "")
            if any(ua.startswith(p) for p in exclude_ua):
                skipped += 1
                continue
            req_headers = {k: v for k, v in flow.request.headers.items(multi=False)}
            query = parse_qs(urlsplit(flow.request.path).query, keep_blank_values=True)
            if not keep_flow(parse_auth(req_headers, query), ua, keep_client, keep_ua):
                other += 1
                continue
            seq += 1
            fx = flow_to_fixture(seq, flow)
            name = f"{seq:04d}-{fx['request']['method']}-{slug(normalize_path(flow.request.path))}.json"
            (out_dir / name).write_text(json.dumps(fx, indent=2, ensure_ascii=False) + "\n")

            key = fx["endpoint"].lower()  # routes are case-insensitive
            entry = census.setdefault(key, {"endpoint": fx["endpoint"], "variants": [], "count": 0, "statuses": {}, "files": []})
            if fx["endpoint"] not in entry["variants"]:
                entry["variants"].append(fx["endpoint"])
            entry["count"] += 1
            status = str(fx.get("response", {}).get("status", "error"))
            entry["statuses"][status] = entry["statuses"].get(status, 0) + 1
            entry["files"].append(name)

    index = OrderedDict(
        client=client,
        scenario=scenario,
        source=capture.name,
        generated=iso(dt.datetime.now(dt.timezone.utc).timestamp()),
        flows=seq,
        skipped_user_agents=skipped,
        skipped_other_clients=other,
        filters={"keep_client": keep_client.pattern if keep_client else None,
                 "keep_ua": keep_ua.pattern if keep_ua else None},
        endpoints=sorted(census.values(), key=lambda e: e["endpoint"].split(" ", 1)[1].lower()),
    )
    (out_dir / "index.json").write_text(json.dumps(index, indent=2) + "\n")
    print(f"{capture.name}: {seq} flows ({skipped} skipped, {other} other clients) → {out_dir} ({len(census)} endpoints)")
    return out_dir


def main(argv: list[str] | None = None) -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("capture", type=Path)
    ap.add_argument("--client", required=True, help="e.g. findroid-1.1.0")
    ap.add_argument("--scenario", default="full")
    ap.add_argument("--out", type=Path, default=Path("testdata/jellyfin/_raw"))
    ap.add_argument("--exclude-ua", action="append", default=None,
                    help="skip flows whose User-Agent starts with this (default: curl/)")
    ap.add_argument("--keep-client", type=re.compile, help="regex on the (URL-decoded) MediaBrowser Client field")
    ap.add_argument("--keep-ua", type=re.compile, help="regex on User-Agent, for flows without a Client field")
    args = ap.parse_args(argv)
    convert(args.capture, args.client, args.scenario, args.out, args.exclude_ua or ["curl/"],
            args.keep_client, args.keep_ua)


if __name__ == "__main__":
    main()

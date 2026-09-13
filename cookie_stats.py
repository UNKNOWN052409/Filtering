#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
CookieStats — cookie inventory + stats for combo/cookie lists.
Made by @torbug (extension of ComboFilter v1.0)

What it answers (per run, over the ENTIRE input file):
  - kitne total cookie-lines the?
  - kitni services/domains use hui (kis cookie ki hai)?
  - kitni valid-format vs malformed vs creds vs url lines?
  - kisi cookie string me paid-plan hints (premium/prime/plus/family/max/ultimate/
    yearly/quarterly/monthly/ads-free) hain to unko count karo
  - keystore: cookie names (netflixid, session, at, amzn_uid, sid, auth...)
    kitni baar repeat hoti hain -> kis service kaunse cookies rakhta hai
  - per-service breakdown + end summary + JSON report

NO network calls. Pure offline inventory + heuristics. Same streaming design
as ComboFilter: 8MB chunks, constant RAM, one output file per service, LRU
handle pool -> 10M-100M+ line files safe.

Usage
-----
  python3 cookie_stats.py -i cookies.txt
  python3 cookie_stats.py -i cookies.txt -o stats_out --json

Output
------
  stats_out/<service>_<run_id>.txt   per-service cookie lines (sorted by hits)
  stats_out/REPORT_<run_id>.txt      human-readable summary
  stats_out/REPORT_<run_id>.json     machine-readable detail
"""

import argparse
import json
import os
import random
import re
import sys
import time
from collections import OrderedDict

VERSION = "1.1"
AUTHOR = "@torbug"
CHUNK_SIZE = 8 * 1024 * 1024

# ---- paid-plan keyword hints ----
# matched against cookie VALUE tokens only (after stripping Domain=/Path=/HttpOnly
# attributes and the domain itself), so 'primevideo.com' Domain= does NOT
# count as a 'prime' plan hint.
PLAN_RE = re.compile(
    rb"(?<![a-z0-9])(premium|ultimate|family|yearly|annual|quarterly|monthly|"
    rb"ads[-_]?free|gift|plus|max|prime)(?![a-z0-9])")

# ---- cookie key regexes (word-boundary match, not substring) ----
# ONE combined alternation = one regex pass per line (22 separate searches
# made 1M-line runs ~10x slower). Longer keys listed first so alternation
# picks the exact name (sessionid before session etc.).
# 'at' is deliberately OMITTED — it appears inside netflixid/c_data/etc and
# is not a useful standalone key; 'rt', 'kl', 'TS', 'sb' were removed for the
# same reason (too short, too many false positives).
COOKIE_KEY_RE = re.compile(
    rb"(?<![a-z0-9])(sessionid|secure_session|netflixid|connect\.sid|PHPSESSID|"
    rb"JSESSIONID|_sp_id|g_session_id|ds_user_id|cf_clearance|amzn_uid|c_user|"
    rb"li_at|datr|session|auth|token|csid|sid|xs)(?![a-z0-9])|__Secure-|__Host-",
    re.I)

EMAIL_RE = re.compile(rb"^[^@\s]+@[^@\s]+\.[a-z]{2,}$", re.I)
DOMAIN_RE = re.compile(rb"^[a-z0-9][a-z0-9.\-]*\.[a-z]{2,}$", re.I)


def extract_domain(url):
    """Same as ComboFilter — bare lowercase host out of any URL form."""
    d = url
    i = d.find(b'://')
    if i != -1:
        d = d[i + 3:]
    for sep in (b'/', b'?', b'#'):
        i = d.find(sep)
        if i != -1:
            d = d[:i]
    i = d.rfind(b'@')
    if i != -1:
        d = d[i + 1:]
    i = d.rfind(b':')
    if i != -1:
        d = d[:i]
    d = d.lower()
    if d.startswith(b'www.'):
        d = d[4:]
    if d.endswith(b'.'):
        d = d[:-1]
    return d


def registrable(domain):
    parts = domain.rsplit(b'.', 2)
    if len(parts) == 2:
        return domain
    return parts[-2] + b'.' + parts[-1] if len(parts) == 3 else domain


def sanitize_name(name_bytes):
    s = name_bytes.decode('utf-8', 'replace')
    for ch in '\\/:*?"<>| \t':
        s = s.replace(ch, '_')
    return s or 'unknown'


def cookie_domain_hint(raw):
    """Try to find the service a cookie line belongs to:
    - explicit 'Domain=' attribute
    - url prefixed before '|' or space (e.g. 'https://netflix.com/... | cookie')
    - bare host domain
    Returns bytes-domain or b''. Pure pattern match, no network."""
    low = raw.lower()
    m = re.search(rb"domain\s*=\s*\.?([a-z0-9.\-]+\.[a-z]{2,})", low)
    if m and DOMAIN_RE.match(m.group(1)):
        return m.group(1).strip(b'.')
    for sep in (b'|', b'\t', b' '):
        if sep in low:
            pre = low.split(sep)[0]
            d = extract_domain(pre)
            if d and b'.' in d:
                return d
    d = extract_domain(low)
    if d and b'.' in d:
        return d
    return b''


def classify_raw(raw):
    """-> (kind, payload, domain) where kind is cookie|creds|url|other.
    cookie  : key=value pairs (netflixid=...; session=...)
    creds   : email:pass / login:pass / url:login:pass / url | login:pass
    url     : http(s) URL or bare domain
    other   : anything unclassifiable (returned for stats, not written)
    domain  : service the line belongs to (from url prefix / Domain= / email)"""
    # url | rest  (e.g. 'https://netflix.com/browse | netflixid=...; Secure')
    if b'|' in raw:
        pre, rest = raw.split(b'|', 1)
        dom = extract_domain(pre.strip())
        if dom and b'.' in dom:
            rest = rest.strip()
            if b'=' in rest:
                return "cookie", rest, dom
            lp = rest.split(b':', 1)
            if len(lp) == 2 and lp[0] and lp[1]:
                return "creds", rest, dom
            return "cookie", rest, dom
    # url:login:pass  (ComboFilter output format)
    parts = raw.rsplit(b':', 2)
    if len(parts) == 3 and all(parts):
        url, login, pwd = parts
        dom = extract_domain(url)
        if b'://' in url or (dom and b'.' in dom):
            return "creds", login + b':' + pwd, dom if (dom and b'.' in dom) else b''
    # cookie?
    if b'=' in raw and (b';' in raw or b'=' in raw):
        eqs = re.findall(rb"[\w.\-\[\]]+\s*=\s*[^;\s]+", raw)
        if eqs:
            dom = cookie_domain_hint(raw)
            return "cookie", raw, dom
    # email:pass
    lp = raw.split(b':', 1)
    if len(lp) == 2 and lp[0] and lp[1]:
        if EMAIL_RE.match(lp[0]):
            dom = lp[0].rsplit(b'@', 1)[1].lower()
            return "creds", raw, dom
        if not re.search(rb"[a-zA-Z]", lp[0].strip()):
            return "creds", raw, b''
        return "creds", raw, b''
    # url or bare domain
    d = extract_domain(raw)
    if d and b'.' in d:
        return "url", raw, d
    return "other", raw, b''


class OutputPool:
    """Per-service file pool with LRU handle cap (same as ComboFilter)."""

    def __init__(self, outdir, run_id, max_open=512):
        self.outdir = outdir
        self.run_id = run_id
        self.max_open = max_open
        self.handles = OrderedDict()
        self.counts = {}

    def _path_for(self, name):
        return os.path.join(self.outdir, f"{name}_{self.run_id}.txt")

    def write(self, name, payload):
        path = self._path_for(name)
        h = self.handles.get(path)
        if h is None:
            h = open(path, 'ab')
            self.handles[path] = h
            if len(self.handles) > self.max_open:
                _, old = self.handles.popitem(last=False)
                old.close()
            else:
                self.handles.move_to_end(path)
        else:
            self.handles.move_to_end(path)
        h.write(payload + b'\n')
        self.counts[name] = self.counts.get(name, 0) + 1
        return True

    def close_all(self):
        while self.handles:
            _, h = self.handles.popitem()
            h.close()


def run(input_path, outdir, quiet):
    is_tty = sys.stderr.isatty() and not quiet
    total_bytes = os.path.getsize(input_path)
    run_id = random.randint(10 ** 17, 10 ** 18 - 1)
    os.makedirs(outdir, exist_ok=True)
    pool = OutputPool(outdir, run_id)

    stats = {
        "run_id": run_id,
        "total_lines": 0,
        "cookie": 0, "creds": 0, "url": 0, "other": 0,
        "valid_format": 0, "malformed": 0,
        "with_plan_hint": 0, "without_plan_hint": 0,
        "domains": {},          # service -> count
        "cookie_names": {},     # cookie name -> count
        "plan_hints": {},       # hint -> count
        "unique_services": 0,
    }
    lines = malformed = 0

    t0 = time.perf_counter()
    with open(input_path, 'rb') as f:
        tail = b''
        first_line = True
        while True:
            chunk = f.read(CHUNK_SIZE)
            if not chunk:
                break
            data = tail + chunk if tail else chunk
            cut = data.rfind(b'\n')
            if cut == -1:
                tail = data
                continue
            block, tail = data[:cut], data[cut + 1:]

            for raw in block.split(b'\n'):
                if first_line and raw.startswith(b'\xef\xbb\xbf'):
                    raw = raw[3:]
                    first_line = False
                if raw.endswith(b'\r'):
                    raw = raw[:-1]
                if not raw:
                    continue
                lines += 1
                stats["total_lines"] += 1

                kind, payload, dom = classify_raw(raw)
                stats[kind] += 1

                if kind == "cookie":
                    stats["valid_format"] += 1
                    if dom:
                        key = sanitize_name(registrable(dom)) if dom else "unknown"
                        pool.write(key, payload)
                        stats["domains"][key] = stats["domains"].get(key, 0) + 1
                    else:
                        stats["domains"]["unknown"] = stats["domains"].get("unknown", 0) + 1
                    # plan hints — value tokens only (strip cookie attributes)
                    scan = payload.lower()
                    # strip Domain=/Path=/Expires=... attribute values so the
                    # service name can't produce a false plan hint
                    plain = re.sub(rb"(?:domain|path|expires|max-age|samesite)\s*=\s*[^;]*", b"", scan)
                    hint_any = False
                    for m in PLAN_RE.finditer(plain):
                        h = m.group(1).decode()
                        stats["plan_hints"][h] = stats["plan_hints"].get(h, 0) + 1
                        hint_any = True
                    if hint_any:
                        stats["with_plan_hint"] += 1
                    else:
                        stats["without_plan_hint"] += 1
                    # cookie key names — single combined regex pass
                    for m in COOKIE_KEY_RE.finditer(payload):
                        label = (m.group(1) or (b"__Secure-" if m.group(0) == b"__Secure-" else b"__Host-")).decode()
                        stats["cookie_names"][label] = stats["cookie_names"].get(label, 0) + 1
                else:
                    stats["malformed"] += 1

            now = time.perf_counter()
            if is_tty and now - t0 > 2:
                pct = f.tell() / total_bytes * 100 if total_bytes else 0
                sys.stderr.write(f"\r  {pct:5.1f}%  lines: {lines:,}  "
                                 f"cookies: {stats['cookie']:,}  "
                                 f"services: {len(stats['domains'])}  ")
                sys.stderr.flush()

        if tail:
            raw = tail.rstrip(b'\r\n')
            kind, payload, dom = classify_raw(raw)
            lines += 1
            stats["total_lines"] += 1
            stats[kind] += 1
            if kind == "cookie":
                stats["valid_format"] += 1
                if dom:
                    key = sanitize_name(registrable(dom))
                    pool.write(key, payload)
                    stats["domains"][key] = stats["domains"].get(key, 0) + 1
                    # plan hints — value tokens only (strip cookie attributes)
                    scan = payload.lower()
                    plain = re.sub(rb"(?:domain|path|expires|max-age|samesite)\s*=\s*[^;]*", b"", scan)
                    hint_any = False
                    for m in PLAN_RE.finditer(plain):
                        h = m.group(1).decode()
                        stats["plan_hints"][h] = stats["plan_hints"].get(h, 0) + 1
                        hint_any = True
                    if hint_any:
                        stats["with_plan_hint"] += 1
                    # cookie key names — single combined regex pass
                    for m in COOKIE_KEY_RE.finditer(payload):
                        label = (m.group(1) or (b"__Secure-" if m.group(0) == b"__Secure-" else b"__Host-")).decode()
                        stats["cookie_names"][label] = stats["cookie_names"].get(label, 0) + 1

    pool.close_all()
    stats["unique_services"] = len(stats["domains"])
    return stats, pool


def fmt_int(n):
    return f"{n:,}"


def render_report(stats, pool, outdir, elapsed):
    lines = []
    a = lines.append
    a("=" * 60)
    a(f"  CookieStats v{VERSION} — Made by {AUTHOR}")
    a("=" * 60)
    a(f"  Run ID            : {stats['run_id']}")
    a(f"  Processing time   : {elapsed:.2f}s")
    a("-" * 60)
    a(f"  Total lines       : {fmt_int(stats['total_lines'])}")
    a(f"  Cookie lines      : {fmt_int(stats['cookie'])}")
    a(f"  Valid format      : {fmt_int(stats['valid_format'])}")
    a(f"  Malformed/other   : {fmt_int(stats['malformed'])}")
    a(f"  Creds lines       : {fmt_int(stats['creds'])}")
    a(f"  URL lines         : {fmt_int(stats['url'])}")
    a(f"  Other             : {fmt_int(stats['other'])}")
    a(f"  Unique services   : {fmt_int(stats['unique_services'])}")
    a(f"  With plan hint    : {fmt_int(stats['with_plan_hint'])}")
    a(f"  Without plan hint : {fmt_int(stats['without_plan_hint'])}")
    a("-" * 60)
    a("  SERVICES (cookie lines per service, sorted by hits):")
    for name, cnt in sorted(stats["domains"].items(), key=lambda x: -x[1]):
        a(f"    {cnt:>12,}  {name}")
    if stats["plan_hints"]:
        a("-" * 60)
        a("  PLAN HINTS found (substring in cookie strings):")
        for name, cnt in sorted(stats["plan_hints"].items(), key=lambda x: -x[1]):
            a(f"    {cnt:>12,}  {name}")
    if stats["cookie_names"]:
        a("-" * 60)
        a("  COOKIE KEYS (most common cookie names):")
        for name, cnt in sorted(stats["cookie_names"].items(), key=lambda x: -x[1])[:15]:
            a(f"    {cnt:>12,}  {name}")
    a("=" * 60)
    return "\n".join(lines) + "\n"


def main():
    ap = argparse.ArgumentParser(
        prog='cookie_stats',
        description='Cookie inventory + stats (kiski kitni, valid/malformed, plan hints) — Made by @torbug')
    ap.add_argument('-i', '--input', required=True, help='input .txt file (cookies/combos)')
    ap.add_argument('-o', '--outdir', default='cookie_stats_out', help='output directory')
    ap.add_argument('--json', action='store_true', help='also write machine-readable JSON report')
    ap.add_argument('-q', '--quiet', action='store_true')
    args = ap.parse_args()

    if not os.path.isfile(args.input):
        print(f"[!] Input file nahi mila: {args.input}")
        sys.exit(1)

    print(f"[*] CookieStats v{VERSION} — Made by {AUTHOR}")
    print(f"[*] Input : {args.input}")
    print(f"[*] Size  : {os.path.getsize(args.input):,} bytes")
    print("-" * 50)

    t0 = time.perf_counter()
    stats, pool = run(args.input, args.outdir, args.quiet)
    elapsed = time.perf_counter() - t0

    if sys.stderr.isatty() and not args.quiet:
        sys.stderr.write('\r' + ' ' * 120 + '\n')

    report = render_report(stats, pool, args.outdir, elapsed)
    rp = os.path.join(args.outdir, f"REPORT_{stats['run_id']}.txt")
    with open(rp, "w") as f:
        f.write(report)
    print(report)
    print(f"[+] Report saved: {os.path.abspath(rp)}")

    if args.json:
        jp = os.path.join(args.outdir, f"REPORT_{stats['run_id']}.json")
        with open(jp, "w") as f:
            json.dump(stats, f, indent=2)
        print(f"[+] JSON saved  : {os.path.abspath(jp)}")


if __name__ == "__main__":
    main()

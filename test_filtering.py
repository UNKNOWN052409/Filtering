#!/usr/bin/env python3
"""Regression tests for all 5 Filtering-repo modules.

Covers every confirmed bug fix:
  B1  combofilter tail: matched counter clobber (hit = matched = True)
  B2  combofilter registrable: co.uk-style TLDs collapsed wrong
  B3  combofilter keyword dedupe: same kw twice → double write
  B4  cookie_stats tail: missing stats (malformed, unknown, without_plan_hint)
  B5  cookie_stats __Secure- label: case-sensitive compare failed re.I input
  B6  bot.py: missing traceback import
  B7  bot.py Engine.w LRU: closed handle never deleted → dict grows unbounded
  B8  bot.py registrable: co.uk-style TLDs
  B9  validator.py: missing-endpoint only exits when NOT quiet (inverted)
  B10 gen_test.py: hardcoded Linux-only path

Run:  python test_filtering.py          (from Filtering/)
      python -m pytest test_filtering.py (if pytest available)
"""
import os
import sys
import json
import shutil
import signal
import socket
import tempfile
import textwrap
import threading
import time
import unittest
from http.server import HTTPServer, BaseHTTPRequestHandler
from unittest.mock import patch

# ensure repo root on path
REPO = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, REPO)

import combofilter as cf
import cookie_stats as cs


# ─────────────────────── helpers ───────────────────────
def _tmpfile(content, suffix=".txt", tmpdir=None):
    """Write text content to a temp file, return its path."""
    td = tmpdir or tempfile.mkdtemp(prefix="ft_")
    p = os.path.join(td, "input" + suffix)
    with open(p, "w", newline="", encoding="utf-8") as f:
        f.write(content)
    return p, td


def _combo(lines):
    return "\r\n".join(lines)


# ═══════════════════════════════════════════════════════════
#  combofilter.py
# ═══════════════════════════════════════════════════════════
class TestExtractDomain(unittest.TestCase):
    """B2+B8 regressions use extract_domain internally."""

    def test_basic(self):
        self.assertEqual(cf.extract_domain(b"https://netflix.com/login"), b"netflix.com")

    def test_scheme_www_port(self):
        self.assertEqual(cf.extract_domain(b"https://www.hbo.com:8080/x"), b"hbo.com")

    def test_userinfo(self):
        self.assertEqual(cf.extract_domain(b"https://user:pass@hbo.com/path"), b"hbo.com")

    def test_uppercase(self):
        self.assertEqual(cf.extract_domain(b"NETFLIX.COM"), b"netflix.com")

    def test_trailing_dot(self):
        self.assertEqual(cf.extract_domain(b"netflix.com."), b"netflix.com")

    def test_evil_domain(self):
        self.assertEqual(cf.extract_domain(b"http://netflix.com.evil.io"), b"netflix.com.evil.io")


class TestRegistrable(unittest.TestCase):
    """B2 regression: mail.yahoo.co.uk should stay yahoo.co.uk, not co.uk."""

    def test_normal(self):
        self.assertEqual(cf.registrable(b"accounts.netflix.com"), b"netflix.com")

    def test_two_level_tld(self):
        self.assertEqual(cf.registrable(b"mail.yahoo.co.uk"), b"yahoo.co.uk")

    def test_two_level_deep(self):
        self.assertEqual(cf.registrable(b"a.b.yahoo.co.uk"), b"yahoo.co.uk")

    def test_plain_two_part(self):
        self.assertEqual(cf.registrable(b"netflix.com"), b"netflix.com")

    def test_au(self):
        self.assertEqual(cf.registrable(b"www.example.com.au"), b"example.com.au")


class TestDomainMatches(unittest.TestCase):
    def test_exact(self):
        self.assertTrue(cf.domain_matches(b"netflix.com", b"netflix.com"))

    def test_subdomain(self):
        self.assertTrue(cf.domain_matches(b"accounts.netflix.com", b"netflix.com"))

    def test_notnetflix(self):
        self.assertFalse(cf.domain_matches(b"notnetflix.com", b"netflix.com"))

    def test_evil_suffix(self):
        self.assertFalse(cf.domain_matches(b"netflix.com.evil.io", b"netflix.com"))


class TestComboFilterRun(unittest.TestCase):
    """End-to-end run() — covers B1 (tail matched counter), B3 (kw dedupe)."""

    def _run(self, content, keywords=None, auto=False, outdir=None, dedupe=False):
        p, td = _tmpfile(content)
        od = outdir or os.path.join(td, "out")
        kw_list = keywords or []
        pool, kwl, kwc, lines, matched, malformed, empty, run_id = cf.run(
            p, kw_list, auto, od, dedupe, quiet=True)
        return pool, kwl, kwc, lines, matched, malformed, empty, td

    # B1: last line w/o newline hits keyword → matched must be correct integer
    def test_tail_matched_not_clobbered(self):
        content = "https://netflix.com:user1:pass1\nhttps://netflix.com:user2:pass2"
        _, _, kwc, _, matched, _, _, td = self._run(content, ["netflix.com"])
        self.assertEqual(matched, 2, "tail line matched but counter was clobbered to True==1")
        self.assertEqual(kwc.get(b"netflix.com", 0), 2)
        shutil.rmtree(td, ignore_errors=True)

    # B3: same keyword twice → files not duplicated
    def test_keyword_dedup(self):
        content = "https://netflix.com:a:b"
        _, kwl, kwc, _, matched, _, _, td = self._run(content, ["netflix.com", "Netflix.com"])
        self.assertEqual(len(kwl), 1, "duplicate keyword should be deduped")
        self.assertEqual(matched, 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_evil_not_matched(self):
        lines = [
            "https://notnetflix.com:x:y",
            "http://netflix.com.evil.io:x:y",
            "https://netflix.com:x:y",
        ]
        content = _combo(lines)
        _, _, _, _, matched, _, _, td = self._run(content, ["netflix.com"])
        self.assertEqual(matched, 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_auto_mode_creates_per_domain(self):
        lines = [
            "https://netflix.com:a:b",
            "https://hbo.com:c:d",
        ]
        content = _combo(lines)
        pool, _, _, _, _, _, _, td = self._run(content, auto=True)
        self.assertIn("netflix.com", pool.counts)
        self.assertIn("hbo.com", pool.counts)
        shutil.rmtree(td, ignore_errors=True)

    def test_malformed_and_empty(self):
        content = "onlyone\n\n\nhttps://netflix.com:a:b"
        _, _, _, lines, _, malformed, empty, td = self._run(content, ["netflix.com"])
        self.assertEqual(lines, 1)
        self.assertEqual(malformed, 1)
        self.assertEqual(empty, 2)
        shutil.rmtree(td, ignore_errors=True)

    def test_bom_handled(self):
        content = "\ufeffhttps://netflix.com:x:y"
        _, _, _, lines, matched, _, _, td = self._run(content, ["netflix.com"])
        self.assertEqual(lines, 1)
        self.assertEqual(matched, 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_dedupe_mode(self):
        content = "https://netflix.com:x:y\nhttps://netflix.com:x:y"
        pool, _, _, _, matched, _, _, td = self._run(content, ["netflix.com"], dedupe=True)
        # duplicate login:password should be deduped
        counts = pool.counts
        self.assertEqual(counts.get("netflix.com", 0), 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_co_ukRegistrableFileNaming(self):
        content = "https://mail.yahoo.co.uk:user:pass"
        pool, _, _, _, matched, _, _, td = self._run(content, auto=True)
        names = list(pool.counts.keys())
        self.assertTrue(any("yahoo.co.uk" in n for n in names),
                        f"expected yahoo.co.uk in output names, got {names}")
        shutil.rmtree(td, ignore_errors=True)


# ═══════════════════════════════════════════════════════════
#  cookie_stats.py
# ═══════════════════════════════════════════════════════════
class TestCookieKeyLabel(unittest.TestCase):
    """B5: __SECURE-XYZ matched re.I must label __Secure-, not __Host-."""

    def test_secure_uppercase(self):
        import re as _re
        pat = cs.COOKIE_KEY_RE
        m = pat.search(b"__SECURE-abc=xyz")
        self.assertIsNotNone(m)
        self.assertEqual(cs.cookie_key_label(m), "__secure-")

    def test_host_uppercase(self):
        import re as _re
        pat = cs.COOKIE_KEY_RE
        m = pat.search(b"__HOST-abc=xyz")
        self.assertIsNotNone(m)
        self.assertEqual(cs.cookie_key_label(m), "__host-")

    def test_named_key(self):
        import re as _re
        m = cs.COOKIE_KEY_RE.search(b"netflixid=abc123")
        self.assertEqual(cs.cookie_key_label(m), "netflixid")


class TestCookieStatsRun(unittest.TestCase):
    """B4: tail-line parity — last line must be accounted fully."""

    def _run(self, content):
        p, td = _tmpfile(content)
        od = os.path.join(td, "out")
        stats, pool = cs.run(p, od, quiet=True)
        return stats, pool, td

    def test_tail_cookie_counts(self):
        content = "netflixid=abc123; Domain=.netflix.com"
        stats, _, td = self._run(content)
        self.assertEqual(stats["cookie"], 1)
        self.assertEqual(stats["valid_format"], 1)
        self.assertIn("netflix.com", stats["domains"])
        shutil.rmtree(td, ignore_errors=True)

    def test_tail_non_cookie_malformed(self):
        content = "user@gmail.com:pass123"
        stats, _, td = self._run(content)
        self.assertEqual(stats["creds"], 1)
        self.assertEqual(stats["malformed"], 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_plan_hint_in_value(self):
        content = "session=premium_abc123; Domain=.netflix.com"
        stats, _, td = self._run(content)
        self.assertEqual(stats["with_plan_hint"], 1)
        self.assertEqual(stats["plan_hints"].get("premium", 0), 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_plan_not_from_domain(self):
        content = "sid=abc; Domain=.primevideo.com"
        stats, _, td = self._run(content)
        self.assertEqual(stats["with_plan_hint"], 0,
                         "Domain=.primevideo.com must NOT count as plan hint")
        shutil.rmtree(td, ignore_errors=True)

    def test_tail_no_newline(self):
        content = "netflixid=xyz"  # no trailing \n → lands in tail
        stats, _, td = self._run(content)
        self.assertEqual(stats["cookie"], 1)
        self.assertEqual(stats["valid_format"], 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_tail_noncookie_no_newline(self):
        content = "baddata"  # no trailing \n, no =, no : → other
        stats, _, td = self._run(content)
        self.assertEqual(stats["other"], 1)
        self.assertEqual(stats["malformed"], 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_cookie_unknown_domain(self):
        content = "session_id=abc123; Secure"  # no Domain=, no url prefix
        stats, _, td = self._run(content)
        self.assertEqual(stats["domains"].get("unknown", 0), 1)
        shutil.rmtree(td, ignore_errors=True)

    def test_registrable_co_uk(self):
        content = "sid=x; Domain=.yahoo.co.uk"
        stats, _, td = self._run(content)
        names = list(stats["domains"].keys())
        self.assertTrue(any("yahoo.co.uk" in n for n in names),
                        f"expected yahoo.co.uk, got {names}")
        shutil.rmtree(td, ignore_errors=True)


# ═══════════════════════════════════════════════════════════
#  bot.py
# ═══════════════════════════════════════════════════════════
class TestBot(unittest.TestCase):
    """B6: traceback import; B7: LRU eviction deletes; B8: registrable."""

    def test_traceback_importable(self):
        import bot as _bot
        self.assertTrue(hasattr(_bot, "traceback"),
                        "bot.py must import traceback (used in poll error handler)")

    def test_registrable_co_uk(self):
        import bot as _bot
        self.assertEqual(_bot.registrable("mail.yahoo.co.uk"), "yahoo.co.uk")
        self.assertEqual(_bot.registrable("accounts.netflix.com"), "netflix.com")
        self.assertEqual(_bot.registrable("netflix.com"), "netflix.com")

    def test_engine_lru_evicts(self):
        """B7: Engine.w LRU cap ≤256 — closed handle must be removed from dict."""
        import bot as _bot
        tmpdir = tempfile.mkdtemp(prefix="bot_lru_")
        orig_work = _bot.WORK
        _bot.WORK = tmpdir
        try:
            fake_bot = type("Fake", (), {"edit": lambda s, *a: None,
                                         "done": lambda s, *a: None})()
            eng = _bot.Engine(fake_bot, 1, os.path.join(tmpdir, "dummy.txt"), [], True)
            eng.t0 = time.time()
            # write 300 distinct file names — must not exceed 256 open handles
            for i in range(300):
                eng.w(f"domain_{i}", f"login_{i}:pwd_{i}")
            self.assertLessEqual(len(eng.files), 256,
                                 "LRU eviction should cap dict at 256")
            eng.w(f"domain_300", f"login300:pwd300")
            # verify the most recent entry is accessible
            self.assertIn("domain_300", eng.counts)
        finally:
            for h in list(eng.files.values()):
                h.close()
            eng.files.clear()
            _bot.WORK = orig_work
            shutil.rmtree(tmpdir, ignore_errors=True)

    def test_phone_cc(self):
        import bot as _bot
        cc, name = _bot.phone_cc("+14155551234")
        self.assertEqual(cc, 1)
        self.assertEqual(name, "US_CA")

    def test_classify_url(self):
        import bot as _bot
        c = _bot.classify("https://netflix.com:user:pass")
        self.assertIsNotNone(c)
        self.assertEqual(c["kind"], "url")
        self.assertEqual(c["dom"], "netflix.com")

    def test_classify_email(self):
        import bot as _bot
        c = _bot.classify("user@gmail.com:mypass123")
        self.assertIsNotNone(c)
        self.assertEqual(c["kind"], "email")
        self.assertEqual(c["dom"], "gmail.com")

    def test_classify_phone(self):
        import bot as _bot
        c = _bot.classify("+919876543210:mypass")
        self.assertIsNotNone(c)
        self.assertEqual(c["kind"], "phone")

    def test_classify_user(self):
        import bot as _bot
        c = _bot.classify("john:pass123")
        self.assertIsNotNone(c)
        self.assertEqual(c["kind"], "user")


# ═══════════════════════════════════════════════════════════
#  validator.py — uses a tiny local HTTP stub server
# ═══════════════════════════════════════════════════════════
class _StubHandler(BaseHTTPRequestHandler):
    """Minimal HTTP server for validator tests."""
    def do_GET(self):
        cookie = self.headers.get("Cookie", "")
        if "sid=good" in cookie:
            body = json.dumps({"model": {"summary": {
                "userCountry": "IN", "subPlan": "PREMIUM",
                "membershipStatus": "ACTIVE"}}}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(401)
            self.end_headers()

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(n)) if n else {}
        if body.get("login") == "good":
            resp = json.dumps({"ok": True}).encode()
            self.send_response(200)
            self.send_header("Set-Cookie", "sid=good; Path=/")
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(resp)))
            self.end_headers()
            self.wfile.write(resp)
        else:
            self.send_response(401)
            self.end_headers()

    def log_message(self, *a):
        pass  # silence


def _start_stub():
    """Start local HTTP server on an ephemeral port, return (server, port)."""
    srv = HTTPServer(("127.0.0.1", 0), _StubHandler)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, port


class TestValidator(unittest.TestCase):
    def test_no_endpoint_always_exits(self):
        """B9: missing endpoint must sys.exit regardless of --quiet."""
        import subprocess
        config_dir = os.path.join(REPO, "checker")
        config_path = os.path.join(config_dir, "config.json")
        backup_path = config_path + ".bak"
        # swap in a config without endpoint
        try:
            shutil.copy2(config_path, backup_path)
            with open(config_path, "w") as f:
                json.dump({}, f)
            result = subprocess.run(
                [sys.executable, os.path.join(REPO, "checker", "validator.py"),
                 "-c", os.path.join(REPO, "test_a.txt"), "-q"],
                capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0,
                                "missing endpoint should exit nonzero even with -q")
        finally:
            if os.path.exists(backup_path):
                shutil.move(backup_path, config_path)

    def test_missing_input_file_exits(self):
        import subprocess
        result = subprocess.run(
            [sys.executable, os.path.join(REPO, "checker", "validator.py"),
             "-c", "nonexistent_file_12345.txt"],
            capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0,
                            "missing input file should exit nonzero")

    def test_valid_cookie(self):
        """Cookie with sid=good → valid; bad cookie → invalid."""
        srv, port = _start_stub()
        try:
            tmpdir = tempfile.mkdtemp(prefix="val_")
            cfg_path = os.path.join(tmpdir, "config.json")
            with open(cfg_path, "w") as f:
                json.dump({"endpoint": f"http://127.0.0.1:{port}",
                           "logged_in_probes": ["model.summary"]}, f)
            cookies_path = os.path.join(tmpdir, "cookies.txt")
            with open(cookies_path, "w") as f:
                f.write("sid=good\n")
            invalid_path = os.path.join(tmpdir, "bad.txt")
            with open(invalid_path, "w") as f:
                f.write("sid=bad123\n")
            outdir = os.path.join(tmpdir, "results")

            # patch CONFIG_FILE temporarily
            import checker.validator as val
            old_cfg = val.CONFIG_FILE
            val.CONFIG_FILE = cfg_path
            try:
                val_args = ["-c", cookies_path, "-o", outdir, "-t", "1"]
                with patch("sys.argv", ["validator"] + val_args):
                    val.main()
            finally:
                val.CONFIG_FILE = old_cfg

            valid_files = [f for f in os.listdir(outdir) if f.startswith("valid_")]
            self.assertEqual(len(valid_files), 1)
            with open(os.path.join(outdir, valid_files[0])) as f:
                content = f.read()
            self.assertIn("sid=good", content)
            shutil.rmtree(tmpdir, ignore_errors=True)
        finally:
            srv.shutdown()

    def test_no_input_flag(self):
        import subprocess
        result = subprocess.run(
            [sys.executable, os.path.join(REPO, "checker", "validator.py")],
            capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)


# ═══════════════════════════════════════════════════════════
#  gen_test.py
# ═══════════════════════════════════════════════════════════
class TestGenTest(unittest.TestCase):
    """B10: gen_test must produce output on Windows, not crash on Linux path."""

    def test_generates_valid_file(self):
        import subprocess
        tmpdir = tempfile.mkdtemp(prefix="gentest_")
        outpath = os.path.join(tmpdir, "combos.txt")
        try:
            r = subprocess.run(
                [sys.executable, os.path.join(REPO, "gen_test.py"), outpath],
                capture_output=True, text=True, timeout=10)
            self.assertEqual(r.returncode, 0, f"gen_test crashed: {r.stderr}")
            self.assertTrue(os.path.exists(outpath))
            with open(outpath, encoding="utf-8") as f:
                lines = f.read().splitlines()
            self.assertGreater(len(lines), 200, "should have ~212 lines")
            # first line should have BOM
            with open(outpath, "rb") as f:
                raw = f.read()
            self.assertTrue(raw.startswith(b"\xef\xbb\xbf"),
                            "file should start with UTF-8 BOM")
        finally:
            shutil.rmtree(tmpdir, ignore_errors=True)


# ═══════════════════════════════════════════════════════════
if __name__ == "__main__":
    unittest.main(verbosity=2)

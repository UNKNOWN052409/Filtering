#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
ComboFilter v1.0 — high-speed combo-list URL filterer
Made by @torbug

Input  : a .txt file, one combo per line, format  url:login:password
Output : one file per keyword / per discovered domain, format  login:password
        named  <keyword>_<18-digit-random-int>.txt

Modes:
  -k/--keywords netflix.com spotify.com   -> targeted filtering, live counts
  --auto                                  -> discover every domain in the file,
                                             sort all lines into per-domain files
  (both flags together = one pass, both outputs)

Engineered for 10M-100M+ line files:
  - streams 8 MB binary chunks (constant RAM, no lag)
  - full bytes pipeline (no per-line decode)
  - LRU pool caps open file handles (no crash on 1000s of domains)
"""

import argparse
import os
import random
import sys
import time
from collections import OrderedDict

VERSION = "1.0"
AUTHOR = "@torbug"
CHUNK_SIZE = 8 * 1024 * 1024  # 8 MB read chunks
BIG_RAND_DIGITS = 18          # output filename random integer length


# ──────────────────────────── domain extraction ────────────────────────────

def extract_domain(url):
    """Pull the bare lowercase host out of any URL form.
    Handles: https://, http://, ftp://, www., ports, paths, user@host creds."""
    d = url
    i = d.find(b'://')                    # strip scheme
    if i != -1:
        d = d[i + 3:]
    for sep in (b'/', b'?', b'#'):        # strip path / query / fragment
        i = d.find(sep)
        if i != -1:
            d = d[:i]
    i = d.rfind(b'@')                     # strip user:pass@ creds if any
    if i != -1:
        d = d[i + 1:]
    i = d.rfind(b':')                     # strip port
    if i != -1:
        d = d[:i]
    d = d.lower()
    if d.startswith(b'www.'):
        d = d[4:]
    if d.endswith(b'.'):
        d = d[:-1]
    return d


def registrable(domain):
    """Collapse a subdomain to its parent site: accounts.netflix.com -> netflix.com"""
    parts = domain.rsplit(b'.', 2)
    if len(parts) == 2:
        return domain
    return parts[-2] + b'.' + parts[-1] if len(parts) == 3 else domain


def normalize_keyword(kw):
    """User-typed keywords can be sloppy: 'Netflix.com', 'https://www.netflix.com',
    'WWW.SPOTIFY.COM' — all normalize to the bare domain."""
    return extract_domain(kw.strip().lower().encode('utf-8', 'replace'))


def domain_matches(domain, kw):
    """netflix.com matches netflix.com, www.netflix.com, accounts.netflix.com
    but NOT notnetflix.com or evil-netflix.com."""
    return domain == kw or domain.endswith(b'.' + kw)


def sanitize_name(name_bytes):
    """Filesystem-safe keyword -> filename."""
    s = name_bytes.decode('utf-8', 'replace')
    for ch in '\\/:*?"<>| \t':
        s = s.replace(ch, '_')
    return s or 'unknown'


# ──────────────────────────── output file pool ────────────────────────────

class OutputPool:
    """Lazily opens per-key output files, evicts least-recently-used handles
    so a 10,000-domain file can't blow the OS file-descriptor limit."""

    def __init__(self, outdir, run_id, max_open=512, dedupe=False):
        self.outdir = outdir
        self.run_id = run_id
        self.max_open = max_open
        self.dedupe = dedupe
        self.handles = OrderedDict()   # path -> file handle
        self.counts = {}              # sanitized name -> matched line count
        self.seen = {}                 # name -> set of lines (dedupe mode only)

    def _path_for(self, name):
        return os.path.join(self.outdir, f"{name}_{self.run_id}.txt")

    def write(self, name, payload):
        """payload = b'login:password'"""
        if self.dedupe:
            s = self.seen.setdefault(name, set())
            if payload in s:
                return False
            s.add(payload)
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


# ──────────────────────────── formatting helpers ───────────────────────────

def fmt_int(n):
    return f"{n:,}"


def fmt_time(sec):
    m, s = divmod(int(sec), 60)
    h, m = divmod(m, 60)
    return f"{h:02d}:{m:02d}:{s:02d}" if h else f"{m:02d}:{s:02d}"


def progress_bar(frac, width=22):
    filled = int(frac * width)
    return '[' + '#' * filled + '-' * (width - filled) + ']'


def render_stats(is_tty, bytes_done, total_bytes, lines, matched, per_kw,
                 start, elapsed_snap):
    if not is_tty:
        return
    frac = bytes_done / total_bytes if total_bytes else 0.0
    speed = lines / elapsed_snap if elapsed_snap > 0 else 0.0
    kw_str = ' | '.join(f"{k.decode()}: {fmt_int(v)}" for k, v in per_kw)
    line = (f"\r  {progress_bar(frac)} {frac*100:5.1f}%  "
            f"lines: {fmt_int(lines)}  {fmt_int(speed)}/s  "
            f"hits: {fmt_int(matched)}  {kw_str[:60]}  "
            f"t: {fmt_time(elapsed_snap)}      ")
    sys.stderr.write(line[:200])
    sys.stderr.flush()


# ──────────────────────────── core engine ──────────────────────────────────

def run(input_path, keywords, auto_mode, outdir, dedupe, quiet):
    is_tty = sys.stderr.isatty() and not quiet
    total_bytes = os.path.getsize(input_path)
    run_id = random.randint(10 ** (BIG_RAND_DIGITS - 1), 10 ** BIG_RAND_DIGITS - 1)

    os.makedirs(outdir, exist_ok=True)
    pool = OutputPool(outdir, run_id, dedupe=dedupe)

    # normalized keyword bytes
    kw_list = [normalize_keyword(k) for k in keywords]
    kw_counts = {k: 0 for k in kw_list}

    lines = matched = malformed = empty = 0
    start = time.perf_counter()
    t0 = start
    last_render = start

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
                    empty += 1
                    continue

                parts = raw.rsplit(b':', 2)
                if len(parts) != 3:
                    malformed += 1
                    continue
                url, login, pwd = parts
                if not url or not login or not pwd:
                    malformed += 1
                    continue

                lines += 1
                domain = extract_domain(url)
                if not domain or b'.' not in domain:
                    malformed += 1
                    continue

                payload = login + b':' + pwd

                hit_any = False
                if kw_list:
                    for kw in kw_list:
                        if domain_matches(domain, kw):
                            name = sanitize_name(kw)
                            if pool.write(name, payload):
                                kw_counts[kw] += 1
                            hit_any = True
                if hit_any:
                    matched += 1

                # auto mode writes every discovered domain EXCEPT ones already
                # captured by a keyword, so combined mode never duplicates lines
                if auto_mode and not hit_any:
                    name = sanitize_name(registrable(domain))
                    pool.write(name, payload)

            bytes_done = f.tell() - len(tail)
            now = time.perf_counter()
            if is_tty and now - last_render > 0.25:
                render_stats(is_tty, bytes_done, total_bytes, lines, matched,
                             [(k, kw_counts[k]) for k in kw_list],
                             now - last_render, now - t0)
                last_render = now

        # final partial line
        if tail:
            raw = tail.rstrip(b'\r\n')
            parts = raw.rsplit(b':', 2)
            if len(parts) == 3 and all(parts):
                lines += 1
                domain = extract_domain(parts[0])
                if domain and b'.' in domain:
                    payload = parts[1] + b':' + parts[2]
                    hit = False
                    for kw in kw_list:
                        if domain_matches(domain, kw):
                            if pool.write(sanitize_name(kw), payload):
                                kw_counts[kw] += 1
                            hit = matched = True
                    if auto_mode and not hit:
                        pool.write(sanitize_name(registrable(domain)), payload)

    pool.close_all()
    return pool, kw_list, kw_counts, lines, matched, malformed, empty, run_id


# ──────────────────────────── CLI ───────────────────────────────────────────

def main():
    ap = argparse.ArgumentParser(
        prog='combofilter',
        description='High-speed url:login:password combo filterer — Made by @torbug')
    ap.add_argument('-i', '--input', required=True, help='input .txt combo file')
    ap.add_argument('-k', '--keywords', nargs='*', default=[],
                    help='target keyword(s): netflix.com spotify.com ...')
    ap.add_argument('--auto', action='store_true',
                    help='auto-discover every domain and split into per-domain files')
    ap.add_argument('-o', '--outdir', default='results', help='output directory')
    ap.add_argument('--dedupe', action='store_true',
                    help='drop duplicate login:password lines per output file (uses RAM)')
    ap.add_argument('-q', '--quiet', action='store_true', help='no live stats')
    ap.add_argument('-v', '--version', action='version',
                    version=f'ComboFilter v{VERSION} — Made by {AUTHOR}')
    args = ap.parse_args()

    banner = f"""
==================================================
   ComboFilter v{VERSION}            Made by {AUTHOR}
   combo-list domain filterer | streaming engine
==================================================
"""
    print(banner)

    if not args.keywords and not args.auto:
        print("[!] koi keyword nahi mila. -k netflix.com spotify.com do, ya --auto use karo.")
        sys.exit(1)

    if not os.path.isfile(args.input):
        print(f"[!] Input file nahi mila: {args.input}")
        sys.exit(1)
    if not args.input.lower().endswith('.txt'):
        print(f"[!] Sirf .txt files accept hoti hain. {args.input} rejected.")
        sys.exit(1)

    print(f"[*] Input   : {args.input}")
    print(f"[*] Size    : {os.path.getsize(args.input):,} bytes")
    if args.keywords:
        print(f"[*] Targets : {', '.join(args.keywords)}")
    if args.auto:
        print("[*] Mode    : AUTO domain discovery (per-domain files)")
    print(f"[*] Output  : {args.outdir}/  (name_keyword_<random-int>.txt)")
    print("-" * 50)

    t0 = time.perf_counter()
    pool, kw_list, kw_counts, lines, matched, malformed, empty, run_id = run(
        args.input, args.keywords, args.auto, args.outdir, args.dedupe, args.quiet)
    elapsed = time.perf_counter() - t0

    if sys.stderr.isatty() and not args.quiet:
        sys.stderr.write('\r' + ' ' * 200 + '\n')

    speed = lines / elapsed if elapsed > 0 else 0
    print()
    print("=" * 50)
    print(f"  DONE — Made by {AUTHOR}")
    print("=" * 50)
    print(f"  Valid lines processed : {fmt_int(lines)}")
    print(f"  Speed                : {fmt_int(speed)} lines/sec")
    print(f"  Processing time      : {fmt_time(elapsed)}  ({elapsed:.2f}s)")
    print(f"  Malformed skipped    : {fmt_int(malformed)}")
    print(f"  Empty lines          : {fmt_int(empty)}")
    print(f"  Random run ID        : {run_id}")
    print("-" * 50)

    if kw_list:
        print("  KEYWORD RESULTS (live-counted, 0 = not found):")
        for kw in kw_list:
            print(f"    {kw.decode():<30} : {fmt_int(kw_counts[kw])}")
    else:
        print("  (no targeted keywords — auto mode only)")

    if pool.counts:
        print("-" * 50)
        print("  OUTPUT FILES (sorted by hits):")
        for name, cnt in sorted(pool.counts.items(), key=lambda x: -x[1]):
            path = os.path.join(args.outdir, f"{name}_{run_id}.txt")
            print(f"    {fmt_int(cnt):>12}  {path}")
    print()
    print(f"  [+] Files saved in: {os.path.abspath(args.outdir)}")
    print(f"  ComboFilter v{VERSION} — Made by {AUTHOR}")


if __name__ == '__main__':
    main()

# ComboFilter

High-speed combo-list URL filterer — **Made by @torbug**

Combos in, sorted `login:password` combos out. Built to chew through **10M–100M+ line** files without breaking a sweat.

## What it does

You feed it a `.txt` combo file where each line looks like:

```
https://netflix.com:john@gmail.com:pass123
www.spotify.com|mike_2007:hunter2   <- no, colons only. url:login:password
```

It extracts the domain from the URL part (strips `https://`, `www.`, ports, paths, `user:pass@` junk), matches it against your target keywords, and saves every hit as `login:password` — one clean file per keyword.

## Usage

```
python combofilter.py -i combos.txt -k netflix.com spotify.com
python combofilter.py -i combos.txt --auto
python combofilter.py -i combos.txt -k netflix.com --auto -o my_results
```

| Flag | Meaning |
|---|---|
| `-i` | input file (`.txt` only — anything else is rejected) |
| `-k` | one or more target keywords: `netflix.com spotify.com hbo.com` |
| `--auto` | discover EVERY domain in the file, split into per-domain files |
| `-o` | output directory (default `results/`) |
| `--dedupe` | drop duplicate `login:password` lines per output file |
| `-q` | no live stats (for scripting) |

## Keyword matching

Enter `netflix.com` and it catches all of these:

```
https://netflix.com
http://NETFLIX.COM
www.netflix.com
accounts.netflix.com
secure.netflix.com
ftp://files.netflix.com:2121/path
https://user:pass@login.netflix.com:8843/x
```

But it will NOT be fooled by:

```
notnetflix.com            <- different domain
netflix.com.evil.io       <- subdomain trick
netflixx.com
```

## Auto mode

Don't know what's inside the file? `--auto` scans everything, groups every line by its parent domain (subdomains fold up: `accounts.netflix.com` -> `netflix.com`), and writes one file per domain — primevideo, netflix, crunchyroll, whatever's in there. Output summary is sorted by hit count so the big fish surface first.

## Output files

Named `<keyword>_<18-digit-random-int>.txt`, e.g.:

```
results/netflix.com_466017029839746906.txt
results/spotify.com_466017029839746906.txt
```

Same run = same random ID, so one run's outputs never collide with the next.

## Realtime stats

While it runs you get a live-updating bar: progress %, lines/sec, total lines, per-keyword hit counters climbing in real time, and elapsed time. Keywords with 0 hits show 0. Final summary prints processing time, speed, malformed-line count, and every output file sorted by size.

## Performance (real numbers, not vibes)

Tested on a 10,000,000-line / 398 MB file:

```
Speed            : 211,468 lines/sec
Processing time  : 47 seconds
Peak RAM         : 79 MB
```

The engine streams 8 MB binary chunks — RAM stays flat whether the file is 10M or 100M lines. A LRU pool caps open file handles, so even a file with thousands of unique domains can't crash it.

## ComboFilter v1.0 — Made by @torbug

# StreamGuard checker/ — account & cookie validator

Made by @torbug (for LO). Proves which cookies / login:password are valid,
which plan they carry, and which region they sit in — by replaying the
authenticated endpoint the app itself uses.

## How it works (the short version)

You find the account endpoint ONCE by MITM / Burp / mitmproxy on your *own*
device and app. Drop it into `config.json`. This script then replays that
endpoint with each cookie and parses the JSON reply for:

- valid / invalid
- plan (Premium / Standard / Basic / ...)
- region / country
- account status

Fresh output per run — output ALWAYS follows the input you fed it, never a
stale repeat. Every run gets its own timestamp + run_id in the filenames.

## Setup

1. `pip install -r requirements.txt`
2. Edit `config.json`:

   `endpoint` — the JSON endpoint you captured via MITM.
   `build_id` — current Netflix build id (rotate when you start seeing 401s
                on every line; that means the build id went stale, not that
                your cookies died).
   `api_key`  — optional header key if the service needs one.
   `region_path` / `plan_path` / `status_path` — dot-notation JSON pointers
   into the endpoint's reply. Config comes pre-set for the Netflix
   `/accountMenu` and `pathEvaluator` shapes; adjust to match your capture.

## Usage

Check cookies:
```
python3 validator.py -c cookies.txt
```

Check `login:password` combos (logs in first, then validates):
```
python3 validator.py -C combo.txt --login-endpoint https://<host>/.../login
```

Both at once, more workers, results elsewhere:
```
python3 validator.py -c cookies.txt -C combo.txt -t 40 -o out
```

Through proxies (one per line):
```
python3 validator.py -c cookies.txt -p proxies.txt
```

## Input formats (one per line, auto-detected)

- Cookie lines: anything you'd put in a `Cookie` header, passed through
  verbatim. e.g.
  `netflixid=abc123; Secure; Path=/; Domain=.netflix.com`
- Creds lines (with `-C`): `email:password` / `login:password`, converted to
  a session cookie via the login endpoint first.

## Output

- `out/valid_<ts>_<runid>.txt`   — hits, each tagged `region=.. plan=.. status=..`
- `out/invalid_<ts>_<runid>.txt` — rejected / dead
- `out/errors_<ts>_<runid>.txt`  — network / parse failures
- `out/report_<ts>_<runid>.json` — machine-readable totals + file paths

## Operational notes

- **Build id drift** is the #1 cause of "everything suddenly 401". Re-capture
  a fresh build id via MITM and update `config.json`.
- Don't hammer — keep `-t` around 20-40 and use proxies for real volume or
  the IP gets throttled.
- Region / plan read-outs are only as good as the paths in `config.json`.
  If you see `UNKNOWN`, inspect one live reply and update the path pointers.

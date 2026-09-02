#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
torbugbot — adaptive combo filter bot (NO AI, pure heuristics)
Made by @torbug

Drop any .txt combo file in chat. Bot sniffs every line:
  url:login:pass  -> domain extraction (https://, www., ports, subdomains stripped)
  email:pass      -> sorted per email-domain
  phone:pass      -> sorted per country code
  user:pass       -> usernames file
Modes: AUTO (sort everything by domain/category) or KEYWORDS (targeted).
Live stats via message edits while processing. Results sent back as files.
"""

import os, re, io, sys, time, json, random, zipfile, logging, threading
import requests

TOKEN = ""
if os.environ.get("BOT_TOKEN"):
    TOKEN = os.environ["BOT_TOKEN"]
elif len(sys.argv) > 1:
    TOKEN = sys.argv[1]
API = f"https://api.telegram.org/bot{TOKEN}"
DL = f"https://api.telegram.org/file/bot{TOKEN}"
WORK = os.path.expanduser("~/Filtering/bot_results")
MB20 = 20 * 1024 * 1024          # telegram bot download cap
EDIT_EVERY = 2.0                  # sec between live-stat edits
CC_MAP = {1:"US_CA",7:"RU_KZ",20:"Egypt",27:"SouthAfrica",30:"Turkey",31:"Netherlands",
32:"Belgium",33:"France",34:"Spain",36:"Hungary",39:"Italy",40:"Romania",41:"Switzerland",
43:"Austria",44:"UK",45:"Denmark",46:"Sweden",47:"Norway",48:"Poland",49:"Germany",
51:"Peru",52:"Mexico",53:"Cuba",54:"Argentina",55:"Brazil",56:"Chile",57:"Colombia",
58:"Venezuela",60:"Malaysia",61:"Australia",62:"Indonesia",63:"Philippines",64:"NewZealand",
65:"Singapore",66:"Thailand",81:"Japan",82:"SouthKorea",84:"Vietnam",86:"China",
88:"Bangladesh",90:"Turkey",91:"India",92:"Pakistan",93:"Afghanistan",94:"SriLanka",
212:"Morocco",216:"Tunisia",218:"Libya",233:"Ghana",234:"Nigeria",237:"Cameroon",
251:"Ethiopia",254:"Kenya",255:"Tanzania",256:"Uganda",351:"Portugal",352:"Luxembourg",
353:"Ireland",355:"Albania",356:"Malta",358:"Finland",359:"Bulgaria",370:"Lithuania",
371:"Latvia",372:"Estonia",373:"Moldova",380:"Ukraine",385:"Croatia",386:"Slovenia",
420:"Czechia",421:"Slovakia",502:"Guatemala",503:"ElSalvador",504:"Honduras",
505:"Nicaragua",507:"Panama",509:"Haiti",593:"Ecuador",598:"Uruguay",852:"HongKong",
880:"Bangladesh",886:"Taiwan",961:"Lebanon",962:"Jordan",963:"Syria",964:"Iraq",
965:"Kuwait",967:"Yemen",968:"Oman",971:"UAE",972:"Israel",973:"Bahrain",974:"Qatar",
976:"Mongolia",977:"Nepal",994:"Azerbaijan",995:"Georgia"}

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(message)s")
log = logging.getLogger("torbugbot")

# ─────────────────────────── telegram api ───────────────────────────

def tg(method, **params):
    try:
        r = requests.post(f"{API}/{method}", params=params, timeout=60)
        j = r.json()
        if not j.get("ok"):
            log.warning("%s -> %s", method, j.get("description"))
        return j
    except Exception as e:
        log.error("%s crashed: %s", method, e)
        return {"ok": False}

def tg_send(chat, text, kb=None):
    return tg("sendMessage", chat_id=chat, text=text[:4000],
              parse_mode="HTML", reply_markup=json.dumps(kb) if kb else None)

def tg_edit(chat, mid, text):
    return tg("editMessageText", chat_id=chat, message_id=mid, text=text[:4000],
              parse_mode="HTML")

def tg_doc(chat, path, caption=""):
    with open(path, "rb") as f:
        r = requests.post(f"{API}/sendDocument",
                          data={"chat_id": chat, "caption": caption[:1000]},
                          files={"document": (os.path.basename(path), f)}, timeout=300)
        return r.json().get("ok", False)

def dl_file(file_id, dest):
    j = tg("getFile", file_id=file_id)
    if not j.get("ok"):
        return False
    p = j["result"]["file_path"]
    r = requests.get(f"{DL}/{p}", timeout=300)
    if r.status_code != 200:
        return False
    open(dest, "wb").write(r.content)
    return True

# ─────────────────────────── line classifier (heuristics only) ───────────────────────────

DOM_RE = re.compile(r'^[a-z0-9][a-z0-9.\-]*\.[a-z]{2,}$', re.I)
PHONE_RE = re.compile(r'^\+?\d[\d\s\-()]*$')

def extract_domain(url):
    d = url
    i = d.find("://")
    if i != -1: d = d[i+3:]
    for sep in ("/", "?", "#"):
        i = d.find(sep)
        if i != -1: d = d[:i]
    i = d.rfind("@")
    if i != -1: d = d[i+1:]
    i = d.rfind(":")
    if i != -1: d = d[:i]
    d = d.lower().strip(".")
    if d.startswith("www."): d = d[4:]
    return d if DOM_RE.match(d) else ""

def registrable(dom):
    p = dom.rsplit(".", 2)
    return f"{p[-2]}.{p[-1]}" if len(p) == 3 else dom

def phone_cc(login):
    digits = re.sub(r"\D", "", login)
    for L in (3, 2, 1):
        if len(digits) > L and int(digits[:L]) in CC_MAP:
            return int(digits[:L]), CC_MAP[int(digits[:L])]
    return None, None

def classify(line):
    """-> dict(kind=..., payload=login:pass) or None. Pure pattern math, no AI."""
    parts = line.rsplit(":", 2)
    if len(parts) == 3 and all(parts):
        url, login, pwd = parts
        dom = extract_domain(url)
        if dom:
            return {"kind": "url", "dom": dom, "pay": f"{login}:{pwd}"}
    parts = line.split(":", 1)
    if len(parts) == 2 and parts[0] and parts[1]:
        login, pwd = parts
        login = login.strip()
        if "@" in login and re.search(r"@[^@]+\.[^@]+$", login):
            dom = login.rsplit("@", 1)[1].lower()
            if re.match(r"^[a-z0-9.\-]+\.[a-z]{2,}$", dom):
                return {"kind": "email", "dom": dom, "pay": f"{login}:{parts[1]}"}
        d2 = re.sub(r"\D", "", login)
        if login and (login.startswith("+") or (d2 and not re.search(r"[a-zA-Z]", login))):
            cc, name = phone_cc(login)
            return {"kind": "phone", "cc": cc, "name": name, "pay": line}
        if login:
            return {"kind": "user", "pay": line}
    return None

def norm_kw(kw):
    kw = kw.strip().lower()
    kw = kw.split("://", 1)[-1]
    for sep in ("/", "?", "#"):
        kw = kw.split(sep)[0]
    kw = kw.split("@")[-1].split(":")[0]
    return kw[4:] if kw.startswith("www.") else kw

def kw_match(dom, kw):
    return dom == kw or dom.endswith("." + kw)

def safe_name(s):
    for ch in '\\/:*?"<>| ':
        s = s.replace(ch, "_")
    return s or "unknown"

# ─────────────────────────── filter engine ───────────────────────────

class Engine(threading.Thread):
    def __init__(self, bot, chat, src_path, kws, auto):
        super().__init__(daemon=True)
        self.bot, self.chat, self.src, self.kws, self.auto = bot, chat, src_path, kws, auto
        self.run_id = random.randint(10**17, 10**18 - 1)
        self.out = os.path.join(WORK, str(self.run_id))
        os.makedirs(self.out, exist_ok=True)
        self.stop = False
        self.files, self.counts = {}, {}
        self.lines = self.hits = self.bad = self.seen_dup = 0
        self.t0 = None

    def w(self, name, payload):
        path = os.path.join(self.out, f"{safe_name(name)}_{self.run_id}.txt")
        h = self.files.get(path)
        if h is None:
            if len(self.files) >= 256:                      # LRU-ish cap
                old = next(iter(self.files.values())); old.close()
            h = open(path, "a", encoding="utf-8"); self.files[path] = h
        h.write(payload + "\n")
        self.counts[name] = self.counts.get(name, 0) + 1

    def run(self):
        self.t0 = time.time()
        seen = set()
        with open(self.src, encoding="utf-8", errors="replace") as f:
            for raw in f:
                if self.stop:
                    break
                raw = raw.rstrip("\r\n")
                if not raw:
                    continue
                c = classify(raw)
                if c is None:
                    self.bad += 1
                    continue
                self.lines += 1
                if raw in seen:
                    self.seen_dup += 1
                    continue
                seen.add(raw)
                self.route(c)
        for h in self.files.values():
            h.close()
        self.finish()

    def route(self, c):
        if c["kind"] == "url":
            hit = False
            for kw in self.kws:
                if kw_match(c["dom"], kw) or kw_match(registrable(c["dom"]), kw):
                    self.w(kw, c["pay"]); hit = True
            if not hit and self.auto:
                self.w(registrable(c["dom"]), c["pay"])
        elif c["kind"] == "email":
            self.w(c["dom"], c["pay"])
        elif c["kind"] == "phone":
            if c["cc"]:
                self.w(f"+{c['cc']}_{c['name']}", c["pay"])
            else:
                self.w("phone_unknown", c["pay"])
        else:
            self.w("usernames", c["pay"])

    def stats(self):
        el = time.time() - self.t0 if self.t0 else 0
        sp = self.lines / el if el > 0 else 0
        rows = "".join(f"\n<code>{k[:28]:<28}</code>{v:>9,}" for k, v in
                       sorted(self.counts.items(), key=lambda x: -x[1])[:8])
        return (f"⚡ <b>torbugbot</b> — filtering\n"
                f"lines: {self.lines:,} | {sp:,.0f}/s | elapsed {int(el)}s\n"
                f"hits: {sum(self.counts.values()):,} | dup: {self.seen_dup:,} | bad: {self.bad:,}"
                f"{rows}")

    def finish(self):
        el = time.time() - self.t0
        total = sum(self.counts.values())
        rows = "".join(f"\n<code>{k[:28]:<28}</code>{v:>9,}" for k, v in
                       sorted(self.counts.items(), key=lambda x: -x[1])[:15])
        msg = (f"✅ <b>DONE</b> — {el:.1f}s | {self.lines:,} lines | {total:,} sorted\n"
               f"files: {len(self.counts)}{rows}")
        self.bot.edit(self.chat, msg)
        # ship files
        paths = [os.path.join(self.out, f"{safe_name(n)}_{self.run_id}.txt")
                 for n in self.counts]
        paths = [p for p in paths if os.path.exists(p)]
        try:
            if len(paths) > 12:
                z = os.path.join(self.out, f"filtered_{self.run_id}.zip")
                with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as zf:
                    for p in paths:
                        zf.write(p, os.path.basename(p))
                tg_doc(self.chat, z, f"filtered — Made by @torbug")
            else:
                for p in sorted(paths):
                    tg_doc(self.chat, p, "Made by @torbug")
        except Exception as e:
            log.error("send fail %s", e)
        tg_send(self.chat, f"📁 saved: <code>{self.out}</code>\nMade by @torbug ⚡")
        self.bot.done(self.chat)

# ─────────────────────────── bot core ───────────────────────────

HELP = ("⚡ <b>torbugbot</b> — adaptive combo filter (no AI, pure heuristics)\n\n"
        "Send me any <b>.txt</b> combo file. I auto-detect each line:\n"
        "• <code>url:login:pass</code> → domain filtering (https://, www., subdomains — all caught)\n"
        "• <code>email:pass</code> → sorted per email domain\n"
        "• <code>phone:pass</code> → sorted per country code\n"
        "• <code>user:pass</code> → usernames\n\n"
        "After upload choose <b>AUTO</b> (sort everything) or <b>KEYWORDS</b> "
        "(type targets like <code>netflix.com spotify.com</code>).\n"
        "Live stats while I work. Results come back as files.\n\n"
        "/cancel — stop current job\nMade by @torbug")

KB = {"inline_keyboard": [[
    {"text": "🚀 AUTO sort all", "callback_data": "auto"},
    {"text": "🎯 Keywords", "callback_data": "kw"},
]]}

class Bot:
    def __init__(self):
        self.jobs = {}          # chat -> Engine
        self.state = {}         # chat -> 'idle'|'kw'|'busy'
        self.pending = {}       # chat -> src path
        self.status_mid = {}    # chat -> live stats msg id

    def edit(self, chat, text):
        mid = self.status_mid.get(chat)
        if mid:
            tg_edit(chat, mid, text)

    def done(self, chat):
        self.jobs.pop(chat, None); self.state[chat] = "idle"
        self.pending.pop(chat, None); self.status_mid.pop(chat, None)

    def handle(self, u):
        msg = u.get("message") or u.get("edited_message") or {}
        cq = u.get("callback_query") or {}
        chat = (msg.get("chat") or cq.get("message", {}).get("chat") or {}).get("id")
        if not chat:
            return
        text = (msg.get("text") or "").strip()
        doc = msg.get("document")

        if text == "/start":
            tg_send(chat, HELP); return
        if text == "/cancel":
            j = self.jobs.get(chat)
            if j: j.stop = True
            self.state[chat] = "idle"; self.pending.pop(chat, None)
            self.status_mid.pop(chat, None)
            tg_send(chat, "🛑 cancelled."); return

        # keyword answer
        if self.state.get(chat) == "kw" and text:
            kws = list(dict.fromkeys(norm_kw(k) for k in re.split(r"[,\s]+", text) if k))
            src = self.pending.pop(chat, None)
            if not src or not os.path.exists(src):
                tg_send(chat, "file gaayab — dobara bhejo"); self.state[chat] = "idle"; return
            self.launch(chat, src, kws, auto=False); return

        if doc:
            if self.state.get(chat) == "busy":
                tg_send(chat, "⏳ busy — /cancel pehle"); return
            name = doc.get("file_name", "combo.txt")
            if not name.lower().endswith(".txt"):
                tg_send(chat, "❌ only .txt files"); return
            if doc.get("file_size", 0) > MB20:
                tg_send(chat, "❌ 20MB+ file Telegram bot API cap — split karke bhejo"); return
            dest = os.path.join(WORK, f"in_{int(time.time())}_{name}")
            os.makedirs(WORK, exist_ok=True)
            tg_send(chat, f"📥 downloading <code>{name}</code>…")
            if not dl_file(doc["file_id"], dest):
                tg_send(chat, "download fail"); return
            # caption keywords shortcut
            cap = (msg.get("caption") or "").strip()
            if cap:
                kws = list(dict.fromkeys(norm_kw(k) for k in re.split(r"[,\s]+", cap) if k))
                self.launch(chat, dest, kws, auto=True); return
            self.pending[chat] = dest
            tg_send(chat, "file mila. mode chuno:", KB)
            return

        if cq:
            tg("answerCallbackQuery", callback_query_id=cq["id"])
            data = cq.get("data")
            if data == "auto":
                src = self.pending.pop(chat, None)
                if not src:
                    tg_send(chat, "file nahi — dobara bhejo"); return
                self.launch(chat, src, kws=[], auto=True)
            elif data == "kw":
                self.state[chat] = "kw"
                tg_send(chat, "🎯 keywords bhejo (space/comma se separate):\n<code>netflix.com spotify.com hbo.com</code>")
            return

        if text and self.state.get(chat) == "idle":
            tg_send(chat, HELP)

    def launch(self, chat, src, kws, auto):
        j = Engine(self, chat, src, kws, auto)
        self.jobs[chat] = j
        self.state[chat] = "busy"
        mode = "KEYWORDS: " + ", ".join(kws) if kws else "AUTO"
        m = tg_send(chat, f"⚙️ <b>filtering start</b> — {mode}")
        if m.get("ok"):
            self.status_mid[chat] = m["result"]["message_id"]
        j.start()

    def poll(self):
        off = 0
        log.info("torbugbot polling…")
        while True:
            try:
                r = requests.get(f"{API}/getUpdates",
                                 params={"offset": off, "timeout": 25, "allowed_updates": json.dumps(["message", "callback_query"])},
                                 timeout=35)
                j = r.json()
                if not j.get("ok"):
                    time.sleep(3); continue
                for u in j.get("result", []):
                    off = u["update_id"] + 1
                    try:
                        self.handle(u)
                    except Exception:
                        log.error("handler: %s", traceback.format_exc())
                # live stats ticker
                for chat, job in list(self.jobs.items()):
                    if job.is_alive() and job.t0 and time.time() - job.t0 > 1:
                        if not hasattr(job, "_last_edit") or time.time() - job._last_edit > EDIT_EVERY:
                            self.edit(chat, job.stats())
                            job._last_edit = time.time()
            except KeyboardInterrupt:
                log.info("bye"); break
            except Exception as e:
                log.error("poll: %s", e); time.sleep(3)

if __name__ == "__main__":
    if not TOKEN:
        print("usage: BOT_TOKEN=... python3 bot.py   |   python3 bot.py <token>"); sys.exit(1)
    os.makedirs(WORK, exist_ok=True)
    Bot().poll()

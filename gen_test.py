#!/usr/bin/env python3
"""Test-data generator for ComboFilter — creates a combo file exercising
every parser edge case."""
import random

random.seed(42)

domains = [
    ("netflix.com", ["https://netflix.com", "www.netflix.com", "accounts.netflix.com", "NETFLIX.COM", "https://secure.netflix.com"]),
    ("spotify.com", ["https://www.spotify.com", "spotify.com", "login.spotify.com"]),
    ("primevideo.com", ["https://www.primevideo.com", "primevideo.com", "auth.primevideo.com"]),
    ("crunchyroll.com", ["https://www.crunchyroll.com", "crunchyroll.com"]),
    ("notnetflix.com", ["https://notnetflix.com"]),          # must NOT match netflix.com
    ("netflix.com.evil.io", ["http://netflix.com.evil.io"]), # must NOT match netflix.com
    ("hbo.com", ["https://www.hbo.com"]),
    ("discord.com", ["https://discord.com", "canary.discord.com"]),
]

names = ["john", "mike", "sara", "x_user", "email@gmail.com", "user+tag"]
pwds = ["pass123", "hunter2", "P@ssw0rd!", "qwerty", "letmein"]

lines = []
for dom, variants in domains:
    for i in range(25):
        v = random.choice(variants)
        u = random.choice(names)
        p = random.choice(pwds)
        lines.append(f"{v}:{u}:{p}")

# exact-match negative test: netflix.com keyword must not catch this
lines.append("https://netflix.com.evil.io:bad:bad")

# malformed lines
lines.append("justoncolon")                    # no colons
lines.append("only:one")                        # 1 colon
lines.append("a:b:c:d")                        # 4 colons -> rsplit gives url=a:b? no: rsplit(b':',2) => ['a:b', 'c', 'd'] url='a:b' -> domain extract on 'a:b'... hmm
lines.append("user@host:pass")                  # 2 colons? "user@host:pass" rsplit => ['user@host','pass'] len2 -> malformed
lines.append("https://netflix.com:")            # empty password -> malformed
lines.append(":login:pass")                     # empty url -> malformed
lines.append("")                               # empty line
lines.append("")                                # another empty
lines.append("https://netflix.com:cool@netflix.com:88/x:y:z")  # user:pass@host:port in URL
lines.append("https://t.co:1:2")                # short domain
lines.append("ftp://files.spotify.com:2121/a/b:login:pwd")

with open("/home/kali/Filtering/test_combos.txt", "w", newline="") as f:
    f.write("\ufeff")  # BOM on first line
    f.write("\r\n".join(lines[:50]) + "\r\n")  # first 50 CRLF
    f.write("\n".join(lines[50:]))             # rest LF

print(f"wrote {len(lines)} lines")

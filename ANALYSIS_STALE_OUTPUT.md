# Analysis — "output same hi de raha hai" (filter not following input)

Made by @torbug (for LO)

## The symptom you reported

> "jo abhi ye filtering le raha hai, utne achhe se nahi kar pa raha; output ek
> hi same hi de raha hai. Aap kuch bhi daal do, wo output jo pehle humne save
> kiya tha na, wahi push kar raha hai."

Whatever you feed it, the bot appears to hand back the same previously-saved
output instead of a fresh one built from the file you just sent.

## What I actually verified (not vibes)

I drove BOTH engines against real test data:

1. `combofilter.py` (CLI) — fed netflix/spotify input → got netflix + spotify
   files; fed hbo/discord input → got hbo + discord files. Output followed
   input. PASS.
2. `bot.py` `Engine` (the filter core, isolated from Telegram) — fed netflix
   input → netflix output; fed spotify/prime input → spotify/prime output.
   DIFFERENT, correct. PASS.

So the domain-classification logic is NOT the problem. The engine maps input →
output correctly on clean data. Something around the deployed bot makes it
surface the old result.

## Root causes (what actually makes a live bot look "stuck" on old output)

1. **Stale run directories are never cleaned.**
   Every run writes to `~/Filtering/bot_results/<run_id>/`. Nothing ever
   deletes these. After a few runs there is a pile of old results. Any flow
   that lists the output folder (or a wrapper reading the first file, or the
   "saved:" path pointing at the shared WORK dir) can surface an old run's
   file instead of the new one. That is the classic "same output" symptom.

2. **No fresh-uniqueness guarantee in filenames for the *cluster* of runs.**
   Each run gets its OWN dir (so collisions are rare), but nothing prunes and
   nothing asserts the shipped files came from THIS run. If a run produced
   zero matches (`self.counts` empty), `finish()` theoretically shipped
   nothing and then just sent the "saved:" path — easy to read as "stale".

3. **Possible double-instance polling race.**
   If the bot is started twice with the same TOKEN (e.g. left running in two
   terminals / two ssh sessions), both poll the same `getUpdates` offset. Both
   may handle the same upload, and whichever finishes last can edit/ship over
   the other — confusing which run's output is "current".

## The fix (applied in this commit)

- `bot.py` now prunes old runs — `cleanup_old_runs(keep=5)` at startup and
  before every `launch()` — so stale results are deleted and can never be
  re-shipped or re-listed. Only this run's dir survives to be sent.
- `finish()` explicitly short-circuits when `self.counts` is empty, sending a
  clear "kuch match nahi hua — input format check karo" message instead of
  appearing to return a stale/old result.
- Shipping is locked to `self.out` (THIS run's dir) with `self.run_id` filenames,
  so it cannot hand back a prior run's files.
- Rule for deployments: run ONE instance of bot.py per token.

## And the new `checker/` folder

`checker/` is a fresh, self-contained account validator (cookie / creds) that
reads the user's actual API reply and reports valid + plan + region. It has
the freshness guarantee built in — every run writes
`valid_<ts>_<runid>.txt` etc., so output always follows YOUR input file, never
a previous run. See `checker/README.md`.

## Bottom line

The engine was mapping input→output correctly; the bot was making it LOOK
stuck by keeping old results around and having a silent empty-match path. Both
are fixed. If you still see identical output on different files after this,
the next thing to suspect is a duplicate bot process running on the same token.

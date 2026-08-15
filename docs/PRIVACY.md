# Privacy model — the honest envelope

This document describes what the noise machine actually defends against, how, and — the important
part — what it does **not** defend against. It is deliberately not marketing. If you are relying on
this tool to protect sensitive DNS activity, read the [Honest limits](#honest-limits) section first
and act on it.

**One sentence:** cover traffic makes de-anonymizing which domains you visit *expensive and
probabilistic*; it is not a guarantee, and it defends the **wire**, not the **host**.

## Threat model

### What it defends

**An on-path or upstream observer** — your ISP, a resolver operator, someone sniffing the link — who
sees your outgoing DNS queries and tries to unmask **which domains you actually visit**, and **how
much / when** you are active.

Against that observer the goal is: the queries that leave the box should be indistinguishable from a
generic, busy household's DNS, so that the observer cannot subtract the cover traffic back out and
recover your real activity.

### What is out of scope

- **The local databases.** Decoy queries are recorded in `querylog.db` labeled `decoy=1`. Anyone
  with **the file** or **an authenticated LAN web UI session** can read your real query history
  directly — filtering decoys out is trivial for them. This is by design (it powers the Noise
  dashboard) and it is an accepted trade-off: **this tool defends the wire, not the host.** Protect
  the box and its LAN like any other server holding sensitive logs.
- **The web UI is login-gated but LAN-only, on purpose.** First-run password setup plus a signed
  session cookie stop a random LAN device from owning the box (see the auth section in
  [api-reference.md](api-reference.md#auth-gate)), but auth does not turn this into a wire defense:
  without TLS the cookie rides cleartext on the LAN, and anyone with a valid login or file access can
  still read your real history. Do not expose the web UI to the internet.
- Config-at-rest secrets, local instance fingerprinting, and other host-side threats. Out of scope.

## How it works

Two independent problems, two independent layers.

### Hiding *how much* you browse — persona-compensating cover

A naive "add random noise" or "track my real rate and match it" scheme leaks your **activity level**:
quiet periods still look quiet, busy periods still look busy, and a self-exciting (Hawkes) model
recovers *when* and *how much* you were active even if it can't read the domains.

Instead the engine emits toward a fixed **household persona curve** — a generic diurnal browsing
shape with a busy-hour ceiling and a pre-dawn floor — **subtractively**:

```
decoy_rate(t) = max(0, targetCurve(t) − recentRealRate(t))
```

Decoys fill the gap between the target curve and your actual traffic, so **total egress ≈ the target
curve regardless of what you do.** Your real queries ride *within* the budget (never delayed — unlike
constant-rate cover, real lookups are not slowed down); decoys absorb the slack. The observer sees the
same generic household shape every day, which reveals nothing per-day.

Knobs: `privacy.decoy.personaCover`, `targetQpmPeak`, `targetQpmTrough`.

### Hiding *which* domains — sessions, cohorts, fingerprint-matching

Making the *volume* generic isn't enough if the decoy domains are filterable. So the decoys are
shaped to look like real browsing:

- **Cohorts, not lone queries.** Real page loads fire a burst — first-party + `www` + CDN + fonts +
  analytics + trackers — within a couple of seconds. The engine records real cohorts (their member
  set, including blocked members, and inter-arrival timing) and **emits decoys as whole recorded
  cohorts** with realistic timing, primary domain first. A lone tracker query is a tell; a cohort
  isn't. (`cohortPct`)
- **Session coherence.** Decoy sessions walk plausible successor chains learned from your real
  timeline instead of independent random picks — real browsing is session-structured, IID chaff
  isn't. (`sessionCoherence`, `stepPct`)
- **Fingerprint matching.** Decoys are stamped with the EDNS option order, buffer size, 0x20 case
  pattern, qtype mix (incl. HTTPS/SVCB and A+AAAA pairing), and cookie state sampled from your real
  clients, and attributed to plausible client identities — so chaff isn't filterable by uniformity or
  by "wrong-shaped" packets. (`fingerprintMatch`, `personaAttribution`, `dualStackPct`)
- **Realism details** that remove "always-succeeds / always-UDP / web-only" tells: device background
  chatter (connectivity checks, NTP, PTR), transport/qtype/failure diversity, miss-chaff
  (plausible NXDOMAIN labels), revisit-cadence modeling, adaptive back-off under upstream strain.
  (`chatterPct`, `tcpPct`, `missChaffPct`, `failChaffPct`, `revisitCadence`, `adaptiveBackoff`)

### Dissolving the "you run a blocking resolver" tell — shadow completion

A plain ad-blocker has a signature: it *never* emits the tracker/ad queries a normal device would, so
its cohorts are conspicuously missing their expected members. That alone marks you out.

**Shadow completion** decouples the client answer from the wire query. When a client hits a blocked
domain, the client still gets the block response (**ad-blocking stays fully intact — the ad never
loads**), *but* JungleBlock also egresses the real upstream query and **discards** the answer. Now:

- real page-load cohorts are **complete on the wire** (trackers included), matching decoy cohorts —
  no "cohort missing its trackers = real visit" tell;
- tracker egress happens on **both** real and decoy paths, so the box looks like a normal unprotected
  device.

A discarded DNS lookup is not an ad impression, so this leaks nothing to the tracker. Knob:
`privacy.shadowBlockedQueries` (default on, but **only activates when the decoy engine is enabled** —
so a blocking-only setup never egresses blocked queries uncovered). Blocklist domains are also used
*as* cover traffic — real devices hit ad/tracker domains constantly, so they make excellent noise.

### Covering your personal tail — persistent corpus + pre-warming

Public lists (Tranco, blocklists) cover the *popular* web but not `bank.example`, `*.lan`, your SSO,
or your niche sites. So:

- **Persistent noise corpus.** Every domain your clients have *ever* visited (`decoy=0`) accumulates
  into a durable per-instance `noise_corpus` table and is replayed as cover **forever**. Once you've
  visited a personal domain, an observer can no longer tell a real visit from corpus chaff. This is
  per-instance only and never shared (it's a record of your household's real domains). Knobs:
  `corpusWeight`, `replayWeight`.
- **Pre-warming.** A background worker pulls trending/rising domains into the corpus **before** you
  first visit them, shrinking the first-visit gap. Offline-first: with no `prewarmURL` it mines the
  embedded Tranco mid-popularity band. Knobs: `prewarmEnable`, `prewarmURL`, `prewarmIntervalHours`.

Source selection is weighted so your own domains dominate the cover (visited corpus + replay ≫ public
lists), and companions triggered by real browsing dominate overall — the list's ~million domains
never drown out your few hundred real ones.

## Honest limits

These are real and unfixed. Do not pretend otherwise.

1. **Activity above the persona ceiling still leaks.** The compensating cover hides your activity
   level only *up to* `targetQpmPeak`. If your real traffic exceeds the busy-hour ceiling, total
   egress spikes above the curve and the excess is visible as "unusually active right now." Raise the
   ceiling to cover more (the cost is 24/7 baseline bandwidth — cheap in bytes, but not free).

2. **Genuine first-ever visits leak, briefly.** A domain nobody in your household has ever queried,
   that pre-warming hasn't pulled in, is emitted for real *before* it's in the corpus — a one-shot
   exposure. Recurring personal domains are covered persistently after the first visit; the genuine
   first visit of a truly novel domain is the residual. Lean on encrypted upstreams for the sensitive
   tail.

3. **Recursive-default talks PLAINTEXT to authoritative servers.** The default recursive path resolves
   iteratively from the root over **unencrypted Do53**, so the registrable domain of each lookup is
   visible in clear to the root/TLD/authoritative operators and anyone on that path (mixed in with
   decoys, but in clear). **For the sensitive tail, configure encrypted upstreams** (DoT/DoH/DoQ) and
   route the domains you care about through them — encryption hides the domain from the *link*, and the
   noise engine hides *which* of your encrypted lookups are real.

4. **Volume/timing is only shaped, never erased.** The cover hides *which* domain and — within the
   persona ceiling — *how much*. It decorrelates the flux; it does not remove the fact that DNS is
   happening. A determined adversary fitting a self-exciting model to encrypted, shaped traffic can
   still recover coarse timing signals.

5. **A well-resourced adversary with your own public lists can probabilistically denoise.** The Tranco
   and blocklist decoy sources are public and reproducible. An adversary who runs the same lists can
   down-weight those decoys statistically. That's exactly why your **own** corpus and replayed real
   queries are weighted to dominate the cover — those they *cannot* reproduce — but the public-list
   layer is the weakest, and de-anonymization remains a probability, not an impossibility.

6. **The host is out of scope.** Restated because it matters: decoys are labeled `decoy=1` in the DB.
   File access or LAN UI access = your real history in the clear. This defends the wire.

## Practical guidance

Turn the layers on **together** — they compose, and each alone leaves a tell the others cover:

- **Enable the noise engine** (`privacy.decoy.enable = true`). Defaults are tuned for a home box.
- **Keep shadow completion on** (`privacy.shadowBlockedQueries = true`) — it needs the decoy engine
  enabled to activate.
- **Add encrypted upstreams and route your sensitive tail through them.** This is the single most
  important step for anything you actually care about — it closes the plaintext-recursive and
  first-visit residuals. Combine with `privacy.ednsPadding` to hide query size on those links.
- **Set the persona ceiling above your real peak.** If your household is busier than
  `targetQpmPeak`, raise it so your busy hours don't spike above the curve.
- **Leave the corpus and pre-warming on** so your personal domains get covered and novel domains get
  pulled in ahead of you.
- Optional: `privacy.ttlJitter` and `privacy.queryCaseRandomization` (0x20, forwarding path only)
  harden individual queries; they are anti-tampering/anti-spoofing more than anti-observation.

The full knob list is in [config.schema.json](config.schema.json) under `privacy.decoy`.

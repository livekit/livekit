# Ardius Tech fork of `livekit/livekit`

Forked so we can patch SFU-side issues we hit in production without waiting on
upstream review/release cadence. Kept as a **separate file, not an edit to
README.md**, deliberately — editing the upstream README would conflict on
every future `git merge upstream/master`; this file never will.

- `origin` = this fork (`ardiustech/livekit-server`)
- `upstream` = `https://github.com/livekit/livekit.git`
- Base: `upstream/master` @ `v1.13.5` (the latest tag as of 2026-07-31).
  **Currently deployed production** (watercooler's LiveKit box) is one patch
  behind, at `v1.13.4` — see the patch list below for what changes when this
  fork is actually adopted.

## Patches on top of upstream

### 1. Proactive republish nudge for stuck publisher tracks (`feat/publish-mid-stuck-nudge`)

**Problem:** `pkg/rtc/participant.go`'s `mediaTrackReceived` can receive RTP
for a just-published track before the SDP negotiation assigning it a `mid`
has resolved (logged as `WARN "could not get mid for track"`). Upstream
queues the track (`pendingRemoteTracks`) and waits for the SAME participant
to renegotiate again for some UNRELATED reason
(`handlePendingRemoteTracks`'s three call sites are all client-initiated: a
fresh offer, an AddTrack request, or an unpublish) — there's no code path
where the server proactively asks the stuck publisher to renegotiate. An app
whose usage pattern doesn't naturally trigger another renegotiation (publish
once, then nothing) can have this track sit broken indefinitely — invisible
to every OTHER peer in the room, with the publisher's own client reporting
itself as perfectly healthy.

Full incident writeup + evidence from watercooler's production LiveKit box:
`docs/sfu-multiparty-triage-2026-07-31.md` in the `ardiustech/watercooler`
repo (a 2-minute-long stuck-track incident during a live meeting, 215 failed
mid-resolution attempts on one participant, only cleared when that
participant's client happened to reconnect on its own).

**Fix:** `pkg/rtc/participant.go` — when a track lands in
`pendingRemoteTracks` because `mid == ""`, schedule a check 750ms later
(`scheduleStuckPublishNudge`). If the SAME track is still stuck, proactively
push a small marker payload down that SAME participant's own data channel
(`sendRepublishNudge`, via the existing, unmodified `DataPacket_User` wire
type — `p.TransportManager.SendDataMessage`). No new protobuf message type,
no `livekit-protocol` changes, no `client-sdk-js` changes: `RoomEvent.DataReceived`
already exists in the stock JS SDK and already surfaces this payload
unmodified to app code. **This fork touches exactly one repo.**

The app-side handler (`ardiustech/watercooler`, `feat/sfu-republish-signal-handler`,
PR #162) listens for `RoomEvent.DataReceived` with topic
`_ardiustech_republish_nudge` and republishes ONLY the one named stuck track
(`republishOneTrack`, unpublish+publish of that single track) — NOT
`republishAllTracks()`. That app's OWN client-side blind timer (which DID
call `republishAllTracks()` on a fixed schedule) has since been deleted
entirely: live A/B/C instrumentation (`INVESTIGATION_LOG.md` facts #20-22 on
this repo's [`experiment/mid-resolution-timing`](https://github.com/ardiustech/livekit-server/tree/experiment/mid-resolution-timing)
branch — a diagnostic-only branch, not merged to `master` or this PR's
branch, so that file won't appear in a plain checkout of `feat/publish-mid-stuck-nudge`;
follow the link) proved that blind timer was the DOMINANT source of the
exact "could not get mid for track" collateral damage this whole effort
exists to fix, not a mitigation for it. This reactive, server-signaled
nudge is now the ONLY republish-nudge mechanism on the client — which is
why the dedup/cap/escalation fix below matters: there's no other backstop
left if this one runs away.

**Fixed since first opened (review round, 2026-08-01 — see PR #1 comments):**
- **No de-dup/cap on the nudge timer (was a must-fix).** Every retry of a
  still-stuck track (215, in the production incident) called
  `scheduleStuckPublishNudge` again, arming an independent duplicate
  `time.AfterFunc` with no coordination — under sustained failure this could
  compound into a self-sustaining nudge/renegotiate loop with the app-side
  handler. Added `stuckPublishNudgeState` (per-trackID, guarded by the
  existing `pendingTracksLock`): a duplicate call while a timer is already
  in flight is now a no-op, actual send attempts are capped at
  `stuckPublishNudgeMaxAttempts` (3) per stuck episode, and exhausting that
  cap escalates to `p.IssueFullReconnect(...)` — self-contained recovery
  that doesn't depend on the same (possibly impaired) client renegotiating
  correctly again — instead of nudging indefinitely.
- **No Close()-time cleanup.** Unlike this file's existing
  `migrationTimer`/`disconnectTimer` pattern, a participant disconnecting
  within the 750ms grace period could leave a nudge timer firing after
  teardown. `clearAllStuckPublishNudges()` is now called from `Close()`;
  `onStuckPublishNudgeFired` also checks `IsClosed()`/`IsDisconnected()` as
  a second guard.
- **Reserved topic wasn't blocked on the inbound relay path.** Any
  participant could send a `DataPacket_User` naming the
  `_ardiustech_republish_nudge` topic to another participant, and
  `handleReceivedDataMessage` would relay it like any other user payload.
  The app-side client already rejects this (a relayed message always
  resolves to a real, non-`undefined` `RemoteParticipant`, which its
  `handleDataReceived` guard ignores), so this was never end-to-end
  exploitable — but defense shouldn't depend on only one side of a
  two-repo boundary staying correct forever. Now hard-dropped at the source.
- **Hand-built JSON payload could be invalid for a control-byte trackID.**
  `fmt.Sprintf("%q", trackID)` escapes some control bytes as `\xNN`, not
  valid JSON's `\u00NN` — and `trackID` originates from client-controlled
  SDP/`MediaStreamTrack` id. Switched to `encoding/json.Marshal` on a typed
  `republishNudgePayload` struct.

**Round 2 fixes (full-panel re-review after round 1's fixes landed —
verdict came back "Reconsider," not "ship-with-changes"; all 5 must-fixes
addressed):**
- **MUST-FIX, most severe: a guaranteed self-deadlock on the NORMAL
  (non-stuck) publish path.** `mediaTrackReceived` acquires
  `pendingTracksLock` at entry and only unlocks it inside the `mid==""`
  branch — so the mid-RESOLVED success path (added in round 1) that called
  the LOCKING `clearStuckPublishNudge` was calling `Lock()` on an
  already-held, non-reentrant `utils.RWMutex`. Every track whose `mid`
  resolved on the first try (the overwhelmingly common case) would hang the
  calling goroutine forever, wedging every other operation on that
  participant needing the same lock. Missed in round 1 because a real
  `*webrtc.TrackRemote` can't be constructed in this test suite, so
  `mediaTrackReceived`'s actual runtime path was never unit-exercised. Fixed
  by splitting into `clearStuckPublishNudge` (locking, for external callers)
  and `clearStuckPublishNudgeLocked` (for callers already holding the lock);
  regression test uses a goroutine + timeout to detect a re-introduced
  deadlock without hanging the test suite itself if it recurs.
- **MUST-FIX: escalation was unreachable in the exact scenario this patch
  targets.** `scheduleStuckPublishNudge` is only called from
  `mediaTrackReceived`'s `mid==""` branch, which only fires again on an
  UNRELATED renegotiation. In a "publish once, then nothing" app (this
  fork's own stated target case), one nudge fired at t=750ms and `attempts`
  froze at 1 forever — no further nudges, no escalation, regardless of
  whether the client ever acted on it. `onStuckPublishNudgeFired` now
  re-arms itself (`p.scheduleStuckPublishNudge(trackID)`) after every
  still-stuck firing, making the check self-sustaining until the track
  resolves or the cap trips.
- **MUST-FIX: escalation bypassed an existing operator control on an
  untuned timeline.** `IssueFullReconnect` fired unconditionally once the
  cap tripped (~3s), for a strictly larger set of cases than the one
  incident this targets (a merely-slow client, not just a pathologically
  stuck one) — and the incident this cites ran 2 MINUTES with ordinary
  renegotiations already failing to help, so ~3s is unvalidated against the
  fork's own evidence. Now gated behind `p.params.ReconnectOnPublicationError`,
  the same flag `onPublicationError` already uses elsewhere in this file;
  logs clearly either way.
- **MUST-FIX: a failed nudge SEND still counted toward the escalation
  cap.** `attempts` incremented unconditionally before `sendRepublishNudge`
  even ran — a transport-level failure (data channel not yet open, e.g.
  right after a fresh join) is not evidence the track is actually stuck,
  but could still burn through the cap and trigger an unwarranted full
  reconnect. `sendRepublishNudge` now returns whether the transport
  actually accepted the message; only a confirmed-sent attempt counts.
  Still re-arms on failure either way, so a transient hiccup gets retried
  next tick instead of going quiet.
- **Should-consider, applied: the escalation cap's trackID-stability
  assumption is real but not independently re-verified in this round.**
  The cap only engages if the SAME `track.ID()` keeps reappearing across
  republish cycles — if `republishOneTrack`'s unpublish+publish somehow
  produced a fresh id each time, the cap would silently never trip. Per
  earlier investigation (`ardiustech/watercooler`'s `findLocalTrackById` doc
  comment, cross-checked against this fork's own `getPublishedTrackBySdpCid`
  lookup and the client SDK's `AddTrackRequest{cid: track.mediaStreamTrack.id}`
  construction), the id IS the client's own `cid`, which stays stable across
  a republish because `republishOneTrack` reuses the SAME `LocalTrack`
  instance rather than constructing a new one — so this should already hold.
  Flagged here rather than silently assumed: this specific claim has NOT
  been independently re-verified end-to-end in this round (the same
  `*webrtc.TrackRemote`-construction limitation applies), so treat it as
  well-evidenced, not proven, until a live multi-nudge repro confirms it.
- Also applied two nice-to-haves: `stuckPublishNudges` entries are now
  explicitly deleted (not just timer-nulled) on the resolved-early path, and
  the `INVESTIGATION_LOG.md` citation above now names the actual branch it
  lives on (it's diagnostic-only, never merged to `master` or this PR).

**Round 3 fixes (full-panel re-review after round 2's fixes landed — verdict
came back "Reconsider" again; 1 of 2 must-fixes addressed, see "Not yet
done" below for the other):**
- **MUST-FIX: a persistently-failing send could loop forever without ever
  escalating.** Round 2's fix made `attempts` count only CONFIRMED-sent
  nudges (correctly, to stop a transient failure from burning cap budget) —
  but combined with the same round's unconditional re-arm, a participant
  whose data channel NEVER opens (a persistent, not transient, failure)
  would never advance `attempts`, and `onStuckPublishNudgeFired` would keep
  firing every `stuckPublishNudgeGracePeriod` indefinitely, never reaching
  `IssueFullReconnect`. Fixed with a second, independent counter — `checks`
  — that increments on every still-stuck firing regardless of send outcome;
  escalation now trips on `attempts >= stuckPublishNudgeMaxAttempts OR
  checks >= stuckPublishNudgeMaxChecks` (8, deliberately looser than
  `attempts`' 3, since this is the "give up on sending at all" backstop, not
  the normal case). Explicitly did NOT revert to counting a failed send as
  an attempt — that's the round-2 bug again. The decision itself is now a
  pure function (`shouldEscalateStuckPublish`), directly unit-tested, since
  the surrounding "still stuck" branch can't be driven end-to-end here (see
  "Files changed" below).

**Files changed:** `pkg/rtc/participant.go` (~280 lines net across both
rounds — the pure functions `isTrackStillPending`/`buildRepublishNudgePacket`,
the scheduling/send/dedup/cap/escalate/re-arm glue, the locked/unlocked
clear split, and the relay-path hard-drop),
`pkg/rtc/participant_republish_nudge_test.go` (covers the pure functions
directly, `onStuckPublishNudgeFired`'s resolved-without-sending/re-arming
cases, dedup/re-arm/clear/clear-all behavior via direct state manipulation
— no real timer waits — a goroutine+timeout deadlock regression test, and a
JSON-validity regression test for the control-byte fix; a real
`*webrtc.TrackRemote` still can't be constructed in a unit test at all,
since every field is private with no exported constructor, so the "still
stuck, N attempts, then escalates" full integration path remains covered
only by the pure `isTrackStillPending` decision plus manual/live
verification — this is the SAME limitation that let round 1's deadlock
ship in the first place, not a newly-accepted gap).

**Verified:**
- `go build ./pkg/rtc/...`, `go vet ./pkg/rtc/...`, `gofmt -l` all clean.
- New/updated tests: 11/11 pass, repeatably, no `time.Sleep`-based waits.
- Full `pkg/rtc` suite: passes clean on multiple runs. **Caveat:** this
  package has pre-existing flaky tests unrelated to this patch —
  `TestNegotiationFailed`, `TestFilteringCandidates`, and
  `TestFirstAnswerMissedDuringICERestart` in `transport_test.go` do real
  ICE/network-candidate work and fail intermittently (~1 in 3-4 runs)
  **on completely unmodified upstream code too** (confirmed by stashing this
  patch and re-running). Don't be alarmed if CI flakes on one of those three
  specifically; re-run before assuming a regression.

**Not yet done (tracked as follow-ups, not blocking this patch):**
- **Deploy prerequisite, not just a nice-to-have: `ReconnectOnPublicationError`
  defaults to `false` upstream** (`pkg/service/roommanager.go`: "default do
  not force full reconnect on a publication error"). Every round of this
  patch's escalation work (round 2's gating, round 3's `checks` counter) is
  reachable code that is a NO-OP in production unless an operator explicitly
  sets this flag in config. Without it, a persistently-stuck track still
  gets nudged on a bounded schedule but never actually recovers via
  `IssueFullReconnect` — the same "stuck until an unrelated renegotiation or
  the participant leaves" outcome the original incident had, just with more
  logging. This must be set (`rtc.reconnect_on_publication_error: true`, or
  the equivalent env var) as part of deploying this fork, not left at its
  upstream default.
- **Stale `pendingRemoteTracks` entries are a pre-existing upstream
  bookkeeping gap, not something this patch introduced or fixes.** Round 3's
  review flagged that an entry for a given `trackID` can in principle
  outlive the specific `*webrtc.TrackRemote` it was created for (e.g. across
  a republish that produces a fresh `TrackRemote` for the same `trackID`)
  and only gets purged in bulk when `handlePendingRemoteTracks` next runs,
  not by trackID as each one individually resolves. This predates every line
  this patch added — `isTrackStillPending`/`onStuckPublishNudgeFired` only
  READ `pendingRemoteTracks`, they don't own its lifecycle — so fixing it
  means touching `mediaTrackReceived`'s/`handlePendingRemoteTracks`'s core
  track-management logic well outside this patch's scope, with real risk of
  destabilizing paths this patch doesn't otherwise touch. Scoped out
  deliberately; flagging for whoever owns that code next.
- **"Escalation sawtooth" follow-up:** `onStuckPublishNudgeFired` calls
  `clearStuckPublishNudge` (which deletes the map entry) on cap-trip. If a
  LATER, unrelated renegotiation re-stuck the same trackID, it would start a
  brand-new episode at `attempts=0, checks=0` — i.e. a track that already
  escalated once could earn a full fresh budget rather than staying
  escalated. Suggested fix for whoever picks this up: add a terminal
  `escalated bool` to `stuckPublishNudgeState` that's checked (and skips
  re-arming/re-nudging) once set, rather than deleting the entry on
  cap-trip — do not implement this as a generation/episode counter with a
  fresh budget per renegotiation, which reintroduces the exact sawtooth this
  is meant to close.
- Production deploy — `infrastructure/livekit/terraform.tfvars`'s
  `livekit_image` still points at the stock `livekit/livekit-server` image.
  This fork is not live anywhere yet; deploying it is a deliberate follow-up
  decision, not a side effect of creating this fork. **This now matters
  more than it did at first open**: the app-side blind timer that used to
  provide SOME (confirmed net-negative, but non-zero) recovery coverage on
  its own has been deleted, so until this fork deploys, the original
  incident class has NO republish-nudge coverage at all.
- The equivalent nudge for the *other* branch that appends to
  `pendingRemoteTracks` (`ti == nil`, a related but distinct AddTrack-timing
  race) — scoped out of this first patch to stay narrowly targeted at the
  exact, confirmed, reproduced production incident.
- A live, multi-nudge, real-negotiation repro to independently re-confirm
  the trackID-stability assumption the escalation cap depends on (see
  above) — real evidence exists (client-side investigation, cited), but a
  fresh, this-mechanism-specific confirmation would close the gap fully.
- The recovery signal still rides the same WebRTC DataChannel/negotiation
  path this patch works around, rather than the always-up WS signaling
  channel LiveKit's own server-push messages (RoomUpdate,
  ConnectionQualityUpdate) already use. A stronger delivery-ack pattern
  (`PerformRpc`'s ack/timeout, elsewhere in this file) is also still a
  fire-and-forget-vs-confirmed-delivery gap. Both are real, scoped-out
  hardening opportunities, not correctness bugs in what shipped.
- Whether renegotiation-based recovery is even the right general mechanism
  is a real open question the review round raised: the incident's own 215
  failed attempts happened WHILE ordinary renegotiations were occurring, and
  the incident only actually cleared via a full client reconnect. The
  escalate-to-`IssueFullReconnect` behavior above is the concrete answer for
  the bounded-attempts case; whether to skip straight to it sooner is a
  tuning question for after this has real production data, not decided
  here.
- A delivery-confirmation/retry pattern for the nudge itself (it's currently
  fire-and-forget over `SendDataMessage(RELIABLE, ...)` — "reliable" bounds
  in-order delivery if the channel is up, not that it arrives at all) —
  `PerformRpc`'s ack/timeout pattern elsewhere in this file could be adapted
  if this needs to be more than best-effort.

## Upstream sync process

No automation yet. Manual process until this needs more rigor:

```bash
git fetch upstream
git checkout master
git merge upstream/master   # resolve conflicts (ARDIUSTECH_FORK.md never will)
git checkout feat/publish-mid-stuck-nudge
git rebase master
go test ./pkg/rtc/...
```

Re-run `go test ./pkg/rtc/... -count=1` a couple of times after any sync
given the flaky-test caveat above — don't rebase-and-ship on a single red run
without checking whether it's one of the three known-flaky tests.

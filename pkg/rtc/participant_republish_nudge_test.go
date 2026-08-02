// Copyright 2026 Ardius Tech, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package rtc

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/protocol/livekit"
)

// ardiustech fork: tests for the stuck-publish republish-nudge added in
// pkg/rtc/participant.go — see that file's doc comments for the full
// rationale (watercooler's docs/sfu-multiparty-triage-2026-07-31.md has the
// production incident this fixes).

func TestIsTrackStillPending(t *testing.T) {
	tests := []struct {
		name    string
		pending []string
		trackID string
		want    bool
	}{
		{"empty list", nil, "track-1", false},
		{"present, only entry", []string{"track-1"}, "track-1", true},
		{"present, among several", []string{"track-0", "track-1", "track-2"}, "track-1", true},
		{"absent, non-empty list", []string{"track-0", "track-2"}, "track-1", false},
		{"resolved and removed", []string{}, "track-1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, isTrackStillPending(tt.pending, tt.trackID))
		})
	}
}

func TestBuildRepublishNudgePacket(t *testing.T) {
	identity := livekit.ParticipantIdentity("scott-identity")
	trackID := `TR_weird"quote`

	dp := buildRepublishNudgePacket(identity, trackID)

	require.Equal(t, string(identity), dp.ParticipantIdentity)
	user, ok := dp.Value.(*livekit.DataPacket_User)
	require.True(t, ok, "expected DataPacket_User variant, not a new oneof member")
	require.NotNil(t, user.User.Topic)
	require.Equal(t, republishNudgeTopic, *user.User.Topic)
	require.JSONEq(t, `{"reason":"stuck_mid","trackId":"TR_weird\"quote"}`, string(user.User.Payload))

	// Round-trips through actual protobuf marshal/unmarshal, matching what
	// sendRepublishNudge really does — catches anything the struct-literal
	// assertions above wouldn't (e.g. a field with no wire tag).
	raw, err := proto.Marshal(dp)
	require.NoError(t, err)
	roundTripped := &livekit.DataPacket{}
	require.NoError(t, proto.Unmarshal(raw, roundTripped))
	rtUser, ok := roundTripped.Value.(*livekit.DataPacket_User)
	require.True(t, ok)
	require.Equal(t, republishNudgeTopic, *rtUser.User.Topic)
}

// TestBuildRepublishNudgePacket_ValidJSONForControlByteTrackID pins the
// review finding (2026-08-01) that the ORIGINAL fmt.Sprintf(`{"reason":
// "stuck_mid","trackId":%q}`, trackID) implementation could emit invalid
// JSON: Go's %q escapes some control bytes as `\xNN`, which is not valid
// JSON (`\u00NN` is required), and trackID originates from client-controlled
// SDP/MediaStreamTrack id, not a purely server-controlled value. The
// existing TestBuildRepublishNudgePacket above only covers an embedded `"`,
// which %q happens to escape identically to JSON — masking this class of
// bug. Switching to encoding/json.Marshal (this commit) fixes it; this test
// would have failed against the old %q-based implementation.
func TestBuildRepublishNudgePacket_ValidJSONForControlByteTrackID(t *testing.T) {
	trackID := "TR_weird\x01control\x1fbytes"

	dp := buildRepublishNudgePacket(livekit.ParticipantIdentity("scott-identity"), trackID)

	user, ok := dp.Value.(*livekit.DataPacket_User)
	require.True(t, ok)
	require.True(t, json.Valid(user.User.Payload), "payload must be valid JSON even for a trackID with raw control bytes: %s", user.User.Payload)

	var decoded republishNudgePayload
	require.NoError(t, json.Unmarshal(user.User.Payload, &decoded))
	require.Equal(t, trackID, decoded.TrackID)
}

// TestOnStuckPublishNudgeFired_ResolvedClearsStateWithoutSending exercises
// onStuckPublishNudgeFired directly (review finding, 2026-08-01: the
// PREVIOUS version of this test — TestScheduleStuckPublishNudge_ResolvedDoesNotPanic
// — went through the real timer via a 1s+ time.Sleep and only asserted
// NotPanics, which would still pass even if the resolved case incorrectly
// sent a nudge or left stale state behind). newParticipantForTest's
// pendingRemoteTracks starts empty, matching a track that was never stuck
// (or already flushed by an unrelated renegotiation) — isTrackStillPending
// returns false, so this must be a pure no-op: no nudge sent, no state left
// behind. Asserts the entry is REMOVED entirely (round-2 fix, 2026-08-01),
// not just left with attempts==0 — a leaked zero-value entry is still a
// leak, and this is also belt-and-suspenders against
// mediaTrackReceived's own clearStuckPublishNudgeLocked call not having run
// for some reason.
func TestOnStuckPublishNudgeFired_ResolvedClearsStateWithoutSending(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_will_resolve"
	p.stuckPublishNudges[trackID] = &stuckPublishNudgeState{}

	require.NotPanics(t, func() {
		p.onStuckPublishNudgeFired(trackID)
	})
	_, exists := p.stuckPublishNudges[trackID]
	require.False(t, exists, "must remove the entry entirely for a track that already resolved, not just zero its attempts")
}

// TestScheduleStuckPublishNudge_DedupsWhileATimerIsAlreadyInFlight pins the
// review finding (2026-08-01) that scheduleStuckPublishNudge previously
// armed a BRAND NEW time.AfterFunc on every call with no dedup — since
// mediaTrackReceived calls this on EVERY retry of a still-stuck track, a
// track retried many times (215, in the production incident this fixes)
// could accumulate many independent, uncoordinated timers. A second call
// while the first's timer is still pending must be a no-op.
func TestScheduleStuckPublishNudge_DedupsWhileATimerIsAlreadyInFlight(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_stuck"

	p.scheduleStuckPublishNudge(trackID)
	require.Len(t, p.stuckPublishNudges, 1)
	firstTimer := p.stuckPublishNudges[trackID].timer
	require.NotNil(t, firstTimer)

	p.scheduleStuckPublishNudge(trackID)

	require.Len(t, p.stuckPublishNudges, 1, "must not create a second entry")
	require.Same(t, firstTimer, p.stuckPublishNudges[trackID].timer, "must not replace the already-in-flight timer with a new one")

	p.clearStuckPublishNudge(trackID) // don't leak a real timer past the test
}

// TestScheduleStuckPublishNudge_ReArmsOnceThePreviousTimerHasFired confirms
// the dedup above is scoped to "already in flight," not "ever scheduled
// before" — once a timer fires (state.timer reset to nil), a later call for
// the SAME still-stuck track must be able to arm a fresh one.
func TestScheduleStuckPublishNudge_ReArmsOnceThePreviousTimerHasFired(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_stuck"

	p.scheduleStuckPublishNudge(trackID)
	p.stuckPublishNudges[trackID].timer = nil // simulate the timer having already fired

	p.scheduleStuckPublishNudge(trackID)

	require.NotNil(t, p.stuckPublishNudges[trackID].timer, "must arm a fresh timer once the previous one is no longer in flight")
	p.clearStuckPublishNudge(trackID)
}

// TestClearStuckPublishNudge_RemovesStateAndStopsTheTimer covers both the
// mediaTrackReceived success-path caller (a track resolving mid-episode)
// and general cleanup — a cleared track must accept a completely fresh
// schedule/attempts cycle later (e.g. a NEW stuck episode reusing the same
// trackID after a full unpublish/republish).
func TestClearStuckPublishNudge_RemovesStateAndStopsTheTimer(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_stuck"
	p.scheduleStuckPublishNudge(trackID)
	require.Len(t, p.stuckPublishNudges, 1)

	p.clearStuckPublishNudge(trackID)

	require.Empty(t, p.stuckPublishNudges)
	// A fresh schedule after clearing must behave like a first-ever call —
	// not blocked by stale dedup state.
	p.scheduleStuckPublishNudge(trackID)
	require.Len(t, p.stuckPublishNudges, 1)
	require.Equal(t, 0, p.stuckPublishNudges[trackID].attempts)
	p.clearStuckPublishNudge(trackID)
}

// TestClearStuckPublishNudgeLocked_DoesNotReacquireTheLock is a regression
// test for a real, guaranteed self-deadlock (review finding, 2026-08-01,
// caught before this ever reached production): mediaTrackReceived's
// mid-resolved path called the LOCKING clearStuckPublishNudge while it
// ALREADY held pendingTracksLock (acquired at function entry, only unlocked
// in the mid=="" branch) — utils.RWMutex is not reentrant, so this hung
// EVERY successful (non-stuck) track publish, wedging every other operation
// on that participant needing the same lock (AddTrack, handlePendingRemoteTracks,
// Close). The fix splits it into a locking wrapper (clearStuckPublishNudge,
// for callers that don't hold the lock) and clearStuckPublishNudgeLocked
// (for callers already inside the critical section). This test simulates
// mediaTrackReceived's exact pattern — Lock(), then clear — and would hang
// (caught via a timeout, not an actual indefinite block) if
// clearStuckPublishNudgeLocked ever regressed to re-acquiring the lock.
func TestClearStuckPublishNudgeLocked_DoesNotReacquireTheLock(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_stuck"
	p.scheduleStuckPublishNudge(trackID)

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.pendingTracksLock.Lock() // exactly mediaTrackReceived's own entry lock
		defer p.pendingTracksLock.Unlock()
		p.clearStuckPublishNudgeLocked(trackID)
	}()

	select {
	case <-done:
		require.Empty(t, p.stuckPublishNudges)
	case <-time.After(2 * time.Second):
		t.Fatal(
			"clearStuckPublishNudgeLocked deadlocked while pendingTracksLock was already held — " +
				"this is the exact self-deadlock bug the locked/unlocked split exists to prevent",
		)
	}
}

// TestOnStuckPublishNudgeFired_ReArmsAfterSendingWithoutARealPendingTrack
// covers as much of the re-arm fix (review finding, 2026-08-01 — must-fix:
// escalation was UNREACHABLE because nothing re-scheduled after a nudge) as
// is constructible without a real *webrtc.TrackRemote (see
// isTrackStillPending's doc comment — pendingRemoteTracks entries can't be
// fabricated in this test binary, so the "still stuck" branch itself can't
// be driven end-to-end here). What IS directly testable: calling
// scheduleStuckPublishNudge → firing it manually via onStuckPublishNudgeFired
// with an EMPTY pendingRemoteTracks (the "resolved" branch) must NOT re-arm —
// re-arming is conditional on stillPending, and this pins that it stays
// conditional rather than firing unconditionally.
func TestOnStuckPublishNudgeFired_ResolvedDoesNotReArm(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	const trackID = "TR_will_resolve"
	p.stuckPublishNudges[trackID] = &stuckPublishNudgeState{}

	p.onStuckPublishNudgeFired(trackID)

	// The entry is removed entirely on resolve (see
	// TestOnStuckPublishNudgeFired_ResolvedClearsStateWithoutSending) — which
	// itself is what prevents a re-arm: scheduleStuckPublishNudge's dedup
	// check only reads state that no longer exists here.
	state, exists := p.stuckPublishNudges[trackID]
	require.False(t, exists, "must not re-arm for a track that already resolved")
	require.Nil(t, state)
}

// TestClearAllStuckPublishNudges_StopsEveryTimerAcrossMultipleTracks covers
// the Close()-time cleanup (review finding, 2026-08-01: this class of timer
// previously had NO Close()-time cleanup at all, unlike this file's existing
// migrationTimer/disconnectTimer pattern) — a participant with SEVERAL
// simultaneously-stuck tracks (e.g. camera+mic+screen, per the PR's own
// self-feeding-loop discussion) must have every one of them cleared, not
// just the first.
func TestClearAllStuckPublishNudges_StopsEveryTimerAcrossMultipleTracks(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	p.scheduleStuckPublishNudge("TR_cam")
	p.scheduleStuckPublishNudge("TR_mic")
	p.scheduleStuckPublishNudge("TR_screen")
	require.Len(t, p.stuckPublishNudges, 3)

	p.clearAllStuckPublishNudges()

	require.Empty(t, p.stuckPublishNudges)
}

// TestShouldEscalateStuckPublish pins the review finding (2026-08-01 —
// must-fix, round 3): attempts alone can never trip for a participant whose
// sendRepublishNudge call NEVER succeeds (e.g. a data channel that never
// opens) — attempts only increments on a CONFIRMED send (see
// onStuckPublishNudgeFired's "peek before incrementing" fix from round 2),
// so a persistently-failing send combined with the unconditional re-arm
// would otherwise loop forever, never reaching IssueFullReconnect. checks
// increments on every firing regardless of send outcome and independently
// bounds this case. Explicitly does NOT revert to counting a failed send as
// an attempt — that was the round-2 bug (a transient failure right after
// join burning real retry budget).
func TestShouldEscalateStuckPublish(t *testing.T) {
	tests := []struct {
		name     string
		attempts int
		checks   int
		want     bool
	}{
		{"neither counter near its cap", 0, 0, false},
		{"attempts reaches its cap, checks still low", stuckPublishNudgeMaxAttempts, 1, true},
		{"attempts below cap, checks reaches its cap (persistent send failure)", 0, stuckPublishNudgeMaxChecks, true},
		{"checks below cap and attempts below cap", stuckPublishNudgeMaxAttempts - 1, stuckPublishNudgeMaxChecks - 1, false},
		{"checks always outpaces attempts on a persistent failure, so checks alone must trip it", 0, stuckPublishNudgeMaxChecks + 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, shouldEscalateStuckPublish(tt.attempts, tt.checks))
		})
	}
}

// TestSendRepublishNudge_DoesNotPanicWithoutLiveTransport covers the "still
// stuck, nudge actually attempted" path directly (rather than through
// scheduleStuckPublishNudge's timer, which would need a real, populated
// pendingRemoteTracks entry — not constructible in a unit test; see above).
// newParticipantForTest builds a real TransportManager with no live
// PeerConnection/ICE, so the actual SendDataMessage call is expected to
// error here — sendRepublishNudge already handles that by logging and
// returning, not panicking, which is exactly what this asserts.
func TestSendRepublishNudge_DoesNotPanicWithoutLiveTransport(t *testing.T) {
	p := newParticipantForTest("scott-identity")
	require.NotPanics(t, func() {
		p.sendRepublishNudge("TR_stuck")
	})
}

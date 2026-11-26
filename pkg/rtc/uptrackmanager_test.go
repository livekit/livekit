// Copyright 2023 LiveKit, Inc.
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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
)

var defaultUptrackManagerParams = UpTrackManagerParams{
	Logger:           logger.GetLogger(),
	VersionGenerator: utils.NewDefaultTimedVersionGenerator(),
}

func TestUpdateSubscriptionPermission(t *testing.T) {
	t.Run("updates subscription permission", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		trv := &typesfakes.FakeMediaTrack{}
		trv.IDReturns("video")
		um.AddPublishedTrack(trv)

		// no restrictive subscription permission
		subscriptionPermission := &livekit.SubscriptionPermission{
			AllParticipants: true,
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.Nil(t, um.subscriberPermissions)

		// nobody is allowed to subscribe
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.NotNil(t, um.subscriberPermissions)
		require.Equal(t, 0, len(um.subscriberPermissions))

		lp1 := &typesfakes.FakeLocalParticipant{}
		lp1.IdentityReturns("p1")
		lp2 := &typesfakes.FakeLocalParticipant{}
		lp2.IdentityReturns("p2")

		sidResolver := func(sid livekit.ParticipantID) types.LocalParticipant {
			if sid == "p1" {
				return lp1
			}

			if sid == "p2" {
				return lp2
			}

			return nil
		}

		// allow all tracks for participants
		perms1 := &livekit.TrackPermission{
			ParticipantSid: "p1",
			AllTracks:      true,
		}
		perms2 := &livekit.TrackPermission{
			ParticipantSid: "p2",
			AllTracks:      true,
		}
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				perms1,
				perms2,
			},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), sidResolver)
		require.Equal(t, 2, len(um.subscriberPermissions))
		require.EqualValues(t, perms1, um.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriberPermissions["p2"])

		// allow all tracks for some and restrictive for others
		perms1 = &livekit.TrackPermission{
			ParticipantIdentity: "p1",
			AllTracks:           true,
		}
		perms2 = &livekit.TrackPermission{
			ParticipantIdentity: "p2",
			TrackSids:           []string{"audio"},
		}
		perms3 := &livekit.TrackPermission{
			ParticipantIdentity: "p3",
			TrackSids:           []string{"video"},
		}
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				perms1,
				perms2,
				perms3,
			},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.Equal(t, 3, len(um.subscriberPermissions))
		require.EqualValues(t, perms1, um.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriberPermissions["p2"])
		require.EqualValues(t, perms3, um.subscriberPermissions["p3"])
	})

	t.Run("updates subscription permission using both", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		trv := &typesfakes.FakeMediaTrack{}
		trv.IDReturns("video")
		um.AddPublishedTrack(trv)

		lp1 := &typesfakes.FakeLocalParticipant{}
		lp1.IdentityReturns("p1")
		lp2 := &typesfakes.FakeLocalParticipant{}
		lp2.IdentityReturns("p2")

		sidResolver := func(sid livekit.ParticipantID) types.LocalParticipant {
			if sid == "p1" {
				return lp1
			}

			if sid == "p2" {
				return lp2
			}

			return nil
		}

		// allow all tracks for participants
		perms1 := &livekit.TrackPermission{
			ParticipantSid:      "p1",
			ParticipantIdentity: "p1",
			AllTracks:           true,
		}
		perms2 := &livekit.TrackPermission{
			ParticipantSid:      "p2",
			ParticipantIdentity: "p2",
			AllTracks:           true,
		}
		subscriptionPermission := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				perms1,
				perms2,
			},
		}
		err := um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), sidResolver)
		require.NoError(t, err)
		require.Equal(t, 2, len(um.subscriberPermissions))
		require.EqualValues(t, perms1, um.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriberPermissions["p2"])

		// mismatched identities should fail a permission update
		badSidResolver := func(sid livekit.ParticipantID) types.LocalParticipant {
			if sid == "p1" {
				return lp2
			}

			if sid == "p2" {
				return lp1
			}

			return nil
		}

		err = um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), badSidResolver)
		require.NoError(t, err)
		require.Equal(t, 2, len(um.subscriberPermissions))
		require.EqualValues(t, perms1, um.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriberPermissions["p2"])
	})

	t.Run("update versions", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		v0, v1, v2 := vg.Next(), vg.Next(), vg.Next()

		um.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v1, nil)
		require.Equal(t, v1.Load(), um.subscriptionPermissionVersion.Load(), "first update should be applied")

		um.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v2, nil)
		require.Equal(t, v2.Load(), um.subscriptionPermissionVersion.Load(), "ordered updates should be applied")

		um.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v0, nil)
		require.Equal(t, v2.Load(), um.subscriptionPermissionVersion.Load(), "out of order updates should be ignored")

		um.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, utils.TimedVersion(0), nil)
		require.True(t, um.subscriptionPermissionVersion.After(v2), "zero version in updates should use next local version")
	})

	t.Run("local updates advance past a remote clock", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		remoteVersion := utils.TimedVersionFromTime(time.Now().Add(time.Hour))
		allow := &livekit.SubscriptionPermission{AllParticipants: true}
		deny := &livekit.SubscriptionPermission{}

		require.NoError(t, um.UpdateSubscriptionPermission(allow, remoteVersion, nil))
		require.NoError(t, um.UpdateSubscriptionPermission(deny, 0, nil))
		_, localVersion := um.SubscriptionPermission()
		require.True(t, localVersion.After(remoteVersion))
		require.False(t, um.HasPermission("video", "subscriber"))

		// This update was sent before the local change, with the next remote tick.
		require.NoError(t, um.UpdateSubscriptionPermission(allow, remoteVersion.Next(), nil))
		require.False(t, um.HasPermission("video", "subscriber"))

		require.NoError(t, um.UpdateSubscriptionPermission(allow, 0, nil))
		_, nextLocalVersion := um.SubscriptionPermission()
		require.True(t, nextLocalVersion.After(localVersion))
		require.True(t, um.HasPermission("video", "subscriber"))
	})

	t.Run("invalid update leaves version, proto, and subscriber permissions unchanged", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		v1, v2, v3 := vg.Next(), vg.Next(), vg.Next()
		permV1 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantIdentity: "p1",
					AllTracks:           true,
				},
			},
		}
		require.NoError(t, um.UpdateSubscriptionPermission(permV1, v1, nil))
		require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())

		invalidPerms := []*livekit.SubscriptionPermission{
			{
				TrackPermissions: []*livekit.TrackPermission{
					{ParticipantIdentity: "p2", AllTracks: true},
					{},
				},
			},
			{
				TrackPermissions: []*livekit.TrackPermission{nil},
			},
		}
		for _, invalid := range invalidPerms {
			err := um.UpdateSubscriptionPermission(invalid, v3, nil)
			require.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)

			err = um.UpdateSubscriptionPermission(invalid, 0, nil)
			require.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)

			gotPerm, gotVer := um.SubscriptionPermission()
			require.Equal(t, v1.Load(), gotVer.Load())
			require.Equal(t, permV1, gotPerm)
			require.True(t, um.HasPermission("audio", "p1"))
			require.False(t, um.HasPermission("audio", "p2"))
			require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
		}

		// Because v3 failed, v2 (between v1 and v3) must still be accepted cleanly.
		permV2 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantIdentity: "p2",
					AllTracks:           true,
				},
			},
		}
		require.NoError(t, um.UpdateSubscriptionPermission(permV2, v2, nil))
		gotPerm, gotVer := um.SubscriptionPermission()
		require.Equal(t, v2.Load(), gotVer.Load())
		require.Equal(t, permV2, gotPerm)
		require.False(t, um.HasPermission("audio", "p1"))
		require.True(t, um.HasPermission("audio", "p2"))
		require.Equal(t, 2, tra.RevokeDisallowedSubscribersCallCount())
	})

	t.Run("nil permission updates version without clearing map or revoking", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		v1, v2 := vg.Next(), vg.Next()
		permV1 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantIdentity: "p1",
					AllTracks:           true,
				},
			},
		}
		require.NoError(t, um.UpdateSubscriptionPermission(permV1, v1, nil))
		require.NoError(t, um.UpdateSubscriptionPermission(nil, v2, nil))

		gotPerm, gotVer := um.SubscriptionPermission()
		require.Nil(t, gotPerm)
		require.Equal(t, v2.Load(), gotVer.Load())
		require.True(t, um.HasPermission("audio", "p1"))
		require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
	})

	t.Run("stale concurrent update does not overwrite newer update while resolving SIDs", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		lp2 := &typesfakes.FakeLocalParticipant{}
		lp2.IdentityReturns("p2")
		lp3 := &typesfakes.FakeLocalParticipant{}
		lp3.IdentityReturns("p3")

		v2, v3 := vg.Next(), vg.Next()
		permV2 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantSid: "PA_2",
					AllTracks:      true,
				},
			},
		}
		permV3 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantSid: "PA_3",
					AllTracks:      true,
				},
			},
		}

		enteredV2Resolver := make(chan struct{})
		releaseV2Resolver := make(chan struct{})
		releaseV2 := sync.OnceFunc(func() {
			close(releaseV2Resolver)
		})
		t.Cleanup(releaseV2)

		resolver := func(sid livekit.ParticipantID) types.LocalParticipant {
			switch sid {
			case "PA_2":
				// Reading permissions from inside resolver must not deadlock on u.lock.
				_ = um.HasPermission("audio", "p1")
				_, _ = um.SubscriptionPermission()
				close(enteredV2Resolver)
				<-releaseV2Resolver
				return lp2
			case "PA_3":
				return lp3
			default:
				return nil
			}
		}

		v2ErrCh := make(chan error, 1)
		go func() {
			v2ErrCh <- um.UpdateSubscriptionPermission(permV2, v2, resolver)
		}()

		select {
		case <-enteredV2Resolver:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for resolver entry")
		}

		// Apply newer v3 while v2 is suspended inside resolverBySid.
		require.NoError(t, um.UpdateSubscriptionPermission(permV3, v3, resolver))
		releaseV2()
		select {
		case err := <-v2ErrCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for UpdateSubscriptionPermission completion")
		}

		gotPerm, gotVer := um.SubscriptionPermission()
		require.Equal(t, v3.Load(), gotVer.Load())
		require.Equal(t, permV3, gotPerm)
		require.False(t, um.HasPermission("audio", "p2"))
		require.True(t, um.HasPermission("audio", "p3"))
		require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
		require.Equal(t, []livekit.ParticipantIdentity{"p3"}, tra.RevokeDisallowedSubscribersArgsForCall(0))
	})

	t.Run("concurrent updates and permission reads", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		lp1 := &typesfakes.FakeLocalParticipant{}
		lp1.IdentityReturns("p1")
		lp2 := &typesfakes.FakeLocalParticipant{}
		lp2.IdentityReturns("p2")
		resolver := func(sid livekit.ParticipantID) types.LocalParticipant {
			switch sid {
			case "PA_1":
				return lp1
			case "PA_2":
				return lp2
			default:
				return nil
			}
		}

		perm1 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{{ParticipantSid: "PA_1", AllTracks: true}},
		}
		perm2 := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{{ParticipantSid: "PA_2", AllTracks: true}},
		}
		invalidPerm := &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{{}},
		}

		versions := make([]utils.TimedVersion, 40)
		for i := range versions {
			versions[i] = vg.Next()
		}

		var wg sync.WaitGroup
		for g := range 4 {
			wg.Add(1)
			go func(worker int) {
				defer wg.Done()
				for i := range 25 {
					idx := (worker*7 + i) % len(versions)
					op := (worker + i) % 3
					// Ensure the highest version always applies a valid permission so final state is valid and deterministic.
					if idx == len(versions)-1 && op == 2 {
						op = 0
					}
					switch op {
					case 0:
						assert.NoError(t, um.UpdateSubscriptionPermission(perm1, versions[idx], resolver))
					case 1:
						assert.NoError(t, um.UpdateSubscriptionPermission(perm2, versions[idx], resolver))
					default:
						err := um.UpdateSubscriptionPermission(invalidPerm, versions[idx], resolver)
						if err != nil {
							assert.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)
						}
					}
					_ = um.HasPermission("audio", "p1")
					_ = um.GetAllowedSubscribers("audio")
					_, _ = um.SubscriptionPermission()
				}
			}(g)
		}
		wg.Wait()

		finalPerm, finalVer := um.SubscriptionPermission()
		require.NotNil(t, finalPerm)
		require.Equal(t, versions[len(versions)-1].Load(), finalVer.Load())
		if finalPerm == perm1 {
			require.True(t, um.HasPermission("audio", "p1"))
			require.False(t, um.HasPermission("audio", "p2"))
		} else {
			require.Same(t, perm2, finalPerm)
			require.False(t, um.HasPermission("audio", "p1"))
			require.True(t, um.HasPermission("audio", "p2"))
		}
	})
}

func TestSubscriptionPermission(t *testing.T) {
	t.Run("checks subscription permission", func(t *testing.T) {
		um := NewUpTrackManager(defaultUptrackManagerParams)
		vg := utils.NewDefaultTimedVersionGenerator()

		tra := &typesfakes.FakeMediaTrack{}
		tra.IDReturns("audio")
		um.AddPublishedTrack(tra)

		trv := &typesfakes.FakeMediaTrack{}
		trv.IDReturns("video")
		um.AddPublishedTrack(trv)

		// no restrictive permission
		subscriptionPermission := &livekit.SubscriptionPermission{
			AllParticipants: true,
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, um.hasPermissionLocked("audio", "p1"))
		require.True(t, um.hasPermissionLocked("audio", "p2"))

		// nobody is allowed to subscribe
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.False(t, um.hasPermissionLocked("audio", "p1"))
		require.False(t, um.hasPermissionLocked("audio", "p2"))

		// allow all tracks for participants
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantIdentity: "p1",
					AllTracks:           true,
				},
				{
					ParticipantIdentity: "p2",
					AllTracks:           true,
				},
			},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, um.hasPermissionLocked("audio", "p1"))
		require.True(t, um.hasPermissionLocked("video", "p1"))
		require.True(t, um.hasPermissionLocked("audio", "p2"))
		require.True(t, um.hasPermissionLocked("video", "p2"))

		// add a new track after permissions are set
		trs := &typesfakes.FakeMediaTrack{}
		trs.IDReturns("screen")
		um.AddPublishedTrack(trs)

		require.True(t, um.hasPermissionLocked("audio", "p1"))
		require.True(t, um.hasPermissionLocked("video", "p1"))
		require.True(t, um.hasPermissionLocked("screen", "p1"))
		require.True(t, um.hasPermissionLocked("audio", "p2"))
		require.True(t, um.hasPermissionLocked("video", "p2"))
		require.True(t, um.hasPermissionLocked("screen", "p2"))

		// allow all tracks for some and restrictive for others
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantIdentity: "p1",
					AllTracks:           true,
				},
				{
					ParticipantIdentity: "p2",
					TrackSids:           []string{"audio"},
				},
				{
					ParticipantIdentity: "p3",
					TrackSids:           []string{"video"},
				},
			},
		}
		um.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, um.hasPermissionLocked("audio", "p1"))
		require.True(t, um.hasPermissionLocked("video", "p1"))
		require.True(t, um.hasPermissionLocked("screen", "p1"))

		require.True(t, um.hasPermissionLocked("audio", "p2"))
		require.False(t, um.hasPermissionLocked("video", "p2"))
		require.False(t, um.hasPermissionLocked("screen", "p2"))

		require.False(t, um.hasPermissionLocked("audio", "p3"))
		require.True(t, um.hasPermissionLocked("video", "p3"))
		require.False(t, um.hasPermissionLocked("screen", "p3"))

		// add a new track after restrictive permissions are set
		trw := &typesfakes.FakeMediaTrack{}
		trw.IDReturns("watch")
		um.AddPublishedTrack(trw)

		require.True(t, um.hasPermissionLocked("audio", "p1"))
		require.True(t, um.hasPermissionLocked("video", "p1"))
		require.True(t, um.hasPermissionLocked("screen", "p1"))
		require.True(t, um.hasPermissionLocked("watch", "p1"))

		require.True(t, um.hasPermissionLocked("audio", "p2"))
		require.False(t, um.hasPermissionLocked("video", "p2"))
		require.False(t, um.hasPermissionLocked("screen", "p2"))
		require.False(t, um.hasPermissionLocked("watch", "p2"))

		require.False(t, um.hasPermissionLocked("audio", "p3"))
		require.True(t, um.hasPermissionLocked("video", "p3"))
		require.False(t, um.hasPermissionLocked("screen", "p3"))
		require.False(t, um.hasPermissionLocked("watch", "p3"))
	})
}

func TestUpTrackManagerPermissionTrackRegistration(t *testing.T) {
	vg := utils.NewDefaultTimedVersionGenerator()
	denyAll := &livekit.SubscriptionPermission{}

	um := NewUpTrackManager(defaultUptrackManagerParams)
	tra := &typesfakes.FakeMediaTrack{}
	tra.IDReturns("TR_audio")
	trv := &typesfakes.FakeMediaTrack{}
	trv.IDReturns("TR_video")
	um.AddPublishedTrack(tra)
	um.AddPublishedTrack(trv)

	require.NoError(t, um.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, trv.RevokeDisallowedSubscribersCallCount())

	// OnClose callback unregisters tra; RemovePublishedTrack unregisters trv.
	tra.AddOnCloseArgsForCall(0)(false)
	um.RemovePublishedTrack(trv, false)
	require.NoError(t, um.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, trv.RevokeDisallowedSubscribersCallCount())

	// Close unregisters any remaining published tracks.
	trs := &typesfakes.FakeMediaTrack{}
	trs.IDReturns("TR_screen")
	um.AddPublishedTrack(trs)
	um.Close(false)
	require.NoError(t, um.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Zero(t, trs.RevokeDisallowedSubscribersCallCount())

	// Removing an older track pointer with the same ID must not unregister the active track.
	trsReplacement := &typesfakes.FakeMediaTrack{}
	trsReplacement.IDReturns("TR_screen")
	um.addTrack(nil)
	um.removeTrack(nil)
	um.addTrack(trsReplacement)
	um.removeTrack(trs)
	require.NoError(t, um.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Zero(t, trs.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, trsReplacement.RevokeDisallowedSubscribersCallCount())
}

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

func newSubscriptionPermissionTestHarness() (*SubscriptionPermissionManager, *UpTrackManager) {
	um := NewUpTrackManager(UpTrackManagerParams{
		Logger: logger.GetLogger(),
	})
	udm, _, _ := newUpDataTrackManagerForTest()
	pm := NewSubscriptionPermissionManager(SubscriptionPermissionManagerParams{
		Logger:           logger.GetLogger(),
		VersionGenerator: utils.NewDefaultTimedVersionGenerator(),
		GetMediaTracks:   um.GetPublishedTracks,
		GetDataTracks:    udm.GetPublishedDataTracks,
	})
	return pm, um
}

func TestUpdateSubscriptionPermission(t *testing.T) {
	t.Run("updates subscription permission", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.Nil(t, pm.subscriberPermissions)

		// nobody is allowed to subscribe
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.NotNil(t, pm.subscriberPermissions)
		require.Equal(t, 0, len(pm.subscriberPermissions))

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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), sidResolver)
		require.Equal(t, 2, len(pm.subscriberPermissions))
		require.EqualValues(t, perms1, pm.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, pm.subscriberPermissions["p2"])

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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.Equal(t, 3, len(pm.subscriberPermissions))
		require.EqualValues(t, perms1, pm.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, pm.subscriberPermissions["p2"])
		require.EqualValues(t, perms3, pm.subscriberPermissions["p3"])
	})

	t.Run("updates subscription permission using both", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
		err := pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), sidResolver)
		require.NoError(t, err)
		require.Equal(t, 2, len(pm.subscriberPermissions))
		require.EqualValues(t, perms1, pm.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, pm.subscriberPermissions["p2"])

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

		err = pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), badSidResolver)
		require.NoError(t, err)
		require.Equal(t, 2, len(pm.subscriberPermissions))
		require.EqualValues(t, perms1, pm.subscriberPermissions["p1"])
		require.EqualValues(t, perms2, pm.subscriberPermissions["p2"])
	})

	t.Run("update versions", func(t *testing.T) {
		pm, _ := newSubscriptionPermissionTestHarness()
		vg := utils.NewDefaultTimedVersionGenerator()

		v0, v1, v2 := vg.Next(), vg.Next(), vg.Next()

		pm.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v1, nil)
		require.Equal(t, v1.Load(), pm.subscriptionPermissionVersion.Load(), "first update should be applied")

		pm.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v2, nil)
		require.Equal(t, v2.Load(), pm.subscriptionPermissionVersion.Load(), "ordered updates should be applied")

		pm.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, v0, nil)
		require.Equal(t, v2.Load(), pm.subscriptionPermissionVersion.Load(), "out of order updates should be ignored")

		pm.UpdateSubscriptionPermission(&livekit.SubscriptionPermission{}, utils.TimedVersion(0), nil)
		require.True(t, pm.subscriptionPermissionVersion.After(v2), "zero version in updates should use next local version")
	})

	t.Run("local updates advance past a remote clock", func(t *testing.T) {
		pm, _ := newSubscriptionPermissionTestHarness()
		remoteVersion := utils.TimedVersionFromTime(time.Now().Add(time.Hour))
		allow := &livekit.SubscriptionPermission{AllParticipants: true}
		deny := &livekit.SubscriptionPermission{}

		require.NoError(t, pm.UpdateSubscriptionPermission(allow, remoteVersion, nil))
		require.NoError(t, pm.UpdateSubscriptionPermission(deny, 0, nil))
		_, localVersion := pm.SubscriptionPermission()
		require.True(t, localVersion.After(remoteVersion))
		require.False(t, pm.HasPermission("video", "subscriber"))

		// This update was sent before the local change, with the next remote tick.
		require.NoError(t, pm.UpdateSubscriptionPermission(allow, remoteVersion.Next(), nil))
		require.False(t, pm.HasPermission("video", "subscriber"))

		require.NoError(t, pm.UpdateSubscriptionPermission(allow, 0, nil))
		_, nextLocalVersion := pm.SubscriptionPermission()
		require.True(t, nextLocalVersion.After(localVersion))
		require.True(t, pm.HasPermission("video", "subscriber"))
	})

	t.Run("invalid update leaves version, proto, and subscriber permissions unchanged", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
		require.NoError(t, pm.UpdateSubscriptionPermission(permV1, v1, nil))
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
			err := pm.UpdateSubscriptionPermission(invalid, v3, nil)
			require.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)

			err = pm.UpdateSubscriptionPermission(invalid, 0, nil)
			require.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)

			gotPerm, gotVer := pm.SubscriptionPermission()
			require.Equal(t, v1.Load(), gotVer.Load())
			require.Equal(t, permV1, gotPerm)
			require.True(t, pm.HasPermission("audio", "p1"))
			require.False(t, pm.HasPermission("audio", "p2"))
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
		require.NoError(t, pm.UpdateSubscriptionPermission(permV2, v2, nil))
		gotPerm, gotVer := pm.SubscriptionPermission()
		require.Equal(t, v2.Load(), gotVer.Load())
		require.Equal(t, permV2, gotPerm)
		require.False(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("audio", "p2"))
		require.Equal(t, 2, tra.RevokeDisallowedSubscribersCallCount())
	})

	t.Run("stale concurrent update does not overwrite newer update while resolving SIDs", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
				// Reading permissions from inside resolver must not deadlock on m.lock.
				_ = pm.HasPermission("audio", "p1")
				_, _ = pm.SubscriptionPermission()
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
			v2ErrCh <- pm.UpdateSubscriptionPermission(permV2, v2, resolver)
		}()

		select {
		case <-enteredV2Resolver:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for resolver entry")
		}

		// Apply newer v3 while v2 is suspended inside resolverBySid.
		require.NoError(t, pm.UpdateSubscriptionPermission(permV3, v3, resolver))
		releaseV2()
		select {
		case err := <-v2ErrCh:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "timed out waiting for UpdateSubscriptionPermission completion")
		}

		gotPerm, gotVer := pm.SubscriptionPermission()
		require.Equal(t, v3.Load(), gotVer.Load())
		require.Equal(t, permV3, gotPerm)
		require.False(t, pm.HasPermission("audio", "p2"))
		require.True(t, pm.HasPermission("audio", "p3"))
		require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
		require.Equal(t, []livekit.ParticipantIdentity{"p3"}, tra.RevokeDisallowedSubscribersArgsForCall(0))
	})

	t.Run("concurrent updates and permission reads", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
						assert.NoError(t, pm.UpdateSubscriptionPermission(perm1, versions[idx], resolver))
					case 1:
						assert.NoError(t, pm.UpdateSubscriptionPermission(perm2, versions[idx], resolver))
					default:
						err := pm.UpdateSubscriptionPermission(invalidPerm, versions[idx], resolver)
						if err != nil {
							assert.ErrorIs(t, err, ErrSubscriptionPermissionNeedsId)
						}
					}
					_ = pm.HasPermission("audio", "p1")
					_ = pm.GetAllowedSubscribers("audio")
					_, _ = pm.SubscriptionPermission()
				}
			}(g)
		}
		wg.Wait()

		finalPerm, finalVer := pm.SubscriptionPermission()
		require.NotNil(t, finalPerm)
		require.Equal(t, versions[len(versions)-1].Load(), finalVer.Load())
		if finalPerm == perm1 {
			require.True(t, pm.HasPermission("audio", "p1"))
			require.False(t, pm.HasPermission("audio", "p2"))
		} else {
			require.Same(t, perm2, finalPerm)
			require.False(t, pm.HasPermission("audio", "p1"))
			require.True(t, pm.HasPermission("audio", "p2"))
		}
	})
}

func TestSubscriptionPermission(t *testing.T) {
	t.Run("checks subscription permission", func(t *testing.T) {
		pm, um := newSubscriptionPermissionTestHarness()
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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("audio", "p2"))

		// nobody is allowed to subscribe
		subscriptionPermission = &livekit.SubscriptionPermission{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.False(t, pm.HasPermission("audio", "p1"))
		require.False(t, pm.HasPermission("audio", "p2"))

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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("video", "p1"))
		require.True(t, pm.HasPermission("audio", "p2"))
		require.True(t, pm.HasPermission("video", "p2"))

		// add a new track after permissions are set
		trs := &typesfakes.FakeMediaTrack{}
		trs.IDReturns("screen")
		um.AddPublishedTrack(trs)

		require.True(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("video", "p1"))
		require.True(t, pm.HasPermission("screen", "p1"))
		require.True(t, pm.HasPermission("audio", "p2"))
		require.True(t, pm.HasPermission("video", "p2"))
		require.True(t, pm.HasPermission("screen", "p2"))

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
		pm.UpdateSubscriptionPermission(subscriptionPermission, vg.Next(), nil)
		require.True(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("video", "p1"))
		require.True(t, pm.HasPermission("screen", "p1"))

		require.True(t, pm.HasPermission("audio", "p2"))
		require.False(t, pm.HasPermission("video", "p2"))
		require.False(t, pm.HasPermission("screen", "p2"))

		require.False(t, pm.HasPermission("audio", "p3"))
		require.True(t, pm.HasPermission("video", "p3"))
		require.False(t, pm.HasPermission("screen", "p3"))

		// add a new track after restrictive permissions are set
		trw := &typesfakes.FakeMediaTrack{}
		trw.IDReturns("watch")
		um.AddPublishedTrack(trw)

		require.True(t, pm.HasPermission("audio", "p1"))
		require.True(t, pm.HasPermission("video", "p1"))
		require.True(t, pm.HasPermission("screen", "p1"))
		require.True(t, pm.HasPermission("watch", "p1"))

		require.True(t, pm.HasPermission("audio", "p2"))
		require.False(t, pm.HasPermission("video", "p2"))
		require.False(t, pm.HasPermission("screen", "p2"))
		require.False(t, pm.HasPermission("watch", "p2"))

		require.False(t, pm.HasPermission("audio", "p3"))
		require.True(t, pm.HasPermission("video", "p3"))
		require.False(t, pm.HasPermission("screen", "p3"))
		require.False(t, pm.HasPermission("watch", "p3"))
	})
}

func TestSubscriptionPermissionManagerTrackSources(t *testing.T) {
	vg := utils.NewDefaultTimedVersionGenerator()
	denyAll := &livekit.SubscriptionPermission{}

	um := NewUpTrackManager(UpTrackManagerParams{
		Logger: logger.GetLogger(),
	})
	udm, _, _ := newUpDataTrackManagerForTest()
	pm := NewSubscriptionPermissionManager(SubscriptionPermissionManagerParams{
		Logger:           logger.GetLogger(),
		VersionGenerator: vg,
		GetMediaTracks:   um.GetPublishedTracks,
		GetDataTracks:    udm.GetPublishedDataTracks,
	})

	tra := &typesfakes.FakeMediaTrack{}
	tra.IDReturns("TR_audio")
	trv := &typesfakes.FakeMediaTrack{}
	trv.IDReturns("TR_video")
	um.AddPublishedTrack(tra)
	um.AddPublishedTrack(trv)

	dt1 := newFakeDataTrackForTest(1)
	dt1.IDReturns("DT_1")
	dt1Replacement := newFakeDataTrackForTest(1)
	dt1Replacement.IDReturns("DT_1_new")
	dt2 := newFakeDataTrackForTest(2)
	dt2.IDReturns("DT_2")
	udm.AddPublishedDataTrack(dt1)
	udm.AddPublishedDataTrack(dt1Replacement)
	udm.AddPublishedDataTrack(dt2)

	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, trv.RevokeDisallowedSubscribersCallCount())
	require.Zero(t, dt1.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, dt1Replacement.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, dt2.RevokeDisallowedSubscribersCallCount())

	// OnClose callback unregisters tra; RemovePublishedTrack unregisters trv; RemovePublishedDataTrack unregisters dt1Replacement.
	tra.AddOnCloseArgsForCall(0)(false)
	um.RemovePublishedTrack(trv, false)
	udm.RemovePublishedDataTrack(dt1Replacement)
	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Equal(t, 1, tra.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, trv.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 1, dt1Replacement.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 2, dt2.RevokeDisallowedSubscribersCallCount())

	// Closing managers leaves no tracks to revoke.
	trs := &typesfakes.FakeMediaTrack{}
	trs.IDReturns("TR_screen")
	um.AddPublishedTrack(trs)
	um.Close(false)
	udm.Close()
	require.NoError(t, pm.UpdateSubscriptionPermission(denyAll, vg.Next(), nil))
	require.Zero(t, trs.RevokeDisallowedSubscribersCallCount())
	require.Equal(t, 2, dt2.RevokeDisallowedSubscribersCallCount())
}

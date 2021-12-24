package rtc

import (
	"testing"

	"github.com/livekit/livekit-server/pkg/rtc/types/typesfakes"
	"github.com/livekit/protocol/livekit"
	"github.com/stretchr/testify/require"
)

func TestUpdateSubscriptionPermissions(t *testing.T) {
	t.Run("updates permissions", func(t *testing.T) {
		um := NewUptrackManager(UptrackManagerParams{})

		tra := &typesfakes.FakePublishedTrack{}
		tra.IDReturns("audio")
		um.publishedTracks["audio"] = tra

		trv := &typesfakes.FakePublishedTrack{}
		trv.IDReturns("video")
		um.publishedTracks["video"] = trv

		// no restrictive permissions
		permissions := &livekit.UpdateSubscriptionPermissions{
			AllParticipants: true,
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.Nil(t, um.subscriptionPermissions)

		// nobody is allowed to subscribe
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.NotNil(t, um.subscriptionPermissions)
		require.Equal(t, 0, len(um.subscriptionPermissions))

		// allow all tracks for participants
		perms1 := &livekit.TrackPermission{
			ParticipantSid: "p1",
			AllTracks:      true,
		}
		perms2 := &livekit.TrackPermission{
			ParticipantSid: "p2",
			AllTracks:      true,
		}
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{
				perms1,
				perms2,
			},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.Equal(t, 2, len(um.subscriptionPermissions))
		require.EqualValues(t, perms1, um.subscriptionPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriptionPermissions["p2"])

		// allow all tracks for some and restrictive for others
		perms1 = &livekit.TrackPermission{
			ParticipantSid: "p1",
			AllTracks:      true,
		}
		perms2 = &livekit.TrackPermission{
			ParticipantSid: "p2",
			TrackSids:      []string{"audio"},
		}
		perms3 := &livekit.TrackPermission{
			ParticipantSid: "p3",
			TrackSids:      []string{"video"},
		}
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{
				perms1,
				perms2,
				perms3,
			},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.Equal(t, 3, len(um.subscriptionPermissions))
		require.EqualValues(t, perms1, um.subscriptionPermissions["p1"])
		require.EqualValues(t, perms2, um.subscriptionPermissions["p2"])
		require.EqualValues(t, perms3, um.subscriptionPermissions["p3"])
	})
}

func TestPermissions(t *testing.T) {
	t.Run("checks permissions", func(t *testing.T) {
		um := NewUptrackManager(UptrackManagerParams{})

		tra := &typesfakes.FakePublishedTrack{}
		tra.IDReturns("audio")
		um.publishedTracks["audio"] = tra

		trv := &typesfakes.FakePublishedTrack{}
		trv.IDReturns("video")
		um.publishedTracks["video"] = trv

		// no restrictive permissions
		permissions := &livekit.UpdateSubscriptionPermissions{
			AllParticipants: true,
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.True(t, um.hasPermission("audio", "p1"))
		require.True(t, um.hasPermission("audio", "p2"))

		// nobody is allowed to subscribe
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.False(t, um.hasPermission("audio", "p1"))
		require.False(t, um.hasPermission("audio", "p2"))

		// allow all tracks for participants
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantSid: "p1",
					AllTracks:      true,
				},
				{
					ParticipantSid: "p2",
					AllTracks:      true,
				},
			},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.True(t, um.hasPermission("audio", "p1"))
		require.True(t, um.hasPermission("video", "p1"))
		require.True(t, um.hasPermission("audio", "p2"))
		require.True(t, um.hasPermission("video", "p2"))

		// allow all tracks for some and restrictive for others
		permissions = &livekit.UpdateSubscriptionPermissions{
			TrackPermissions: []*livekit.TrackPermission{
				{
					ParticipantSid: "p1",
					AllTracks:      true,
				},
				{
					ParticipantSid: "p2",
					TrackSids:      []string{"audio"},
				},
				{
					ParticipantSid: "p3",
					TrackSids:      []string{"video"},
				},
			},
		}
		um.UpdateSubscriptionPermissions(permissions, nil)
		require.True(t, um.hasPermission("audio", "p1"))
		require.True(t, um.hasPermission("video", "p1"))
		require.True(t, um.hasPermission("audio", "p2"))
		require.False(t, um.hasPermission("video", "p2"))
		require.False(t, um.hasPermission("audio", "p3"))
		require.True(t, um.hasPermission("video", "p3"))
	})
}

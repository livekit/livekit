package signaldeduper

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

func TestSubscriptionDeduper(t *testing.T) {
	t.Run("dedupes subscription", func(t *testing.T) {
		sd := NewSubscriptionDeduper(logger.GetLogger())

		// new track using UpdateSubscription
		us := &livekit.SignalRequest{
			Message: &livekit.SignalRequest_Subscription{
				Subscription: &livekit.UpdateSubscription{
					TrackSids: []string{
						"p1.track1",
						"p1.track2",
					},
					Subscribe: true,
				},
			},
		}
		require.False(t, sd.Dedupe("p0", us))

		// new track using UpdateSubscription - using ParticipantTracks
		us = &livekit.SignalRequest{
			Message: &livekit.SignalRequest_Subscription{
				Subscription: &livekit.UpdateSubscription{
					ParticipantTracks: []*livekit.ParticipantTracks{
						&livekit.ParticipantTracks{
							ParticipantSid: "p2",
							TrackSids: []string{
								"p2.track1",
								"p2.track2",
							},
						},
						&livekit.ParticipantTracks{
							ParticipantSid: "p3",
							TrackSids: []string{
								"p3.track1",
								"p3.track2",
							},
						},
					},
					Subscribe: true,
				},
			},
		}
		require.False(t, sd.Dedupe("p0", us))

		// some tracks re-subscribing, should be a dupe
		us = &livekit.SignalRequest{
			Message: &livekit.SignalRequest_Subscription{
				Subscription: &livekit.UpdateSubscription{
					TrackSids: []string{
						"p1.track1",
					},
					ParticipantTracks: []*livekit.ParticipantTracks{
						&livekit.ParticipantTracks{
							ParticipantSid: "p2",
							TrackSids: []string{
								"p2.track1",
							},
						},
					},
					Subscribe: true,
				},
			},
		}
		require.True(t, sd.Dedupe("p0", us))

		// update track settings, should not be a dupe
		uts := &livekit.SignalRequest{
			Message: &livekit.SignalRequest_TrackSetting{
				TrackSetting: &livekit.UpdateTrackSettings{
					TrackSids: []string{
						"p1.track1",
					},
					Width: 1280,
				},
			},
		}
		require.False(t, sd.Dedupe("p0", uts))

		// same message again will be a dupe
		require.True(t, sd.Dedupe("p0", uts))

		// unsubscribe a track, should not be a dupe
		uts = &livekit.SignalRequest{
			Message: &livekit.SignalRequest_TrackSetting{
				TrackSetting: &livekit.UpdateTrackSettings{
					TrackSids: []string{
						"p2.track1",
					},
					Disabled: true,
				},
			},
		}
		require.False(t, sd.Dedupe("p0", uts))

		// use UpdateSubscription and unsubscribe, although different protocol message, effect is the same, hence should be dupe
		us = &livekit.SignalRequest{
			Message: &livekit.SignalRequest_Subscription{
				Subscription: &livekit.UpdateSubscription{
					TrackSids: []string{
						"p2.track1",
					},
				},
			},
		}
		require.True(t, sd.Dedupe("p0", us))

		//
		// Although unsubscribed, updating track setting with some other value populated should return not a dupe.
		// Although track is still unsubscribed, deduper does not extrapolate functionality and does only a equality comparison
		// to be on the safe side.
		//
		uts = &livekit.SignalRequest{
			Message: &livekit.SignalRequest_TrackSetting{
				TrackSetting: &livekit.UpdateTrackSettings{
					TrackSids: []string{
						"p2.track1",
					},
					Disabled: true,
					Quality:  livekit.VideoQuality_HIGH,
				},
			},
		}
		require.False(t, sd.Dedupe("p0", uts))
	})
}

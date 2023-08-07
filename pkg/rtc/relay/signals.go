package relay

import (
	"github.com/livekit/protocol/livekit"
)

type AddTrackSignal struct {
	Identity string             `json:"identity,omitempty"`
	Track    *livekit.TrackInfo `json:"track,omitempty"`
}

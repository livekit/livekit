package rtc

import (
	"context"
	"fmt"
	"strings"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
)

type EgressLauncher interface {
	StartEgress(context.Context, *livekit.StartEgressRequest) (*livekit.EgressInfo, error)
}

func (r *Room) startTrackEgress(track types.MediaTrack) {
	if r.egressLauncher == nil {
		r.Logger.Errorw("egress launcher not found", nil)
		return
	}

	output := &livekit.TrackEgressRequest_File{
		File: &livekit.DirectFileOutput{},
	}

	if prefix := r.protoRoom.Egress.Tracks.FilePrefix; prefix != "" {
		if strings.HasSuffix(prefix, "/") {
			output.File.Filepath = prefix
		} else {
			output.File.Filepath = fmt.Sprintf("%s_%s", prefix, track.ID())
		}
	}

	switch out := r.protoRoom.Egress.Tracks.Output.(type) {
	case *livekit.AutoTrackEgress_Azure:
		output.File.Output = &livekit.DirectFileOutput_Azure{Azure: out.Azure}
	case *livekit.AutoTrackEgress_Gcp:
		output.File.Output = &livekit.DirectFileOutput_Gcp{Gcp: out.Gcp}
	case *livekit.AutoTrackEgress_S3:
		output.File.Output = &livekit.DirectFileOutput_S3{S3: out.S3}
	}

	_, err := r.egressLauncher.StartEgress(context.Background(), &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Track{
			Track: &livekit.TrackEgressRequest{
				RoomName: string(r.Name()),
				TrackId:  string(track.ID()),
				Output:   output,
			},
		},
		RoomId: string(r.ID()),
	})
	if err != nil {
		r.Logger.Errorw("failed to launch track egress", err,
			"trackID", track.ID(),
		)
	}
}

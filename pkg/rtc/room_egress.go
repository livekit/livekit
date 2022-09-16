package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
)

type EgressLauncher interface {
	StartEgress(context.Context, *livekit.StartEgressRequest) (*livekit.EgressInfo, error)
}

func StartTrackEgress(
	ctx context.Context,
	launcher EgressLauncher,
	track types.MediaTrack,
	opts *livekit.AutoTrackEgress,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) error {
	if launcher == nil {
		return errors.New("egress launcher not found")
	}

	output := &livekit.TrackEgressRequest_File{
		File: &livekit.DirectFileOutput{},
	}

	if prefix := opts.FilePrefix; prefix != "" {
		if strings.HasSuffix(prefix, "/") {
			output.File.Filepath = prefix
		} else {
			output.File.Filepath = fmt.Sprintf("%s_%s", prefix, track.ID())
		}
	}

	switch out := opts.Output.(type) {
	case *livekit.AutoTrackEgress_Azure:
		output.File.Output = &livekit.DirectFileOutput_Azure{Azure: out.Azure}
	case *livekit.AutoTrackEgress_Gcp:
		output.File.Output = &livekit.DirectFileOutput_Gcp{Gcp: out.Gcp}
	case *livekit.AutoTrackEgress_S3:
		output.File.Output = &livekit.DirectFileOutput_S3{S3: out.S3}
	}

	_, err := launcher.StartEgress(ctx, &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Track{
			Track: &livekit.TrackEgressRequest{
				RoomName: string(roomName),
				TrackId:  string(track.ID()),
				Output:   output,
			},
		},
		RoomId: string(roomID),
	})
	return err
}

package rtc

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/webhook"
)

type EgressLauncher interface {
	StartEgress(context.Context, *livekit.StartEgressRequest) (*livekit.EgressInfo, error)
}

func StartTrackEgress(
	ctx context.Context,
	launcher EgressLauncher,
	ts telemetry.TelemetryService,
	track types.MediaTrack,
	opts *livekit.AutoTrackEgress,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) {
	if req, err := startTrackEgress(ctx, launcher, track, opts, roomName, roomID); err != nil {
		// send egress failed webhook
		ts.NotifyEvent(ctx, &livekit.WebhookEvent{
			Event: webhook.EventEgressEnded,
			EgressInfo: &livekit.EgressInfo{
				RoomId:   string(roomID),
				RoomName: string(roomName),
				Status:   livekit.EgressStatus_EGRESS_FAILED,
				Error:    err.Error(),
				Request:  &livekit.EgressInfo_Track{Track: req},
			},
		})
	}
}

func startTrackEgress(
	ctx context.Context,
	launcher EgressLauncher,
	track types.MediaTrack,
	opts *livekit.AutoTrackEgress,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) (*livekit.TrackEgressRequest, error) {

	output := &livekit.DirectFileOutput{}
	if prefix := opts.FilePrefix; prefix != "" {
		if strings.HasSuffix(prefix, "/") {
			output.Filepath = fmt.Sprintf("%s%s_%s", prefix, track.ID(), time.Now().Format("2006-01-02T150405"))
		} else {
			output.Filepath = fmt.Sprintf("%s_%s_%s", prefix, track.ID(), time.Now().Format("2006-01-02T150405"))
		}
	}

	switch out := opts.Output.(type) {
	case *livekit.AutoTrackEgress_Azure:
		output.Output = &livekit.DirectFileOutput_Azure{Azure: out.Azure}
	case *livekit.AutoTrackEgress_Gcp:
		output.Output = &livekit.DirectFileOutput_Gcp{Gcp: out.Gcp}
	case *livekit.AutoTrackEgress_S3:
		output.Output = &livekit.DirectFileOutput_S3{S3: out.S3}
	}

	req := &livekit.TrackEgressRequest{
		RoomName: string(roomName),
		TrackId:  string(track.ID()),
		Output: &livekit.TrackEgressRequest_File{
			File: output,
		},
	}

	if launcher == nil {
		return req, errors.New("egress launcher not found")
	}

	_, err := launcher.StartEgress(ctx, &livekit.StartEgressRequest{
		Request: &livekit.StartEgressRequest_Track{
			Track: req,
		},
		RoomId: string(roomID),
	})
	return req, err
}

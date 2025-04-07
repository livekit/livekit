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
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/telemetry"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/protocol/webhook"
)

type EgressLauncher interface {
	StartEgress(context.Context, *rpc.StartEgressRequest) (*livekit.EgressInfo, error)
}

func StartParticipantEgress(
	ctx context.Context,
	launcher EgressLauncher,
	ts telemetry.TelemetryService,
	opts *livekit.AutoParticipantEgress,
	identity livekit.ParticipantIdentity,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) error {
	if req, err := startParticipantEgress(ctx, launcher, opts, identity, roomName, roomID); err != nil {
		// send egress failed webhook

		info := &livekit.EgressInfo{
			RoomId:   string(roomID),
			RoomName: string(roomName),
			Status:   livekit.EgressStatus_EGRESS_FAILED,
			Error:    err.Error(),
			Request:  &livekit.EgressInfo_Participant{Participant: req},
		}

		ts.NotifyEgressEvent(ctx, webhook.EventEgressEnded, info)

		return err
	}
	return nil
}

func startParticipantEgress(
	ctx context.Context,
	launcher EgressLauncher,
	opts *livekit.AutoParticipantEgress,
	identity livekit.ParticipantIdentity,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) (*livekit.ParticipantEgressRequest, error) {
	req := &livekit.ParticipantEgressRequest{
		RoomName:       string(roomName),
		Identity:       string(identity),
		FileOutputs:    opts.FileOutputs,
		SegmentOutputs: opts.SegmentOutputs,
	}

	switch o := opts.Options.(type) {
	case *livekit.AutoParticipantEgress_Preset:
		req.Options = &livekit.ParticipantEgressRequest_Preset{Preset: o.Preset}
	case *livekit.AutoParticipantEgress_Advanced:
		req.Options = &livekit.ParticipantEgressRequest_Advanced{Advanced: o.Advanced}
	}

	if launcher == nil {
		return req, errors.New("egress launcher not found")
	}

	_, err := launcher.StartEgress(ctx, &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_Participant{
			Participant: req,
		},
		RoomId: string(roomID),
	})
	return req, err
}

func StartTrackEgress(
	ctx context.Context,
	launcher EgressLauncher,
	ts telemetry.TelemetryService,
	opts *livekit.AutoTrackEgress,
	track types.MediaTrack,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) error {
	if req, err := startTrackEgress(ctx, launcher, opts, track, roomName, roomID); err != nil {
		// send egress failed webhook

		info := &livekit.EgressInfo{
			RoomId:   string(roomID),
			RoomName: string(roomName),
			Status:   livekit.EgressStatus_EGRESS_FAILED,
			Error:    err.Error(),
			Request:  &livekit.EgressInfo_Track{Track: req},
		}
		ts.NotifyEgressEvent(ctx, webhook.EventEgressEnded, info)

		return err
	}
	return nil
}

func startTrackEgress(
	ctx context.Context,
	launcher EgressLauncher,
	opts *livekit.AutoTrackEgress,
	track types.MediaTrack,
	roomName livekit.RoomName,
	roomID livekit.RoomID,
) (*livekit.TrackEgressRequest, error) {
	output := &livekit.DirectFileOutput{
		Filepath: getFilePath(opts.Filepath),
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

	_, err := launcher.StartEgress(ctx, &rpc.StartEgressRequest{
		Request: &rpc.StartEgressRequest_Track{
			Track: req,
		},
		RoomId: string(roomID),
	})
	return req, err
}

func getFilePath(filepath string) string {
	if filepath == "" || strings.HasSuffix(filepath, "/") || strings.Contains(filepath, "{track_id}") {
		return filepath
	}

	idx := strings.Index(filepath, ".")
	if idx == -1 {
		return fmt.Sprintf("%s-{track_id}", filepath)
	} else {
		return fmt.Sprintf("%s-%s%s", filepath[:idx], "{track_id}", filepath[idx:])
	}
}

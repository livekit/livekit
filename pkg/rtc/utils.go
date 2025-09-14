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
	"errors"
	"io"
	"net"
	"strings"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"

	"github.com/livekit/livekit-server/pkg/sfu/mime"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	trackIdSeparator = "|"

	cMinIPTruncateLen = 8
)

func UnpackStreamID(packed string) (participantID livekit.ParticipantID, trackID livekit.TrackID) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) > 1 {
		return livekit.ParticipantID(parts[0]), livekit.TrackID(packed[len(parts[0])+1:])
	}
	return livekit.ParticipantID(packed), ""
}

func PackStreamID(participantID livekit.ParticipantID, trackID livekit.TrackID) string {
	return string(participantID) + trackIdSeparator + string(trackID)
}

func PackSyncStreamID(participantID livekit.ParticipantID, stream string) string {
	return string(participantID) + trackIdSeparator + stream
}

func StreamFromTrackSource(source livekit.TrackSource) string {
	// group camera/mic, screenshare/audio together
	switch source {
	case livekit.TrackSource_SCREEN_SHARE:
		return "screen"
	case livekit.TrackSource_SCREEN_SHARE_AUDIO:
		return "screen"
	case livekit.TrackSource_CAMERA:
		return "camera"
	case livekit.TrackSource_MICROPHONE:
		return "camera"
	}
	return "unknown"
}

func PackDataTrackLabel(participantID livekit.ParticipantID, trackID livekit.TrackID, label string) string {
	return string(participantID) + trackIdSeparator + string(trackID) + trackIdSeparator + label
}

func UnpackDataTrackLabel(packed string) (participantID livekit.ParticipantID, trackID livekit.TrackID, label string) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) != 3 {
		return "", livekit.TrackID(packed), ""
	}
	participantID = livekit.ParticipantID(parts[0])
	trackID = livekit.TrackID(parts[1])
	label = parts[2]
	return
}

func ToProtoTrackKind(kind webrtc.RTPCodecType) livekit.TrackType {
	switch kind {
	case webrtc.RTPCodecTypeVideo:
		return livekit.TrackType_VIDEO
	case webrtc.RTPCodecTypeAudio:
		return livekit.TrackType_AUDIO
	}
	panic("unsupported track direction")
}

func IsEOF(err error) bool {
	return err == io.ErrClosedPipe || err == io.EOF
}

func Recover(l logger.Logger) any {
	if l == nil {
		l = logger.GetLogger()
	}
	r := recover()
	if r != nil {
		var err error
		switch e := r.(type) {
		case string:
			err = errors.New(e)
		case error:
			err = e
		default:
			err = errors.New("unknown panic")
		}
		l.Errorw("recovered panic", err, "panic", r)
	}

	return r
}

// logger helpers
func LoggerWithParticipant(l logger.Logger, identity livekit.ParticipantIdentity, sid livekit.ParticipantID, isRemote bool) logger.Logger {
	values := make([]interface{}, 0, 4)
	if identity != "" {
		values = append(values, "participant", identity)
	}
	if sid != "" {
		values = append(values, "pID", sid)
	}
	values = append(values, "remote", isRemote)
	// enable sampling per participant
	return l.WithValues(values...)
}

func LoggerWithRoom(l logger.Logger, name livekit.RoomName, roomID livekit.RoomID) logger.Logger {
	values := make([]interface{}, 0, 2)
	if name != "" {
		values = append(values, "room", name)
	}
	if roomID != "" {
		values = append(values, "roomID", roomID)
	}
	// also sample for the room
	return l.WithItemSampler().WithValues(values...)
}

func LoggerWithTrack(l logger.Logger, trackID livekit.TrackID, isRelayed bool) logger.Logger {
	// sampling not required because caller already passing in participant's logger
	if trackID != "" {
		return l.WithValues("trackID", trackID, "relayed", isRelayed)
	}
	return l
}

func LoggerWithPCTarget(l logger.Logger, target livekit.SignalTarget) logger.Logger {
	return l.WithValues("transport", target)
}

func LoggerWithCodecMime(l logger.Logger, mimeType mime.MimeType) logger.Logger {
	if mimeType != mime.MimeTypeUnknown {
		return l.WithValues("mime", mimeType.String())
	}
	return l
}

func MaybeTruncateIP(addr string) string {
	ipAddr := net.ParseIP(addr)
	if ipAddr == nil {
		return ""
	}

	if ipAddr.IsPrivate() || len(addr) <= cMinIPTruncateLen {
		return addr
	}

	return addr[:len(addr)-3] + "..."
}

func ChunkProtoBatch[T proto.Message](batch []T, target int) [][]T {
	var chunks [][]T
	var start, size int
	for i, m := range batch {
		s := proto.Size(m)
		if size+s > target {
			if start < i {
				chunks = append(chunks, batch[start:i])
			}
			start = i
			size = 0
		}
		size += s
	}
	if start < len(batch) {
		chunks = append(chunks, batch[start:])
	}
	return chunks
}

func IsRedEnabled(ti *livekit.TrackInfo) bool {
	if len(ti.Codecs) != 0 && ti.Codecs[0].MimeType != "" {
		return mime.IsMimeTypeStringRED(ti.Codecs[0].MimeType)
	}

	return !ti.GetDisableRed()
}

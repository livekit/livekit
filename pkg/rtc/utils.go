package rtc

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/livekit/protocol/logger"
	livekit "github.com/livekit/protocol/proto"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const (
	trackIdSeparator = "|"
)

func UnpackStreamID(packed string) (participantId string, trackId string) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) > 1 {
		return parts[0], packed[len(parts[0])+1:]
	}
	return packed, ""
}

func PackStreamID(participantId, trackId string) string {
	return participantId + trackIdSeparator + trackId
}

func PackDataTrackLabel(participantId, trackId string, label string) string {
	return participantId + trackIdSeparator + trackId + trackIdSeparator + label
}

func UnpackDataTrackLabel(packed string) (peerId string, trackId string, label string) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) != 3 {
		return "", packed, ""
	}
	peerId = parts[0]
	trackId = parts[1]
	label = parts[2]
	return
}

// converts a fixed point number to the number part of %
func FixedPointToPercent(frac uint8) uint32 {
	return (uint32(frac) * 100) >> 8
}

func ToProtoParticipants(participants []types.Participant) []*livekit.ParticipantInfo {
	infos := make([]*livekit.ParticipantInfo, 0, len(participants))
	for _, op := range participants {
		infos = append(infos, op.ToProto())
	}
	return infos
}

func ToProtoSessionDescription(sd webrtc.SessionDescription) *livekit.SessionDescription {
	return &livekit.SessionDescription{
		Type: sd.Type.String(),
		Sdp:  sd.SDP,
	}
}

func FromProtoSessionDescription(sd *livekit.SessionDescription) webrtc.SessionDescription {
	var sdType webrtc.SDPType
	switch sd.Type {
	case webrtc.SDPTypeOffer.String():
		sdType = webrtc.SDPTypeOffer
	case webrtc.SDPTypeAnswer.String():
		sdType = webrtc.SDPTypeAnswer
	case webrtc.SDPTypePranswer.String():
		sdType = webrtc.SDPTypePranswer
	case webrtc.SDPTypeRollback.String():
		sdType = webrtc.SDPTypeRollback
	}
	return webrtc.SessionDescription{
		Type: sdType,
		SDP:  sd.Sdp,
	}
}

func ToProtoTrickle(candidateInit webrtc.ICECandidateInit) *livekit.TrickleRequest {
	data, _ := json.Marshal(candidateInit)
	return &livekit.TrickleRequest{
		CandidateInit: string(data),
	}
}

func FromProtoTrickle(trickle *livekit.TrickleRequest) (webrtc.ICECandidateInit, error) {
	ci := webrtc.ICECandidateInit{}
	err := json.Unmarshal([]byte(trickle.CandidateInit), &ci)
	if err != nil {
		return webrtc.ICECandidateInit{}, err
	}
	return ci, nil
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

func RecoverSilent() {
	recover()
}

func Recover() {
	if r := recover(); r != nil {
		var err error
		switch e := r.(type) {
		case string:
			err = errors.New(e)
		case error:
			err = e
		default:
			err = errors.New("unknown panic")
		}
		logger.GetLogger().Error(err, "recovered panic", "panic", r)
	}
}

package rtc

import (
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/go-logr/logr"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const (
	trackIdSeparator = "|"
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

func ToProtoParticipants(participants []types.LocalParticipant) []*livekit.ParticipantInfo {
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
		logger.Errorw("recovered panic", err, "panic", r)
	}
}

// logger helpers
func LoggerWithParticipant(l logger.Logger, identity livekit.ParticipantIdentity, sid livekit.ParticipantID, isRemote bool) logger.Logger {
	lr := logr.Logger(l)
	if identity != "" {
		lr = lr.WithValues("participant", identity)
	}
	if sid != "" {
		lr = lr.WithValues("pID", sid)
	}
	lr = lr.WithValues("remote", isRemote)
	return logger.Logger(lr)
}

func LoggerWithRoom(l logger.Logger, name livekit.RoomName, roomID livekit.RoomID) logger.Logger {
	lr := logr.Logger(l)
	if name != "" {
		lr = lr.WithValues("room", name)
	}
	if roomID != "" {
		lr = lr.WithValues("roomID", roomID)
	}
	return logger.Logger(lr)
}

func LoggerWithTrack(l logger.Logger, trackID livekit.TrackID, isRelayed bool) logger.Logger {
	lr := logr.Logger(l)
	if trackID != "" {
		lr = lr.WithValues("trackID", trackID)
		lr = lr.WithValues("relayed", isRelayed)
	}
	return logger.Logger(lr)
}

func LoggerWithPCTarget(l logger.Logger, target livekit.SignalTarget) logger.Logger {
	lr := logr.Logger(l)
	if lr.GetSink() == nil {
		return l
	}

	lr = lr.WithValues("transport", target)
	return logger.Logger(lr)
}

func LoggerWithCodecMime(l logger.Logger, mime string) logger.Logger {
	lr := logr.Logger(l)
	if mime != "" {
		lr = lr.WithValues("mime", mime)
	}
	return logger.Logger(lr)
}

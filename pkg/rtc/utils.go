package rtc

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	trackIdSeparator = "|"
)

func UnpackTrackId(packed string) (peerId string, trackId string) {
	parts := strings.Split(packed, trackIdSeparator)
	if len(parts) > 1 {
		return parts[0], packed[len(parts[0])+1:]
	}
	return "", packed
}

func PackTrackId(participantId, trackId string) string {
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

func ToProtoParticipants(participants []Participant) []*livekit.ParticipantInfo {
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

func ToProtoTrickle(candidateInit webrtc.ICECandidateInit) *livekit.Trickle {
	data, _ := json.Marshal(candidateInit)
	return &livekit.Trickle{
		CandidateInit: string(data),
	}
}

func FromProtoTrickle(trickle *livekit.Trickle) webrtc.ICECandidateInit {
	ci := webrtc.ICECandidateInit{}
	json.Unmarshal([]byte(trickle.CandidateInit), &ci)
	return ci
}

func ToProtoTrack(t PublishedTrack) *livekit.TrackInfo {
	return &livekit.TrackInfo{
		Sid:   t.ID(),
		Type:  t.Kind(),
		Name:  t.StreamID(),
		Muted: t.IsMuted(),
	}
}

func IsEOF(err error) bool {
	return err == io.ErrClosedPipe || err == io.EOF
}

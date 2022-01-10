package rtc

import (
	"errors"
	"time"

	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/protocol/livekit"
	"github.com/pion/webrtc/v3"
)

var (
	ErrUnimplemented = errors.New("unimplemented")
)

type NoOpLocalParticipant struct {
}

func (p *NoOpLocalParticipant) ProtocolVersion() types.ProtocolVersion {
	return 0
}

func (p *NoOpLocalParticipant) ConnectedAt() time.Time {
	return time.Time{}
}

func (p *NoOpLocalParticipant) State() livekit.ParticipantInfo_State {
	return livekit.ParticipantInfo_DISCONNECTED
}

func (p *NoOpLocalParticipant) IsReady() bool {
	return false
}

func (p *NoOpLocalParticipant) IsRecorder() bool {
	return false
}

func (p *NoOpLocalParticipant) SubscriberAsPrimary() bool {
	return false
}

func (p *NoOpLocalParticipant) GetResponseSink() routing.MessageSink {
	return nil
}

func (p *NoOpLocalParticipant) SetResponseSink(_sink routing.MessageSink) {
}

func (p *NoOpLocalParticipant) SetPermission(_permission *livekit.ParticipantPermission) {
}

func (p *NoOpLocalParticipant) CanPublish() bool {
	return false
}

func (p *NoOpLocalParticipant) CanSubscribe() bool {
	return false
}

func (p *NoOpLocalParticipant) CanPublishData() bool {
	return false
}

func (p *NoOpLocalParticipant) AddICECandidate(_candidate webrtc.ICECandidateInit, _target livekit.SignalTarget) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) HandleOffer(_sdp webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	return webrtc.SessionDescription{}, ErrUnimplemented
}

func (p *NoOpLocalParticipant) AddTrack(_req *livekit.AddTrackRequest) {
}

func (p *NoOpLocalParticipant) SetTrackMuted(_trackID livekit.TrackID, _muted bool, _fromAdmin bool) {
}

func (p *NoOpLocalParticipant) SubscriberMediaEngine() *webrtc.MediaEngine {
	return nil
}

func (p *NoOpLocalParticipant) SubscriberPC() *webrtc.PeerConnection {
	return nil
}

func (p *NoOpLocalParticipant) HandleAnswer(_sdp webrtc.SessionDescription) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) Negotiate() {
}

func (p *NoOpLocalParticipant) ICERestart() error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) AddSubscribedTrack(_subTrack types.SubscribedTrack) {
}

func (p *NoOpLocalParticipant) RemoveSubscribedTrack(_subTrack types.SubscribedTrack) {
}

func (p *NoOpLocalParticipant) GetSubscribedTrack(_sid livekit.TrackID) types.SubscribedTrack {
	return nil
}

func (p *NoOpLocalParticipant) GetSubscribedTracks() []types.SubscribedTrack {
	return nil
}

func (p *NoOpLocalParticipant) GetSubscribedParticipants() []livekit.ParticipantID {
	return nil
}

func (t *NoOpLocalParticipant) GetAudioLevel() (level uint8, active bool) {
	return SilentAudioLevel, false
}

func (p *NoOpLocalParticipant) GetConnectionQuality() *livekit.ConnectionQualityInfo {
	return &livekit.ConnectionQualityInfo{}
}

func (p *NoOpLocalParticipant) SendJoinResponse(
	_roomInfo *livekit.Room,
	_otherParticipants []*livekit.ParticipantInfo,
	_iceServers []*livekit.ICEServer,
) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SendParticipantUpdate(_participantsToUpdate []*livekit.ParticipantInfo, updatedAt time.Time) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SendSpeakerUpdate(_speakers []*livekit.SpeakerInfo) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SendDataPacket(_dp *livekit.DataPacket) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SendRoomUpdate(_room *livekit.Room) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SendConnectionQualityUpdate(_update *livekit.ConnectionQualityUpdate) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SubscriptionPermissionUpdate(_publisherID livekit.ParticipantID, _trackID livekit.TrackID, _allowed bool) {
}

func (p *NoOpLocalParticipant) OnStateChange(_callback func(p types.Participant, oldState livekit.ParticipantInfo_State)) {
}

func (p *NoOpLocalParticipant) OnTrackUpdated(_callback func(types.Participant, types.PublishedTrack)) {
}

func (p *NoOpLocalParticipant) OnMetadataUpdate(_callback func(types.Participant)) {
}

func (p *NoOpLocalParticipant) OnDataPacket(_callback func(types.Participant, *livekit.DataPacket)) {
}

func (p *NoOpLocalParticipant) OnClose(_callback func(types.Participant, map[livekit.TrackID]livekit.ParticipantID)) {
}

func (p *NoOpLocalParticipant) UpdateSubscribedQuality(_nodeID string, _trackID livekit.TrackID, _maxQuality livekit.VideoQuality) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) UpdateMediaLoss(_nodeID string, _trackID livekit.TrackID, _fractionalLoss uint32) error {
	return ErrUnimplemented
}

func (p *NoOpLocalParticipant) SetMigrateState(_s types.MigrateState) {
}

func (p *NoOpLocalParticipant) MigrateState() types.MigrateState {
	return types.MigrateStateInit
}

func (p *NoOpLocalParticipant) AddMigratedTrack(cid string, ti *livekit.TrackInfo) {
}

func (p *NoOpLocalParticipant) SetPreviousAnswer(_previous *webrtc.SessionDescription) {
}

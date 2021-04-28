package rtc

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	livekit "github.com/livekit/livekit-server/proto"
	"github.com/livekit/protocol/utils"
)

const (
	DefaultEmptyTimeout = 5 * 60 // 5 mins
)

type Room struct {
	livekit.Room
	config     WebRTCConfig
	iceServers []*livekit.ICEServer
	lock       sync.RWMutex
	// map of identity -> Participant
	participants map[string]types.Participant
	// time the first participant joined the room
	joinedAt atomic.Value
	// time that the last participant left the room
	leftAt   atomic.Value
	isClosed utils.AtomicFlag

	// for active speaker updates
	audioUpdateInterval uint32
	lastActiveSpeakers  []*livekit.SpeakerInfo

	onParticipantChanged func(p types.Participant)
	onClose              func()
}

func NewRoom(room *livekit.Room, config WebRTCConfig, iceServers []*livekit.ICEServer, audioUpdateInterval uint32) *Room {
	r := &Room{
		Room:                *room,
		config:              config,
		iceServers:          iceServers,
		audioUpdateInterval: audioUpdateInterval,
		lock:                sync.RWMutex{},
		participants:        make(map[string]types.Participant),
	}
	if r.EmptyTimeout == 0 {
		r.EmptyTimeout = DefaultEmptyTimeout
	}
	if r.CreationTime == 0 {
		r.CreationTime = time.Now().Unix()
	}
	go r.audioUpdateWorker()
	return r
}

func (r *Room) GetParticipant(identity string) types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	return r.participants[identity]
}

func (r *Room) GetParticipants() []types.Participant {
	r.lock.RLock()
	defer r.lock.RUnlock()
	participants := make([]types.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		participants = append(participants, p)
	}
	return participants
}

func (r *Room) GetActiveSpeakers() []*livekit.SpeakerInfo {
	participants := r.GetParticipants()
	speakers := make([]*livekit.SpeakerInfo, 0, len(participants))
	for _, p := range participants {
		level, active := p.GetAudioLevel()
		if !active {
			continue
		}
		speakers = append(speakers, &livekit.SpeakerInfo{
			Sid:    p.ID(),
			Level:  convertAudioLevel(level),
			Active: active,
		})
	}

	sort.Slice(speakers, func(i, j int) bool {
		return speakers[i].Level > speakers[j].Level
	})
	return speakers
}

func (r *Room) FirstJoinedAt() int64 {
	j := r.joinedAt.Load()
	if t, ok := j.(int64); ok {
		return t
	}
	return 0
}

func (r *Room) LastLeftAt() int64 {
	l := r.leftAt.Load()
	if t, ok := l.(int64); ok {
		return t
	}
	return 0
}

func (r *Room) Join(participant types.Participant) error {
	if r.isClosed.Get() {
		return ErrRoomClosed
	}

	r.lock.Lock()
	defer r.lock.Unlock()

	if r.participants[participant.Identity()] != nil {
		return ErrAlreadyJoined
	}

	if r.MaxParticipants > 0 && int(r.MaxParticipants) == len(r.participants) {
		return ErrMaxParticipantsExceeded
	}

	if r.FirstJoinedAt() == 0 {
		r.joinedAt.Store(time.Now().Unix())
	}

	// it's important to set this before connection, we don't want to miss out on any publishedTracks
	participant.OnTrackPublished(r.onTrackAdded)
	participant.OnStateChange(func(p types.Participant, oldState livekit.ParticipantInfo_State) {
		logger.Debugw("participant state changed", "state", p.State(), "participant", p.Identity(),
			"oldState", oldState)
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(participant)
		}
		r.broadcastParticipantState(p, true)

		if p.State() == livekit.ParticipantInfo_ACTIVE {
			// subscribe participant to existing publishedTracks
			r.subscribeToExistingTracks(p)

			// start the workers once connectivity is established
			p.Start()

		} else if p.State() == livekit.ParticipantInfo_DISCONNECTED {
			// remove participant from room
			go r.RemoveParticipant(p.Identity())
		}
	})
	participant.OnTrackUpdated(r.onTrackUpdated)
	participant.OnMetadataUpdate(r.onParticipantMetadataUpdate)
	participant.OnDataPacket(r.onDataPacket)
	logger.Infow("new participant joined",
		"id", participant.ID(),
		"identity", participant.Identity(),
		"roomId", r.Sid)

	r.participants[participant.Identity()] = participant

	// gather other participants and send join response
	otherParticipants := make([]types.Participant, 0, len(r.participants))
	for _, p := range r.participants {
		if p.ID() != participant.ID() {
			otherParticipants = append(otherParticipants, p)
		}
	}

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}

	return participant.SendJoinResponse(&r.Room, otherParticipants, r.iceServers)
}

func (r *Room) RemoveParticipant(identity string) {
	r.lock.Lock()
	p, ok := r.participants[identity]
	if ok {
		delete(r.participants, identity)
	}
	r.lock.Unlock()
	if !ok {
		return
	}

	// send broadcast only if it's not already closed
	sendUpdates := p.State() != livekit.ParticipantInfo_DISCONNECTED

	p.OnTrackUpdated(nil)
	p.OnTrackPublished(nil)
	p.OnStateChange(nil)
	p.OnMetadataUpdate(nil)
	p.OnDataPacket(nil)

	// close participant as well
	_ = p.Close()

	if len(r.participants) == 0 {
		r.leftAt.Store(time.Now().Unix())
	}

	if sendUpdates {
		if r.onParticipantChanged != nil {
			r.onParticipantChanged(p)
		}
		r.broadcastParticipantState(p, true)
	}
}

func (r *Room) UpdateSubscriptions(participant types.Participant, subscription *livekit.UpdateSubscription) error {
	// find all matching tracks
	var tracks []types.PublishedTrack
	participants := r.GetParticipants()
	for _, p := range participants {
		for _, sid := range subscription.TrackSids {
			for _, track := range p.GetPublishedTracks() {
				if sid == track.ID() {
					tracks = append(tracks, track)
				}
			}
		}
	}

	// handle subscription changes
	for _, track := range tracks {
		if subscription.Subscribe {
			if err := track.AddSubscriber(participant); err != nil {
				return err
			}
		} else {
			track.RemoveSubscriber(participant.ID())
		}
	}
	return nil
}

// CloseIfEmpty closes the room if all participants had left, or it's still empty past timeout
func (r *Room) CloseIfEmpty() {
	if r.isClosed.Get() {
		return
	}

	r.lock.RLock()
	numParticipants := len(r.participants)
	r.lock.RUnlock()

	if numParticipants > 0 {
		return
	}

	var elapsed int64
	if r.FirstJoinedAt() > 0 {
		// compute elasped from last departure
		elapsed = time.Now().Unix() - r.LastLeftAt()
	} else {
		elapsed = time.Now().Unix() - r.CreationTime
	}
	if elapsed >= int64(r.EmptyTimeout) {
		r.Close()
	}
}

func (r *Room) Close() {
	logger.Infow("closing room", "room", r.Sid, "name", r.Name)
	if r.isClosed.TrySet(true) && r.onClose != nil {
		r.onClose()
	}
}

func (r *Room) OnClose(f func()) {
	r.onClose = f
}

func (r *Room) OnParticipantChanged(f func(participant types.Participant)) {
	r.onParticipantChanged = f
}

// a ParticipantImpl in the room added a new remoteTrack, subscribe other participants to it
func (r *Room) onTrackAdded(participant types.Participant, track types.PublishedTrack) {
	// publish participant update, since track state is changed
	r.broadcastParticipantState(participant, true)

	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing participants to this PublishedTrack
	// this is the default behavior. in the future this could be more selective
	for _, existingParticipant := range r.participants {
		if existingParticipant == participant {
			// skip publishing participant
			continue
		}
		if existingParticipant.State() != livekit.ParticipantInfo_ACTIVE {
			// not fully joined. don't subscribe yet
			continue
		}

		logger.Debugw("subscribing to new track",
			"source", participant.Identity(),
			"remoteTrack", track.ID(),
			"dest", existingParticipant.Identity())
		if err := track.AddSubscriber(existingParticipant); err != nil {
			logger.Errorw("could not subscribe to remoteTrack",
				"source", participant.Identity(),
				"remoteTrack", track.ID(),
				"dest", existingParticipant.Identity())
		}
	}

	if r.onParticipantChanged != nil {
		r.onParticipantChanged(participant)
	}
}

func (r *Room) onTrackUpdated(p types.Participant, track types.PublishedTrack) {
	// send track updates to everyone, especially if track was updated by admin
	r.broadcastParticipantState(p, false)
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onParticipantMetadataUpdate(p types.Participant) {
	r.broadcastParticipantState(p, false)
	if r.onParticipantChanged != nil {
		r.onParticipantChanged(p)
	}
}

func (r *Room) onDataPacket(source types.Participant, dp *livekit.DataPacket) {
	for _, op := range r.GetParticipants() {
		if op.State() != livekit.ParticipantInfo_ACTIVE {
			continue
		}
		if op.ID() == source.ID() {
			continue
		}
		_ = op.SendDataPacket(dp)
	}
}

func (r *Room) subscribeToExistingTracks(p types.Participant) {
	tracksAdded := 0
	for _, op := range r.GetParticipants() {
		if p.ID() == op.ID() {
			// don't send to itself
			continue
		}
		if n, err := op.AddSubscriber(p); err != nil {
			// TODO: log error? or disconnect?
			logger.Errorw("could not subscribe to participant",
				"dest", p.Identity(),
				"source", op.Identity())
		} else {
			tracksAdded += n
		}
	}
	if tracksAdded > 0 {
		logger.Debugw("subscribed participants to existing tracks", "tracks", tracksAdded)
	}
}

// broadcast an update about participant p
func (r *Room) broadcastParticipantState(p types.Participant, skipSource bool) {
	updates := ToProtoParticipants([]types.Participant{p})
	participants := r.GetParticipants()
	for _, op := range participants {
		// skip itself && closed participants
		if (skipSource && p.ID() == op.ID()) || op.State() == livekit.ParticipantInfo_DISCONNECTED {
			continue
		}

		err := op.SendParticipantUpdate(updates)
		if err != nil {
			logger.Errorw("could not send update to participant",
				"participant", p.Identity(),
				"err", err)
		}
	}
}

func (r *Room) sendSpeakerUpdates(speakers []*livekit.SpeakerInfo) {
	dp := &livekit.DataPacket{
		Kind: livekit.DataPacket_LOSSY,
		Value: &livekit.DataPacket_Speaker{
			Speaker: &livekit.ActiveSpeakerUpdate{
				Speakers: speakers,
			},
		},
	}

	for _, p := range r.GetParticipants() {
		if p.ProtocolVersion().HandlesDataPackets() {
			_ = p.SendDataPacket(dp)
		} else {
			_ = p.SendActiveSpeakers(speakers)
		}
	}
}

func (r *Room) audioUpdateWorker() {
	for {
		if r.isClosed.Get() {
			return
		}

		speakers := r.GetActiveSpeakers()

		// see if an update is needed
		if len(speakers) == len(r.lastActiveSpeakers) {
			for i, speaker := range speakers {
				if speaker.Sid != r.lastActiveSpeakers[i].Sid {
					r.sendSpeakerUpdates(speakers)
					break
				}
			}
		} else {
			r.sendSpeakerUpdates(speakers)
		}

		r.lastActiveSpeakers = speakers

		time.Sleep(time.Duration(r.audioUpdateInterval) * time.Millisecond)
	}
}

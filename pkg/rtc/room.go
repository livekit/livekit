package rtc

import (
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/proto/livekit"
)

type Room struct {
	livekit.Room
	config WebRTCConfig
	lock   sync.RWMutex
	// map of peerId -> WebRTCPeer
	peers map[string]*WebRTCPeer
	// Client ID => list of tracks they are publishing
	//tracks map[string][]PeerTrack
}

func NewRoomForRequest(req *livekit.CreateRoomRequest, config *WebRTCConfig) (*Room, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}

	if req.RoomId == "" {
		return nil, ErrRoomIdMissing
	}

	return &Room{
		Room: livekit.Room{
			RoomId:          req.RoomId,
			EmptyTimeout:    req.EmptyTimeout,
			MaxParticipants: req.MaxParticipants,
			CreationTime:    time.Now().Unix(),
			Token:           id.String(),
		},
		config: *config,
		lock:   sync.RWMutex{},
		peers:  make(map[string]*WebRTCPeer),
	}, nil
}

func (r *Room) ToRoomInfo(node *livekit.Node) *livekit.RoomInfo {
	return &livekit.RoomInfo{
		RoomId:       r.RoomId,
		NodeIp:       node.Ip,
		NodeRtcPort:  node.RtcPort,
		CreationTime: r.CreationTime,
		Token:        r.Token,
	}
}

func (r *Room) Join(peerId string, sdp string) (peer *WebRTCPeer, err error) {
	r.lock.Lock()
	defer r.lock.Unlock()

	_, ok := r.peers[peerId]

	if ok {
		// already exists, return error
		return nil, ErrPeerExists
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}

	me := MediaEngine{}
	if err := me.PopulateFromSDP(offer); err != nil {
		return nil, errors.Wrapf(err, "could not parse SDP")
	}

	peer, err = NewWebRTCPeer(peerId, &me, r.config)
	if err != nil {
		return nil, errors.Wrap(err, "could not create peer")
	}
	peer.OnPeerTrack = r.onTrackAdded

	r.peers[peerId] = peer

	// subscribe peer to existing tracks
	for _, p := range r.peers {
		if err := p.AddSubscriber(peer); err != nil {
			// TODO: log error? or disconnect?
			logger.GetLogger().Errorw("could not subscribe to peer",
				"dstPeer", peer.ID(),
				"srcPeer", p.ID())
		}
	}

	return
}

func (r *Room) RemovePeer(peerId string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if p, ok := r.peers[peerId]; ok {
		// also stop connection if needed
		p.Close()
	}

	delete(r.peers, peerId)
}

// a peer in the room added a new track, subscribe other peers to it
func (r *Room) onTrackAdded(peer *WebRTCPeer, track *PeerTrack) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	// subscribe all existing peers to this track
	for _, p := range r.peers {
		if p == peer {
			// skip publishing peer
			continue
		}
		if err := track.AddSubscriber(peer); err != nil {
			logger.GetLogger().Errorw("could not subscribe to track",
				"srcPeer", peer.ID(),
				"track", track.id,
				"dstPeer", p.ID())
		}
	}
}

package service

import (
	"encoding/json"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	skipTokenCheck bool
	manager        *rtc.RoomManager
	upgrader       websocket.Upgrader
}

func NewRTCService(conf *config.Config, manager *rtc.RoomManager) *RTCService {
	s := &RTCService{
		manager:  manager,
		upgrader: websocket.Upgrader{},
	}

	if conf.Development {
		s.skipTokenCheck = true
		s.upgrader.CheckOrigin = func(r *http.Request) bool {
			// allow all in dev
			return true
		}
	}

	return s
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomId := r.FormValue("room_id")
	token := r.FormValue("token")
	peerId := r.FormValue("peer_id")
	log := logger.GetLogger()

	log.Infow("new client connected",
		"roomId", roomId,
		"peerId", peerId,
	)

	room := s.manager.GetRoom(roomId)
	if room == nil {
		writeJSONError(w, http.StatusNotFound, "room not found")
		return
	}

	if !s.skipTokenCheck && room.Token != token {
		writeJSONError(w, http.StatusUnauthorized, "invalid room token")
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		logger.GetLogger().Warnw("could not upgrade to WS",
			"err", err,
		)
	}
	conn.SetCloseHandler(func(code int, text string) error {
		log.Infow("websocket closed by remote")
		return nil
	})
	defer func() {
		log.Infow("connection returned")
	}()

	signalConn := NewWSSignalConnection(conn, peerId)
	var peer *rtc.WebRTCPeer

	// read connection and wait for commands

	// TODO: pass in context from WS, so termination of WS would disconnect RTC
	//ctx := context.Background()
	for {
		req, err := signalConn.ReadRequest()
		if err != nil {
			logger.GetLogger().Errorw("error reading WS",
				"err", err,
				"peerId", peerId,
				"roomId", roomId)
			return
		}

		switch msg := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			peer, err = s.handleJoin(signalConn, room, msg.Offer.Sdp)
			defer func() {
				// remove peer from room
				room.RemovePeer(peerId)
			}()
			if err != nil {
				log.Errorw("could not handle join", "err", err, "peerId", peerId)
				return
			}
		case *livekit.SignalRequest_Negotiate:
			if peer == nil {
				log.Errorw("cannot negotiate before peer offer", "peerId", peerId)
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
				return
			}
			err = s.handleNegotiate(signalConn, peer, msg.Negotiate)
			if err != nil {
				log.Errorw("could not handle negotiate", "peerId", peerId, "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle negotiate", err.Error()))
				return
			}
		case *livekit.SignalRequest_Trickle:
			if peer == nil {
				log.Errorw("cannot trickle before peer offer", "peerId", peerId)
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot trickle before peer offer"))
				return
			}

			err = s.handleTrickle(peer, msg.Trickle)
			if err != nil {
				log.Errorw("could not handle trickle", "peerId", peerId, "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle trickle", err.Error()))
				return
			}
		}

	}
}

func (s *RTCService) handleJoin(sc SignalConnection, room *rtc.Room, sdp string) (*rtc.WebRTCPeer, error) {
	log := logger.GetLogger()

	log.Infow("handling join")
	peer, err := room.Join(sc.PeerId(), sdp)
	if err != nil {
		return nil, errors.Wrap(err, "could not join room")
	}

	// TODO: it might be better to return error instead of nil
	peer.OnICECandidate = func(c *webrtc.ICECandidateInit) {
		err = sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: &livekit.Trickle{
					Candidate: c.Candidate,
					// TODO: there are other candidateInit fields that we might want
				},
			},
		})
		if err != nil {
			logger.GetLogger().Errorw("could not send trickle", "err", err)
		}
	}

	// send peer new offer
	peer.OnOffer = func(o webrtc.SessionDescription) {
		err := sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Negotiate{
				Negotiate: ToProtoSessionDescription(o),
			},
		})
		if err != nil {
			logger.GetLogger().Errorw("could not send offer to peer",
				"err", err)
		}
	}

	offer := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  sdp,
	}
	answer, err := peer.Answer(offer)
	if err != nil {
		return nil, errors.Wrap(err, "could not answer offer")
	}

	// finally send answer
	err = sc.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if err != nil {
		return nil, errors.Wrap(err, "could not join")
	}

	return peer, nil
}

func (s *RTCService) handleNegotiate(sc SignalConnection, peer *rtc.WebRTCPeer, neg *livekit.SessionDescription) error {
	if neg.Type == webrtc.SDPTypeOffer.String() {
		offer := FromProtoSessionDescription(neg)
		answer, err := peer.Answer(offer)
		if err != nil {
			return err
		}

		err = sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Negotiate{
				Negotiate: ToProtoSessionDescription(answer),
			},
		})

		if err != nil {
			return err
		}
	} else if neg.Type == webrtc.SDPTypeAnswer.String() {
		answer := FromProtoSessionDescription(neg)
		err := peer.SetRemoteDescription(answer)
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *RTCService) handleTrickle(peer *rtc.WebRTCPeer, trickle *livekit.Trickle) error {
	candidateInit := FromProtoTrickle(trickle)
	if err := peer.AddICECandidate(*candidateInit); err != nil {
		return err
	}

	return nil
}

type errStruct struct {
	StatusCode int    `json:"statusCode"`
	Error      string `json:"error"`
	Message    string `json:"message,omitempty"`
}

func writeJSONError(w http.ResponseWriter, code int, error ...string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(code)

	json.NewEncoder(w).Encode(jsonError(code, error...))
}

func jsonError(code int, error ...string) errStruct {
	es := errStruct{
		StatusCode: code,
	}
	if len(error) > 0 {
		es.Error = error[0]
	}
	if len(error) > 1 {
		es.Message = error[1]
	}
	return es
}

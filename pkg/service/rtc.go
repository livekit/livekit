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
	pName := r.FormValue("name")
	log := logger.GetLogger()

	log.Infow("new client connected",
		"roomId", roomId,
		"participantName", pName,
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

	participant, err := rtc.NewParticipant(s.manager.Config(), pName)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create participant", err.Error())
	}

	// upgrade only once the basics are good to go
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
	signalConn := NewWSSignalConnection(conn, pName)

	// send Join response
	signalConn.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Join{
			Join: &livekit.JoinResponse{
				Participant: participant.ToProto(),
			},
		},
	})

	// read connection and wait for commands
	// TODO: pass in context from WS, so termination of WS would disconnect RTC
	//ctx := context.Background()
	for {
		req, err := signalConn.ReadRequest()
		if err != nil {
			logger.GetLogger().Errorw("error reading WS",
				"err", err,
				"participantName", pName,
				"roomId", roomId)
			return
		}

		switch msg := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			err = s.handleJoin(signalConn, room, participant, msg.Offer.Sdp)
			if err != nil {
				log.Errorw("could not handle join", "err", err, "participant", participant.ID())
				return
			}
			defer func() {
				// remove peer from room upon disconnection
				room.RemoveParticipant(participant.ID())
			}()
		case *livekit.SignalRequest_Negotiate:
			if participant.State() == rtc.ParticipantStateConnecting {
				log.Errorw("cannot negotiate before peer offer", "participant", participant.ID())
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
				return
			}
			err = s.handleNegotiate(signalConn, participant, msg.Negotiate)
			if err != nil {
				log.Errorw("could not handle negotiate", "participant", participant.ID(), "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle negotiate", err.Error()))
				return
			}
		case *livekit.SignalRequest_Trickle:
			if participant.State() == rtc.ParticipantStateConnecting {
				log.Errorw("cannot trickle before peer offer", "participant", participant.ID())
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot trickle before peer offer"))
				return
			}

			err = s.handleTrickle(participant, msg.Trickle)
			if err != nil {
				log.Errorw("could not handle trickle", "participant", participant.ID(), "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle trickle", err.Error()))
				return
			}
		}

	}
}

func (s *RTCService) handleJoin(sc SignalConnection, room *rtc.Room, participant *rtc.Participant, sdp string) error {
	log := logger.GetLogger()

	log.Infow("handling join")
	err := room.Join(participant)
	if err != nil {
		return errors.Wrap(err, "could not join room")
	}

	// TODO: it might be better to return error instead of nil
	participant.OnICECandidate = func(c *webrtc.ICECandidateInit) {
		log.Debugw("sending ICE candidate", "participantId", participant.ID())
		err = sc.WriteResponse(&livekit.SignalResponse{
			Message: &livekit.SignalResponse_Trickle{
				Trickle: &livekit.Trickle{
					Candidate: c.Candidate,
					// TODO: there are other candidateInit fields that we might want
				},
			},
		})
		if err != nil {
			log.Errorw("could not send trickle", "err", err)
		}
	}

	// send peer new offer
	participant.OnOffer = func(o webrtc.SessionDescription) {
		log.Debugw("sending available offer to participant")
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
	answer, err := participant.Answer(offer)
	if err != nil {
		return errors.Wrap(err, "could not answer offer")
	}

	logger.GetLogger().Debugw("answered, writing response")

	// finally send answer
	err = sc.WriteResponse(&livekit.SignalResponse{
		Message: &livekit.SignalResponse_Answer{
			Answer: ToProtoSessionDescription(answer),
		},
	})

	if err != nil {
		return errors.Wrap(err, "could not create answer")
	}

	return nil
}

func (s *RTCService) handleNegotiate(sc SignalConnection, peer *rtc.Participant, neg *livekit.SessionDescription) error {
	logger.GetLogger().Debugw("handling incoming negotiate")
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

func (s *RTCService) handleTrickle(peer *rtc.Participant, trickle *livekit.Trickle) error {
	candidateInit := FromProtoTrickle(trickle)
	logger.GetLogger().Debugw("adding peer candidate", "participantId", peer.ID())
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

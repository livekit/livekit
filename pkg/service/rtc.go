package service

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/pkg/errors"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/proto/livekit"
)

type RTCService struct {
	manager  *rtc.RoomManager
	upgrader websocket.Upgrader
	isDev    bool
}

func NewRTCService(conf *config.Config, manager *rtc.RoomManager) *RTCService {
	s := &RTCService{
		manager:  manager,
		upgrader: websocket.Upgrader{},
		isDev:    conf.Development,
	}

	if s.isDev {
		s.upgrader.CheckOrigin = func(r *http.Request) bool {
			// allow all in dev
			return true
		}
	}

	return s
}

func (s *RTCService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	roomId := r.FormValue("room_id")
	var pName string
	if s.isDev {
		r.FormValue("name")
	} else {
		claims := GetGrants(r.Context())
		// require a claim
		if claims == nil || claims.Video == nil {
			writeJSONError(w, http.StatusUnauthorized, rtc.ErrPermissionDenied.Error())
		}
		pName = claims.Identity
	}
	log := logger.GetLogger()

	onlyName, err := EnsureJoinPermission(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusUnauthorized, err.Error())
		return
	}

	room, err := s.manager.GetRoomWithConstraint(roomId, onlyName)
	if err != nil {
		// TODO: return errors/status correctly
		writeJSONError(w, http.StatusNotFound, err.Error())
		return
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
	signalConn := rtc.NewWSSignalConnection(conn)

	pc, err := rtc.NewPeerConnection(s.manager.Config())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create peerConnection", err.Error())
		return
	}
	participant, err := rtc.NewParticipant(pc, signalConn, pName)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not create participant", err.Error())
		return
	}

	log.Infow("new client connected",
		"roomId", roomId,
		"name", pName,
		"participant", participant.ID(),
	)

	if err := room.Join(participant); err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not join room", err.Error())
		return
	}

	defer func() {
		// remove peer from room upon disconnection
		room.RemoveParticipant(participant.ID())
		participant.Close()
		log.Infow("WS connection closed")
	}()

	// read connection and wait for commands
	//ctx := context.Background()
	for {
		req, err := signalConn.ReadRequest()
		if err == io.EOF {
			// client disconnected from websocket
			return
		} else if err != nil {
			return
		}

		if req == nil {
			continue
		}

		switch msg := req.Message.(type) {
		case *livekit.SignalRequest_Offer:
			err = s.handleOffer(participant, msg.Offer)
			if err != nil {
				log.Errorw("could not handle join", "err", err, "participant", participant.ID())
				return
			}
		case *livekit.SignalRequest_Negotiate:
			if participant.State() == livekit.ParticipantInfo_JOINING {
				log.Errorw("cannot negotiate before peer offer", "participant", participant.ID())
				//conn.WriteJSON(jsonError(http.StatusNotAcceptable, "cannot negotiate before peer offer"))
				return
			}
			sd := rtc.FromProtoSessionDescription(msg.Negotiate)
			err = participant.HandleNegotiate(sd)
			if err != nil {
				log.Errorw("could not handle negotiate", "participant", participant.ID(), "err", err)
				//conn.WriteJSON(
				//	jsonError(http.StatusInternalServerError, "could not handle negotiate", err.Error()))
				return
			}
		case *livekit.SignalRequest_Trickle:
			if participant.State() == livekit.ParticipantInfo_JOINING {
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

func (s *RTCService) handleOffer(participant rtc.Participant, offer *livekit.SessionDescription) error {
	log := logger.GetLogger()

	_, err := participant.Answer(rtc.FromProtoSessionDescription(offer))
	if err != nil {
		return errors.Wrap(err, "could not answer offer")
	}

	log.Debugw("answered client offer")
	return nil
}

func (s *RTCService) handleTrickle(participant rtc.Participant, trickle *livekit.Trickle) error {
	candidateInit := rtc.FromProtoTrickle(trickle)
	logger.GetLogger().Debugw("adding peer candidate", "participant", participant.ID())
	if err := participant.AddICECandidate(candidateInit); err != nil {
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

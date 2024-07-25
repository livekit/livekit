package whep

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/mediatransportutil/pkg/rtcconfig"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"
)

const (
	authorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "
)

type Server struct {
	*http.ServeMux

	sessions map[string]*Session
}

func (s *Server) Start() error {
	s.ctx, s.cancel = context.WithCancel(context.Background())

	logger.Infow("starting WHEP server")

	if onPublish == nil {
		return psrpc.NewErrorf(psrpc.Internal, "no onPublish callback provided")
	}

	s.onPublish = onPublish

	var err error
	s.webRTCConfig, err = rtcconfig.NewWebRTCConfig(&conf.RTCConfig, conf.Development)
	if err != nil {
		return err
	}

	r := http.NewServeMux()
	r.HandleFunc("POST /{room}/{participant}", s.handleNewSession)

	r.HandleFunc("PATCH /{room}/{participant}/{resource_id}", s.handleICE)
	r.HandleFunc("DELETE /{room}/{participant}/{resource_id}", s.handleDeleteSession)

	hs := &http.Server{
		Addr:         fmt.Sprintf(":%d", conf.WHIPPort),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	go func() {
		err := hs.ListenAndServe()
		if err != http.ErrServerClosed {
			logger.Errorw("WHIP server start failed", err)
		}
	}()

	return nil
}

func (s *Server) Stop() {

}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get(authorizationHeader)
	var authToken string

	if authHeader != "" {
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			handleError(w, r, http.StatusUnauthorized, ErrMissingAuthorization)
			return
		}

		authToken = authHeader[len(bearerPrefix):]
	}

	room := r.PathValue("room")
	participant := r.PathValue("participant")

	offer := bytes.Buffer{}

	_, err := io.Copy(&offer, r.Body)
	if err != nil {
		return err
	}

	resourceID := utils.NewGuid(utils.WHIPResourcePrefix)
	session := NewSession(room, participant, authToken, offer)

	answer := session.CreateAnswer()

	s.sessions[resourceID] = session

	go session.Run()

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Location")
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", fmt.Sprintf("/%s/%s/%s", room, participant, resourceID))
	w.Header().Set("ETag", fmt.Sprintf("%08x", crc32.ChecksumIEEE(offer.Bytes())))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(answer))
}

func (s *Server) handleICE(w http.ResponseWriter, r *http.Request) {
	// TODO: trickle ice and ice restart
	w.WriteHeader(http.StatusNotImplemented)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	resourceID := r.PathValue("resource_id")
	if session, ok := s.sessions[resourceID]; ok {
		session.Close()
	}
}

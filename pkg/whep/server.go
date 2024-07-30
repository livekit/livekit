package whep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	"github.com/livekit/psrpc"
)

const (
	authorizationHeader = "Authorization"
	bearerPrefix        = "Bearer "
	accessTokenParam    = "access_token"
)

type Server struct {
	*http.ServeMux

	psrpcClient rpc.WHEPClient
	logger      logger.Logger
}

func NewServer(psrpcClient rpc.WHEPClient) *Server {
	s := &Server{
		psrpcClient: psrpcClient,
		logger:      logger.GetLogger(),
	}
	r := http.NewServeMux()
	r.HandleFunc("POST /{room}/{participant}", s.handleNewSession)

	r.HandleFunc("PATCH /{room}/{participant}/{resource_id}", s.handleICE)
	r.HandleFunc("DELETE /{room}/{participant}/{resource_id}", s.handleDeleteSession)
	s.ServeMux = r

	return s
}

func (s *Server) handleNewSession(w http.ResponseWriter, r *http.Request) {
	authHeader := r.Header.Get(authorizationHeader)
	var authToken string

	if authHeader != "" {
		// the request has already passed authorize middleware so this should not happen
		if !strings.HasPrefix(authHeader, bearerPrefix) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		authToken = authHeader[len(bearerPrefix):]
	} else {
		authToken = r.FormValue(accessTokenParam)
	}

	room := r.PathValue("room")
	participant := r.PathValue("participant")

	offer := bytes.Buffer{}

	_, err := io.Copy(&offer, r.Body)
	if err != nil {
		s.handleError(w, r, err)
		return
	}

	wsUrl := r.FormValue("livekit_url")
	if wsUrl == "" {
		wsUrl = r.Host
	}
	resp, err := s.psrpcClient.StartWHEP(context.Background(), &rpc.StartWHEPRequest{
		Token:       authToken,
		WsUrl:       wsUrl,
		Participant: participant,
		Offer:       offer.String(),
	}, psrpc.WithRequestTimeout(5*time.Second))
	if err != nil {
		s.handleError(w, r, err)
		return
	}

	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Expose-Headers", "Location")
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", fmt.Sprintf("/%s/%s/%s", room, participant, resp.ResourceId))
	w.Header().Set("ETag", fmt.Sprintf("%08x", crc32.ChecksumIEEE(offer.Bytes())))
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(resp.Answer))
}

func (s *Server) handleICE(w http.ResponseWriter, r *http.Request) {
	// TODO: trickle ice and ice restart
	w.WriteHeader(http.StatusNotImplemented)
}

func (s *Server) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	resourceID := r.PathValue("resource_id")
	_, err := s.psrpcClient.DeleteWHEP(context.Background(), resourceID, &rpc.DeleteWHEPRequest{
		ResourceId: resourceID,
	}, psrpc.WithRequestTimeout(5*time.Second))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
}

func (s *Server) handleError(w http.ResponseWriter, r *http.Request, err error) {
	var psrpcErr psrpc.Error
	switch {
	case errors.As(err, &psrpcErr):
		http.Error(w, psrpcErr.Error(), psrpcErr.ToHttp())
	default:
		s.logger.Infow("whep request failed", "error", err, "method", r.Method, "path", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

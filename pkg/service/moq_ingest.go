// Copyright 2026 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package service

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/livekit/protocol/auth"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/protocol/utils/guid"
	"github.com/quic-go/quic-go/quicvarint"
	webtransport "github.com/quic-go/webtransport-go"

	"github.com/livekit/livekit-server/pkg/config"
	"github.com/livekit/livekit-server/pkg/routing"
	"github.com/livekit/livekit-server/pkg/rtc"
	"github.com/livekit/livekit-server/pkg/rtc/types"
)

const moqLiteStreamPublishControl uint64 = 6

type moqIngestRegistryParams struct {
	Config      config.MoQConfig
	Logger      logger.Logger
	RoomManager *RoomManager
}

type moqIngestRegistry struct {
	params moqIngestRegistryParams

	lock     sync.Mutex
	sessions map[string]*moqIngestSession
}

type moqIngestSession struct {
	registry *moqIngestRegistry

	sessionID   string
	resumeToken string
	roomName    livekit.RoomName
	identity    livekit.ParticipantIdentity
	trackName   string
	trackID     livekit.TrackID

	participant types.LocalParticipant
	track       types.MediaTrack
	receiver    *moqIngestReceiver

	lock           sync.Mutex
	generation     uint64
	resumeDeadline time.Time
	expireTimer    *time.Timer
	closed         bool
}

type moqIngestControlResponse struct {
	PublishSessionID string `json:"publish_session_id"`
	ResumeToken      string `json:"resume_token"`
	TrackSID         string `json:"track_sid"`
	NextSequence     uint64 `json:"next_sequence"`
	ResumeDeadlineMs int64  `json:"resume_deadline_ms"`
}

func newMoQIngestRegistry(params moqIngestRegistryParams) *moqIngestRegistry {
	return &moqIngestRegistry{
		params:   params,
		sessions: make(map[string]*moqIngestSession),
	}
}

func (s *MoQService) serveMoQLitePublishSession(
	sess *webtransport.Session,
	r *http.Request,
	roomName livekit.RoomName,
	claims *auth.ClaimGrants,
) {
	params, err := parseMoQIngestPublishParams(r)
	if err != nil {
		_ = sess.CloseWithError(1, err.Error())
		return
	}

	ingestSession, generation, err := s.ingest.Acquire(sess.Context(), roomName, livekit.ParticipantIdentity(claims.Identity), claims, params, r)
	if err != nil {
		_ = sess.CloseWithError(1, err.Error())
		return
	}
	defer ingestSession.ReleaseGeneration(generation)

	if err := writeMoQIngestControl(sess.Context(), sess, s.config.WriteTimeout, ingestSession.ControlResponse()); err != nil {
		s.logger.Debugw("could not write moq ingest control response", "error", err)
		return
	}

	for {
		stream, err := sess.AcceptUniStream(sess.Context())
		if err != nil {
			if sess.Context().Err() == nil {
				s.logger.Debugw("could not accept moq ingest stream", "error", err)
			}
			return
		}
		group, err := readMoQLitePublishGroup(stream, s.config.IngestMaxFrameBytes)
		if err != nil {
			s.logger.Debugw("could not read moq ingest group", "error", err)
			continue
		}
		if !ingestSession.Enqueue(generation, group.sequence, group.payload) {
			s.logger.Debugw("dropped moq ingest group", "trackID", ingestSession.trackID, "sequence", group.sequence)
		}
	}
}

func (r *moqIngestRegistry) Acquire(
	ctx context.Context,
	roomName livekit.RoomName,
	identity livekit.ParticipantIdentity,
	claims *auth.ClaimGrants,
	params moqIngestPublishParams,
	req *http.Request,
) (*moqIngestSession, uint64, error) {
	if params.ResumeToken != "" {
		return r.resume(roomName, identity, params)
	}
	return r.create(ctx, roomName, identity, claims, params, req)
}

func (r *moqIngestRegistry) create(
	ctx context.Context,
	roomName livekit.RoomName,
	identity livekit.ParticipantIdentity,
	claims *auth.ClaimGrants,
	params moqIngestPublishParams,
	req *http.Request,
) (*moqIngestSession, uint64, error) {
	r.lock.Lock()
	if len(r.sessions) >= r.params.Config.IngestMaxResumeSessions {
		r.lock.Unlock()
		return nil, 0, errors.New("too many moq ingest resume sessions")
	}
	r.lock.Unlock()

	connID := livekit.ConnectionID(guid.New("CO_"))
	pi := routing.ParticipantInit{
		Identity:      identity,
		Name:          livekit.ParticipantName(claims.Name),
		AutoSubscribe: false,
		Client:        ParseClientInfo(req),
		Grants:        claims,
		CreateRoom: &livekit.CreateRoomRequest{
			Name:       string(roomName),
			RoomPreset: claims.RoomPreset,
		},
		AdaptiveStream: false,
		DisableICELite: true,
	}
	if pi.Client.Protocol == 0 {
		pi.Client.Protocol = types.CurrentProtocol
	}
	SetRoomConfiguration(pi.CreateRoom, claims.GetRoomConfiguration())

	source := routing.NewNullMessageSource(connID)
	sink := routing.NewNullMessageSink(connID)
	if err := r.params.RoomManager.StartSession(ctx, pi, source, sink, true); err != nil {
		return nil, 0, err
	}

	room := r.params.RoomManager.GetRoom(ctx, roomName)
	if room == nil {
		return nil, 0, ErrRoomNotFound
	}
	participant := room.GetParticipant(identity)
	if participant == nil {
		return nil, 0, ErrParticipantNotFound
	}

	trackID := livekit.TrackID(guid.New(utils.TrackPrefix))
	receiver, err := newMoQIngestReceiver(trackID, params.TrackName, params.Width, params.Height, params.FPS, r.params.Config, r.params.Logger)
	if err != nil {
		_ = participant.Close(false, types.ParticipantCloseReasonPublicationError, false)
		return nil, 0, err
	}

	impl, ok := participant.(*rtc.ParticipantImpl)
	if !ok {
		receiver.Close()
		_ = participant.Close(false, types.ParticipantCloseReasonPublicationError, false)
		return nil, 0, errors.New("moq ingest requires local rtc participant")
	}
	track, err := impl.PublishSyntheticTrack(&livekit.AddTrackRequest{
		Cid:    string(trackID),
		Name:   params.TrackName,
		Type:   livekit.TrackType_VIDEO,
		Width:  params.Width,
		Height: params.Height,
		Source: livekit.TrackSource_CAMERA,
		Stream: "camera",
		Muted:  false,
	}, receiver)
	if err != nil {
		receiver.Close()
		_ = participant.Close(false, types.ParticipantCloseReasonPublicationError, false)
		return nil, 0, err
	}

	resumeToken, err := randomResumeToken()
	if err != nil {
		track.Close(false)
		receiver.Close()
		_ = participant.Close(false, types.ParticipantCloseReasonPublicationError, false)
		return nil, 0, err
	}

	session := &moqIngestSession{
		registry:    r,
		sessionID:   guid.New("MQ_"),
		resumeToken: resumeToken,
		roomName:    roomName,
		identity:    identity,
		trackName:   params.TrackName,
		trackID:     track.ID(),
		participant: participant,
		track:       track,
		receiver:    receiver,
	}

	r.lock.Lock()
	r.sessions[resumeToken] = session
	r.lock.Unlock()

	return session, session.Bind(params.NextSequence), nil
}

func (r *moqIngestRegistry) resume(
	roomName livekit.RoomName,
	identity livekit.ParticipantIdentity,
	params moqIngestPublishParams,
) (*moqIngestSession, uint64, error) {
	r.lock.Lock()
	session := r.sessions[params.ResumeToken]
	r.lock.Unlock()
	if session == nil {
		return nil, 0, errors.New("moq ingest resume token not found")
	}
	if session.roomName != roomName || session.identity != identity || session.trackName != params.TrackName {
		return nil, 0, errors.New("moq ingest resume metadata mismatch")
	}
	if params.TrackID != "" && params.TrackID != session.trackID {
		return nil, 0, errors.New("moq ingest resume track mismatch")
	}
	if !session.CanResume() {
		return nil, 0, errors.New("moq ingest resume window expired")
	}
	return session, session.Bind(params.NextSequence), nil
}

func (s *moqIngestSession) Bind(nextSequence uint64) uint64 {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.expireTimer != nil {
		s.expireTimer.Stop()
		s.expireTimer = nil
	}
	s.generation++
	s.resumeDeadline = time.Time{}
	s.receiver.SetNextSequence(nextSequence)
	return s.generation
}

func (s *moqIngestSession) ReleaseGeneration(generation uint64) {
	s.lock.Lock()
	defer s.lock.Unlock()
	if s.closed || generation != s.generation {
		return
	}
	s.resumeDeadline = time.Now().Add(s.registry.params.Config.IngestResumeGrace)
	if s.expireTimer != nil {
		s.expireTimer.Stop()
	}
	s.expireTimer = time.AfterFunc(s.registry.params.Config.IngestResumeGrace, func() {
		s.Expire()
	})
}

func (s *moqIngestSession) CanResume() bool {
	s.lock.Lock()
	defer s.lock.Unlock()
	return !s.closed && (s.resumeDeadline.IsZero() || time.Now().Before(s.resumeDeadline))
}

func (s *moqIngestSession) Enqueue(generation uint64, sequence uint64, payload []byte) bool {
	s.lock.Lock()
	active := !s.closed && generation == s.generation
	s.lock.Unlock()
	if !active {
		return false
	}
	return s.receiver.EnqueueAccessUnit(sequence, payload)
}

func (s *moqIngestSession) ControlResponse() moqIngestControlResponse {
	s.lock.Lock()
	defer s.lock.Unlock()
	deadline := time.Now().Add(s.registry.params.Config.IngestResumeGrace)
	if !s.resumeDeadline.IsZero() {
		deadline = s.resumeDeadline
	}
	return moqIngestControlResponse{
		PublishSessionID: s.sessionID,
		ResumeToken:      s.resumeToken,
		TrackSID:         string(s.trackID),
		NextSequence:     s.receiver.NextSequence(),
		ResumeDeadlineMs: max(int64(time.Until(deadline)/time.Millisecond), 0),
	}
}

func (s *moqIngestSession) Expire() {
	s.lock.Lock()
	if s.closed || s.resumeDeadline.IsZero() || time.Now().Before(s.resumeDeadline) {
		s.lock.Unlock()
		return
	}
	s.closed = true
	s.lock.Unlock()

	s.registry.lock.Lock()
	if s.registry.sessions[s.resumeToken] == s {
		delete(s.registry.sessions, s.resumeToken)
	}
	s.registry.lock.Unlock()

	s.track.Close(false)
	s.receiver.Close()
	_ = s.participant.Close(false, types.ParticipantCloseReasonSignalSourceClose, false)
}

func writeMoQIngestControl(
	ctx context.Context,
	sess *webtransport.Session,
	timeout time.Duration,
	response moqIngestControlResponse,
) error {
	stream, err := sess.OpenUniStreamSync(ctx)
	if err != nil {
		return err
	}
	defer func() {
		_ = stream.Close()
	}()
	payload, err := json.Marshal(response)
	if err != nil {
		return err
	}
	var data []byte
	data = quicvarint.Append(data, moqLiteStreamPublishControl)
	data = appendMoQLiteMessage(data, payload)
	return writeMoQLiteBytes(stream, timeout, data)
}

func randomResumeToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

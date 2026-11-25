// Copyright 2026 LiveKit, Inc.
// SPDX-License-Identifier: Apache-2.0

package moq

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	"github.com/quic-go/quic-go"
	"github.com/trickstercache/trickster/v2/pkg/moq/codec"
	"github.com/trickstercache/trickster/v2/pkg/moq/message"
	"github.com/trickstercache/trickster/v2/pkg/moq/session"
	"golang.org/x/sync/semaphore"

	"github.com/livekit/livekit-server/pkg/moq/media"
	"github.com/livekit/livekit-server/pkg/rtc"
)

// ALPN identifies the supported native QUIC transport draft.
const ALPN = "moqt-23"

type moqPeer struct {
	conn               *quic.Conn
	path               pathQuality
	lookup             func(context.Context, livekit.RoomName) *rtc.Room
	room               *rtc.Room
	session            *session.Session
	participant        *rtc.ParticipantImpl
	generation         uint64
	mu                 sync.Mutex
	catalog            *session.Request
	uploads            map[livekit.TrackID]*session.Request
	downloads          map[uint16]*dataDownload
	catalogGroup       atomic.Uint64
	inflight           *semaphore.Weighted
	mediaSubscriptions map[livekit.TrackID]livekit.TrackType
}

// Endpoint handles native MoQ sessions for rooms owned by this node.
type Endpoint struct {
	lookup func(context.Context, livekit.RoomName) *rtc.Room
}

func NewEndpoint(lookup func(context.Context, livekit.RoomName) *rtc.Room) *Endpoint {
	return &Endpoint{lookup: lookup}
}

func (e *Endpoint) Serve(ctx context.Context, conn *quic.Conn) {
	c, _ := codec.ForDraft(23)
	peer := &moqPeer{
		conn: conn, lookup: e.lookup, inflight: semaphore.NewWeighted(16 << 20),
		uploads:   make(map[livekit.TrackID]*session.Request),
		downloads: make(map[uint16]*dataDownload),
	}
	sess, err := session.New(session.FromQUIC(conn), session.Options{
		Codec: c, Role: session.RoleServer, Handler: peer, OnSetup: peer.authenticate,
		MaxRequests: 128, HandshakeTimeout: 5 * time.Second,
	})
	if err != nil {
		_ = conn.CloseWithError(quic.ApplicationErrorCode(message.CodeInternalError), "cannot create MoQ session")
		return
	}
	peer.session = sess
	defer peer.Close()
	// A canceled listener also releases sessions whose peers remain connected.
	stop := context.AfterFunc(ctx, peer.Close)
	defer stop()
	if err = sess.Start(ctx); err == nil {
		<-sess.Done()
	}
	peer.mu.Lock()
	p, generation := peer.participant, peer.generation
	peer.mu.Unlock()
	if p != nil {
		p.DetachMoQ(generation)
	}
}

func (p *moqPeer) authenticate(s *session.Session, setup *message.Setup) error {
	invalid := &message.SessionError{Code: message.CodeUnauthorized, Reason: "invalid room attachment"}
	u, err := url.ParseRequestURI(setup.Path)
	if err != nil || u.Path != "/moq" {
		return invalid
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["room"]) != 1 || len(q["sid"]) != 1 || len(q["generation"]) != 1 || len(q["ticket"]) != 1 {
		return invalid
	}
	generation, err := strconv.ParseUint(q.Get("generation"), 10, 64)
	if err != nil {
		return invalid
	}
	room := p.lookup(s.Context(), livekit.RoomName(q.Get("room")))
	if room == nil {
		return invalid
	}
	participant, _ := room.GetParticipantByID(livekit.ParticipantID(q.Get("sid"))).(*rtc.ParticipantImpl)
	if participant == nil {
		return invalid
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if s.Context().Err() != nil || participant.AttachMoQ(generation, q.Get("ticket"), p) != nil {
		return invalid
	}
	p.room, p.participant, p.generation = room, participant, generation
	return nil
}

func (p *moqPeer) Close() { _ = p.session.Close(message.CodeNoError, "room attachment ended") }

func (p *moqPeer) HandleGoAway(s *session.Session, _ *message.GoAway) { p.Close() }

func (p *moqPeer) HandleRequest(r *session.Request) {
	// The session calls handlers only after successful SETUP.
	switch req := r.Message().(type) {
	case *message.Publish:
		if p.ownNamespace(req.Track) {
			name := string(req.Track.Name)
			if sid, ok := strings.CutPrefix(name, "dt/up/"); ok {
				if p.conn.ConnectionState().SupportsDatagrams.Local && p.acceptDataUpload(r, livekit.TrackID(sid)) {
					return
				}
			} else if p.acceptMedia(r, name) {
				return
			}
		}
	case *message.Subscribe:
		// This live-only endpoint cannot satisfy filtered or paused subscriptions.
		if !req.Params.ForwardOr(true) || len(req.Params.RangeFilters) != 0 || req.Params.Has(message.ParamLocationFilter) || req.Params.Has(message.ParamFillParameters) {
			_ = r.Reject(message.RequestNotSupported, "unsupported live subscription parameters", 0)
			return
		}
		if p.ownNamespace(req.Track) && p.participant.CanSubscribe() {
			if string(req.Track.Name) == media.CatalogTrackName {
				p.serveCatalog(r)
				return
			}
			name := string(req.Track.Name)
			if sid, ok := strings.CutPrefix(name, "dt/down/"); ok {
				if p.conn.ConnectionState().SupportsDatagrams.Remote && p.serveData(r, livekit.TrackID(sid)) {
					return
				}
			} else if p.serveMedia(r, name) {
				return
			}
		}
	default:
		_ = r.Reject(message.RequestNotSupported, "unsupported room request", 0)
		return
	}
	_ = r.Reject(message.RequestDoesNotExist, "track unavailable", 0)
}

func (p *moqPeer) ownNamespace(track message.FullTrackName) bool {
	return len(track.Namespace) == 2 && string(track.Namespace[0]) == "livekit" && string(track.Namespace[1]) == string(p.participant.ID())
}

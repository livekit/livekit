package whep

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"

	google_protobuf "google.golang.org/protobuf/types/known/emptypb"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/logger"
	"github.com/livekit/protocol/rpc"
	sdputil "github.com/livekit/protocol/sdp"
	"github.com/livekit/protocol/tracer"
	"github.com/livekit/protocol/utils"
	"github.com/livekit/psrpc"

	lksdk "github.com/livekit/server-sdk-go/v2"
)

var (
	ErrResourceNotFound = psrpc.NewErrorf(psrpc.NotFound, "WHEP resource not found")
)

type SessionParams struct {
	Logger      logger.Logger
	Token       string
	Participant string
	Offer       string
	WsUrl       string
	RtcAPI      *webrtc.API
}

type Session struct {
	params SessionParams

	room              *lksdk.Room
	pc                *webrtc.PeerConnection
	lock              sync.Mutex
	forwardingSenders map[string]*webrtc.RTPSender // RemoteTrack.SID -> Local RTPSender
	availableSenders  []*webrtc.RTPSender
	closed            bool

	onClose func()
}

func NewSession(params SessionParams) *Session {
	return &Session{
		params:            params,
		forwardingSenders: make(map[string]*webrtc.RTPSender),
	}
}

func (s *Session) CreateAnswer() (string, error) {
	pc, err := s.params.RtcAPI.NewPeerConnection(webrtc.Configuration{SDPSemantics: webrtc.SDPSemanticsUnifiedPlan})
	if err != nil {
		return "", err
	}
	s.pc = pc

	offerSdp := webrtc.SessionDescription{
		Type: webrtc.SDPTypeOffer,
		SDP:  s.params.Offer,
	}

	// create track placeholders for forwarding sender
	parsedOffer, err := offerSdp.Unmarshal()
	if err != nil {
		return "", err
	}
	for _, media := range parsedOffer.MediaDescriptions {
		switch media.MediaName.Media {
		case "audio":
			track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
				MimeType:    webrtc.MimeTypeOpus,
				ClockRate:   48000,
				Channels:    2,
				SDPFmtpLine: "minptime=10;useinbandfec=1",
			}, utils.NewGuid(utils.TrackPrefix), s.params.Participant)
			if err != nil {
				return "", err
			}
			if t, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
				return "", err
			} else {
				s.availableSenders = append(s.availableSenders, t.Sender())
			}

		case "video":
			track, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{
				MimeType:  webrtc.MimeTypeVP8,
				ClockRate: 90000,
			}, utils.NewGuid(utils.TrackPrefix), s.params.Participant)
			if err != nil {
				return "", err
			}
			if t, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
				return "", err
			} else {
				s.availableSenders = append(s.availableSenders, t.Sender())
			}

		default:
			continue
		}
	}

	if err = pc.SetRemoteDescription(offerSdp); err != nil {
		return "", err
	}
	gatherCompelte := webrtc.GatheringCompletePromise(pc)

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return "", err
	}
	if err = pc.SetLocalDescription(answer); err != nil {
		return "", err
	}

	room, err := lksdk.ConnectToRoomWithToken(s.params.WsUrl, s.params.Token, &lksdk.RoomCallback{
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackPublished:    s.onTrackPublished,
			OnTrackSubscribed:   s.onTrackSubscribed,
			OnTrackUnsubscribed: s.onTrackUnsubscribed,
		},
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			s.params.Logger.Infow("sdk client disconnected", "reason", reason)
			go s.Close()
		},
	}, lksdk.WithAutoSubscribe(false))
	if err != nil {
		return "", err
	}
	s.room = room

	<-gatherCompelte
	answerSdp := *pc.CurrentLocalDescription()

	pc.OnConnectionStateChange(func(pcs webrtc.PeerConnectionState) {
		if pcs == webrtc.PeerConnectionStateFailed {
			s.params.Logger.Infow("pc connection failed")
			s.Close()
		}

	})
	return answerSdp.SDP, err
}

func (s *Session) OnClose(f func()) {
	s.lock.Lock()
	s.onClose = f
	s.lock.Unlock()
}

func (s *Session) Close() {
	s.lock.Lock()
	if s.closed {
		s.lock.Unlock()
		return
	}
	s.closed = true
	pc := s.pc
	room := s.room
	onClose := s.onClose
	s.pc = nil
	s.room = nil
	s.forwardingSenders = make(map[string]*webrtc.RTPSender)
	s.availableSenders = nil
	s.lock.Unlock()

	if pc != nil {
		pc.Close()
	}

	if room != nil {
		room.Disconnect()
	}

	if onClose != nil {
		onClose()
	}
}

func (s *Session) onTrackPublished(publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if rp.Identity() != s.params.Participant {
		return
	}

	publication.SetSubscribed(true)
}

func (s *Session) onTrackSubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if rp.Identity() != s.params.Participant {
		return
	}

	trackSID := publication.SID()
	s.params.Logger.Debugw("onTrackSubscribed", "trackID", trackSID)
	s.lock.Lock()
	defer s.lock.Unlock()

	for i, sender := range s.availableSenders {
		if sender.Track().Kind() != track.Kind() {
			continue
		}

		localTrack, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, sender.Track().ID(), sender.Track().StreamID())
		if err != nil {
			s.params.Logger.Errorw("failed to create local track", err, "trackID", trackSID)
			return
		}
		if err = sender.ReplaceTrack(localTrack); err != nil {
			s.params.Logger.Errorw("failed to replace track", err, "trackID", trackSID)
			return
		}
		s.forwardingSenders[trackSID] = sender
		s.availableSenders[i] = s.availableSenders[len(s.availableSenders)-1]
		s.availableSenders[len(s.availableSenders)-1] = nil
		s.availableSenders = s.availableSenders[:len(s.availableSenders)-1]

		s.params.Logger.Infow("start forwarding", "trackID", trackSID)
		go s.forward(track, trackSID, rp, localTrack, sender)
		return
	}

	s.params.Logger.Infow("no available sender for track", "trackID", trackSID)
}

func (s *Session) onTrackUnsubscribed(track *webrtc.TrackRemote, publication *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
	if rp.Identity() != s.params.Participant {
		return
	}

	trackSID := publication.SID()
	s.params.Logger.Debugw("onTrackUnSubscribed", "trackID", trackSID)
	s.lock.Lock()
	defer s.lock.Unlock()
	_, ok := s.forwardingSenders[trackSID]
	if !ok {
		return
	}

	delete(s.forwardingSenders, trackSID)
	// TODO: support sender reuse, this requires munged continuous timestamp/sequence
	// s.availableSenders = append(s.availableSenders, sender)
}

func (s *Session) forward(remoteTrack *webrtc.TrackRemote, trakcSID string, rp *lksdk.RemoteParticipant, localTrack *webrtc.TrackLocalStaticRTP, sender *webrtc.RTPSender) {
	done := make(chan struct{})
	defer close(done)
	var rtpPkt rtp.Packet
	buf := make([]byte, 1500)
	for {
		n, _, err := remoteTrack.Read(buf)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.params.Logger.Warnw("read remote track failed", err, "trackID", trakcSID)
			}
			break
		}
		err = rtpPkt.Unmarshal(buf[:n])

		if err != nil {
			continue
		}
		rtpPkt.Header.Extension = false
		rtpPkt.Header.ExtensionProfile = 0
		rtpPkt.Header.Extensions = []rtp.Extension{}
		if err = localTrack.WriteRTP(&rtpPkt); err != nil {
			break
		}
	}

	go func() {
		for {
			select {
			case <-done:
				return
			default:
				rtcpPkts, _, err := sender.ReadRTCP()
				if err != nil {
					if !errors.Is(err, io.EOF) {
						s.params.Logger.Warnw("read forwarding track rtcp failed", err, "trackID", trakcSID)
						return
					}
				}
				for _, pkt := range rtcpPkts {
					if _, ok := pkt.(*rtcp.PictureLossIndication); ok {
						rp.WritePLI(remoteTrack.SSRC())
						break
					}
				}
			}
		}

	}()
}

// TODO: support trickle ICE
func (s *Session) ICETrickle(ctx context.Context, req *rpc.ICETrickleRequest) (*google_protobuf.Empty, error) {
	_, span := tracer.Start(ctx, "WHEPSession.ICETrickle")
	defer span.End()

	return nil, psrpc.Unimplemented
}

func (s *Session) ICERestart(ctx context.Context, req *rpc.ICERestartWHIPResourceRequest) (*rpc.ICERestartWHIPResourceResponse, error) {
	_, span := tracer.Start(ctx, "WHEPSession.ICERestart")
	defer span.End()

	s.lock.Lock()
	if s.closed {
		s.lock.Unlock()
		return nil, ErrResourceNotFound
	}
	pc := s.pc
	s.lock.Unlock()

	if pc == nil {
		return nil, ErrResourceNotFound
	}

	remoteDescription := pc.CurrentRemoteDescription()
	if remoteDescription == nil {
		return nil, ErrResourceNotFound
	}

	// Replace the current remote description with the values from remote
	newRemoteDescription, err := sdputil.ReplaceICEDetails(remoteDescription.SDP, req.UserFragment, req.Password)
	if err != nil {
		s.params.Logger.Errorw("RepalceICEDetails failed", err, "ufrag", req.UserFragment, "candidates", req.Candidates)
		return nil, psrpc.Internal
	}
	remoteDescription.SDP = newRemoteDescription

	if err := pc.SetRemoteDescription(*remoteDescription); err != nil {
		s.params.Logger.Errorw("SetRemoteDescription failed", err, "sdp", remoteDescription.SDP)
		return nil, psrpc.Internal
	}

	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		s.params.Logger.Errorw("CreateAnswer failed", err)
		return nil, psrpc.Internal
	}

	gatherComplete := webrtc.GatheringCompletePromise(pc)
	if err = pc.SetLocalDescription(answer); err != nil {
		s.params.Logger.Errorw("SetLocalDescription failed", err)
		return nil, psrpc.Internal
	}
	<-gatherComplete

	// Discard all `a=` lines that aren't ICE related
	// "WHIP does not support renegotiation of non-ICE related SDP information"
	//
	// https://www.ietf.org/archive/id/draft-ietf-wish-whip-14.html#name-ice-restarts
	var trickleIceSdpfrag strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(pc.LocalDescription().SDP))
	for scanner.Scan() {
		l := scanner.Text()
		if strings.HasPrefix(l, "a=") && !strings.HasPrefix(l, "a=ice-pwd") && !strings.HasPrefix(l, "a=ice-ufrag") && !strings.HasPrefix(l, "a=candidate") {
			continue
		}

		trickleIceSdpfrag.WriteString(l + "\n")
	}

	return &rpc.ICERestartWHIPResourceResponse{TrickleIceSdpfrag: trickleIceSdpfrag.String()}, nil
}

func (s *Session) DeleteWHEP(ctx context.Context, req *rpc.DeleteWHEPRequest) (*google_protobuf.Empty, error) {
	_, span := tracer.Start(ctx, "WHEPSession.DeleteWHEP")
	defer span.End()

	s.params.Logger.Debugw("DeleteWHEP")
	s.Close()

	return nil, nil
}

package sfu

import (
	"sync"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"

	"github.com/pion/ion-sfu/pkg/buffer"
	"github.com/pion/ion-sfu/pkg/stats"
	"github.com/pion/ion-sfu/pkg/twcc"
)

// Router defines a track rtp/rtcp Router
type Router interface {
	ID() string
	AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, trackID, streamID string) (Receiver, bool)
	AddDownTracks(s *Subscriber, r Receiver) error
	SetRTCPWriter(func([]rtcp.Packet) error)
	AddDownTrack(s *Subscriber, r Receiver) (*DownTrack, error)
	Stop()
}

// RouterConfig defines Router configurations
type RouterConfig struct {
	WithStats           bool            `mapstructure:"withstats"`
	MaxBandwidth        uint64          `mapstructure:"maxbandwidth"`
	MaxPacketTrack      int             `mapstructure:"maxpackettrack"`
	AudioLevelInterval  int             `mapstructure:"audiolevelinterval"`
	AudioLevelThreshold uint8           `mapstructure:"audiolevelthreshold"`
	AudioLevelFilter    int             `mapstructure:"audiolevelfilter"`
	Simulcast           SimulcastConfig `mapstructure:"simulcast"`
	PliThrottle         int64           `mapstructure:"plithrottle"`
}

type router struct {
	sync.RWMutex
	id            string
	twcc          *twcc.Responder
	stats         map[uint32]*stats.Stream
	rtcpCh        chan []rtcp.Packet
	stopCh        chan struct{}
	config        RouterConfig
	session       Session
	receivers     map[string]Receiver
	bufferFactory *buffer.Factory
	writeRTCP     func([]rtcp.Packet) error
}

// newRouter for routing rtp/rtcp packets
func newRouter(id string, session Session, config *WebRTCTransportConfig) Router {
	ch := make(chan []rtcp.Packet, 10)
	r := &router{
		id:            id,
		rtcpCh:        ch,
		stopCh:        make(chan struct{}),
		config:        config.Router,
		session:       session,
		receivers:     make(map[string]Receiver),
		stats:         make(map[uint32]*stats.Stream),
		bufferFactory: config.BufferFactory,
	}

	if config.Router.WithStats {
		stats.Peers.Inc()
	}

	return r
}

func (r *router) ID() string {
	return r.id
}

func (r *router) Stop() {
	r.stopCh <- struct{}{}

	if r.config.WithStats {
		stats.Peers.Dec()
	}
}

func (r *router) AddReceiver(receiver *webrtc.RTPReceiver, track *webrtc.TrackRemote, trackID, streamID string) (Receiver, bool) {
	r.Lock()
	defer r.Unlock()

	publish := false

	buff, rtcpReader := r.bufferFactory.GetBufferPair(uint32(track.SSRC()))

	buff.OnFeedback(func(fb []rtcp.Packet) {
		r.rtcpCh <- fb
	})

	if track.Kind() == webrtc.RTPCodecTypeAudio {
		buff.OnAudioLevel(func(level uint8) {
			r.session.AudioObserver().observe(streamID, level)
		})
		r.session.AudioObserver().addStream(streamID)

	} else if track.Kind() == webrtc.RTPCodecTypeVideo {
		if r.twcc == nil {
			r.twcc = twcc.NewTransportWideCCResponder(uint32(track.SSRC()))
			r.twcc.OnFeedback(func(p rtcp.RawPacket) {
				r.rtcpCh <- []rtcp.Packet{&p}
			})
		}
		buff.OnTransportWideCC(func(sn uint16, timeNS int64, marker bool) {
			r.twcc.Push(sn, timeNS, marker)
		})
	}

	if r.config.WithStats {
		r.stats[uint32(track.SSRC())] = stats.NewStream(buff)
	}

	rtcpReader.OnPacket(func(bytes []byte) {
		pkts, err := rtcp.Unmarshal(bytes)
		if err != nil {
			Logger.Error(err, "Unmarshal rtcp receiver packets err")
			return
		}
		for _, pkt := range pkts {
			switch pkt := pkt.(type) {
			case *rtcp.SourceDescription:
				if r.config.WithStats {
					for _, chunk := range pkt.Chunks {
						if s, ok := r.stats[chunk.Source]; ok {
							for _, item := range chunk.Items {
								if item.Type == rtcp.SDESCNAME {
									s.SetCName(item.Text)
								}
							}
						}
					}
				}
			case *rtcp.SenderReport:
				buff.SetSenderReportData(pkt.RTPTime, pkt.NTPTime)
				if r.config.WithStats {
					if st := r.stats[pkt.SSRC]; st != nil {
						r.updateStats(st)
					}
				}
			}
		}
	})

	recv, ok := r.receivers[trackID]
	if !ok {
		recv = NewWebRTCReceiver(receiver, track, r.id, WithPliThrottle(r.config.PliThrottle*1e6))
		r.receivers[trackID] = recv
		recv.SetRTCPCh(r.rtcpCh)
		recv.OnCloseHandler(func() {
			if r.config.WithStats {
				if track.Kind() == webrtc.RTPCodecTypeVideo {
					stats.VideoTracks.Dec()
				} else {
					stats.AudioTracks.Dec()
				}
			}
			if recv.Kind() == webrtc.RTPCodecTypeAudio {
				r.session.AudioObserver().removeStream(track.StreamID())
			}
			r.deleteReceiver(trackID, uint32(track.SSRC()))
		})
		publish = true
	}

	recv.AddUpTrack(track, buff, r.config.Simulcast.BestQualityFirst)

	buff.Bind(receiver.GetParameters(), buffer.Options{
		MaxBitRate: r.config.MaxBandwidth,
	})

	if r.config.WithStats {
		if track.Kind() == webrtc.RTPCodecTypeVideo {
			stats.VideoTracks.Inc()
		} else {
			stats.AudioTracks.Inc()
		}
	}

	return recv, publish
}

func (r *router) AddDownTracks(s *Subscriber, recv Receiver) error {
	r.Lock()
	defer r.Unlock()

	if s.noAutoSubscribe {
		Logger.Info("peer turns off automatic subscription, skip tracks add")
		return nil
	}

	if recv != nil {
		if _, err := r.AddDownTrack(s, recv); err != nil {
			return err
		}
		s.negotiate()
		return nil
	}

	if len(r.receivers) > 0 {
		for _, rcv := range r.receivers {
			if _, err := r.AddDownTrack(s, rcv); err != nil {
				return err
			}
		}
		s.negotiate()
	}
	return nil
}

func (r *router) SetRTCPWriter(fn func(packet []rtcp.Packet) error) {
	r.writeRTCP = fn
	go r.sendRTCP()
}

func (r *router) AddDownTrack(sub *Subscriber, recv Receiver) (*DownTrack, error) {
	for _, dt := range sub.GetDownTracks(recv.StreamID()) {
		if dt.ID() == recv.TrackID() {
			return dt, nil
		}
	}

	codec := recv.Codec()
	if err := sub.me.RegisterCodec(codec, recv.Kind()); err != nil {
		return nil, err
	}

	downTrack, err := NewDownTrack(webrtc.RTPCodecCapability{
		MimeType:     codec.MimeType,
		ClockRate:    codec.ClockRate,
		Channels:     codec.Channels,
		SDPFmtpLine:  codec.SDPFmtpLine,
		RTCPFeedback: []webrtc.RTCPFeedback{{"goog-remb", ""}, {"nack", ""}, {"nack", "pli"}},
	}, recv, r.bufferFactory, sub.id, r.config.MaxPacketTrack)
	if err != nil {
		return nil, err
	}
	// Create webrtc sender for the peer we are sending track to
	if downTrack.transceiver, err = sub.pc.AddTransceiverFromTrack(downTrack, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionSendonly,
	}); err != nil {
		return nil, err
	}

	// nolint:scopelint
	downTrack.OnCloseHandler(func() {
		if sub.pc.ConnectionState() != webrtc.PeerConnectionStateClosed {
			if err := sub.pc.RemoveTrack(downTrack.transceiver.Sender()); err != nil {
				if err == webrtc.ErrConnectionClosed {
					return
				}
				Logger.Error(err, "Error closing down track")
			} else {
				sub.RemoveDownTrack(recv.StreamID(), downTrack)
				sub.negotiate()
			}
		}
	})

	downTrack.OnBind(func() {
		go sub.sendStreamDownTracksReports(recv.StreamID())
	})

	sub.AddDownTrack(recv.StreamID(), downTrack)
	recv.AddDownTrack(downTrack, r.config.Simulcast.BestQualityFirst)
	return downTrack, nil
}

func (r *router) deleteReceiver(track string, ssrc uint32) {
	r.Lock()
	delete(r.receivers, track)
	delete(r.stats, ssrc)
	r.Unlock()
}

func (r *router) sendRTCP() {
	for {
		select {
		case pkts := <-r.rtcpCh:
			if err := r.writeRTCP(pkts); err != nil {
				Logger.Error(err, "Write rtcp to peer err", "peer_id", r.id)
			}
		case <-r.stopCh:
			return
		}
	}
}

func (r *router) updateStats(stream *stats.Stream) {
	calculateLatestMinMaxSenderNtpTime := func(cname string) (minPacketNtpTimeInMillisSinceSenderEpoch uint64, maxPacketNtpTimeInMillisSinceSenderEpoch uint64) {
		if len(cname) < 1 {
			return
		}
		r.RLock()
		defer r.RUnlock()

		for _, s := range r.stats {
			if s.GetCName() != cname {
				continue
			}

			clockRate := s.Buffer.GetClockRate()
			srrtp, srntp, _ := s.Buffer.GetSenderReportData()
			latestTimestamp, _ := s.Buffer.GetLatestTimestamp()

			fastForwardTimestampInClockRate := fastForwardTimestampAmount(latestTimestamp, srrtp)
			fastForwardTimestampInMillis := (fastForwardTimestampInClockRate * 1000) / clockRate
			latestPacketNtpTimeInMillisSinceSenderEpoch := ntpToMillisSinceEpoch(srntp) + uint64(fastForwardTimestampInMillis)

			if 0 == minPacketNtpTimeInMillisSinceSenderEpoch || latestPacketNtpTimeInMillisSinceSenderEpoch < minPacketNtpTimeInMillisSinceSenderEpoch {
				minPacketNtpTimeInMillisSinceSenderEpoch = latestPacketNtpTimeInMillisSinceSenderEpoch
			}
			if 0 == maxPacketNtpTimeInMillisSinceSenderEpoch || latestPacketNtpTimeInMillisSinceSenderEpoch > maxPacketNtpTimeInMillisSinceSenderEpoch {
				maxPacketNtpTimeInMillisSinceSenderEpoch = latestPacketNtpTimeInMillisSinceSenderEpoch
			}
		}
		return minPacketNtpTimeInMillisSinceSenderEpoch, maxPacketNtpTimeInMillisSinceSenderEpoch
	}

	setDrift := func(cname string, driftInMillis uint64) {
		if len(cname) < 1 {
			return
		}
		r.RLock()
		defer r.RUnlock()

		for _, s := range r.stats {
			if s.GetCName() != cname {
				continue
			}
			s.SetDriftInMillis(driftInMillis)
		}
	}

	cname := stream.GetCName()

	minPacketNtpTimeInMillisSinceSenderEpoch, maxPacketNtpTimeInMillisSinceSenderEpoch := calculateLatestMinMaxSenderNtpTime(cname)

	driftInMillis := maxPacketNtpTimeInMillisSinceSenderEpoch - minPacketNtpTimeInMillisSinceSenderEpoch

	setDrift(cname, driftInMillis)

	stream.CalcStats()
}

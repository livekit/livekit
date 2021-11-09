package sfu

import (
	"sync"
	"testing"
	"time"

	"github.com/lucsky/cuid"

	"github.com/pion/webrtc/v3"
	med "github.com/pion/webrtc/v3/pkg/media"
	"github.com/stretchr/testify/assert"

	"github.com/livekit/livekit-server/pkg/sfu/logger"
)

// Init test helpers

func signalPair(pcOffer *webrtc.PeerConnection, pcAnswer *webrtc.PeerConnection) error {
	offer, err := pcOffer.CreateOffer(nil)
	if err != nil {
		return err
	}
	gatherComplete := webrtc.GatheringCompletePromise(pcOffer)
	if err = pcOffer.SetLocalDescription(offer); err != nil {
		return err
	}
	<-gatherComplete
	if err = pcAnswer.SetRemoteDescription(*pcOffer.LocalDescription()); err != nil {
		return err
	}

	answer, err := pcAnswer.CreateAnswer(nil)
	if err != nil {
		return err
	}
	if err = pcAnswer.SetLocalDescription(answer); err != nil {
		return err
	}
	return pcOffer.SetRemoteDescription(*pcAnswer.LocalDescription())
}

func sendRTPUntilDone(start, done <-chan struct{}, t *testing.T, track *webrtc.TrackLocalStaticSample) {
	<-start
	for {
		select {
		case <-time.After(20 * time.Millisecond):
			assert.NoError(t, track.WriteSample(med.Sample{Data: []byte{0x0, 0xff, 0xff, 0xff, 0xff}, Duration: time.Second}))
		case <-done:
			return
		}
	}
}

// newPair creates two new peer connections (an offerer and an answerer) using
// the api.
func newPair(cfg webrtc.Configuration, api *webrtc.API) (pcOffer *webrtc.PeerConnection, pcAnswer *webrtc.PeerConnection, err error) {
	pca, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, nil, err
	}

	pcb, err := api.NewPeerConnection(cfg)
	if err != nil {
		return nil, nil, err
	}

	return pca, pcb, nil
}

type media struct {
	kind string
	id   string
	tid  string
}

type action struct {
	id    string
	kind  string
	sleep time.Duration
	media []media
}

type peer struct {
	id        string
	mu        sync.Mutex
	local     *PeerLocal
	remotePub *webrtc.PeerConnection
	remoteSub *webrtc.PeerConnection
	subs      sync.WaitGroup
	pubs      []*sender
}

type step struct {
	actions []*action
}

type sender struct {
	transceiver *webrtc.RTPTransceiver
	start       chan struct{}
}

func addMedia(done <-chan struct{}, t *testing.T, pc *webrtc.PeerConnection, media []media) []*sender {
	var senders []*sender
	for _, media := range media {
		var track *webrtc.TrackLocalStaticSample
		var err error

		start := make(chan struct{})

		switch media.kind {
		case "audio":
			track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, media.tid, media.id)
			assert.NoError(t, err)
			transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			})
			assert.NoError(t, err)
			senders = append(senders, &sender{transceiver: transceiver, start: start})
		case "video":
			track, err = webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/vp8"}, media.tid, media.id)
			assert.NoError(t, err)
			transceiver, err := pc.AddTransceiverFromTrack(track, webrtc.RTPTransceiverInit{
				Direction: webrtc.RTPTransceiverDirectionSendonly,
			})
			assert.NoError(t, err)
			senders = append(senders, &sender{transceiver: transceiver, start: start})
		}

		go sendRTPUntilDone(start, done, t, track)
	}
	return senders
}

func newTestConfig() Config {
	return Config{
		Router: RouterConfig{MaxPacketTrack: 200},
	}
}

func TestSFU_SessionScenarios(t *testing.T) {
	logger.SetGlobalOptions(logger.GlobalConfig{V: 2}) // 2 - TRACE
	Logger = logger.New()
	config := newTestConfig()
	sfu := NewSFU(config)
	sfu.NewDatachannel(APIChannelLabel)
	tests := []struct {
		name  string
		steps []step
	}{
		{
			name: "Multiple stream publish",
			steps: []step{
				{
					actions: []*action{{
						id:   "remote1",
						kind: "join",
					}, {
						id:   "remote2",
						kind: "join",
					}},
				},
				{
					actions: []*action{{
						id:   "remote1",
						kind: "publish",
						media: []media{
							{kind: "audio", id: "stream1", tid: "audio1"},
							{kind: "video", id: "stream1", tid: "video1"},
						},
					}},
				}, {
					actions: []*action{{
						id:   "remote2",
						kind: "publish",
						media: []media{
							{kind: "audio", id: "stream2", tid: "audio2"},
							{kind: "video", id: "stream2", tid: "video2"},
						},
					}},
				},
				{
					actions: []*action{{
						id:   "remote1",
						kind: "unpublish",
						media: []media{
							{kind: "audio", id: "stream3", tid: "audio3"},
							{kind: "video", id: "stream3", tid: "video3"},
						},
					}},
				},
				{
					actions: []*action{{
						id:   "remote2",
						kind: "unpublish",
						media: []media{
							{kind: "audio", id: "stream1", tid: "audio1"},
							{kind: "video", id: "stream1", tid: "video1"},
						},
					}},
				},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			testDone := atomicBool(0)
			var mu sync.RWMutex
			done := make(chan struct{})
			peers := make(map[string]*peer)

			for _, step := range tt.steps {
				for _, action := range step.actions {
					func() {
						switch action.kind {
						case "join":
							me, _ := getPublisherMediaEngine()
							se := webrtc.SettingEngine{}
							se.DisableMediaEngineCopy(true)
							err := me.RegisterDefaultCodecs()
							assert.NoError(t, err)
							api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(se))
							pub, err := api.NewPeerConnection(webrtc.Configuration{})
							assert.NoError(t, err)
							sub, err := api.NewPeerConnection(webrtc.Configuration{})
							assert.NoError(t, err)
							local := NewPeer(sfu)
							_, err = pub.CreateDataChannel("ion-sfu", nil)
							p := &peer{id: action.id, remotePub: pub, remoteSub: sub, local: local}
							sub.OnTrack(func(track *webrtc.TrackRemote, recv *webrtc.RTPReceiver) {
								mu.Lock()
								p.subs.Done()
								mu.Unlock()
							})

							mu.Lock()
							for id, existing := range peers {
								if id != action.id {
									p.subs.Add(len(existing.pubs))
								}
							}
							peers[action.id] = p
							mu.Unlock()

							p.mu.Lock()
							p.remotePub.OnNegotiationNeeded(func() {
								p.mu.Lock()
								defer p.mu.Unlock()
								o, err := p.remotePub.CreateOffer(nil)
								assert.NoError(t, err)
								err = p.remotePub.SetLocalDescription(o)
								assert.NoError(t, err)
								a, err := p.local.Answer(o)
								assert.NoError(t, err)
								err = p.remotePub.SetRemoteDescription(*a)
								assert.NoError(t, err)
								for _, pub := range p.pubs {
									if pub.start != nil {
										close(pub.start)
										pub.start = nil
									}
								}
							})

							p.local.OnIceCandidate = func(init *webrtc.ICECandidateInit, i int) {
								switch i {
								case subscriber:
									p.remoteSub.AddICECandidate(*init)
								case publisher:
									p.remotePub.AddICECandidate(*init)
								}
							}

							p.local.OnOffer = func(o *webrtc.SessionDescription) {
								if testDone.get() {
									return
								}
								p.mu.Lock()
								defer p.mu.Unlock()
								err := p.remoteSub.SetRemoteDescription(*o)
								assert.NoError(t, err)
								a, err := p.remoteSub.CreateAnswer(nil)
								assert.NoError(t, err)
								err = p.remoteSub.SetLocalDescription(a)
								assert.NoError(t, err)
								go func() {
									if testDone.get() {
										return
									}
									err = p.local.SetRemoteDescription(a)
									assert.NoError(t, err)
								}()
							}

							offer, err := p.remotePub.CreateOffer(nil)
							assert.NoError(t, err)
							gatherComplete := webrtc.GatheringCompletePromise(p.remotePub)
							err = p.remotePub.SetLocalDescription(offer)
							assert.NoError(t, err)
							<-gatherComplete
							err = p.local.Join("test sid", cuid.New())
							assert.NoError(t, err)
							answer, err := p.local.Answer(*p.remotePub.LocalDescription())
							err = p.remotePub.SetRemoteDescription(*answer)
							assert.NoError(t, err)
							p.mu.Unlock()

						case "publish":
							mu.Lock()
							peer := peers[action.id]
							peer.mu.Lock()
							// all other peers should get sub'd
							for id, p := range peers {
								if id != peer.id {
									p.subs.Add(len(action.media))
								}
							}

							peer.pubs = append(peer.pubs, addMedia(done, t, peer.remotePub, action.media)...)
							peer.mu.Unlock()
							mu.Unlock()

						case "unpublish":
							mu.Lock()
							peer := peers[action.id]
							peer.mu.Lock()
							for _, media := range action.media {
								for _, pub := range peer.pubs {
									if pub.transceiver != nil && pub.transceiver.Sender().Track().ID() == media.tid {
										peer.remotePub.RemoveTrack(pub.transceiver.Sender())
										pub.transceiver = nil
									}
								}
							}
							peer.mu.Unlock()
							mu.Unlock()
						}
					}()
					time.Sleep(1 * time.Second)
				}
			}

			for _, p := range peers {
				p.subs.Wait()
			}
			testDone.set(true)
			close(done)

			for _, p := range peers {
				p.mu.Lock()
				p.remotePub.Close()
				p.remoteSub.Close()
				p.local.Close()
				p.mu.Unlock()
			}
		})
	}
}

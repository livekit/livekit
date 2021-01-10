package rtc

import (
	"sync"

	"github.com/pion/webrtc/v3"

	"github.com/livekit/livekit-server/pkg/logger"
	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
	"github.com/livekit/livekit-server/proto/livekit"
)

const (
	dataBufferSize = 50
)

// DataTrack wraps a WebRTC DataChannel to satisfy the PublishedTrack interface
// it shall forward publishedTracks to all of its subscribers
type DataTrack struct {
	id            string
	name          string
	participantId string
	dataChannel   *webrtc.DataChannel
	lock          sync.RWMutex
	once          sync.Once
	msgChan       chan livekit.DataMessage

	// map of target participantId -> DownDataChannel
	subscribers map[string]*DownDataChannel
}

func NewDataTrack(participantId string, dc *webrtc.DataChannel) *DataTrack {
	t := &DataTrack{
		//ctx:           context.Background(),
		id:            utils.NewGuid(utils.TrackPrefix),
		name:          dc.Label(),
		participantId: participantId,
		dataChannel:   dc,
		msgChan:       make(chan livekit.DataMessage, dataBufferSize),
		lock:          sync.RWMutex{},
		subscribers:   make(map[string]*DownDataChannel),
	}

	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		dm := messageFromDataChannelMessage(msg)
		t.msgChan <- dm
	})

	return t
}

func (t *DataTrack) Start() {
	t.once.Do(func() {
		go t.forwardWorker()
	})
}

func (t *DataTrack) ID() string {
	return t.id
}

func (t *DataTrack) Kind() livekit.TrackType {
	return livekit.TrackType_DATA
}

func (t *DataTrack) Name() string {
	return t.name
}

func (t *DataTrack) SetName(name string) {
	t.name = name
}

// DataTrack cannot be muted
func (t *DataTrack) IsMuted() bool {
	return false
}

func (t *DataTrack) AddSubscriber(participant types.Participant) error {
	label := PackDataTrackLabel(t.participantId, t.ID(), t.dataChannel.Label())
	downChannel, err := participant.PeerConnection().CreateDataChannel(label, t.dataChannelOptions())
	if err != nil {
		return err
	}

	sub := &DownDataChannel{
		participantId: participant.ID(),
		dataChannel:   downChannel,
	}

	t.lock.Lock()
	t.subscribers[participant.ID()] = sub
	t.lock.Unlock()

	downChannel.OnClose(func() {
		t.RemoveSubscriber(sub.participantId)
	})
	return nil
}

func (t *DataTrack) RemoveSubscriber(participantId string) {
	t.lock.Lock()
	sub := t.subscribers[participantId]
	delete(t.subscribers, participantId)
	t.lock.Unlock()

	if sub != nil {
		go sub.dataChannel.Close()
	}
}

func (t *DataTrack) RemoveAllSubscribers() {
	t.lock.Lock()
	defer t.lock.Unlock()
	for _, sub := range t.subscribers {
		go sub.dataChannel.Close()
	}
	t.subscribers = make(map[string]*DownDataChannel)
}

func (t *DataTrack) forwardWorker() {
	defer func() {
		t.RemoveAllSubscribers()
	}()

	for {
		msg := <-t.msgChan

		if msg.Value == nil {
			// track closed
			return
		}

		t.lock.RLock()
		for _, sub := range t.subscribers {
			err := sub.SendMessage(msg)
			if err != nil {
				logger.GetLogger().Errorw("could not send data message",
					"err", err,
					"source", t.participantId,
					"dest", sub.participantId)
			}
		}
		t.lock.RUnlock()
	}
}

func (t *DataTrack) dataChannelOptions() *webrtc.DataChannelInit {
	ordered := t.dataChannel.Ordered()
	protocol := t.dataChannel.Protocol()
	negotiated := false

	return &webrtc.DataChannelInit{
		Ordered:           &ordered,
		MaxPacketLifeTime: t.dataChannel.MaxPacketLifeTime(),
		MaxRetransmits:    t.dataChannel.MaxRetransmits(),
		Protocol:          &protocol,
		Negotiated:        &negotiated,
	}
}

type DownDataChannel struct {
	participantId string
	dataChannel   *webrtc.DataChannel
}

func (d *DownDataChannel) SendMessage(msg livekit.DataMessage) error {
	var err error
	switch val := msg.Value.(type) {
	case *livekit.DataMessage_Binary:
		err = d.dataChannel.Send(val.Binary)
	case *livekit.DataMessage_Text:
		err = d.dataChannel.SendText(val.Text)
	}
	return err
}

func messageFromDataChannelMessage(msg webrtc.DataChannelMessage) livekit.DataMessage {
	dm := livekit.DataMessage{}
	if msg.IsString {
		dm.Value = &livekit.DataMessage_Text{
			Text: string(msg.Data),
		}
	} else {
		dm.Value = &livekit.DataMessage_Binary{
			Binary: msg.Data,
		}

	}
	return dm
}

package rtc

import (
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/pion/webrtc/v3"

	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
)

const (
	initialQualityUpdateWait = 10 * time.Second
)

type DynacastQualityParams struct {
	TrackType          livekit.TrackType
	DynacastPauseDelay time.Duration
	Logger             logger.Logger
}

// DybacastQuality manages max subscribed quality of a media track
type DynacastQuality struct {
	params DynacastQualityParams

	// quality level enable/disable
	maxQualityLock               sync.RWMutex
	maxSubscriberQuality         map[livekit.ParticipantID]*types.SubscribedCodecQuality
	maxSubscriberNodeQuality     map[livekit.NodeID][]types.SubscribedCodecQuality
	maxSubscribedQuality         map[string]livekit.VideoQuality // codec mime -> quality
	maxSubscribedQualityDebounce func(func())
	onSubscribedMaxQualityChange func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)
	maxQualityTimer              *time.Timer

	qualityNotifyOpQueue *utils.OpsQueue
}

func NewDynacastQuality(params DynacastQualityParams) *DynacastQuality {
	t := &DynacastQuality{
		params:                       params,
		maxSubscriberQuality:         make(map[livekit.ParticipantID]*types.SubscribedCodecQuality),
		maxSubscriberNodeQuality:     make(map[livekit.NodeID][]types.SubscribedCodecQuality),
		maxSubscribedQuality:         make(map[string]livekit.VideoQuality),
		maxSubscribedQualityDebounce: debounce.New(params.DynacastPauseDelay),
		qualityNotifyOpQueue:         utils.NewOpsQueue(params.Logger, "quality-notify", 100),
	}

	return t
}

func (d *DynacastQuality) Start() {
	d.qualityNotifyOpQueue.Start()
	d.startMaxQualityTimer(false)
}

func (d *DynacastQuality) Restart() {
	d.startMaxQualityTimer(true)
}

func (d *DynacastQuality) Stop() {
	d.stopMaxQualityTimer()
}

func (d *DynacastQuality) Close() {
	d.qualityNotifyOpQueue.Stop()
	d.stopMaxQualityTimer()
}

func (d *DynacastQuality) OnSubscribedMaxQualityChange(f func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)) {
	d.onSubscribedMaxQualityChange = f
}

func (d *DynacastQuality) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, codec webrtc.RTPCodecCapability, quality livekit.VideoQuality) {
	if d.params.TrackType != livekit.TrackType_VIDEO {
		return
	}
	d.params.Logger.Debugw("notifying subscriber max quality", "subscriberID", subscriberID, "codec", codec, "quality", quality)

	if codec.MimeType == "" {
		d.params.Logger.Errorw("codec mime type is empty", nil)
	}

	d.maxQualityLock.Lock()
	if quality == livekit.VideoQuality_OFF {
		_, ok := d.maxSubscriberQuality[subscriberID]
		if !ok {
			d.maxQualityLock.Unlock()
			return
		}

		delete(d.maxSubscriberQuality, subscriberID)
	} else {
		maxQuality, ok := d.maxSubscriberQuality[subscriberID]
		if ok {
			if maxQuality.Quality == quality && maxQuality.CodecMime == codec.MimeType {
				d.maxQualityLock.Unlock()
				return
			}
			maxQuality.CodecMime = codec.MimeType
			maxQuality.Quality = quality
		} else {
			d.maxSubscriberQuality[subscriberID] = &types.SubscribedCodecQuality{
				Quality:   quality,
				CodecMime: codec.MimeType,
			}
		}
	}
	d.maxQualityLock.Unlock()

	go d.UpdateQualityChange(false)
}

func (d *DynacastQuality) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []types.SubscribedCodecQuality) {
	if d.params.TrackType != livekit.TrackType_VIDEO {
		return
	}

	if len(qualities) == 1 && qualities[0].CodecMime == "" {
		// for old version msg don't have codec mime, use first mime type
		d.maxQualityLock.RLock()
		for mime := range d.maxSubscribedQuality {
			qualities[0].CodecMime = mime
			break
		}
		d.maxQualityLock.RUnlock()
	}

	d.maxQualityLock.Lock()
	if len(qualities) == 0 {
		if _, ok := d.maxSubscriberNodeQuality[nodeID]; !ok {
			d.maxQualityLock.Unlock()
			return
		}
		delete(d.maxSubscriberNodeQuality, nodeID)
	} else {
		if maxQualities, ok := d.maxSubscriberNodeQuality[nodeID]; ok {
			var matchCounter int
			for _, quality := range qualities {
				for _, maxQuality := range maxQualities {
					if quality == maxQuality {
						matchCounter++
						break
					}
				}
			}

			if matchCounter == len(qualities) && matchCounter == len(maxQualities) {
				d.maxQualityLock.Unlock()
				return
			}
		}
		d.maxSubscriberNodeQuality[nodeID] = qualities
	}
	d.maxQualityLock.Unlock()

	go d.UpdateQualityChange(false)
}

func (d *DynacastQuality) UpdateQualityChange(force bool) {
	if d.params.TrackType != livekit.TrackType_VIDEO {
		return
	}

	d.maxQualityLock.Lock()
	d.params.Logger.Debugw("updating quality change",
		"force", force,
		"maxSubscriberQuality", d.maxSubscriberQuality,
		"maxSubscriberNodeQuality", d.maxSubscriberNodeQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality)

	maxSubscribedQuality := make(map[string]livekit.VideoQuality, len(d.maxSubscribedQuality))
	var changed bool
	// reset maxSubscribedQuality
	for mime := range d.maxSubscribedQuality {
		maxSubscribedQuality[mime] = livekit.VideoQuality_OFF
	}

	for _, subQuality := range d.maxSubscriberQuality {
		if q, ok := maxSubscribedQuality[subQuality.CodecMime]; ok {
			if q == livekit.VideoQuality_OFF || (subQuality.Quality != livekit.VideoQuality_OFF && subQuality.Quality > q) {
				maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
			}
		} else {
			maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
		}
	}
	for _, subQualities := range d.maxSubscriberNodeQuality {
		for _, subQuality := range subQualities {
			if q, ok := maxSubscribedQuality[subQuality.CodecMime]; ok {
				if q == livekit.VideoQuality_OFF || (subQuality.Quality != livekit.VideoQuality_OFF && subQuality.Quality > q) {
					maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
				}
			} else {
				maxSubscribedQuality[subQuality.CodecMime] = subQuality.Quality
			}
		}
	}

	qualityDowngrades := make(map[string]livekit.VideoQuality, len(d.maxSubscribedQuality))
	noChangeCount := 0
	for mime, q := range maxSubscribedQuality {
		origin, ok := d.maxSubscribedQuality[mime]
		if !ok {
			origin = livekit.VideoQuality_OFF
		}
		if origin != q {
			if q == livekit.VideoQuality_OFF || (origin != livekit.VideoQuality_OFF && origin > q) {
				// quality downgrade (or become off), delay notify to publisher
				qualityDowngrades[mime] = origin
				if force {
					d.maxSubscribedQuality[mime] = q
				}
			} else {
				// quality upgrade, update immediately
				d.maxSubscribedQuality[mime] = q
			}
			changed = true
		} else {
			noChangeCount++
		}
	}
	d.params.Logger.Debugw("updated quality change",
		"changed", changed,
		"maxSubscribedQuality", maxSubscribedQuality,
		"d.maxSubscribedQuality", d.maxSubscribedQuality,
		"qualityDowngrades", qualityDowngrades)

	if !changed && !force {
		d.maxQualityLock.Unlock()
		return
	}

	// if quality downgrade (or become OFF), delay notify to publisher if needed
	if len(qualityDowngrades) > 0 && !force {
		d.maxSubscribedQualityDebounce(func() {
			d.UpdateQualityChange(true)
		})

		// no quality upgrades
		if len(qualityDowngrades)+noChangeCount == len(d.maxSubscribedQuality) {
			d.maxQualityLock.Unlock()
			return
		}
	}

	subscribedCodec := make([]*livekit.SubscribedCodec, 0, len(d.maxSubscribedQuality))
	maxSubscribedQualities := make([]types.SubscribedCodecQuality, 0, len(d.maxSubscribedQuality))
	for mime, maxQuality := range d.maxSubscribedQuality {
		maxSubscribedQualities = append(maxSubscribedQualities, types.SubscribedCodecQuality{
			CodecMime: mime,
			Quality:   maxQuality,
		})

		if maxQuality == livekit.VideoQuality_OFF {
			subscribedCodec = append(subscribedCodec, &livekit.SubscribedCodec{
				Codec: mime,
				Qualities: []*livekit.SubscribedQuality{
					{Quality: livekit.VideoQuality_LOW, Enabled: false},
					{Quality: livekit.VideoQuality_MEDIUM, Enabled: false},
					{Quality: livekit.VideoQuality_HIGH, Enabled: false},
				},
			})
		} else {
			var subscribedQualities []*livekit.SubscribedQuality
			for q := livekit.VideoQuality_LOW; q <= livekit.VideoQuality_HIGH; q++ {
				subscribedQualities = append(subscribedQualities, &livekit.SubscribedQuality{
					Quality: q,
					Enabled: q <= maxQuality,
				})
			}
			subscribedCodec = append(subscribedCodec, &livekit.SubscribedCodec{
				Codec:     mime,
				Qualities: subscribedQualities,
			})
		}
	}
	if d.onSubscribedMaxQualityChange != nil {
		d.params.Logger.Debugw("subscribedMaxQualityChange",
			"subscribedCodec", subscribedCodec,
			"maxSubscribedQualities", maxSubscribedQualities)
		d.qualityNotifyOpQueue.Enqueue(func() {
			d.onSubscribedMaxQualityChange(subscribedCodec, maxSubscribedQualities)
		})
	}
	d.maxQualityLock.Unlock()
}

func (d *DynacastQuality) startMaxQualityTimer(force bool) {
	d.maxQualityLock.Lock()
	defer d.maxQualityLock.Unlock()

	if d.params.TrackType != livekit.TrackType_VIDEO {
		return
	}

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}

	d.maxQualityTimer = time.AfterFunc(initialQualityUpdateWait, func() {
		d.stopMaxQualityTimer()
		d.UpdateQualityChange(force)
	})
}

func (d *DynacastQuality) stopMaxQualityTimer() {
	d.maxQualityLock.Lock()
	defer d.maxQualityLock.Unlock()

	if d.maxQualityTimer != nil {
		d.maxQualityTimer.Stop()
		d.maxQualityTimer = nil
	}
}

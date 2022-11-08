package rtc

import (
	"sync"
	"time"

	"github.com/bep/debounce"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"

	"github.com/livekit/livekit-server/pkg/rtc/types"
	"github.com/livekit/livekit-server/pkg/utils"
)

type DynacastManagerParams struct {
	DynacastPauseDelay time.Duration
	Logger             logger.Logger
}

type DynacastManager struct {
	params DynacastManagerParams

	lock                          sync.RWMutex
	dynacastQuality               map[string]*DynacastQuality // mime type => DynacastQuality
	maxSubscribedQuality          map[string]livekit.VideoQuality
	committedMaxSubscribedQuality map[string]livekit.VideoQuality

	maxSubscribedQualityDebounce func(func())

	qualityNotifyOpQueue *utils.OpsQueue

	isClosed bool

	onSubscribedMaxQualityChange func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)
}

func NewDynacastManager(params DynacastManagerParams) *DynacastManager {
	d := &DynacastManager{
		params:                        params,
		dynacastQuality:               make(map[string]*DynacastQuality),
		maxSubscribedQuality:          make(map[string]livekit.VideoQuality),
		committedMaxSubscribedQuality: make(map[string]livekit.VideoQuality),
		maxSubscribedQualityDebounce:  debounce.New(params.DynacastPauseDelay),
		qualityNotifyOpQueue:          utils.NewOpsQueue(params.Logger, "quality-notify", 100),
	}
	d.qualityNotifyOpQueue.Start()
	return d
}

func (d *DynacastManager) OnSubscribedMaxQualityChange(f func(subscribedQualities []*livekit.SubscribedCodec, maxSubscribedQualities []types.SubscribedCodecQuality)) {
	d.lock.Lock()
	d.onSubscribedMaxQualityChange = f
	d.lock.Unlock()
}

func (d *DynacastManager) AddCodec(mime string) {
	d.getOrCreateDynacastQuality(mime)
}

func (d *DynacastManager) Restart() {
	d.lock.Lock()
	d.committedMaxSubscribedQuality = make(map[string]livekit.VideoQuality)

	dqs := d.getDynacastQualitiesLocked()
	d.lock.Unlock()

	for _, dq := range dqs {
		dq.Restart()
	}
}

func (d *DynacastManager) Close() {
	d.qualityNotifyOpQueue.Stop()

	d.lock.Lock()
	dqs := d.getDynacastQualitiesLocked()
	d.dynacastQuality = make(map[string]*DynacastQuality)

	d.isClosed = true
	d.lock.Unlock()

	for _, dq := range dqs {
		dq.Stop()
	}
}

//
// THere are situations like track unmute or streaming from a sifferent node
// where subscribed quality needs to sent to the provider immediately.
// This bypasses any debouncing and forces a subscribed quality update
// with immediate effect.
//
func (d *DynacastManager) ForceUpdate() {
	d.update(true)
}

//
// It is possible for tracks to be in pending close state. When track
// is waiting to be closed, a node is not streaming a track. This can
// be used to force an update announcing that subscribed quality is OFF,
// i.e. indicating not pulling track any more.
//
func (d *DynacastManager) ForceQuality(quality livekit.VideoQuality) {
	d.lock.Lock()
	defer d.lock.Unlock()

	for mime := range d.committedMaxSubscribedQuality {
		d.committedMaxSubscribedQuality[mime] = quality
	}

	d.enqueueSubscribedQualityChange()
}

func (d *DynacastManager) NotifySubscriberMaxQuality(subscriberID livekit.ParticipantID, mime string, quality livekit.VideoQuality) {
	dq := d.getOrCreateDynacastQuality(mime)
	if dq != nil {
		dq.NotifySubscriberMaxQuality(subscriberID, quality)
	}
}

func (d *DynacastManager) NotifySubscriberNodeMaxQuality(nodeID livekit.NodeID, qualities []types.SubscribedCodecQuality) {
	for _, quality := range qualities {
		dq := d.getOrCreateDynacastQuality(quality.CodecMime)
		if dq != nil {
			dq.NotifySubscriberNodeMaxQuality(nodeID, quality.Quality)
		}
	}
}

func (d *DynacastManager) getOrCreateDynacastQuality(mime string) *DynacastQuality {
	d.lock.Lock()
	defer d.lock.Unlock()

	if d.isClosed {
		return nil
	}

	if dq := d.dynacastQuality[mime]; dq != nil {
		return dq
	}

	dq := NewDynacastQuality(DynacastQualityParams{
		MimeType:           mime,
		DynacastPauseDelay: d.params.DynacastPauseDelay,
		Logger:             d.params.Logger,
	})
	dq.OnSubscribedMaxQualityChange(func(maxQuality livekit.VideoQuality) {
		d.updateMaxQualityForMime(mime, maxQuality)
	})
	dq.Start()

	d.dynacastQuality[mime] = dq

	return dq
}

func (d *DynacastManager) getDynacastQualitiesLocked() []*DynacastQuality {
	dqs := make([]*DynacastQuality, 0, len(d.dynacastQuality))
	for _, dq := range d.dynacastQuality {
		dqs = append(dqs, dq)
	}

	return dqs
}

func (d *DynacastManager) updateMaxQualityForMime(mime string, maxQuality livekit.VideoQuality) {
	d.lock.Lock()
	d.maxSubscribedQuality[mime] = maxQuality
	d.lock.Unlock()

	d.update(false)
}

func (d *DynacastManager) update(force bool) {
	d.lock.Lock()

	d.params.Logger.Debugw("processing quality change",
		"force", force,
		"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
	)

	if len(d.maxSubscribedQuality) == 0 {
		// no mime has been added, nothing to update
		d.lock.Unlock()
		return
	}

	// add or remove of a mime triggers an update
	changed := len(d.maxSubscribedQuality) != len(d.committedMaxSubscribedQuality)
	downgradesOnly := !changed
	if !changed {
		for mime, quality := range d.maxSubscribedQuality {
			if cq, ok := d.committedMaxSubscribedQuality[mime]; ok {
				if cq != quality {
					changed = true
				}

				if (cq == livekit.VideoQuality_OFF && quality != livekit.VideoQuality_OFF) || (cq != livekit.VideoQuality_OFF && quality != livekit.VideoQuality_OFF && cq < quality) {
					downgradesOnly = false
				}
			}
		}
	}

	if !force {
		if !changed {
			d.lock.Unlock()
			return
		}

		if downgradesOnly {
			d.params.Logger.Infow("debouncing quality downgrade",
				"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
				"maxSubscribedQuality", d.maxSubscribedQuality,
			)
			d.maxSubscribedQualityDebounce(func() {
				d.update(true)
			})
			d.lock.Unlock()
			return
		}
	}

	// clear debounce on send
	d.maxSubscribedQualityDebounce(func() {})

	d.params.Logger.Infow("committing quality change",
		"force", force,
		"committedMaxSubscribedQuality", d.committedMaxSubscribedQuality,
		"maxSubscribedQuality", d.maxSubscribedQuality,
	)

	// commit change
	d.committedMaxSubscribedQuality = make(map[string]livekit.VideoQuality, len(d.maxSubscribedQuality))
	for mime, quality := range d.maxSubscribedQuality {
		d.committedMaxSubscribedQuality[mime] = quality
	}

	d.enqueueSubscribedQualityChange()
	d.lock.Unlock()
}

func (d *DynacastManager) enqueueSubscribedQualityChange() {
	if d.isClosed || d.onSubscribedMaxQualityChange == nil {
		return
	}

	subscribedCodecs := make([]*livekit.SubscribedCodec, 0, len(d.committedMaxSubscribedQuality))
	maxSubscribedQualities := make([]types.SubscribedCodecQuality, 0, len(d.committedMaxSubscribedQuality))
	for mime, quality := range d.committedMaxSubscribedQuality {
		maxSubscribedQualities = append(maxSubscribedQualities, types.SubscribedCodecQuality{
			CodecMime: mime,
			Quality:   quality,
		})

		if quality == livekit.VideoQuality_OFF {
			subscribedCodecs = append(subscribedCodecs, &livekit.SubscribedCodec{
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
					Enabled: q <= quality,
				})
			}
			subscribedCodecs = append(subscribedCodecs, &livekit.SubscribedCodec{
				Codec:     mime,
				Qualities: subscribedQualities,
			})
		}
	}

	d.params.Logger.Debugw("subscribedMaxQualityChange",
		"subscribedCodecs", subscribedCodecs,
		"maxSubscribedQualities", maxSubscribedQualities)
	d.qualityNotifyOpQueue.Enqueue(func() {
		d.onSubscribedMaxQualityChange(subscribedCodecs, maxSubscribedQualities)
	})
}

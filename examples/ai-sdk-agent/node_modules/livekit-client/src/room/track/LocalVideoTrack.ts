import { Mutex } from '@livekit/mutex';
import {
  VideoQuality as ProtoVideoQuality,
  SubscribedCodec,
  SubscribedQuality,
  VideoLayer,
} from '@livekit/protocol';
import type { SignalClient } from '../../api/SignalClient';
import type { StructuredLogger } from '../../logger';
import { TrackEvent } from '../events';
import { ScalabilityMode } from '../participant/publishUtils';
import type { VideoSenderStats } from '../stats';
import { computeBitrate, monitorFrequency } from '../stats';
import type { LoggerOptions } from '../types';
import { isFireFox, isMobile, isSVCCodec, isWeb } from '../utils';
import LocalTrack from './LocalTrack';
import { Track, VideoQuality } from './Track';
import type { VideoCaptureOptions, VideoCodec } from './options';
import type { TrackProcessor } from './processor/types';
import { constraintsForOptions } from './utils';

export class SimulcastTrackInfo {
  codec: VideoCodec;

  mediaStreamTrack: MediaStreamTrack;

  sender?: RTCRtpSender;

  encodings?: RTCRtpEncodingParameters[];

  constructor(codec: VideoCodec, mediaStreamTrack: MediaStreamTrack) {
    this.codec = codec;
    this.mediaStreamTrack = mediaStreamTrack;
  }
}

const refreshSubscribedCodecAfterNewCodec = 5000;

export default class LocalVideoTrack extends LocalTrack<Track.Kind.Video> {
  /* @internal */
  signalClient?: SignalClient;

  private prevStats?: Map<string, VideoSenderStats>;

  private encodings?: RTCRtpEncodingParameters[];

  /* @internal */
  simulcastCodecs: Map<VideoCodec, SimulcastTrackInfo> = new Map<VideoCodec, SimulcastTrackInfo>();

  private subscribedCodecs?: SubscribedCodec[];

  // prevents concurrent manipulations to track sender
  // if multiple get/setParameter are called concurrently, certain timing of events
  // could lead to the browser throwing an exception in `setParameter`, due to
  // a missing `getParameter` call.
  private senderLock: Mutex;

  private degradationPreference: RTCDegradationPreference = 'balanced';

  private isCpuConstrained: boolean = false;

  private optimizeForPerformance: boolean = false;

  get sender(): RTCRtpSender | undefined {
    return this._sender;
  }

  set sender(sender: RTCRtpSender | undefined) {
    this._sender = sender;
    if (this.degradationPreference) {
      this.setDegradationPreference(this.degradationPreference);
    }
  }

  /**
   *
   * @param mediaTrack
   * @param constraints MediaTrackConstraints that are being used when restarting or reacquiring tracks
   * @param userProvidedTrack Signals to the SDK whether or not the mediaTrack should be managed (i.e. released and reacquired) internally by the SDK
   */
  constructor(
    mediaTrack: MediaStreamTrack,
    constraints?: MediaTrackConstraints,
    userProvidedTrack = true,
    loggerOptions?: LoggerOptions,
  ) {
    super(mediaTrack, Track.Kind.Video, constraints, userProvidedTrack, loggerOptions);
    this.senderLock = new Mutex();
  }

  get isSimulcast(): boolean {
    if (this.sender && this.sender.getParameters().encodings.length > 1) {
      return true;
    }
    return false;
  }

  /* @internal */
  startMonitor(signalClient: SignalClient) {
    this.signalClient = signalClient;
    if (!isWeb()) {
      return;
    }
    // save original encodings
    // TODO : merge simulcast tracks stats
    const params = this.sender?.getParameters();
    if (params) {
      this.encodings = params.encodings;
    }

    if (this.monitorInterval) {
      return;
    }
    this.monitorInterval = setInterval(() => {
      this.monitorSender();
    }, monitorFrequency);
  }

  stop() {
    this._mediaStreamTrack.getConstraints();
    this.simulcastCodecs.forEach((trackInfo) => {
      trackInfo.mediaStreamTrack.stop();
    });
    super.stop();
  }

  async pauseUpstream() {
    await super.pauseUpstream();
    for await (const sc of this.simulcastCodecs.values()) {
      await sc.sender?.replaceTrack(null);
    }
  }

  async resumeUpstream() {
    await super.resumeUpstream();
    for await (const sc of this.simulcastCodecs.values()) {
      await sc.sender?.replaceTrack(sc.mediaStreamTrack);
    }
  }

  async mute(): Promise<typeof this> {
    const unlock = await this.muteLock.lock();
    try {
      if (this.isMuted) {
        this.log.debug('Track already muted', this.logContext);
        return this;
      }

      if (this.source === Track.Source.Camera && !this.isUserProvided) {
        this.log.debug('stopping camera track', this.logContext);
        // also stop the track, so that camera indicator is turned off
        this._mediaStreamTrack.stop();
      }
      await super.mute();
      return this;
    } finally {
      unlock();
    }
  }

  async unmute(): Promise<typeof this> {
    const unlock = await this.muteLock.lock();
    try {
      if (!this.isMuted) {
        this.log.debug('Track already unmuted', this.logContext);
        return this;
      }

      if (this.source === Track.Source.Camera && !this.isUserProvided) {
        this.log.debug('reacquiring camera track', this.logContext);
        await this.restartTrack();
      }
      await super.unmute();
      return this;
    } finally {
      unlock();
    }
  }

  protected setTrackMuted(muted: boolean) {
    super.setTrackMuted(muted);
    for (const sc of this.simulcastCodecs.values()) {
      sc.mediaStreamTrack.enabled = !muted;
    }
  }

  async getSenderStats(): Promise<VideoSenderStats[]> {
    if (!this.sender?.getStats) {
      return [];
    }

    const items: VideoSenderStats[] = [];

    const stats = await this.sender.getStats();
    stats.forEach((v) => {
      if (v.type === 'outbound-rtp') {
        const vs: VideoSenderStats = {
          type: 'video',
          streamId: v.id,
          frameHeight: v.frameHeight,
          frameWidth: v.frameWidth,
          framesPerSecond: v.framesPerSecond,
          framesSent: v.framesSent,
          firCount: v.firCount,
          pliCount: v.pliCount,
          nackCount: v.nackCount,
          packetsSent: v.packetsSent,
          bytesSent: v.bytesSent,
          qualityLimitationReason: v.qualityLimitationReason,
          qualityLimitationDurations: v.qualityLimitationDurations,
          qualityLimitationResolutionChanges: v.qualityLimitationResolutionChanges,
          rid: v.rid ?? v.id,
          retransmittedPacketsSent: v.retransmittedPacketsSent,
          targetBitrate: v.targetBitrate,
          timestamp: v.timestamp,
        };

        // locate the appropriate remote-inbound-rtp item
        const r = stats.get(v.remoteId);
        if (r) {
          vs.jitter = r.jitter;
          vs.packetsLost = r.packetsLost;
          vs.roundTripTime = r.roundTripTime;
        }

        items.push(vs);
      }
    });

    // make sure highest res layer is always first
    items.sort((a, b) => (b.frameWidth ?? 0) - (a.frameWidth ?? 0));
    return items;
  }

  setPublishingQuality(maxQuality: VideoQuality) {
    const qualities: SubscribedQuality[] = [];
    for (let q = VideoQuality.LOW; q <= VideoQuality.HIGH; q += 1) {
      qualities.push(
        new SubscribedQuality({
          quality: q,
          enabled: q <= maxQuality,
        }),
      );
    }
    this.log.debug(`setting publishing quality. max quality ${maxQuality}`, this.logContext);
    this.setPublishingLayers(isSVCCodec(this.codec), qualities);
  }

  async restartTrack(options?: VideoCaptureOptions) {
    let constraints: MediaTrackConstraints | undefined;
    if (options) {
      const streamConstraints = constraintsForOptions({ video: options });
      if (typeof streamConstraints.video !== 'boolean') {
        constraints = streamConstraints.video;
      }
    }
    await this.restart(constraints);

    // reset cpu constrained state after track is restarted
    this.isCpuConstrained = false;

    for await (const sc of this.simulcastCodecs.values()) {
      if (sc.sender && sc.sender.transport?.state !== 'closed') {
        sc.mediaStreamTrack = this.mediaStreamTrack.clone();
        await sc.sender.replaceTrack(sc.mediaStreamTrack);
      }
    }
  }

  async setProcessor(
    processor: TrackProcessor<Track.Kind.Video>,
    showProcessedStreamLocally = true,
  ) {
    await super.setProcessor(processor, showProcessedStreamLocally);

    if (this.processor?.processedTrack) {
      for await (const sc of this.simulcastCodecs.values()) {
        await sc.sender?.replaceTrack(this.processor.processedTrack);
      }
    }
  }

  async setDegradationPreference(preference: RTCDegradationPreference) {
    this.degradationPreference = preference;
    if (this.sender) {
      try {
        this.log.debug(`setting degradationPreference to ${preference}`, this.logContext);
        const params = this.sender.getParameters();
        params.degradationPreference = preference;
        this.sender.setParameters(params);
      } catch (e: any) {
        this.log.warn(`failed to set degradationPreference`, { error: e, ...this.logContext });
      }
    }
  }

  addSimulcastTrack(
    codec: VideoCodec,
    encodings?: RTCRtpEncodingParameters[],
  ): SimulcastTrackInfo | undefined {
    if (this.simulcastCodecs.has(codec)) {
      this.log.error(`${codec} already added, skipping adding simulcast codec`, this.logContext);
      return;
    }
    const simulcastCodecInfo: SimulcastTrackInfo = {
      codec,
      mediaStreamTrack: this.mediaStreamTrack.clone(),
      sender: undefined,
      encodings,
    };
    this.simulcastCodecs.set(codec, simulcastCodecInfo);
    return simulcastCodecInfo;
  }

  setSimulcastTrackSender(codec: VideoCodec, sender: RTCRtpSender) {
    const simulcastCodecInfo = this.simulcastCodecs.get(codec);
    if (!simulcastCodecInfo) {
      return;
    }
    simulcastCodecInfo.sender = sender;

    // browser will reenable disabled codec/layers after new codec has been published,
    // so refresh subscribedCodecs after publish a new codec
    setTimeout(() => {
      if (this.subscribedCodecs) {
        this.setPublishingCodecs(this.subscribedCodecs);
      }
    }, refreshSubscribedCodecAfterNewCodec);
  }

  /**
   * @internal
   * Sets codecs that should be publishing, returns new codecs that have not yet
   * been published
   */
  async setPublishingCodecs(codecs: SubscribedCodec[]): Promise<VideoCodec[]> {
    this.log.debug('setting publishing codecs', {
      ...this.logContext,
      codecs,
      currentCodec: this.codec,
    });
    // only enable simulcast codec for preference codec setted
    if (!this.codec && codecs.length > 0) {
      await this.setPublishingLayers(isSVCCodec(codecs[0].codec), codecs[0].qualities);

      return [];
    }

    this.subscribedCodecs = codecs;

    const newCodecs: VideoCodec[] = [];
    for await (const codec of codecs) {
      if (!this.codec || this.codec === codec.codec) {
        await this.setPublishingLayers(isSVCCodec(codec.codec), codec.qualities);
      } else {
        const simulcastCodecInfo = this.simulcastCodecs.get(codec.codec as VideoCodec);
        this.log.debug(`try setPublishingCodec for ${codec.codec}`, {
          ...this.logContext,
          simulcastCodecInfo,
        });
        if (!simulcastCodecInfo || !simulcastCodecInfo.sender) {
          for (const q of codec.qualities) {
            if (q.enabled) {
              newCodecs.push(codec.codec as VideoCodec);
              break;
            }
          }
        } else if (simulcastCodecInfo.encodings) {
          this.log.debug(`try setPublishingLayersForSender ${codec.codec}`, this.logContext);
          await setPublishingLayersForSender(
            simulcastCodecInfo.sender,
            simulcastCodecInfo.encodings!,
            codec.qualities,
            this.senderLock,
            isSVCCodec(codec.codec),
            this.log,
            this.logContext,
          );
        }
      }
    }
    return newCodecs;
  }

  /**
   * @internal
   * Sets layers that should be publishing
   */
  async setPublishingLayers(isSvc: boolean, qualities: SubscribedQuality[]) {
    if (this.optimizeForPerformance) {
      this.log.info('skipping setPublishingLayers due to optimized publishing performance', {
        ...this.logContext,
        qualities,
      });
      return;
    }
    this.log.debug('setting publishing layers', { ...this.logContext, qualities });
    if (!this.sender || !this.encodings) {
      return;
    }

    await setPublishingLayersForSender(
      this.sender,
      this.encodings,
      qualities,
      this.senderLock,
      isSvc,
      this.log,
      this.logContext,
    );
  }

  /**
   * Designed for lower powered devices, reduces video publishing quality and disables simulcast.
   * @experimental
   */
  async prioritizePerformance() {
    if (!this.sender) {
      throw new Error('sender not found');
    }

    const unlock = await this.senderLock.lock();

    try {
      this.optimizeForPerformance = true;
      const params = this.sender.getParameters();

      params.encodings = params.encodings.map((e, idx) => ({
        ...e,
        active: idx === 0,
        scaleResolutionDownBy: Math.max(
          1,
          Math.ceil((this.mediaStreamTrack.getSettings().height ?? 360) / 360),
        ),
        scalabilityMode: idx === 0 && isSVCCodec(this.codec) ? 'L1T3' : undefined,
        maxFramerate: idx === 0 ? 15 : 0,
        maxBitrate: idx === 0 ? e.maxBitrate : 0,
      }));
      this.log.debug('setting performance optimised encodings', {
        ...this.logContext,
        encodings: params.encodings,
      });
      this.encodings = params.encodings;
      await this.sender.setParameters(params);
    } catch (e) {
      this.log.error('failed to set performance optimised encodings', {
        ...this.logContext,
        error: e,
      });
      this.optimizeForPerformance = false;
    } finally {
      unlock();
    }
  }

  protected monitorSender = async () => {
    if (!this.sender) {
      this._currentBitrate = 0;
      return;
    }

    let stats: VideoSenderStats[] | undefined;
    try {
      stats = await this.getSenderStats();
    } catch (e) {
      this.log.error('could not get video sender stats', { ...this.logContext, error: e });
      return;
    }
    const statsMap = new Map<string, VideoSenderStats>(stats.map((s) => [s.rid, s]));

    const isCpuConstrained = stats.some((s) => s.qualityLimitationReason === 'cpu');
    if (isCpuConstrained !== this.isCpuConstrained) {
      this.isCpuConstrained = isCpuConstrained;
      if (this.isCpuConstrained) {
        this.emit(TrackEvent.CpuConstrained);
      }
    }

    if (this.prevStats) {
      let totalBitrate = 0;
      statsMap.forEach((s, key) => {
        const prev = this.prevStats?.get(key);
        totalBitrate += computeBitrate(s, prev);
      });
      this._currentBitrate = totalBitrate;
    }

    this.prevStats = statsMap;
  };

  protected async handleAppVisibilityChanged() {
    await super.handleAppVisibilityChanged();
    if (!isMobile()) return;
    if (this.isInBackground && this.source === Track.Source.Camera) {
      this._mediaStreamTrack.enabled = false;
    }
  }
}

async function setPublishingLayersForSender(
  sender: RTCRtpSender,
  senderEncodings: RTCRtpEncodingParameters[],
  qualities: SubscribedQuality[],
  senderLock: Mutex,
  isSVC: boolean,
  log: StructuredLogger,
  logContext: Record<string, unknown>,
) {
  const unlock = await senderLock.lock();
  log.debug('setPublishingLayersForSender', { ...logContext, sender, qualities, senderEncodings });
  try {
    const params = sender.getParameters();
    const { encodings } = params;
    if (!encodings) {
      return;
    }

    if (encodings.length !== senderEncodings.length) {
      log.warn('cannot set publishing layers, encodings mismatch', {
        ...logContext,
        encodings,
        senderEncodings,
      });
      return;
    }

    let hasChanged = false;

    /* disable closable spatial layer as it has video blur / frozen issue with current server / client
    1. chrome 113: when switching to up layer with scalability Mode change, it will generate a
          low resolution frame and recover very quickly, but noticable
    2. livekit sfu: additional pli request cause video frozen for a few frames, also noticable */
    const closableSpatial = false;
    /* @ts-ignore */
    if (closableSpatial && encodings[0].scalabilityMode) {
      // svc dynacast encodings
      const encoding = encodings[0];
      /* @ts-ignore */
      // const mode = new ScalabilityMode(encoding.scalabilityMode);
      let maxQuality = ProtoVideoQuality.OFF;
      qualities.forEach((q) => {
        if (q.enabled && (maxQuality === ProtoVideoQuality.OFF || q.quality > maxQuality)) {
          maxQuality = q.quality;
        }
      });

      if (maxQuality === ProtoVideoQuality.OFF) {
        if (encoding.active) {
          encoding.active = false;
          hasChanged = true;
        }
      } else if (!encoding.active /* || mode.spatial !== maxQuality + 1*/) {
        hasChanged = true;
        encoding.active = true;
        /*
        @ts-ignore
        const originalMode = new ScalabilityMode(senderEncodings[0].scalabilityMode)
        mode.spatial = maxQuality + 1;
        mode.suffix = originalMode.suffix;
        if (mode.spatial === 1) {
          // no suffix for L1Tx
          mode.suffix = undefined;
        }
        @ts-ignore
        encoding.scalabilityMode = mode.toString();
        encoding.scaleResolutionDownBy = 2 ** (2 - maxQuality);
      */
      }
    } else {
      if (isSVC) {
        const hasEnabledEncoding = qualities.some((q) => q.enabled);
        if (hasEnabledEncoding) {
          qualities.forEach((q) => (q.enabled = true));
        }
      }
      // simulcast dynacast encodings
      encodings.forEach((encoding, idx) => {
        let rid = encoding.rid ?? '';
        if (rid === '') {
          rid = 'q';
        }
        const quality = videoQualityForRid(rid);
        const subscribedQuality = qualities.find((q) => q.quality === quality);
        if (!subscribedQuality) {
          return;
        }
        if (encoding.active !== subscribedQuality.enabled) {
          hasChanged = true;
          encoding.active = subscribedQuality.enabled;
          log.debug(
            `setting layer ${subscribedQuality.quality} to ${
              encoding.active ? 'enabled' : 'disabled'
            }`,
            logContext,
          );

          // FireFox does not support setting encoding.active to false, so we
          // have a workaround of lowering its bitrate and resolution to the min.
          if (isFireFox()) {
            if (subscribedQuality.enabled) {
              encoding.scaleResolutionDownBy = senderEncodings[idx].scaleResolutionDownBy;
              encoding.maxBitrate = senderEncodings[idx].maxBitrate;
              /* @ts-ignore */
              encoding.maxFrameRate = senderEncodings[idx].maxFrameRate;
            } else {
              encoding.scaleResolutionDownBy = 4;
              encoding.maxBitrate = 10;
              /* @ts-ignore */
              encoding.maxFrameRate = 2;
            }
          }
        }
      });
    }

    if (hasChanged) {
      params.encodings = encodings;
      log.debug(`setting encodings`, { ...logContext, encodings: params.encodings });
      await sender.setParameters(params);
    }
  } finally {
    unlock();
  }
}

export function videoQualityForRid(rid: string): VideoQuality {
  switch (rid) {
    case 'f':
      return VideoQuality.HIGH;
    case 'h':
      return VideoQuality.MEDIUM;
    case 'q':
      return VideoQuality.LOW;
    default:
      return VideoQuality.HIGH;
  }
}

export function videoLayersFromEncodings(
  width: number,
  height: number,
  encodings?: RTCRtpEncodingParameters[],
  svc?: boolean,
): VideoLayer[] {
  // default to a single layer, HQ
  if (!encodings) {
    return [
      new VideoLayer({
        quality: VideoQuality.HIGH,
        width,
        height,
        bitrate: 0,
        ssrc: 0,
      }),
    ];
  }

  if (svc) {
    // svc layers
    /* @ts-ignore */
    const encodingSM = encodings[0].scalabilityMode as string;
    const sm = new ScalabilityMode(encodingSM);
    const layers = [];
    const resRatio = sm.suffix == 'h' ? 1.5 : 2;
    const bitratesRatio = sm.suffix == 'h' ? 2 : 3;
    for (let i = 0; i < sm.spatial; i += 1) {
      layers.push(
        new VideoLayer({
          quality: Math.min(VideoQuality.HIGH, sm.spatial - 1) - i,
          width: Math.ceil(width / resRatio ** i),
          height: Math.ceil(height / resRatio ** i),
          bitrate: encodings[0].maxBitrate
            ? Math.ceil(encodings[0].maxBitrate / bitratesRatio ** i)
            : 0,
          ssrc: 0,
        }),
      );
    }
    return layers;
  }

  return encodings.map((encoding) => {
    const scale = encoding.scaleResolutionDownBy ?? 1;
    let quality = videoQualityForRid(encoding.rid ?? '');
    return new VideoLayer({
      quality,
      width: Math.ceil(width / scale),
      height: Math.ceil(height / scale),
      bitrate: encoding.maxBitrate ?? 0,
      ssrc: 0,
    });
  });
}

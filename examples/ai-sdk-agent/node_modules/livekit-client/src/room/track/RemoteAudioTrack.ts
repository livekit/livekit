import { TrackEvent } from '../events';
import type { AudioReceiverStats } from '../stats';
import { computeBitrate } from '../stats';
import type { LoggerOptions } from '../types';
import { isReactNative, supportsSetSinkId } from '../utils';
import RemoteTrack from './RemoteTrack';
import { Track } from './Track';
import type { AudioOutputOptions } from './options';

export default class RemoteAudioTrack extends RemoteTrack<Track.Kind.Audio> {
  private prevStats?: AudioReceiverStats;

  private elementVolume: number | undefined;

  private audioContext?: AudioContext;

  private gainNode?: GainNode;

  private sourceNode?: MediaStreamAudioSourceNode;

  private webAudioPluginNodes: AudioNode[];

  private sinkId?: string;

  constructor(
    mediaTrack: MediaStreamTrack,
    sid: string,
    receiver: RTCRtpReceiver,
    audioContext?: AudioContext,
    audioOutput?: AudioOutputOptions,
    loggerOptions?: LoggerOptions,
  ) {
    super(mediaTrack, sid, Track.Kind.Audio, receiver, loggerOptions);
    this.audioContext = audioContext;
    this.webAudioPluginNodes = [];
    if (audioOutput) {
      this.sinkId = audioOutput.deviceId;
    }
  }

  /**
   * sets the volume for all attached audio elements
   */
  setVolume(volume: number) {
    for (const el of this.attachedElements) {
      if (this.audioContext) {
        this.gainNode?.gain.setTargetAtTime(volume, 0, 0.1);
      } else {
        el.volume = volume;
      }
    }
    if (isReactNative()) {
      // @ts-ignore
      this._mediaStreamTrack._setVolume(volume);
    }
    this.elementVolume = volume;
  }

  /**
   * gets the volume of attached audio elements (loudest)
   */
  getVolume(): number {
    if (this.elementVolume) {
      return this.elementVolume;
    }
    if (isReactNative()) {
      // RN volume value defaults to 1.0 if hasn't been changed.
      return 1.0;
    }
    let highestVolume = 0;
    this.attachedElements.forEach((element) => {
      if (element.volume > highestVolume) {
        highestVolume = element.volume;
      }
    });
    return highestVolume;
  }

  /**
   * calls setSinkId on all attached elements, if supported
   * @param deviceId audio output device
   */
  async setSinkId(deviceId: string) {
    this.sinkId = deviceId;
    await Promise.all(
      this.attachedElements.map((elm) => {
        if (!supportsSetSinkId(elm)) {
          return;
        }
        /* @ts-ignore */
        return elm.setSinkId(deviceId) as Promise<void>;
      }),
    );
  }

  attach(): HTMLMediaElement;
  attach(element: HTMLMediaElement): HTMLMediaElement;
  attach(element?: HTMLMediaElement): HTMLMediaElement {
    const needsNewWebAudioConnection = this.attachedElements.length === 0;
    if (!element) {
      element = super.attach();
    } else {
      super.attach(element);
    }

    if (this.sinkId && supportsSetSinkId(element)) {
      element.setSinkId(this.sinkId).catch((e) => {
        this.log.error('Failed to set sink id on remote audio track', e, this.logContext);
      });
    }
    if (this.audioContext && needsNewWebAudioConnection) {
      this.log.debug('using audio context mapping', this.logContext);
      this.connectWebAudio(this.audioContext, element);
      element.volume = 0;
      element.muted = true;
    }

    if (this.elementVolume) {
      // make sure volume setting is being applied to the newly attached element
      this.setVolume(this.elementVolume);
    }

    return element;
  }

  /**
   * Detaches from all attached elements
   */
  detach(): HTMLMediaElement[];

  /**
   * Detach from a single element
   * @param element
   */
  detach(element: HTMLMediaElement): HTMLMediaElement;
  detach(element?: HTMLMediaElement): HTMLMediaElement | HTMLMediaElement[] {
    let detached: HTMLMediaElement | HTMLMediaElement[];
    if (!element) {
      detached = super.detach();
      this.disconnectWebAudio();
    } else {
      detached = super.detach(element);
      // if there are still any attached elements after detaching, connect webaudio to the first element that's left
      // disconnect webaudio otherwise
      if (this.audioContext) {
        if (this.attachedElements.length > 0) {
          this.connectWebAudio(this.audioContext, this.attachedElements[0]);
        } else {
          this.disconnectWebAudio();
        }
      }
    }
    return detached;
  }

  /**
   * @internal
   * @experimental
   */
  setAudioContext(audioContext: AudioContext | undefined) {
    this.audioContext = audioContext;
    if (audioContext && this.attachedElements.length > 0) {
      this.connectWebAudio(audioContext, this.attachedElements[0]);
    } else if (!audioContext) {
      this.disconnectWebAudio();
    }
  }

  /**
   * @internal
   * @experimental
   * @param {AudioNode[]} nodes - An array of WebAudio nodes. These nodes should not be connected to each other when passed, as the sdk will take care of connecting them in the order of the array.
   */
  setWebAudioPlugins(nodes: AudioNode[]) {
    this.webAudioPluginNodes = nodes;
    if (this.attachedElements.length > 0 && this.audioContext) {
      this.connectWebAudio(this.audioContext, this.attachedElements[0]);
    }
  }

  private connectWebAudio(context: AudioContext, element: HTMLMediaElement) {
    this.disconnectWebAudio();
    // @ts-ignore attached elements always have a srcObject set
    this.sourceNode = context.createMediaStreamSource(element.srcObject);
    let lastNode: AudioNode = this.sourceNode;
    this.webAudioPluginNodes.forEach((node) => {
      lastNode.connect(node);
      lastNode = node;
    });
    this.gainNode = context.createGain();
    lastNode.connect(this.gainNode);
    this.gainNode.connect(context.destination);

    if (this.elementVolume) {
      this.gainNode.gain.setTargetAtTime(this.elementVolume, 0, 0.1);
    }

    // try to resume the context if it isn't running already
    if (context.state !== 'running') {
      context
        .resume()
        .then(() => {
          if (context.state !== 'running') {
            this.emit(
              TrackEvent.AudioPlaybackFailed,
              new Error("Audio Context couldn't be started automatically"),
            );
          }
        })
        .catch((e) => {
          this.emit(TrackEvent.AudioPlaybackFailed, e);
        });
    }
  }

  private disconnectWebAudio() {
    this.gainNode?.disconnect();
    this.sourceNode?.disconnect();
    this.gainNode = undefined;
    this.sourceNode = undefined;
  }

  protected monitorReceiver = async () => {
    if (!this.receiver) {
      this._currentBitrate = 0;
      return;
    }
    const stats = await this.getReceiverStats();

    if (stats && this.prevStats && this.receiver) {
      this._currentBitrate = computeBitrate(stats, this.prevStats);
    }

    this.prevStats = stats;
  };

  async getReceiverStats(): Promise<AudioReceiverStats | undefined> {
    if (!this.receiver || !this.receiver.getStats) {
      return;
    }

    const stats = await this.receiver.getStats();
    let receiverStats: AudioReceiverStats | undefined;
    stats.forEach((v) => {
      if (v.type === 'inbound-rtp') {
        receiverStats = {
          type: 'audio',
          streamId: v.id,
          timestamp: v.timestamp,
          jitter: v.jitter,
          bytesReceived: v.bytesReceived,
          concealedSamples: v.concealedSamples,
          concealmentEvents: v.concealmentEvents,
          silentConcealedSamples: v.silentConcealedSamples,
          silentConcealmentEvents: v.silentConcealmentEvents,
          totalAudioEnergy: v.totalAudioEnergy,
          totalSamplesDuration: v.totalSamplesDuration,
        };
      }
    });
    return receiverStats;
  }
}

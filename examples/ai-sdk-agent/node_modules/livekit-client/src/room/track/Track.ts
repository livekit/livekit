import {
  AudioTrackFeature,
  VideoQuality as ProtoQuality,
  StreamState as ProtoStreamState,
  TrackSource,
  TrackType,
} from '@livekit/protocol';
import { EventEmitter } from 'events';
import type TypedEventEmitter from 'typed-emitter';
import type { SignalClient } from '../../api/SignalClient';
import log, { LoggerNames, type StructuredLogger, getLogger } from '../../logger';
import { TrackEvent } from '../events';
import type { LoggerOptions } from '../types';
import { isFireFox, isSafari, isWeb } from '../utils';
import type { TrackProcessor } from './processor/types';
import { getLogContextFromTrack } from './utils';

const BACKGROUND_REACTION_DELAY = 5000;

// keep old audio elements when detached, we would re-use them since on iOS
// Safari tracks which audio elements have been "blessed" by the user.
const recycledElements: Array<HTMLAudioElement> = [];

export enum VideoQuality {
  LOW = ProtoQuality.LOW,
  MEDIUM = ProtoQuality.MEDIUM,
  HIGH = ProtoQuality.HIGH,
}
export abstract class Track<
  TrackKind extends Track.Kind = Track.Kind,
> extends (EventEmitter as new () => TypedEventEmitter<TrackEventCallbacks>) {
  readonly kind: TrackKind;

  attachedElements: HTMLMediaElement[] = [];

  isMuted: boolean = false;

  source: Track.Source;

  private _streamState: Track.StreamState = Track.StreamState.Active;

  /**
   * sid is set after track is published to server, or if it's a remote track
   */
  sid?: Track.SID;

  /**
   * @internal
   */
  mediaStream?: MediaStream;

  /**
   * indicates current state of stream, it'll indicate `paused` if the track
   * has been paused by congestion controller
   */
  get streamState(): Track.StreamState {
    return this._streamState;
  }

  /** @internal */
  setStreamState(value: Track.StreamState) {
    this._streamState = value;
  }

  /** @internal */
  rtpTimestamp: number | undefined;

  protected _mediaStreamTrack: MediaStreamTrack;

  protected _mediaStreamID: string;

  protected isInBackground: boolean = false;

  private backgroundTimeout: ReturnType<typeof setTimeout> | undefined;

  private loggerContextCb: LoggerOptions['loggerContextCb'];

  protected timeSyncHandle: number | undefined;

  protected _currentBitrate: number = 0;

  protected monitorInterval?: ReturnType<typeof setInterval>;

  protected log: StructuredLogger = log;

  protected constructor(
    mediaTrack: MediaStreamTrack,
    kind: TrackKind,
    loggerOptions: LoggerOptions = {},
  ) {
    super();
    this.log = getLogger(loggerOptions.loggerName ?? LoggerNames.Track);
    this.loggerContextCb = loggerOptions.loggerContextCb;

    this.setMaxListeners(100);
    this.kind = kind;
    this._mediaStreamTrack = mediaTrack;
    this._mediaStreamID = mediaTrack.id;
    this.source = Track.Source.Unknown;
  }

  protected get logContext() {
    return {
      ...this.loggerContextCb?.(),
      ...getLogContextFromTrack(this),
    };
  }

  /** current receive bits per second */
  get currentBitrate(): number {
    return this._currentBitrate;
  }

  get mediaStreamTrack() {
    return this._mediaStreamTrack;
  }

  abstract get isLocal(): boolean;

  /**
   * @internal
   * used for keep mediaStream's first id, since it's id might change
   * if we disable/enable a track
   */
  get mediaStreamID(): string {
    return this._mediaStreamID;
  }

  /**
   * creates a new HTMLAudioElement or HTMLVideoElement, attaches to it, and returns it
   */
  attach(): HTMLMediaElement;

  /**
   * attaches track to an existing HTMLAudioElement or HTMLVideoElement
   */
  attach(element: HTMLMediaElement): HTMLMediaElement;
  attach(element?: HTMLMediaElement): HTMLMediaElement {
    let elementType = 'audio';
    if (this.kind === Track.Kind.Video) {
      elementType = 'video';
    }
    if (this.attachedElements.length === 0 && this.kind === Track.Kind.Video) {
      this.addAppVisibilityListener();
    }
    if (!element) {
      if (elementType === 'audio') {
        recycledElements.forEach((e) => {
          if (e.parentElement === null && !element) {
            element = e;
          }
        });
        if (element) {
          // remove it from pool
          recycledElements.splice(recycledElements.indexOf(element), 1);
        }
      }
      if (!element) {
        element = <HTMLMediaElement>document.createElement(elementType);
      }
    }

    if (!this.attachedElements.includes(element)) {
      this.attachedElements.push(element);
    }

    // even if we believe it's already attached to the element, it's possible
    // the element's srcObject was set to something else out of band.
    // we'll want to re-attach it in that case
    attachToElement(this.mediaStreamTrack, element);

    // handle auto playback failures
    const allMediaStreamTracks = (element.srcObject as MediaStream).getTracks();
    const hasAudio = allMediaStreamTracks.some((tr) => tr.kind === 'audio');

    // manually play media to detect auto playback status
    element
      .play()
      .then(() => {
        this.emit(hasAudio ? TrackEvent.AudioPlaybackStarted : TrackEvent.VideoPlaybackStarted);
      })
      .catch((e) => {
        if (e.name === 'NotAllowedError') {
          this.emit(hasAudio ? TrackEvent.AudioPlaybackFailed : TrackEvent.VideoPlaybackFailed, e);
        } else if (e.name === 'AbortError') {
          // commonly triggered by another `play` request, only log for debugging purposes
          log.debug(
            `${hasAudio ? 'audio' : 'video'} playback aborted, likely due to new play request`,
          );
        } else {
          log.warn(`could not playback ${hasAudio ? 'audio' : 'video'}`, e);
        }
        // If audio playback isn't allowed make sure we still play back the video
        if (
          hasAudio &&
          element &&
          allMediaStreamTracks.some((tr) => tr.kind === 'video') &&
          e.name === 'NotAllowedError'
        ) {
          element.muted = true;
          element.play().catch(() => {
            // catch for Safari, exceeded options at this point to automatically play the media element
          });
        }
      });

    this.emit(TrackEvent.ElementAttached, element);
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
    try {
      // detach from a single element
      if (element) {
        detachTrack(this.mediaStreamTrack, element);
        const idx = this.attachedElements.indexOf(element);
        if (idx >= 0) {
          this.attachedElements.splice(idx, 1);
          this.recycleElement(element);
          this.emit(TrackEvent.ElementDetached, element);
        }
        return element;
      }

      const detached: HTMLMediaElement[] = [];
      this.attachedElements.forEach((elm) => {
        detachTrack(this.mediaStreamTrack, elm);
        detached.push(elm);
        this.recycleElement(elm);
        this.emit(TrackEvent.ElementDetached, elm);
      });

      // remove all tracks
      this.attachedElements = [];
      return detached;
    } finally {
      if (this.attachedElements.length === 0) {
        this.removeAppVisibilityListener();
      }
    }
  }

  stop() {
    this.stopMonitor();
    this._mediaStreamTrack.stop();
  }

  protected enable() {
    this._mediaStreamTrack.enabled = true;
  }

  protected disable() {
    this._mediaStreamTrack.enabled = false;
  }

  /* @internal */
  abstract startMonitor(signalClient?: SignalClient): void;

  /* @internal */
  stopMonitor() {
    if (this.monitorInterval) {
      clearInterval(this.monitorInterval);
    }
    if (this.timeSyncHandle) {
      cancelAnimationFrame(this.timeSyncHandle);
    }
  }

  /** @internal */
  updateLoggerOptions(loggerOptions: LoggerOptions) {
    if (loggerOptions.loggerName) {
      this.log = getLogger(loggerOptions.loggerName);
    }
    if (loggerOptions.loggerContextCb) {
      this.loggerContextCb = loggerOptions.loggerContextCb;
    }
  }

  private recycleElement(element: HTMLMediaElement) {
    if (element instanceof HTMLAudioElement) {
      // we only need to re-use a single element
      let shouldCache = true;
      element.pause();
      recycledElements.forEach((e) => {
        if (!e.parentElement) {
          shouldCache = false;
        }
      });
      if (shouldCache) {
        recycledElements.push(element);
      }
    }
  }

  protected appVisibilityChangedListener = () => {
    if (this.backgroundTimeout) {
      clearTimeout(this.backgroundTimeout);
    }
    // delay app visibility update if it goes to hidden
    // update immediately if it comes back to focus
    if (document.visibilityState === 'hidden') {
      this.backgroundTimeout = setTimeout(
        () => this.handleAppVisibilityChanged(),
        BACKGROUND_REACTION_DELAY,
      );
    } else {
      this.handleAppVisibilityChanged();
    }
  };

  protected async handleAppVisibilityChanged() {
    this.isInBackground = document.visibilityState === 'hidden';
    if (!this.isInBackground && this.kind === Track.Kind.Video) {
      setTimeout(
        () =>
          this.attachedElements.forEach((el) =>
            el.play().catch(() => {
              /** catch clause necessary for Safari */
            }),
          ),
        0,
      );
    }
  }

  protected addAppVisibilityListener() {
    if (isWeb()) {
      this.isInBackground = document.visibilityState === 'hidden';
      document.addEventListener('visibilitychange', this.appVisibilityChangedListener);
    } else {
      this.isInBackground = false;
    }
  }

  protected removeAppVisibilityListener() {
    if (isWeb()) {
      document.removeEventListener('visibilitychange', this.appVisibilityChangedListener);
    }
  }
}

export function attachToElement(track: MediaStreamTrack, element: HTMLMediaElement) {
  let mediaStream: MediaStream;
  if (element.srcObject instanceof MediaStream) {
    mediaStream = element.srcObject;
  } else {
    mediaStream = new MediaStream();
  }

  // check if track matches existing track
  let existingTracks: MediaStreamTrack[];
  if (track.kind === 'audio') {
    existingTracks = mediaStream.getAudioTracks();
  } else {
    existingTracks = mediaStream.getVideoTracks();
  }
  if (!existingTracks.includes(track)) {
    existingTracks.forEach((et) => {
      mediaStream.removeTrack(et);
    });
    mediaStream.addTrack(track);
  }

  if (!isSafari() || !(element instanceof HTMLVideoElement)) {
    // when in low power mode (applies to both macOS and iOS), Safari will show a play/pause overlay
    // when a video starts that has the `autoplay` attribute is set.
    // we work around this by _not_ setting the autoplay attribute on safari and instead call `setTimeout(() => el.play(),0)` further down
    element.autoplay = true;
  }
  // In case there are no audio tracks present on the mediastream, we set the element as muted to ensure autoplay works
  element.muted = mediaStream.getAudioTracks().length === 0;
  if (element instanceof HTMLVideoElement) {
    element.playsInline = true;
  }

  // avoid flicker
  if (element.srcObject !== mediaStream) {
    element.srcObject = mediaStream;
    if ((isSafari() || isFireFox()) && element instanceof HTMLVideoElement) {
      // Firefox also has a timing issue where video doesn't actually get attached unless
      // performed out-of-band
      // Safari 15 has a bug where in certain layouts, video element renders
      // black until the page is resized or other changes take place.
      // Resetting the src triggers it to render.
      // https://developer.apple.com/forums/thread/690523
      setTimeout(() => {
        element.srcObject = mediaStream;
        // Safari 15 sometimes fails to start a video
        // when the window is backgrounded before the first frame is drawn
        // manually calling play here seems to fix that
        element.play().catch(() => {
          /** do nothing */
        });
      }, 0);
    }
  }
}

/** @internal */
export function detachTrack(track: MediaStreamTrack, element: HTMLMediaElement) {
  if (element.srcObject instanceof MediaStream) {
    const mediaStream = element.srcObject;
    mediaStream.removeTrack(track);
    if (mediaStream.getTracks().length > 0) {
      element.srcObject = mediaStream;
    } else {
      element.srcObject = null;
    }
  }
}

export namespace Track {
  export enum Kind {
    Audio = 'audio',
    Video = 'video',
    Unknown = 'unknown',
  }
  export type SID = string;
  export enum Source {
    Camera = 'camera',
    Microphone = 'microphone',
    ScreenShare = 'screen_share',
    ScreenShareAudio = 'screen_share_audio',
    Unknown = 'unknown',
  }

  export enum StreamState {
    Active = 'active',
    Paused = 'paused',
    Unknown = 'unknown',
  }

  export interface Dimensions {
    width: number;
    height: number;
  }

  /** @internal */
  export function kindToProto(k: Kind): TrackType {
    switch (k) {
      case Kind.Audio:
        return TrackType.AUDIO;
      case Kind.Video:
        return TrackType.VIDEO;
      default:
        // FIXME this was UNRECOGNIZED before
        return TrackType.DATA;
    }
  }

  /** @internal */
  export function kindFromProto(t: TrackType): Kind | undefined {
    switch (t) {
      case TrackType.AUDIO:
        return Kind.Audio;
      case TrackType.VIDEO:
        return Kind.Video;
      default:
        return Kind.Unknown;
    }
  }

  /** @internal */
  export function sourceToProto(s: Source): TrackSource {
    switch (s) {
      case Source.Camera:
        return TrackSource.CAMERA;
      case Source.Microphone:
        return TrackSource.MICROPHONE;
      case Source.ScreenShare:
        return TrackSource.SCREEN_SHARE;
      case Source.ScreenShareAudio:
        return TrackSource.SCREEN_SHARE_AUDIO;
      default:
        return TrackSource.UNKNOWN;
    }
  }

  /** @internal */
  export function sourceFromProto(s: TrackSource): Source {
    switch (s) {
      case TrackSource.CAMERA:
        return Source.Camera;
      case TrackSource.MICROPHONE:
        return Source.Microphone;
      case TrackSource.SCREEN_SHARE:
        return Source.ScreenShare;
      case TrackSource.SCREEN_SHARE_AUDIO:
        return Source.ScreenShareAudio;
      default:
        return Source.Unknown;
    }
  }

  /** @internal */
  export function streamStateFromProto(s: ProtoStreamState): StreamState {
    switch (s) {
      case ProtoStreamState.ACTIVE:
        return StreamState.Active;
      case ProtoStreamState.PAUSED:
        return StreamState.Paused;
      default:
        return StreamState.Unknown;
    }
  }
}

export type TrackEventCallbacks = {
  message: () => void;
  muted: (track?: any) => void;
  unmuted: (track?: any) => void;
  restarted: (track?: any) => void;
  ended: (track?: any) => void;
  updateSettings: () => void;
  updateSubscription: () => void;
  audioPlaybackStarted: () => void;
  audioPlaybackFailed: (error?: Error) => void;
  audioSilenceDetected: () => void;
  visibilityChanged: (visible: boolean, track?: any) => void;
  videoDimensionsChanged: (dimensions: Track.Dimensions, track?: any) => void;
  videoPlaybackStarted: () => void;
  videoPlaybackFailed: (error?: Error) => void;
  elementAttached: (element: HTMLMediaElement) => void;
  elementDetached: (element: HTMLMediaElement) => void;
  upstreamPaused: (track: any) => void;
  upstreamResumed: (track: any) => void;
  trackProcessorUpdate: (processor?: TrackProcessor<Track.Kind, any>) => void;
  audioTrackFeatureUpdate: (track: any, feature: AudioTrackFeature, enabled: boolean) => void;
  timeSyncUpdate: (update: { timestamp: number; rtpTimestamp: number }) => void;
  preConnectBufferFlushed: (buffer: Uint8Array[]) => void;
  cpuConstrained: () => void;
};

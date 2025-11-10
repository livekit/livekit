import {
  ChatMessage as ChatMessageModel,
  ClientInfo,
  ClientInfo_SDK,
  DisconnectReason,
  Transcription as TranscriptionModel,
} from '@livekit/protocol';
import { type BrowserDetails, getBrowser } from '../utils/browserParser';
import { protocolVersion, version } from '../version';
import { type ConnectionError, ConnectionErrorReason } from './errors';
import type LocalParticipant from './participant/LocalParticipant';
import type Participant from './participant/Participant';
import type RemoteParticipant from './participant/RemoteParticipant';
import CriticalTimers from './timers';
import type LocalAudioTrack from './track/LocalAudioTrack';
import type LocalTrack from './track/LocalTrack';
import type LocalTrackPublication from './track/LocalTrackPublication';
import type LocalVideoTrack from './track/LocalVideoTrack';
import type RemoteAudioTrack from './track/RemoteAudioTrack';
import type RemoteTrack from './track/RemoteTrack';
import type RemoteTrackPublication from './track/RemoteTrackPublication';
import type RemoteVideoTrack from './track/RemoteVideoTrack';
import { Track } from './track/Track';
import type { TrackPublication } from './track/TrackPublication';
import { type AudioCodec, type VideoCodec, audioCodecs, videoCodecs } from './track/options';
import { getNewAudioContext } from './track/utils';
import type { ChatMessage, LiveKitReactNativeInfo, TranscriptionSegment } from './types';

const separator = '|';
export const ddExtensionURI =
  'https://aomediacodec.github.io/av1-rtp-spec/#dependency-descriptor-rtp-header-extension';

export function unpackStreamId(packed: string): string[] {
  const parts = packed.split(separator);
  if (parts.length > 1) {
    return [parts[0], packed.substr(parts[0].length + 1)];
  }
  return [packed, ''];
}

export async function sleep(duration: number): Promise<void> {
  return new Promise((resolve) => CriticalTimers.setTimeout(resolve, duration));
}

/** @internal */
export function supportsTransceiver() {
  return 'addTransceiver' in RTCPeerConnection.prototype;
}

/** @internal */
export function supportsAddTrack() {
  return 'addTrack' in RTCPeerConnection.prototype;
}

export function supportsAdaptiveStream() {
  return typeof ResizeObserver !== undefined && typeof IntersectionObserver !== undefined;
}

export function supportsDynacast() {
  return supportsTransceiver();
}

export function supportsAV1(): boolean {
  if (!('getCapabilities' in RTCRtpSender)) {
    return false;
  }
  if (isSafari() || isFireFox()) {
    // Safari 17 on iPhone14 reports AV1 capability, but does not actually support it
    // Firefox does support AV1, but SVC publishing is not supported
    return false;
  }
  const capabilities = RTCRtpSender.getCapabilities('video');
  let hasAV1 = false;
  if (capabilities) {
    for (const codec of capabilities.codecs) {
      if (codec.mimeType.toLowerCase() === 'video/av1') {
        hasAV1 = true;
        break;
      }
    }
  }
  return hasAV1;
}

export function supportsVP9(): boolean {
  if (!('getCapabilities' in RTCRtpSender)) {
    return false;
  }
  if (isFireFox()) {
    // technically speaking FireFox supports VP9, but SVC publishing is broken
    // https://bugzilla.mozilla.org/show_bug.cgi?id=1633876
    return false;
  }
  if (isSafari()) {
    const browser = getBrowser();
    if (browser?.version && compareVersions(browser.version, '16') < 0) {
      // Safari 16 and below does not support VP9
      return false;
    }
    if (
      browser?.os === 'iOS' &&
      browser?.osVersion &&
      compareVersions(browser.osVersion, '16') < 0
    ) {
      // Safari 16 and below on iOS does not support VP9 we need the iOS check to account for other browsers running webkit under the hood
      return false;
    }
  }
  const capabilities = RTCRtpSender.getCapabilities('video');
  let hasVP9 = false;
  if (capabilities) {
    for (const codec of capabilities.codecs) {
      if (codec.mimeType.toLowerCase() === 'video/vp9') {
        hasVP9 = true;
        break;
      }
    }
  }
  return hasVP9;
}

export function supportsH265(): boolean {
  if (!('getCapabilities' in RTCRtpSender)) {
    return false;
  }

  const capabilities = RTCRtpSender.getCapabilities('video');
  let hasH265 = false;
  if (capabilities) {
    for (const codec of capabilities.codecs) {
      if (codec.mimeType.toLowerCase() === 'video/h265') {
        hasH265 = true;
        break;
      }
    }
  }
  return hasH265;
}

export function isSVCCodec(codec?: string): boolean {
  return codec === 'av1' || codec === 'vp9';
}

export function supportsSetSinkId(elm?: HTMLMediaElement): boolean {
  if (!document || isSafariBased()) {
    return false;
  }
  if (!elm) {
    elm = document.createElement('audio');
  }
  return 'setSinkId' in elm;
}

/**
 * Checks whether or not setting an audio output via {@link Room#setActiveDevice}
 * is supported for the current browser.
 */
export function supportsAudioOutputSelection(): boolean {
  // Note: this is method publicly exported under a user friendly name and currently only proxying `supportsSetSinkId`
  return supportsSetSinkId();
}

export function isBrowserSupported() {
  if (typeof RTCPeerConnection === 'undefined') {
    return false;
  }
  return supportsTransceiver() || supportsAddTrack();
}

export function isFireFox(): boolean {
  return getBrowser()?.name === 'Firefox';
}

export function isChromiumBased(): boolean {
  const browser = getBrowser();
  return !!browser && browser.name === 'Chrome' && browser.os !== 'iOS';
}

export function isSafari(): boolean {
  return getBrowser()?.name === 'Safari';
}

export function isSafariBased(): boolean {
  const b = getBrowser();
  return b?.name === 'Safari' || b?.os === 'iOS';
}

export function isSafari17Based(): boolean {
  const b = getBrowser();
  return (
    (b?.name === 'Safari' && b.version.startsWith('17.')) ||
    (b?.os === 'iOS' && !!b?.osVersion && compareVersions(b.osVersion, '17') >= 0)
  );
}

export function isSafariSvcApi(browser?: BrowserDetails): boolean {
  if (!browser) {
    browser = getBrowser();
  }
  // Safari 18.4 requires legacy svc api and scaleResolutionDown to be set
  return (
    (browser?.name === 'Safari' && compareVersions(browser.version, '18.3') > 0) ||
    (browser?.os === 'iOS' &&
      !!browser?.osVersion &&
      compareVersions(browser.osVersion, '18.3') > 0)
  );
}

export function isMobile(): boolean {
  if (!isWeb()) return false;

  return (
    // @ts-expect-error `userAgentData` is not yet part of typescript
    navigator.userAgentData?.mobile ??
    /Tablet|iPad|Mobile|Android|BlackBerry/.test(navigator.userAgent)
  );
}

export function isE2EESimulcastSupported() {
  const browser = getBrowser();
  const supportedSafariVersion = '17.2'; // see https://bugs.webkit.org/show_bug.cgi?id=257803
  if (browser) {
    if (browser.name !== 'Safari' && browser.os !== 'iOS') {
      return true;
    } else if (
      browser.os === 'iOS' &&
      browser.osVersion &&
      compareVersions(browser.osVersion, supportedSafariVersion) >= 0
    ) {
      return true;
    } else if (
      browser.name === 'Safari' &&
      compareVersions(browser.version, supportedSafariVersion) >= 0
    ) {
      return true;
    } else {
      return false;
    }
  }
}

export function isWeb(): boolean {
  return typeof document !== 'undefined';
}

export function isReactNative(): boolean {
  // navigator.product is deprecated on browsers, but will be set appropriately for react-native.
  return navigator.product == 'ReactNative';
}

export function isCloud(serverUrl: URL) {
  return (
    serverUrl.hostname.endsWith('.livekit.cloud') || serverUrl.hostname.endsWith('.livekit.run')
  );
}

function getLKReactNativeInfo(): LiveKitReactNativeInfo | undefined {
  // global defined only for ReactNative.
  // @ts-ignore
  if (global && global.LiveKitReactNativeGlobal) {
    // @ts-ignore
    return global.LiveKitReactNativeGlobal as LiveKitReactNativeInfo;
  }

  return undefined;
}

export function getReactNativeOs(): string | undefined {
  if (!isReactNative()) {
    return undefined;
  }

  let info = getLKReactNativeInfo();
  if (info) {
    return info.platform;
  }

  return undefined;
}

export function getDevicePixelRatio(): number {
  if (isWeb()) {
    return window.devicePixelRatio;
  }

  if (isReactNative()) {
    let info = getLKReactNativeInfo();
    if (info) {
      return info.devicePixelRatio;
    }
  }

  return 1;
}

/**
 * @param v1 - The first version string to compare.
 * @param v2 - The second version string to compare.
 * @returns A number indicating the order of the versions:
 *   - 1 if v1 is greater than v2
 *   - -1 if v1 is less than v2
 *   - 0 if v1 and v2 are equal
 */
export function compareVersions(v1: string, v2: string): number {
  const parts1 = v1.split('.');
  const parts2 = v2.split('.');
  const k = Math.min(parts1.length, parts2.length);
  for (let i = 0; i < k; ++i) {
    const p1 = parseInt(parts1[i], 10);
    const p2 = parseInt(parts2[i], 10);
    if (p1 > p2) return 1;
    if (p1 < p2) return -1;
    if (i === k - 1 && p1 === p2) return 0;
  }
  if (v1 === '' && v2 !== '') {
    return -1;
  } else if (v2 === '') {
    return 1;
  }
  return parts1.length == parts2.length ? 0 : parts1.length < parts2.length ? -1 : 1;
}

function roDispatchCallback(entries: ResizeObserverEntry[]) {
  for (const entry of entries) {
    (entry.target as ObservableMediaElement).handleResize(entry);
  }
}

function ioDispatchCallback(entries: IntersectionObserverEntry[]) {
  for (const entry of entries) {
    (entry.target as ObservableMediaElement).handleVisibilityChanged(entry);
  }
}

let resizeObserver: ResizeObserver | null = null;
export const getResizeObserver = () => {
  if (!resizeObserver) resizeObserver = new ResizeObserver(roDispatchCallback);
  return resizeObserver;
};

let intersectionObserver: IntersectionObserver | null = null;
export const getIntersectionObserver = () => {
  if (!intersectionObserver) {
    intersectionObserver = new IntersectionObserver(ioDispatchCallback, {
      root: null,
      rootMargin: '0px',
    });
  }
  return intersectionObserver;
};

export interface ObservableMediaElement extends HTMLMediaElement {
  handleResize: (entry: ResizeObserverEntry) => void;
  handleVisibilityChanged: (entry: IntersectionObserverEntry) => void;
}

export function getClientInfo(): ClientInfo {
  const info = new ClientInfo({
    sdk: ClientInfo_SDK.JS,
    protocol: protocolVersion,
    version,
  });

  if (isReactNative()) {
    info.os = getReactNativeOs() ?? '';
  }
  return info;
}

let emptyVideoStreamTrack: MediaStreamTrack | undefined;

export function getEmptyVideoStreamTrack() {
  if (!emptyVideoStreamTrack) {
    emptyVideoStreamTrack = createDummyVideoStreamTrack();
  }
  return emptyVideoStreamTrack.clone();
}

export function createDummyVideoStreamTrack(
  width: number = 16,
  height: number = 16,
  enabled: boolean = false,
  paintContent: boolean = false,
) {
  const canvas = document.createElement('canvas');
  // the canvas size is set to 16 by default, because electron apps seem to fail with smaller values
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d');
  ctx?.fillRect(0, 0, canvas.width, canvas.height);
  if (paintContent && ctx) {
    ctx.beginPath();
    ctx.arc(width / 2, height / 2, 50, 0, Math.PI * 2, true);
    ctx.closePath();
    ctx.fillStyle = 'grey';
    ctx.fill();
  }
  // @ts-ignore
  const dummyStream = canvas.captureStream();
  const [dummyTrack] = dummyStream.getTracks();
  if (!dummyTrack) {
    throw Error('Could not get empty media stream video track');
  }
  dummyTrack.enabled = enabled;

  return dummyTrack;
}

let emptyAudioStreamTrack: MediaStreamTrack | undefined;

export function getEmptyAudioStreamTrack() {
  if (!emptyAudioStreamTrack) {
    // implementation adapted from https://blog.mozilla.org/webrtc/warm-up-with-replacetrack/
    const ctx = new AudioContext();
    const oscillator = ctx.createOscillator();
    const gain = ctx.createGain();
    gain.gain.setValueAtTime(0, 0);
    const dst = ctx.createMediaStreamDestination();
    oscillator.connect(gain);
    gain.connect(dst);
    oscillator.start();
    [emptyAudioStreamTrack] = dst.stream.getAudioTracks();
    if (!emptyAudioStreamTrack) {
      throw Error('Could not get empty media stream audio track');
    }
    emptyAudioStreamTrack.enabled = false;
  }
  return emptyAudioStreamTrack.clone();
}

export function getStereoAudioStreamTrack() {
  const ctx = new AudioContext();
  const oscLeft = ctx.createOscillator();
  const oscRight = ctx.createOscillator();
  oscLeft.frequency.value = 440;
  oscRight.frequency.value = 220;
  const merger = ctx.createChannelMerger(2);
  oscLeft.connect(merger, 0, 0); // left channel
  oscRight.connect(merger, 0, 1); // right channel
  const dst = ctx.createMediaStreamDestination();
  merger.connect(dst);
  oscLeft.start();
  oscRight.start();
  const [stereoTrack] = dst.stream.getAudioTracks();
  if (!stereoTrack) {
    throw Error('Could not get stereo media stream audio track');
  }
  return stereoTrack;
}

export class Future<T> {
  promise: Promise<T>;

  resolve?: (arg: T) => void;

  reject?: (e: any) => void;

  onFinally?: () => void;

  get isResolved(): boolean {
    return this._isResolved;
  }

  private _isResolved: boolean = false;

  constructor(
    futureBase?: (resolve: (arg: T) => void, reject: (e: any) => void) => void,
    onFinally?: () => void,
  ) {
    this.onFinally = onFinally;
    this.promise = new Promise<T>(async (resolve, reject) => {
      this.resolve = resolve;
      this.reject = reject;
      if (futureBase) {
        await futureBase(resolve, reject);
      }
    }).finally(() => {
      this._isResolved = true;
      this.onFinally?.();
    });
  }
}

export type AudioAnalyserOptions = {
  /**
   * If set to true, the analyser will use a cloned version of the underlying mediastreamtrack, which won't be impacted by muting the track.
   * Useful for local tracks when implementing things like "seems like you're muted, but trying to speak".
   * Defaults to false
   */
  cloneTrack?: boolean;
  /**
   * see https://developer.mozilla.org/en-US/docs/Web/API/AnalyserNode/fftSize
   */
  fftSize?: number;
  /**
   * see https://developer.mozilla.org/en-US/docs/Web/API/AnalyserNode/smoothingTimeConstant
   */
  smoothingTimeConstant?: number;
  /**
   * see https://developer.mozilla.org/en-US/docs/Web/API/AnalyserNode/minDecibels
   */
  minDecibels?: number;
  /**
   * see https://developer.mozilla.org/en-US/docs/Web/API/AnalyserNode/maxDecibels
   */
  maxDecibels?: number;
};

/**
 * Creates and returns an analyser web audio node that is attached to the provided track.
 * Additionally returns a convenience method `calculateVolume` to perform instant volume readings on that track.
 * Call the returned `cleanup` function to close the audioContext that has been created for the instance of this helper
 */
export function createAudioAnalyser(
  track: LocalAudioTrack | RemoteAudioTrack,
  options?: AudioAnalyserOptions,
) {
  const opts = {
    cloneTrack: false,
    fftSize: 2048,
    smoothingTimeConstant: 0.8,
    minDecibels: -100,
    maxDecibels: -80,
    ...options,
  };
  const audioContext = getNewAudioContext();

  if (!audioContext) {
    throw new Error('Audio Context not supported on this browser');
  }

  const streamTrack = opts.cloneTrack ? track.mediaStreamTrack.clone() : track.mediaStreamTrack;
  const mediaStreamSource = audioContext.createMediaStreamSource(new MediaStream([streamTrack]));
  const analyser = audioContext.createAnalyser();
  analyser.minDecibels = opts.minDecibels;
  analyser.maxDecibels = opts.maxDecibels;
  analyser.fftSize = opts.fftSize;
  analyser.smoothingTimeConstant = opts.smoothingTimeConstant;

  mediaStreamSource.connect(analyser);
  const dataArray = new Uint8Array(analyser.frequencyBinCount);

  /**
   * Calculates the current volume of the track in the range from 0 to 1
   */
  const calculateVolume = () => {
    analyser.getByteFrequencyData(dataArray);
    let sum = 0;
    for (const amplitude of dataArray) {
      sum += Math.pow(amplitude / 255, 2);
    }
    const volume = Math.sqrt(sum / dataArray.length);
    return volume;
  };

  const cleanup = async () => {
    await audioContext.close();
    if (opts.cloneTrack) {
      streamTrack.stop();
    }
  };

  return { calculateVolume, analyser, cleanup };
}

export function isAudioCodec(maybeCodec: string): maybeCodec is AudioCodec {
  return audioCodecs.includes(maybeCodec as AudioCodec);
}

export function isVideoCodec(maybeCodec: string): maybeCodec is VideoCodec {
  return videoCodecs.includes(maybeCodec as VideoCodec);
}

export function unwrapConstraint(constraint: ConstrainDOMString): string;
export function unwrapConstraint(constraint: ConstrainULong): number;
export function unwrapConstraint(constraint: ConstrainDOMString | ConstrainULong): string | number {
  if (typeof constraint === 'string' || typeof constraint === 'number') {
    return constraint;
  }

  if (Array.isArray(constraint)) {
    return constraint[0];
  }
  if (constraint.exact !== undefined) {
    if (Array.isArray(constraint.exact)) {
      return constraint.exact[0];
    }
    return constraint.exact;
  }
  if (constraint.ideal !== undefined) {
    if (Array.isArray(constraint.ideal)) {
      return constraint.ideal[0];
    }
    return constraint.ideal;
  }
  throw Error('could not unwrap constraint');
}

export function toWebsocketUrl(url: string): string {
  if (url.startsWith('http')) {
    return url.replace(/^(http)/, 'ws');
  }
  return url;
}

export function toHttpUrl(url: string): string {
  if (url.startsWith('ws')) {
    return url.replace(/^(ws)/, 'http');
  }
  return url;
}

export function extractTranscriptionSegments(
  transcription: TranscriptionModel,
  firstReceivedTimesMap: Map<string, number>,
): TranscriptionSegment[] {
  return transcription.segments.map(({ id, text, language, startTime, endTime, final }) => {
    const firstReceivedTime = firstReceivedTimesMap.get(id) ?? Date.now();
    const lastReceivedTime = Date.now();
    if (final) {
      firstReceivedTimesMap.delete(id);
    } else {
      firstReceivedTimesMap.set(id, firstReceivedTime);
    }
    return {
      id,
      text,
      startTime: Number.parseInt(startTime.toString()),
      endTime: Number.parseInt(endTime.toString()),
      final,
      language,
      firstReceivedTime,
      lastReceivedTime,
    };
  });
}

export function extractChatMessage(msg: ChatMessageModel): ChatMessage {
  const { id, timestamp, message, editTimestamp } = msg;
  return {
    id,
    timestamp: Number.parseInt(timestamp.toString()),
    editTimestamp: editTimestamp ? Number.parseInt(editTimestamp.toString()) : undefined,
    message,
  };
}

export function getDisconnectReasonFromConnectionError(e: ConnectionError) {
  switch (e.reason) {
    case ConnectionErrorReason.LeaveRequest:
      return e.context as DisconnectReason;
    case ConnectionErrorReason.Cancelled:
      return DisconnectReason.CLIENT_INITIATED;
    case ConnectionErrorReason.NotAllowed:
      return DisconnectReason.USER_REJECTED;
    case ConnectionErrorReason.ServerUnreachable:
      return DisconnectReason.JOIN_FAILURE;
    default:
      return DisconnectReason.UNKNOWN_REASON;
  }
}

/** convert bigints to numbers preserving undefined values */
export function bigIntToNumber<T extends BigInt | undefined>(
  value: T,
): T extends BigInt ? number : undefined {
  return (value !== undefined ? Number(value) : undefined) as T extends BigInt ? number : undefined;
}

/** convert numbers to bigints preserving undefined values */
export function numberToBigInt<T extends number | undefined>(
  value: T,
): T extends number ? bigint : undefined {
  return (value !== undefined ? BigInt(value) : undefined) as T extends number ? bigint : undefined;
}

export function isLocalTrack(track: Track | MediaStreamTrack | undefined): track is LocalTrack {
  return !!track && !(track instanceof MediaStreamTrack) && track.isLocal;
}

export function isAudioTrack(
  track: Track | undefined,
): track is LocalAudioTrack | RemoteAudioTrack {
  return !!track && track.kind == Track.Kind.Audio;
}

export function isVideoTrack(
  track: Track | undefined,
): track is LocalVideoTrack | RemoteVideoTrack {
  return !!track && track.kind == Track.Kind.Video;
}

export function isLocalVideoTrack(
  track: Track | MediaStreamTrack | undefined,
): track is LocalVideoTrack {
  return isLocalTrack(track) && isVideoTrack(track);
}

export function isLocalAudioTrack(
  track: Track | MediaStreamTrack | undefined,
): track is LocalAudioTrack {
  return isLocalTrack(track) && isAudioTrack(track);
}

export function isRemoteTrack(track: Track | undefined): track is RemoteTrack {
  return !!track && !track.isLocal;
}

export function isRemotePub(pub: TrackPublication | undefined): pub is RemoteTrackPublication {
  return !!pub && !pub.isLocal;
}

export function isLocalPub(pub: TrackPublication | undefined): pub is LocalTrackPublication {
  return !!pub && !pub.isLocal;
}

export function isRemoteVideoTrack(track: Track | undefined): track is RemoteVideoTrack {
  return isRemoteTrack(track) && isVideoTrack(track);
}

export function isLocalParticipant(p: Participant): p is LocalParticipant {
  return p.isLocal;
}

export function isRemoteParticipant(p: Participant): p is RemoteParticipant {
  return !p.isLocal;
}

export function splitUtf8(s: string, n: number): Uint8Array[] {
  if (n < 4) {
    throw new Error('n must be at least 4 due to utf8 encoding rules');
  }
  // adapted from https://stackoverflow.com/a/6043797
  const result: Uint8Array[] = [];
  let encoded = new TextEncoder().encode(s);
  while (encoded.length > n) {
    let k = n;
    while (k > 0) {
      const byte = encoded[k];
      if (byte !== undefined && (byte & 0xc0) !== 0x80) {
        break;
      }
      k--;
    }
    result.push(encoded.slice(0, k));
    encoded = encoded.slice(k);
  }
  if (encoded.length > 0) {
    result.push(encoded);
  }
  return result;
}

export function extractMaxAgeFromRequestHeaders(headers: Headers): number | undefined {
  const cacheControl = headers.get('Cache-Control');
  if (cacheControl) {
    const maxAge = cacheControl.match(/(?:^|[,\s])max-age=(\d+)/)?.[1];
    if (maxAge) {
      return parseInt(maxAge, 10);
    }
  }
  return undefined;
}

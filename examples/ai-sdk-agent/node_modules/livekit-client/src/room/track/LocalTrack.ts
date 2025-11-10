import { Mutex } from '@livekit/mutex';
import { debounce } from 'ts-debounce';
import { getBrowser } from '../../utils/browserParser';
import DeviceManager from '../DeviceManager';
import { DeviceUnsupportedError, TrackInvalidError } from '../errors';
import { TrackEvent } from '../events';
import type { LoggerOptions } from '../types';
import { compareVersions, isMobile, sleep, unwrapConstraint } from '../utils';
import { Track, attachToElement, detachTrack } from './Track';
import type { VideoCodec } from './options';
import type { TrackProcessor } from './processor/types';
import { LocalTrackRecorder, isRecordingSupported } from './record';
import type { ReplaceTrackOptions } from './types';

const DEFAULT_DIMENSIONS_TIMEOUT = 1000;
const PRE_CONNECT_BUFFER_TIMEOUT = 10_000;

export default abstract class LocalTrack<
  TrackKind extends Track.Kind = Track.Kind,
> extends Track<TrackKind> {
  protected _sender?: RTCRtpSender;

  private autoStopPreConnectBuffer: ReturnType<typeof setTimeout> | undefined;

  /** @internal */
  get sender(): RTCRtpSender | undefined {
    return this._sender;
  }

  /** @internal */
  set sender(sender: RTCRtpSender | undefined) {
    this._sender = sender;
  }

  /** @internal */
  codec?: VideoCodec;

  get constraints() {
    return this._constraints;
  }

  get hasPreConnectBuffer() {
    return !!this.localTrackRecorder;
  }

  protected _constraints: MediaTrackConstraints;

  protected reacquireTrack: boolean;

  protected providedByUser: boolean;

  protected muteLock: Mutex;

  protected pauseUpstreamLock: Mutex;

  protected processorElement?: HTMLMediaElement;

  protected processor?: TrackProcessor<TrackKind, any>;

  protected audioContext?: AudioContext;

  protected manuallyStopped: boolean = false;

  protected localTrackRecorder: LocalTrackRecorder<typeof this> | undefined;

  protected trackChangeLock: Mutex;

  /**
   *
   * @param mediaTrack
   * @param kind
   * @param constraints MediaTrackConstraints that are being used when restarting or reacquiring tracks
   * @param userProvidedTrack Signals to the SDK whether or not the mediaTrack should be managed (i.e. released and reacquired) internally by the SDK
   */
  protected constructor(
    mediaTrack: MediaStreamTrack,
    kind: TrackKind,
    constraints?: MediaTrackConstraints,
    userProvidedTrack = false,
    loggerOptions?: LoggerOptions,
  ) {
    super(mediaTrack, kind, loggerOptions);
    this.reacquireTrack = false;
    this.providedByUser = userProvidedTrack;
    this.muteLock = new Mutex();
    this.pauseUpstreamLock = new Mutex();
    this.trackChangeLock = new Mutex();
    this.trackChangeLock.lock().then(async (unlock) => {
      try {
        await this.setMediaStreamTrack(mediaTrack, true);
      } finally {
        unlock();
      }
    });

    // added to satisfy TS compiler, constraints are synced with MediaStreamTrack
    this._constraints = mediaTrack.getConstraints();
    if (constraints) {
      this._constraints = constraints;
    }
  }

  get id(): string {
    return this._mediaStreamTrack.id;
  }

  get dimensions(): Track.Dimensions | undefined {
    if (this.kind !== Track.Kind.Video) {
      return undefined;
    }

    const { width, height } = this._mediaStreamTrack.getSettings();
    if (width && height) {
      return {
        width,
        height,
      };
    }
    return undefined;
  }

  private _isUpstreamPaused: boolean = false;

  get isUpstreamPaused() {
    return this._isUpstreamPaused;
  }

  get isUserProvided() {
    return this.providedByUser;
  }

  get mediaStreamTrack() {
    return this.processor?.processedTrack ?? this._mediaStreamTrack;
  }

  get isLocal() {
    return true;
  }

  /**
   * @internal
   * returns mediaStreamTrack settings of the capturing mediastreamtrack source - ignoring processors
   */
  getSourceTrackSettings() {
    return this._mediaStreamTrack.getSettings();
  }

  private async setMediaStreamTrack(newTrack: MediaStreamTrack, force?: boolean) {
    if (newTrack === this._mediaStreamTrack && !force) {
      return;
    }
    if (this._mediaStreamTrack) {
      // detach
      this.attachedElements.forEach((el) => {
        detachTrack(this._mediaStreamTrack, el);
      });
      this.debouncedTrackMuteHandler.cancel('new-track');
      this._mediaStreamTrack.removeEventListener('ended', this.handleEnded);
      this._mediaStreamTrack.removeEventListener('mute', this.handleTrackMuteEvent);
      this._mediaStreamTrack.removeEventListener('unmute', this.handleTrackUnmuteEvent);
    }

    this.mediaStream = new MediaStream([newTrack]);
    if (newTrack) {
      newTrack.addEventListener('ended', this.handleEnded);
      // when underlying track emits mute, it indicates that the device is unable
      // to produce media. In this case we'll need to signal with remote that
      // the track is "muted"
      // note this is different from LocalTrack.mute because we do not want to
      // touch MediaStreamTrack.enabled
      newTrack.addEventListener('mute', this.handleTrackMuteEvent);
      newTrack.addEventListener('unmute', this.handleTrackUnmuteEvent);
      this._constraints = newTrack.getConstraints();
    }
    let processedTrack: MediaStreamTrack | undefined;
    if (this.processor && newTrack) {
      this.log.debug('restarting processor', this.logContext);
      if (this.kind === 'unknown') {
        throw TypeError('cannot set processor on track of unknown kind');
      }

      if (this.processorElement) {
        attachToElement(newTrack, this.processorElement);
        // ensure the processorElement itself stays muted
        this.processorElement.muted = true;
      }
      await this.processor.restart({
        track: newTrack,
        kind: this.kind,
        element: this.processorElement,
      });
      processedTrack = this.processor.processedTrack;
    }
    if (this.sender && this.sender.transport?.state !== 'closed') {
      await this.sender.replaceTrack(processedTrack ?? newTrack);
    }
    // if `newTrack` is different from the existing track, stop the
    // older track just before replacing it
    if (!this.providedByUser && this._mediaStreamTrack !== newTrack) {
      this._mediaStreamTrack.stop();
    }
    this._mediaStreamTrack = newTrack;
    if (newTrack) {
      // sync muted state with the enabled state of the newly provided track
      this._mediaStreamTrack.enabled = !this.isMuted;
      // when a valid track is replace, we'd want to start producing
      await this.resumeUpstream();
      this.attachedElements.forEach((el) => {
        attachToElement(processedTrack ?? newTrack, el);
      });
    }
  }

  async waitForDimensions(timeout = DEFAULT_DIMENSIONS_TIMEOUT): Promise<Track.Dimensions> {
    if (this.kind === Track.Kind.Audio) {
      throw new Error('cannot get dimensions for audio tracks');
    }

    if (getBrowser()?.os === 'iOS') {
      // browsers report wrong initial resolution on iOS.
      // when slightly delaying the call to .getSettings(), the correct resolution is being reported
      await sleep(10);
    }

    const started = Date.now();
    while (Date.now() - started < timeout) {
      const dims = this.dimensions;
      if (dims) {
        return dims;
      }
      await sleep(50);
    }
    throw new TrackInvalidError('unable to get track dimensions after timeout');
  }

  async setDeviceId(deviceId: ConstrainDOMString): Promise<boolean> {
    if (
      this._constraints.deviceId === deviceId &&
      this._mediaStreamTrack.getSettings().deviceId === unwrapConstraint(deviceId)
    ) {
      return true;
    }

    this._constraints.deviceId = deviceId;

    // when track is muted, underlying media stream track is stopped and
    // will be restarted later
    if (this.isMuted) {
      return true;
    }

    await this.restartTrack();

    return unwrapConstraint(deviceId) === this._mediaStreamTrack.getSettings().deviceId;
  }

  abstract restartTrack(constraints?: unknown): Promise<void>;

  /**
   * @returns DeviceID of the device that is currently being used for this track
   */
  async getDeviceId(normalize = true): Promise<string | undefined> {
    // screen share doesn't have a usable device id
    if (this.source === Track.Source.ScreenShare) {
      return;
    }
    const { deviceId, groupId } = this._mediaStreamTrack.getSettings();
    const kind = this.kind === Track.Kind.Audio ? 'audioinput' : 'videoinput';

    return normalize
      ? DeviceManager.getInstance().normalizeDeviceId(kind, deviceId, groupId)
      : deviceId;
  }

  async mute() {
    this.setTrackMuted(true);
    return this;
  }

  async unmute() {
    this.setTrackMuted(false);
    return this;
  }

  async replaceTrack(track: MediaStreamTrack, options?: ReplaceTrackOptions): Promise<typeof this>;
  async replaceTrack(track: MediaStreamTrack, userProvidedTrack?: boolean): Promise<typeof this>;
  async replaceTrack(
    track: MediaStreamTrack,
    userProvidedOrOptions: boolean | ReplaceTrackOptions | undefined,
  ) {
    const unlock = await this.trackChangeLock.lock();
    try {
      if (!this.sender) {
        throw new TrackInvalidError('unable to replace an unpublished track');
      }

      let userProvidedTrack: boolean | undefined;
      let stopProcessor: boolean | undefined;

      if (typeof userProvidedOrOptions === 'boolean') {
        userProvidedTrack = userProvidedOrOptions;
      } else if (userProvidedOrOptions !== undefined) {
        userProvidedTrack = userProvidedOrOptions.userProvidedTrack;
        stopProcessor = userProvidedOrOptions.stopProcessor;
      }

      this.providedByUser = userProvidedTrack ?? true;

      this.log.debug('replace MediaStreamTrack', this.logContext);
      await this.setMediaStreamTrack(track);
      // this must be synced *after* setting mediaStreamTrack above, since it relies
      // on the previous state in order to cleanup

      if (stopProcessor && this.processor) {
        await this.internalStopProcessor();
      }
      return this;
    } finally {
      unlock();
    }
  }

  protected async restart(constraints?: MediaTrackConstraints) {
    this.manuallyStopped = false;
    const unlock = await this.trackChangeLock.lock();

    try {
      if (!constraints) {
        constraints = this._constraints;
      }
      const { deviceId, facingMode, ...otherConstraints } = constraints;
      this.log.debug('restarting track with constraints', { ...this.logContext, constraints });

      const streamConstraints: MediaStreamConstraints = {
        audio: false,
        video: false,
      };

      if (this.kind === Track.Kind.Video) {
        streamConstraints.video = deviceId || facingMode ? { deviceId, facingMode } : true;
      } else {
        streamConstraints.audio = deviceId ? { deviceId, ...otherConstraints } : true;
      }

      // these steps are duplicated from setMediaStreamTrack because we must stop
      // the previous tracks before new tracks can be acquired
      this.attachedElements.forEach((el) => {
        detachTrack(this.mediaStreamTrack, el);
      });
      this._mediaStreamTrack.removeEventListener('ended', this.handleEnded);
      // on Safari, the old audio track must be stopped before attempting to acquire
      // the new track, otherwise the new track will stop with
      // 'A MediaStreamTrack ended due to a capture failure`
      this._mediaStreamTrack.stop();

      // create new track and attach
      const mediaStream = await navigator.mediaDevices.getUserMedia(streamConstraints);
      const newTrack = mediaStream.getTracks()[0];
      if (this.kind === Track.Kind.Video) {
        // we already captured the audio track with the constraints, so we only need to apply the video constraints
        await newTrack.applyConstraints(otherConstraints);
      }
      newTrack.addEventListener('ended', this.handleEnded);
      this.log.debug('re-acquired MediaStreamTrack', this.logContext);

      await this.setMediaStreamTrack(newTrack);
      this._constraints = constraints;
      this.emit(TrackEvent.Restarted, this);
      if (this.manuallyStopped) {
        this.log.warn(
          'track was stopped during a restart, stopping restarted track',
          this.logContext,
        );
        this.stop();
      }
      return this;
    } finally {
      unlock();
    }
  }

  protected setTrackMuted(muted: boolean) {
    this.log.debug(`setting ${this.kind} track ${muted ? 'muted' : 'unmuted'}`, this.logContext);

    if (this.isMuted === muted && this._mediaStreamTrack.enabled !== muted) {
      return;
    }

    this.isMuted = muted;
    this._mediaStreamTrack.enabled = !muted;
    this.emit(muted ? TrackEvent.Muted : TrackEvent.Unmuted, this);
  }

  protected get needsReAcquisition(): boolean {
    return (
      this._mediaStreamTrack.readyState !== 'live' ||
      this._mediaStreamTrack.muted ||
      !this._mediaStreamTrack.enabled ||
      this.reacquireTrack
    );
  }

  protected async handleAppVisibilityChanged() {
    await super.handleAppVisibilityChanged();
    if (!isMobile()) return;
    this.log.debug(`visibility changed, is in Background: ${this.isInBackground}`, this.logContext);

    if (!this.isInBackground && this.needsReAcquisition && !this.isUserProvided && !this.isMuted) {
      this.log.debug(`track needs to be reacquired, restarting ${this.source}`, this.logContext);
      await this.restart();
      this.reacquireTrack = false;
    }
  }

  private handleTrackMuteEvent = () =>
    this.debouncedTrackMuteHandler().catch(() =>
      this.log.debug('track mute bounce got cancelled by an unmute event', this.logContext),
    );

  private debouncedTrackMuteHandler = debounce(async () => {
    await this.pauseUpstream();
  }, 5000);

  private handleTrackUnmuteEvent = async () => {
    this.debouncedTrackMuteHandler.cancel('unmute');
    await this.resumeUpstream();
  };

  private handleEnded = () => {
    if (this.isInBackground) {
      this.reacquireTrack = true;
    }
    this._mediaStreamTrack.removeEventListener('mute', this.handleTrackMuteEvent);
    this._mediaStreamTrack.removeEventListener('unmute', this.handleTrackUnmuteEvent);
    this.emit(TrackEvent.Ended, this);
  };

  stop() {
    this.manuallyStopped = true;
    super.stop();

    this._mediaStreamTrack.removeEventListener('ended', this.handleEnded);
    this._mediaStreamTrack.removeEventListener('mute', this.handleTrackMuteEvent);
    this._mediaStreamTrack.removeEventListener('unmute', this.handleTrackUnmuteEvent);
    this.processor?.destroy();
    this.processor = undefined;
  }

  /**
   * pauses publishing to the server without disabling the local MediaStreamTrack
   * this is used to display a user's own video locally while pausing publishing to
   * the server.
   * this API is unsupported on Safari < 12 due to a bug
   **/
  async pauseUpstream() {
    const unlock = await this.pauseUpstreamLock.lock();
    try {
      if (this._isUpstreamPaused === true) {
        return;
      }
      if (!this.sender) {
        this.log.warn('unable to pause upstream for an unpublished track', this.logContext);
        return;
      }

      this._isUpstreamPaused = true;
      this.emit(TrackEvent.UpstreamPaused, this);
      const browser = getBrowser();
      if (browser?.name === 'Safari' && compareVersions(browser.version, '12.0') < 0) {
        // https://bugs.webkit.org/show_bug.cgi?id=184911
        throw new DeviceUnsupportedError('pauseUpstream is not supported on Safari < 12.');
      }
      if (this.sender.transport?.state !== 'closed') {
        await this.sender.replaceTrack(null);
      }
    } finally {
      unlock();
    }
  }

  async resumeUpstream() {
    const unlock = await this.pauseUpstreamLock.lock();
    try {
      if (this._isUpstreamPaused === false) {
        return;
      }
      if (!this.sender) {
        this.log.warn('unable to resume upstream for an unpublished track', this.logContext);
        return;
      }
      this._isUpstreamPaused = false;
      this.emit(TrackEvent.UpstreamResumed, this);

      if (this.sender.transport?.state !== 'closed') {
        // this operation is noop if mediastreamtrack is already being sent
        await this.sender.replaceTrack(this.mediaStreamTrack);
      }
    } finally {
      unlock();
    }
  }

  /**
   * Gets the RTCStatsReport for the LocalTrack's underlying RTCRtpSender
   * See https://developer.mozilla.org/en-US/docs/Web/API/RTCStatsReport
   *
   * @returns Promise<RTCStatsReport> | undefined
   */
  async getRTCStatsReport(): Promise<RTCStatsReport | undefined> {
    if (!this.sender?.getStats) {
      return;
    }
    const statsReport = await this.sender.getStats();
    return statsReport;
  }

  /**
   * Sets a processor on this track.
   * See https://github.com/livekit/track-processors-js for example usage
   *
   * @experimental
   *
   * @param processor
   * @param showProcessedStreamLocally
   * @returns
   */
  async setProcessor(processor: TrackProcessor<TrackKind>, showProcessedStreamLocally = true) {
    const unlock = await this.trackChangeLock.lock();
    try {
      this.log.debug('setting up processor', this.logContext);

      const processorElement = document.createElement(this.kind) as HTMLMediaElement;

      const processorOptions = {
        kind: this.kind,
        track: this._mediaStreamTrack,
        element: processorElement,
        audioContext: this.audioContext,
      };
      await processor.init(processorOptions);
      this.log.debug('processor initialized', this.logContext);

      if (this.processor) {
        await this.internalStopProcessor();
      }
      if (this.kind === 'unknown') {
        throw TypeError('cannot set processor on track of unknown kind');
      }

      attachToElement(this._mediaStreamTrack, processorElement);
      processorElement.muted = true;

      processorElement.play().catch((error) => {
        if (error instanceof DOMException && error.name === 'AbortError') {
          // This happens on Safari when the processor is restarted, try again after a delay
          this.log.warn('failed to play processor element, retrying', {
            ...this.logContext,
            error,
          });
          setTimeout(() => {
            processorElement.play().catch((err) => {
              this.log.error('failed to play processor element', { ...this.logContext, err });
            });
          }, 100);
        } else {
          this.log.error('failed to play processor element', { ...this.logContext, error });
        }
      });

      this.processor = processor;
      this.processorElement = processorElement;
      if (this.processor.processedTrack) {
        for (const el of this.attachedElements) {
          if (el !== this.processorElement && showProcessedStreamLocally) {
            detachTrack(this._mediaStreamTrack, el);
            attachToElement(this.processor.processedTrack, el);
          }
        }
        await this.sender?.replaceTrack(this.processor.processedTrack);
      }
      this.emit(TrackEvent.TrackProcessorUpdate, this.processor);
    } finally {
      unlock();
    }
  }

  getProcessor() {
    return this.processor;
  }

  /**
   * Stops the track processor
   * See https://github.com/livekit/track-processors-js for example usage
   *
   * @experimental
   * @returns
   */
  async stopProcessor(keepElement = true) {
    const unlock = await this.trackChangeLock.lock();
    try {
      await this.internalStopProcessor(keepElement);
    } finally {
      unlock();
    }
  }

  /**
   * @internal
   * This method assumes the caller has acquired a trackChangeLock already.
   * The public facing method for stopping the processor is `stopProcessor` and it wraps this method in the trackChangeLock.
   */
  protected async internalStopProcessor(keepElement = true) {
    if (!this.processor) return;
    this.log.debug('stopping processor', this.logContext);
    this.processor.processedTrack?.stop();
    await this.processor.destroy();
    this.processor = undefined;
    if (!keepElement) {
      this.processorElement?.remove();
      this.processorElement = undefined;
    }
    // apply original track constraints in case the processor changed them
    await this._mediaStreamTrack.applyConstraints(this._constraints);
    // force re-setting of the mediaStreamTrack on the sender
    await this.setMediaStreamTrack(this._mediaStreamTrack, true);
    this.emit(TrackEvent.TrackProcessorUpdate);
  }

  /** @internal */
  startPreConnectBuffer(timeslice: number = 100) {
    if (!isRecordingSupported()) {
      this.log.warn(
        'MediaRecorder is not available, cannot start preconnect buffer',
        this.logContext,
      );
      return;
    }

    if (!this.localTrackRecorder) {
      let mimeType = 'audio/webm;codecs=opus';
      if (!MediaRecorder.isTypeSupported(mimeType)) {
        // iOS currently only supports video/mp4 as a mime type - even for audio.
        mimeType = 'video/mp4';
      }
      this.localTrackRecorder = new LocalTrackRecorder(this, {
        mimeType,
      });
    } else {
      this.log.warn('preconnect buffer already started');
      return;
    }

    this.localTrackRecorder.start(timeslice);
    this.autoStopPreConnectBuffer = setTimeout(() => {
      this.log.warn(
        'preconnect buffer timed out, stopping recording automatically',
        this.logContext,
      );
      this.stopPreConnectBuffer();
    }, PRE_CONNECT_BUFFER_TIMEOUT);
  }

  /** @internal */
  stopPreConnectBuffer() {
    clearTimeout(this.autoStopPreConnectBuffer);
    if (this.localTrackRecorder) {
      this.localTrackRecorder.stop();
      this.localTrackRecorder = undefined;
    }
  }

  /** @internal */
  getPreConnectBuffer(): ReadableStream<Uint8Array> | undefined {
    return this.localTrackRecorder?.byteStream;
  }

  getPreConnectBufferMimeType() {
    return this.localTrackRecorder?.mimeType;
  }

  protected abstract monitorSender(): void;
}

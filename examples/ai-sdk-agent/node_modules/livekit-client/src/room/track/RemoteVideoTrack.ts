import { debounce } from 'ts-debounce';
import { TrackEvent } from '../events';
import type { VideoReceiverStats } from '../stats';
import { computeBitrate } from '../stats';
import CriticalTimers from '../timers';
import type { LoggerOptions } from '../types';
import type { ObservableMediaElement } from '../utils';
import { getDevicePixelRatio, getIntersectionObserver, getResizeObserver, isWeb } from '../utils';
import RemoteTrack from './RemoteTrack';
import { Track, attachToElement, detachTrack } from './Track';
import type { AdaptiveStreamSettings } from './types';

const REACTION_DELAY = 100;

export default class RemoteVideoTrack extends RemoteTrack<Track.Kind.Video> {
  private prevStats?: VideoReceiverStats;

  private elementInfos: ElementInfo[] = [];

  private adaptiveStreamSettings?: AdaptiveStreamSettings;

  private lastVisible?: boolean;

  private lastDimensions?: Track.Dimensions;

  constructor(
    mediaTrack: MediaStreamTrack,
    sid: string,
    receiver: RTCRtpReceiver,
    adaptiveStreamSettings?: AdaptiveStreamSettings,
    loggerOptions?: LoggerOptions,
  ) {
    super(mediaTrack, sid, Track.Kind.Video, receiver, loggerOptions);
    this.adaptiveStreamSettings = adaptiveStreamSettings;
  }

  get isAdaptiveStream(): boolean {
    return this.adaptiveStreamSettings !== undefined;
  }

  override setStreamState(value: Track.StreamState) {
    super.setStreamState(value);
    this.log.debug('setStreamState', value);
    if (this.isAdaptiveStream && value === Track.StreamState.Active) {
      // update visibility for adaptive stream tracks when stream state received from server is active
      // this is needed to ensure the track is stopped when there's no element attached to it at all
      this.updateVisibility();
    }
  }

  /**
   * Note: When using adaptiveStream, you need to use remoteVideoTrack.attach() to add the track to a HTMLVideoElement, otherwise your video tracks might never start
   */
  get mediaStreamTrack() {
    return this._mediaStreamTrack;
  }

  /** @internal */
  setMuted(muted: boolean) {
    super.setMuted(muted);

    this.attachedElements.forEach((element) => {
      // detach or attach
      if (muted) {
        detachTrack(this._mediaStreamTrack, element);
      } else {
        attachToElement(this._mediaStreamTrack, element);
      }
    });
  }

  attach(): HTMLMediaElement;
  attach(element: HTMLMediaElement): HTMLMediaElement;
  attach(element?: HTMLMediaElement): HTMLMediaElement {
    if (!element) {
      element = super.attach();
    } else {
      super.attach(element);
    }

    // It's possible attach is called multiple times on an element. When that's
    // the case, we'd want to avoid adding duplicate elementInfos
    if (
      this.adaptiveStreamSettings &&
      this.elementInfos.find((info) => info.element === element) === undefined
    ) {
      const elementInfo = new HTMLElementInfo(element);
      this.observeElementInfo(elementInfo);
    }
    return element;
  }

  /**
   * Observe an ElementInfo for changes when adaptive streaming.
   * @param elementInfo
   * @internal
   */
  observeElementInfo(elementInfo: ElementInfo) {
    if (
      this.adaptiveStreamSettings &&
      this.elementInfos.find((info) => info === elementInfo) === undefined
    ) {
      elementInfo.handleResize = () => {
        this.debouncedHandleResize();
      };
      elementInfo.handleVisibilityChanged = () => {
        this.updateVisibility();
      };
      this.elementInfos.push(elementInfo);
      elementInfo.observe();
      // trigger the first resize update cycle
      // if the tab is backgrounded, the initial resize event does not fire until
      // the tab comes into focus for the first time.
      this.debouncedHandleResize();
      this.updateVisibility();
    } else {
      this.log.warn('visibility resize observer not triggered', this.logContext);
    }
  }

  /**
   * Stop observing an ElementInfo for changes.
   * @param elementInfo
   * @internal
   */
  stopObservingElementInfo(elementInfo: ElementInfo) {
    if (!this.isAdaptiveStream) {
      this.log.warn('stopObservingElementInfo ignored', this.logContext);
      return;
    }
    const stopElementInfos = this.elementInfos.filter((info) => info === elementInfo);
    for (const info of stopElementInfos) {
      info.stopObserving();
    }
    this.elementInfos = this.elementInfos.filter((info) => info !== elementInfo);
    this.updateVisibility();
    this.debouncedHandleResize();
  }

  detach(): HTMLMediaElement[];
  detach(element: HTMLMediaElement): HTMLMediaElement;
  detach(element?: HTMLMediaElement): HTMLMediaElement | HTMLMediaElement[] {
    let detachedElements: HTMLMediaElement[] = [];
    if (element) {
      this.stopObservingElement(element);
      return super.detach(element);
    }
    detachedElements = super.detach();

    for (const e of detachedElements) {
      this.stopObservingElement(e);
    }

    return detachedElements;
  }

  /** @internal */
  getDecoderImplementation(): string | undefined {
    return this.prevStats?.decoderImplementation;
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

  async getReceiverStats(): Promise<VideoReceiverStats | undefined> {
    if (!this.receiver || !this.receiver.getStats) {
      return;
    }

    const stats = await this.receiver.getStats();
    let receiverStats: VideoReceiverStats | undefined;
    let codecID = '';
    let codecs = new Map<string, any>();
    stats.forEach((v) => {
      if (v.type === 'inbound-rtp') {
        codecID = v.codecId;
        receiverStats = {
          type: 'video',
          streamId: v.id,
          framesDecoded: v.framesDecoded,
          framesDropped: v.framesDropped,
          framesReceived: v.framesReceived,
          packetsReceived: v.packetsReceived,
          packetsLost: v.packetsLost,
          frameWidth: v.frameWidth,
          frameHeight: v.frameHeight,
          pliCount: v.pliCount,
          firCount: v.firCount,
          nackCount: v.nackCount,
          jitter: v.jitter,
          timestamp: v.timestamp,
          bytesReceived: v.bytesReceived,
          decoderImplementation: v.decoderImplementation,
        };
      } else if (v.type === 'codec') {
        codecs.set(v.id, v);
      }
    });
    if (receiverStats && codecID !== '' && codecs.get(codecID)) {
      receiverStats.mimeType = codecs.get(codecID).mimeType;
    }
    return receiverStats;
  }

  private stopObservingElement(element: HTMLMediaElement) {
    const stopElementInfos = this.elementInfos.filter((info) => info.element === element);
    for (const info of stopElementInfos) {
      this.stopObservingElementInfo(info);
    }
  }

  protected async handleAppVisibilityChanged() {
    await super.handleAppVisibilityChanged();
    if (!this.isAdaptiveStream) return;
    this.updateVisibility();
  }

  private readonly debouncedHandleResize = debounce(() => {
    this.updateDimensions();
  }, REACTION_DELAY);

  private updateVisibility(forceEmit?: boolean) {
    const lastVisibilityChange = this.elementInfos.reduce(
      (prev, info) => Math.max(prev, info.visibilityChangedAt || 0),
      0,
    );

    const backgroundPause =
      (this.adaptiveStreamSettings?.pauseVideoInBackground ?? true) // default to true
        ? this.isInBackground
        : false;
    const isPiPMode = this.elementInfos.some((info) => info.pictureInPicture);
    const isVisible =
      (this.elementInfos.some((info) => info.visible) && !backgroundPause) || isPiPMode;

    if (this.lastVisible === isVisible && !forceEmit) {
      return;
    }

    if (!isVisible && Date.now() - lastVisibilityChange < REACTION_DELAY) {
      // delay hidden events
      CriticalTimers.setTimeout(() => {
        this.updateVisibility();
      }, REACTION_DELAY);
      return;
    }

    this.lastVisible = isVisible;
    this.emit(TrackEvent.VisibilityChanged, isVisible, this);
  }

  private updateDimensions() {
    let maxWidth = 0;
    let maxHeight = 0;
    const pixelDensity = this.getPixelDensity();
    for (const info of this.elementInfos) {
      const currentElementWidth = info.width() * pixelDensity;
      const currentElementHeight = info.height() * pixelDensity;
      if (currentElementWidth + currentElementHeight > maxWidth + maxHeight) {
        maxWidth = currentElementWidth;
        maxHeight = currentElementHeight;
      }
    }

    if (this.lastDimensions?.width === maxWidth && this.lastDimensions?.height === maxHeight) {
      return;
    }

    this.lastDimensions = {
      width: maxWidth,
      height: maxHeight,
    };

    this.emit(TrackEvent.VideoDimensionsChanged, this.lastDimensions, this);
  }

  private getPixelDensity(): number {
    const pixelDensity = this.adaptiveStreamSettings?.pixelDensity;
    if (pixelDensity === 'screen') {
      return getDevicePixelRatio();
    } else if (!pixelDensity) {
      // when unset, we'll pick a sane default here.
      // for higher pixel density devices (mobile phones, etc), we'll use 2
      // otherwise it defaults to 1
      const devicePixelRatio = getDevicePixelRatio();
      if (devicePixelRatio > 2) {
        return 2;
      } else {
        return 1;
      }
    }
    return pixelDensity;
  }
}

export interface ElementInfo {
  element: object;
  width(): number;
  height(): number;
  visible: boolean;
  pictureInPicture: boolean;
  visibilityChangedAt: number | undefined;

  handleResize?: () => void;
  handleVisibilityChanged?: () => void;
  observe(): void;
  stopObserving(): void;
}

class HTMLElementInfo implements ElementInfo {
  element: HTMLMediaElement;

  get visible(): boolean {
    return this.isPiP || this.isIntersecting;
  }

  get pictureInPicture(): boolean {
    return this.isPiP;
  }

  visibilityChangedAt: number | undefined;

  handleResize?: () => void;

  handleVisibilityChanged?: () => void;

  private isPiP: boolean;

  private isIntersecting: boolean;

  constructor(element: HTMLMediaElement, visible?: boolean) {
    this.element = element;
    this.isIntersecting = visible ?? isElementInViewport(element);
    this.isPiP = isWeb() && isElementInPiP(element);
    this.visibilityChangedAt = 0;
  }

  width(): number {
    return this.element.clientWidth;
  }

  height(): number {
    return this.element.clientHeight;
  }

  observe() {
    // make sure we update the current visible state once we start to observe
    this.isIntersecting = isElementInViewport(this.element);
    this.isPiP = isElementInPiP(this.element);

    (this.element as ObservableMediaElement).handleResize = () => {
      this.handleResize?.();
    };
    (this.element as ObservableMediaElement).handleVisibilityChanged = this.onVisibilityChanged;

    getIntersectionObserver().observe(this.element);
    getResizeObserver().observe(this.element);
    (this.element as HTMLVideoElement).addEventListener('enterpictureinpicture', this.onEnterPiP);
    (this.element as HTMLVideoElement).addEventListener('leavepictureinpicture', this.onLeavePiP);
    window.documentPictureInPicture?.addEventListener('enter', this.onEnterPiP);
    window.documentPictureInPicture?.window?.addEventListener('pagehide', this.onLeavePiP);
  }

  private onVisibilityChanged = (entry: IntersectionObserverEntry) => {
    const { target, isIntersecting } = entry;
    if (target === this.element) {
      this.isIntersecting = isIntersecting;
      this.isPiP = isElementInPiP(this.element);
      this.visibilityChangedAt = Date.now();
      this.handleVisibilityChanged?.();
    }
  };

  private onEnterPiP = () => {
    window.documentPictureInPicture?.window?.addEventListener('pagehide', this.onLeavePiP);
    this.isPiP = isElementInPiP(this.element);
    this.handleVisibilityChanged?.();
  };

  private onLeavePiP = () => {
    this.isPiP = isElementInPiP(this.element);
    this.handleVisibilityChanged?.();
  };

  stopObserving() {
    getIntersectionObserver()?.unobserve(this.element);
    getResizeObserver()?.unobserve(this.element);
    (this.element as HTMLVideoElement).removeEventListener(
      'enterpictureinpicture',
      this.onEnterPiP,
    );
    (this.element as HTMLVideoElement).removeEventListener(
      'leavepictureinpicture',
      this.onLeavePiP,
    );
    window.documentPictureInPicture?.removeEventListener('enter', this.onEnterPiP);
    window.documentPictureInPicture?.window?.removeEventListener('pagehide', this.onLeavePiP);
  }
}

function isElementInPiP(el: HTMLElement) {
  // Simple video PiP
  if (document.pictureInPictureElement === el) return true;
  // Document PiP
  if (window.documentPictureInPicture?.window)
    return isElementInViewport(el, window.documentPictureInPicture?.window);
  return false;
}

// does not account for occlusion by other elements or opacity property
function isElementInViewport(el: HTMLElement, win?: Window) {
  const viewportWindow = win || window;
  let top = el.offsetTop;
  let left = el.offsetLeft;
  const width = el.offsetWidth;
  const height = el.offsetHeight;
  const { hidden } = el;
  const { display } = getComputedStyle(el);

  while (el.offsetParent) {
    el = el.offsetParent as HTMLElement;
    top += el.offsetTop;
    left += el.offsetLeft;
  }

  return (
    top < viewportWindow.pageYOffset + viewportWindow.innerHeight &&
    left < viewportWindow.pageXOffset + viewportWindow.innerWidth &&
    top + height > viewportWindow.pageYOffset &&
    left + width > viewportWindow.pageXOffset &&
    !hidden &&
    display !== 'none'
  );
}

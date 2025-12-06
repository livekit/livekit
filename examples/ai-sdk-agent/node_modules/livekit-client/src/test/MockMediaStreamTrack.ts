/* eslint-disable @typescript-eslint/no-unused-vars */
// @ts-ignore
export default class MockMediaStreamTrack implements MediaStreamTrack {
  contentHint: string = '';

  enabled: boolean = true;

  id: string = 'id';

  kind: string = 'video';

  label: string = 'label';

  muted: boolean = false;

  onended: ((this: MediaStreamTrack, ev: Event) => any) | null = null;

  onmute: ((this: MediaStreamTrack, ev: Event) => any) | null = null;

  onunmute: ((this: MediaStreamTrack, ev: Event) => any) | null = null;

  readyState: MediaStreamTrackState = 'live';

  isolated: boolean = false;

  onisolationchange: ((this: MediaStreamTrack, ev: Event) => any) | null = null;

  // @ts-ignore
  applyConstraints(constraints?: MediaTrackConstraints): Promise<void> {
    throw new Error('Method not implemented.');
  }

  clone(): MediaStreamTrack {
    throw new Error('Method not implemented.');
  }

  getCapabilities(): MediaTrackCapabilities {
    throw new Error('Method not implemented.');
  }

  getConstraints(): MediaTrackConstraints {
    throw new Error('Method not implemented.');
  }

  getSettings(): MediaTrackSettings {
    throw new Error('Method not implemented.');
  }

  stop(): void {
    throw new Error('Method not implemented.');
  }

  addEventListener<K extends keyof MediaStreamTrackEventMap>(
    type: K,
    listener: (this: MediaStreamTrack, ev: MediaStreamTrackEventMap[K]) => any,
    options?: boolean | AddEventListenerOptions,
  ): void;
  addEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | AddEventListenerOptions,
  ): void;
  addEventListener(type: any, listener: any, options?: any): void {
    throw new Error('Method not implemented.');
  }

  removeEventListener<K extends keyof MediaStreamTrackEventMap>(
    type: K,
    listener: (this: MediaStreamTrack, ev: MediaStreamTrackEventMap[K]) => any,
    options?: boolean | EventListenerOptions,
  ): void;
  removeEventListener(
    type: string,
    listener: EventListenerOrEventListenerObject,
    options?: boolean | EventListenerOptions,
  ): void;
  removeEventListener(type: any, listener: any, options?: any): void {
    throw new Error('Method not implemented.');
  }

  dispatchEvent(event: Event): boolean {
    throw new Error('Method not implemented.');
  }
}

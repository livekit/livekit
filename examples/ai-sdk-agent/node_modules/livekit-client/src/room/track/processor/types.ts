import type Room from '../../Room';
import type { Track } from '../Track';

/**
 * @experimental
 */
export type ProcessorOptions<T extends Track.Kind> = {
  kind: T;
  track: MediaStreamTrack;
  element?: HTMLMediaElement;
  audioContext?: AudioContext;
};

/**
 * @experimental
 */
export interface AudioProcessorOptions extends ProcessorOptions<Track.Kind.Audio> {
  audioContext: AudioContext;
}

/**
 * @experimental
 */
export interface VideoProcessorOptions extends ProcessorOptions<Track.Kind.Video> {}

/**
 * @experimental
 */
export interface TrackProcessor<
  T extends Track.Kind,
  U extends ProcessorOptions<T> = ProcessorOptions<T>,
> {
  name: string;
  init: (opts: U) => Promise<void>;
  restart: (opts: U) => Promise<void>;
  destroy: () => Promise<void>;
  processedTrack?: MediaStreamTrack;
  onPublish?: (room: Room) => Promise<void>;
  onUnpublish?: () => Promise<void>;
}

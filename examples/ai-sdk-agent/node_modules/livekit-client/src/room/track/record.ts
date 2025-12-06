import type LocalTrack from './LocalTrack';

// Check if MediaRecorder is available
const isMediaRecorderAvailable = typeof MediaRecorder !== 'undefined';

// Fallback class for environments without MediaRecorder
class FallbackRecorder {
  constructor() {
    throw new Error('MediaRecorder is not available in this environment');
  }
}

// Use conditional inheritance to avoid parse-time errors
const RecorderBase = isMediaRecorderAvailable
  ? MediaRecorder
  : (FallbackRecorder as unknown as typeof MediaRecorder);

export class LocalTrackRecorder<T extends LocalTrack> extends RecorderBase {
  byteStream: ReadableStream<Uint8Array>;

  constructor(track: T, options?: MediaRecorderOptions) {
    if (!isMediaRecorderAvailable) {
      throw new Error('MediaRecorder is not available in this environment');
    }

    super(new MediaStream([track.mediaStreamTrack]), options);

    let dataListener: (event: BlobEvent) => void;

    let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;

    const isClosed = () => streamController === undefined;

    const onStop = () => {
      this.removeEventListener('dataavailable', dataListener);
      this.removeEventListener('stop', onStop);
      this.removeEventListener('error', onError);
      streamController?.close();
      streamController = undefined;
    };

    const onError = (event: Event) => {
      streamController?.error(event);
      this.removeEventListener('dataavailable', dataListener);
      this.removeEventListener('stop', onStop);
      this.removeEventListener('error', onError);
      streamController = undefined;
    };

    this.byteStream = new ReadableStream({
      start: (controller) => {
        streamController = controller;
        dataListener = async (event: BlobEvent) => {
          let data: Uint8Array;

          if (event.data.arrayBuffer) {
            const arrayBuffer = await event.data.arrayBuffer();
            data = new Uint8Array(arrayBuffer);

            // @ts-expect-error react-native passes over Uint8Arrays directly
          } else if (event.data.byteArray) {
            // @ts-expect-error
            data = event.data.byteArray as Uint8Array;
          } else {
            throw new Error('no data available!');
          }

          if (isClosed()) {
            return;
          }
          controller.enqueue(data);
        };
        this.addEventListener('dataavailable', dataListener);
      },
      cancel: () => {
        onStop();
      },
    });

    this.addEventListener('stop', onStop);
    this.addEventListener('error', onError);
  }
}

// Helper function to check if recording is supported
export function isRecordingSupported(): boolean {
  return isMediaRecorderAvailable;
}

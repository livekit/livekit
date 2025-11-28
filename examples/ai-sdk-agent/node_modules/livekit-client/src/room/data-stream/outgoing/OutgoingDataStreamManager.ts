import { Mutex } from '@livekit/mutex';
import {
  DataPacket,
  DataPacket_Kind,
  DataStream_ByteHeader,
  DataStream_Chunk,
  DataStream_Header,
  DataStream_OperationType,
  DataStream_TextHeader,
  DataStream_Trailer,
  Encryption_Type,
} from '@livekit/protocol';
import { type StructuredLogger } from '../../../logger';
import type RTCEngine from '../../RTCEngine';
import { EngineEvent } from '../../events';
import type {
  ByteStreamInfo,
  SendFileOptions,
  SendTextOptions,
  StreamBytesOptions,
  StreamTextOptions,
  TextStreamInfo,
} from '../../types';
import { numberToBigInt, splitUtf8 } from '../../utils';
import { ByteStreamWriter, TextStreamWriter } from './StreamWriter';

const STREAM_CHUNK_SIZE = 15_000;

/**
 * Manages sending custom user data via data channels.
 * @internal
 */
export default class OutgoingDataStreamManager {
  protected engine: RTCEngine;

  protected log: StructuredLogger;

  constructor(engine: RTCEngine, log: StructuredLogger) {
    this.engine = engine;
    this.log = log;
  }

  setupEngine(engine: RTCEngine) {
    this.engine = engine;
  }

  /** {@inheritDoc LocalParticipant.sendText} */
  async sendText(text: string, options?: SendTextOptions): Promise<TextStreamInfo> {
    const streamId = crypto.randomUUID();
    const textInBytes = new TextEncoder().encode(text);
    const totalTextLength = textInBytes.byteLength;

    const fileIds = options?.attachments?.map(() => crypto.randomUUID());

    const progresses = new Array<number>(fileIds ? fileIds.length + 1 : 1).fill(0);

    const handleProgress = (progress: number, idx: number) => {
      progresses[idx] = progress;
      const totalProgress = progresses.reduce((acc, val) => acc + val, 0);
      options?.onProgress?.(totalProgress);
    };

    const writer = await this.streamText({
      streamId,
      totalSize: totalTextLength,
      destinationIdentities: options?.destinationIdentities,
      topic: options?.topic,
      attachedStreamIds: fileIds,
      attributes: options?.attributes,
    });

    await writer.write(text);
    // set text part of progress to 1
    handleProgress(1, 0);

    await writer.close();

    if (options?.attachments && fileIds) {
      await Promise.all(
        options.attachments.map(async (file, idx) =>
          this._sendFile(fileIds[idx], file, {
            topic: options.topic,
            mimeType: file.type,
            onProgress: (progress) => {
              handleProgress(progress, idx + 1);
            },
          }),
        ),
      );
    }
    return writer.info;
  }

  /**
   * @internal
   * @experimental CAUTION, might get removed in a minor release
   */
  async streamText(options?: StreamTextOptions): Promise<TextStreamWriter> {
    const streamId = options?.streamId ?? crypto.randomUUID();

    const info: TextStreamInfo = {
      id: streamId,
      mimeType: 'text/plain',
      timestamp: Date.now(),
      topic: options?.topic ?? '',
      size: options?.totalSize,
      attributes: options?.attributes,
      encryptionType: this.engine.e2eeManager?.isDataChannelEncryptionEnabled
        ? Encryption_Type.GCM
        : Encryption_Type.NONE,
    };
    const header = new DataStream_Header({
      streamId,
      mimeType: info.mimeType,
      topic: info.topic,
      timestamp: numberToBigInt(info.timestamp),
      totalLength: numberToBigInt(options?.totalSize),
      attributes: info.attributes,
      contentHeader: {
        case: 'textHeader',
        value: new DataStream_TextHeader({
          version: options?.version,
          attachedStreamIds: options?.attachedStreamIds,
          replyToStreamId: options?.replyToStreamId,
          operationType:
            options?.type === 'update'
              ? DataStream_OperationType.UPDATE
              : DataStream_OperationType.CREATE,
        }),
      },
    });
    const destinationIdentities = options?.destinationIdentities;
    const packet = new DataPacket({
      destinationIdentities,
      value: {
        case: 'streamHeader',
        value: header,
      },
    });
    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);

    let chunkId = 0;
    const engine = this.engine;

    const writableStream = new WritableStream<string>({
      // Implement the sink
      async write(text) {
        for (const textByteChunk of splitUtf8(text, STREAM_CHUNK_SIZE)) {
          await engine.waitForBufferStatusLow(DataPacket_Kind.RELIABLE);
          const chunk = new DataStream_Chunk({
            content: textByteChunk,
            streamId,
            chunkIndex: numberToBigInt(chunkId),
          });
          const chunkPacket = new DataPacket({
            destinationIdentities,
            value: {
              case: 'streamChunk',
              value: chunk,
            },
          });
          await engine.sendDataPacket(chunkPacket, DataPacket_Kind.RELIABLE);

          chunkId += 1;
        }
      },
      async close() {
        const trailer = new DataStream_Trailer({
          streamId,
        });
        const trailerPacket = new DataPacket({
          destinationIdentities,
          value: {
            case: 'streamTrailer',
            value: trailer,
          },
        });
        await engine.sendDataPacket(trailerPacket, DataPacket_Kind.RELIABLE);
      },
      abort(err) {
        console.log('Sink error:', err);
        // TODO handle aborts to signal something to receiver side
      },
    });

    let onEngineClose = async () => {
      await writer.close();
    };

    engine.once(EngineEvent.Closing, onEngineClose);

    const writer = new TextStreamWriter(writableStream, info, () =>
      this.engine.off(EngineEvent.Closing, onEngineClose),
    );

    return writer;
  }

  async sendFile(file: File, options?: SendFileOptions): Promise<{ id: string }> {
    const streamId = crypto.randomUUID();
    await this._sendFile(streamId, file, options);
    return { id: streamId };
  }

  private async _sendFile(streamId: string, file: File, options?: SendFileOptions) {
    const writer = await this.streamBytes({
      streamId,
      totalSize: file.size,
      name: file.name,
      mimeType: options?.mimeType ?? file.type,
      topic: options?.topic,
      destinationIdentities: options?.destinationIdentities,
    });
    const reader = file.stream().getReader();
    while (true) {
      const { done, value } = await reader.read();
      if (done) {
        break;
      }
      await writer.write(value);
    }
    await writer.close();
    return writer.info;
  }

  async streamBytes(options?: StreamBytesOptions) {
    const streamId = options?.streamId ?? crypto.randomUUID();
    const destinationIdentities = options?.destinationIdentities;

    const info: ByteStreamInfo = {
      id: streamId,
      mimeType: options?.mimeType ?? 'application/octet-stream',
      topic: options?.topic ?? '',
      timestamp: Date.now(),
      attributes: options?.attributes,
      size: options?.totalSize,
      name: options?.name ?? 'unknown',
      encryptionType: this.engine.e2eeManager?.isDataChannelEncryptionEnabled
        ? Encryption_Type.GCM
        : Encryption_Type.NONE,
    };

    const header = new DataStream_Header({
      totalLength: numberToBigInt(info.size ?? 0),
      mimeType: info.mimeType,
      streamId,
      topic: info.topic,
      timestamp: numberToBigInt(Date.now()),
      attributes: info.attributes,
      contentHeader: {
        case: 'byteHeader',
        value: new DataStream_ByteHeader({
          name: info.name,
        }),
      },
    });

    const packet = new DataPacket({
      destinationIdentities,
      value: {
        case: 'streamHeader',
        value: header,
      },
    });

    await this.engine.sendDataPacket(packet, DataPacket_Kind.RELIABLE);

    let chunkId = 0;
    const writeMutex = new Mutex();
    const engine = this.engine;
    const logLocal = this.log;

    const writableStream = new WritableStream<Uint8Array>({
      async write(chunk) {
        const unlock = await writeMutex.lock();

        let byteOffset = 0;
        try {
          while (byteOffset < chunk.byteLength) {
            const subChunk = chunk.slice(byteOffset, byteOffset + STREAM_CHUNK_SIZE);
            await engine.waitForBufferStatusLow(DataPacket_Kind.RELIABLE);
            const chunkPacket = new DataPacket({
              destinationIdentities,
              value: {
                case: 'streamChunk',
                value: new DataStream_Chunk({
                  content: subChunk,
                  streamId,
                  chunkIndex: numberToBigInt(chunkId),
                }),
              },
            });
            await engine.sendDataPacket(chunkPacket, DataPacket_Kind.RELIABLE);
            chunkId += 1;
            byteOffset += subChunk.byteLength;
          }
        } finally {
          unlock();
        }
      },
      async close() {
        const trailer = new DataStream_Trailer({
          streamId,
        });
        const trailerPacket = new DataPacket({
          destinationIdentities,
          value: {
            case: 'streamTrailer',
            value: trailer,
          },
        });
        await engine.sendDataPacket(trailerPacket, DataPacket_Kind.RELIABLE);
      },
      abort(err) {
        logLocal.error('Sink error:', err);
      },
    });

    const byteWriter = new ByteStreamWriter(writableStream, info);

    return byteWriter;
  }
}

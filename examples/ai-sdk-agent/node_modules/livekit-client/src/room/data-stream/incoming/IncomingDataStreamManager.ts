import {
  type DataPacket,
  DataStream_Chunk,
  DataStream_Header,
  DataStream_Trailer,
  Encryption_Type,
} from '@livekit/protocol';
import log from '../../../logger';
import { DataStreamError, DataStreamErrorReason } from '../../errors';
import { type ByteStreamInfo, type StreamController, type TextStreamInfo } from '../../types';
import { Future, bigIntToNumber } from '../../utils';
import {
  type ByteStreamHandler,
  ByteStreamReader,
  type TextStreamHandler,
  TextStreamReader,
} from './StreamReader';

export default class IncomingDataStreamManager {
  private log = log;

  private byteStreamControllers = new Map<string, StreamController<DataStream_Chunk>>();

  private textStreamControllers = new Map<string, StreamController<DataStream_Chunk>>();

  private byteStreamHandlers = new Map<string, ByteStreamHandler>();

  private textStreamHandlers = new Map<string, TextStreamHandler>();

  registerTextStreamHandler(topic: string, callback: TextStreamHandler) {
    if (this.textStreamHandlers.has(topic)) {
      throw new DataStreamError(
        `A text stream handler for topic "${topic}" has already been set.`,
        DataStreamErrorReason.HandlerAlreadyRegistered,
      );
    }
    this.textStreamHandlers.set(topic, callback);
  }

  unregisterTextStreamHandler(topic: string) {
    this.textStreamHandlers.delete(topic);
  }

  registerByteStreamHandler(topic: string, callback: ByteStreamHandler) {
    if (this.byteStreamHandlers.has(topic)) {
      throw new DataStreamError(
        `A byte stream handler for topic "${topic}" has already been set.`,
        DataStreamErrorReason.HandlerAlreadyRegistered,
      );
    }
    this.byteStreamHandlers.set(topic, callback);
  }

  unregisterByteStreamHandler(topic: string) {
    this.byteStreamHandlers.delete(topic);
  }

  clearHandlersAndControllers() {
    this.byteStreamControllers.clear();
    this.textStreamControllers.clear();
    this.byteStreamHandlers.clear();
    this.textStreamHandlers.clear();
  }

  validateParticipantHasNoActiveDataStreams(participantIdentity: string) {
    // Terminate any in flight data stream receives from the given participant
    const textStreamsBeingSentByDisconnectingParticipant = Array.from(
      this.textStreamControllers.entries(),
    ).filter((entry) => entry[1].sendingParticipantIdentity === participantIdentity);
    const byteStreamsBeingSentByDisconnectingParticipant = Array.from(
      this.byteStreamControllers.entries(),
    ).filter((entry) => entry[1].sendingParticipantIdentity === participantIdentity);

    if (
      textStreamsBeingSentByDisconnectingParticipant.length > 0 ||
      byteStreamsBeingSentByDisconnectingParticipant.length > 0
    ) {
      const abnormalEndError = new DataStreamError(
        `Participant ${participantIdentity} unexpectedly disconnected in the middle of sending data`,
        DataStreamErrorReason.AbnormalEnd,
      );
      for (const [id, controller] of byteStreamsBeingSentByDisconnectingParticipant) {
        controller.outOfBandFailureRejectingFuture.reject?.(abnormalEndError);
        this.byteStreamControllers.delete(id);
      }
      for (const [id, controller] of textStreamsBeingSentByDisconnectingParticipant) {
        controller.outOfBandFailureRejectingFuture.reject?.(abnormalEndError);
        this.textStreamControllers.delete(id);
      }
    }
  }

  async handleDataStreamPacket(packet: DataPacket, encryptionType: Encryption_Type) {
    switch (packet.value.case) {
      case 'streamHeader':
        return this.handleStreamHeader(
          packet.value.value,
          packet.participantIdentity,
          encryptionType,
        );
      case 'streamChunk':
        return this.handleStreamChunk(packet.value.value, encryptionType);
      case 'streamTrailer':
        return this.handleStreamTrailer(packet.value.value, encryptionType);
      default:
        throw new Error(`DataPacket of value "${packet.value.case}" is not data stream related!`);
    }
  }

  private async handleStreamHeader(
    streamHeader: DataStream_Header,
    participantIdentity: string,
    encryptionType: Encryption_Type,
  ) {
    if (streamHeader.contentHeader.case === 'byteHeader') {
      const streamHandlerCallback = this.byteStreamHandlers.get(streamHeader.topic);
      if (!streamHandlerCallback) {
        this.log.debug(
          'ignoring incoming byte stream due to no handler for topic',
          streamHeader.topic,
        );
        return;
      }

      let streamController: ReadableStreamDefaultController<DataStream_Chunk>;
      const outOfBandFailureRejectingFuture = new Future<never>();
      outOfBandFailureRejectingFuture.promise.catch((err) => {
        this.log.error(err);
      });

      const info: ByteStreamInfo = {
        id: streamHeader.streamId,
        name: streamHeader.contentHeader.value.name ?? 'unknown',
        mimeType: streamHeader.mimeType,
        size: streamHeader.totalLength ? Number(streamHeader.totalLength) : undefined,
        topic: streamHeader.topic,
        timestamp: bigIntToNumber(streamHeader.timestamp),
        attributes: streamHeader.attributes,
        encryptionType,
      };
      const stream = new ReadableStream({
        start: (controller) => {
          streamController = controller;

          if (this.textStreamControllers.has(streamHeader.streamId)) {
            throw new DataStreamError(
              `A data stream read is already in progress for a stream with id ${streamHeader.streamId}.`,
              DataStreamErrorReason.AlreadyOpened,
            );
          }

          this.byteStreamControllers.set(streamHeader.streamId, {
            info,
            controller: streamController,
            startTime: Date.now(),
            sendingParticipantIdentity: participantIdentity,
            outOfBandFailureRejectingFuture,
          });
        },
      });
      streamHandlerCallback(
        new ByteStreamReader(
          info,
          stream,
          bigIntToNumber(streamHeader.totalLength),
          outOfBandFailureRejectingFuture,
        ),
        {
          identity: participantIdentity,
        },
      );
    } else if (streamHeader.contentHeader.case === 'textHeader') {
      const streamHandlerCallback = this.textStreamHandlers.get(streamHeader.topic);
      if (!streamHandlerCallback) {
        this.log.debug(
          'ignoring incoming text stream due to no handler for topic',
          streamHeader.topic,
        );
        return;
      }

      let streamController: ReadableStreamDefaultController<DataStream_Chunk>;
      const outOfBandFailureRejectingFuture = new Future<never>();
      outOfBandFailureRejectingFuture.promise.catch((err) => {
        this.log.error(err);
      });

      const info: TextStreamInfo = {
        id: streamHeader.streamId,
        mimeType: streamHeader.mimeType,
        size: streamHeader.totalLength ? Number(streamHeader.totalLength) : undefined,
        topic: streamHeader.topic,
        timestamp: Number(streamHeader.timestamp),
        attributes: streamHeader.attributes,
        encryptionType,
      };

      const stream = new ReadableStream<DataStream_Chunk>({
        start: (controller) => {
          streamController = controller;

          if (this.textStreamControllers.has(streamHeader.streamId)) {
            throw new DataStreamError(
              `A data stream read is already in progress for a stream with id ${streamHeader.streamId}.`,
              DataStreamErrorReason.AlreadyOpened,
            );
          }

          this.textStreamControllers.set(streamHeader.streamId, {
            info,
            controller: streamController,
            startTime: Date.now(),
            sendingParticipantIdentity: participantIdentity,
            outOfBandFailureRejectingFuture,
          });
        },
      });
      streamHandlerCallback(
        new TextStreamReader(
          info,
          stream,
          bigIntToNumber(streamHeader.totalLength),
          outOfBandFailureRejectingFuture,
        ),
        { identity: participantIdentity },
      );
    }
  }

  private handleStreamChunk(chunk: DataStream_Chunk, encryptionType: Encryption_Type) {
    const fileBuffer = this.byteStreamControllers.get(chunk.streamId);
    if (fileBuffer) {
      if (fileBuffer.info.encryptionType !== encryptionType) {
        fileBuffer.controller.error(
          new DataStreamError(
            `Encryption type mismatch for stream ${chunk.streamId}. Expected ${encryptionType}, got ${fileBuffer.info.encryptionType}`,
            DataStreamErrorReason.EncryptionTypeMismatch,
          ),
        );
        this.byteStreamControllers.delete(chunk.streamId);
      } else if (chunk.content.length > 0) {
        fileBuffer.controller.enqueue(chunk);
      }
    }
    const textBuffer = this.textStreamControllers.get(chunk.streamId);
    if (textBuffer) {
      if (textBuffer.info.encryptionType !== encryptionType) {
        textBuffer.controller.error(
          new DataStreamError(
            `Encryption type mismatch for stream ${chunk.streamId}. Expected ${encryptionType}, got ${textBuffer.info.encryptionType}`,
            DataStreamErrorReason.EncryptionTypeMismatch,
          ),
        );
        this.textStreamControllers.delete(chunk.streamId);
      } else if (chunk.content.length > 0) {
        textBuffer.controller.enqueue(chunk);
      }
    }
  }

  private handleStreamTrailer(trailer: DataStream_Trailer, encryptionType: Encryption_Type) {
    const textBuffer = this.textStreamControllers.get(trailer.streamId);
    if (textBuffer) {
      if (textBuffer.info.encryptionType !== encryptionType) {
        textBuffer.controller.error(
          new DataStreamError(
            `Encryption type mismatch for stream ${trailer.streamId}. Expected ${encryptionType}, got ${textBuffer.info.encryptionType}`,
            DataStreamErrorReason.EncryptionTypeMismatch,
          ),
        );
      } else {
        textBuffer.info.attributes = { ...textBuffer.info.attributes, ...trailer.attributes };
        textBuffer.controller.close();
        this.textStreamControllers.delete(trailer.streamId);
      }
    }

    const fileBuffer = this.byteStreamControllers.get(trailer.streamId);
    if (fileBuffer) {
      if (fileBuffer.info.encryptionType !== encryptionType) {
        fileBuffer.controller.error(
          new DataStreamError(
            `Encryption type mismatch for stream ${trailer.streamId}. Expected ${encryptionType}, got ${fileBuffer.info.encryptionType}`,
            DataStreamErrorReason.EncryptionTypeMismatch,
          ),
        );
      } else {
        fileBuffer.info.attributes = { ...fileBuffer.info.attributes, ...trailer.attributes };
        fileBuffer.controller.close();
      }
      this.byteStreamControllers.delete(trailer.streamId);
    }
  }
}

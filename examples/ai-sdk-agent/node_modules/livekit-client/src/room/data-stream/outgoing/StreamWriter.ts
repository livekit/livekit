import type { BaseStreamInfo, ByteStreamInfo, TextStreamInfo } from '../../types';

class BaseStreamWriter<T, InfoType extends BaseStreamInfo> {
  protected writableStream: WritableStream<T>;

  protected defaultWriter: WritableStreamDefaultWriter<T>;

  protected onClose?: () => void;

  readonly info: InfoType;

  constructor(writableStream: WritableStream<T>, info: InfoType, onClose?: () => void) {
    this.writableStream = writableStream;
    this.defaultWriter = writableStream.getWriter();
    this.onClose = onClose;
    this.info = info;
  }

  write(chunk: T): Promise<void> {
    return this.defaultWriter.write(chunk);
  }

  async close() {
    await this.defaultWriter.close();
    this.defaultWriter.releaseLock();
    this.onClose?.();
  }
}

export class TextStreamWriter extends BaseStreamWriter<string, TextStreamInfo> {}

export class ByteStreamWriter extends BaseStreamWriter<Uint8Array, ByteStreamInfo> {}

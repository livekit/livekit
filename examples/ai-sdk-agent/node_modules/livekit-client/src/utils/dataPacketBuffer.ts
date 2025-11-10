export interface DataPacketItem {
  data: Uint8Array;
  sequence: number;
}

export class DataPacketBuffer {
  private buffer: DataPacketItem[] = [];

  private _totalSize = 0;

  push(item: DataPacketItem) {
    this.buffer.push(item);
    this._totalSize += item.data.byteLength;
  }

  pop(): DataPacketItem | undefined {
    const item = this.buffer.shift();
    if (item) {
      this._totalSize -= item.data.byteLength;
    }
    return item;
  }

  getAll(): DataPacketItem[] {
    return this.buffer.slice();
  }

  popToSequence(sequence: number) {
    while (this.buffer.length > 0) {
      const first = this.buffer[0];
      if (first.sequence <= sequence) {
        this.pop();
      } else {
        break;
      }
    }
  }

  alignBufferedAmount(bufferedAmount: number) {
    while (this.buffer.length > 0) {
      const first = this.buffer[0];
      if (this._totalSize - first.data.byteLength <= bufferedAmount) {
        break;
      }
      this.pop();
    }
  }

  get length(): number {
    return this.buffer.length;
  }
}

/* eslint-disable @typescript-eslint/no-unused-vars */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { WebSocketStream } from './WebSocketStream';

// Mock WebSocket
class MockWebSocket {
  static CONNECTING = 0;

  static OPEN = 1;

  static CLOSING = 2;

  static CLOSED = 3;

  url: string;

  protocol: string;

  extensions: string;

  readyState: number;

  binaryType: BinaryType;

  onopen: ((event: Event) => void) | null = null;

  onclose: ((event: CloseEvent) => void) | null = null;

  onerror: ((event: Event) => void) | null = null;

  onmessage: ((event: MessageEvent) => void) | null = null;

  private eventListeners: Map<string, Set<EventListener>> = new Map();

  constructor(url: string, protocols?: string | string[]) {
    this.url = url;
    this.protocol = Array.isArray(protocols) && protocols.length > 0 ? protocols[0] : '';
    this.extensions = '';
    this.readyState = MockWebSocket.CONNECTING;
    this.binaryType = 'arraybuffer';
  }

  send(data: string | ArrayBuffer | Blob | ArrayBufferView) {
    if (this.readyState !== MockWebSocket.OPEN) {
      throw new DOMException('WebSocket is not open', 'InvalidStateError');
    }
  }

  close(code?: number, reason?: string) {
    if (this.readyState === MockWebSocket.CLOSING || this.readyState === MockWebSocket.CLOSED) {
      return;
    }
    this.readyState = MockWebSocket.CLOSING;
  }

  addEventListener(type: string, listener: EventListener) {
    if (!this.eventListeners.has(type)) {
      this.eventListeners.set(type, new Set());
    }
    this.eventListeners.get(type)!.add(listener);
  }

  removeEventListener(type: string, listener: EventListener) {
    const listeners = this.eventListeners.get(type);
    if (listeners) {
      listeners.delete(listener);
    }
  }

  dispatchEvent(event: Event): boolean {
    const listeners = this.eventListeners.get(event.type);
    if (listeners) {
      listeners.forEach((listener) => listener(event));
    }

    // Also call on* handlers
    if (event.type === 'open' && this.onopen) {
      this.onopen(event);
    } else if (event.type === 'close' && this.onclose) {
      this.onclose(event as CloseEvent);
    } else if (event.type === 'error' && this.onerror) {
      this.onerror(event);
    } else if (event.type === 'message' && this.onmessage) {
      this.onmessage(event as MessageEvent);
    }

    return true;
  }

  // Test helpers
  triggerOpen() {
    this.readyState = MockWebSocket.OPEN;
    this.dispatchEvent(new Event('open'));
  }

  triggerClose(code: number = 1000, reason: string = '') {
    this.readyState = MockWebSocket.CLOSED;
    const closeEvent = Object.assign(new Event('close'), {
      code,
      reason,
      wasClean: code === 1000,
    }) as CloseEvent;
    this.dispatchEvent(closeEvent);
  }

  triggerError() {
    const errorEvent = new Event('error');
    this.dispatchEvent(errorEvent);
  }

  triggerMessage(data: any) {
    if (this.readyState !== MockWebSocket.OPEN) {
      throw new Error('Cannot send message when WebSocket is not open');
    }
    const messageEvent = new MessageEvent('message', { data });
    this.dispatchEvent(messageEvent);
  }
}

// Mock sleep function
vi.mock('../room/utils', () => ({
  sleep: vi.fn((duration: number) => new Promise((resolve) => setTimeout(resolve, duration))),
}));

describe('WebSocketStream', () => {
  let mockWebSocket: MockWebSocket;
  let originalWebSocket: typeof WebSocket;

  beforeEach(() => {
    vi.clearAllMocks();

    // Store original WebSocket
    originalWebSocket = global.WebSocket;

    // Mock WebSocket globally
    global.WebSocket = vi.fn((url: string, protocols?: string | string[]) => {
      mockWebSocket = new MockWebSocket(url, protocols);
      return mockWebSocket as any;
    }) as any;

    // Add constants to the mocked WebSocket
    (global.WebSocket as any).CONNECTING = MockWebSocket.CONNECTING;
    (global.WebSocket as any).OPEN = MockWebSocket.OPEN;
    (global.WebSocket as any).CLOSING = MockWebSocket.CLOSING;
    (global.WebSocket as any).CLOSED = MockWebSocket.CLOSED;
  });

  afterEach(() => {
    // Restore original WebSocket
    global.WebSocket = originalWebSocket;
  });

  describe('Constructor and Initialization', () => {
    it('should create WebSocketStream with URL and protocols', () => {
      const wsStream = new WebSocketStream('wss://test.example.com', {
        protocols: ['protocol1', 'protocol2'],
      });

      expect(wsStream.url).toBe('wss://test.example.com');
      expect(mockWebSocket.url).toBe('wss://test.example.com');
      expect(mockWebSocket.binaryType).toBe('arraybuffer');
      expect(mockWebSocket.protocol).toBe('protocol1');
      expect(wsStream.readyState).toBe(MockWebSocket.CONNECTING);

      mockWebSocket.triggerOpen();
      expect(wsStream.readyState).toBe(MockWebSocket.OPEN);
    });

    it('should throw when signal is already aborted', () => {
      const abortController = new AbortController();
      abortController.abort();

      expect(() => {
        new WebSocketStream('wss://test.example.com', {
          signal: abortController.signal,
        });
      }).toThrow('This operation was aborted');
    });

    it('should close when abort signal is triggered', () => {
      const abortController = new AbortController();
      const wsStream = new WebSocketStream('wss://test.example.com', {
        signal: abortController.signal,
      });

      const closeSpy = vi.spyOn(mockWebSocket, 'close');
      abortController.abort();

      expect(closeSpy).toHaveBeenCalled();
    });
  });

  describe('opened Promise', () => {
    it('should resolve with readable/writable streams and remove error listener', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com', {
        protocols: ['test-protocol'],
      });

      mockWebSocket.protocol = 'test-protocol';
      mockWebSocket.extensions = 'test-extension';
      const removeEventListenerSpy = vi.spyOn(mockWebSocket, 'removeEventListener');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      expect(connection.readable).toBeInstanceOf(ReadableStream);
      expect(connection.writable).toBeInstanceOf(WritableStream);
      expect(connection.protocol).toBe('test-protocol');
      expect(connection.extensions).toBe('test-extension');
      expect(removeEventListenerSpy).toHaveBeenCalledWith('error', expect.any(Function));
    });

    it('should reject when WebSocket errors before opening', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerError();

      await expect(wsStream.opened).rejects.toThrow();
    });
  });

  describe('closed Promise', () => {
    it('should resolve with close code and reason, removing error listener', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');
      const removeEventListenerSpy = vi.spyOn(mockWebSocket, 'removeEventListener');

      mockWebSocket.triggerOpen();
      mockWebSocket.triggerClose(1001, 'Going away');

      const closeInfo = await wsStream.closed;

      expect(closeInfo.closeCode).toBe(1001);
      expect(closeInfo.reason).toBe('Going away');
      expect(removeEventListenerSpy).toHaveBeenCalledWith('error', expect.any(Function));
    });

    it('should handle error followed by close event', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      mockWebSocket.triggerError();
      mockWebSocket.triggerClose(1006, 'Connection failed');

      const closeInfo = await wsStream.closed;

      expect(closeInfo.closeCode).toBe(1006);
      expect(closeInfo.reason).toBe('Connection failed');
    });

    it('should reject when error occurs without timely close event', async () => {
      const { sleep } = await import('../room/utils');
      vi.mocked(sleep).mockResolvedValue(undefined);

      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      mockWebSocket.triggerError();

      await expect(wsStream.closed).rejects.toThrow(
        'Encountered unspecified websocket error without a timely close event',
      );
    });
  });

  describe('ReadableStream behavior', () => {
    it('should enqueue and read multiple messages (ArrayBuffer and string)', async () => {
      const wsStream = new WebSocketStream<ArrayBuffer | string>('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();

      const message1 = new ArrayBuffer(8);
      const message2 = 'Hello, World!';

      mockWebSocket.triggerMessage(message1);
      mockWebSocket.triggerMessage(message2);

      const result1 = await reader.read();
      expect(result1.done).toBe(false);
      expect(result1.value).toBe(message1);

      const result2 = await reader.read();
      expect(result2.done).toBe(false);
      expect(result2.value).toBe(message2);

      reader.releaseLock();
    });

    it('should error readable stream when WebSocket errors', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();

      mockWebSocket.triggerError();

      await Promise.all([
        expect(reader.read()).rejects.toBeDefined(),
        expect(wsStream.closed).rejects.toBeDefined(),
      ]);
    });

    it('should close WebSocket with custom close info when cancelled', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();
      const closeSpy = vi.spyOn(mockWebSocket, 'close');

      await reader.cancel({ closeCode: 1001, reason: 'Client is leaving' });

      expect(closeSpy).toHaveBeenCalledWith(1001, 'Client is leaving');
    });

    it('should throw when getting a second reader (locked stream)', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader1 = connection.readable.getReader();

      expect(() => connection.readable.getReader()).toThrow();

      reader1.releaseLock();
    });
  });

  describe('WritableStream behavior', () => {
    it('should send multiple chunks through WebSocket', async () => {
      const wsStream = new WebSocketStream<ArrayBuffer | string>('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const writer = connection.writable.getWriter();
      const sendSpy = vi.spyOn(mockWebSocket, 'send');

      const chunk1 = new ArrayBuffer(8);
      const chunk2 = 'Hello, WebSocket!';
      const chunk3 = new ArrayBuffer(16);

      await writer.write(chunk1);
      await writer.write(chunk2);
      await writer.write(chunk3);

      expect(sendSpy).toHaveBeenCalledTimes(3);
      expect(sendSpy).toHaveBeenNthCalledWith(1, chunk1);
      expect(sendSpy).toHaveBeenNthCalledWith(2, chunk2);
      expect(sendSpy).toHaveBeenNthCalledWith(3, chunk3);

      await writer.close();
    });

    it('should close WebSocket when writable stream is closed or aborted', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const writer = connection.writable.getWriter();
      const closeSpy = vi.spyOn(mockWebSocket, 'close');

      await writer.abort();

      expect(closeSpy).toHaveBeenCalledWith();
    });

    it('should throw error when writing to closed WebSocket', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const writer = connection.writable.getWriter();

      mockWebSocket.readyState = MockWebSocket.CLOSED;

      await expect(writer.write(new ArrayBuffer(8))).rejects.toThrow('WebSocket is not open');
    });
  });

  describe('close() method', () => {
    it('should close WebSocket with custom close code and reason', () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      const closeSpy = vi.spyOn(mockWebSocket, 'close');

      wsStream.close({ closeCode: 1001, reason: 'Going away' });

      expect(closeSpy).toHaveBeenCalledWith(1001, 'Going away');
    });

    it('should handle close with partial or no arguments', () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      const closeSpy = vi.spyOn(mockWebSocket, 'close');

      wsStream.close({ closeCode: 1000 });
      expect(closeSpy).toHaveBeenCalledWith(1000, undefined);

      wsStream.close();
      expect(closeSpy).toHaveBeenCalledWith(undefined, undefined);
    });
  });

  describe('AbortSignal integration', () => {
    it('should close WebSocket when AbortSignal is triggered at any stage', async () => {
      const abortController = new AbortController();
      const wsStream = new WebSocketStream('wss://test.example.com', {
        signal: abortController.signal,
      });

      mockWebSocket.triggerOpen();
      await wsStream.opened;

      const closeSpy = vi.spyOn(mockWebSocket, 'close');

      abortController.abort();

      expect(closeSpy).toHaveBeenCalled();
    });
  });

  describe('Edge cases', () => {
    it('should handle reading from stream after WebSocket closes', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();

      // Send a message then close
      mockWebSocket.triggerMessage(new ArrayBuffer(8));
      mockWebSocket.triggerClose(1000);

      // Should still be able to read the buffered message
      const result = await reader.read();
      expect(result.done).toBe(false);
    });

    it('should have proper readyState transitions', () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      expect(wsStream.readyState).toBe(MockWebSocket.CONNECTING);

      mockWebSocket.triggerOpen();
      expect(wsStream.readyState).toBe(MockWebSocket.OPEN);

      mockWebSocket.readyState = MockWebSocket.CLOSING;
      expect(wsStream.readyState).toBe(MockWebSocket.CLOSING);

      mockWebSocket.triggerClose(1000);
      expect(wsStream.readyState).toBe(MockWebSocket.CLOSED);
    });
  });

  describe('Stream integration', () => {
    it('should support simultaneous reading and writing', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();
      const writer = connection.writable.getWriter();

      // Write data
      const outgoingData = new ArrayBuffer(8);
      const writePromise = writer.write(outgoingData);

      // Receive data
      const incomingData = new ArrayBuffer(16);
      mockWebSocket.triggerMessage(incomingData);

      await writePromise;
      const readResult = await reader.read();

      expect(readResult.value).toBe(incomingData);

      reader.releaseLock();
      await writer.close();
    });

    it('should handle piping streams to WebSocket writable', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const sourceData = [new ArrayBuffer(8), new ArrayBuffer(16), new ArrayBuffer(32)];
      let dataIndex = 0;

      const sourceStream = new ReadableStream({
        pull(controller) {
          if (dataIndex < sourceData.length) {
            controller.enqueue(sourceData[dataIndex++]);
          } else {
            controller.close();
          }
        },
      });

      const sendSpy = vi.spyOn(mockWebSocket, 'send');

      await sourceStream.pipeTo(connection.writable);

      expect(sendSpy).toHaveBeenCalledTimes(3);
      expect(sendSpy).toHaveBeenNthCalledWith(1, sourceData[0]);
      expect(sendSpy).toHaveBeenNthCalledWith(2, sourceData[1]);
      expect(sendSpy).toHaveBeenNthCalledWith(3, sourceData[2]);
    });
  });

  describe('Complex scenarios', () => {
    it('should handle multiple messages queued before first read', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const msg1 = new ArrayBuffer(8);
      const msg2 = new ArrayBuffer(16);
      const msg3 = new ArrayBuffer(32);

      mockWebSocket.triggerMessage(msg1);
      mockWebSocket.triggerMessage(msg2);
      mockWebSocket.triggerMessage(msg3);

      const reader = connection.readable.getReader();

      const result1 = await reader.read();
      expect(result1.value).toBe(msg1);

      const result2 = await reader.read();
      expect(result2.value).toBe(msg2);

      const result3 = await reader.read();
      expect(result3.value).toBe(msg3);

      reader.releaseLock();
    });

    it('should handle error during active read operation', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();

      // Start a read that will be interrupted by an error
      const readPromise = reader.read();

      // Trigger error while read is pending
      mockWebSocket.triggerError();

      await Promise.all([
        expect(readPromise).rejects.toBeDefined(),
        expect(wsStream.closed).rejects.toBeDefined(),
      ]);
    });

    it('should support zero-length and empty messages', async () => {
      const wsStream = new WebSocketStream<ArrayBuffer | string>('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();
      const writer = connection.writable.getWriter();

      const emptyBuffer = new ArrayBuffer(0);
      await writer.write(emptyBuffer);
      await writer.write('');

      mockWebSocket.triggerMessage(emptyBuffer);
      mockWebSocket.triggerMessage('');

      const result1 = await reader.read();
      expect(result1.value).toBe(emptyBuffer);
      expect((result1.value as ArrayBuffer).byteLength).toBe(0);

      const result2 = await reader.read();
      expect(result2.value).toBe('');

      reader.releaseLock();
      await writer.close();
    });

    it('should preserve message order under load', async () => {
      const wsStream = new WebSocketStream('wss://test.example.com');

      mockWebSocket.triggerOpen();
      const connection = await wsStream.opened;

      const reader = connection.readable.getReader();

      // Send 100 messages rapidly
      const messageCount = 100;
      for (let i = 0; i < messageCount; i++) {
        const buffer = new ArrayBuffer(4);
        const view = new Uint32Array(buffer);
        view[0] = i;
        mockWebSocket.triggerMessage(buffer);
      }

      // Read all messages and verify order
      for (let i = 0; i < messageCount; i++) {
        const result = await reader.read();
        const view = new Uint32Array(result.value as ArrayBuffer);
        expect(view[0]).toBe(i);
      }

      reader.releaseLock();
    });
  });
});

import { EventEmitter } from 'events';
import type TypedEmitter from 'typed-emitter';
import type { RoomConnectOptions, RoomOptions } from '../../options';
import type RTCEngine from '../../room/RTCEngine';
import Room, { ConnectionState } from '../../room/Room';
import { RoomEvent } from '../../room/events';
import type { SimulationScenario } from '../../room/types';
import { sleep } from '../../room/utils';

type LogMessage = {
  level: 'info' | 'warning' | 'error';
  message: string;
};

export enum CheckStatus {
  IDLE,
  RUNNING,
  SKIPPED,
  SUCCESS,
  FAILED,
}

export type CheckInfo = {
  name: string;
  logs: Array<LogMessage>;
  status: CheckStatus;
  description: string;
  data?: any;
};

export interface CheckerOptions {
  errorsAsWarnings?: boolean;
  roomOptions?: RoomOptions;
  connectOptions?: RoomConnectOptions;
  protocol?: 'udp' | 'tcp';
}

export abstract class Checker extends (EventEmitter as new () => TypedEmitter<CheckerCallbacks>) {
  protected url: string;

  protected token: string;

  room: Room;

  connectOptions?: RoomConnectOptions;

  status: CheckStatus = CheckStatus.IDLE;

  logs: Array<LogMessage> = [];

  name: string;

  options: CheckerOptions = {};

  constructor(url: string, token: string, options: CheckerOptions = {}) {
    super();
    this.url = url;
    this.token = token;
    this.name = this.constructor.name;
    this.room = new Room(options.roomOptions);
    this.connectOptions = options.connectOptions;
    this.options = options;
  }

  abstract get description(): string;

  protected abstract perform(): Promise<void>;

  async run(onComplete?: () => void) {
    if (this.status !== CheckStatus.IDLE) {
      throw Error('check is running already');
    }
    this.setStatus(CheckStatus.RUNNING);

    try {
      await this.perform();
    } catch (err) {
      if (err instanceof Error) {
        if (this.options.errorsAsWarnings) {
          this.appendWarning(err.message);
        } else {
          this.appendError(err.message);
        }
      }
    }

    await this.disconnect();

    // sleep for a bit to ensure disconnect
    await new Promise((resolve) => setTimeout(resolve, 500));

    // @ts-ignore
    if (this.status !== CheckStatus.SKIPPED) {
      this.setStatus(this.isSuccess() ? CheckStatus.SUCCESS : CheckStatus.FAILED);
    }

    if (onComplete) {
      onComplete();
    }
    return this.getInfo();
  }

  protected isSuccess(): boolean {
    return !this.logs.some((l) => l.level === 'error');
  }

  protected async connect(url?: string): Promise<Room> {
    if (this.room.state === ConnectionState.Connected) {
      return this.room;
    }
    if (!url) {
      url = this.url;
    }
    await this.room.connect(url, this.token, this.connectOptions);
    return this.room;
  }

  protected async disconnect() {
    if (this.room && this.room.state !== ConnectionState.Disconnected) {
      await this.room.disconnect();
      // wait for it to go through
      await new Promise((resolve) => setTimeout(resolve, 500));
    }
  }

  protected skip() {
    this.setStatus(CheckStatus.SKIPPED);
  }

  protected async switchProtocol(protocol: 'udp' | 'tcp' | 'tls') {
    let hasReconnecting = false;
    let hasReconnected = false;
    this.room.on(RoomEvent.Reconnecting, () => {
      hasReconnecting = true;
    });
    this.room.once(RoomEvent.Reconnected, () => {
      hasReconnected = true;
    });
    this.room.simulateScenario(`force-${protocol}` as SimulationScenario);
    await new Promise((resolve) => setTimeout(resolve, 1000));
    if (!hasReconnecting) {
      // no need to wait for reconnection
      return;
    }

    // wait for 10 seconds for reconnection
    const timeout = Date.now() + 10000;
    while (Date.now() < timeout) {
      if (hasReconnected) {
        return;
      }
      await sleep(100);
    }
    throw new Error(`Could not reconnect using ${protocol} protocol after 10 seconds`);
  }

  protected appendMessage(message: string) {
    this.logs.push({ level: 'info', message });
    this.emit('update', this.getInfo());
  }

  protected appendWarning(message: string) {
    this.logs.push({ level: 'warning', message });
    this.emit('update', this.getInfo());
  }

  protected appendError(message: string) {
    this.logs.push({ level: 'error', message });
    this.emit('update', this.getInfo());
  }

  protected setStatus(status: CheckStatus) {
    this.status = status;
    this.emit('update', this.getInfo());
  }

  protected get engine(): RTCEngine | undefined {
    return this.room?.engine;
  }

  getInfo(): CheckInfo {
    return {
      logs: this.logs,
      name: this.name,
      status: this.status,
      description: this.description,
    };
  }
}
export type InstantiableCheck<T extends Checker> = {
  new (url: string, token: string, options?: CheckerOptions): T;
};

type CheckerCallbacks = {
  update: (info: CheckInfo) => void;
};

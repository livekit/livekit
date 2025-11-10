export class Mutex {
  private _locking: Promise<void>;

  private _locks: number;

  constructor() {
    this._locking = Promise.resolve();
    this._locks = 0;
  }

  isLocked() {
    return this._locks > 0;
  }

  lock() {
    this._locks += 1;

    let unlockNext: () => void;

    const willLock = new Promise<void>(
      (resolve) =>
        (unlockNext = () => {
          this._locks -= 1;
          resolve();
        }),
    );

    const willUnlock = this._locking.then(() => unlockNext);

    this._locking = this._locking.then(() => willLock);

    return willUnlock;
  }
}

export class MultiMutex {
  private _queue: (() => void)[];
  private _limit: number;
  private _locks: number;

  constructor(limit: number) {
    this._queue = [];
    this._limit = limit;
    this._locks = 0;
  }

  isLocked() {
    return this._locks >= this._limit;
  }

  async lock(): Promise<() => void> {
    if (!this.isLocked()) {
      this._locks++;
      return this._unlock.bind(this);
    }

    return new Promise((resolve) => {
      this._queue.push(() => {
        this._locks++;
        resolve(this._unlock.bind(this));
      });
    });
  }

  private _unlock() {
    this._locks--;
    if (this._queue.length && !this.isLocked()) {
      const nextUnlock = this._queue.shift();
      nextUnlock?.();
    }
  }
}

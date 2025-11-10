export class TTLMap<K, V> {
  private _map = new Map<K, { value: V; expiresAt: number }>();

  private ttl: number;

  private _lastCleanup = 0;

  /**
   * @param ttl ttl of the key (ms)
   */
  constructor(ttl: number) {
    this.ttl = ttl;
  }

  set(key: K, value: V) {
    const now = Date.now();
    if (now - this._lastCleanup > this.ttl / 2) {
      this.cleanup();
    }
    const expiresAt = now + this.ttl;
    this._map.set(key, { value, expiresAt });
    return this;
  }

  get(key: K): V | undefined {
    const entry = this._map.get(key);
    if (!entry) return undefined;
    if (entry.expiresAt < Date.now()) {
      this._map.delete(key);
      return undefined;
    }
    return entry.value;
  }

  has(key: K): boolean {
    const entry = this._map.get(key);
    if (!entry) return false;
    if (entry.expiresAt < Date.now()) {
      this._map.delete(key);
      return false;
    }
    return true;
  }

  delete(key: K): boolean {
    return this._map.delete(key);
  }

  clear() {
    this._map.clear();
  }

  cleanup() {
    const now = Date.now();
    for (const [key, entry] of this._map.entries()) {
      if (entry.expiresAt < now) {
        this._map.delete(key);
      }
    }
    this._lastCleanup = now;
  }

  get size() {
    this.cleanup();
    return this._map.size;
  }

  forEach(callback: (value: V, key: K, map: Map<K, V>) => void) {
    this.cleanup();
    for (const [key, entry] of this._map.entries()) {
      if (entry.expiresAt >= Date.now()) {
        callback(entry.value, key, this.asValueMap());
      }
    }
  }

  map<U>(callback: (value: V, key: K, map: Map<K, V>) => U): U[] {
    this.cleanup();
    const result: U[] = [];
    const valueMap = this.asValueMap();
    for (const [key, value] of valueMap.entries()) {
      result.push(callback(value, key, valueMap));
    }
    return result;
  }

  private asValueMap(): Map<K, V> {
    const result = new Map<K, V>();
    for (const [key, entry] of this._map.entries()) {
      if (entry.expiresAt >= Date.now()) {
        result.set(key, entry.value);
      }
    }
    return result;
  }
}

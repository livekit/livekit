import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { cloneDeep } from './cloneDeep';

describe('cloneDeep', () => {
  beforeEach(() => {
    global.structuredClone = vi.fn((val) => {
      return JSON.parse(JSON.stringify(val));
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('should clone a simple object', () => {
    const structuredCloneSpy = vi.spyOn(global, 'structuredClone');

    const original = { name: 'John', age: 30 };
    const cloned = cloneDeep(original);

    expect(cloned).toEqual(original);
    expect(cloned).not.toBe(original);
    expect(structuredCloneSpy).toHaveBeenCalledTimes(1);
  });

  it('should clone an object with nested properties', () => {
    const structuredCloneSpy = vi.spyOn(global, 'structuredClone');

    const original = { name: 'John', age: 30, children: [{ name: 'Mark', age: 7 }] };
    const cloned = cloneDeep(original);

    expect(cloned).toEqual(original);
    expect(cloned).not.toBe(original);
    expect(cloned?.children).not.toBe(original.children);
    expect(structuredCloneSpy).toHaveBeenCalledTimes(1);
  });

  it('should use JSON namespace as a fallback', () => {
    const structuredCloneSpy = vi.spyOn(global, 'structuredClone');
    const serializeSpy = vi.spyOn(JSON, 'stringify');
    const deserializeSpy = vi.spyOn(JSON, 'parse');

    global.structuredClone = undefined as any;

    const original = { name: 'John', age: 30 };
    const cloned = cloneDeep(original);

    expect(cloned).toEqual(original);
    expect(cloned).not.toBe(original);
    expect(structuredCloneSpy).not.toHaveBeenCalled();
    expect(serializeSpy).toHaveBeenCalledTimes(1);
    expect(deserializeSpy).toHaveBeenCalledTimes(1);
  });
});

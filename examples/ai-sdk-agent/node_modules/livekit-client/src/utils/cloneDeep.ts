export function cloneDeep<T>(value: T): T {
  if (typeof value === 'undefined') {
    return value as T;
  }

  if (typeof structuredClone === 'function') {
    if (typeof value === 'object' && value !== null) {
      // ensure that the value is not a proxy by spreading it
      return structuredClone({ ...value });
    }
    return structuredClone(value);
  } else {
    return JSON.parse(JSON.stringify(value)) as T;
  }
}

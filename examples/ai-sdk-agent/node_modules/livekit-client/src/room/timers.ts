/**
 * Timers that can be overridden with platform specific implementations
 * that ensure that they are fired. These should be used when it is critical
 * that the timer fires on time.
 */
export default class CriticalTimers {
  static setTimeout: (...args: Parameters<typeof setTimeout>) => ReturnType<typeof setTimeout> = (
    ...args: Parameters<typeof setTimeout>
    // eslint-disable-next-line @typescript-eslint/no-implied-eval
  ) => setTimeout(...args);

  static setInterval: (...args: Parameters<typeof setInterval>) => ReturnType<typeof setInterval> =
    // eslint-disable-next-line @typescript-eslint/no-implied-eval
    (...args: Parameters<typeof setInterval>) => setInterval(...args);

  static clearTimeout: (
    ...args: Parameters<typeof clearTimeout>
  ) => ReturnType<typeof clearTimeout> = (...args: Parameters<typeof clearTimeout>) =>
    clearTimeout(...args);

  static clearInterval: (
    ...args: Parameters<typeof clearInterval>
  ) => ReturnType<typeof clearInterval> = (...args: Parameters<typeof clearInterval>) =>
    clearInterval(...args);
}

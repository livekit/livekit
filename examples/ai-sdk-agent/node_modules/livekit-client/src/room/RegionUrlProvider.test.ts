import type { RegionInfo, RegionSettings } from '@livekit/protocol';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { RegionUrlProvider } from './RegionUrlProvider';
import { ConnectionError, ConnectionErrorReason } from './errors';

// Use fake timers for testing auto-refetch
vi.useFakeTimers();

// Test helpers
function createMockRegionSettings(regions: Array<{ region: string; url: string }>): RegionSettings {
  return {
    regions: regions.map((r) => ({
      region: r.region,
      url: r.url,
    })) as RegionInfo[],
  } as RegionSettings;
}

function createMockResponse(
  status: number,
  data?: any,
  headers?: Record<string, string>,
): Response {
  const defaultHeaders = new Headers(headers || {});
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 401 ? 'Unauthorized' : status === 500 ? 'Internal Server Error' : 'OK',
    headers: defaultHeaders,
    json: vi.fn().mockResolvedValue(data),
  } as unknown as Response;
}

describe('RegionUrlProvider', () => {
  let fetchMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    // Reset the static cache before each test
    // @ts-ignore - accessing private static field for testing
    RegionUrlProvider.cache = new Map();
    // @ts-ignore - accessing private static field for testing
    if (RegionUrlProvider.settingsTimeout) {
      // @ts-ignore
      clearTimeout(RegionUrlProvider.settingsTimeout);
    }

    // Mock fetch
    fetchMock = vi.fn();
    vi.stubGlobal('fetch', fetchMock);
  });

  afterEach(() => {
    vi.clearAllTimers();
    vi.restoreAllMocks();
  });

  describe('constructor and basic methods', () => {
    it('constructs with valid URL and token', () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'test-token');
      expect(provider).toBeDefined();
      expect(provider.getServerUrl().toString()).toBe('wss://test.livekit.cloud/');
    });

    it('updates token correctly', () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'initial-token');
      provider.updateToken('new-token');
      // Token is private, but we can verify it's used in subsequent calls
      expect(provider).toBeDefined();
    });

    it('returns server URL', () => {
      const url = 'wss://example.livekit.cloud';
      const provider = new RegionUrlProvider(url, 'token');
      expect(provider.getServerUrl().toString()).toBe('wss://example.livekit.cloud/');
    });

    it('resets attempted regions', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      // Get first region
      const region1 = await provider.getNextBestRegionUrl();
      expect(region1).toBe('wss://us-west.livekit.cloud');

      // Reset and verify we can get the first region again
      provider.resetAttempts();
      const region2 = await provider.getNextBestRegionUrl();
      expect(region2).toBe('wss://us-west.livekit.cloud');
    });
  });

  describe('isCloud', () => {
    it('returns true for .livekit.cloud domains', () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      expect(provider.isCloud()).toBe(true);
    });

    it('returns true for .livekit.run domains', () => {
      const provider = new RegionUrlProvider('wss://test.livekit.run', 'token');
      expect(provider.isCloud()).toBe(true);
    });

    it('returns false for non-cloud domains', () => {
      const provider = new RegionUrlProvider('wss://self-hosted.example.com', 'token');
      expect(provider.isCloud()).toBe(false);
    });

    it('returns false for localhost', () => {
      const provider = new RegionUrlProvider('ws://localhost:7880', 'token');
      expect(provider.isCloud()).toBe(false);
    });
  });

  describe('fetchRegionSettings', () => {
    it('fetches successfully with valid token and response', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const result = await provider.fetchRegionSettings();

      expect(fetchMock).toHaveBeenCalledWith('https://test.livekit.cloud/settings/regions', {
        headers: { authorization: 'Bearer token' },
        signal: undefined,
      });
      expect(result.regionSettings).toEqual(mockSettings);
      expect(result.maxAgeInMs).toBe(3600000);
    });

    it('converts wss to https for settings URL', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(200, createMockRegionSettings([])));

      await provider.fetchRegionSettings();

      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('https://test.livekit.cloud'),
        expect.anything(),
      );
    });

    it('converts ws to http for settings URL', async () => {
      const provider = new RegionUrlProvider('ws://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(200, createMockRegionSettings([])));

      await provider.fetchRegionSettings();

      expect(fetchMock).toHaveBeenCalledWith(
        expect.stringContaining('http://test.livekit.cloud'),
        expect.anything(),
      );
    });

    it('respects abort signal', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const abortController = new AbortController();
      fetchMock.mockResolvedValue(createMockResponse(200, createMockRegionSettings([])));

      await provider.fetchRegionSettings(abortController.signal);

      expect(fetchMock).toHaveBeenCalledWith(expect.anything(), {
        headers: { authorization: 'Bearer token' },
        signal: abortController.signal,
      });
    });

    it('throws ConnectionError with NotAllowed for 401 response', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(401));

      await expect(provider.fetchRegionSettings()).rejects.toThrow(ConnectionError);
      await expect(provider.fetchRegionSettings()).rejects.toMatchObject({
        reason: ConnectionErrorReason.NotAllowed,
        status: 401,
      });
    });

    it('throws ConnectionError with InternalError for 500 response', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(500));

      await expect(provider.fetchRegionSettings()).rejects.toThrow(ConnectionError);
      await expect(provider.fetchRegionSettings()).rejects.toMatchObject({
        reason: ConnectionErrorReason.InternalError,
        status: 500,
      });
    });

    it('extracts max-age from Cache-Control header', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(
        createMockResponse(200, createMockRegionSettings([]), {
          'Cache-Control': 'max-age=7200',
        }),
      );

      const result = await provider.fetchRegionSettings();
      expect(result.maxAgeInMs).toBe(7200000);
    });

    it('uses default max-age when Cache-Control is missing', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(200, createMockRegionSettings([])));

      const result = await provider.fetchRegionSettings();
      expect(result.maxAgeInMs).toBe(5000); // DEFAULT_MAX_AGE_MS
    });

    it('uses default max-age when max-age is not in Cache-Control', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(
        createMockResponse(200, createMockRegionSettings([]), {
          'Cache-Control': 'public, no-cache',
        }),
      );

      const result = await provider.fetchRegionSettings();
      expect(result.maxAgeInMs).toBe(5000);
    });

    it('sets updatedAtInMs to current time', async () => {
      const now = Date.now();
      vi.setSystemTime(now);

      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      fetchMock.mockResolvedValue(createMockResponse(200, createMockRegionSettings([])));

      const result = await provider.fetchRegionSettings();
      expect(result.updatedAtInMs).toBe(now);
    });
  });

  describe('getNextBestRegionUrl', () => {
    it('throws error for non-cloud domains', async () => {
      const provider = new RegionUrlProvider('wss://self-hosted.example.com', 'token');

      await expect(provider.getNextBestRegionUrl()).rejects.toThrow(
        'region availability is only supported for LiveKit Cloud domains',
      );
    });

    it('returns first region on initial call', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const region = await provider.getNextBestRegionUrl();
      expect(region).toBe('wss://us-west.livekit.cloud');
    });

    it('returns subsequent regions on repeated calls', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
        { region: 'eu-central', url: 'wss://eu-central.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const region1 = await provider.getNextBestRegionUrl();
      const region2 = await provider.getNextBestRegionUrl();
      const region3 = await provider.getNextBestRegionUrl();

      expect(region1).toBe('wss://us-west.livekit.cloud');
      expect(region2).toBe('wss://us-east.livekit.cloud');
      expect(region3).toBe('wss://eu-central.livekit.cloud');
    });

    it('returns null when all regions exhausted', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      await provider.getNextBestRegionUrl();
      const region = await provider.getNextBestRegionUrl();

      expect(region).toBeNull();
    });

    it('uses cached settings when available and fresh', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      // First call should fetch
      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Create new provider with same host
      const provider2 = new RegionUrlProvider('wss://test.livekit.cloud', 'token');

      // Second call should use cache
      await provider2.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1); // Still 1, no new fetch
    });

    it('fetches new settings when cache is expired', async () => {
      const now = Date.now();
      vi.setSystemTime(now);

      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=1' }), // 1 second TTL
      );

      // First call
      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Advance time beyond TTL
      vi.setSystemTime(now + 2000);

      // Create new provider and call again
      const provider2 = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      await provider2.getNextBestRegionUrl();

      expect(fetchMock).toHaveBeenCalledTimes(2); // Should fetch again
    });

    it('fetches new settings when cache is empty', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalled();
    });

    it('filters out already attempted regions', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const region1 = await provider.getNextBestRegionUrl();
      const region2 = await provider.getNextBestRegionUrl();

      expect(region1).toBe('wss://us-west.livekit.cloud');
      expect(region2).toBe('wss://us-east.livekit.cloud');
      expect(region1).not.toBe(region2);
    });

    it('works correctly after resetAttempts', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      // Exhaust regions
      await provider.getNextBestRegionUrl();
      let region = await provider.getNextBestRegionUrl();
      expect(region).toBeNull();

      // Reset and try again
      provider.resetAttempts();
      region = await provider.getNextBestRegionUrl();
      expect(region).toBe('wss://us-west.livekit.cloud');
    });

    it('respects abort signal', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const abortController = new AbortController();
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(createMockResponse(200, mockSettings));

      await provider.getNextBestRegionUrl(abortController.signal);

      expect(fetchMock).toHaveBeenCalledWith(expect.anything(), {
        headers: expect.anything(),
        signal: abortController.signal,
      });
    });
  });

  describe('caching behavior', () => {
    it('cache is shared across multiple instances with same hostname', async () => {
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const provider1 = new RegionUrlProvider('wss://test.livekit.cloud', 'token1');
      await provider1.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Different token, same hostname - should use cache
      const provider2 = new RegionUrlProvider('wss://test.livekit.cloud', 'token2');
      await provider2.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1); // Still 1
    });

    it('cache is separate for different hostnames', async () => {
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const provider1 = new RegionUrlProvider('wss://test1.livekit.cloud', 'token');
      await provider1.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Different hostname - should fetch again
      const provider2 = new RegionUrlProvider('wss://test2.livekit.cloud', 'token');
      await provider2.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    it('cache keys by hostname without port or protocol', async () => {
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const provider1 = new RegionUrlProvider('wss://test.livekit.cloud:443', 'token');
      await provider1.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Same hostname, different protocol/port - should use cache
      const provider2 = new RegionUrlProvider('https://test.livekit.cloud', 'token');
      await provider2.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);
    });
  });

  describe('auto-refetch mechanism', () => {
    it('schedules auto-refetch with correct max-age duration', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=10' }),
      );

      await provider.getNextBestRegionUrl();

      // Verify timeout is set (we can't easily check the exact duration without accessing internals)
      expect(vi.getTimerCount()).toBeGreaterThan(0);
    });

    it('auto-refetch updates cache on success', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const initialSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);
      const updatedSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      fetchMock
        .mockResolvedValueOnce(
          createMockResponse(200, initialSettings, { 'Cache-Control': 'max-age=100' }),
        )
        .mockResolvedValue(
          createMockResponse(200, updatedSettings, { 'Cache-Control': 'max-age=100' }),
        );

      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Advance time to trigger auto-refetch
      await vi.runOnlyPendingTimersAsync();

      // Verify the refetch happened
      expect(fetchMock).toHaveBeenCalledTimes(2);

      // Verify cache was updated
      // @ts-ignore - accessing private cache for testing
      const cached = RegionUrlProvider.cache.get('test.livekit.cloud');
      expect(cached?.regionSettings).toEqual(updatedSettings);
    });

    it('auto-refetch handles errors gracefully', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock
        .mockResolvedValueOnce(
          createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=100' }),
        )
        .mockRejectedValueOnce(new Error('Fetch failed'))
        .mockResolvedValue(
          createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=100' }),
        );

      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledTimes(1);

      // Advance time to trigger auto-refetch (which will fail)
      await vi.runOnlyPendingTimersAsync();

      // Verify fetch was attempted and failed
      expect(fetchMock).toHaveBeenCalledTimes(2);

      // Advance time again to trigger retry after error
      await vi.runOnlyPendingTimersAsync();

      // Verify it retried and succeeded
      expect(fetchMock).toHaveBeenCalledTimes(3);
    });

    it('clears previous timeout when updating cache', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=10' }),
      );

      await provider.getNextBestRegionUrl();
      const firstTimerCount = vi.getTimerCount();

      // Update cache again (should clear previous timeout)
      provider.setServerReportedRegions({
        regionSettings: mockSettings,
        updatedAtInMs: Date.now(),
        maxAgeInMs: 5000,
      });
      const secondTimerCount = vi.getTimerCount();

      // Should still have timers but not accumulating
      expect(secondTimerCount).toBeGreaterThan(0);
      expect(secondTimerCount).toBeLessThanOrEqual(firstTimerCount + 1);
    });
  });

  describe('setServerReportedRegions', () => {
    it('stores region settings in cache', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      provider.setServerReportedRegions({
        regionSettings: mockSettings,
        updatedAtInMs: Date.now(),
        maxAgeInMs: 3600000,
      });

      // Should use cached settings without fetching
      const region = await provider.getNextBestRegionUrl();
      expect(region).toBe('wss://us-west.livekit.cloud');
      expect(fetchMock).not.toHaveBeenCalled();
    });

    it('stores settings with correct fields', () => {
      const now = Date.now();
      vi.setSystemTime(now);

      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      provider.setServerReportedRegions({
        regionSettings: mockSettings,
        updatedAtInMs: now,
        maxAgeInMs: 7200000,
      });

      // @ts-ignore - accessing private cache for testing
      const cached = RegionUrlProvider.cache.get('test.livekit.cloud');
      expect(cached?.regionSettings).toEqual(mockSettings);
      expect(cached?.updatedAtInMs).toBe(now);
      expect(cached?.maxAgeInMs).toBe(7200000);
    });

    it('updates existing cache entry', () => {
      const now = Date.now();
      vi.setSystemTime(now);

      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const initialSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);
      const updatedSettings = createMockRegionSettings([
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      provider.setServerReportedRegions({
        regionSettings: initialSettings,
        updatedAtInMs: now,
        maxAgeInMs: 5000,
      });

      provider.setServerReportedRegions({
        regionSettings: updatedSettings,
        updatedAtInMs: now + 1000,
        maxAgeInMs: 10000,
      });

      // @ts-ignore - accessing private cache for testing
      const cached = RegionUrlProvider.cache.get('test.livekit.cloud');
      expect(cached?.regionSettings).toEqual(updatedSettings);
      expect(cached?.updatedAtInMs).toBe(now + 1000);
      expect(cached?.maxAgeInMs).toBe(10000);
    });

    it('triggers auto-refetch timeout', () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      const initialTimerCount = vi.getTimerCount();
      provider.setServerReportedRegions({
        regionSettings: mockSettings,
        updatedAtInMs: Date.now(),
        maxAgeInMs: 5000,
      });
      const finalTimerCount = vi.getTimerCount();

      expect(finalTimerCount).toBeGreaterThan(initialTimerCount);
    });
  });

  describe('edge cases', () => {
    it('handles empty regions list', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const region = await provider.getNextBestRegionUrl();
      expect(region).toBeNull();
    });

    it('handles malformed region settings response', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');

      fetchMock.mockResolvedValue(
        createMockResponse(200, { invalid: 'data' }, { 'Cache-Control': 'max-age=3600' }),
      );

      // Should not throw during fetch, but may have issues when accessing regions
      const result = await provider.fetchRegionSettings();
      expect(result).toBeDefined();
    });

    it('handles network timeout', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');

      fetchMock.mockRejectedValue(new Error('Network timeout'));

      await expect(provider.fetchRegionSettings()).rejects.toThrow('Network timeout');
    });

    it('handles concurrent getNextBestRegionUrl calls', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-east', url: 'wss://us-east.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      // Make concurrent calls
      const [region1, region2] = await Promise.all([
        provider.getNextBestRegionUrl(),
        provider.getNextBestRegionUrl(),
      ]);

      // Both should return regions (may be same or different depending on timing)
      expect(region1).toBeTruthy();
      expect(region2).toBeTruthy();
    });

    it('preserves cache when one instance fails to fetch', async () => {
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock
        .mockResolvedValueOnce(
          createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
        )
        .mockResolvedValueOnce(createMockResponse(500));

      // First provider populates cache
      const provider1 = new RegionUrlProvider('wss://test.livekit.cloud', 'token1');
      await provider1.getNextBestRegionUrl();

      // Advance time to expire cache
      vi.setSystemTime(Date.now() + 4000000);

      // Second provider tries to fetch but fails
      const provider2 = new RegionUrlProvider('wss://test.livekit.cloud', 'token2');
      await expect(provider2.getNextBestRegionUrl()).rejects.toThrow();

      // Cache should still be accessible by a third instance if still valid
      expect(fetchMock).toHaveBeenCalledTimes(2);
    });

    it('handles token update correctly', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'initial-token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=1' }),
      );

      // Initial fetch with initial token
      await provider.getNextBestRegionUrl();
      expect(fetchMock).toHaveBeenCalledWith(expect.anything(), {
        headers: { authorization: 'Bearer initial-token' },
        signal: undefined,
      });

      // Update token
      provider.updateToken('new-token');

      // Expire cache and fetch again
      vi.setSystemTime(Date.now() + 2000);
      const provider2 = new RegionUrlProvider('wss://test.livekit.cloud', 'new-token');
      await provider2.getNextBestRegionUrl();

      // Should use new token
      expect(fetchMock).toHaveBeenLastCalledWith(expect.anything(), {
        headers: { authorization: 'Bearer new-token' },
        signal: undefined,
      });
    });

    it('filters regions with same URL correctly', async () => {
      const provider = new RegionUrlProvider('wss://test.livekit.cloud', 'token');
      const mockSettings = createMockRegionSettings([
        { region: 'us-west-1', url: 'wss://us-west.livekit.cloud' },
        { region: 'us-west-2', url: 'wss://us-west.livekit.cloud' },
      ]);

      fetchMock.mockResolvedValue(
        createMockResponse(200, mockSettings, { 'Cache-Control': 'max-age=3600' }),
      );

      const region1 = await provider.getNextBestRegionUrl();
      const region2 = await provider.getNextBestRegionUrl();

      // First region should be returned, second should be null since it has the same URL
      expect(region1).toBe('wss://us-west.livekit.cloud');
      expect(region2).toBeNull(); // Filtered out because same URL was already attempted
    });
  });
});

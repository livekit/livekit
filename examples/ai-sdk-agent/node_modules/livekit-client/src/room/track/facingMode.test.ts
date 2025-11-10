import { describe, expect, test } from 'vitest';
import { facingModeFromDeviceLabel } from './facingMode';

describe('Test facingMode detection', () => {
  test('OBS virtual camera should be detected.', () => {
    const result = facingModeFromDeviceLabel('OBS Virtual Camera');
    expect(result?.facingMode).toEqual('environment');
    expect(result?.confidence).toEqual('medium');
  });

  test.each([
    ['Peter’s iPhone Camera', { facingMode: 'environment', confidence: 'medium' }],
    ['iPhone de Théo Camera', { facingMode: 'environment', confidence: 'medium' }],
  ])(
    'Device labels that contain "iphone" should return facingMode "environment".',
    (label, expected) => {
      const result = facingModeFromDeviceLabel(label);
      expect(result?.facingMode).toEqual(expected.facingMode);
      expect(result?.confidence).toEqual(expected.confidence);
    },
  );

  test.each([
    ['Peter’s iPad Camera', { facingMode: 'environment', confidence: 'medium' }],
    ['iPad de Théo Camera', { facingMode: 'environment', confidence: 'medium' }],
  ])('Device label that contain "ipad" should detect.', (label, expected) => {
    const result = facingModeFromDeviceLabel(label);
    expect(result?.facingMode).toEqual(expected.facingMode);
    expect(result?.confidence).toEqual(expected.confidence);
  });
});

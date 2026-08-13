import { describe, expect, it } from 'vitest';
import { sizeBytes, validName } from './validate';

describe('validName', () => {
	it('accepts DNS-shaped names', () => {
		expect(validName('media')).toBe(true);
		expect(validName('vm-disk-0')).toBe(true);
		expect(validName('a')).toBe(true);
	});
	it('rejects what the CRDs would reject', () => {
		expect(validName('')).toBe(false);
		expect(validName('Media')).toBe(false);
		expect(validName('-media')).toBe(false);
		expect(validName('media-')).toBe(false);
		expect(validName('a'.repeat(64))).toBe(false);
		expect(validName('a'.repeat(63))).toBe(true);
	});
});

describe('sizeBytes', () => {
	it('parses binary quantities', () => {
		expect(sizeBytes('1Ki')).toBe(1024);
		expect(sizeBytes('100Gi')).toBe(100 * 2 ** 30);
		expect(sizeBytes('1.5Ti')).toBe(1.5 * 2 ** 40);
		expect(sizeBytes(' 10Mi ')).toBe(10 * 2 ** 20);
	});
	it('returns null for anything else', () => {
		expect(sizeBytes('10')).toBeNull();
		expect(sizeBytes('10GB')).toBeNull();
		expect(sizeBytes('Gi')).toBeNull();
		expect(sizeBytes('-1Gi')).toBeNull();
	});
});

// Client mirrors of server-side validation: per-field hints only, never
// enforcement (the CRDs keep the authority, so kubectl and the UI are
// rejected identically).
export const NAME_HINT = 'lowercase letters, digits, dashes; max 63 chars';
export function validName(n: string): boolean {
	return /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(n);
}

// Pools name their VG, and snapshot device-mapper names count each dash twice.
export const POOL_NAME_HINT = 'lowercase letters, digits, dashes; length plus dashes at most 21';
export function validPoolName(n: string): boolean {
	return validName(n) && n.length + (n.match(/-/g) ?? []).length <= 21;
}

export const TIER_HINT = 'lowercase letters, digits, dashes; starts with a letter; max 24 chars';
export function validTier(t: string): boolean {
	return /^[a-z]([a-z0-9-]{0,22}[a-z0-9])?$/.test(t);
}

export const SIZE_HINT = 'number plus binary unit, e.g. 100Gi';
const UNITS: Record<string, number> = {
	Ki: 2 ** 10,
	Mi: 2 ** 20,
	Gi: 2 ** 30,
	Ti: 2 ** 40,
};
// Returns the byte count, or null when the string is not a valid quantity.
export function sizeBytes(s: string): number | null {
	const m = /^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti)$/.exec(s.trim());
	if (!m) return null;
	return Number(m[1]) * UNITS[m[2]];
}

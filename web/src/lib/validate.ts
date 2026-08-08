// Client mirrors of server-side validation: per-field hints only, never
// enforcement (the CRDs keep the authority, so kubectl and the UI are
// rejected identically).
export const NAME_HINT = 'lowercase letters, digits, dashes; max 63 chars';
export function validName(n: string): boolean {
	return /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/.test(n);
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

// Mutation helper: POST JSON, surface the server's message on failure so
// CRD validation errors reach the user verbatim.
export async function post(path: string, body: unknown): Promise<string> {
	const r = await fetch(path, {
		method: 'POST',
		headers: { 'Content-Type': 'application/json' },
		body: JSON.stringify(body),
	}).catch(() => undefined);
	if (!r) return 'network error';
	if (r.ok) return '';
	return (await r.text()) || `error ${r.status}`;
}

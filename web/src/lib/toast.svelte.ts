// Imperative action results surface as toasts; form validation errors stay
// inline in the owning modal or form.
type ToastKind = 'success' | 'error' | 'info';
export type Toast = { id: number; kind: ToastKind; msg: string };

class Toasts {
	list = $state<Toast[]>([]);
	#seq = 0;

	show(msg: string, kind: ToastKind = 'info') {
		const t: Toast = { id: ++this.#seq, kind, msg };
		this.list = [...this.list, t].slice(-3);
		// Errors linger longer: they carry the "what went wrong" the admin reads.
		setTimeout(() => this.dismiss(t.id), kind === 'error' ? 8000 : 5000);
	}

	dismiss(id: number) {
		this.list = this.list.filter((t) => t.id !== id);
	}
}

export const toasts = new Toasts();

<script lang="ts">
	import type { Snippet } from 'svelte';
	import { X } from 'lucide-svelte';

	// The one dialog shell: overlay, backdrop-click + Escape dismissal, focus
	// containment, and the title bar every modal shares. Callers own the body
	// markup and pass footer content into the standard bottom bar.
	let {
		title,
		size = 'md',
		danger = false,
		onclose,
		children,
		footer,
	}: {
		title: string;
		size?: 'md' | 'lg';
		// Destructive dialogs render the title in red.
		danger?: boolean;
		onclose: () => void;
		children: Snippet;
		footer?: Snippet;
	} = $props();

	const titleId = $props.id();
	const width = $derived({ md: 'max-w-md', lg: 'max-w-lg' }[size]);

	let panel = $state<HTMLDivElement>();

	const FOCUSABLE =
		'a[href], button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';

	// Focus lands inside on open ([data-autofocus] -> first focusable -> the
	// panel itself) and returns to the opener on close.
	$effect(() => {
		if (!panel) return;
		const opener = document.activeElement as HTMLElement | null;
		const target =
			panel.querySelector<HTMLElement>('[data-autofocus]') ??
			panel.querySelector<HTMLElement>(FOCUSABLE) ??
			panel;
		target.focus();
		return () => opener?.focus();
	});

	// Hand-rolled tab trap: <dialog>'s top layer would paint over the toast
	// host and take over stacking of nested dialogs.
	function trapTab(e: KeyboardEvent) {
		if (e.key !== 'Tab' || !panel) return;
		const items = [...panel.querySelectorAll<HTMLElement>(FOCUSABLE)].filter(
			(el) => el.offsetParent !== null,
		);
		if (items.length === 0) return;
		const first = items[0];
		const last = items[items.length - 1];
		const active = document.activeElement;
		if (e.shiftKey && (active === first || active === panel)) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && active === last) {
			e.preventDefault();
			first.focus();
		}
	}
</script>

<div
	class="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
	onclick={(e) => e.target === e.currentTarget && onclose()}
	onkeydown={(e) => e.key === 'Escape' && onclose()}
	role="presentation"
>
	<div
		bind:this={panel}
		role="dialog"
		aria-modal="true"
		aria-labelledby={titleId}
		tabindex="-1"
		onkeydown={trapTab}
		class="flex max-h-[90vh] w-full {width} flex-col rounded-lg border border-slate-800 bg-slate-900 shadow-xl outline-none"
	>
		<header class="flex items-center justify-between border-b border-slate-800 px-5 py-3">
			<h2 id={titleId} class="text-base font-semibold {danger ? 'text-red-400' : 'text-slate-100'}">
				{title}
			</h2>
			<button onclick={onclose} aria-label="Close" class="text-slate-500 hover:text-slate-300">
				<X size={18} />
			</button>
		</header>
		{@render children()}
		{#if footer}
			<footer class="flex items-center gap-2 border-t border-slate-800 px-5 py-3">
				{@render footer()}
			</footer>
		{/if}
	</div>
</div>

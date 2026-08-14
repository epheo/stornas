// UI phases driven by the e2e harnesses against live appliances.
// Playwright library, not its test runner: the harness owns sequencing
// because phases interleave with host-side failure injection (QMP disk
// pulls, network partitions). Needs `npx playwright install chromium`
// once per machine.
import { chromium } from 'playwright';

const url = process.env.UI_URL;
const password = process.env.ADMIN_PW;
const user = process.env.UI_USER || 'admin';
const phase = process.argv[2];

const browser = await chromium.launch();
const page = await browser.newPage();
page.setDefaultTimeout(60_000);
page.on('pageerror', (err) => {
	console.error(`page error: ${err.message}`);
	process.exitCode = 1;
});
// The dashboard links to the same pages as the sidebar; scope navigation
// to the aside so getByRole stays unambiguous.
const nav = (name) => page.locator('aside').getByRole('link', { name });

async function login(user, pw) {
	await page.goto(url);
	await page.getByPlaceholder('Username').fill(user);
	await page.getByPlaceholder('Password').fill(pw);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await nav('Dashboard').waitFor();
	// The generated password nudges a change on every load; only the
	// onboarding phase follows it.
	const cancel = page.getByRole('button', { name: 'Cancel' });
	if (await cancel.isVisible()) await cancel.click();
}

async function goTo(link, heading) {
	await nav(link).click();
	await page.getByRole('heading', { name: heading }).waitFor();
}

const phases = {
	async smoke() {
		await goTo('Pools', 'Storage pools');
		await page.getByText('test', { exact: true }).waitFor();
		await page.getByText('Online', { exact: true }).waitFor();
		if ((await page.getByText('InSync').count()) < 2) {
			throw new Error('expected both raid members InSync');
		}
		// Every page must render its live data without a script error.
		for (const [link, heading] of [
			['Dashboard', 'Dashboard'],
			['Volumes', 'Volumes'],
			['Shares', 'Shares'],
			['Targets', 'iSCSI targets'],
			['Nodes', 'Nodes'],
			['Alerts', 'Alerts'],
			['Users', 'Users'],
		]) {
			await goTo(link, heading);
		}
	},

	async 'degraded-replace'() {
		// The stream must surface the pulled disk, then the dialog drives
		// the same replace the failure matrix promises.
		await goTo('Pools', 'Storage pools');
		await page.getByText('Degraded', { exact: true }).waitFor();
		await page.getByText('Missing', { exact: true }).waitFor();
		await page
			.locator('span.inline-flex', { hasText: 'Missing' })
			.getByRole('button', { name: 'replace' })
			.click();
		await page
			.locator('label', { hasText: 'virtio-STORNASC' })
			.locator('input[type=radio]')
			.check();
		await page.getByRole('button', { name: 'Replace disk' }).click();
		await page.getByRole('heading', { name: 'Replace disk in test' }).waitFor({ state: 'hidden' });
	},

	async online() {
		await goTo('Pools', 'Storage pools');
		await page.getByText('Degraded', { exact: true }).waitFor({ state: 'hidden' });
		await page.getByText('virtio-STORNASC').first().waitFor();
		await page.getByText('Online', { exact: true }).waitFor();
		if (await page.getByText('Missing', { exact: true }).isVisible()) {
			throw new Error('dead member still shown after replace');
		}
	},

	async 'repl-pages'() {
		// Two-node data after the failover cycle: the pages must show the
		// moved placements and live replica states, not just render.
		await goTo('Nodes', 'Nodes');
		for (const n of ['node1', 'node2']) {
			await page.getByText(n, { exact: true }).first().waitFor();
		}
		await goTo('Volumes', 'Volumes');
		await page.getByText('repl-test', { exact: true }).waitFor();
		await page.getByText('node1: UpToDate', { exact: false }).first().waitFor();
		await page.getByText('node2: UpToDate', { exact: false }).first().waitFor();
		await goTo('Targets', 'iSCSI targets');
		const trow = page.locator('tr', { hasText: 'failover' });
		await trow.getByText('iqn.2026-08.io.stornas:failover').waitFor();
		await trow.getByText('node1', { exact: true }).waitFor();
		if (process.env.REPL_VIP) await trow.getByText(process.env.REPL_VIP).waitFor();
		await trow.getByText('Exported').waitFor();
		await goTo('Shares', 'Shares');
		const srow = page.locator('tr', { hasText: 'failover' });
		// The shown mount source must be the fsid=0 pseudo path a client
		// actually mounts, never the on-host directory.
		if (process.env.REPL_NFS) await srow.getByText(process.env.REPL_NFS).waitFor();
		await srow.getByText('Exported').waitFor();
	},

	async viewer() {
		// RBAC in the chrome: management affordances must not render for
		// the viewer role, matching the 403s behind them.
		await goTo('Pools', 'Storage pools');
		if (await nav('Users').isVisible()) throw new Error('viewer sees the Users page');
		if (await page.getByRole('heading', { name: 'New pool' }).isVisible()) {
			throw new Error('viewer sees the create-pool form');
		}
		await goTo('Volumes', 'Volumes');
		if (await page.getByRole('button', { name: 'delete' }).first().isVisible()) {
			throw new Error('viewer sees volume actions');
		}
	},

	async 'split-brain'() {
		// The failure-matrix promise: divergence is surfaced and resolved
		// in the UI, not by drbd incantations on the host.
		const survivor = process.env.SURVIVOR || 'node1';
		await goTo('Volumes', 'Volumes');
		await page.getByText('split brain', { exact: true }).waitFor();
		await page
			.locator('tr', { hasText: 'repl-test' })
			.getByRole('button', { name: 'resolve' })
			.click();
		const title = page.getByRole('heading', { name: 'Resolve split brain on repl-test' });
		await title.waitFor();
		await page.locator('label', { hasText: survivor }).locator('input[type=radio]').check();
		await page.getByRole('button', { name: `Keep ${survivor} and discard others` }).click();
		await title.waitFor({ state: 'hidden' });
	},
};

if (!url || !password || !phases[phase]) {
	console.error(`usage: UI_URL=... ADMIN_PW=... node ui.mjs <${Object.keys(phases).join('|')}>`);
	process.exit(2);
}

try {
	await login(user, password);
	await phases[phase]();
	console.log(`ui ${phase}: ok`);
} catch (err) {
	console.error(`ui ${phase}: ${err.message}`);
	await page.screenshot({ path: `ui-${phase}-failed.png` }).catch(() => {});
	process.exitCode = 1;
} finally {
	await browser.close();
}

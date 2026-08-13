// UI phases driven by hack/boot-test.sh against the live appliance.
// Playwright library, not its test runner: the harness owns sequencing
// because phases interleave with host-side failure injection (QMP disk
// pulls). Needs `npx playwright install chromium` once per machine.
import { chromium } from 'playwright';

const url = process.env.UI_URL;
const password = process.env.ADMIN_PW;
const phase = process.argv[2];
const phases = ['smoke', 'degraded-replace', 'online'];
if (!url || !password || !phases.includes(phase)) {
	console.error(`usage: UI_URL=... ADMIN_PW=... node ui.mjs <${phases.join('|')}>`);
	process.exit(2);
}

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

try {
	await page.goto(url);
	await page.getByPlaceholder('Username').fill('admin');
	await page.getByPlaceholder('Password').fill(password);
	await page.getByRole('button', { name: 'Sign in' }).click();
	await nav('Pools').waitFor();
	// The generated password nudges a change on every load; not this flow.
	const cancel = page.getByRole('button', { name: 'Cancel' });
	if (await cancel.isVisible()) await cancel.click();

	await nav('Pools').click();
	await page.getByRole('heading', { name: 'Storage pools' }).waitFor();

	if (phase === 'smoke') {
		await page.getByText('test', { exact: true }).waitFor();
		await page.getByText('rpool', { exact: true }).waitFor();
		if ((await page.getByText('Online', { exact: true }).count()) < 2) {
			throw new Error('expected both pools Online');
		}
		if ((await page.getByText('InSync').count()) < 3) {
			throw new Error('expected all pool members InSync');
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
			await nav(link).click();
			await page.getByRole('heading', { name: heading }).waitFor();
		}
	}

	if (phase === 'degraded-replace') {
		// The stream must surface the pulled disk, then the dialog drives
		// the same replace the failure matrix promises.
		await page.getByText('Degraded', { exact: true }).waitFor();
		await page.getByText('Missing', { exact: true }).waitFor();
		await page
			.locator('span.inline-flex', { hasText: 'Missing' })
			.getByRole('button', { name: 'replace' })
			.click();
		await page
			.locator('label', { hasText: 'virtio-STORNASD' })
			.locator('input[type=radio]')
			.check();
		await page.getByRole('button', { name: 'Replace disk' }).click();
		await page
			.getByRole('heading', { name: 'Replace disk in rpool' })
			.waitFor({ state: 'hidden' });
	}

	if (phase === 'online') {
		await page.getByText('Degraded', { exact: true }).waitFor({ state: 'hidden' });
		await page.getByText('virtio-STORNASD').first().waitFor();
		if ((await page.getByText('Online', { exact: true }).count()) < 2) {
			throw new Error('expected both pools Online after replace');
		}
		if (await page.getByText('Missing', { exact: true }).isVisible()) {
			throw new Error('dead member still shown after replace');
		}
	}

	console.log(`ui ${phase}: ok`);
} catch (err) {
	console.error(`ui ${phase}: ${err.message}`);
	await page.screenshot({ path: `ui-${phase}-failed.png` }).catch(() => {});
	process.exitCode = 1;
} finally {
	await browser.close();
}

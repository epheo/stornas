import { defineConfig } from 'vitest/config';

// Separate from vite.config.ts on purpose: the unit tests cover plain TS
// modules and must not drag the sveltekit plugin into every run.
export default defineConfig({
	test: {
		include: ['src/**/*.test.ts'],
		environment: 'node'
	}
});

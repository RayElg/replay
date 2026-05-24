import { defineConfig } from '@playwright/test';

export default defineConfig({
  use: {
    // BASE_URL in the environment's vars sets the Playwright baseURL.
    // Scripts can then use page.goto('/login') instead of hardcoding the full URL.
    baseURL: process.env.BASE_URL,
    video: 'on',
    screenshot: 'on',
    // Full trace: per-action DOM snapshots, console, network. Consumed by the runner
    // (parsed into a structured summary) and surfaced to the agent for triage.
    trace: 'on',
    // 1920×1080 gives sharper recordings and screenshots than the 1280×720 default.
    viewport: { width: 1920, height: 1080 },
    launchOptions: {
      args: ['--no-sandbox', '--disable-setuid-sandbox'],
    },
  },
  workers: 1,
});

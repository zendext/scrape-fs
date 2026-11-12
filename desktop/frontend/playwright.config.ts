import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./tests",
  workers: 1,
  timeout: 30000,
  use: {
    channel: "chromium",
    baseURL: "http://127.0.0.1:9481",
    viewport: { width: 1240, height: 820 },
    screenshot: "only-on-failure",
    trace: "retain-on-failure",
  },
  webServer: {
    command: "../bin/test-server --assets dist",
    url: "http://127.0.0.1:9481",
    timeout: 30000,
    reuseExistingServer: false,
  },
});

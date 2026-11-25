import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  timeout: 90_000,
  use: {
    baseURL: process.env.AUTOLIFE_E2E_ORIGIN ?? "http://127.0.0.1:8080",
    browserName: "chromium",
    launchOptions: { executablePath: process.env.AUTOLIFE_E2E_CHROMIUM },
  },
});

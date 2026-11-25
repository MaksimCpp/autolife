import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

const mailpit = process.env.AUTOLIFE_E2E_MAILPIT ?? "http://127.0.0.1:8025";
const password = "initial-local-fixture-password";
const resetPassword = "recovered-local-fixture-password";

async function mailLink(request: APIRequestContext, email: string, action: "verify" | "reset") {
  let link = "";
  await expect.poll(async () => {
    const response = await request.get(mailpit + "/api/v1/messages");
    if (!response.ok()) return false;
    const data: { messages: { ID: string; To: { Address: string }[]; Subject: string }[] } = await response.json();
    const message = data.messages.find((item) => item.To.some((to) => to.Address === email)
      && item.Subject === "AutoLife account " + action);
    if (!message) return false;
    const content = await request.get(mailpit + "/api/v1/message/" + message.ID);
    if (!content.ok()) return false;
    const details: { Text: string } = await content.json();
    link = details.Text.match(/https?:\/\/[^\s]+#token=[A-Za-z0-9_-]+/)?.[0] ?? "";
    return Boolean(link);
  }, { timeout: 15_000 }).toBe(true);
  return link;
}

async function register(page: Page, request: APIRequestContext, email: string) {
  await page.goto("/en/account/register");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Create account" }).click();
  await expect(page.getByRole("status")).toContainText("a verification link");
  await page.goto(await mailLink(request, email, "verify"));
  await page.getByRole("button", { name: "Подтвердить почту" }).click();
  await expect(page.getByRole("status")).toContainText("Почта подтверждена");
}

async function signIn(page: Page, email: string, secret: string) {
  await page.goto("/en/account/login");
  await page.getByLabel("Email").fill(email);
  await page.getByLabel("Password", { exact: true }).fill(secret);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page.getByRole("heading", { name: "Account settings" })).toBeVisible();
  await expect(page.getByText("Current session", { exact: true })).toBeVisible();
}

test("registration, recovery, settings and private exports through the same origin", async ({ page, browser, request, baseURL }) => {
  const email = `accounts-${Date.now()}@example.test`;
  await register(page, request, email);
  await signIn(page, email, password);
  await expect(page.getByLabel("Allow optional usage analytics")).not.toBeChecked();
  await page.getByRole("combobox", { name: "Language", exact: true }).selectOption("en");
  await page.getByLabel("Time zone").fill("Europe/Moscow");
  await page.getByRole("combobox", { name: "Theme" }).selectOption("dark");
  await page.getByLabel("Allow optional usage analytics").check();
  await page.getByRole("button", { name: "Save settings" }).click();
  await expect(page.getByRole("status")).toContainText("Settings saved.");
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  await page.reload();
  await expect(page.getByLabel("Time zone")).toHaveValue("Europe/Moscow");
  await expect(page.getByLabel("Allow optional usage analytics")).toBeChecked();

  const exportForm = page.locator("form").filter({ has: page.getByRole("button", { name: "Request export" }) });
  await exportForm.getByLabel("Current password").fill(password);
  const accepted = page.waitForResponse((response) => response.url().endsWith("/api/v1/me/export") && response.status() === 202);
  await exportForm.getByRole("button", { name: "Request export" }).click();
  const { id } = await (await accepted).json();
  await expect(page.getByRole("status")).toContainText("Export status: Ready");
  await page.reload();
  await expect(page.getByRole("status")).toContainText("Export status: Ready");
  const download = page.waitForEvent("download");
  await page.getByRole("button", { name: "Download export" }).click();
  expect((await download).suggestedFilename()).toBe("autolife-export.json");

  const other = await browser.newContext({ baseURL });
  try {
    const otherPage = await other.newPage();
    const otherEmail = "other-" + email;
    await register(otherPage, request, otherEmail);
    await signIn(otherPage, otherEmail, password);
    const denied = await other.request.get("/api/v1/me/exports/" + id);
    expect(denied.status()).toBe(404);
    const own = await page.request.get("/api/v1/me/sessions");
    const { items } = await own.json();
    const csrf = await other.request.get("/api/v1/auth/csrf");
    const rejected = await other.request.delete("/api/v1/me/sessions/" + items[0].id, {
      headers: { Origin: baseURL ?? "", "X-CSRF-Token": (await csrf.json()).csrf_token },
    });
    expect(rejected.status()).toBe(404);
  } finally {
    await other.close();
  }

  await page.getByRole("button", { name: "Sign out" }).click();
  await expect(page.getByRole("heading", { name: "Sign in" })).toBeVisible();
  expect((await page.request.get("/api/v1/me")).status()).toBe(401);
  await page.goto("/en/account/forgot");
  await page.getByLabel("Email").fill(email);
  await page.getByRole("button", { name: "Recover access" }).click();
  await expect(page.getByRole("status")).toContainText("you’ll receive a link");
  await page.goto(await mailLink(request, email, "reset"));
  await page.getByLabel("Новый пароль").fill(resetPassword);
  await page.getByRole("button", { name: "Сбросить пароль" }).click();
  await expect(page.getByRole("status")).toContainText("Пароль обновлён");
  await signIn(page, email, resetPassword);
  const deleteForm = page.locator("form").filter({ has: page.getByRole("button", { name: "Delete account" }) });
  await deleteForm.getByLabel("Current password").fill(resetPassword);
  await deleteForm.getByLabel("I confirm that I want to delete my account.").check();
  await deleteForm.getByRole("button", { name: "Delete account" }).click();
  await expect(page.getByRole("status")).toContainText("Deletion accepted");
  expect((await page.request.get("/api/v1/me")).status()).toBe(401);
});

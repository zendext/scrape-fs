import { test, expect, type Page } from "@playwright/test";
import { chooseLanguage } from "./helpers";

test.describe.configure({ mode: "serial" });

async function open(page: Page) {
  await page.goto("/");
  await chooseLanguage(page, "en");
  await expect(page.getByRole("button", { name: "New", exact: true })).toBeEnabled();
}

async function navigate(page: Page, address: string) {
  await page.getByRole("textbox", { name: "Current folder address" }).fill(address);
  await page.getByRole("button", { name: "Go", exact: true }).click();
  await expect(page.locator(".current-path")).toHaveText(address);
}

async function createLibrary(page: Page, name: string) {
  await page.getByRole("button", { name: "New", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox", { name: "New name" }).fill(name);
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText(`${name}:/`);
}

async function createFolder(page: Page, name: string) {
  await page.getByRole("button", { name: "New folder", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox").fill(name);
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
}

async function removeTree(page: Page) {
  const modal = page.getByRole("dialog");
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await expect(modal).not.toBeVisible();
}

test("managing another library keeps the current library and directory", async ({ page }) => {
  await open(page);
  await createLibrary(page, "state-other");
  await createFolder(page, "Shows");
  await navigate(page, "media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  await page.locator(".folder").filter({ hasText: /^state-other$/ }).click({ button: "right" });
  const modal = page.getByRole("dialog");
  await modal.getByRole("button", { name: "Rename", exact: true }).click();
  await modal.getByRole("textbox", { name: "New name" }).fill("state-renamed");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.getByRole("alert")).toHaveCount(0);

  const root = page.locator(".tree > ul > li").filter({
    has: page.locator(".folder").filter({ hasText: /^state-renamed$/ }),
  });
  await root.locator(":scope > .tree-row .toggle").click();
  await root.locator(".folder").filter({ hasText: /^Shows$/ }).click({ button: "right" });
  await modal.getByRole("button", { name: "Rename / move" }).click();
  await modal.getByRole("textbox").fill("Moved");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  await root.locator(".folder").filter({ hasText: /^Moved$/ }).click({ button: "right" });
  await modal.getByRole("button", { name: "Delete folder" }).click();
  await removeTree(page);
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  await page.locator(".folder").filter({ hasText: /^state-renamed$/ }).click({ button: "right" });
  await modal.getByRole("button", { name: "Delete library" }).click();
  await removeTree(page);
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("saving and restarting reloads namespace and clears obsolete selection and diagnostics", async ({ page, context }) => {
  await open(page);
  await createLibrary(page, "state-restart");
  await createFolder(page, "Gone");
  await navigate(page, "state-restart:/Gone");
  await page.getByRole("button", { name: "Add mapping", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("button", { name: "Choose file…" }).click();
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await page.locator(".filename").filter({ hasText: /^source.mkv$/ }).click();
  await page.getByRole("button", { name: "Details", exact: true }).click();
  await expect(page.locator(".detail")).toBeVisible();
  await page.getByRole("button", { name: "Service status", exact: true }).click();
  await page.getByRole("button", { name: "Run diagnostics", exact: true }).click();
  await expect(page.locator(".diagnostics .findings")).toBeVisible();
  await page.getByRole("button", { name: "Configuration", exact: true }).click();
  await page.getByRole("textbox", { name: "Attribute cache timeout", exact: true }).fill("7s");
  await page.getByRole("button", { name: "Preview changes", exact: true }).click();
  await expect(page.locator(".config-preview")).toBeVisible();

  const other = await context.newPage();
  await open(other);
  await navigate(other, "state-restart:/Gone");
  await other.getByRole("button", { name: "Folder actions", exact: true }).click();
  await other.getByRole("dialog").getByRole("button", { name: "Delete folder" }).click();
  await removeTree(other);
  await createLibrary(other, "state-after-restart");
  await other.close();

  await page.getByRole("button", { name: "Save and restart", exact: true }).click();
  await expect(page.locator("footer")).toContainText("Configuration saved");
  await expect(page.locator(".config-state")).toContainText("match");
  await page.getByRole("button", { name: "Libraries", exact: true }).click();
  await expect(page.locator(".folder").filter({ hasText: /^state-after-restart$/ })).toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("state-restart:/");
  await expect(page.locator("tbody tr")).toHaveCount(0);
  await expect(page.locator(".detail")).not.toBeVisible();
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.getByRole("button", { name: "Service status", exact: true }).click();
  await expect(page.locator(".diagnostics")).toContainText("Diagnostics have not been run");
  await expect(page.locator(".diagnostics .findings")).toHaveCount(0);
});

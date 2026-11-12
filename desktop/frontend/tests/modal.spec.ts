import { test, expect, type Page } from "@playwright/test";

async function english(page: Page) {
  await page.goto("/");
  await page.locator(".language").selectOption("en");
  await expect(page.getByRole("button", { name: "New", exact: true })).toBeEnabled();
}

test("management dialogs block the background, retain drafts across locale changes, and restore focus", async ({ page }) => {
  await english(page);
  const trigger = page.getByRole("button", { name: "New", exact: true });
  await trigger.click();
  const modal = page.getByRole("dialog");
  await expect(modal).toBeVisible();
  expect(await modal.evaluate((element) => element.matches(":modal"))).toBe(true);
  const name = modal.getByRole("textbox", { name: "New name" });
  await expect(name).toBeFocused();
  await name.fill("retained modal draft");
  // Inert background controls cannot steal keyboard focus or receive clicks.
  await page.locator(".top .tab").nth(1).evaluate((element: HTMLButtonElement) => element.focus());
  expect(await modal.evaluate((element) => element.contains(document.activeElement))).toBe(true);
  for (let count = 0; count < 12; count++) {
    await page.keyboard.press("Tab");
    await expect(page.locator(".top :focus, .library-page :focus, .fullpage :focus, footer :focus")).toHaveCount(0);
  }
  const background = await page.locator(".top .tab").nth(1).boundingBox();
  await page.mouse.click(background!.x + background!.width / 2, background!.y + background!.height / 2);
  await expect(page.locator(".library-page")).toBeVisible();
  await expect(modal).toBeVisible();
  await modal.locator(".dialog-language").selectOption("zh");
  await expect(modal.getByRole("heading", { name: "新建媒体库" })).toBeVisible();
  await expect(modal.getByRole("textbox", { name: "新的名称" })).toHaveValue("retained modal draft");
  await modal.locator(".dialog-language").selectOption("en");
  await page.keyboard.press("Escape");
  await expect(modal).not.toBeVisible();
  await expect(trigger).toBeFocused();
  await trigger.click();
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  await expect(trigger).toBeFocused();
});

test("a pending mutation cannot be dismissed with Escape", async ({ page }) => {
  await english(page);
  let release!: () => void;
  const pending = new Promise<void>((resolve) => { release = resolve; });
  await page.route("**/wails/runtime", async (route) => {
    const call = route.request().postDataJSON();
    if (call.args?.args?.[0]?.command === "create_library") await pending;
    await route.continue();
  });
  await page.getByRole("button", { name: "New", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox", { name: "New name" }).fill("modal-busy-test");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  try {
    await expect(modal.getByRole("button", { name: "Confirm", exact: true })).toBeDisabled();
    await page.keyboard.press("Escape");
    await expect(modal).toBeVisible();
    await expect(modal.getByRole("button", { name: "Close", exact: true })).toBeDisabled();
  } finally {
    release();
  }
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("modal-busy-test:/");
  await page.getByRole("button", { name: "Manage", exact: true }).click();
  await modal.getByRole("button", { name: "Delete library", exact: true }).click();
  await modal.getByRole("button", { name: "Preview deletion", exact: true }).click();
  await modal.getByRole("button", { name: "Confirm deletion", exact: true }).click();
  await expect(modal).not.toBeVisible();
});

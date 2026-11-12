import { test, expect, type Page } from "@playwright/test";

async function english(page: Page) {
  await page.goto("/");
  const language = page.locator(".top .language").getByRole("combobox");
  await language.click();
  await page.getByRole("option", { name: "English", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "New", exact: true }),
  ).toBeEnabled();
  return language;
}

test("custom language options distinguish selection, keyboard focus and hover", async ({
  page,
}) => {
  const language = await english(page);
  await expect(language).toBeFocused();
  await expect(page.locator("select")).toHaveCount(0);
  await language.click();
  const menu = page.getByRole("listbox");
  const selected = menu.getByRole("option", { name: "English", exact: true });
  const chinese = menu.getByRole("option", { name: "中文", exact: true });
  await expect(selected).toHaveAttribute("aria-selected", "true");
  await language.press("Home");
  await expect(chinese).toHaveClass(/is-active/);
  await expect(chinese).toHaveAttribute("aria-selected", "false");
  await expect(selected).toHaveAttribute("aria-selected", "true");
  await selected.hover();
  expect(
    await selected.evaluate(
      (element) => getComputedStyle(element).backgroundColor,
    ),
  ).toBe("rgb(42, 64, 89)");
  expect(
    await chinese.evaluate((element) => getComputedStyle(element).outlineColor),
  ).toBe("rgb(169, 208, 255)");
  expect(
    await menu.evaluate((element) => getComputedStyle(element).backgroundColor),
  ).toBe("rgb(32, 35, 38)");
  await page.screenshot({
    path: test.info().outputPath("selector-dark-options.png"),
  });
  await language.press("Escape");
  await expect(menu).not.toBeVisible();
  await expect(language).toContainText("English");
  await expect(language).toBeFocused();
  await language.press(" ");
  await language.press("ArrowUp");
  await language.press("Enter");
  await expect(language).toContainText("中文");
  await expect(language).toBeFocused();
  await language.press("e");
  await language.press(" ");
  await expect(language).toContainText("English");
  await expect(language).toBeFocused();
  await language.click();
  await selected.click();
  await expect(menu).not.toBeVisible();
  await expect(language).toContainText("English");
  await language.click();
  await page.locator(".top strong").click();
  await expect(menu).not.toBeVisible();
});

test("profile options stay in the modal and keyboard selection does not submit its form", async ({
  page,
}) => {
  await english(page);
  let createCalls = 0;
  let release!: () => void;
  const pending = new Promise<void>((resolve) => {
    release = resolve;
  });
  await page.route("**/wails/runtime", async (route) => {
    const call = route.request().postDataJSON();
    if (call.args?.args?.[0]?.command === "create_library") {
      createCalls++;
      await pending;
    }
    await route.continue();
  });
  await page.getByRole("button", { name: "New", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal
    .getByRole("textbox", { name: "New name" })
    .fill("selector-keyboard-test");
  const profile = modal.getByRole("combobox", {
    name: "Naming rules",
    exact: true,
  });
  await profile.press("ArrowDown");
  const options = modal.getByRole("listbox");
  await expect(options).toBeVisible();
  expect(
    await options.evaluate((element) =>
      element.closest("dialog")?.matches(":modal"),
    ),
  ).toBe(true);
  const menuBox = (await options.boundingBox())!;
  const modalBox = (await modal.boundingBox())!;
  expect(menuBox.y).toBeGreaterThanOrEqual(modalBox.y);
  expect(menuBox.y + menuBox.height).toBeLessThanOrEqual(
    modalBox.y + modalBox.height,
  );
  await profile.press("End");
  await profile.press("Escape");
  await expect(options).not.toBeVisible();
  await expect(modal).toBeVisible();
  await expect(profile).toContainText("jellyfin@1");
  await profile.press("p");
  await profile.press("Enter");
  await expect(profile).toContainText("plex@1");
  await expect(modal).toBeVisible();
  await expect(profile).toBeFocused();
  await profile.click();
  await options.getByRole("option", { name: "plex@1", exact: true }).click();
  await expect(options).not.toBeVisible();
  await expect(profile).toBeFocused();
  await profile.press("Home");
  await profile.press("Tab");
  await expect(profile).toContainText("jellyfin@1");
  await expect(
    modal.getByRole("button", { name: "Confirm", exact: true }),
  ).toBeFocused();
  expect(createCalls).toBe(0);
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  try {
    await expect(profile).toBeDisabled();
    await expect(options).not.toBeVisible();
    expect(createCalls).toBe(1);
  } finally {
    release();
  }
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText(
    "selector-keyboard-test:/",
  );
  await page.locator(".tree").getByRole("button", { name: "Delete library selector-keyboard-test", exact: true }).click();
  await modal
    .getByRole("button", { name: "Preview deletion", exact: true })
    .click();
  await modal
    .getByRole("button", { name: "Confirm deletion", exact: true })
    .click();
  await expect(modal).not.toBeVisible();
});

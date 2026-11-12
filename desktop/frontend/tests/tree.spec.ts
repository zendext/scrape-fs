import { test, expect, type Page } from "@playwright/test";
import { chooseLanguage } from "./helpers";

test.describe.configure({ mode: "serial" });

async function english(page: Page) {
  await page.goto("/");
  await chooseLanguage(page, "en");
  await expect(
    page.getByRole("button", { name: "New", exact: true }),
  ).toBeEnabled();
}

async function createLibrary(page: Page, name: string) {
  await page.getByRole("button", { name: "New", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox", { name: "New name" }).fill(name);
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText(`${name}:/`);
}

test("each library and folder has a named management button", async ({
  page,
}) => {
  await page.goto("/");
  await expect(page.locator(".tree-row .tree-manage").first()).toBeVisible();
  await chooseLanguage(page, "en");
  await expect(
    page
      .locator(".tree-heading")
      .getByRole("button", { name: "Manage", exact: true }),
  ).toHaveCount(0);
  const library = page.getByRole("button", {
    name: "Manage library media",
    exact: true,
  });
  const folder = page.getByRole("button", {
    name: "Folder actions Shows",
    exact: true,
  });
  await expect(library).toHaveAttribute("title", "Manage library media");
  await expect(folder).toHaveAttribute("title", "Folder actions Shows");
  await expect(library).toBeEnabled();
  await expect(folder).toBeEnabled();
  expect(await page.locator(".tree-manage").count()).toBe(
    await page.locator(".tree-row").count(),
  );
});

test("management buttons target their own nodes without navigating or expanding the tree", async ({
  page,
}) => {
  await english(page);
  await createLibrary(page, "tree-other");
  await page.getByRole("button", { name: "New folder", exact: true }).click();
  let modal = page.getByRole("dialog");
  await modal.getByRole("textbox").fill("Own folder");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  const trigger = page.getByRole("button", {
    name: "Manage library tree-other",
    exact: true,
  });
  const root = page.locator(".tree > ul > li").filter({ has: trigger });
  const toggle = root.locator(":scope > .tree-row .toggle");
  const expanded = await toggle.getAttribute("aria-expanded");
  await trigger.focus();
  await page.keyboard.press("Enter");
  modal = page.getByRole("dialog");
  await expect(
    modal.getByRole("heading", { name: "Manage library", exact: true }),
  ).toBeVisible();
  await expect(modal.locator(".dialog-path")).toHaveText("tree-other:/");
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(toggle).toHaveAttribute("aria-expanded", expanded!);
  await expect(modal.locator(".dialog-language")).toHaveCount(0);
  await expect(
    modal.getByRole("combobox", { name: "Interface language", exact: true }),
  ).toHaveCount(0);
  await page.keyboard.press("Escape");
  await expect(modal).not.toBeVisible();
  await expect(trigger).toBeFocused();

  const folder = root.getByRole("button", {
    name: "Folder actions Own folder",
    exact: true,
  });
  await folder.click();
  await expect(
    modal.getByRole("heading", { name: "Folder actions", exact: true }),
  ).toBeVisible();
  await expect(modal.locator(".dialog-path")).toHaveText(
    "tree-other:/Own folder",
  );
  await modal
    .getByRole("button", { name: "Rename / move", exact: true })
    .click();
  await modal.getByRole("textbox").fill("Renamed folder");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(
    root.getByRole("button", {
      name: "Folder actions Renamed folder",
      exact: true,
    }),
  ).toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  await root
    .getByRole("button", { name: "Folder actions Renamed folder", exact: true })
    .click();
  await modal.getByRole("button", { name: "Delete folder" }).click();
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await expect(modal).not.toBeVisible();
  await expect(
    root.getByRole("button", {
      name: "Folder actions Renamed folder",
      exact: true,
    }),
  ).toHaveCount(0);
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
});

test("touch users can open node management and pending actions disable all node buttons", async ({
  browser,
}) => {
  const context = await browser.newContext({
    hasTouch: true,
    viewport: { width: 1240, height: 820 },
  });
  const page = await context.newPage();
  try {
    await english(page);
    await createLibrary(page, "tree-touch");
    const trigger = page.getByRole("button", {
      name: "Manage library tree-touch",
      exact: true,
    });
    const size = await trigger.boundingBox();
    expect(size!.width).toBeGreaterThanOrEqual(44);
    expect(size!.height).toBeGreaterThanOrEqual(44);
    await trigger.tap();
    const modal = page.getByRole("dialog");
    await expect(modal).toBeVisible();
    await modal.getByRole("button", { name: "Rename", exact: true }).click();
    let release!: () => void;
    const pending = new Promise<void>((resolve) => {
      release = resolve;
    });
    await page.route("**/wails/runtime", async (route) => {
      if (
        route.request().postDataJSON().args?.args?.[0]?.command ===
        "rename_library"
      )
        await pending;
      await route.continue();
    });
    await modal
      .getByRole("textbox", { name: "New name" })
      .fill("tree-touch-renamed");
    await modal.getByRole("button", { name: "Confirm", exact: true }).click();
    try {
      await expect(
        modal.getByRole("button", { name: "Confirm", exact: true }),
      ).toBeDisabled();
      for (const button of await page.locator(".tree-manage").all())
        await expect(button).toBeDisabled();
    } finally {
      release();
    }
    await expect(modal).not.toBeVisible();
    await expect(
      page.getByRole("button", {
        name: "Manage library tree-touch-renamed",
        exact: true,
      }),
    ).toBeEnabled();
  } finally {
    await context.close();
  }
});

test("control users can browse but node management remains disabled", async ({
  page,
}) => {
  await page.route("**/wails/runtime", async (route) => {
    if (route.request().postDataJSON().args?.args?.[0]?.command !== "state") {
      await route.continue();
      return;
    }
    const response = await route.fetch();
    const body = await response.json();
    body.data.daemon.can_admin = false;
    await route.fulfill({ response, json: body });
  });
  await page.goto("/");
  await chooseLanguage(page, "en");
  await expect(
    page.getByRole("button", { name: "Manage library media", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Folder actions Shows", exact: true }),
  ).toBeDisabled();
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click({ button: "right" });
  await expect(page.getByRole("dialog")).not.toBeVisible();
});

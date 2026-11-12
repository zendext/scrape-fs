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

async function confirmRemoval(page: Page) {
  const modal = page.getByRole("dialog");
  await expect(
    modal.getByRole("button", { name: "Confirm deletion" }),
  ).toHaveCount(0);
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await expect(modal).not.toBeVisible();
}

test("library and folder icons open each action directly and deletion still requires a preview", async ({
  page,
}) => {
  await english(page);
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await expect(page.locator(".tree-manage")).toHaveCount(0);
  await expect(
    page
      .locator(".tree-heading")
      .getByRole("button", { name: "Manage", exact: true }),
  ).toHaveCount(0);
  const modal = page.getByRole("dialog");
  for (const [name, heading, path] of [
    ["Rename media", "Rename", "media:/"],
    ["Change rules media", "Change rules", "media:/"],
    ["Delete library media", "Delete library", "media:/"],
    ["Rename / move Shows", "Rename / move", "media:/Shows"],
    ["Delete folder Shows", "Delete folder", "media:/Shows"],
  ]) {
    const action = page.getByRole("button", { name, exact: true });
    await expect(action).toHaveAttribute("title", name);
    await action.click();
    await expect(
      modal.getByRole("heading", { name: heading, exact: true }),
    ).toBeVisible();
    await expect(modal.locator(".dialog-path")).toHaveText(path);
    expect(await modal.evaluate((element) => element.matches(":modal"))).toBe(
      true,
    );
    if (heading === "Rename")
      await expect(modal.getByRole("textbox")).toHaveValue("media");
    if (heading === "Rename / move")
      await expect(modal.getByRole("textbox")).toHaveValue("Shows");
    if (heading === "Change rules")
      await expect(
        modal.getByRole("combobox", { name: "Naming rules", exact: true }),
      ).toHaveText("jellyfin@1");
    if (heading.startsWith("Delete")) {
      await expect(
        modal.getByRole("button", { name: "Confirm deletion" }),
      ).toHaveCount(0);
      await modal.getByRole("button", { name: "Preview deletion" }).click();
      await expect(
        modal.getByRole("button", { name: "Confirm deletion" }),
      ).toBeVisible();
    }
    await page.keyboard.press("Escape");
    await expect(modal).not.toBeVisible();
    await expect(action).toBeFocused();
    await expect(page.locator(".current-path")).toHaveText("media:/Shows");
    await expect(page.locator("tbody tr")).toHaveCount(141);
  }
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click({ button: "right" });
  await expect(modal).not.toBeVisible();
});

test("direct actions target their own nodes without navigating or expanding the tree", async ({
  page,
}) => {
  await english(page);
  await createLibrary(page, "tree-other");
  await page.getByRole("button", { name: "New folder", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox").fill("Own folder");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  const trigger = page.getByRole("button", {
    name: "Rename tree-other",
    exact: true,
  });
  const root = page.locator(".tree > ul > li").filter({
    has: page.locator(".folder").filter({ hasText: /^tree-other$/ }),
  });
  const toggle = root.locator(":scope > .tree-row .toggle");
  const expanded = await toggle.getAttribute("aria-expanded");
  await trigger.focus();
  await page.keyboard.press("Enter");
  await expect(
    modal.getByRole("heading", { name: "Rename", exact: true }),
  ).toBeVisible();
  await expect(modal.locator(".dialog-path")).toHaveText("tree-other:/");
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(toggle).toHaveAttribute("aria-expanded", expanded!);
  await modal.getByRole("textbox", { name: "New name" }).fill("tree-renamed");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);

  await page
    .getByRole("button", { name: "Change rules tree-renamed", exact: true })
    .click();
  await modal
    .getByRole("combobox", { name: "Naming rules", exact: true })
    .click();
  await modal.getByRole("option", { name: "plex@1", exact: true }).click();
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await page
    .getByRole("button", { name: "Change rules tree-renamed", exact: true })
    .click();
  await expect(
    modal.getByRole("combobox", { name: "Naming rules", exact: true }),
  ).toHaveText("plex@1");
  await page.keyboard.press("Escape");

  const renamedRoot = page.locator(".tree > ul > li").filter({
    has: page.locator(".folder").filter({ hasText: /^tree-renamed$/ }),
  });
  await renamedRoot.locator(":scope > .tree-row .toggle").click();
  await renamedRoot
    .getByRole("button", { name: "Rename / move Own folder", exact: true })
    .click();
  await expect(modal.getByRole("textbox")).toHaveValue("Own folder");
  await modal.getByRole("textbox").fill("Renamed folder");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await renamedRoot
    .getByRole("button", { name: "Delete folder Renamed folder", exact: true })
    .click();
  await confirmRemoval(page);
  await expect(
    renamedRoot.getByRole("button", {
      name: "Delete folder Renamed folder",
      exact: true,
    }),
  ).toHaveCount(0);
  await page
    .getByRole("button", { name: "Delete library tree-renamed", exact: true })
    .click();
  await confirmRemoval(page);
  await expect(renamedRoot).toHaveCount(0);
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await expect(page.getByRole("alert")).toHaveCount(0);
});

test("touch users can invoke an action directly and pending mutations disable every tree action", async ({
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
      name: "Rename tree-touch",
      exact: true,
    });
    const size = await trigger.boundingBox();
    expect(size!.width).toBeGreaterThanOrEqual(44);
    expect(size!.height).toBeGreaterThanOrEqual(44);
    await trigger.tap();
    const modal = page.getByRole("dialog");
    await expect(
      modal.getByRole("heading", { name: "Rename", exact: true }),
    ).toBeVisible();
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
      for (const button of await page
        .locator(".tree-actions .item-action")
        .all())
        await expect(button).toBeDisabled();
    } finally {
      release();
    }
    await expect(modal).not.toBeVisible();
    await expect(
      page.getByRole("button", {
        name: "Rename tree-touch-renamed",
        exact: true,
      }),
    ).toBeEnabled();
  } finally {
    await context.close();
  }
});

test("control users can browse while every direct management action stays disabled", async ({
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
    page.getByRole("button", { name: "Rename media", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Change rules media", exact: true }),
  ).toBeDisabled();
  await expect(
    page.getByRole("button", { name: "Delete library media", exact: true }),
  ).toBeDisabled();
  await expect(
    page
      .locator(".tree")
      .getByRole("button", { name: "Rename / move Shows", exact: true }),
  ).toBeDisabled();
  await expect(
    page
      .locator(".tree")
      .getByRole("button", { name: "Delete folder Shows", exact: true }),
  ).toBeDisabled();
  await expect(
    page
      .locator("tbody")
      .getByRole("button", { name: "Rename / move Shows", exact: true }),
  ).toBeDisabled();
  await expect(
    page
      .locator("tbody")
      .getByRole("button", { name: "Delete folder Shows", exact: true }),
  ).toBeDisabled();
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  await expect(page.locator("tbody tr")).toHaveCount(141);
});

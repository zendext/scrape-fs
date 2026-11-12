import { test, expect, type Page } from "@playwright/test";
import { chooseLanguage } from "./helpers";

async function english(page: Page) {
  await page.goto("/");
  await chooseLanguage(page, "en");
  await page
    .locator(".folder")
    .filter({ hasText: /^media$/ })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/");
}

function row(page: Page, name: string) {
  return page
    .locator("tbody tr")
    .filter({
      has: page
        .locator(".filename")
        .filter({
          hasText: new RegExp(
            `^${name.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")}$`,
          ),
        }),
    });
}

test("selection displays details automatically and the whole row owns hover feedback", async ({
  page,
}) => {
  await english(page);
  await expect(
    page.getByRole("button", { name: "Details", exact: true }),
  ).toHaveCount(0);
  await expect(page.locator(".localbar button")).toHaveText([
    "New folder",
    "Add mapping",
  ]);
  await expect(
    page.getByRole("button", { name: "Open folder", exact: true }),
  ).toBeVisible();
  const shows = row(page, "Shows");
  await shows.locator("td").nth(1).click();
  await expect(page.locator(".detail")).toContainText("media:/Shows");
  await expect(shows).toHaveClass(/selected/);
  await page
    .locator(".detail")
    .getByRole("button", { name: "Close", exact: true })
    .click();
  await expect(page.locator(".detail")).not.toBeVisible();
  await expect(shows).not.toHaveClass(/selected/);
  await shows.locator("td").nth(1).hover();
  expect(
    await shows.evaluate(
      (element) => getComputedStyle(element).backgroundColor,
    ),
  ).not.toBe("rgba(0, 0, 0, 0)");
  await shows.locator(".filename").hover();
  expect(
    await shows
      .locator(".filename")
      .evaluate((element) => getComputedStyle(element).backgroundColor),
  ).toBe("rgba(0, 0, 0, 0)");
  await shows.locator(".filename").focus();
  await expect(page.locator(".detail")).toContainText("media:/Shows");
  await page.screenshot({
    path: test.info().outputPath("row-actions-and-details.png"),
  });
});

test("directory row actions target their own entry directly and stop parent double click navigation", async ({
  page,
}) => {
  await english(page);
  await page.getByRole("button", { name: "New folder", exact: true }).click();
  const modal = page.getByRole("dialog");
  await modal.getByRole("textbox").fill("Rows other");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  const shows = row(page, "Shows");
  const other = row(page, "Rows other");
  await shows.locator("td").nth(1).click();
  await expect(other.locator(".item-action")).toHaveCount(3);
  await other
    .getByRole("button", { name: "Rename / move Rows other", exact: true })
    .click();
  await expect(
    modal.getByRole("heading", { name: "Rename / move", exact: true }),
  ).toBeVisible();
  await expect(modal.locator(".dialog-path")).toHaveText("media:/Rows other");
  await expect(page.locator(".detail")).toContainText("media:/Shows");
  await page.keyboard.press("Escape");
  await other
    .getByRole("button", { name: "Delete folder Rows other", exact: true })
    .click();
  await expect(
    modal.getByRole("heading", { name: "Delete folder", exact: true }),
  ).toBeVisible();
  await expect(modal.locator(".dialog-path")).toHaveText("media:/Rows other");
  await modal
    .getByRole("button", { name: "Preview deletion", exact: true })
    .click();
  await modal
    .getByRole("button", { name: "Confirm deletion", exact: true })
    .click();
  await expect(modal).not.toBeVisible();
  const open = shows.getByRole("button", { name: "Open Shows", exact: true });
  const unexpectedNavigation = page
    .waitForRequest(
      (request) => {
        if (
          !request.url().endsWith("/wails/runtime") ||
          request.method() !== "POST"
        )
          return false;
        const call = request.postDataJSON().args?.args?.[0];
        return call?.command === "browse" && call.path === "Shows";
      },
      { timeout: 250 },
    )
    .then(
      () => true,
      () => false,
    );
  await open.dispatchEvent("dblclick");
  expect(await unexpectedNavigation).toBe(false);
  await expect(page.locator(".current-path")).toHaveText("media:/");
  await open.click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
});

test("file and mapping row actions preserve ownership restrictions and explicit targets", async ({
  page,
}) => {
  const opened: { library: string; path: string }[] = [];
  let restricted = false;
  await page.route("**/wails/runtime", async (route) => {
    const request = route.request().postDataJSON().args?.args?.[0];
    if (request?.command === "open_file") {
      opened.push(request);
      await route.fulfill({ json: { data: {} } });
    } else if (restricted && request?.command === "browse") {
      const response = await route.fetch();
      const result = await response.json();
      for (const entry of result.data.entries)
        if (entry.kind === "mapping") entry.can_rebind = false;
      await route.fulfill({ response, json: result });
    } else await route.continue();
  });
  await english(page);
  await row(page, "Shows")
    .getByRole("button", { name: "Open Shows", exact: true })
    .click();
  await expect(page.locator(".current-path")).toHaveText("media:/Shows");
  const file = row(page, "episode-000.nfo");
  const name = "Long media filename with 中文字符 and a source mapping.mkv";
  const mapping = row(page, name);
  await expect(file.locator(".item-action")).toHaveCount(1);
  await expect(mapping.locator(".item-action")).toHaveCount(4);
  await file.locator("td").nth(1).click();
  await expect(page.locator(".detail")).toContainText("system application");
  await expect(page.locator(".detail button")).toHaveCount(1);
  await file
    .getByRole("button", { name: "Open episode-000.nfo", exact: true })
    .click();
  await expect.poll(() => opened.length).toBe(1);
  expect(opened[0]).toMatchObject({
    library: "media",
    path: "Shows/episode-000.nfo",
  });
  const modal = page.getByRole("dialog");
  for (const [action, heading] of [
    ["Rename / move", "Rename / move"],
    ["Change source file", "Change source file"],
    ["Remove mapping", "Remove mapping"],
  ]) {
    await mapping
      .getByRole("button", { name: `${action} ${name}`, exact: true })
      .click();
    await expect(
      modal.getByRole("heading", { name: heading, exact: true }),
    ).toBeVisible();
    await expect(modal.locator(".dialog-path")).toHaveText(
      `media:/Shows/${name}`,
    );
    await expect(page.locator(".detail")).toContainText("episode-000.nfo");
    await page.keyboard.press("Escape");
  }
  await mapping
    .getByRole("button", { name: `Open ${name}`, exact: true })
    .dblclick();
  await expect
    .poll(() => opened.filter((entry) => entry.path === `Shows/${name}`).length)
    .toBe(2);
  restricted = true;
  await page
    .locator(".toolbar")
    .getByRole("button", { name: "Refresh", exact: true })
    .click();
  await expect(mapping.locator(".item-action")).toHaveCount(3);
  await expect(
    mapping.getByRole("button", {
      name: `Change source file ${name}`,
      exact: true,
    }),
  ).toHaveCount(0);
});

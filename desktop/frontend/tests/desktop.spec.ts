import { test, expect, type Page } from "@playwright/test";
import { chooseLanguage } from "./helpers";

test.describe.configure({ mode: "serial" });
async function english(page: Page) {
  await page.goto("/");
  await chooseLanguage(page, "en");
  await expect(
    page.getByRole("button", { name: "Libraries", exact: true }),
  ).toBeVisible();
  await expect(
    page.locator(".folder").filter({ hasText: "media" }),
  ).toBeVisible();
}
async function dialog(page: Page, title: string) {
  const modal = page.getByRole("dialog");
  await expect(
    modal.getByRole("heading", { name: title, exact: true }),
  ).toBeVisible();
  return modal;
}

test("real bindings browse all files, align controls, preserve scrolling and locale", async ({
  page,
}) => {
  const errors: string[] = [];
  page.on("pageerror", (error) => errors.push(error.message));
  await english(page);
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await expect(page.locator("tbody tr")).toHaveCount(141);
  await page.screenshot({ path: test.info().outputPath("compact.png") });
  const address = page.getByRole("textbox", { name: "Current folder address" });
  await expect(address).toHaveValue("media:/Shows");
  const aligned = await page.locator(".toolbar").evaluate((element) =>
    [...element.querySelectorAll("input,button")].map((item) => {
      const box = item.getBoundingClientRect();
      return [box.y, box.height];
    }),
  );
  expect(new Set(aligned.map((value) => JSON.stringify(value))).size).toBe(1);
  const languageRight = await page
    .locator(".language")
    .evaluate((element) => element.getBoundingClientRect().right);
  expect(1240 - languageRight).toBeLessThanOrEqual(12);
  await page.locator(".list-viewport").evaluate((element) => {
    element.scrollTop = 650;
  });
  await expect
    .poll(() =>
      page.locator(".list-viewport").evaluate((element) => element.scrollTop),
    )
    .toBe(650);
  await chooseLanguage(page, "zh");
  await expect(
    page.getByRole("button", { name: "打开文件夹", exact: true }),
  ).toBeVisible();
  expect(
    await page
      .locator(".list-viewport")
      .evaluate((element) => element.scrollTop),
  ).toBe(650);
  await expect(page.getByRole("textbox", { name: "当前目录地址" })).toHaveValue(
    "media:/Shows",
  );
  await chooseLanguage(page, "en");
  await page.reload();
  await expect(page.locator(".language").getByRole("combobox")).toHaveText("English");
  await page
    .locator(".folder")
    .filter({ hasText: /^Shows$/ })
    .click();
  await page.getByRole("button", { name: "Open folder", exact: true }).click();
  await expect
    .poll(
      async () =>
        (await (await page.request.get("/test/observations")).json()).opened,
    )
    .toMatch(/^true:.*\/mount\/media\/Shows$/);
  await expect(page.getByRole("alert")).toHaveCount(0);
  await page.keyboard.press("Control+l");
  await expect(address).toBeFocused();
  await address.fill("media:/../outside");
  await address.press("Enter");
  await expect(page.getByRole("alert")).toContainText("Invalid path");
  await address.press("Escape");
  await expect(address).toHaveValue("media:/Shows");
  await page.getByRole("button", { name: "Close", exact: true }).click();
  await page
    .locator(".filename")
    .filter({ hasText: "episode-000.nfo" })
    .click();
  await page.getByRole("button", { name: "Details", exact: true }).click();
  await expect(page.locator(".detail")).toContainText("system application");
  await expect(page.locator(".detail button")).toHaveCount(1);
  expect(errors).toEqual([]);
});

test("library and folder CRUD use confirmed removal and mapping creation uses a real source", async ({
  page,
}) => {
  await english(page);
  await page.getByRole("button", { name: "New", exact: true }).click();
  let modal = await dialog(page, "New library");
  await modal.getByRole("textbox", { name: "New name" }).fill("gui-test");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(
    page.getByRole("textbox", { name: "Current folder address" }),
  ).toHaveValue("gui-test:/");
  await page.getByRole("button", { name: "New folder", exact: true }).click();
  modal = await dialog(page, "New folder");
  await modal.getByRole("textbox").fill("Films");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await page.locator(".filename").filter({ hasText: "Films" }).dblclick();
  await page.getByRole("button", { name: "Add mapping", exact: true }).click();
  modal = await dialog(page, "Add mapping");
  await modal.getByRole("button", { name: "Choose file…" }).click();
  await expect(
    modal.getByRole("textbox", { name: "Source file", exact: true }),
  ).toHaveValue(/source.mkv$/);
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(page.locator("tbody tr")).toHaveCount(1);
  await page
    .getByRole("button", { name: "Folder actions", exact: true })
    .click();
  modal = await dialog(page, "Folder actions");
  await modal.getByRole("button", { name: "Rename / move" }).click();
  modal = await dialog(page, "Rename / move");
  await modal.getByRole("textbox").fill("Movies");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(
    page.getByRole("textbox", { name: "Current folder address" }),
  ).toHaveValue("gui-test:/Movies");
  await page
    .getByRole("button", { name: "Folder actions", exact: true })
    .click();
  modal = await dialog(page, "Folder actions");
  await modal.getByRole("button", { name: "Delete folder" }).click();
  modal = await dialog(page, "Delete folder");
  await expect(modal).toContainText("Source files are kept");
  await expect(
    modal.getByRole("button", { name: "Confirm deletion" }),
  ).toHaveCount(0);
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await expect(modal.locator("dd")).toHaveText(["0", "1", "0"]);
  await expect(modal.getByRole("button", { name: "Confirm deletion" })).toBeVisible();
  const concurrent = await page.context().newPage();
  await english(concurrent);
  await concurrent
    .getByRole("textbox", { name: "Current folder address" })
    .fill("gui-test:/Movies");
  await concurrent.getByRole("button", { name: "Go", exact: true }).click();
  await expect(concurrent.locator(".current-path")).toHaveText(
    "gui-test:/Movies",
  );
  await concurrent
    .getByRole("button", { name: "New folder", exact: true })
    .click();
  const otherDialog = concurrent.getByRole("dialog");
  await otherDialog.getByRole("textbox").fill("Concurrent");
  await otherDialog
    .getByRole("button", { name: "Confirm", exact: true })
    .click();
  await expect(otherDialog).not.toBeVisible();
  await concurrent.close();
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await expect(modal.getByRole("alert")).toContainText("The contents have changed");
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await expect(modal.locator("dd")).toHaveText(["1", "1", "0"]);
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await chooseLanguage(page, "en");
  await expect(
    page.getByRole("textbox", { name: "Current folder address" }),
  ).toHaveValue("gui-test:/");
  await expect(page.locator("tbody tr")).toHaveCount(0);
  expect(
    (await (await page.request.get("/test/observations")).json()).source,
  ).toBe("original media");
  await page.getByRole("button", { name: "Manage", exact: true }).click();
  modal = await dialog(page, "Manage library");
  await modal.getByRole("button", { name: "Delete library" }).click();
  modal = await dialog(page, "Delete library");
  await modal.getByRole("button", { name: "Preview deletion" }).click();
  await modal.getByRole("button", { name: "Confirm deletion" }).click();
  await expect(
    page.locator(".folder").filter({ hasText: /^gui-test$/ }),
  ).toHaveCount(0);
});

test("saved and running configurations stay distinct until a verified restart", async ({
  page,
}) => {
  await english(page);
  await page
    .getByRole("button", { name: "Configuration", exact: true })
    .click();
  await expect(page.locator(".config-state")).toContainText("match");
  await page
    .getByRole("textbox", { name: "Attribute cache timeout", exact: true })
    .fill("3s");
  await chooseLanguage(page, "zh");
  await expect(
    page.getByRole("textbox", { name: "属性缓存时间", exact: true }),
  ).toHaveValue("3s");
  await chooseLanguage(page, "en");
  await page
    .getByRole("button", { name: "Preview changes", exact: true })
    .click();
  await expect(page.locator(".config-preview")).toContainText("3s");
  await page.getByRole("button", { name: "Save only", exact: true }).click();
  await expect(page.locator(".config-state")).toContainText("Restart to apply");
  const saved = await (await page.request.get("/test/observations")).json();
  expect(saved.configuration).toContain("# retained fixture comment");
  expect(saved.configuration).toContain('attr_timeout = "3s"');
  expect(saved.backup).not.toContain('attr_timeout = "3s"');
  await page.getByRole("button", { name: "Raw TOML", exact: true }).click();
  await expect(
    page.getByRole("textbox", { name: "Raw TOML", exact: true }),
  ).toHaveValue(/# retained fixture comment/);
  await page
    .getByRole("button", { name: "Service status", exact: true })
    .click();
  await expect(page.locator(".summary dt").filter({ hasText: /^Database$/ }).locator("+ dd code"))
    .toHaveText(/\/state\/scrapefs\.db$/);
  await expect(page.locator(".summary dt").filter({ hasText: /^Library file storage$/ }).locator("+ dd code"))
    .toHaveText(/\/objects$/);
  await page
    .getByRole("button", { name: "Restart service", exact: true })
    .click();
  let modal = await dialog(page, "Restart service");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(modal).not.toBeVisible();
  await page
    .getByRole("button", { name: "Configuration", exact: true })
    .click();
  await expect(page.locator(".config-state")).toContainText("match");
  await page
    .getByRole("button", { name: "Service status", exact: true })
    .click();
  await page
    .getByRole("button", { name: "Run diagnostics", exact: true })
    .click();
  await expect(page.locator(".diagnostics")).toContainText(
    "Source mappings are healthy",
  );
  await page.getByRole("button", { name: "Stop service", exact: true }).click();
  modal = await dialog(page, "Stop service");
  await modal.getByRole("button", { name: "Confirm", exact: true }).click();
  await expect(
    page.getByRole("button", { name: "Start service", exact: true }),
  ).toBeEnabled();
  await page
    .getByRole("button", { name: "Start service", exact: true })
    .click();
  await expect(
    page.getByRole("button", { name: "Start service", exact: true }),
  ).toBeDisabled();
});

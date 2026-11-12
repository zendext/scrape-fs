import { expect, type Page } from "@playwright/test";

export async function chooseLanguage(page: Page, language: "zh" | "en") {
  const label = language === "en" ? "English" : "中文";
  const control = page.locator(".language").getByRole("combobox");
  await control.click();
  await page.getByRole("option", { name: label, exact: true }).click();
  await expect(control).toHaveText(label);
}

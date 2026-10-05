import { expect, test } from "@playwright/test";
import { loginAsAdmin } from "./helpers";

test.describe.configure({ mode: "serial" });

test.beforeEach(async ({ page }) => {
  await loginAsAdmin(page);
});

test("dashboard tiles, charts and top lists", async ({ page }) => {
  await expect(page.locator(".tile").first()).toContainText("Séries");
  await expect(page.getByRole("img", { name: "Heures regardées par jour sur 30 jours" })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Derniers visionnages" })).toBeVisible();
  await expect(page.locator(".disks li")).toHaveCount(2);

  await page.locator("#top").getByRole("link", { name: "Tout" }).click();
  await expect(page.locator("#top").getByRole("link", { name: "Breaking Bad" })).toBeVisible();
  await page.locator("#top").getByRole("link", { name: "Breaking Bad" }).click();
  await expect(page.locator("#modal")).toBeVisible();
});

test("stats filters and user page", async ({ page }) => {
  await page.getByRole("link", { name: "Stats", exact: true }).click();
  await page.getByLabel("Période").selectOption({ label: "Tout" });
  await expect(page).toHaveURL(/p=all/);
  await expect(page.getByRole("img", { name: /par jour de la semaine et par heure/ })).toBeVisible();
  await expect(page.getByRole("heading", { name: "Qui regarde quoi" })).toBeVisible();
  await page.getByRole("link", { name: "Alice" }).first().click();
  await expect(page.getByRole("heading", { name: "Alice", level: 1 })).toBeVisible();
  await expect(page.getByRole("img", { name: "Heures regardées par mois" })).toBeVisible();
});

test("cleanup lists candidates and never-watched seasons", async ({ page }) => {
  await page.getByRole("link", { name: "Nettoyage" }).click();
  const rows = page.locator(".cleanup-table tbody tr");
  await expect(rows.first()).toContainText("Tenet");
  await expect(page.locator(".cleanup-table")).not.toContainText("Severance");
  await expect(page.getByRole("heading", { name: "Saisons jamais vues" })).toBeVisible();
  await rows.first().getByRole("button", { name: "Supprimer" }).click();
  await expect(page.locator("#confirm")).toContainText("Mode simulation");
});

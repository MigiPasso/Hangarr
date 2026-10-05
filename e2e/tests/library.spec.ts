import { expect, test } from "@playwright/test";
import { loginAsAdmin } from "./helpers";

test.describe.configure({ mode: "serial" });

const cards = (page: import("@playwright/test").Page) => page.locator(".media-card h3");

test.beforeEach(async ({ page }) => {
  await loginAsAdmin(page);
});

test("series grid shows posters and watch states", async ({ page }) => {
  await page.getByRole("link", { name: "Séries" }).click();
  await expect(page).toHaveURL(/\/series$/);
  await expect(page.locator(".summary")).toContainText("6 séries");
  await expect(cards(page)).toHaveText(["Arcane", "Breaking Bad", "The Expanse", "Home Videos", "Severance", "Shōgun"]);

  const bb = page.locator(".media-card", { hasText: "Breaking Bad" });
  await expect(bb.locator(".badge")).toHaveText("Terminé");
  const poster = bb.locator("img");
  await expect(poster).toBeVisible();
  expect(await poster.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth > 0)).toBe(true);

  await expect(page.locator(".media-card", { hasText: "The Expanse" }).locator(".badge")).toHaveText("Abandonné 3/6");
  await expect(page.locator(".media-card", { hasText: "Severance" }).getByRole("img", { name: "Protégé" })).toBeVisible();
});

test("search, filters, history and perspective", async ({ page }) => {
  await page.goto("/series");
  await page.getByRole("searchbox", { name: "Rechercher" }).fill("shog");
  await expect(cards(page)).toHaveText(["Shōgun"]);
  await expect(page).toHaveURL(/q=shog/);

  await page.getByRole("searchbox", { name: "Rechercher" }).fill("");
  await expect(cards(page)).toHaveCount(6);
  await page.getByLabel("Jamais vu", { exact: true }).check();
  await expect(cards(page)).toHaveText(["Arcane", "Severance"]);
  await expect(page).toHaveURL(/state=never/);

  // Back restores the previous list.
  await page.goBack();
  await expect(cards(page)).toHaveCount(6);
  await page.goForward();
  await expect(cards(page)).toHaveText(["Arcane", "Severance"]);

  await page.goto("/series");
  await page.getByLabel("Point de vue").selectOption({ label: "Alice" });
  await expect(page.locator(".summary")).toContainText("point de vue de Alice");
  await expect(page.locator(".media-card", { hasText: "Shōgun" }).locator(".badge")).toHaveText("Jamais vu");

  await page.goto("/movies?sort=size&dir=desc");
  await expect(cards(page).first()).toHaveText("Dune");
});

test("table view is remembered", async ({ page }) => {
  await page.goto("/movies");
  await page.getByRole("link", { name: "Tableau" }).click();
  await expect(page.locator("table.library-table")).toBeVisible();
  await page.goto("/movies");
  await expect(page.locator("table.library-table tbody tr")).toHaveCount(6);
  await page.getByRole("link", { name: "Grille" }).click();
  await expect(cards(page)).toHaveCount(6);
});

test("manual sync", async ({ page }) => {
  await page.goto("/series");
  await page.getByRole("button", { name: "Synchroniser" }).click();
  await expect(page.locator("#sync-status")).toContainText("Synchro", { timeout: 10_000 });
  await expect(page.getByRole("button", { name: "Synchroniser" })).toBeVisible();
});

test("daily sync time can be changed", async ({ page }) => {
  await page.getByRole("link", { name: "Réglages" }).click();
  await page.getByLabel("Heure de la synchronisation quotidienne").fill("06:30");
  await page.locator("#settings-form").getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByText("Enregistré.", { exact: true })).toBeVisible();
  await expect(page.getByText(/Prochaine synchronisation : .* à 06:30/)).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Heure de la synchronisation quotidienne")).toHaveValue("06:30");
});

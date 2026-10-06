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

  // Known by Plex only: the poster comes from Plex.
  const home = page.locator(".media-card", { hasText: "Home Videos" }).locator("img");
  await expect(home).toHaveAttribute("src", "/img/plex/205/poster");
  await expect.poll(() => home.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBeGreaterThan(0);
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
  await page.getByLabel("Qui ?").selectOption({ label: "Alice" });
  await expect(page.locator(".summary")).toContainText("point de vue de Alice");
  await expect(page.locator(".media-card", { hasText: "Shōgun" }).locator(".badge")).toHaveText("Jamais vu");

  await page.goto("/movies?sort=size&dir=desc");
  await expect(cards(page).first()).toHaveText("Dune");
});

test("table view is remembered", async ({ page }) => {
  await page.goto("/movies");
  await page.getByRole("link", { name: "Tableau", exact: true }).click();
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
  const hour = page.getByLabel("Heure", { exact: true });
  const minute = page.getByLabel("Minute", { exact: true });
  await expect(hour).toHaveValue("04");
  await hour.selectOption("06");
  // Typing on a focused drop-down picks the value: "4" then "5" gives 45.
  await minute.focus();
  await page.keyboard.type("45");
  await expect(minute).toHaveValue("45");
  await page.locator("#settings-form").getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByText("Enregistré.", { exact: true })).toBeVisible();
  await expect(page.getByText(/Prochaine synchronisation : .* 06:45/)).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Heure", { exact: true })).toHaveValue("06");
  await expect(page.getByLabel("Minute", { exact: true })).toHaveValue("45");

  // Several syncs a day: from the chosen time, every N hours.
  await page.getByLabel("Fréquence").selectOption({ label: "Toutes les 6 heures" });
  await page.locator("#settings-form").getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByText("Chaque jour à 00:45, 06:45, 12:45, 18:45")).toBeVisible();
  await page.reload();
  await expect(page.getByLabel("Fréquence")).toHaveValue("6");
  await page.getByLabel("Fréquence").selectOption({ label: "Une fois par jour" });
  await page.locator("#settings-form").getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByText(/Chaque jour à/)).toHaveCount(0);
});

test("theme button switches light / dark and remembers it", async ({ page }) => {
  await page.emulateMedia({ colorScheme: "light" });
  await page.goto("/");
  const html = page.locator("html");
  await expect(html).not.toHaveAttribute("data-theme", /.+/);
  const bg = () => page.evaluate(() => getComputedStyle(document.body).backgroundColor);
  const light = await bg();
  await page.getByRole("button", { name: "Changer de thème (clair / sombre)" }).click();
  await expect(html).toHaveAttribute("data-theme", "dark");
  expect(await bg()).not.toBe(light);
  // Rendered by the server from the cookie: no flash on reload.
  await page.reload();
  await expect(html).toHaveAttribute("data-theme", "dark");
  await page.getByRole("button", { name: "Changer de thème (clair / sombre)" }).click();
  await expect(html).toHaveAttribute("data-theme", "light");
  expect(await bg()).toBe(light);
});

test("language: English and French, switched from the top bar", async ({ page }) => {
  // The e2e instances start in French (DEFAULT_LANG=fr); English is the default otherwise.
  await expect(page.getByRole("link", { name: "Tableau de bord" })).toBeVisible();
  await page.getByRole("link", { name: "EN", exact: true }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.getByRole("heading", { name: "Dashboard" })).toBeVisible();
  await expect(page.getByRole("link", { name: "Cleanup" })).toBeVisible();
  await page.goto("/settings");
  await expect(page.getByRole("heading", { name: "Settings" })).toBeVisible();
  await page.getByRole("link", { name: "FR", exact: true }).click();
  await expect(page).toHaveURL(/\/settings$/);
  await expect(page.getByRole("heading", { name: "Réglages" })).toBeVisible();
});

test("help icons show their text on focus", async ({ page }) => {
  await page.goto("/settings");
  const info = page.locator(".info").first();
  await info.focus();
  await expect(info).toHaveAttribute("data-tip", /Sonarr, Radarr, Plex et Tautulli/);
  await expect(page.getByRole("heading", { name: "Synchronisation", exact: true })).toBeVisible();
});

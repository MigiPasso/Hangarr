import { expect, test } from "@playwright/test";
import { login, username } from "./helpers";

// Jellyfin only (app-e2e-jellyfin, DRY_RUN=true): the first test checks that it is
// the media server in use, the others check the result.
test.describe.configure({ mode: "serial" });
test.use({ baseURL: process.env.BASE_URL_JELLYFIN ?? "http://app-e2e-jellyfin:8080" });

// Chosen with HANGARR_ADMIN_PASSWORD (compose.dev.yml): nothing to change.
const password = process.env.ADMIN_PASSWORD_JELLYFIN ?? "";
// The mock serves Jellyfin on its own port; /_calls lists the calls of every service.
const mockURL = "http://mock:8096";

type Call = { service: string; method: string; path: string; query: string; body: string };

test("Jellyfin is the only media server", { timeout: 120_000 }, async ({ page }) => {
  expect(password).not.toBe("");
  await login(page, username, password);
  // The first sync reads Jellyfin: the card is named after the server in use.
  await expect(async () => {
    await page.goto("/movies");
    await expect(page.locator(".media-card", { hasText: "Vacances 2019" })).toContainText("Jellyfin seul", { timeout: 1000 });
  }).toPass({ timeout: 60_000 });
  await expect(page.locator(".server-chip")).toHaveText("Jellyfin");

  await page.goto("/config");
  const plex = page.locator("#service-plex");
  await expect(plex).toContainText("Jellyfin est configuré : videz sa configuration pour utiliser Plex.");
  const inputs = plex.locator("input:not([type=hidden])");
  await expect(inputs.first()).toBeDisabled();
  for (const input of await inputs.all()) {
    await expect(input).toBeDisabled();
  }
  await expect(plex.getByRole("button", { name: "Enregistrer", exact: true })).toHaveCount(0);
  await expect(page.locator("#service-tautulli")).toContainText("Tautulli ne fonctionne qu'avec Plex : il n'est pas lu tant que Jellyfin est utilisé.");
});

test("library and filters name Jellyfin", async ({ page }) => {
  await login(page, username, password);
  await page.goto("/movies");
  await expect(page.getByLabel("Jellyfin et Sonarr/Radarr", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Jellyfin seul", { exact: true })).toBeVisible();

  await page.goto("/movies?match=server_only");
  await expect(page.locator(".media-card h3")).toHaveText(["Vacances 2019"]);
  const poster = page.locator(".media-card", { hasText: "Vacances 2019" }).locator("img");
  await expect(poster).toHaveAttribute("src", "/img/jellyfin/f0000000000000000000000000000105/poster");
  await expect.poll(() => poster.evaluate((img: HTMLImageElement) => img.complete && img.naturalWidth)).toBeGreaterThan(0);
  const image = await page.request.get("/img/jellyfin/f0000000000000000000000000000105/poster");
  expect(image.status()).toBe(200);
  expect(image.headers()["content-type"]).toMatch(/^image\//);
});

test("detail links to Jellyfin", async ({ page }) => {
  await login(page, username, password);
  await page.goto("/movies");
  await page.locator(".media-card", { hasText: "Inception" }).click();
  const modal = page.locator("#modal");
  await expect(modal).toBeVisible();
  await expect(modal.getByRole("link", { name: /Jellyfin/ })).toHaveAttribute(
    "href",
    /\/web\/#\/details\?id=f0000000000000000000000000000101&serverId=5e0c1e6a3b2f4d7c9a8b1c2d3e4f5a6b/,
  );
  await expect(modal.getByRole("link", { name: /Plex/ })).toHaveCount(0);
});

test("a movie known by Jellyfin only is deleted through Jellyfin (dry-run)", async ({ page, request }) => {
  await login(page, username, password);
  await page.goto("/movies");
  await page.locator(".media-card", { hasText: "Vacances 2019" }).click();
  await page.locator("#modal").getByRole("button", { name: "Supprimer le film" }).click();
  const confirm = page.locator("#confirm");
  await expect(confirm).toContainText("Jellyfin le supprime, avec ses fichiers sur le disque");
  await confirm.getByRole("button", { name: "Simuler la suppression" }).click();
  await expect(page.locator(".toast")).toContainText("Simulation : « Vacances 2019 » aurait été supprimé");

  const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
  expect(calls.filter((c) => c.service === "jellyfin" && c.method === "DELETE")).toEqual([]);

  await page.goto("/journal");
  await expect(page.getByRole("row", { name: /Vacances 2019/ })).toContainText("Supprimé via Jellyfin");
});

test("health names Jellyfin", async ({ page }) => {
  await login(page, username, password);
  await page.goto("/health");
  await expect(page.locator(".health-grid")).toContainText("Dans Jellyfin mais pas dans Sonarr/Radarr");
});

test("Configuration page fits a phone", async ({ page }) => {
  await login(page, username, password);
  await page.setViewportSize({ width: 390, height: 844 });
  await page.goto("/config");
  await page.waitForLoadState("networkidle");
  const [scroll, client] = await page.evaluate(() => [document.documentElement.scrollWidth, document.documentElement.clientWidth]);
  expect(scroll, `/config is ${scroll}px wide for a ${client}px screen`).toBeLessThanOrEqual(client);
});

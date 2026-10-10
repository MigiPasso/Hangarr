import { expect, test, type Page } from "@playwright/test";
import { login, loginAsAdmin, username } from "./helpers";

// Dune is on Plex in two versions, in one folder: the Remux (Radarr's file, of
// a monitored movie) and a WEB-DL. The DRY_RUN=true instance only simulates
// deletions; app-e2e-live really deletes them (the mock records the calls).
test.describe.configure({ mode: "serial" });

const mockURL = process.env.MOCK_URL ?? "http://mock:8989";
// Chosen with HANGARR_ADMIN_PASSWORD (compose.dev.yml): nothing to change.
const livePassword = process.env.ADMIN_PASSWORD_LIVE ?? "";
const remux = "Dune (2021) Remux-2160p.mkv";
const webdl = "Dune.2021.1080p.WEB-DL.DDP5.1.H.264-GRP.mkv";

type Call = { service: string; method: string; path: string; query: string; body: string };

const dune = (page: Page) => page.locator("section.dup-card", { hasText: "Dune" });

test.describe("dry-run instance (app-e2e)", () => {
  test.beforeEach(async ({ page }) => {
    await loginAsAdmin(page);
  });

  // Whatever the test did, Dune is to review again.
  test.afterEach(async ({ page }) => {
    await page.goto("/duplicates?tab=acked");
    const review = dune(page).getByRole("button", { name: "Revoir" });
    if ((await review.count()) > 0) {
      await review.click();
      await expect(page.getByText("Aucun doublon acquitté.")).toBeVisible();
    }
  });

  test("Duplicates lists the two versions of Dune with their tracks", async ({ page }) => {
    await page.getByRole("link", { name: "Doublons", exact: true }).click();
    await expect(page).toHaveURL(/\/duplicates$/);
    await expect(page.getByRole("heading", { name: "Doublons", exact: true })).toBeVisible();

    const card = dune(page);
    await expect(card.getByRole("heading", { level: 2 })).toContainText("Dune");
    const rows = card.locator("tbody tr");
    await expect(rows).toHaveCount(2);

    const remuxRow = rows.filter({ hasText: "Remux" });
    await expect(remuxRow).toContainText("4K");
    // The tracks are read live, once the table is shown.
    await expect(remuxRow).toContainText("Français (AC3 5.1)");
    await expect(remuxRow.locator(".badge", { hasText: "Radarr" })).toBeVisible();
    await expect(remuxRow.getByText("surveillé", { exact: true })).toBeVisible();

    const webRow = rows.filter({ hasText: "WEB-DL" });
    await expect(webRow).toContainText("1080p");
    await expect(webRow.locator(".badge")).toHaveCount(0);
  });

  test("the path of a version is copied, and its button says so", async ({ page }) => {
    await page.goto("/duplicates");
    // Wait for the live fragment: it replaces the table, and the button with it.
    await expect(dune(page).getByText("Français (AC3 5.1)")).toBeVisible();
    const copy = dune(page).getByRole("button", { name: `Copier le chemin de ${webdl}` });
    await copy.click();
    await expect(copy).toHaveText("Copié ✓");
    // The aria-label hides the button's text from screen readers: the live region says it.
    await expect(page.locator("#copy-status")).toHaveText("Copié ✓");
    // The copy fallback selects a hidden textarea: the focus must come back to the button.
    await expect(copy).toBeFocused();
    await expect(copy).toHaveText("Chemin");
  });

  test("keeping a duplicate hides it until its versions change", async ({ page }) => {
    await page.goto("/duplicates");
    await dune(page).getByRole("button", { name: "Tout garder" }).click();
    await expect(page.getByText("Aucun film n'est présent plus d'une fois sur Plex.")).toBeVisible();

    await page.getByRole("link", { name: "Acquittés (1)" }).click();
    await expect(dune(page)).toBeVisible();
    await dune(page).getByRole("button", { name: "Revoir" }).click();
    await expect(page.getByText("Aucun doublon acquitté.")).toBeVisible();

    await page.getByRole("link", { name: /^À revoir/ }).click();
    await expect(dune(page)).toBeVisible();
  });

  test("deleting a version is only simulated, and journaled", async ({ page, request }) => {
    await request.post(`${mockURL}/_reset`);
    await page.goto("/duplicates");
    await expect(dune(page).getByText("Français (AC3 5.1)")).toBeVisible();
    await dune(page).getByRole("button", { name: `Supprimer ${webdl}` }).click();

    const confirm = page.locator("#confirm");
    await expect(confirm).toBeVisible();
    await expect(confirm).toContainText("Simulation (DRY_RUN) : journalisé, rien n'est supprimé.");
    await confirm.getByRole("button", { name: "Simuler la suppression" }).click();
    await expect(confirm).toContainText("Simulation effectuée : aucune version supprimée.");

    const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
    expect(calls).toEqual([]);
    await page.goto("/journal");
    await expect(page.getByRole("row", { name: /Dune \(Dune\.2021/ })).toContainText("Simulation");
  });

  test("Radarr's file of a monitored movie cannot be deleted", async ({ page }) => {
    await page.goto("/duplicates");
    await expect(dune(page).getByText("Français (AC3 5.1)")).toBeVisible();
    await dune(page).getByRole("button", { name: `Supprimer ${remux}` }).click();

    const confirm = page.locator("#confirm");
    await expect(confirm).toBeVisible();
    await confirm.getByRole("button", { name: "Simuler la suppression" }).click();
    await expect(confirm.getByRole("alert")).toContainText("C'est le fichier de Radarr et Radarr surveille le film");
    await confirm.getByRole("link", { name: "Annuler" }).click();
    await expect(confirm).toBeHidden();
  });
});

test.describe("live instance (app-e2e-live)", () => {
  test.use({ baseURL: process.env.BASE_URL_LIVE ?? "http://app-e2e-live:8080" });

  test.beforeEach(async ({ page }) => {
    await login(page, username, livePassword);
  });

  test("deleting a version deletes its file through Plex", async ({ page, request }) => {
    await request.post(`${mockURL}/_reset`);
    await page.goto("/duplicates");
    await expect(dune(page).getByText("Français (AC3 5.1)")).toBeVisible();
    await dune(page).getByRole("button", { name: `Supprimer ${webdl}` }).click();

    const confirm = page.locator("#confirm");
    await expect(confirm).toBeVisible();
    await expect(confirm).not.toContainText("Simulation (DRY_RUN)");
    await confirm.getByRole("button", { name: "Supprimer définitivement" }).click();
    await expect(confirm).toContainText("Version supprimée.");

    // Plex then rescans its movies (PLEX_REFRESH_AFTER_DELETE): only the deletions are compared.
    const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
    expect(calls.filter((c) => c.method === "DELETE").map((c) => `${c.service} ${c.method} ${c.path}`)).toEqual([
      "plex DELETE /library/metadata/102/media/1022",
    ]);

    // The version is forgotten: Dune has one version left, so it is no duplicate.
    await page.goto("/duplicates");
    await expect(page.getByText("Aucun film n'est présent plus d'une fois sur Plex.")).toBeVisible();
    await page.goto("/journal");
    await expect(page.getByRole("row", { name: /Dune \(Dune\.2021/ })).toContainText("Effectuée");
  });
});

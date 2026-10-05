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
  // Nobody watched for 180 days (default), largest first, with the reason.
  await expect(page.getByText("Ce que personne n'a regardé depuis 180 jours")).toBeVisible();
  await expect(rows.first()).toContainText("Interstellar");
  await expect(rows.first()).toContainText("Dernière lecture il y a");
  await expect(page.locator(".cleanup-table tr", { hasText: "Tenet" })).toContainText("Jamais vu, ajouté il y a");
  await expect(page.locator(".cleanup-table")).not.toContainText("Severance");
  await expect(page.getByRole("heading", { name: "Saisons jamais vues" })).toBeVisible();
  await rows.first().getByRole("button", { name: "Supprimer" }).click();
  await expect(page.locator("#confirm")).toContainText("Mode simulation");
});

test("health page: live services, sync runs and data gaps", async ({ page }) => {
  await page.getByRole("link", { name: "Santé" }).click();
  const services = page.locator("#health-services");
  await expect(services.locator(".badge-ok")).toHaveCount(4);
  await expect(services).toContainText("Version 4.0.0.0-mock");
  await expect(services).toContainText("Indexers unavailable due to failures");
  await expect(page.getByRole("cell", { name: "Tautulli (historique)" })).toBeVisible();
  const plexOnly = page.locator(".health-list", { hasText: "Dans Plex mais pas dans Sonarr/Radarr" });
  await expect(plexOnly.getByRole("link", { name: "Home Videos" })).toBeVisible();
  await plexOnly.getByRole("link", { name: "Home Videos" }).click();
  await expect(page.locator("#modal").getByRole("heading", { name: /Home Videos/ })).toBeVisible();
});

test("batch selection on the cleanup page (dry-run)", async ({ page }) => {
  await page.getByRole("link", { name: "Nettoyage" }).click();
  const bar = page.locator("#batch-form");
  await expect(bar).toBeHidden();
  await page.getByRole("checkbox", { name: "Sélectionner Interstellar" }).check();
  await page.getByRole("checkbox", { name: "Sélectionner Tenet" }).check();
  await expect(bar).toBeVisible();
  await expect(bar).toContainText("2 sélectionnés · 35,0 Go");
  await bar.getByRole("button", { name: "Supprimer la sélection" }).click();
  const confirm = page.locator("#confirm");
  await expect(confirm).toContainText("2 éléments seront retirés");
  await confirm.getByRole("button", { name: "Simuler la suppression" }).click();
  await expect(confirm).toContainText("Suppression de la sélection (simulation)");
  await expect(confirm).toContainText("2 réussites");
  await expect(page.locator(".toast")).toContainText("Simulation");
  await confirm.getByRole("link", { name: "Fermer" }).click();
  // Nothing was deleted: rows stay, the selection is cleared.
  await expect(page.locator(".cleanup-table")).toContainText("Interstellar");
  await expect(bar).toBeHidden();
});

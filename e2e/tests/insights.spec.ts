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
  await expect(page.locator(".tile-hint", { hasText: "non regardés depuis 180 jours" })).toBeVisible();
  await expect(rows.first()).toContainText("Interstellar");
  await expect(rows.first()).toContainText("Dernière lecture il y a");
  await expect(page.locator(".cleanup-table tr", { hasText: "Tenet" })).toContainText("Jamais vu, ajouté il y a");
  await expect(page.locator(".cleanup-table")).not.toContainText("Severance");
  await expect(page.getByRole("heading", { name: "Saisons jamais vues" })).toBeVisible();
  await rows.first().getByRole("button", { name: "Supprimer" }).click();
  await expect(page.locator("#confirm")).toContainText("Simulation (DRY_RUN)");
});

test("cleanup filters: state, plays and size in the sidebar", async ({ page }) => {
  await page.getByRole("link", { name: "Nettoyage" }).click();
  const table = page.locator(".cleanup-table");
  const filters = page.getByRole("complementary", { name: "Filtres" });
  await filters.getByRole("checkbox", { name: "Jamais vu" }).check();
  await expect(page).toHaveURL(/state=never/);
  await expect(table).toContainText("Tenet");
  await expect(table).not.toContainText("Interstellar");
  await expect(page.getByRole("heading", { name: "Saisons jamais vues" })).toBeHidden();
  // Sorting keeps the filters.
  await table.getByRole("link", { name: "Titre" }).click();
  await expect(page).toHaveURL(/sort=title/);
  await expect(page).toHaveURL(/state=never/);
  await filters.getByRole("link", { name: "Réinitialiser" }).click();
  await expect(table).toContainText("Interstellar");
  await filters.getByRole("group", { name: "Lectures" }).getByLabel("Min.").fill("1");
  await expect(table).not.toContainText("Tenet");
  await expect(table).toContainText("Interstellar");
  await filters.getByRole("group", { name: "Taille (Go)" }).getByLabel("Min.").fill("99999");
  await expect(page.getByText("Aucun candidat ne correspond à ces filtres.")).toBeVisible();
});

test("health page: live services, sync runs and data gaps", async ({ page }) => {
  // System opens its first page, Health.
  await page.getByRole("link", { name: "Système" }).click();
  await expect(page.getByRole("link", { name: "Santé" })).toHaveAttribute("aria-current", "page");
  const services = page.locator("#health-services");
  await expect(services.locator(".badge-ok")).toHaveCount(4);
  await expect(services).toContainText("Version 4.0.0.0-mock");
  await expect(services).not.toContainText("Indexers unavailable due to failures");
  const trackers = page.locator("#health-trackers");
  await expect(trackers.locator("tr", { hasText: "NZBgeek" }).first()).toContainText("En échec");
  await expect(trackers.locator("tr", { hasText: "YTS" })).toContainText("Désactivé");
  await expect(trackers).toContainText("Indexers unavailable due to failures");
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
  await expect(confirm).toContainText("Supprimer la sélection (simulation)");
  await expect(confirm).toContainText("2 réussites");
  await expect(page.locator(".toast")).toContainText("Simulation");
  await confirm.getByRole("link", { name: "Fermer" }).click();
  // Nothing was deleted: rows stay, the selection is cleared.
  await expect(page.locator(".cleanup-table")).toContainText("Interstellar");
  await expect(bar).toBeHidden();
});

test("upcoming lists monitored releases by date, by page", async ({ page }) => {
  await page.getByRole("link", { name: "À venir" }).click();
  await expect(page.getByRole("heading", { name: "À venir", level: 1 })).toBeVisible();
  await expect(page.locator(".cal-day").first()).toContainText("Aujourd'hui");
  await expect(page.locator(".cal-day li", { hasText: "Hello, Ms. Cobel" })).toContainText("S02E01");
  await expect(page.locator(".cal-day li")).toHaveCount(25);
  // Page size, then the next page; the period keeps the page size.
  await expect(page.getByLabel("Par page")).toHaveValue("25");
  await page.getByLabel("Par page").selectOption("10");
  await expect(page).toHaveURL(/size=10/);
  await expect(page.locator(".cal-day li")).toHaveCount(10);
  await page.getByRole("link", { name: "Suivant" }).click();
  await expect(page).toHaveURL(/size=10&page=2/);
  await expect(page.locator(".calendar")).not.toContainText("Hello, Ms. Cobel");
  await page.getByRole("link", { name: "1 an" }).click();
  await expect(page).toHaveURL(/days=365&size=10$/);
  await page.goto("/calendar?size=100");
  await expect(page.locator(".cal-day li", { hasText: "Shōgun" })).toContainText("3 épisodes");
  await expect(page.locator(".cal-day li", { hasText: "Avatar" })).toContainText("Au cinéma");
  await page.locator(".cal-day").getByRole("link", { name: "Severance" }).first().click();
  await expect(page.locator("#modal")).toBeVisible();
});

test("upcoming month view", async ({ page }) => {
  await page.goto("/calendar");
  await page.getByRole("link", { name: "Calendrier", exact: true }).click();
  await expect(page).toHaveURL(/view=month/);
  await expect(page.locator(".cal-month td[aria-current=date]")).toHaveCount(1);
  await expect(page.locator(".cal-month thead th")).toHaveCount(7);
  // Shōgun's episodes air in 20 days: this month or the next one.
  const chip = page.locator(".cal-chip", { hasText: "S02E01–E03" });
  if ((await chip.count()) === 0) {
    await page.getByRole("link", { name: "Mois suivant" }).click();
  }
  await expect(chip).toContainText("Shōgun");
  await page.getByRole("link", { name: "Liste" }).click();
  await expect(page.locator(".cal-day").first()).toBeVisible();
});

test("upcoming grid view keeps the page size", async ({ page }) => {
  await page.goto("/calendar?size=50");
  await page.getByRole("group", { name: "Affichage" }).getByRole("link", { name: "Grille" }).click();
  await expect(page).toHaveURL(/view=grid.*size=50/);
  await expect(page.locator(".cal-card")).toHaveCount(50);
  // One flow: a date tile before each day's cards.
  await expect(page.locator(".cal-flow > .cal-date").first()).toBeVisible();
  await expect(page.getByRole("heading", { name: "Aujourd'hui" })).toBeVisible();
  await expect(page.locator(".cal-card", { hasText: "Shōgun" }).locator(".badge")).toHaveText("S02E01–E03");
  await page.getByLabel("Par page").selectOption("10");
  await expect(page).toHaveURL(/view=grid/);
  await expect(page.locator(".cal-card")).toHaveCount(10);
});

test("sidebar collapses to icons and slides in on small screens", async ({ page }) => {
  const sidebar = page.locator("#sidebar");
  await page.getByRole("button", { name: "Réduire le menu" }).click();
  await expect(page.locator(".shell")).toHaveClass(/is-collapsed/);
  await page.reload();
  await expect(page.locator(".shell")).toHaveClass(/is-collapsed/);
  // Labels stay accessible while collapsed.
  await page.getByRole("link", { name: "À venir" }).click();
  await expect(page).toHaveURL(/\/calendar/);
  await page.getByRole("button", { name: "Réduire le menu" }).click();
  await expect(page.locator(".shell")).not.toHaveClass(/is-collapsed/);

  await page.setViewportSize({ width: 390, height: 800 });
  await expect(sidebar).not.toBeInViewport();
  await page.getByRole("button", { name: "Ouvrir le menu" }).click();
  await expect(sidebar).toBeInViewport();
  await page.keyboard.press("Escape");
  await expect(sidebar).not.toBeInViewport();
});

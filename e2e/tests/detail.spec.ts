import { expect, test } from "@playwright/test";
import { loginAsAdmin } from "./helpers";

// DRY_RUN=true instance: deletions are simulated and journaled.
test.describe.configure({ mode: "serial" });

const mockURL = process.env.MOCK_URL ?? "http://mock:8989";

test.beforeEach(async ({ page }) => {
  await loginAsAdmin(page);
});

test("detail opens in a modal with seasons, viewers and history", async ({ page }) => {
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "Breaking Bad" }).click();
  const modal = page.locator("#modal");
  await expect(modal).toBeVisible();
  await expect(modal.getByRole("heading", { name: /Breaking Bad/ })).toBeVisible();
  await expect(modal.getByRole("cell", { name: "Spéciaux" })).toBeVisible();
  await expect(modal.getByRole("heading", { name: "Progression par spectateur" })).toBeVisible();
  await expect(modal.getByRole("row", { name: /Alice/ }).first()).toContainText("S02E03");
  await expect(modal.getByRole("link", { name: "Sonarr ↗" })).toHaveAttribute("href", /\/series\/breaking-bad$/);

  await modal.getByRole("button", { name: "Fermer" }).click();
  await expect(modal).toBeHidden();
  await expect(page).toHaveURL(/\/series$/);
});

test("protected series cannot be deleted", async ({ page }) => {
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "Severance" }).click();
  const modal = page.locator("#modal");
  await expect(modal.getByRole("button", { name: "Supprimer la série" })).toBeDisabled();
  await expect(modal.getByRole("button", { name: "Supprimer la saison" })).toBeDisabled();
});

test("season deletion in dry-run touches nothing", async ({ page, request }) => {
  await request.post(`${mockURL}/_reset`);
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "The Expanse" }).click();
  const modal = page.locator("#modal");
  await modal.getByRole("row", { name: /Saison 2/ }).getByRole("button", { name: "Supprimer la saison" }).click();

  const confirm = page.locator("#confirm");
  await expect(confirm).toBeVisible();
  await expect(confirm).toContainText("Mode simulation");
  await expect(confirm).toContainText("4,5 Go");
  await confirm.getByRole("button", { name: "Simuler la suppression" }).click();

  await expect(page.locator(".toast")).toContainText("Simulation : La saison 2 de « The Expanse »");
  await expect(confirm).toBeHidden();
  const calls = await (await request.get(`${mockURL}/_calls`)).json();
  expect(calls).toEqual([]);

  await page.goto("/journal");
  await expect(page.getByRole("row", { name: /The Expanse · saison 2/ })).toContainText("Simulation");
});

test("deleting a whole series requires typing its title", async ({ page }) => {
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "Arcane" }).click();
  await page.locator("#modal").getByRole("button", { name: "Supprimer la série" }).click();
  const confirm = page.locator("#confirm");
  await confirm.getByLabel(/Pour confirmer/).fill("Arcan");
  await confirm.getByRole("button", { name: "Simuler la suppression" }).click();
  await expect(confirm.getByRole("alert")).toHaveText("Le titre saisi ne correspond pas.");
  await confirm.getByRole("link", { name: "Annuler" }).click();
  await expect(confirm).toBeHidden();
});

test("security journal lists logins", async ({ page }) => {
  await page.goto("/journal?tab=security");
  await expect(page.getByRole("cell", { name: "Connexion", exact: true }).first()).toBeVisible();
  await expect(page.getByRole("cell", { name: "Mot de passe changé" }).first()).toBeVisible();
});

import { expect, test, type APIRequestContext } from "@playwright/test";
import { login, newPassword, username } from "./helpers";

// DRY_RUN=false instance (app-e2e-live): deletions really reach the mock.
test.describe.configure({ mode: "serial" });
test.use({ baseURL: process.env.BASE_URL_LIVE ?? "http://app-e2e-live:8080" });

const mockURL = process.env.MOCK_URL ?? "http://mock:8989";
const initialPassword = process.env.ADMIN_INITIAL_PASSWORD_LIVE ?? "";

type Call = { service: string; method: string; path: string; query: string; body: string };

async function arrCalls(request: APIRequestContext): Promise<Call[]> {
  const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
  return calls.filter((c) => c.service === "sonarr" || c.service === "radarr");
}

test("first login on the live instance", async ({ page }) => {
  expect(initialPassword).toHaveLength(24);
  await login(page, username, initialPassword);
  await page.getByLabel("Mot de passe actuel").fill(initialPassword);
  await page.getByLabel("Nouveau mot de passe").fill(newPassword);
  await page.getByLabel("Confirmation").fill(newPassword);
  await page.getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();
  await expect(page.getByText("Mode simulation (DRY_RUN)")).toHaveCount(0);
});

test("season deletion: files first, then the season is unmonitored", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "The Expanse" }).click();
  await page.locator("#modal").getByRole("row", { name: /Saison 2/ }).getByRole("button", { name: "Supprimer la saison" }).click();
  const confirm = page.locator("#confirm");
  await expect(confirm.getByRole("checkbox", { name: "Supprimer les fichiers du disque" })).toBeChecked();
  await confirm.getByRole("button", { name: "Supprimer définitivement" }).click();
  await expect(page.locator(".toast")).toContainText("La saison 2 de « The Expanse » a été supprimée (4,5 Go libérés)");

  const calls = await arrCalls(request);
  expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual([
    "DELETE /api/v3/episodefile/bulk",
    "PUT /api/v3/series/2",
  ]);
  expect(JSON.parse(calls[0].body)).toEqual({ episodeFileIds: [20201, 20202, 20203] });
  const series = JSON.parse(calls[1].body);
  const monitored = Object.fromEntries(series.seasons.map((s: any) => [s.seasonNumber, s.monitored]));
  expect(monitored).toEqual({ 1: true, 2: false });
  expect(series.network).toBe("Prime Video"); // object sent back intact

  // The detail is refreshed in place and the modal stays open.
  await expect(page.locator("#modal")).toBeVisible();
});

test("movie deletion removes the card", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/movies");
  await page.locator(".media-card", { hasText: "Tenet" }).click();
  await page.locator("#modal").getByRole("button", { name: "Supprimer le film" }).click();
  const confirm = page.locator("#confirm");
  await confirm.getByRole("checkbox", { name: "Ajouter une exclusion de liste d'import" }).check();
  await confirm.getByRole("button", { name: "Supprimer définitivement" }).click();
  await expect(page.locator(".toast")).toContainText("« Tenet » a été supprimé");
  await expect(page.locator(".media-card", { hasText: "Tenet" })).toHaveCount(0);
  await expect(page.locator("#modal")).toBeHidden();

  const calls = await arrCalls(request);
  expect(calls).toHaveLength(1);
  expect(calls[0].method).toBe("DELETE");
  expect(calls[0].path).toBe("/api/v3/movie/3");
  expect(calls[0].query).toBe("addImportExclusion=true&deleteFiles=true");

  await page.goto("/journal");
  await expect(page.getByRole("row", { name: /Tenet/ })).toContainText("Effectuée");
});

test("a movie known by Plex only is deleted through Plex", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/movies");
  await page.locator(".media-card", { hasText: "Vacances 2019" }).click();
  await page.locator("#modal").getByRole("button", { name: "Supprimer le film" }).click();
  const confirm = page.locator("#confirm");
  await expect(confirm).toContainText("Plex le supprime, avec ses fichiers sur le disque");
  await expect(confirm.getByRole("checkbox")).toHaveCount(0);
  await confirm.getByRole("button", { name: "Supprimer définitivement" }).click();
  await expect(page.locator(".toast")).toContainText("« Vacances 2019 » a été supprimé");
  await expect(page.locator(".media-card", { hasText: "Vacances 2019" })).toHaveCount(0);

  const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
  expect(calls.map((c) => `${c.service} ${c.method} ${c.path}`)).toEqual(["plex DELETE /library/metadata/105"]);
  await page.goto("/journal");
  await expect(page.getByRole("row", { name: /Vacances 2019/ })).toContainText("Supprimé via Plex");
});

test("a series known by Plex only is protected in Cleanarr only", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "Home Videos" }).click();
  const modal = page.locator("#modal");
  await modal.getByRole("button", { name: "Protéger", exact: true }).click();
  await expect(page.locator(".toast")).toContainText("« Home Videos » est protégé dans Cleanarr");
  await expect(modal.getByRole("button", { name: "Ne plus protéger" })).toBeVisible();
  await expect(modal.getByRole("button", { name: "Supprimer la série" })).toBeDisabled();

  // Kept across page loads, with the lock on the card.
  await page.goto("/series");
  await expect(page.locator(".media-card", { hasText: "Home Videos" }).locator(".lock")).toBeVisible();
  await page.locator(".media-card", { hasText: "Home Videos" }).click();
  await modal.getByRole("button", { name: "Ne plus protéger" }).click();
  await expect(modal.getByRole("button", { name: "Supprimer la série" })).toBeEnabled();
  // Nothing was sent to Plex, Sonarr or Radarr.
  const calls: Call[] = await (await request.get(`${mockURL}/_calls`)).json();
  expect(calls).toEqual([]);
});

test("protect then unprotect a series from its detail", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/series");
  await page.locator(".media-card", { hasText: "Breaking Bad" }).click();
  const modal = page.locator("#modal");
  await modal.getByRole("button", { name: "Protéger", exact: true }).click();
  await expect(page.locator(".toast")).toContainText("« Breaking Bad » est protégé (tag keep)");
  // The detail is reloaded: protected state, deletion disabled.
  await expect(modal.getByRole("button", { name: "Ne plus protéger" })).toBeVisible();
  await expect(modal.getByRole("button", { name: "Supprimer la série" })).toBeDisabled();

  let calls = await arrCalls(request);
  expect(calls.map((c) => `${c.method} ${c.path}`)).toEqual(["PUT /api/v3/series/1"]);
  const sent = JSON.parse(calls[0].body);
  expect(sent.tags).toEqual([1]);
  expect(sent.title).toBe("Breaking Bad"); // whole object sent back

  // The mock keeps its fixtures: Sonarr still reports no tag, so removing
  // the protection finds nothing there and the local state is realigned.
  await modal.getByRole("button", { name: "Ne plus protéger" }).click();
  await expect(page.locator(".toast").last()).toContainText("n'est pas protégé");
  await expect(modal.getByRole("button", { name: "Protéger", exact: true })).toBeVisible();

  await page.goto("/journal");
  await expect(page.getByRole("row", { name: /Breaking Bad/ }).first()).toContainText("Protégé");
});

test("batch deletion from the movie grid", async ({ page, request }) => {
  await login(page, username, newPassword);
  await request.post(`${mockURL}/_reset`);
  await page.goto("/movies");
  await page.getByRole("checkbox", { name: "Sélectionner Interstellar" }).check();
  await page.getByRole("checkbox", { name: "Sélectionner Dune" }).check();
  await page.locator("#batch-form").getByRole("button", { name: "Supprimer la sélection" }).click();
  const confirm = page.locator("#confirm");
  await confirm.getByRole("button", { name: "Supprimer définitivement" }).click();
  await expect(confirm).toContainText("2 réussites");
  await expect(page.locator(".media-card", { hasText: "Interstellar" })).toHaveCount(0);
  await expect(page.locator(".media-card", { hasText: "Dune" })).toHaveCount(0);
  const calls = await arrCalls(request);
  expect(calls.map((c) => `${c.method} ${c.path}`).sort()).toEqual(["DELETE /api/v3/movie/2", "DELETE /api/v3/movie/4"]);
});

import { expect, test } from "@playwright/test";
import { initialPassword, login, newPassword, username } from "./helpers";

// Scenarios run in order against a blank database: the initial password is
// extracted from the app-e2e logs by `make e2e` and changed along the way.
test.describe.configure({ mode: "serial" });

test.beforeAll(() => {
  expect(initialPassword, "ADMIN_INITIAL_PASSWORD must come from the app logs").toHaveLength(24);
});

test("anonymous visitors are sent to the login page", async ({ page }) => {
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);
  await expect(page.getByRole("button", { name: "Se connecter" })).toBeVisible();
});

test("wrong credentials show a generic error", async ({ page }) => {
  await login(page, username, "pas-le-bon-mot-de-passe");
  await expect(page.getByRole("alert")).toHaveText("Identifiants invalides.");
  await login(page, "inconnu", "pas-le-bon-mot-de-passe");
  await expect(page.getByRole("alert")).toHaveText("Identifiants invalides.");
});

test("first login forces a password change and rejects weak passwords", async ({ page }) => {
  await login(page, username, initialPassword);
  await expect(page).toHaveURL(/\/account\/password$/);
  await expect(page.getByText("Choisissez un nouveau mot de passe")).toBeVisible();

  // Every other page redirects back while the change is pending.
  await page.goto("/account");
  await expect(page).toHaveURL(/\/account\/password$/);

  // Live checklist (client side, informative only).
  await page.getByLabel("Nouveau mot de passe").fill("abc");
  await expect(page.locator('[data-rule="lower"]')).toHaveClass(/ok/);
  await expect(page.locator('[data-rule="length"]')).not.toHaveClass(/ok/);

  // Weak password: rejected by the server, errors swapped in by HTMX.
  await page.getByLabel("Mot de passe actuel").fill(initialPassword);
  await page.getByLabel("Nouveau mot de passe").fill("password");
  await page.getByLabel("Confirmation").fill("password");
  await page.getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.locator("#new-errors")).toContainText("au moins 12 caractères");
  await expect(page.locator("#new-errors")).toContainText("trop courant");
  await expect(page).toHaveURL(/\/account\/password$/);

  // Password containing the username.
  await page.getByLabel("Mot de passe actuel").fill(initialPassword);
  await page.getByLabel("Nouveau mot de passe").fill(`Mon-${username}-2026!`);
  await page.getByLabel("Confirmation").fill(`Mon-${username}-2026!`);
  await page.getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.locator("#new-errors")).toContainText("ne doit pas contenir le nom d'utilisateur");

  // Strong password: accepted, redirected to the dashboard.
  await page.getByLabel("Mot de passe actuel").fill(initialPassword);
  await page.getByLabel("Nouveau mot de passe").fill(newPassword);
  await page.getByLabel("Confirmation").fill(newPassword);
  await page.getByRole("button", { name: "Enregistrer" }).click();
  await expect(page).toHaveURL(/\/$/);
  await expect(page.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();
  await expect(page.getByText("Mode simulation (DRY_RUN)")).toBeVisible();
});

test("logout, old password refused, new password accepted", async ({ page }) => {
  await login(page, username, newPassword);
  await expect(page.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();

  await page.getByRole("button", { name: "Déconnexion" }).click();
  await expect(page).toHaveURL(/\/login$/);
  await page.goto("/");
  await expect(page).toHaveURL(/\/login$/);

  await login(page, username, initialPassword);
  await expect(page.getByRole("alert")).toHaveText("Identifiants invalides.");

  await login(page, username, newPassword);
  await expect(page).toHaveURL(/\/$/);
});

test("a session can be revoked from another browser", async ({ browser }) => {
  const ctxA = await browser.newContext();
  const ctxB = await browser.newContext();
  const a = await ctxA.newPage();
  const b = await ctxB.newPage();

  await login(a, username, newPassword);
  await expect(a.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();
  await login(b, username, newPassword);
  await expect(b.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();

  await a.goto("/account");
  const sessions = a.locator("#sessions tbody tr");
  const before = await sessions.count();
  expect(before).toBeGreaterThanOrEqual(2);
  await expect(a.getByText("Session actuelle")).toBeVisible();

  // Revoke every other session (B is among them).
  while ((await a.getByRole("button", { name: "Révoquer" }).count()) > 0) {
    const n = await sessions.count();
    await a.getByRole("button", { name: "Révoquer" }).first().click();
    await expect(sessions).toHaveCount(n - 1);
  }
  await expect(sessions).toHaveCount(1);

  await b.reload();
  await expect(b).toHaveURL(/\/login$/);

  // A is still logged in.
  await a.goto("/");
  await expect(a.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();

  await ctxA.close();
  await ctxB.close();
});

test("health endpoints are public", async ({ request }) => {
  expect((await request.get("/healthz")).status()).toBe(200);
  expect((await request.get("/readyz")).status()).toBe(200);
});

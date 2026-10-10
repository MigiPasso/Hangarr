import { expect, test, type Page } from "@playwright/test";
import { login, newPassword, username } from "./helpers";

// Instance without any service variable (app-e2e-config): everything is
// configured from the UI; the master key comes from a Docker secret file.
test.describe.configure({ mode: "serial" });
test.use({ baseURL: process.env.BASE_URL_CONFIG ?? "http://app-e2e-config:8080" });

const initialPassword = process.env.ADMIN_INITIAL_PASSWORD_CONFIG ?? "";
const mockKey = "mock-api-key";

async function configure(page: Page, service: string, fields: Record<string, string>) {
  const section = page.locator(`#service-${service}`);
  for (const [label, value] of Object.entries(fields)) {
    await section.getByLabel(label, { exact: true }).fill(value);
  }
  await section.getByRole("button", { name: "Tester la connexion" }).click();
  await expect(section.locator(".test-result")).toContainText("Connexion réussie");
  await section.getByRole("button", { name: "Enregistrer" }).click();
  await expect(section.getByText("Enregistré.")).toBeVisible();
  await expect(section.locator(".badge")).toHaveText("Configuré");
}

test("first start without configuration", async ({ page }) => {
  expect(initialPassword).toHaveLength(24);
  await login(page, username, initialPassword);
  await page.getByLabel("Mot de passe actuel").fill(initialPassword);
  await page.getByLabel("Nouveau mot de passe").fill(newPassword);
  await page.getByLabel("Confirmation").fill(newPassword);
  await page.getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();
  await expect(page.locator(".config-banner")).toContainText("Services non configurés (Sonarr, Radarr, Plex / Jellyfin)");
  await expect(page.getByRole("button", { name: "Synchroniser" })).toHaveCount(0);
});

test("services are configured from the UI, credentials never come back", async ({ page }) => {
  await login(page, username, newPassword);
  await page.locator(".config-banner").getByRole("link", { name: "Configurer les services" }).click();
  await expect(page.getByRole("heading", { name: "Configuration", exact: true })).toBeVisible();
  await expect(page.getByText("Aucune clé maître")).toHaveCount(0);

  // A wrong key is reported by the connection test, nothing is saved.
  const sonarr = page.locator("#service-sonarr");
  await sonarr.getByLabel("URL de l'API", { exact: true }).fill("http://mock:8989");
  await sonarr.getByLabel("Clé d'API", { exact: true }).fill("wrong-key");
  await sonarr.getByRole("button", { name: "Tester la connexion" }).click();
  await expect(sonarr.locator(".test-result")).toContainText("Accès refusé (HTTP 401)");
  // Radarr's address in the Sonarr section is detected.
  await sonarr.getByLabel("URL de l'API", { exact: true }).fill("http://mock:7878");
  await sonarr.getByLabel("Clé d'API", { exact: true }).fill(mockKey);
  await sonarr.getByRole("button", { name: "Tester la connexion" }).click();
  await expect(sonarr.locator(".test-result")).toContainText("répond comme Radarr, pas comme Sonarr");
  await expect(sonarr.locator(".badge")).toHaveText("Non configuré");

  await configure(page, "sonarr", { "URL de l'API": "http://mock:8989", "Clé d'API": mockKey });
  await expect(page.locator(".config-banner")).toContainText("Services non configurés (Radarr, Plex / Jellyfin)");
  await expect(sonarr.getByLabel("Clé d'API", { exact: true })).toHaveAttribute("placeholder", "••••-key (vide = conserver)");
  await expect(sonarr.getByLabel("Clé d'API", { exact: true })).toHaveValue("");

  await configure(page, "radarr", { "URL de l'API": "http://mock:7878", "Clé d'API": mockKey });
  await configure(page, "tautulli", { "URL de l'API": "http://mock:8181", "Clé d'API": mockKey });
  await configure(page, "plex", { "URL du serveur": "http://mock:32400", "Token (X-Plex-Token)": mockKey });
  // The configuration is now complete: the first sync starts right away.
  await expect(page.locator("#service-plex").getByText("Première synchronisation lancée.")).toBeVisible();
  await expect(page.locator(".config-banner")).toHaveCount(0);

  await page.reload();
  expect(await page.content()).not.toContain(mockKey);
  // An empty credential field keeps the saved value.
  await page.locator("#service-sonarr").getByRole("button", { name: "Enregistrer" }).click();
  await expect(page.locator("#service-sonarr").getByText("Aucun changement.")).toBeVisible();
});

test("the synchronized library and the security journal", async ({ page }) => {
  await login(page, username, newPassword);
  await expect(async () => {
    await page.goto("/series");
    await expect(page.locator(".media-card", { hasText: "Breaking Bad" })).toBeVisible({ timeout: 1000 });
  }).toPass({ timeout: 20_000 });
  await page.goto("/journal?tab=security");
  for (const svc of ["Sonarr", "Radarr", "Plex", "Tautulli"]) {
    await expect(page.getByRole("cell", { name: `Configuration ${svc} modifiée` })).toBeVisible();
  }
});

test("without HANGARR_MASTER_KEY a key is generated in /data and the UI warns about it", async ({ page }) => {
  // app-e2e has no master key; auth.spec.ts already set its password.
  const base = process.env.BASE_URL ?? "http://app-e2e:8080";
  await page.goto(`${base}/login`);
  await page.getByLabel("Nom d'utilisateur").fill(username);
  await page.getByLabel("Mot de passe", { exact: true }).fill(newPassword);
  await page.getByRole("button", { name: "Se connecter" }).click();
  await page.goto(`${base}/config`);
  await expect(page.getByText("Clé de chiffrement stockée avec les données.")).toBeVisible();
  // Services come from the environment there: locked fields.
  await expect(page.locator("#service-sonarr").getByLabel("URL de l'API", { exact: true })).toBeDisabled();
});

test("Tautulli webhook: secret generated once, then the webhook accepts it", async ({ page, request }) => {
  await login(page, username, newPassword);
  await page.goto("/config");
  const tautulli = page.locator("#service-tautulli");
  const base = process.env.BASE_URL_CONFIG ?? "http://app-e2e-config:8080";
  expect((await request.post(`${base}/webhooks/tautulli`)).status()).toBe(404);

  await tautulli.getByRole("button", { name: "Générer un secret" }).click();
  const shown = tautulli.locator("code.secret").first();
  await expect(shown).toBeVisible();
  const secret = (await shown.textContent())!.trim();
  expect(secret).toHaveLength(43);

  const hook = `${base}/webhooks/tautulli`;
  expect((await request.post(hook, { data: { event: "stop" } })).status()).toBe(401);
  expect((await request.post(hook, { headers: { "X-Hangarr-Token": secret }, data: { event: "stop" } })).status()).toBe(202);

  await page.reload();
  expect(await page.content()).not.toContain(secret);
  await expect(tautulli.getByRole("button", { name: "Générer un nouveau secret" })).toBeVisible();
});

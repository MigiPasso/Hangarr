import { expect, type Page } from "@playwright/test";

export const username = process.env.ADMIN_USERNAME ?? "admin";
export const initialPassword = process.env.ADMIN_INITIAL_PASSWORD ?? "";
// Set by auth.spec.ts (first file to run); later files log in with it.
export const newPassword = "Tr3s-Solide-Mot-De-Passe!";

export async function login(page: Page, user: string, password: string) {
  await page.goto("/login");
  await page.getByLabel("Nom d'utilisateur").fill(user);
  await page.getByLabel("Mot de passe", { exact: true }).fill(password);
  await page.getByRole("button", { name: "Se connecter" }).click();
}

export async function loginAsAdmin(page: Page) {
  await login(page, username, newPassword);
  await expect(page.getByRole("heading", { name: "Tableau de bord" })).toBeVisible();
}

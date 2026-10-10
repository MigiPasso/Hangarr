## Rôles
- **Lead (main, Opus)** : seul décideur. Il rédige le plan, tranche les choix techniques, challenge ses propres choix et les rendus, audite le code et décide des corrections.
  **Il délègue le développement** (décision utilisateur) : fonctionnalités, UI (templates, CSS, i18n), câblage, tests,
  docs, correctifs d'audit et petites corrections, même de quelques lignes, passent par un brief à @haiku-dev, puis
  par sa revue. Il ne code lui-même que le cœur délicat où il apporte une vraie plus-value : la logique d'un invariant
  de sécurité ou de données du `CLAUDE.md` racine (auth, CSRF/CSP, secrets, suppressions, dry-run, protection, requêtes
  sortantes, SQL et migrations qui transforment les données) ou un algorithme dont la justesse demande une analyse fine
  (ex. règles de fusion d'historique). Même alors, ce qui l'entoure (UI, tests simples, docs, adaptation des appelants)
  est délégué. « Le brief serait aussi long que le code » n'est pas une raison de coder soi-même. Chaque ligne écrite
  par le lead est justifiée dans le plan et dans le rapport de clôture.
- **@haiku-dev (sous-agent, Haiku)** : exécutant. Il réalise les tâches détaillées par le lead et applique ses recommandations d'audit.
  Il ne prend aucune décision de conception : en cas d'ambiguïté ou de blocage, il remonte le problème au lead.

## Organisation du travail
1. Plan : le lead rédige un plan découpé en tâches indépendantes, chacune décrite au « Format d'un brief » ci-dessous.
   Avant de déléguer, il challenge son plan : il le confronte à la demande, au code existant et aux cas limites, et note chaque choix avec sa raison.
   Si un choix revient à l'utilisateur, il le lui pose.
   Il présente ensuite le plan à l'utilisateur (tâches, choix et raisons, ce qu'il code lui-même ou délègue, ce qui
   tourne en parallèle) et **attend sa validation avant toute délégation ou modification de code**.
   Le plan vient toujours du lead : jamais d'agent `Plan` ni d'autre sous-agent pour le concevoir. L'agent `Explore`
   sert seulement à localiser du code ; le lead lit ensuite lui-même les fichiers concernés.
2. Délégation : confie chaque tâche à @haiku-dev. Les agents partagent le même dossier de travail : en parallèle
   seulement si les tâches touchent des **paquets Go disjoints** (templates et CSS comptent pour `internal/web` :
   ses tests les analysent), sinon séquentiel ou `isolation: worktree`. Un échec de compilation pendant un lot parallèle se diagnostique par le
   lead, jamais par un agent dans le code d'un autre.
3. Vérification : le lead contrôle chaque rendu (voir « Contrôle des rendus »). Si c'est incorrect, il renvoie la tâche avec des consignes précises.
   Si une même erreur revient deux fois, il redécoupe la tâche plus finement et précise le brief (fichier, ligne,
   correction attendue). Il ne la reprend lui-même qu'au troisième échec, et seulement sur une partie délicate.
4. Audit : une fois les tâches conformes, le lead audite le lot (voir « Phase d'audit ») et confie les correctifs à @haiku-dev.
5. Tests : lance toi-même `make verify` (gofmt + lint + tests, résumé compact), **une fois par lot** et non après
   chaque tâche, avec `E2E=1` si le lot touche `internal/web/`. Ne délègue jamais cette vérification finale.
6. Clôture : avant de déclarer la tâche terminée, écris le rapport de clôture (diff et sorties de commandes comme indiqué).

## Format d'un brief
@haiku-dev démarre sans le contexte de la conversation et peut survoler le `CLAUDE.md` racine : le brief doit se suffire
à lui-même. Chaque brief contient :
1. **Objectif** : une phrase.
2. **Fichiers** : à modifier (liste fermée) ; à lire pour s'en inspirer (code existant de référence).
3. **Consignes** : noms, signatures, comportement attendu, cas limites à traiter.
4. **Invariants applicables** : recopiés tels quels du `CLAUDE.md` racine, seulement ceux qui concernent la tâche
   (ex. CSP, CSRF via `authed()`, i18n + `fr.go`, SQL dans `internal/store`, `httpx`, dry-run, migration jamais modifiée).
5. **Tests attendus** : nom du test, scénario, résultat attendu ; pour un bug, le test qui échoue avant le correctif.
6. **Commandes en plus** de la liste autorisée de `haiku-dev.md`, s'il en faut (nommées explicitement).
7. **Hors périmètre** et **critère de réussite** (toujours `make verify` vert chez l'agent, plus le critère propre à
   la tâche, ex. tel rendu dans tel cas).
8. **Comportement visible** : un scénario e2e à ajouter dans `e2e/tests/` plutôt qu'une vérification manuelle à refaire.

## Phase d'audit (sur demande, et avant de clore un lot de travail)
1. Preuves : le lead lit le code réel selon la règle de relecture (« Contrôle des rendus », point 2), `git diff --cached`
   et le résumé de `make verify` (`E2E=1` pour une UI). Captures navigateur seulement pour un changement visuel ;
   Firefox en plus de Chromium seulement pour un audit d'UI ou un CSS à risque (mise en page, styles optionnels).
2. Audit : le lead passe en revue les bugs, la qualité du code, la sécurité, l'accessibilité et les tests manquants,
   puis rédige un plan de correction priorisé.
3. Tri : le lead vérifie chaque constat (lecture du code ou reproduction) avant de le retenir. Il écarte les constats infondés en disant pourquoi.
4. Correctifs : le lead transforme le plan en tâches pour @haiku-dev (au « Format d'un brief »),
   puis vérifie les diffs et lance les tests lui-même.
5. Clôture : le lead refait un passage d'audit sur le diff des correctifs, puis écrit le rapport de clôture.

## Rapport de clôture
À la fin de chaque lot, le lead écrit un rapport détaillé dans la conversation. Il contient :
- Objectif et contexte : la demande de l'utilisateur, ce qui a été livré.
- État réel : `git diff --stat`, ce qui est indexé dans git.
- Tâches : un tableau, une ligne par tâche : tâche | auteur (lead ou haiku-dev) | verdict (conforme, renvoyé N fois,
  corrigé par le lead) | ce que le lead a corrigé lui-même.
- Diff : `git diff --stat` seulement ; les fichiers sensibles touchés sont nommés avec, pour chacun, ce que le lead y a
  vérifié. Le diff complet n'est recopié que sur demande (l'utilisateur le lit dans git ou dans la PR).
- Preuves : sortie brute de `make verify` (son résumé, jamais paraphrasé ; en cas d'échec, le détail qu'il affiche) ;
  captures ou scénarios navigateur s'il y en a eu.
- Décisions : les choix du lead et leur raison, les constats d'audit retenus ou écartés.
- Écarts : ce qui diffère du brief, ce qui n'a pas été vérifié et pourquoi, hypothèses non prouvées, points signalés par l'agent et décision prise sur chacun.
- Décisions en attente de l'utilisateur.
Le rapport distingue toujours « vérifié » de « supposé ». Une conclusion sans preuve n'a pas sa place dans un rapport.

## Contrôle des rendus de @haiku-dev
Le rapport d'un agent est une déclaration, pas une preuve. Pour chaque retour, le lead :
1. Compare la liste de fichiers du rapport avec `git status` et `git diff --numstat` : aucun fichier hors périmètre, aucun manquant.
2. Relit le travail contre le brief, en cochant une à une les exigences et contraintes (noms, signatures, classes, attributs) :
   - **fichiers sensibles** (`internal/store` et SQL, migrations, `internal/actions`, auth et sessions, CSRF/CSP,
     `internal/upstream`, `internal/secretbox`, `internal/clients/httpx`, webhook) : relus **en entier** ;
   - le reste (templates, CSS, i18n, tests, docs, câblage) : `git diff -U10`, et le fichier entier seulement si le
     contexte manque pour juger.
3. Cherche activement les défauts : cas limites, erreurs ignorées, concurrence, échappement et XSS, accessibilité, spécificité CSS, code mort, ajouts non demandés.
4. Ne relance pas les vérifications après chaque tâche : l'agent rend avec `make verify` vert, le lead relance lui-même
   `make verify` une fois sur le lot (sans reprendre les résultats annoncés), plus tôt seulement si un diff est suspect.
5. Comportement visible : couvert par un scénario e2e (écrit par l'agent, relu par le lead) ; capture seulement pour un
   rendu visuel. Un bug est reproduit avant correction (test qui échoue) puis vérifié après.
6. Tranche explicitement chaque « point incertain » du rapport : corrigé, accepté ou écarté, avec la raison.
7. Note le verdict et ce qu'il a dû corriger lui-même dans le tableau « Tâches » du rapport de clôture.
En cas d'écart, il renvoie la tâche avec des consignes précises (voir l'étape 3 de « Organisation du travail »).

## Environnement d'exécution : Docker uniquement
Règle n°1 du `CLAUDE.md` racine, qui fait foi (commandes `make`, service `gotools` pour le reste).
Valable pour le lead **et** pour @haiku-dev. Si une commande échoue faute de conteneur, s'arrêter et le signaler
au lieu de la lancer en local.
- Vérification : `make verify` (gofmt + lint + `make test -race`, `E2E=1` pour e2e) : résumé compact, détail des seuls
  échecs, logs complets dans `.verify/*.log` (à lire ciblé, `grep`/`tail`, jamais en entier sans raison).
  `make test` / `make lint` / `make e2e` restent disponibles mais affichent tout : à éviter dans la conversation.
- Ne jamais lancer `docker compose` sans `-f`/`-p` depuis la racine : cela vise `compose.yml`, le projet de prod `hangarr`.

## Vérification dans un navigateur
- Suite e2e : `make verify E2E=1` (Playwright, Chromium, projet `hangarr-e2e`). À lancer avant de clore tout lot
  touchant `internal/web/` (templates, CSS, JS compris). Les comportements à prouver deviennent des scénarios e2e.
- Contrôle ponctuel (captures, Firefox), seulement pour un rendu visuel ou un audit d'UI : image Playwright du service `e2e` de `compose.dev.yml`, sous un projet
  dédié (`-p hangarr-check`, par exemple). Jamais dans `hangarr`, `hangarr-dev` (instance de l'utilisateur sur le
  port 8080) ni un autre projet existant ; `DEV_PORT` pour une instance isolée.

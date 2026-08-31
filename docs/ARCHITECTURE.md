# Architecture d’Temper

État observé le 31 août 2026 sur la branche de préparation de la PR #1. Les preuves de merge restent attachées au SHA final de la PR, pas à ce document.

## Résumé

Temper est aujourd’hui un petit pipeline Go qui lit des transcripts Cursor et Claude Code, applique un filtre déterministe, appelle un endpoint compatible OpenAI, puis propose des règles à écrire dans le dépôt. La base est exploitable, mais plusieurs frontières sont encore confondues : les formats sources, la sélection des signaux, la distillation, le stockage des suggestions et les écritures runtime.

La cible conserve le principe qui fait la valeur du projet : le chemin normal des agents reste intact. Temper apprend hors du chemin critique, par lots, à partir de traces déjà produites.

Le diagramme interactif est disponible dans [docs/diagrams/temper-control-plane.html](diagrams/temper-control-plane.html). Sa source versionnée se trouve dans [docs/diagrams/temper-control-plane.architecture.json](diagrams/temper-control-plane.architecture.json).

## Architecture actuelle

```text
Cursor JSONL ─┐
              ├─ collectors ─> canon.Session ─> préfiltre ─> distiller LLM
Claude JSONL ─┘                                      │
                                                    v
                                           suggestions SQLite
                                                    │
                                                    v
                                     review HTTP (acceptation humaine)
                                                    │
                                                    v
                                  AGENTS.md / .cursor / CLAUDE.md
```

| Composant | Responsabilité réelle | Limite observée |
|---|---|---|
| `cmd/temper` | CLI, orchestration du scan, daemon et installation de service | Plusieurs responsabilités produit et plateforme vivent dans un seul fichier. |
| `internal/collector` | Découverte et parsing Cursor/Claude vers `canon.Session` | Racines codées depuis le home; pas de collector Codex; résolution Cursor heuristique. |
| `internal/canon` | Session commune minimale | Les tools, résultats, erreurs, fichiers et outcomes ne forment pas encore un modèle explicite. |
| `internal/distill` | Préfiltre, prompt, validation de schéma et d’evidence, gardening | Une session utile devient directement une suggestion, sans `Experience` intermédiaire. Depuis HOK-539, tout egress (transcript, contexte existant, garden) passe par `internal/outbound`. |
| `internal/outbound` | Unique préparation du payload provider: redaction, neutralisation des marqueurs, bornes | Frontière de type, pas de sandbox: la redaction reste heuristique. |
| `internal/llm` | Interface d’inférence et providers HTTP chat-completions, Codex CLI, Claude Code CLI | Les CLIs conservent leur comportement système/auth interne; le provider HTTP mélange les en-têtes compatibles. |
| `internal/store` | SQLite local pour checkpoints, suggestions, journal d’acceptation et réservation de ressources | Depuis HOK-540: schéma versionné par `user_version` **et vérifié en forme** à l’ouverture (tables et colonnes obligatoires), migrations transactionnelles avec sauvegarde, refus explicite d’une base corrompue ou incomplète, checkpoint et suggestions dans une seule transaction, décisions en compare-and-set, claims durables par fichier et état d’abandon `rolling_back` explicite. Reste mono-processus et sans réplication. |
| `internal/writer` | Placement et mise à jour des artefacts runtime | Depuis HOK-540: mutation multi-fichiers planifiée en entier, journalisée, atomique par fichier, réversible et réconciliée au démarrage; toute opération filesystem passe par un `os.Root` sur une racine autorisée dont l’identité est capturée puis reprouvée à chaque reprise; checksums et permissions capturés sont des préconditions, revérifiées juste avant chaque remplacement puis sur le manifeste entier avant la décision. Les destinations restent surtout Cursor tant que le registry HOK-544 n’existe pas. |
| `internal/server` | API locale et UI embarquée | Loopback par défaut; les routes API vérifient Host/Origin et les mutations exigent une capacité éphémère, un JSON strict et un corps borné. Une adresse d’écoute non-loopback reste un choix explicite de l’opérateur. |
| `web` | Inbox de review React/Vite | Build séparé avec Bun 1.4.0; aucune suite de tests UI dédiée. |

## Problèmes structurants

1. ~~**La transcript traverse trop de niveaux de confiance.**~~ Fermé par HOK-539: le LLM propose uniquement le contenu sémantique; scope, placement, statut et plan d’artefact sont déterminés localement, puis un humain accepte avant toute écriture. Plus aucun script n’est extrait d’un bloc shell.
2. ~~**L’egress n’a pas une frontière unique.**~~ Fermé par HOK-539: `internal/outbound` est le seul constructeur de payload, et `llm.Provider.Generate` n’accepte rien d’autre.
3. ~~**La persistance n’est pas transactionnelle de bout en bout.**~~ Fermé par HOK-540: le checkpoint et les suggestions qu’il couvre avancent dans une seule transaction, et une acceptation est une saga durable `prepared → applying → rolling_back? → committed | rolled_back` réconciliée au démarrage — le filesystem, le journal et SQLite convergent après succès comme après **interruption du processus**. Le schéma est versionné, sa forme est vérifiée, et une base corrompue ou incomplète est refusée au lieu d’être utilisée à moitié. La portée exacte de la garantie de durabilité — interruption de processus prouvée, coupure d’alimentation matérielle non prouvée portablement — est décrite dans `SECURITY.md`.
4. **La suggestion remplace l’Experience.** Le système ne conserve pas un fait intermédiaire stable, dédupliqué et explicable.
5. **Le registry et les outputs runtime sont confondus.** Une connaissance canonique devrait précéder les adapters Claude, Codex et Cursor.
6. **Le support plateforme dépasse encore la CI.** Build, vet et suite Go passent localement sous Windows; l’amont automatise seulement Ubuntu.

## Architecture cible

```text
traces non fiables
      │
      v
collectors ─> normalizer ─> redaction ─> deterministic signals
                                               │
                                               v
                                       Experience Bank
                                               │ seuil / batch
                                               v
                                     provider de distillation
                                               │
                                               v
                                      candidate operations
                                               │ policy + review
                                               v
                                        skill registry
                                               │
                         ┌─────────────────────┼─────────────────────┐
                         v                     v                     v
                     Claude                Codex                 Cursor
```

### Composants et frontières

| Frontière | Contrat |
|---|---|
| `collectors/` | Découvrir et parser un runtime précis. Tolérer les champs inconnus. Ne jamais exécuter une donnée de transcript. |
| `normalizer/` | Produire une `Session` versionnée sans dépendance au format source. |
| `redaction/` | Construire l’unique payload autorisé à quitter la machine. Refuser l’egress qui ne satisfait pas la policy. |
| `signals/` | Extraire des signaux déterministes et expliquer toute sélection ou exclusion. |
| `experiences/` | Créer des `Experience` stables, sourcées, fingerprintées et dédupliquées. |
| `distillation/` | Traiter un lot borné d’Experiences. Produire des opérations candidates, jamais une vérité. |
| `validation/` | Vérifier schéma, provenance, sensibilité, destination, diff et policy. La review humaine reste le défaut. Une policy locale future ne pourra accepter que des changements non exécutables, entièrement prouvés et bornés. |
| `skills/` | Gérer le lifecycle `candidate → accepted → proven → deprecated`. |
| `storage/` | Versionner le schéma et porter une saga durable `prepared → applying → committed`, idempotente et réconciliée au démarrage. |
| `writers/` | Générer des outputs runtime depuis le registry. Écrire de façon atomique, confinée et réversible. |
| `providers/` | Adapter OpenAI, Anthropic, endpoint compatible et local sans répandre leurs particularités. |
| `platform/` | Centraliser chemins, Windows/WSL, permissions, daemon et diagnostics. |

Ces noms décrivent des frontières logiques. Ils ne prescrivent pas une arborescence immédiate ni un refactor massif.

## Modèle de données principal

### Session

```text
Session
├─ id, source, format_version
├─ agent, project, timestamps
├─ messages[]
├─ tool_calls[] / tool_results[]
├─ errors[]
├─ files_touched[]
├─ commands[]
└─ outcome_signals[]
```

### Experience

```text
Experience
├─ id, source_session_id, agent, project
├─ type, intent, signals, evidence
├─ files, commands, outcome
├─ fingerprint, novelty
└─ observed_at
```

### Skill et opération candidate

```text
Skill
├─ id, title, description, triggers, body, scope
├─ source_experience_ids[], evidence[]
├─ confidence, success_count, failure_count, last_used
├─ status, estimated_cost
└─ created_at, updated_at

CandidateOperation
├─ kind: NOOP | CREATE | UPDATE | MERGE | DEPRECATE
├─ target_skill_ids[]
├─ source_experience_ids[]
├─ proposed_diff
├─ validation_result
└─ review_state
```

## Flux de confiance

1. Les collectors lisent des données non fiables en lecture seule.
2. Le normalizer produit une représentation locale. Aucune donnée brute ne commande le programme.
3. La redaction et la policy construisent le payload outbound. Aucun autre composant n’appelle un provider.
4. Le provider renvoie une proposition non fiable.
5. La validation déterministe vérifie le schéma, les preuves, les destinations et les effets.
6. La review humaine autorise une opération candidate. L’auto-accept actuel est supprimé tant qu’une policy locale déterministe ne recalcule pas les preuves, la sensibilité et la destination. Cette exception future restera interdite au contenu exécutable.
7. Le registry enregistre la décision et prépare une saga durable avec manifeste, preimages, permissions et identité des racines autorisées, puis réserve chaque fichier que le manifeste nomme.
8. Les writers reprouvent chaque destination immédiatement avant de la remplacer, remplacent atomiquement, reprouvent le manifeste entier, puis marquent l’opération `committed`. Un échec après écriture passe par un `rolling_back` durable avant restauration. Au redémarrage, la reconciliation termine une opération `applying`, restaure une opération `rolling_back` sans jamais la rejouer en avant, et relâche une opération `prepared` sans toucher aux fichiers.

## Décisions

- [ADR-0001 — Garder l’apprentissage sur le cold path](adr/0001-cold-path-deterministe.md)
- [ADR-0002 — Séparer le registry canonique des sorties runtime](adr/0002-registry-canonique-et-adapters.md)

## Stratégie de migration

1. Fermer les frontières P0 sans déplacer tout le code.
2. Introduire les nouveaux modèles derrière les interfaces actuelles.
3. Migrer un collector et un writer à la fois avec des golden tests.
4. Conserver les données et la configuration existantes par migrations versionnées.
5. Supprimer les anciens chemins seulement après preuve de parité et rollback testé.

## Preuves et limites actuelles

- `go vet ./...` et `go build ./...` passent sous Windows.
- l’assertion de bit exécutable non portable d’`internal/writer` a disparu avec la génération de `run.sh` (HOK-539); la preuve d’exécution de `go test ./...` appartient à la revue de cette tranche, pas à ce document.
- Lint TypeScript et vérification des types passent lorsqu’ils sont lancés directement.
- Le frontend utilise Bun 1.4.0 avec lockfile gelé; la CI vérifie la version réelle de Bun, le lint, le build et l’état exact de `web/dist`.
- La CI amont prouve uniquement Ubuntu au même SHA, sans release ni artefact.

Ces éléments constituent une baseline. Ils ne prouvent ni la sécurité, ni la portabilité, ni la maturité du produit.

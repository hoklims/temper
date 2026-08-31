# ADR-0002 — Séparer le registry canonique des sorties runtime

Statut : accepté le 30 août 2026.

## Contexte

Claude Code, Codex et Cursor n’emploient ni les mêmes fichiers ni les mêmes mécanismes de chargement. Écrire directement une suggestion dans un artefact runtime confond la connaissance acceptée avec une représentation particulière.

## Décision

Temper conserve une source canonique locale pour les Experiences, les skills et leur lifecycle. Des adapters génèrent ensuite les sorties natives de chaque runtime. Le registry et les outputs restent séparés.

Les writers sont idempotents, atomiques, diffables et réversibles. Ils ne modifient jamais le contenu utilisateur hors des sections gérées. Aucun symlink n’est requis : Windows reste une plateforme de premier rang.

## Alternatives rejetées

- Utiliser `AGENTS.md` comme base canonique : ce fichier est une sortie runtime, pas une base de provenance.
- Maintenir trois registres indépendants : les divergences et doublons deviendraient inévitables.
- Imposer des symlinks entre runtimes : comportement fragile sur Windows et frontières de propriété floues.

## Migration

Le registry importe d’abord les suggestions et blocs gérés existants avec leur provenance disponible. Chaque adapter tourne en shadow et produit un diff. L’ancien writer reste l’autorité jusqu’à parité observée; le basculement se fait runtime par runtime avec rollback testé.

## Conséquences

- Une même skill peut être rendue différemment selon le runtime sans perdre sa provenance.
- Les migrations du registry et celles des outputs suivent des contrats distincts.
- Le routing natif et la progressive disclosure sont évalués avant tout retrieval propriétaire.
- Une suppression d’output n’efface pas l’historique canonique sans décision lifecycle explicite.

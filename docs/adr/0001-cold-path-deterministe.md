# ADR-0001 — Garder l’apprentissage sur le cold path

Statut : accepté le 30 août 2026.

## Contexte

Temper doit capitaliser sur les sessions passées sans ralentir Claude Code, Codex ou Cursor. Un hook cognitif après chaque action multiplierait les appels LLM, le coût, les points de panne et la surface d’injection.

## Décision

Le hot path des agents ne contient aucun appel Temper obligatoire. Les collectors lisent les traces déjà produites. Les signaux et la redaction s’exécutent localement. La distillation attend un seuil ou une commande explicite et traite un lot borné.

L’ordre de préférence reste : règle déterministe, retrieval lexical ou metadata, petit appel LLM ciblé, puis modèle puissant seulement si la tâche le justifie.

## Alternatives rejetées

- Appeler un LLM après chaque tool call : coût et surface d’injection incompatibles avec le produit.
- Injecter toutes les skills dans le contexte : croissance permanente et pertinence décroissante.
- Faire du daemon un agent autonome : autorité trop large et comportement difficile à reproduire.

## Migration

Le scan actuel reste le point d’entrée. Le préfiltre produit d’abord des Experiences persistées; le distiller bascule ensuite vers des lots. Les anciens checkpoints sont migrés sans relancer les transcripts déjà traitées, sauf commande explicite.

## Conséquences

- L’apprentissage peut être différé; il n’est pas instantané.
- Le produit doit gérer checkpoints, lots et reprises.
- Les statistiques doivent distinguer sessions découvertes, rejetées sans LLM et envoyées au provider.
- Le daemon observe et planifie; il ne réfléchit pas en continu.

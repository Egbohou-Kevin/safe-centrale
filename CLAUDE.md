# Consignes pour Claude

<!-- creator:profile go -->
## Vérifier sans gaspiller

Les règles générales sont dans `~/.claude/CLAUDE.md` : petits lots, avancer puis tester, une
seule vérification complète par commit.

- En itérant : `go test ./internal/<paquet>/...`, pour le seul paquet touché.
- À chaque commit, le crochet lance `scripts/check.sh --quick` (gofmt, vet, `go test -short`,
  règles de qualité et d'architecture) : ne jamais le relancer à la main juste avant.
- Le `scripts/check.sh` complet (race, shellcheck, build pour la cible) tourne une fois, avant
  la fusion.
<!-- creator:end -->

## Le cloud, seule source de vérité (AD-23)

Kevin, 31/10/2026 : la centrale, l'appli et la console ne contiennent que le minimum. Tout le reste
(noms, règles, mise en forme partagée) est traité dans le cloud, l'intermédiaire qui fait la
correspondance entre eux. Un contexte du cloud qui a besoin d'un autre ouvre un port (sortie chez
lui, entrée chez le propriétaire, pont dans `app`) : jamais une raison de traiter ailleurs.
Détail : `safeHome-cloud/docs/00-decisions-d-architecture.md`, AD-23.

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

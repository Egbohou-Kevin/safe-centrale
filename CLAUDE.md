# Consignes pour Claude

## Travailler sans gaspiller (les trois dépôts de safeHome)

Les règles générales sont dans `~/.claude/CLAUDE.md`. Voici leurs commandes, dépôt par dépôt.
Le crochet avant commit lance déjà la vérification : ne jamais la relancer à la main juste avant
de commiter.

- **Centrale (ce dépôt).**
  - En itérant : `go test ./internal/<paquet>/...`, pour le seul paquet touché.
  - À chaque commit, le crochet lance `scripts/check.sh --quick`.
  - Le `scripts/check.sh` complet (race, scénarios de bout en bout, script d'installation) tourne
    une fois, avant la fusion.
- **Cloud (`../safeHome-cloud`).**
  - En itérant : `./mvnw -o -q test -pl <module> -Dtest='<Classes>'`, pour le seul module touché.
  - À chaque commit, le crochet lance `scripts/check.sh --quick`, sans Docker.
  - Le `scripts/check.sh` complet, avec Docker, tourne une fois, avant la fusion.
- **Appli (`../safeHome-app`).**
  - En itérant : `./gradlew --offline :shared:testAndroidHostTest --tests '<Classe>'`, ou la seule
    compilation du module touché.
  - À chaque commit, le crochet lance `scripts/check.sh` complet : tests, lint, APK debug et
    release.
  - On commite donc peu souvent, un commit par étape achevée, et jamais `scripts/check.sh` lancé à
    la main avant un commit.

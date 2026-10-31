<img src="seq-icone.svg" width="56" alt="SeQ">

# La marque : SeQ

La marque est SeQ, à lire « sécu » (Kevin, 31/10/2026). La référence est dans le dépôt de l'application :
`safeHome-app/docs/marque/README.md`. On y trouve le nom, les couleurs et l'animation de chargement.

Ici, on trouve :
- `seq-icone.svg` : l'icône, copie de la référence. Elle se met en tête de chaque document (Kevin, 31/10/2026
  03:23 : « dans les rapports, dans les trucs d'architecture, tout, partout »). Le contrôle
  `internal/qualitytest/brand_test.go` fait échouer le build pour un document qui ne l'a pas.
- La console d'installation (`internal/consoleapp/web`) : l'icône est en tête de page et sert de favicon.
  L'icône animée s'affiche pendant une attente, l'appairage ouvert ou les tests sans événement. L'en-tête et
  les boutons sont en bleu nuit `#0F2A44`.

Plus aucun « safeHome » visible (Kevin, 31/10/2026 03:40 : « Je ne veux plus de safe home quelque part. SeQ »,
puis « partout ») : titre et en-tête de la console, nom de son Wi-Fi (`SeQ-<site>` par défaut), SMS, descriptions
des services systemd, documents. Les identifiants techniques restent tels quels (Kevin, 31/10/2026 03:45:39 : « on garde les
identifiants techniques […] Seul le visible devient SeQ ») : le dépôt, le module Go `safehome`, les programmes et services `safehome-…`, le compte `safehome`, les chemins sur le
Pi. La centrale n'a pas d'écran qui démarre, donc pas d'animation de lancement (F).

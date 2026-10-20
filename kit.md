# Kit de base — petit commerce (Togo)

Kit de départ pour une boutique avec une porte d'entrée et une arrière-cour.
Les modèles viennent de la connaissance du matériel courant. Les prix sont des
ordres de grandeur en euros, **non vérifiés** sauf celui du cerveau (prix France
vérifiés en octobre 2026) : à confirmer avant de commander.

## Liste

| Élément | Qté | Modèle possible | Prix approx. |
|---|---|---|---|
| Cerveau | 1 | Raspberry Pi 4 **1 Go**, carte SD « haute endurance » (32 Go), alimentation officielle (voir « Choix du cerveau ») | 60-70 € |
| Radio Zigbee | 1 | Sonoff ZBDongle-E | 15-20 € |
| Modem 4G + SMS/appels (une SIM) | 1 | SIM7600E-H (HAT ou USB), ou Quectel EC25 | 50-70 € |
| Détecteur d'ouverture | 2 | Aqara MCCGQ11LM (porte et arrière-cour) | 15-20 € pièce |
| Détecteur de mouvement | 2 | Aqara RTCGQ11LM | 15-25 € pièce |
| Détecteur de fumée | 1 | Zigbee avec sirène intégrée | 20-30 € |
| Détecteur de bris de vitre ou vibration | 1 | Zigbee (Tuya ou Heiman) | 20-30 € |
| Sirène | 1 | Sirène Zigbee (Heiman ou Tuya) | 25-35 € |
| Clavier ou télécommande | 1 | Clavier Zigbee ou badge | 20-30 € |
| Bouton panique | 1 | Bouton SOS Zigbee | 10-20 € |
| Caméras | 2 | Tapo C200 ou Reolink avec flux RTSP, **en H.264** et avec un flux secondaire basse définition (pour le direct, voir `docs/video.md`) | 25-50 € pièce |
| Routeur 4G Wi-Fi | 1 | Seulement pour un client sans box : relie les caméras et la centrale. Forfait de données à la charge du client | 40-60 € |
| Module d'horloge à pile (RTC) | 1 | Module RTC compatible Raspberry Pi, pour garder l'heure pendant les coupures | 5-10 € |
| Batterie de secours de la centrale | 1 | Module batterie ou UPS qui **signale son état** à la centrale : sur secteur ou sur batterie, niveau de charge (par exemple un UPS HAT pour Raspberry Pi) | 40-80 € |
| Boîtier, câbles | | Boîtier sécurisé. La SIM et son crédit sont à la charge du client | 20-40 € |

Total estimé : **395 à 625 €**, hors routeur 4G (réservé aux clients sans box).
Pour un premier test, on peut retirer une caméra.

## Choix du cerveau

La centrale fait tourner Mosquitto, Zigbee2MQTT et nos trois programmes Go
(centrale, passerelle GSM, adaptateur Zigbee). Estimation : 400 à 500 Mo de mémoire
en tout, très peu de calcul (à mesurer sur le kit réel). Le service caméras ne fait
que des photos à l'alarme, pas d'enregistrement continu. **1 Go de mémoire suffit** ;
un Raspberry Pi 4 ou 5 avec 4 Go est surdimensionné.

Ce qui compte vraiment : deux ports USB (clé Zigbee et modem), une faible
consommation (batterie de secours plus petite pendant les coupures), une carte SD qui
ne s'use pas, la disponibilité et un système maintenu dans le temps.

Prix TTC en France relevés en octobre 2026 (Kubii, revendeur officiel, et presse
spécialisée), avant transport et douane vers le Togo. Les modèles 2 Go viennent
d'augmenter le 1er octobre 2026 à cause de la pénurie de mémoire ; les modèles 1 Go
n'ont pas bougé.

| Carte | Mémoire | Conso. | Prix TTC France | Avis |
|---|---|---|---|---|
| **Raspberry Pi 4 (1 Go)** | 1 Go | ~3 W | 41,70 à 42,90 € (Kubii) ; kit complet 69,90 € | **Retenu** : assez de mémoire, écosystème solide, prix inchangé |
| Raspberry Pi 5 (1 Go) | 1 Go | 3-5 W | ~51 € (jusqu'à 66 € selon les boutiques) | Horloge intégrée (pile à ajouter), mais plus gourmand et exige une alimentation 5 A |
| Raspberry Pi 4 (2 Go) | 2 Go | ~3 W | ~66 € | Marge inutile pour nous, plus de 50 % plus cher que le 1 Go |
| Raspberry Pi 5 (2 Go) | 2 Go | 3-5 W | ~77 € | Surdimensionné |
| Raspberry Pi 5 (4 Go) | 4 Go | 3-5 W | 130 à 137 € | Très surdimensionné : c'était le choix du kit d'origine |
| Orange Pi Zero 3 (1 Go) | 1 Go | ~1 W | Pas de prix fiable trouvé en France (17 à 30 € à sa sortie) | Système maintenu par la communauté, achat surtout sur AliExpress. Option si la consommation devient critique |
| Raspberry Pi Zero 2 W | 512 Mo | < 1 W | Non vérifié en France (rare en 2026) | Trop juste en mémoire pour Zigbee2MQTT, Wi-Fi seulement, un seul port USB : déconseillé |

Le kit complet Kubii (Pi 4 1 Go, boîtier, carte SD 32 Go, alimentation, câble) est
pratique pour le pilote, mais sa carte SD n'est pas « haute endurance » : la changer
pour une installation réelle.

Nos programmes se compilent pour l'ARM 64 bits et 32 bits : on n'est lié à aucune
carte. À faire sur le kit d'essai : mesurer la mémoire réellement utilisée et la
consommation, pour confirmer le 1 Go et dimensionner la batterie de secours.

Sources : [Kubii, Raspberry Pi 4 1 Go](https://www.kubii.com/fr/cartes-nano-ordinateurs/2770-raspberry-pi-4-modele-b-1gb-5056561800325.html),
[Kubii, kit Pi 4 1 Go](https://www.kubii.com/fr/kits-nano-ordinateurs/3775-starter-kit-raspberry-pi4-1gb-3272496312388.html),
[Next, hausse des 2 Go (octobre 2026)](https://next.ink/brief-article/hausse-de-prix-pour-les-raspberry-pi-4-et-5-de-2-go-qui-passent-a-66-et-76-euros/),
[Hardware & Co](https://hardwareand.co/actualites/breves/cest-lete-il-faut-chaud-mais-les-prix-des-raspberry-pi-eux-font-froid-dans-le-dos),
[Blog Nouvelles Technologies (Orange Pi Zero 3)](https://www.blog-nouvelles-technologies.fr/265825/orange-pi-zero-3-est-minuscule-pc-monocarte-bon-marche-mais-allechant/).

## Pourquoi ces choix

- **Batterie qui signale son état** : la centrale détecte elle-même qu'elle passe
  sur batterie (donc une coupure du secteur) et connaît le niveau de charge. Une
  simple batterie externe sans sortie d'état ne suffit pas : il faut un module que
  la centrale peut interroger. À dimensionner pour plusieurs heures de coupure.
- **Modem 4G séparé du routeur** : il envoie SMS et appels même sans internet.
  Vérifier que ses bandes LTE correspondent à celles de Togocom et Moov
  (par exemple B3, B7, B20).
- **Détecteurs Zigbee, caméras à part** : les capteurs sur piles restent en
  service pendant une panne. Les caméras sont un confort : si elles tombent,
  l'alarme continue.
- **Une SIM en v1**, fournie et rechargée par le client. Une deuxième SIM d'un autre
  opérateur demande un deuxième modem et viendra plus tard. Nous vérifions l'état
  de la SIM (voir `docs/escalation.md` et `docs/gsm-gateway.md`).
- **Zigbee 2,4 GHz** : même fréquence dans le monde entier, contrairement au
  868 MHz qui est européen.

## À prévoir

- Les caméras ne survivent pas aux coupures sans batterie : les alimenter aussi
  sur secours, ou accepter qu'elles s'éteignent.
- La box du client existe avant nous et n'est pas gérée par nous : pendant une
  coupure, elle s'éteint et les alertes passent par GSM.
- Les piles des détecteurs s'usent : noter les dates de remplacement.
- Choisir un capteur de mouvement qui ne se déclenche pas à cause des animaux
  ou de la chaleur.
- Vérifier l'homologation du matériel radio auprès de l'ARCEP Togo.

<img src="docs/marque/seq-icone.svg" width="56" alt="SeQ">

# Kit de base — petit commerce (Togo)

Kit de départ pour une boutique avec une porte d'entrée et une arrière-cour.
Les modèles viennent de la connaissance du matériel courant. Les prix marqués
« vérifié » ont été relevés le 22 octobre 2026 (TTC France, avant transport et douane
vers le Togo) ; les autres sont des ordres de grandeur en euros, **non vérifiés** : à
confirmer avant de commander.

## Liste

| Élément | Qté | Modèle possible | Prix approx. |
|---|---|---|---|
| Cerveau | 1 | Raspberry Pi 4 **1 Go** (42 à 56 €), carte SD SanDisk « haute endurance » 32 Go (27 à 29 €), alimentation officielle (12,50 €, souvent en rupture : LDLC l'a en stock). Voir « Choix du cerveau » | 80-100 € (vérifié) |
| Radio Zigbee | 1 | Sonoff ZBDongle-E | 15-20 € |
| Modem 4G + SMS/appels (une SIM) | 1 | Waveshare SIM7600E-H USB (91,77 € chez Eckstein plus port, 133,99 € sur Amazon.fr), ou Quectel EC25. **Reporté** : voir « Pourquoi ces choix » pour les bandes | 92-134 € (vérifié) |
| Détecteur d'ouverture | 2 | Aqara MCCGQ11LM (porte et arrière-cour). Présenté comme arrêté chez Aqara : en prendre tant qu'il est en stock, sinon un autre capteur Zigbee et son profil (`config/profiles`) | 19,99 € pièce (vérifié) |
| Détecteur de mouvement | 2 | Aqara RTCGQ11LM | 15-25 € pièce |
| Détecteur de fumée | 1 | Zigbee avec sirène intégrée | 20-30 € |
| Détecteur de bris de vitre ou vibration | 1 | Zigbee (Tuya ou Heiman) | 20-30 € |
| Sirène | 1 | Sirène Zigbee (Heiman ou Tuya) | 25-35 € |
| Clavier | 1 | frient (Develco) KEPZB-110, celui de nos profils : la plus grosse ligne du kit | 100-106 € (vérifié) |
| Bouton panique | 1 | Tuya TS0215A (compatible Zigbee2MQTT, **sans profil** chez nous pour l'instant) | 7-15 € (vérifié) |
| Caméras | 2 | Tapo C200 ou Reolink avec flux RTSP, **en H.264** et avec un flux secondaire basse définition (pour le direct, voir `docs/video.md`) | 25-50 € pièce |
| Routeur 4G Wi-Fi | 1 | Seulement pour un client sans box : relie les caméras et la centrale. Forfait de données à la charge du client | 40-60 € |
| Module d'horloge à pile (RTC) | 0-1 | **Une horloge à pile est nécessaire** (le Pi 4 n'en a pas, voir `docs/agent.md`, « Horloge ») : celle du X728 si sa carte en porte bien une (annoncée par ses fiches, à confirmer à la réception), sinon un module DS3231 compatible Raspberry Pi, à acheter aussi tant que le X728 manque | 0-10 € |
| Batterie de secours de la centrale | 1 | **Geekworm X728** (choisi le 21 octobre 2026, 45 $ chez Geekworm, **en rupture en Europe** en octobre 2026) : il signale la coupure et la charge à la centrale (`docs/alimentation.md`). Les accus 18650 ne sont pas fournis et voyagent mal par avion : les acheter sur place (paire Samsung 30Q, 14 à 17 €). Il se branche en USB-C 5 V 3 A : l'alimentation officielle du Pi suffit | 55-60 € (vérifié) |
| Boîtier, câbles | | Boîtier sécurisé. La SIM et son crédit sont à la charge du client | 20-40 € |

Total estimé : **555 à 770 €**, hors routeur 4G (réservé aux clients sans box).
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
  Vérifier que ses bandes LTE correspondent à celles de Togocom et Moov : le Togo
  utilise B3, B7, B20 et B66 selon FrequencyCheck, mais la répartition par opérateur
  n'est pas confirmée. À demander aux opérateurs avant tout achat ; la 2G reste le
  repli pour le SMS.
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
- Un modem 4G tire des pointes d'environ 2 A sur l'USB. Un hub USB alimenté (50 à
  60 €) ne s'achète que si le modem décroche à l'essai.
- Choisir un capteur de mouvement qui ne se déclenche pas à cause des animaux
  ou de la chaleur.
- Vérifier l'homologation du matériel radio auprès de l'ARCEP Togo.

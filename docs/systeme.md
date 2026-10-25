# Système de la centrale

Statut : procédure v1, écrite avant le kit d'essai. Les fichiers à poser sont dans
`deploy/` et un test (`go test ./deploy`) vérifie qu'ils restent d'accord avec les
programmes et leurs configurations d'exemple : même broker, même dossier d'état, même
`base_topic`, un service par programme, redémarrage sans fin. Les étapes marquées
**à valider** n'ont pas encore été faites sur un vrai Raspberry Pi.

## Principes

- **Raspberry Pi 4 (1 Go)** et **Raspberry Pi OS Lite 64 bits** (sans bureau), version
  stable du moment.
- **Système en lecture seule** : une coupure de courant brutale (batterie vide) ne
  peut pas abîmer le système. Tout ce qui doit être écrit va sur une **partition de
  données** à part : l'état de l'alarme, les messages retenus de Mosquitto, le réseau
  Zigbee.
- **systemd** démarre et surveille tout : chaque service redémarre sans fin s'il
  tombe, et nos programmes sont redémarrés s'ils se figent (chien de garde de
  systemd, 30 s). Le **chien de garde matériel** du Pi redémarre la carte si le
  système entier se fige.
- **Journaux en mémoire** : la carte SD n'est pas usée à chaque ligne.
- **Pas de Docker** : neuf services, autant de fichiers simples, moins de mémoire.
- **Rien n'écoute sur le réseau du commerce**, sauf SSH par clé (pas de mot de passe).
  La console locale n'écoute que sur le réseau Wi-Fi propre à la centrale, ne répond
  qu'aux appareils de ce réseau, et seulement pendant le mode installation.

## Ce qui tourne

| Service | Rôle | Fichier dans `deploy/` |
|---|---|---|
| `mosquitto` | Le broker MQTT, sur la centrale seulement (`127.0.0.1:1883`) | `mosquitto/mosquitto.conf` |
| `zigbee2mqtt` | Le pont entre la clé Zigbee et le broker | `systemd/zigbee2mqtt.service`, `zigbee2mqtt/configuration.yaml` |
| `safehome-zigbee` | Notre adaptateur Zigbee | `systemd/safehome-zigbee.service` |
| `safehome-gsm` | La passerelle GSM (SMS, appels) | `systemd/safehome-gsm.service` |
| `safehome-hub` | La centrale : moteur, supervision, escalade | `systemd/safehome-hub.service` |
| `safehome-agent` | L'agent de liaison avec le cloud (connexion sortante seulement) | `systemd/safehome-agent.service` |
| `safehome-video` | La vidéo en direct (un go2rtc par session, sans port ouvert) et les photos à l'alarme (site avec caméras) | `systemd/safehome-video.service` |
| `safehome-maint` | La maintenance à distance : un tunnel SSH sortant vers notre bastion, sur demande, une heure au plus (`maintenance.md`) | `systemd/safehome-maint.service` |
| `safehome-power` | Le module batterie (Geekworm X728) : coupure du secteur, charge, arrêt propre avant la batterie vide (`alimentation.md`) | `systemd/safehome-power.service` |
| `safehome-console` | La console locale : pendant le mode installation seulement, le réseau Wi-Fi de la centrale et la page de l'installateur (`security-install.md`) | `systemd/safehome-console.service`, `system/polkit-safehome-console.rules`, `system/nm-dispatcher-safehome-console` |

Réglages système : `system/journald-safehome.conf` (journaux),
`system/watchdog-safehome.conf` (chien de garde), `system/fstab-data` (partition de
données), `system/polkit-safehome-console.rules` (la console peut créer, lever,
baisser et supprimer les connexions de NetworkManager, sans éteindre la radio ; la
règle vaut pour tout programme lancé sous `safehome`, voir la limite dans
`security-install.md`), `system/nm-dispatcher-safehome-console` (le réseau de la
console ne route pas les téléphones vers le réseau du commerce ni vers internet ;
sans lui, la console ne lève pas son réseau).

Nos programmes tournent sous l'utilisateur `safehome`, sans droits
d'administration, et ne peuvent écrire que dans `/var/lib/safehome`
(`ProtectSystem=strict`). La configuration réglée depuis l'application y est gardée
(`site-config.json`, mode 0600 : elle contient les numéros des contacts), puisque
`/etc/safehome` est en lecture seule (voir `operations.md`). La passerelle GSM et Zigbee2MQTT ont en plus accès aux ports
série (groupe `dialout`), le programme du module batterie à ses broches et à son bus
(groupes `gpio` et `i2c`).

## Disposition de la carte SD (32 Go)

| Partition | Taille | Contenu | Montée |
|---|---|---|---|
| 1 `bootfs` | celle de l'image | démarrage | lecture seule |
| 2 `rootfs` | 8 Go | le système et nos programmes | lecture seule (couche en mémoire) |
| 3 `safehome-data` | le reste | `/data` : `safehome/`, `mosquitto/`, `zigbee2mqtt/` | lecture-écriture |

Les trois dossiers de `/data` sont « attachés » (bind) à leur place habituelle :
`/var/lib/safehome`, `/var/lib/mosquitto`, `/var/lib/zigbee2mqtt`. Les programmes ne
savent pas qu'ils écrivent sur une autre partition.

## Installation pas à pas

Compter une heure la première fois. Les commandes se tapent sur la centrale, en SSH,
sauf mention contraire.

Les étapes 1, 2, 4 et 8 se font à la main. Le script d'installation fait
l'installation de Mosquitto (étape 3), les étapes 5, 6 et 7, et les services de
l'étape 9 (voir « Installer par le script » plus bas). La mise à jour du système,
`fake-hwclock`, SSH et le Bluetooth (étape 3), les codes (étape 9) et les étapes 10 à
12 restent à la main.

### 1. Préparer la carte (sur l'ordinateur)

1. Avec **Raspberry Pi Imager** : modèle Raspberry Pi 4, système « Raspberry Pi OS Lite
   (64-bit) », la carte SD haute endurance.
2. Dans les réglages d'Imager :
   - nom de la machine : `safehome-tg-lom-0001` (l'identifiant du site) ;
   - utilisateur d'administration (pas `pi`) ;
   - **SSH avec clé publique seulement** (coller sa clé, pas de mot de passe) ;
   - Wi-Fi de la box si la centrale n'est pas branchée par câble ;
   - fuseau horaire `Africa/Lome`.
3. **Empêcher l'agrandissement automatique** de la racine au premier démarrage, pour
   laisser la place à la partition de données. **À valider** : la méthode dépend de la
   version de Raspberry Pi OS (option dans `cmdline.txt` ou dans la configuration de
   premier démarrage, sur la partition `bootfs` que l'ordinateur sait lire). Si la
   racine a déjà pris toute la carte, réécrire la carte et recommencer.

Plus tard, on préparera une **image maîtresse** une fois pour toutes (carte déjà
partitionnée, tout installé) et chaque nouvelle centrale sera une simple copie.

### 2. Partitions

```bash
# agrandir la racine à 8 Go (possible pendant qu'elle tourne), puis créer les données
sudo parted /dev/mmcblk0 resizepart 2 8GiB
sudo resize2fs /dev/mmcblk0p2
sudo parted /dev/mmcblk0 mkpart primary ext4 8GiB 100%
sudo mkfs.ext4 -L safehome-data /dev/mmcblk0p3

sudo mkdir -p /data /var/lib/safehome /var/lib/mosquitto /var/lib/zigbee2mqtt
# ajouter à /etc/fstab les lignes de deploy/system/fstab-data, puis :
sudo mount /data
sudo mkdir -p /data/safehome /data/mosquitto /data/zigbee2mqtt
sudo mount -a
```

`commit=5` dans `fstab` : les données sont écrites sur la carte au plus tard 5 s
après le changement (au lieu de 30). L'état de l'alarme survit ainsi à une coupure qui
suit de près un armement.

### 3. Système de base

Depuis l'ordinateur, copier d'abord les fichiers à poser (les commandes suivantes les
lisent dans `~/deploy`) :

```bash
scp -r deploy admin@safehome-tg-lom-0001:
```

Puis sur la centrale :

```bash
sudo apt update && sudo apt full-upgrade -y
sudo apt install -y mosquitto
sudo apt purge -y fake-hwclock          # l'heure vient du module RTC (étape 4)
swapon --show                           # pas de fichier d'échange sur la carte (zram en mémoire seulement)
```

- **SSH** : vérifier `PasswordAuthentication no` dans `/etc/ssh/sshd_config.d/` (Imager
  le règle quand on choisit la clé seule).
- **Bluetooth** : inutile, le couper (`dtoverlay=disable-bt` dans
  `/boot/firmware/config.txt`).
- Pas d'interface graphique, pas de serveur web, rien d'autre qui écoute :
  `sudo ss -tlnp` ne doit montrer que SSH (22) et Mosquitto sur `127.0.0.1:1883`.

### 4. Heure (module RTC)

Le Pi 4 n'a pas d'horloge à pile : sans elle, après une coupure sans internet, la
centrale repartirait à une date fausse (armement automatique à la mauvaise heure).

Pour un module DS3231 branché sur les broches I2C, dans `/boot/firmware/config.txt` :

```
dtparam=i2c_arm=on
dtoverlay=i2c-rtc,ds3231
```

Après redémarrage : `timedatectl` (l'heure, `Africa/Lome`, la synchronisation par
internet) et `sudo hwclock -r` (l'heure du module). Quand internet est là, le système
se cale dessus et met le module à l'heure. **À valider** avec le module choisi.

**Module batterie (X728).** Ses réglages vont dans le même fichier : voir
`alimentation.md`, « Réglages du système ». Le module porte peut-être sa propre
horloge, qui dispenserait du DS3231 : **à valider** avec la carte.

Le heartbeat porte `clock_ok` : c'est la ligne `System clock synchronized` de
`timedatectl`. Avec le module, l'heure est juste dès le démarrage, mais `clock_ok`
reste à `false` jusqu'au retour d'internet : le noyau ne connaît que la synchronisation
par NTP.

### 5. Journaux et chien de garde

```bash
sudo mkdir -p /etc/systemd/journald.conf.d /etc/systemd/system.conf.d
sudo cp deploy/system/journald-safehome.conf /etc/systemd/journald.conf.d/safehome.conf
sudo cp deploy/system/watchdog-safehome.conf /etc/systemd/system.conf.d/safehome-watchdog.conf
sudo systemctl restart systemd-journald
sudo systemctl daemon-reexec
systemctl show -p RuntimeWatchdogUSec   # doit afficher 15s
```

Journaux : 30 Mo en mémoire au plus, perdus au redémarrage. Ce qui compte (événements,
alertes) est dans MQTT et part au cloud.

Chien de garde : si le système ne répond plus pendant 15 s, la carte redémarre seule.

### 6. Mosquitto

```bash
sudo cp deploy/mosquitto/mosquitto.conf /etc/mosquitto/mosquitto.conf
sudo chown mosquitto:mosquitto /data/mosquitto
sudo systemctl restart mosquitto
```

Le fichier remplace celui de Debian : un seul point d'écoute, sur la centrale, et les
messages retenus sauvegardés sur la partition de données toutes les 5 minutes. Après
un arrêt brutal (coupure de courant, redémarrage par le chien de garde), les états des
appareils sont donc ceux de la dernière sauvegarde, vieux de 5 minutes au plus : le
moteur d'alarme écarte au démarrage un état plus ancien que ce qu'il avait entendu
(voir `engine.md`, « Persistance et redémarrage »).

**Le code d'accès n'attend pas dans le broker.** Les programmes gardent leur session
un jour : en QoS 1, Mosquitto garde pour un programme absent (un redémarrage pour une
nouvelle configuration, un plantage) ce qui arrive pour lui, et l'écrit dans
`mosquitto.db` à la sauvegarde suivante ou à son propre arrêt. Les deux messages de
la centrale qui portent le code en clair partent donc en QoS 0, et le broker n'en
garde aucun : l'ordre de l'application que l'agent republie sur `alarm/cmd`, et la
séquence d'un clavier avec son code sur `device/+/event` (les autres événements du
clavier, comme l'armement, restent en QoS 1). Le prix : un code tapé ou envoyé
pendant les quelques secondes où la centrale redémarre est perdu, et se redonne ; il
aurait expiré au bout d'une minute de toute façon. En amont, Zigbee2MQTT publie lui
aussi les messages des claviers en QoS 0 : l'adaptateur Zigbee lui pose l'option
d'appareil `qos: 0` à l'appairage de chaque clavier, et de nouveau à chaque démarrage
(`adapter-zigbee.md`, « Clavier : QoS 0 ») ; les autres appareils restent en QoS 1.
`queue_qos0_messages false` est écrit dans le fichier (c'est la valeur par défaut) et
vérifié par `go test ./deploy`.

### 7. Utilisateur et programmes safeHome

Sur l'ordinateur (voir `operations.md`) :

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-hub ./cmd/safehome-hub
GOOS=linux GOARCH=arm64 go build -o safehome-gsm ./cmd/safehome-gsm
GOOS=linux GOARCH=arm64 go build -o safehome-zigbee ./cmd/safehome-zigbee
GOOS=linux GOARCH=arm64 go build -o safehome-agent ./cmd/safehome-agent
GOOS=linux GOARCH=arm64 go build -o safehome-video ./cmd/safehome-video
GOOS=linux GOARCH=arm64 go build -o safehome-maint ./cmd/safehome-maint
GOOS=linux GOARCH=arm64 go build -o safehome-console ./cmd/safehome-console
GOOS=linux GOARCH=arm64 go build -o safehome-install ./cmd/safehome-install
scp safehome-hub safehome-gsm safehome-zigbee safehome-agent safehome-video safehome-maint safehome-console safehome-install admin@safehome-tg-lom-0001:
```

go2rtc, pour la vidéo (seulement sur un site avec caméras) : le fichier
`go2rtc_linux_arm64` de la version 1.9.14, sur la page des versions du projet
AlexxIT/go2rtc sur GitHub. Vérifier son empreinte SHA-256 avec celle indiquée sur
cette page (`shasum -a 256 go2rtc_linux_arm64`), puis le copier sur la centrale avec
les autres. Les photos à l'alarme demandent aussi **ffmpeg**, qui décode une image de
la caméra (paquet Debian, installé plus bas). `safehome-video check` prévient s'il
manque.

Sur la centrale :

```bash
sudo useradd --system --home-dir /var/lib/safehome --shell /usr/sbin/nologin safehome
sudo chown safehome:safehome /data/safehome /data/zigbee2mqtt
sudo chmod 700 /data/safehome
sudo install -m 755 safehome-hub safehome-gsm safehome-zigbee safehome-agent safehome-video safehome-maint safehome-console safehome-install /usr/local/bin/
sudo install -m 755 go2rtc_linux_arm64 /usr/local/bin/go2rtc
sudo apt install --no-install-recommends ffmpeg     # site avec caméras : les photos à l'alarme

sudo mkdir -p /etc/safehome
# hub.json, gsm.json, zigbee.json, agent.json, video.json, maint.json, console.json, power.json et profiles/ : partir de config/*.example.json
# console.secret : la phrase de passe du Wi-Fi de la console, pour l'étiquette (8 à 63 caractères ASCII imprimables)
# maint.key, maint_known_hosts : la clé de la centrale et celle du bastion (maintenance.md)
# cloud.secret : le secret du site pour le cloud, fourni à l'installation (security-install.md)
sudo chown -R root:safehome /etc/safehome
sudo chmod -R u=rwX,g=rX,o= /etc/safehome     # contacts, secret du cloud, mots de passe des caméras

safehome-hub check    -config /etc/safehome/hub.json
safehome-gsm check    -config /etc/safehome/gsm.json
safehome-zigbee check -config /etc/safehome/zigbee.json
safehome-agent check  -config /etc/safehome/agent.json
safehome-video check  -config /etc/safehome/video.json
safehome-maint check  -config /etc/safehome/maint.json
safehome-console check -config /etc/safehome/console.json
safehome-power check  -config /etc/safehome/power.json
```

Les caméras : mot de passe d'usine changé, flux secondaire réglé en H.264, accès à
leur cloud de marque coupé si possible (`video.md`).

`cloud.secret` appartient donc à `root:safehome` avec les droits `0640` : l'agent peut
le lire, pas le modifier, et les autres utilisateurs ne le voient pas.

### 8. Zigbee2MQTT

**Node.js : la version 24 (LTS), pas une autre.** Zigbee2MQTT ne tourne que sur les
versions de Node.js qu'il déclare (2.14.2 : `^22.2.0 || ^24 || <=26.2`), et la
dernière version de Node.js n'en fait pas forcément partie. Constaté à l'essai sur le
matériel du 24 octobre 2026 : avec Node.js 26.5, Zigbee2MQTT ouvrait le port série de
la clé, mais rien de ce qu'il écrivait ne lui parvenait (la clé répondait à la
réinitialisation, `RSTACK`, puis chaque trame EZSP attendait jusqu'à l'échec) ; avec
Node.js 24.22.1, tout a marché aussitôt. On installe donc Node.js depuis le dépôt de
NodeSource **de la ligne 24 seulement** (`node_24.x`) : les mises à jour du système y
prennent les correctifs de la 24, jamais une autre version majeure. Pas le dépôt
`lts` ni `current`, qui changent de version majeure d'eux-mêmes, ni le `nodejs` de
Debian, trop vieux.

```bash
curl -fsSL https://deb.nodesource.com/setup_24.x -o nodesource_setup.sh
less nodesource_setup.sh                 # le lire avant de le lancer
sudo bash nodesource_setup.sh
sudo apt-get install -y nodejs
/usr/bin/node --version                  # v24.x : celui que lance notre service
```

`deploy/install.sh` vérifie `/usr/bin/node` dès que Zigbee2MQTT est installé, et
refuse d'aller plus loin si ce n'est pas une version 22 ou 24, avec la raison.

Installer ensuite Zigbee2MQTT (version 2.x ; 2.14.2 est celle de l'essai) dans
`/opt/zigbee2mqtt` en suivant le guide officiel « Linux » de Zigbee2MQTT, avec deux
différences :

- le dossier de données est `/var/lib/zigbee2mqtt` (variable `ZIGBEE2MQTT_DATA` dans
  notre service), pas `/opt/zigbee2mqtt/data` ;
- on utilise notre service `deploy/systemd/zigbee2mqtt.service` (utilisateur
  `safehome`), qui lance `/usr/bin/node`.

La clé Sonoff ZBDongle-E se présente en USB `10c4:ea60` (« Sonoff Zigbee 3.0 USB Dongle
Plus V2 ») ; à l'essai, son micrologiciel était EmberZNet 7.4.4 (EZSP 13), piloté par
l'adaptateur `ember` à 115200 bauds sans contrôle de flux matériel (`rtscts: false`),
ce que dit `deploy/zigbee2mqtt/configuration.yaml`.

```bash
ls /dev/serial/by-id/                    # le nom stable de la clé Zigbee
sudo -u safehome cp deploy/zigbee2mqtt/configuration.yaml /var/lib/zigbee2mqtt/
# y mettre le port de la clé ; adapter: ember pour la ZBDongle-E, zstack pour la ZBDongle-P
```

Au premier démarrage, Zigbee2MQTT tire au sort la clé du réseau Zigbee et l'écrit dans
son fichier. **Sauvegarder** ensuite `configuration.yaml` et `coordinator_backup.json`
(chiffrés, hors de la centrale) : sans eux, changer la carte SD oblige à réappairer
tous les capteurs.

Notre adaptateur surveille lui-même la présence des appareils et décide de
l'appairage : la disponibilité, l'interface web et Home Assistant de Zigbee2MQTT sont
coupés (moins de mémoire, rien d'exposé). Le `base_topic` doit être le même que dans
`/etc/safehome/zigbee.json` (le test le vérifie pour les exemples).

### 9. Démarrer les services

```bash
sudo cp deploy/systemd/*.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now zigbee2mqtt safehome-zigbee safehome-gsm safehome-hub safehome-agent
sudo systemctl enable --now safehome-video        # seulement sur un site avec caméras
sudo systemctl enable --now safehome-maint
sudo cp deploy/system/polkit-safehome-console.rules /etc/polkit-1/rules.d/50-safehome-console.rules
sudo install -m 755 deploy/system/nm-dispatcher-safehome-console /etc/NetworkManager/dispatcher.d/50-safehome-console
sudo systemctl enable --now safehome-console
systemctl --failed                       # doit être vide
journalctl -u safehome-hub -b            # les journaux depuis le démarrage
```

Les services attendent Mosquitto ; s'il n'est pas encore là, nos programmes réessaient
sans fin. Un service qui tombe redémarre après 2 s (5 s pour Zigbee2MQTT), sans jamais
abandonner.

Codes d'accès (voir `operations.md`) : l'outil doit tourner sous l'utilisateur de la
centrale pour lire son état et lui envoyer le signal de relecture :

```bash
sudo -u safehome safehome-hub codes -config /etc/safehome/hub.json set u_owner
```

`sudo systemctl reload safehome-hub` envoie le même signal.

### 10. Appareils

Depuis un téléphone, avec la **console locale** : régler le code installateur
(`operations.md`), le taper au clavier puis « désarmer ». Se connecter ensuite au Wi-Fi
de la centrale (nom et phrase de passe sur l'étiquette) et ouvrir `http://10.42.0.1/`.
Sur la page : appairer, donner les zones et les sirènes, faire taper son code au
propriétaire, tester, puis « Terminer ». La
centrale redémarre seule pour prendre les zones.

Ou en SSH, avec l'outil de l'installateur (`operations.md`) : `pair` pour chaque
appareil, `zone` ou `siren add` pour lui donner son usage, `watch` pour le tester,
`check` pour finir, puis `sudo systemctl restart safehome-hub`.

Les zones et les sirènes sont gardées dans le dossier d'état. Elles se changent donc
aussi après l'étape 12, sans mode maintenance.

### 11. Mémoire

```bash
free -m
systemctl status mosquitto zigbee2mqtt safehome-zigbee safehome-gsm safehome-hub safehome-agent safehome-video safehome-maint safehome-console | grep -E "●|Memory"
```

Ordres de grandeur attendus sur le Pi 4 de 1 Go (**à mesurer**) :

| Quoi | Mémoire |
|---|---|
| Système (Lite, sans bureau) | 80 à 120 Mo |
| Mosquitto | moins de 10 Mo |
| Zigbee2MQTT (Node.js) | 100 à 150 Mo |
| Nos sept programmes (centrale, GSM, Zigbee, agent, vidéo, maintenance, console) | 10 à 20 Mo chacun |
| go2rtc, pendant une session vidéo (2 au plus) | 20 à 50 Mo chacun |
| go2rtc et ffmpeg, quelques secondes pendant les photos d'une alarme | 30 à 60 Mo (à mesurer) |
| **Total** | **300 à 500 Mo sur 1 Go** |

La colonne `available` de `free -m` doit rester au-dessus de 400 Mo.

### 12. Passer le système en lecture seule

En dernier, quand tout marche :

```bash
sudo raspi-config nonint enable_overlayfs
sudo reboot
```

Le système tourne alors sur une couche en mémoire : ce qui y est écrit disparaît au
redémarrage. Vérifier après redémarrage :

```bash
mount | grep -E " / | /data "           # / en overlay, /data en ext4 lecture-écriture
sudo -u safehome touch /var/lib/safehome/essai && sudo reboot
ls /var/lib/safehome/essai              # doit encore exister
```

**À valider** : si `/data` est lui aussi passé en mémoire, ajouter `:recurse=0` à
l'option `overlayroot=tmpfs` dans `/boot/firmware/cmdline.txt`.

## Installer par le script

Le script fait l'installation de Mosquitto (étape 3), les étapes 5, 6 et 7, et les
services de l'étape 9. La mise à jour du système, `fake-hwclock`, SSH, le Bluetooth et
les codes restent à la main. Le paquet est préparé sur l'ordinateur,
avec la configuration du site déjà écrite. Le script se relance tel quel à chaque mise
à jour.

### Sur l'ordinateur

Préparer un dossier avec la configuration du site :

- toujours : `hub.json`, `gsm.json`, `zigbee.json`, `agent.json` et `cloud.secret`
  (partir de `config/*.example.json`) ;
- si le site les utilise : `video.json`, `maint.json`, `maint_known_hosts`,
  `console.json`, `power.json` (module batterie : son `device_id` doit être le
  `hub_device_id` de `hub.json`).

Puis fabriquer le paquet et l'envoyer :

```bash
deploy/bundle.sh -v 0.1.0 -s ~/sites/tg-lom-0001 -g ~/Downloads/go2rtc_linux_arm64 dist/tg-lom-0001
scp -r dist/tg-lom-0001 admin@safehome-tg-lom-0001:safehome
```

`-g` sert seulement pour un site avec caméras. go2rtc est téléchargé et vérifié à la
main, comme à l'étape 7 : le script ne télécharge rien.

Le paquet contient les secrets du site. L'effacer de l'ordinateur une fois la centrale
installée.

### Sur la centrale

Après les étapes 1, 2, 4 et 8 :

```bash
sudo ./safehome/deploy/install.sh
```

Avant de changer quoi que ce soit, le script refuse de tourner si :

- ce n'est pas un Pi 64 bits ;
- Zigbee2MQTT est installé mais `/usr/bin/node` manque ou n'est pas une version 22 ou
  24 de Node.js (étape 8) ;
- le système est en lecture seule ;
- `/data` et ses trois dossiers ne sont pas montés ;
- une configuration obligatoire manque ;
- `video.json` est là sans go2rtc.

Ensuite, dans l'ordre :

1. Il installe Mosquitto, et ffmpeg s'il y a des caméras.
2. Il crée l'utilisateur `safehome` et règle les droits de `/var/lib`.
3. Il pose la configuration du site dans `/etc/safehome`, avec les profils des
   appareils. Il crée `maint.key` s'il n'existe pas encore et que la maintenance est
   configurée.
4. Il vérifie chaque configuration avec les **nouveaux** programmes (`check`). Si l'une
   est mauvaise, il remet les fichiers d'avant et s'arrête. La vérification de
   l'adaptateur Zigbee échoue aussi quand un appareil appairé n'a plus son profil :
   tous ses messages seraient écartés sans un mot. Aucun de nos programmes
   n'a été remplacé ; seul go2rtc a pu l'être, la vérification de la vidéo en a
   besoin. Aucun service n'a redémarré.
5. Il pose les programmes, les services, les journaux, le chien de garde et Mosquitto.
6. Il active et redémarre les services : toujours la centrale, la passerelle GSM,
   l'adaptateur Zigbee et l'agent ; la vidéo, la maintenance et la console seulement
   si leur configuration est là.
7. Il affiche la clé publique du tunnel, à poser sur le bastion, et la phrase de passe
   du Wi-Fi de la console, pour l'étiquette. Il la tire au sort la première fois
   (`console.secret`, 12 caractères sans ceux qui se confondent). Chacune n'existe
   que si la maintenance, ou la console, est configurée.

Ce que le script ne touche pas à la relance :

- une configuration absente du dossier du site reste celle de la centrale. Pas les
  profils des appareils : ils sont remplacés à chaque relance par ceux du paquet et du
  dossier du site (`site/profiles`). Un profil posé à la main sur la centrale
  disparaît donc ; s'il sert à un appareil appairé, la vérification échoue et la
  relance s'arrête sur la configuration d'avant. Le profil propre au site va dans
  `site/profiles` ;
- `maint.key` n'est jamais refaite : le bastion connaît déjà l'ancienne ;
- le fichier de Zigbee2MQTT n'est posé que la première fois. Ensuite il contient la
  clé du réseau Zigbee.

Zigbee2MQTT n'est activé et démarré que lorsque le port de la clé est réglé (plus de
`CHANGE-ME` dans son fichier). Sinon il redémarrerait toutes les 5 s. Le script dit
alors quoi faire : régler le fichier, puis `sudo systemctl enable --now zigbee2mqtt`.

Restent à la main : le reste de l'étape 3 (mise à jour du système, `fake-hwclock`,
SSH, Bluetooth), les codes d'accès, les appareils (étape 10) et le passage en lecture
seule (étape 12).

`go test ./deploy` fait tourner le script sur un faux système : fichiers posés,
relance, mauvaise configuration, refus, caméras, Zigbee2MQTT et sa version de Node.js.

## Maintenance

Le système étant en lecture seule, une mise à jour (sécurité, nouvelle version de nos
programmes) se fait en **mode maintenance** :

```bash
sudo raspi-config nonint disable_overlayfs && sudo reboot
# ... apt full-upgrade, nouveaux programmes dans /usr/local/bin, etc.
sudo raspi-config nonint enable_overlayfs && sudo reboot
```

Conséquence : **pas de mises à jour automatiques** (elles seraient effacées au
redémarrage). La centrale n'est pas joignable depuis internet (derrière la box ou
l'opérateur 4G), ce qui limite le risque ; prévoir une maintenance par mois au début.
Pendant la maintenance l'alarme continue de tourner, sauf pendant les deux
redémarrages (environ une minute chacun).

Changer la configuration (`/etc/safehome`) demande aussi le mode maintenance. Les
codes d'accès, les zones et les sirènes, eux, sont dans l'état (`/var/lib/safehome`) et
se changent à tout moment.

## Recette sur le kit d'essai

| Essai | Attendu |
|---|---|
| `systemctl --failed` après démarrage | vide |
| `sudo kill -9` du processus `safehome-hub` | redémarré en 2 s, même état (armé reste armé) |
| `sudo systemctl stop mosquitto` 1 min, puis `start` | les programmes attendent puis reprennent seuls |
| Débrancher la clé Zigbee | alerte `zigbee_down` après 3 min |
| Débrancher le modem | lien GSM `down` dans `system.health` en une minute environ ; alerte `gsm_degraded` après 10 min |
| Débrancher la box 5 min, déclencher une alarme d'essai, rebrancher | lien `internet` à `down` en moins d'une minute, alertes par GSM sans attendre le cloud ; l'alarme arrive au cloud au retour |
| Bloquer le port 8883 sur la box | l'agent passe en WebSocket sur le port 443 (`transport` à `wss` dans `agent.status`) |
| `sudo systemctl stop safehome-agent` | le cloud voit le site hors ligne tout de suite ; lien `internet` à `silent` après 3 min |
| Session vidéo de test vers un relais d'essai, puis `video.stop` | `live` en quelques secondes, `ended` avec les octets ; plus aucun processus `go2rtc` après l'arrêt (`pgrep go2rtc` vide) |
| `sudo kill -9` du processus `safehome-video` pendant une session | le go2rtc de la session s'arrête aussi |
| Alarme d'essai avec la caméra branchée | `photos of the alarm taken` dans le journal de `safehome-video`, puis `photo sent to the cloud` dans celui de `safehome-agent` ; `/var/lib/safehome/photos` vide ensuite |
| Débrancher la caméra | `reachable` à `false` dans `video.status` en 5 min au plus ; une demande répond `camera_unreachable` |
| Couper le courant brutalement, 10 fois de suite, dont juste après un armement | redémarre à chaque fois, état conservé |
| Couper internet puis le courant, rallumer | la date est juste (module RTC) |
| `echo c \| sudo tee /proc/sysrq-trigger` (blocage du noyau) | la carte redémarre seule (chien de garde) |
| `sudo kill -STOP` du processus `safehome-hub` (programme figé) | tué et redémarré par systemd en 30 s environ (`journalctl -u safehome-hub` : « Watchdog timeout ») ; même état |
| `sudo ss -tlnp` | seulement SSH et Mosquitto sur `127.0.0.1` |
| Maintenance demandée depuis le cloud, puis fermée | `opening`, `open`, `closed` dans `maint/session` ; connexion `ssh -J ... -p 22001 admin@localhost` possible pendant la session seulement ; plus aucun `ssh` après (`pgrep -u safehome ssh` vide) |
| Code installateur puis « désarmer » au clavier, alarme désarmée | `install.mode` ouvert ; le réseau `safehome-tg-lom-0001` apparaît sur un téléphone ; la page `http://10.42.0.1/` s'affiche avec les appareils |
| Sur la page : une zone, un test, « Terminer » | l'événement s'affiche dans Tests ; le réseau disparaît ; la centrale redémarre et la nouvelle zone déclenche l'alarme une fois armée |
| Code installateur alarme armée | refusé comme un mauvais code ; pas de réseau |
| `sudo ss -tlnp` pendant le mode installation | la page seulement sur `10.42.0.1:80`, rien sur l'adresse de la box |
| Débrancher le bloc secteur du module batterie 1 min, puis rebrancher | `mains` à `lost` puis `present` sur l'appareil de la centrale (`system.health`) ; aucune alerte si la batterie reste au-dessus de 40 % |
| Laisser la centrale sur batterie jusqu'au bout | `battery_low_hub` à 40 % puis à 10 % ; vers 3,0 V, « asking the battery module to shut the hub down » dans le journal, arrêt propre, courant coupé ; au retour du secteur, la centrale redémarre seule, dans son état |
| `free -m` avec tout démarré | `available` au-dessus de 400 Mo |

## Limites connues

- Le chien de garde de systemd voit une boucle bloquée (moteur, santé, escalade,
  passerelle…), pas un programme qui tourne mais se trompe. Pour cela, la
  supervision reste : les autres programmes le voient se taire et alertent.
- Zigbee2MQTT et Mosquitto ne sont pas sous le chien de garde de systemd : seulement
  redémarrés s'ils s'arrêtent. Un Zigbee2MQTT figé est vu par l'adaptateur
  (`zigbee_down`).
- L'agrandissement de la racine, l'option `recurse=0` et le module RTC sont **à
  valider** sur le kit d'essai.
- Les messages d'un clavier partent de Zigbee2MQTT en QoS 0 (option d'appareil
  posée par l'adaptateur, `adapter-zigbee.md`, « Clavier : QoS 0 ») : une touche, un
  sabotage ou un appui sur « urgence » du clavier pendant les quelques secondes où
  l'adaptateur Zigbee redémarre est perdu. Les autres appareils restent en QoS 1.
  Codé et vérifié avec le faux Zigbee2MQTT du kit virtuel ; **à valider** sur le kit
  d'essai avec un vrai clavier. Si Zigbee2MQTT refusait l'option (journal de
  `safehome-zigbee` : « Zigbee2MQTT refused a request », `device/options`), le code
  d'un clavier pourrait encore attendre dans `mosquitto.db`, sur la carte SD de la
  centrale elle-même, le temps d'un arrêt de l'adaptateur.
- La console locale n'a pas encore tourné sur un vrai Pi. À vérifier : NetworkManager
  et `dnsmasq-base` (mode « partagé ») présents sur Raspberry Pi OS Lite, la règle
  polkit, le script qui coupe le routage de son réseau, la portée du réseau.
- Le script d'installation (`deploy/install.sh`) n'a pas encore tourné sur un vrai Pi :
  il est testé sur un faux système. Les partitions, le module RTC, l'installation de
  Zigbee2MQTT et la lecture seule restent à la main. L'image maîtresse viendra après
  le pilote.
- Maintenance à distance : codée sur la centrale (tunnel SSH sortant sur demande,
  `maintenance.md`) ; le bastion reste à installer.

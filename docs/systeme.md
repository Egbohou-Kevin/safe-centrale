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
`system/watchdog-safehome.conf` (chien de garde), `system/safehome-boot-clock.service`
(l'heure lue tôt au démarrage, avant NTP, dans `/run/safehome-boot-clock` : le hub
s'en sert au redémarrage, voir `engine.md`), `system/fstab-data` (partition de
données, dont le dossier de l'horloge de `systemd-timesyncd`, voir l'étape 2), `system/polkit-safehome-console.rules` (la console peut créer, lever,
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

Chaque unité `safehome-*` ferme aussi ce dont nos programmes n'ont pas besoin : ni
module ni journal du noyau, ni bit setuid, ni temps réel, ni espace de noms, ni
personnalité, et seulement les sockets Unix, IP et netlink. Aucune n'a de capacité, sauf
la console (`CAP_NET_BIND_SERVICE`, pour sa page sur le port 80). Leur mémoire n'est
jamais à la fois inscriptible et exécutable, sauf pour la vidéo (go2rtc et ffmpeg). La
centrale, l'agent et l'adaptateur Zigbee, qui n'ouvrent aucun appareil, ont un `/dev`
privé. `ProtectClock` et `PrivateDevices` ailleurs ne sont pas mis : ils fermeraient les
ports série, les broches, le bus et la caméra (un test de `deploy` le vérifie).

## Disposition de la carte SD (32 Go)

| Partition | Taille | Contenu | Montée |
|---|---|---|---|
| 1 `bootfs` | celle de l'image | démarrage | lecture seule |
| 2 `rootfs` | 8 Go | le système et nos programmes | lecture seule (couche en mémoire) |
| 3 `safehome-data` | le reste | `/data` : `safehome/`, `mosquitto/`, `zigbee2mqtt/`, `timesync/` | lecture-écriture |

Les quatre dossiers de `/data` sont « attachés » (bind) à leur place habituelle :
`/var/lib/safehome`, `/var/lib/mosquitto`, `/var/lib/zigbee2mqtt`, et
`/var/lib/systemd/timesync` (l'heure de la dernière synchronisation, étape 2). Les programmes ne
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
   laisser la place à la partition de données. Une fois la carte écrite et **avant** de la
   mettre dans le Pi, sur la partition `bootfs` que l'ordinateur sait lire (méthode validée le
   25 octobre 2026, Raspberry Pi OS Lite Debian 13) :
   - dans `cmdline.txt`, retirer le mot `resize` (l'image l'utilise pour agrandir au démarrage) ;
   - dans `user-data` (cloud-init), ajouter `growpart: {mode: "off"}` et `resize_rootfs: false`.

   Sans cela, la racine prend toute la carte (constaté), et on ne peut pas la réduire en marche :
   réécrire la carte et recommencer.
4. **Wi-Fi** : choisir le réseau en WPA2. Le Pi 4 ne se connecte pas à un réseau WPA3 seul
   (constaté avec le réseau « …-Plus » d'une Bbox, qui est en WPA3 : le Pi ne s'y connecte pas ;
   le réseau du même nom sans « -Plus » est en WPA2 et marche). Un câble de secours évite de
   perdre l'accès pendant la mise au point.
5. Pour la durée de l'installation, `user-data` peut donner à l'utilisateur d'administration
   `sudo: "ALL=(ALL) NOPASSWD:ALL"`, qui permet d'enchaîner les étapes à distance. **Le retirer
   à la fin** (`/etc/sudoers.d/90-cloud-init-users`), avant la lecture seule.

Plus tard, on préparera une **image maîtresse** une fois pour toutes (carte déjà
partitionnée, tout installé) et chaque nouvelle centrale sera une simple copie.

### 2. Partitions

```bash
# agrandir la racine à 8 Go (possible pendant qu'elle tourne), puis créer les données.
# parted demande confirmation parce que la racine est montée : « Yes » (validé le 25/10/2026)
printf 'Yes\n' | sudo parted ---pretend-input-tty /dev/mmcblk0 resizepart 2 8GiB
sudo partprobe /dev/mmcblk0
sudo resize2fs /dev/mmcblk0p2
sudo parted /dev/mmcblk0 mkpart primary ext4 8GiB 100%
sudo mkfs.ext4 -L safehome-data /dev/mmcblk0p3

sudo mkdir -p /data /var/lib/safehome /var/lib/mosquitto /var/lib/zigbee2mqtt
# ajouter à /etc/fstab les lignes de deploy/system/fstab-data, puis :
sudo mount /data
sudo mkdir -p /data/safehome /data/mosquitto /data/zigbee2mqtt /data/timesync
sudo cp -p /var/lib/systemd/timesync/clock /data/timesync/ 2>/dev/null   # sa date, s'il existe
sudo chown -R systemd-timesync:systemd-timesync /data/timesync
sudo mount -a
```

`commit=5` dans `fstab` : les données sont écrites sur la carte au plus tard 5 s
après le changement (au lieu de 30). L'état de l'alarme survit ainsi à une coupure qui
suit de près un armement.

**L'heure au démarrage** (28 octobre 2026). Sans horloge à pile, `systemd-timesyncd` remet
l'horloge, tôt au démarrage et avant NTP, à la date de son fichier
`/var/lib/systemd/timesync/clock`, qu'il touche à chaque synchronisation (puis toutes les
60 s). En lecture seule, ce fichier restait celui de la couche basse : la centrale repartait
chaque fois du jour du passage en lecture seule (le 28/10 à 13:52, 4 h 13 min de retard
jusqu'à ce que NTP réponde, 37 s après le démarrage). Son dossier est donc attaché à
`/data/timesync` : l'horloge repart de la dernière synchronisation, le retard n'est plus que
la durée de l'arrêt (plus une minute au plus).

- **Ordre** : rien à ajouter à l'unité. `systemd-timesyncd` a
  `StateDirectory=systemd/timesync` ; d'après `systemd.exec(5)` (dépendances implicites),
  une unité qui a `StateDirectory=` reçoit d'elle-même `Requires=` et `After=` sur tous les
  montages qu'il faut pour atteindre ce dossier, comme avec `RequiresMountsFor=`. Elle démarre
  donc après ce bind, lui-même après `/data` (un bind suit le montage de sa source). Aucun
  cycle : ces montages ne viennent qu'après `local-fs-pre.target` et la carte, jamais après
  `sysinit.target`, avant laquelle `systemd-timesyncd` démarre ; `safehome-boot-clock` reste
  après lui et avant `sysinit.target`, donc avant le réseau.
- **`nofail`** : un bind qui échoue n'arrête pas le démarrage (`local-fs.target` ne l'exige
  pas). Mais ce `Requires=` implicite fait qu'un bind qui échoue (dossier `/data/timesync`
  absent, erreur de montage) empêche `systemd-timesyncd` de démarrer : plus de NTP du tout
  pendant la marche, visible dans `systemctl --failed`. La sûreté tient donc au dossier créé
  avant la ligne de `fstab`, ici comme par le script d'installation et le kit virtuel (un
  test le vérifie pour chaque source de bind). La ligne de `/data` elle-même n'a pas
  `nofail` (déjà avant ce changement) : sans la partition, le démarrage finit en mode de
  secours.
- **Propriétaire** : `systemd-timesync`, l'utilisateur système de l'unité (`User=`, pas
  `DynamicUser=` : le fichier est bien dans `/var/lib/systemd/timesync`, pas sous
  `/var/lib/private`). systemd le remet de lui-même à chaque démarrage du service
  (`StateDirectory=`).
- **Mise à jour** : le script d'installation pose le dossier et la ligne s'ils
  manquent ; une centrale installée avant le 28/10 les reçoit à sa mise à jour
  (`deploy/update-hub.sh`, phase 2), sans geste à la main.

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

Méthode validée sur le Pi le 25 octobre 2026, plus courte que le guide : le paquet npm, avec
la même version que l'essai, dans un dossier à part, et le lien que notre service attend.

```bash
sudo mkdir -p /opt/z2m-install && cd /opt/z2m-install
echo '{"private":true}' | sudo tee package.json >/dev/null
sudo npm install --no-audit --no-fund zigbee2mqtt@2.14.2
sudo ln -sfn /opt/z2m-install/node_modules/zigbee2mqtt /opt/zigbee2mqtt
```

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
sudo cp deploy/systemd/*.service deploy/system/safehome-boot-clock.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable safehome-boot-clock   # prise au prochain démarrage, avant NTP
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

**Validé sur le Pi le 25 octobre 2026 : `recurse=0` est obligatoire.** Sans lui, `/data` et les trois
dossiers attachés passent eux aussi dans l'overlay en mémoire : les services démarrent avec un état
vide (constaté : Zigbee2MQTT ne pouvait plus écrire, la centrale repartait sans ses codes), alors
que la vraie partition reste intacte. Dans `/boot/firmware/cmdline.txt`, après
`enable_overlayfs` et avant de redémarrer, écrire `overlayroot=tmpfs:recurse=0` (la partition de
démarrage reste inscriptible, donc on peut corriger à distance).

Avec `recurse=0`, vérifié : `/` en overlay, `/data` et les trois dossiers en ext4
lecture-écriture sur la partition 3, un fichier écrit dans `/data` survit au redémarrage, un
fichier écrit dans `/etc` disparaît. Le quatrième dossier (`/var/lib/systemd/timesync`, 28 octobre
2026) suit la même règle : pas encore vérifié sur le Pi.

`systemd-remount-fs` échoue alors (il remonte `/` avec les options de `fstab`, qu'un overlay
refuse) et `systemctl --failed` n'est plus vide. Le masquer **avant** d'activer l'overlay :
`sudo systemctl mask systemd-remount-fs.service`. Après coup, sans quitter la lecture seule :

```bash
sudo mount -o remount,rw /media/root-ro
sudo ln -sf /dev/null /media/root-ro/etc/systemd/system/systemd-remount-fs.service
sudo mount -o remount,ro /media/root-ro
```

Même méthode pour un petit changement de configuration (un réglage de `/etc/safehome`) sans le
mode maintenance complet : remonter `/media/root-ro` en écriture, modifier le fichier dans
`/media/root-ro/etc/...`, le remonter en lecture seule, redémarrer.

## Installer par le script

Le script fait l'installation de Mosquitto (étape 3), les étapes 5, 6 et 7, et les
services de l'étape 9. La mise à jour du système, `fake-hwclock`, SSH, le Bluetooth et
les codes restent à la main. Le paquet est préparé sur l'ordinateur,
avec la configuration du site déjà écrite. Le script se relance tel quel à chaque mise
à jour.

### Sur l'ordinateur

Préparer un dossier avec la configuration du site :

- toujours : `hub.json`, `gsm.json`, `zigbee.json`, `agent.json` et `cloud.secret`
  (partir de `config/*.example.json`) ; sans `gsm.json` pour une centrale sans ligne SMS
  par choix, qui le dit dans `hub.json` (`"without_gsm": true`, voir `operations.md`) ;
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
- `/data` et ses trois dossiers ne sont pas montés (pas le quatrième,
  `/var/lib/systemd/timesync` : le script pose lui-même sa ligne, prise au prochain démarrage) ;
- une configuration obligatoire manque ;
- `video.json` est là sans go2rtc.

Ensuite, dans l'ordre :

1. Il installe Mosquitto, et ffmpeg s'il y a des caméras.
2. Il crée l'utilisateur `safehome` et règle les droits de `/var/lib`. Il garde le fichier
   d'horloge de `systemd-timesyncd` sur `/data` (étape 2, 28 octobre 2026) : le dossier
   `/data/timesync`, le fichier tel qu'il est (avec sa date, pas l'heure du moment, qui peut
   être fausse à la première installation), puis la ligne de `fstab` si elle manque ; le
   tout est pris au prochain démarrage.
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
  clé du réseau Zigbee ;
- la ligne de `/etc/fstab` pour `/var/lib/systemd/timesync`, une fois là (même écrite autrement),
  et le fichier d'horloge de `/data/timesync`, une fois copié (28 octobre 2026).

Zigbee2MQTT n'est activé et démarré que lorsque le port de la clé est réglé (plus de
`CHANGE-ME` dans son fichier). Sinon il redémarrerait toutes les 5 s. Le script dit
alors quoi faire : régler le fichier, puis `sudo systemctl enable --now zigbee2mqtt`.

Restent à la main : le reste de l'étape 3 (mise à jour du système, `fake-hwclock`,
SSH, Bluetooth), les codes d'accès, les appareils (étape 10) et le passage en lecture
seule (étape 12).

`go test ./deploy` fait tourner le script sur un faux système : fichiers posés,
relance, mauvaise configuration, refus, caméras, Zigbee2MQTT et sa version de Node.js,
le fichier d'horloge gardé sur `/data` (une seule fois, même relancé), la version précédente de
chaque programme changé (`.prev`) ; il fait aussi tourner la phase 2 de `deploy/update-hub.sh`
contre une fausse centrale, qui remet les `.prev` quand les nouveaux programmes ne tiennent pas,
et sa phase 4, qui s'arrête au premier défaut (`/` pas en overlay, dossier de timesyncd hors de
`/data`, timesyncd arrêté, services qui ne tiennent pas, mode de l'alarme changé) en le disant.

## Maintenance

Le système étant en lecture seule, une mise à jour (sécurité, nouvelle version de nos
programmes) se fait en **mode maintenance**. Procédure validée sur le Pi les 25 et 26 octobre 2026,
en deux redémarrages :

`deploy/update-hub.sh` la fait depuis l'ordinateur, en quatre phases (1 à 4 ci-dessous : sortir de la
lecture seule, installer, y revenir, vérifier), avec un paquet fait **sans** `-s`. Mise à jour du
28 octobre 2026 :

```bash
deploy/bundle.sh -v 0.2.1-601f598 dist/maj
deploy/update-hub.sh 1
deploy/update-hub.sh -v 0.2.1-601f598 2 dist/maj
deploy/update-hub.sh 3
deploy/update-hub.sh 4
```

La phase 4 refuse aussi une centrale dont l'heure au démarrage repartirait du jour du passage en
lecture seule : le dossier de timesyncd (`/var/lib/systemd/timesync`) doit être monté depuis `/data`
(ext4) et `systemd-timesyncd` actif. La ligne `/data` du fstab n'a pas `nofail` : une carte dont
`/data` ne monte pas s'arrête au démarrage (choix laissé à Kevin, 28 octobre 2026).

**Retour arrière des programmes** (28 octobre 2026). `install.sh` garde chaque programme qu'il
remplace (`safehome-*` de `/usr/local/bin`) en `safehome-<programme>.prev`, à côté, quand l'ancien
diffère du nouveau ; un programme identique ou neuf n'en laisse aucun, pas même un `.prev` d'une
mise à jour précédente (il ramènerait une version plus vieille que celle qui tourne) : ces anciens
`.prev` sont effacés tous ensemble avant la marque, si bien qu'une installation coupée n'en laisse
aucun pour un programme qu'elle n'avait pas encore atteint. Un `.prev` est entier ou absent : copié
d'abord en `.prev.new`, puis renommé ; un `.prev.new` laissé par une coupure n'est jamais remis, et
l'installation suivante l'efface (revue du 28 octobre 2026). Avant de remplacer le premier programme,
il pose la marque `/usr/local/bin/.safehome-prev.unconfirmed` (retirée s'il ne garde aucun `.prev`),
qui contient un jeton tiré au hasard pour cette exécution : tant qu'elle est là, les programmes en
place ne sont pas confirmés,
et une nouvelle installation (phase 2 relancée après une coupure, paquet refait entre-temps) garde
les `.prev` existants, qui sont les programmes qui tenaient ; elle n'ajoute que ceux des programmes
changés depuis. La phase 2 lit d'abord la version (sans attendre) : si elle n'est pas celle du
paquet, ou si le programme ne sait même pas la dire, le retour joue tout de suite ; sinon elle
attend deux minutes, et si une unité n'est pas `active` (un programme qui plante au démarrage n'est
jamais `failed` : `Restart=always` sans limite le laisse `activating`), si `safehome-hub.service`
ne tourne pas du tout (une unité arrêtée n'est pas listée), ou si une unité n'est pas active
depuis au moins 100 s, le retour joue aussi. Ce dernier point attrape un programme qui plante
juste après s'être dit prêt (`RestartSec=2`) : il paraît `active` la plupart du temps, mais son
dernier démarrage (`ActiveEnterTimestampMonotonic`, comparé à `/proc/uptime`) est récent. Le seuil
est de 100 s après `install.sh` et après le retour : chaque `systemctl restart` y attend que l'unité
se dise prête (`Type=notify`), puis viennent les deux minutes d'attente, donc une unité qui tient a
au moins deux minutes. Après un redémarrage de la centrale (phase 4), la durée du démarrage n'est
pas mesurée : le seuil y est de 60 s, la première minute lui reste. `NRestarts` n'a pas été retenu : les unités
qu'`install.sh` ne redémarre pas gardent leur compte depuis le démarrage de la centrale, et sa
remise à zéro par un `systemctl restart` n'a pas pu être vérifiée sans toucher la centrale (revue
du 28 octobre 2026). La phase 4 attend les services de la même façon, avec le seuil de 60 s. Le
retour remet chaque `.prev` à sa place (`mv`, d'un coup ; un seul `mv` raté arrête tout en nommant
ceux déjà revenus), `systemctl daemon-reload`, redémarre les services activés parmi eux (un
redémarrage raté est dit, la suite continue), attend encore deux minutes, puis s'arrête en disant
que l'ancienne version est revenue, et si ses services tiennent. Quand les nouveaux programmes
tiennent, la phase 2 retire la marque : les `.prev` restent, mais la mise à jour suivante repart
d'eux comme d'une installation ordinaire. Rien n'est désarmé ; l'état de l'alarme
(`/var/lib/safehome`) n'est pas touché. Ni la configuration (déjà vérifiée par les nouveaux
programmes), ni les unités ne reviennent : les unités d'avant sont dans
`/data/maintenance/units-<date>.tgz`, à remettre à la main si besoin (étape 5). **L'ancien
programme repart sur l'état et la configuration que le nouveau a déjà utilisés** (`engine.json`,
fichiers de `/etc/safehome`) : un changement de leur format entre deux versions doit rester lisible
par la précédente, sinon le retour ne suffit pas. Quand `install.sh` échoue à la vérification de la
configuration, rien n'a été remplacé : sa configuration revient, rien n'est redémarré, la phase 2
s'arrête sans retour. Quand il échoue plus loin (un redémarrage de service), les programmes sont
déjà les nouveaux et la marque est posée : la phase 2 fait alors le même retour. Elle les distingue
par la marque lue avant `install.sh` : seule une marque nouvelle ou changée (le jeton de cette
exécution) fait jouer le retour ; une marque laissée par une phase 2 coupée, inchangée, ne le fait
pas, et la phase 2 s'arrête en disant que les programmes en place restent ceux d'une exécution non
confirmée.
Les `.prev` s'écrivent en maintenance (`/` en ext4 inscriptible) et restent sous l'overlay après le
retour en lecture seule : c'est voulu, l'ancienne version reste à portée de main. Place prise : au
plus un `.prev` par programme, 9 ; les 9 programmes du paquet `dist/maj-8b00128` font 66 788 Kio
(`du -sk`, environ 65 Mio), le plafond quand tous changent.

1. **Avant tout**, garder une copie de `/boot/firmware/cmdline.txt` sur l'ordinateur, et noter
   l'état de l'alarme (armée ou non). Ne rien désarmer.
2. **Sortir de la lecture seule.** `raspi-config nonint disable_overlayfs` ne reconnaît pas
   `overlayroot=tmpfs:recurse=0` et ne change rien : retirer ce mot de `cmdline.txt` à la main
   (la partition de démarrage reste inscriptible), puis redémarrer :

   ```bash
   sudo sed -i 's/overlayroot=tmpfs[^ ]* //' /boot/firmware/cmdline.txt
   grep -c overlayroot /boot/firmware/cmdline.txt   # 0
   sudo reboot
   ```

3. **Après le redémarrage**, `findmnt -no FSTYPE /` dit `ext4`, mais `/` reste en lecture seule :
   `systemd-remount-fs` est masqué (étape 12). Le remonter en écriture :
   `sudo mount -o remount,rw /`.
4. **Vérifier l'heure** (`timedatectl`) : le 26 octobre, le Pi n'avait ni DNS ni heure juste dans ce
   mode, et `apt` ou TLS échouent avec une heure fausse. Tout est revenu après le second
   redémarrage. Sans heure juste, ne lancer ni `apt` ni l'installation.
   Cause trouvée le 28 octobre 2026 : `/` étant encore en lecture seule au démarrage, NetworkManager
   n'écrit pas `/etc/resolv.conf` (il reste sans `nameserver`) ; il a pourtant le DNS de la box
   dans `/run/NetworkManager/resolv.conf`. Après le remontage en écriture :

   ```bash
   sudo cp /run/NetworkManager/resolv.conf /etc/resolv.conf
   sudo systemctl restart systemd-timesyncd   # synchronisée en quelques secondes
   ```

   Ce fichier reste ensuite sur le disque : la maintenance suivante n'en a plus besoin.
   **Un changement de `/etc/safehome` fait hors maintenance est perdu au redémarrage** (il n'est
   que dans la mémoire de l'overlay) : le 28 octobre, la bascule de l'agent vers le cloud du
   serveur (`agent.json`, faite à 02:22 en lecture seule) a disparu au premier redémarrage et
   l'agent est revenu au cloud du Mac ; refaite en maintenance, elle tient.
5. **Copier le paquet après ce redémarrage** (pas avant : en lecture seule, il disparaîtrait), le
   faire **sans** `-s` quand la configuration du site est déjà sur la centrale (aucun secret dans le
   paquet), puis `sudo ./safehome/deploy/install.sh`. Garder d'abord une copie des unités systemd
   en place : si une unité nouvelle empêche un service de démarrer, remettre la sienne seule,
   `systemctl daemon-reload`, puis redémarrer ce service. Si ce sont les nouveaux programmes qui
   ne tiennent pas, remettre les `.prev` gardés par `install.sh`, puis redémarrer les services
   activés parmi eux (pas d'autre, ni `safehome-boot-clock`) :

   ```bash
   cd /usr/local/bin
   back=$(for f in safehome-*.prev; do [ -e "$f" ] && sudo mv -f "$f" "${f%.prev}" && echo "${f%.prev}"; done)
   sudo rm -f .safehome-prev.unconfirmed
   sudo systemctl daemon-reload
   for p in $back; do systemctl is-enabled -q "$p" && sudo systemctl restart "$p"; done
   ```
6. **Vérifier** : chaque service `active` pendant deux minutes (et depuis au moins 100 s :
   `systemctl show -p ActiveEnterTimestamp <unité>`), `safehome-hub version`,
   `systemctl --failed` vide. Puis confirmer les nouveaux programmes :
   `sudo rm -f /usr/local/bin/.safehome-prev.unconfirmed` (la phase 2 le fait elle-même).
7. **Effacer le paquet et tout dossier `site/`** copié sur la centrale (`rm -rf ~/safehome`), puis
   `ls ~` : rien ne doit rester. Le 26 octobre, un dossier `site/` avec des secrets restait d'un
   essai précédent.
8. **Revenir en lecture seule** : `sudo raspi-config nonint enable_overlayfs`, puis **avant de
   redémarrer** remettre `overlayroot=tmpfs:recurse=0` dans `cmdline.txt` (étape 12), comparer avec
   la copie (`cmp`), vérifier que `systemd-remount-fs` est toujours masqué
   (`systemctl is-enabled systemd-remount-fs` dit `masked`), et seulement alors `sudo reboot`.
9. **Après le second redémarrage** : `/` en overlay, `/data` en ext4, les services actifs, l'alarme
   dans le même état qu'avant.

Depuis le 28 octobre 2026, la phase 2 (`install.sh`) garde aussi le fichier d'horloge de
`systemd-timesyncd` sur `/data` (étape 2), sans geste à la main ; il est pris au redémarrage de la
phase 3. Ce premier démarrage repart de l'heure de l'installation (quelques minutes de retard), les
suivants de la dernière synchronisation. À vérifier sur le Pi après la première mise à jour qui le
pose : `findmnt /var/lib/systemd/timesync` (la partition 3),
`systemctl show systemd-timesyncd -p After -p Requires` (le montage y est, par
`StateDirectory=`), `systemctl is-active systemd-timesyncd` (`active`), `systemctl --failed`
vide, et l'heure de `/run/safehome-boot-clock` proche de l'heure réelle après un redémarrage.

Conséquence : **pas de mises à jour automatiques** (elles seraient effacées au
redémarrage). La centrale n'est pas joignable depuis internet (derrière la box ou
l'opérateur 4G), ce qui limite le risque ; prévoir une maintenance par mois au début.
Pendant la maintenance l'alarme continue de tourner, sauf pendant les deux
redémarrages (environ une minute chacun).

**Essai du 26 octobre 2026** (même Pi, version de `main` du jour, site configuré sans sirène et sans
ligne SMS, `without_gsm`) : la procédure ci-dessus a demandé un troisième redémarrage, faute de
l'étape 2 écrite ainsi ; le durcissement des unités (`865e2fc`) n'a fait tomber aucun service ;
`safehome-hub check` accepte un site sans sirène ; `safehome-gsm` reste désactivé ; 668 Mo de
mémoire disponibles. Constaté ensuite : juste après le redémarrage, un capteur pas encore entendu
est donné hors ligne jusqu'à son premier message (en cours de correction).

Changer la configuration (`/etc/safehome`) demande aussi le mode maintenance. Les
codes d'accès, les zones et les sirènes, eux, sont dans l'état (`/var/lib/safehome`) et
se changent à tout moment.

## Essai sur le vrai Pi (25 octobre 2026)

Premier Raspberry Pi 4 (1 Go) réel, avec la clé Sonoff ZBDongle-E branchée en direct (sans
rallonge) et la porte Aqara. Il a pris la place de la centrale du banc sur le Mac, pour le site
d'essai `tg-lom-0001`. Ce qui a marché tel quel : les étapes 1 à 9 (voir les corrections
ci-dessus), l'installation par script du premier coup, 6 services actifs, 635 Mo de mémoire
disponibles, la porte appairée sur le Mac reprise sans réappairage, l'intrusion vue par l'agent,
le cloud et l'application du téléphone.

Pas de modem ni de module batterie à ce stade : `safehome-gsm` journalise l'absence du port
(attendu), l'alerte `gsm_degraded` part à l'échéance, et l'application affiche « SMS à voir » et
« Courant — ». Aucune sirène n'est appairée : `dev_siren1` n'existe pas dans Zigbee2MQTT, donc
`unknown_device` à chaque ordre et l'application dit « La sirène ne répond pas ».

**Reprendre la place d'une centrale du banc.**

1. Arrêter la centrale du banc, ses superviseurs d'abord, par leur PID. Deux centrales du même
   identifiant de site se délogent l'une l'autre sur le broker du cloud, sans fin.
2. Copier son dossier d'état dans `/var/lib/safehome` du Pi (codes, appareils, zones, numérotation
   de l'agent) : `tar` du dossier, sans les `*.pid`, après avoir arrêté les programmes du Pi et
   vidé son état neuf. Le cloud rejoue alors les messages en attente (« gave number … again » au
   journal du cloud : c'est le dédoublonnage, pas une erreur).
3. Copier `configuration.yaml`, `coordinator_backup.json` et `database.db` de Zigbee2MQTT dans
   `/var/lib/zigbee2mqtt`, en changeant le broker (`mqtt://127.0.0.1:1883`) et le port de la clé
   (`/dev/serial/by-id/…`). Les capteurs ne sont pas réappairés.
4. **`install.sh` remet `hub.json` du paquet** à chaque relance : un réglage fait à la main dans
   `/etc/safehome` (les délais d'entrée et de sortie, par exemple) disparaît. Le changer dans le
   dossier du site, puis refaire le paquet.

**Le cloud de l'essai** est, depuis le 28 octobre 2026, le cloud en production sur le serveur de
Kevin, `safecloud.mewini.com`, derrière le Tunnel Cloudflare (`safeHome-cloud/deploy/README.md`) :
dans `/etc/safehome/agent.json`, `cloud.urls` vaut `["wss://safecloud.mewini.com:443/mqtt"]` et
`cloud.ca_file` est vide (les autorités du système : le TLS s'arrête chez Cloudflare, et 8883 ne
passe pas par le Tunnel). Firebase y est allumé ; sans lui, chaque alerte pour l'application
échouerait avec `no_app`. Avant, le cloud tournait sur le Mac et la centrale le joignait par un
tunnel SSH inverse (`safeHome-cloud/README.md`, « Une vraie centrale sur ce cloud ») ; l'ancienne
configuration reste dans `/etc/safehome/agent.json.mac`.

**Lecture seule et recette partielle faites le 25 octobre 2026** (étape 12 ci-dessus, avec
`recurse=0`) : `kill -9` de la centrale, relancée en 3 s ; centrale gelée (`kill -STOP`), relancée par le
chien de garde 25 s après le gel ; Mosquitto arrêté 60 s, tous les programmes reprennent seuls ;
fichier de `/data` conservé après redémarrage, fichier de `/etc` perdu ; `systemctl --failed`
vide ; horloge synchronisée au retour. Quatre coupures brutales du courant (câble débranché, dont une par accident),
dont une trente secondes après un armement par l'application : à chaque retour, les cinq fichiers
d'état (codes, appareils, configuration du cloud, réseau Zigbee) ont la même empreinte qu'avant, 0 erreur
ext4, 6 services actifs, rien en échec, et la centrale est revenue armée, avec son heure
d'armement d'origine ; juste après le démarrage, l'heure repart de la dernière valeur connue
jusqu'à la synchronisation par internet (quelques secondes). **Décision de Kevin, 25 octobre
2026 : on s'arrête à quatre coupures**, au lieu des dix de la recette (une panne rare de la carte se
mesure sur des mois d'usage, pas sur dix essais). La clé Zigbee débranchée : alerte `zigbee_down`
au téléphone en 3 minutes, reprise seule au rebranchement. **Pas encore faits** : le modem, le
module batterie, les essais de maintenance et de console locale (celle-ci demande le câble
Ethernet, car `wlan0` est l'unique liaison du Pi, et un clavier). Le `sudo` sans mot de passe de
l'utilisateur d'administration (étape 1) est resté en place pour la période d'essai : à retirer avant
une vraie installation chez un client.

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
| Couper le courant brutalement, 10 fois de suite, dont juste après un armement | redémarre à chaque fois, état conservé (fait 4 fois sur le vrai Pi le 25/10/2026, voir plus haut) |
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
- L'option `recurse=0` et le module RTC sont **à valider** sur le kit d'essai.
  L'agrandissement de la racine l'a été le 25 octobre 2026 (étape 1 et 2).
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

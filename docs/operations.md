# Exploitation de la centrale

Ce document dit comment lancer et entretenir les programmes `safehome-hub`,
`safehome-gsm` (la passerelle GSM), `safehome-zigbee` (l'adaptateur Zigbee),
`safehome-agent` (l'agent de liaison avec le cloud), `safehome-video` (la vidéo en
direct), `safehome-maint` (la maintenance à distance) et `safehome-console` (la console
locale), qui tournent sur le Raspberry Pi, et l'outil de l'installateur
`safehome-install`. Les programmes autres que la centrale, et l'outil, ont leur section
à la fin. Le fonctionnement interne est
dans `architecture.md`, l'installation du système dans `systeme.md`.

## Compiler

```bash
go build -ldflags "-X main.version=0.1.0" -o safehome-hub ./cmd/safehome-hub
# pour le Raspberry Pi (64 bits) depuis un autre ordinateur :
GOOS=linux GOARCH=arm64 go build -o safehome-hub ./cmd/safehome-hub
```

Le résultat est un seul fichier, sans dépendance à installer sur la centrale (sauf le
broker Mosquitto, qui tourne à côté).

## Commandes

| Commande | Effet |
|---|---|
| `safehome-hub check -config FICHIER` | Vérifie la configuration et s'arrête |
| `safehome-hub run -config FICHIER` | Lance la centrale |
| `safehome-hub codes -config FICHIER list` | Liste les personnes qui ont un code |
| `safehome-hub codes -config FICHIER set UTILISATEUR` | Pose ou change le code (lu sur l'entrée standard) |
| `safehome-hub codes -config FICHIER remove UTILISATEUR` | Retire le code |
| `safehome-hub version` | Affiche la version |

Les codes ne s'affichent jamais, ni en succès ni en erreur. Exemple :
`read -s CODE; echo "$CODE" | safehome-hub codes -config hub.json set u_owner`.

La centrale ignore le code d'une personne qui n'est plus contact du site (journalisé
sans le code). `codes set` refuse un utilisateur qui n'est pas un contact (ni
`installer`) ; `codes list` signale ces codes restants, à retirer avec `codes remove`
ou depuis la console (« Codes d'anciens contacts »). Les fichiers d'état écrits par un
outil lancé avec `sudo` prennent le propriétaire du dossier, le verrou des codes
(`codes.lock`) aussi ; sinon l'outil échoue en le disant (il faut alors le lancer avec
`sudo -u safehome`).

L'utilisateur `installer` porte le **code installateur**. Tapé au clavier puis
« désarmer », alarme désarmée, il ouvre le mode installation pour 30 minutes, et donc la
console locale (`security-install.md`). Ce code est à part de ceux des personnes : il
ne prend pas une de leurs cinq places, ne désarme jamais, et ne peut pas être égal au
code d'une personne.

```bash
read -s CODE; echo "$CODE" | sudo -u safehome safehome-hub codes -config /etc/safehome/hub.json set installer
```

Les **zones et les sirènes** réglées par l'installateur (console locale ou outil en
ligne de commande) sont gardées dans le dossier d'état, `site-devices.json`. Elles
remplacent alors celles de `hub.json`, qui ne servent plus qu'au départ. `check` dit
d'où elles viennent. Un fichier abîmé, ou que la centrale refuse (modifié à la main,
règle rendue plus stricte par une mise à jour), est laissé de côté : la centrale
démarre quand même, avec les zones de `hub.json`, et l'écrit dans son journal. Ce
n'est qu'un secours : `check` échoue alors (le script d'installation s'arrête avant de
redémarrer quoi que ce soit), et la console comme `safehome-install devices` et
`check` refusent de montrer ou de vérifier ces zones que la centrale ne surveille pas,
avec la raison. Corriger le fichier (ou le retirer pour repartir de `hub.json`). Rien
ne part au cloud : seul le journal de la centrale en garde la trace.

## Configuration

Un fichier JSON, strict : une clé inconnue est une erreur (une faute de frappe sur un
délai de sécurité ne doit pas passer sans bruit). Un exemple complet est dans
`config/hub.example.json`. Les durées s'écrivent `"2m"`, `"45s"`, les heures `"20:00"`.
`check` dit tout de suite ce qui ne va pas.

Les **contacts** (`contacts`) sont les personnes à prévenir : au plus 5, chacun avec un
`id`, un `name`, un `role` (`owner`, `deputy`, `guard`, `manager`, `other`, au moins un
`owner`) et les `channels` qu'il accepte (`app`, `whatsapp`, `sms`, `call`). Le numéro de
téléphone n'est pas ici : il est connu des canaux. Sans contact, personne n'est prévenu
(`check` le signale). `site_name` est le nom du site dans les messages (sans accent).
Le bloc `escalation` règle `internet_confirm` (10 s), `gsm_delay` (1 min),
`max_per_hour` (20) et les **plans** (`plans`) : qui est prévenu, quand et comment,
pour chaque type d'alerte. L'exemple les écrit tous, avec les valeurs par défaut de
`escalation.md` (un test vérifie qu'ils restent identiques au code).

```json
"intrusion": {
  "waves": [
    { "after": "0s", "to": ["first", "guard"],          "double": true },
    { "after": "2m", "to": ["owner", "guard", "first"], "double": true },
    { "after": "5m", "to": ["rest"],                    "double": true }
  ],
  "repeat": { "every": "10m", "times": 3 }
}
```

- Les types d'alerte portent les noms du contrat : `intrusion`, `fire`, `panic`,
  `tamper`, `auth_failed`, `arming_bypassed`, `battery_low_hub`, `mains_restored`,
  `device_offline`, `battery_low`, `gsm_degraded`, `zigbee_down`, `siren_failed`.
- Un plan écrit **remplace entièrement** le plan par défaut de ce type ; un type absent
  garde le sien.
- Une vague (`waves`) : `after` (depuis l'alerte, 0 par défaut, jamais avant la vague
  précédente), `to` (`first` : le responsable, sinon le propriétaire ; `owner`,
  `guard`, `rest` : gérants et autres ; `everyone`), `sms_now` (le SMS part avec le
  message internet, sans attendre), `double` (SMS et appel après `gsm_delay` sans
  réponse).
- `repeat` : les relances après la dernière vague (`every`, `times`).
- `internet_only` : jamais par GSM ; `gsm_after` : sauf après ce délai sans internet.
- Le plan de `siren_failed` ne vaut que tant que l'alarme n'est pas prise en charge.
  Ensuite, l'alerte est discrète quel que soit le plan : une seule étape, jamais
  d'appel (voir `escalation.md`, « Sirène muette pendant une alarme »).

Une erreur dit où elle est (`escalation.plans.fire.waves[2].after`).

Le bloc `health` règle aussi la surveillance du secours GSM : `gsm_silent_after` (3 min,
passerelle considérée muette sans nouvel état) et `gsm_degraded_after` (10 min avant de
prévenir le propriétaire). `gsm_silent_after` doit rester bien au-dessus du
`status_every` de la passerelle (1 min) : au moins trois fois. De même pour
l'adaptateur Zigbee : `zigbee_silent_after` (3 min) et `zigbee_down_after` (3 min avant
l'alerte « capteurs injoignables »).

`without_gsm` (faux par défaut) : à `true`, la centrale n'a **aucune ligne SMS, par
choix** (25 octobre 2026). Elle ne demande jamais de SMS ni d'appel, ne surveille aucune
passerelle (pas de `gsm_degraded`) et dit `none` pour le lien `gsm` de `system.health` :
l'application montre « Sans SMS » en gris au lieu d'une panne. Le site n'a alors pas de
`gsm.json`, et `install.sh` refuse les deux ensemble. `safehome-hub check` le dit, et
nomme les contacts qui n'acceptent que SMS et appel : personne ne les prévient. Sans la
clé, la ligne SMS est attendue, et une passerelle muette est une panne (voir
`escalation.md`, « Une centrale sans ligne SMS »).

À noter : `timezone` (Lomé : `Africa/Lome`, UTC), `zones` (le rôle de chaque appareil :
`name`, `kind`, et `room`, facultatif depuis le 28 octobre 2026, la pièce que la liste
des appareils montre au propriétaire, même règle que `name` ; une centrale plus
ancienne refuse un fichier qui dit `room` : ne l'écrire qu'après sa mise à jour),
`siren_ids` (les sirènes ; `[]` pour un site sans sirène, par choix : ses alarmes ne
font que prévenir, voir `engine.md`, « Un site sans sirène » ; la ligne est obligatoire) et `siren_names` (facultatif : le nom d'une sirène dans les
textes des alertes, par identifiant, par exemple `{"dev_siren1": "Reserve"}` ; même
règle qu'un nom de zone ; une sirène sans nom s'y appelle « Sirène », ou « Sirène 2 »
parmi plusieurs ; une sirène qui a aussi une zone garde le nom de sa zone et n'a pas de
`siren_names`, voir `escalation.md`), `closing` (heure de fermeture par jour, `"off"`
pour ne pas armer ce jour-là), `access.iterations` (coût du hachage, à régler sur la
centrale réelle).

## Démarrage

Sur la centrale, systemd démarre les programmes et les relance s'ils tombent :
installation dans `systeme.md`, fichiers dans `deploy/`.

1. Le fichier de pid `hub.pid` est posé dans le dossier d'état, et la centrale le
   tient par un verrou (`flock`) que le système lâche quand elle s'arrête, de quelque
   façon que ce soit. Si un autre processus le tient, la centrale refuse de démarrer :
   deux centrales sur le même état croiraient chacune posséder l'alarme. Un fichier
   resté après une coupure de courant n'empêche rien, même si son pid a été redonné à
   un autre programme au démarrage suivant : personne ne le tient. Il en va de même
   pour le fichier de pid de chaque programme.
2. L'état sauvegardé est lu. Un état abîmé des deux copies empêche le démarrage : c'est
   à la personne qui installe de décider.
3. Le client MQTT se connecte (il réessaie sans fin si le broker n'est pas là : la
   centrale attend et ne s'arrête pas).
4. Quand les abonnements sont actifs, la **fenêtre de démarrage** (`boot_window`, 2 s)
   commence : la centrale écoute les états retenus des appareils, puis dit au moteur à
   quoi ressemble le site (une porte restée ouverte est connue avant le premier
   événement), chaque état daté de son début (`since`). Le moteur écarte un état plus
   ancien que ce qu'il avait entendu : après une coupure, Mosquitto rend les états de sa
   dernière sauvegarde, vieux de 5 minutes au plus (voir `engine.md`, « Persistance et
   redémarrage »). Si la liaison ne vient pas en 30 s, la centrale écoute quand même.
5. Le moteur reprend où il s'était arrêté (armé reste armé), le premier état retenu, le
   premier message de santé et la liste des appareils (`system.devices`) partent.

Les états retenus reçus après la fenêtre sont ignorés : les événements portent ensuite
chaque changement. Sauf la présence des appareils (`availability`), suivie par son état
retenu même après la fenêtre : lui seul dit depuis quand un appareil est hors ligne et
sa dernière nouvelle.

**Chien de garde.** Chaque programme dit à systemd qu'il a démarré (`READY=1`), puis,
toutes les 10 s, qu'il fonctionne encore (`WATCHDOG=1`), mais seulement si ses boucles
répondent : le moteur, la santé et l'escalade pour la centrale, la boucle de son
domaine pour les autres. Une boucle bloquée (dans le domaine, l'envoi ou le disque)
fait taire le programme ; après 30 s (`WatchdogSec`), systemd le tue avec `SIGABRT`
(l'état de chaque goroutine part dans le journal, utile pour comprendre) et le
redémarre. Le journal dit d'abord « the program does not answer ». Hors systemd (sur
un poste de développement), rien de tout cela ne se passe.

## Arrêt

`SIGTERM` ou Ctrl-C : la centrale envoie ce qui reste dans sa file (3 s au plus), ferme
la connexion, retire `hub.pid`.

## Codes sans redémarrage

La console locale peut aussi régler le code d'un contact pendant le mode installation
(`security-install.md`). Après `codes set`, `codes remove` ou un code donné depuis la
console, l'outil envoie `SIGHUP` à la centrale si elle
tourne (il lit son pid dans `hub.pid`, seulement si la centrale tient le fichier : le
pid d'un fichier que personne ne tient peut être celui d'un autre programme, que le
signal arrêterait), qui relit les codes. Si le rechargement échoue,
les anciens codes restent en usage et une erreur est journalisée.

La centrale prend les contacts tels qu'ils sont enregistrés au moment où elle relit
les codes (fichier de la centrale et configuration venue du cloud). Elle relit aussi
les codes dès qu'une configuration venue du cloud est enregistrée, sans attendre de
redémarrer : un contact ajouté pendant le mode installation peut recevoir un code tout
de suite, un contact retiré ne désarme plus.

## Configuration venue du cloud

Le client règle depuis l'application ses contacts, ses horaires de fermeture, ses
délais et, en données mobiles, son forfait vidéo (`config.set`, voir `contract.md`).

1. La centrale vérifie la configuration avec son propre fichier (un propriétaire
   parmi les contacts s'il y en a, des délais dans les limites, des horaires
   lisibles). Une liste vide envoyée (aucun contact, aucun horaire) est une liste :
   elle remplace celle du fichier, et le reste après le redémarrage. Refusée, elle
   répond tout de suite `config.applied` `rejected`, avec la raison ; rien ne change.
2. Acceptée, elle la garde dans `site-config.json`, dans le dossier d'état
   (`/var/lib/safehome`, lisible par le service seul : le fichier contient les
   numéros). `/etc/safehome` reste en lecture seule.
3. Elle redémarre pour l'appliquer, **jamais pendant un délai, une alarme ni le mode
   installation** : elle attend que l'alarme soit désarmée ou armée et le mode fermé.
   Le moteur le confirme lui-même au moment de redémarrer : un délai commencé à
   l'instant (une porte ouverte) n'est pas encore sur `alarm/state`.
   Une configuration reçue pendant le mode installation est gardée et appliquée à sa
   fermeture. systemd la relance (`Restart=always`), l'état armé ou désarmé est repris
   comme après une coupure.
4. Relancée, elle pose la configuration sur son fichier et publie `config.applied`
   `applied` (retenu).
5. La passerelle GSM et la vidéo voient ce message et redémarrent à leur tour si le
   fichier contient une autre version que la leur. Une alerte GSM en cours est reprise
   au redémarrage ; une session vidéo s'arrête.

Ce que le cloud règle remplace le fichier du programme ; le reste du fichier ne
change pas. Une liste envoyée (contacts, horaires) remplace celle du fichier.

Un `site-config.json` abîmé ou qui ne convient plus (fichier de la centrale modifié
depuis, ou contrat rendu plus strict par une mise à jour) **n'empêche jamais un programme de démarrer** : il est laissé de côté, le
programme tourne sur son propre fichier et le dit dans son journal. La centrale
répond alors `rejected` avec la raison si le fichier se lit mais ne convient plus ; un
fichier illisible est seulement journalisé. `check` affiche la version en service, ou
pourquoi elle est laissée de côté :

```text
configuration OK: site tg-lom-0001, 2 zone(s), 1 siren(s), 2 contact(s), state in /var/lib/safehome
configuration from the cloud: version 4
```

## Journaux

Sur la sortie d'erreur, au format texte (`log_level` : `debug`, `info`, `warn`,
`error`). Aucun journal ne contient un code d'accès (un test le vérifie).

## Limites connues

- La file d'envoi est en mémoire : un message non parti est perdu si la centrale tombe
  avant le retour du broker. Le broker local est sur la même machine, le risque est
  faible, mais il existe.
- Le coût du hachage des codes n'est pas encore mesuré sur un Raspberry Pi.
- Un changement d'horaires de fermeture n'est pris en compte qu'au prochain désarmement.
- La passerelle GSM et la vidéo lisent `site-config.json` à leur démarrage : si l'une
  redémarre pendant que la centrale attend son moment calme, elle prend la nouvelle
  version un peu avant la centrale.
- Les demandes d'alerte par application et WhatsApp partent vers le cloud par l'agent
  de liaison. Le cloud envoie WhatsApp une fois le compte Meta ouvert (avant, il répond
  `failed`) ; l'application Android est en essai (Firebase allumé ; sans lui, `no_app`). Sans cloud joignable,
  l'agent répond `no_uplink`. Dans tous ces cas, l'escalade passe au GSM.

## La passerelle GSM (`safehome-gsm`)

Elle tourne à côté de la centrale et possède le modem, sauf sur une centrale sans ligne
SMS (`without_gsm` dans `hub.json`), qui n'en a pas. Elle exécute les demandes
d'alerte `sms` et `call`, publie leurs résultats, transforme les réponses « 1 » en
acquittements et publie l'état des lignes (`gsm.status`), que la centrale recopie dans
`system.health`. Fonctionnement détaillé : `gsm-gateway.md`.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-gsm ./cmd/safehome-gsm
safehome-gsm check -config /etc/safehome/gsm.json
safehome-gsm run   -config /etc/safehome/gsm.json
```

Configuration (`config/gsm.example.json`), stricte comme celle de la centrale :

- `lines` : une ligne par modem, avec son `port` série (souvent `/dev/ttyUSB2`), sa
  vitesse (`baud`, 115200 par défaut) et `configure_port` (le programme règle le port
  avec `stty` avant de l'ouvrir ; à mettre à `false` si le système le fait déjà).
- `contacts` : le numéro de chaque contact, par identifiant (les mêmes que dans le
  fichier de la centrale). Les numéros locaux reçoivent l'indicatif `country` (228).
- `test_number` : le numéro de contrôle du SMS de test hebdomadaire. Sans lui, une SIM
  suspendue ne se verra qu'au moment d'une alerte (`check` le signale).
- `ring_for`, `check_every`, `test_every`, `max_age` : voir `gsm-gateway.md`.
- `status_every` (1 min) : la passerelle republie son état à ce rythme même sans
  changement. La centrale la croit morte après `health.gsm_silent_after` sans nouvelles.

Elle garde sa file dans le même dossier d'état que la centrale (fichiers `gsm.json`,
`gsm_seq.json` et leur copie `.prev`) et son propre fichier de pid (`gsm.pid`). Un
modem absent au démarrage n'arrête pas la passerelle : elle réessaie à chaque
vérification. Ses journaux ne contiennent ni les textes ni les numéros.

## L'adaptateur Zigbee (`safehome-zigbee`)

Il tourne à côté de Zigbee2MQTT, sur le même broker. Il traduit les messages des
capteurs en événements du contrat, commande la sirène, tient le registre des appareils
et conduit l'appairage. Fonctionnement détaillé : `adapter-zigbee.md`.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-zigbee ./cmd/safehome-zigbee
safehome-zigbee check -config /etc/safehome/zigbee.json
safehome-zigbee run   -config /etc/safehome/zigbee.json
```

Configuration (`config/zigbee.example.json`), stricte :

- `profiles_dir` : le dossier des profils d'appareils (un fichier JSON par modèle,
  exemples dans `config/profiles/`). Un chemin relatif part du dossier du fichier de
  configuration. `check` liste les profils lus ; un profil faux arrête le programme.
- `base_topic` : le sujet de base de Zigbee2MQTT (`zigbee2mqtt` par défaut). Il ne
  peut pas être `safehome` ni commencer par `safehome/` : l'adaptateur lirait ses
  propres messages comme des appareils.
- `confirm_wait` (3 s) et `attempts` (3) : attente de la confirmation d'une commande
  et nombre d'envois, de même pour la réponse de Zigbee2MQTT à une demande d'ouvrir le
  réseau (sans réponse au bout de 9 s, l'appairage échoue) ; `offline_after` (4 h) : silence avant « hors ligne » pour les
  profils qui ne disent rien ; `pair_max` (10 min) : durée maximale d'appairage (ouverte par morceaux de 254 s,
  le plus que Zigbee2MQTT accepte d'une demande) ;
  `step` (5) : écart minimal pour annoncer une pile ou un lien ; `status_every` (1 min).

Il garde son registre et ses états dans le même dossier d'état (fichiers
`zigbee.json`, `zigbee_seq.json` et leur copie `.prev`) et son propre fichier de pid
(`zigbee.pid`). L'adresse Zigbee des appareils ne sort jamais de l'adaptateur, et
aucun code de clavier n'apparaît dans ses journaux.

Zigbee2MQTT doit dater chaque message (`last_seen: ISO_8601` sous `advanced:`, déjà
dans `deploy/zigbee2mqtt/configuration.yaml`). Sans cela, une touche du clavier gardée
par le broker pendant une coupure de l'adaptateur est datée de son arrivée, et peut
encore désarmer (`adapter-zigbee.md`, « QoS »). `deploy/install.sh` ne pose ce fichier
que la première fois. Zigbee2MQTT y ajoute lui-même, sous `devices:`, `qos: 0` pour
chaque clavier : c'est l'adaptateur qui le lui demande (`adapter-zigbee.md`,
« Clavier : QoS 0 »), il ne faut pas l'enlever.

Appairer un appareil se fait localement, par un message `registry.pair` sur
`safehome/{site}/registry/cmd` (le cloud ne peut pas y écrire) : c'est le rôle de
l'outil d'installation, ci-dessous.

## L'agent de liaison (`safehome-agent`)

Il relie la centrale au cloud : il envoie ce que le contrat laisse monter (alarmes,
alertes, états, santé, heartbeat), le garde sur disque quand internet est coupé, et ne
publie sur la centrale que ce que le cloud a le droit d'envoyer. Fonctionnement
détaillé : `agent.md`.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-agent ./cmd/safehome-agent
safehome-agent check -config /etc/safehome/agent.json
safehome-agent run   -config /etc/safehome/agent.json
```

Configuration (`config/agent.example.json`), stricte :

- `cloud.urls` : le broker du cloud, puis ses replis, essayés dans l'ordre. Seulement
  `mqtts://` (MQTT sur TLS, port 8883) ou `wss://` (MQTT dans WebSocket sur TLS, port
  443) : une adresse non chiffrée est refusée, comme un identifiant écrit dans
  l'adresse.
- `cloud.username` (le site par défaut) et `cloud.password_file` : le fichier qui
  contient le secret du site (exemple : `config/cloud.secret.example`). Un chemin
  relatif part du dossier du fichier de configuration. Le secret n'apparaît ni dans
  `check`, ni dans les journaux.
- `cloud.ca_file` : l'autorité qui doit avoir signé le certificat du cloud ; vide, ce
  sont les autorités du système. La même vaut pour l'envoi des photos en HTTPS.
- `uplink` : `fixed` (la box du commerce, par défaut) ou `mobile` (un routeur 4G dont
  le client paie les données). En `mobile`, le **mode économe** s'applique : le
  heartbeat et `agent.status` ne partent au cloud que toutes les 5 minutes. Un
  `agent.status` qui change (liaison, transport) part tout de suite. Mettre la même
  valeur que dans `video.json`.
- `status_every` (1 min) : répétition de `agent.status` ; `confirm_wait` (10 s) :
  attente de la prise en charge d'une alerte par le cloud avant `no_uplink`.
- `queue` : bornes de la file pour le cloud, `max_items` (20 000), `max_bytes` (10 Mo),
  `max_age` (7 jours).
- `down` : limite de ce qui descend du cloud, `burst` (20 d'un coup) et `per_minute`
  (60).

Il garde son état dans le même dossier d'état (fichiers `agent.json`, `agent_seq.json`,
`agent_uplink.json` (le dernier numéro donné au cloud) et leur copie `.prev`), sa file pour le cloud dans `agent_queue/` (un petit fichier par
message) et son propre fichier de pid (`agent.pid`). Il n'ouvre aucun port : la
connexion au cloud est sortante. À l'arrêt, il dit au cloud que le site passe hors
ligne ; s'il disparaît sans prévenir (courant, câble), le broker du cloud le dit à sa
place (Last Will).

Dans les journaux : `connected to the cloud` (avec le transport, `mqtts` ou `wss`),
`the cloud cannot be reached`, `refused a message from the cloud` (avec la raison),
`messages for the cloud were dropped` (file pleine, trop vieux, ou refusé pour de bon
par le broker du cloud ; la raison est dans la ligne), `photo not sent` (avec la raison)
et `a photo could not be sent, trying again later` méritent l'attention. Il envoie
les photos que le service vidéo laisse dans `photos/` du dossier d'état, puis les
efface.

## La vidéo en direct (`safehome-video`)

Quand le cloud demande une session (`video.request`), il fait pousser par go2rtc le
flux d'une caméra vers le relais du cloud, respecte le plafond du mois en données
mobiles et dit comment la session se passe. Il lance un go2rtc par session et l'arrête
à la fin. À l'alarme, il prend les photos des caméras et les laisse à l'agent.
Fonctionnement détaillé : `video.md`.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-video ./cmd/safehome-video
safehome-video check -config /etc/safehome/video.json
safehome-video run   -config /etc/safehome/video.json
```

`check` vérifie aussi que go2rtc est installé. Configuration
(`config/video.example.json`), stricte :

- `cameras` : pour chaque caméra, `id` (`cam_caisse` : lettres minuscules, chiffres et
  `_`), `name` (montré dans l'application) et `source`, l'adresse RTSP du **flux
  secondaire** avec l'utilisateur et le mot de passe de la caméra (Tapo C200 :
  `rtsp://utilisateur:motdepasse@ADRESSE:554/stream2`). La caméra doit envoyer du
  H.264. L'adresse n'apparaît ni dans `check`, ni dans les journaux.
- `go2rtc` : le programme go2rtc (`/usr/local/bin/go2rtc`).
- `ffmpeg` (`/usr/bin/ffmpeg`) : décode les photos ; `check` prévient s'il manque.
  `photos` (`true`) : `false` coupe les photos à l'alarme.
- `uplink` : `fixed` (la box du commerce : ni plafond ni durée limite) ou `mobile`
  (routeur 4G). En `mobile`, `quota_mb` (1024 par défaut, 0 pour aucun, 8 388 608
  au plus) et
  `max_session` (`5m` par défaut, `0` pour aucune) ; en `fixed`, ces deux réglages
  sont refusés.
- `max_sessions` (2) : sessions en même temps ; `start_timeout` (20 s) : délai pour que
  les premiers octets partent ; `stall_after` (30 s) : silence qui termine une session ;
  `probe_every` (5 min) : test des caméras.

Il garde la consommation du mois dans le même dossier d'état (fichiers `video.json`,
`video_seq.json` et leur copie `.prev`) et son propre fichier de pid (`video.pid`).
Chaque go2rtc travaille dans un dossier privé de `/tmp`, effacé à la fin de la
session.

Les photos attendent l'agent dans `photos/` du dossier d'état (60 au plus, 8 Mo au
plus, les plus anciennes partent d'abord).

Dans les journaux : `photos of the alarm taken` (combien, combien d'octets), `a camera
gave no photo`, `no photos for this alarm` (plus de 6 alarmes dans l'heure),
`video session` (chaque étape, avec la raison et les octets),
`a camera does not answer` et `a video stream could not start` (avec la cause, par
exemple une caméra réglée en H.265). Les lignes `go2rtc` sont les messages de go2rtc
lui-même, adresses et jeton masqués.

## La maintenance à distance (`safehome-maint`)

Sur demande du cloud (`maint.open`), il ouvre un tunnel SSH sortant vers notre
bastion, une heure au plus, et dit chaque étape (`maint.session`), que le propriétaire
voit. Fonctionnement, sécurité et installation des clés : `maintenance.md`.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-maint ./cmd/safehome-maint
safehome-maint check -config /etc/safehome/maint.json
safehome-maint run   -config /etc/safehome/maint.json
```

`check` vérifie aussi que `ssh`, la clé de la centrale et la clé d'hôte du bastion
sont là. Il garde une session en cours dans le dossier d'état (fichiers `maint.json`,
`maint_seq.json` et leur copie `.prev`, pid `maint.pid`) : elle reprend après un
redémarrage, jusqu'à son heure de fin.

Dans les journaux : `remote maintenance asked` (qui, pourquoi, combien de temps),
`remote maintenance` (chaque étape), `the bastion granted the tunnel`, `ssh` (les
avertissements et erreurs d'OpenSSH : clé refusée, bastion injoignable) et `ssh
stopped`.

## L'outil d'installation (`safehome-install`)

Un outil en ligne de commande, lancé à la main sur la centrale (en SSH) par
l'installateur. Ce n'est pas un service : il fait une chose et s'arrête. Il lit la
configuration de la centrale (`hub.json`) pour connaître le site et le broker. La
console locale (plus bas) fait le même travail depuis un téléphone.

```bash
GOOS=linux GOARCH=arm64 go build -o safehome-install ./cmd/safehome-install
```

| Commande | Effet |
|---|---|
| `devices -config F` | Les appareils appairés, leur modèle, leurs capacités et leur usage (zone, sirène, clavier, ou « inutilisé ») ; l'état de Zigbee2MQTT et les appareils hors ligne |
| `pair -config F [-for 2m]` | Ouvre le réseau (10 min au plus) et affiche chaque appareil ajouté ou refusé, avec la raison ; Ctrl-C referme le réseau tout de suite |
| `zone -config F -name NOM -kind TYPE [-room PIECE] APPAREIL` | Donne sa zone à un appareil (ou la change) ; `-room` dit aussi sa pièce (même règle que le nom), que la liste des appareils montre au propriétaire. La zone est donnée en entier : sans `-room`, elle n'a plus de pièce |
| `unzone -config F APPAREIL` | Retire la zone d'un appareil |
| `siren -config F add\|remove APPAREIL` | Fait d'un appareil une sirène, ou plus ; `add -name NOM` la nomme aussi |
| `siren -config F name -name NOM APPAREIL` | Donne à une sirène son nom dans les textes des alertes (ou le change) ; une sirène qui a une zone garde le nom de sa zone |
| `remove -config F APPAREIL` | Retire un appareil du réseau pour de bon, et de la configuration (zone, sirène) |
| `watch -config F [APPAREIL]` | Affiche les événements en direct, avec le nom de la zone : pour les tests (déclencher chaque appareil). Les codes du clavier ne s'affichent jamais |
| `check -config F` | Compare la configuration et les appareils appairés |

Les options peuvent aussi venir après l'appareil
(`zone dev_k7p2 -name Reserve -kind entry -config F`).

Une installation type :

```bash
sudo -u safehome safehome-install pair  -config /etc/safehome/hub.json
sudo -u safehome safehome-install zone  -config /etc/safehome/hub.json -name "Porte arriere" -kind instant -room "Reserve" dev_k7p2
sudo -u safehome safehome-install siren -config /etc/safehome/hub.json name -name "Reserve" dev_siren1
sudo -u safehome safehome-install watch -config /etc/safehome/hub.json
sudo -u safehome safehome-install check -config /etc/safehome/hub.json
sudo systemctl restart safehome-hub
```

Ce que l'outil vérifie avant de toucher à quoi que ce soit :

- **La zone convient à l'appareil** : un contact ne peut pas être une zone incendie, une
  sirène doit savoir sonner. Un appareil d'un autre adaptateur (absent du registre
  Zigbee) n'est accepté qu'avec `-force`.
- **La configuration reste valable** : la nouvelle version est vérifiée comme au
  démarrage de la centrale avant d'être écrite. Par exemple, la dernière sirène ne peut
  pas être retirée : il faut d'abord ajouter la nouvelle (un site sans sirène est un
  choix écrit dans `hub.json`, `"siren_ids": []`, jamais l'effet d'un retrait). Pour `remove`, cette vérification a lieu **avant** le retrait du réseau,
  qui ne se défait pas. Pendant le retrait, l'appareil est tenu pour parti : aucun
  autre changement du même programme ne peut laisser un site qui ne marcherait pas sans
  lui (retirer l'autre sirène, par exemple). Cette garde ne vaut qu'à l'intérieur d'un
  programme : la console et l'outil ont chacun la leur, et `check` dit après coup qu'un
  site n'a plus aucune sirène appairée.
- **Un retrait parti sans réponse** : l'ordre est envoyé mais l'adaptateur ne répond
  pas à temps. Il peut encore l'exécuter un instant plus tard : l'appareil quitte donc
  la configuration quand même, et `remove` le dit (`the removal was sent but not
  confirmed`) puis s'arrête en erreur. `devices` montre ensuite s'il est encore
  appairé.
- **L'adaptateur répond** : `pair` attend que l'adaptateur Zigbee confirme l'ouverture
  du réseau (15 s), sinon il dit que `safehome-zigbee` ne tourne pas, au lieu d'attendre
  deux minutes pour rien. L'adaptateur ne la confirme qu'une fois que Zigbee2MQTT l'a
  confirmée lui-même ; s'il refuse ou ne répond pas, `pair` affiche
  `the network could not be opened:` suivi de la raison (les mots de Zigbee2MQTT, ou
  `Zigbee2MQTT did not answer the request to open the network: is it running?`) et
  s'arrête en erreur.

`check` signale :

- comme **problèmes** (code de sortie 1) : une zone qui ne pourra jamais se déclencher,
  un type de zone inconnu, une zone sans nom, une sirène qui ne sait pas sonner, un nom
  de sirène fautif ou donné à une sirène qui a une zone, des sirènes dont aucune n'est
  appairée (rien ne peut sonner) ;
- comme **avertissements** : un appareil appairé qui ne sert à rien, une zone ou une
  sirène dont l'appareil n'est pas appairé, un site sans zone ou sans sirène, une
  sirène sans nom ni zone sur un site qui en a plusieurs ainsi (les textes ne la
  désignent que par son rang, « Sirène 2 »).

Les zones et les sirènes, avec leurs noms, sont écrites dans le dossier d'état de la
centrale (`/var/lib/safehome/site-devices.json`, droits 0600), pas dans `hub.json` :
`/etc/safehome` est en lecture seule une fois l'installation finie, alors qu'un capteur
s'ajoute plus tard. Le remplacement est atomique (une coupure laisse l'ancien fichier
ou le nouveau). Lancer l'outil sous l'utilisateur de la centrale
(`sudo -u safehome`) : le dossier d'état est le sien. Lancé avec `sudo` seul, il rend
quand même le fichier à cet utilisateur.

**La centrale applique les zones d'elle-même** : elle compare `site-devices.json` à
celui qu'elle a chargé à chaque état de l'alarme, à chaque ouverture ou fermeture du
mode installation et à chaque configuration reçue. Une modification faite avec
`safehome-install` hors mode installation est ainsi appliquée au prochain moment calme
(alarme désarmée, ou pendant le délai de sortie, mode fermé) : la centrale redémarre.
Pas une fois armée : l'armement fait sur les anciennes zones n'a jamais vu les nouveaux
appareils, et une fenêtre d'une nouvelle zone restée ouverte serait prise au
redémarrage pour une fenêtre ouverte pendant l'arrêt, une sirène dès l'armement.
Redémarrée pendant le délai de sortie, la centrale le finit sur les nouvelles zones :
cette fenêtre est contournée et signalée, comme à la fin de tout délai de sortie. Des
zones changées trouvées armées attendent le prochain désarmement. Corrigé le 25
octobre 2026 après une revue. Pour l'appliquer tout
de suite, la redémarrer (`sudo systemctl restart safehome-hub`). L'état est conservé :
une alarme armée le reste. Avec la console locale, la centrale redémarre d'elle-même à
la fin du mode installation.

Limites :

- L'outil n'utilise pas le mode installation : il suppose un accès SSH à la centrale,
  donc déjà la clé de l'installateur.

## La console locale (`safehome-console`)

Le service qui sert la page de l'installateur pendant le mode installation (ouvert
avec le code installateur au clavier, 30 minutes au plus ; refaire le code repart pour
30 minutes). Il allume alors le réseau
Wi-Fi de la centrale et y sert la page `http://10.42.0.1/`. Il éteint les deux à la fin
du mode. Le reste du temps, il attend sans rien ouvrir. Fonctionnement et sécurité :
`security-install.md`.

Chaque appareil y montre sa zone et sa pièce, et le formulaire de zone les change
(depuis le 28 octobre 2026). Comme `safehome-install zone`, la zone part en entier : le
champ pièce, prérempli de la pièce actuelle, la garde quand on change seulement le nom
ou le type ; vidé, la zone n'a plus de pièce. Une pièce hors règle est refusée comme un
nom, sans rien écrire.

| Commande | Effet |
|---|---|
| `safehome-console check -config FICHIER` | Vérifie la configuration, celle de la centrale, et la présence de `nmcli` |
| `safehome-console run -config FICHIER` | Lance la console |
| `safehome-console version` | Affiche la version |

`console.json` (exemple : `config/console.example.json`) :

- `site`, `state_dir` : ceux de la centrale (vérifié). La console écrit les zones et
  les sirènes dans ce dossier.
- `hub_config` : le fichier de la centrale (`/etc/safehome/hub.json`), qui sert à
  vérifier chaque changement comme la centrale le ferait.
- `wifi.interface` (`wlan0`), `wifi.ssid` (`safehome-` puis le site), `wifi.address`
  (`10.42.0.1`), `wifi.passphrase_file` : la phrase de passe WPA2 (8 à 63 caractères ASCII imprimables, sans accent),
  celle de l'étiquette. Un chemin relatif part du dossier du fichier. Le script
  d'installation la crée (`console.secret`) s'il n'y en a pas.
- `port` (80), `pair_for` (2 min, la durée d'un appairage depuis la page).

Dans les journaux : `installation mode open`, `the console's page is served`,
`installation mode over`, chaque changement fait depuis la page (`zone set from the
console`, `access code set from the console` avec le contact seulement), et en erreur `the console's network did not start` (avec ce qu'a dit
`nmcli`). La phrase de passe n'apparaît jamais.

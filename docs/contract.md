# Contrat d'événements safeHome

Statut : v1 validée sur les points de format, d'envoi vers le cloud, de heartbeat
et d'armement. Les schémas JSON sont dans `schemas/`, vérifiés par les tests.

Ce contrat est le seul langage commun entre les composants du site (adaptateur
matériel, moteur d'alarme, passerelle GSM, service caméras, agent de liaison) et
le cloud. Les composants n'échangent rien d'autre. Un changement de matériel ne
touche que l'adaptateur (voir `architecture.md`).

## Principes

- **Format** : JSON pour la v1 (lisible, simple en Go comme en Java). Chaque type
  de message a un JSON Schema dans le dépôt, qui sert de référence et génère les
  types dans les deux langages. Les champs restent plats et les valeurs sont des
  chaînes courtes ou des nombres, pour qu'un passage futur à CBOR ou Protobuf
  soit mécanique.
- **Transport** : MQTT, sur le broker local et sur la liaison vers le cloud, avec
  les mêmes topics.
- **Au moins une fois** : les messages d'événements et de commandes passent en QoS
  1. Un message peut arriver deux fois, donc chaque récepteur déduplique avec
  `id`.
- **Tolérant aux champs inconnus** : un récepteur ignore ce qu'il ne connaît pas.
  Ajouter un champ ne casse rien.
- **Pas de secret ni de donnée personnelle** dans les événements. Seule exception :
  le code du site, présent dans les événements `keypad` et dans les ordres
  `alarm.disarm` et `alarm.silence`. Il ne circule que sur le broker local et sur
  la liaison TLS vers le site, n'est jamais écrit dans les journaux, n'est jamais
  renvoyé dans une réponse et n'est jamais transmis au cloud par l'agent de
  liaison dans un événement.

## Enveloppe commune

Tous les messages ont la même enveloppe.

```json
{
  "v": 1,
  "id": "01JB8ZK3M2Q7T4X9H6N5R0WYCE",
  "site": "tg-lom-0001",
  "src": "adapter.zigbee",
  "seq": 48213,
  "ts": "2026-10-17T21:04:05.312Z",
  "type": "device.event",
  "data": { }
}
```

| Champ | Rôle |
|---|---|
| `v` | Version du contrat. Un changement incompatible incrémente `v`. |
| `id` | ULID : unique et triable. Sert à dédupliquer. |
| `site` | Identifiant du site, présent dès le début même avec un seul client. |
| `src` | Composant émetteur (`adapter.zigbee`, `engine`, `gsm`, `video`, `agent.link`). |
| `seq` | Sur le site, compteur croissant de chaque programme, conservé sur disque. Vers le cloud, l'agent renumérote : continu par site, dans l'ordre d'envoi, pour repérer les trous (rejeu : `agent.replay`, voir `agent.md`). 0 : non numéroté (dernier message). |
| `ts` | Heure UTC de la centrale, à la milliseconde. Peut être fausse si l'horloge a dérivé : le cloud ajoute sa propre heure de réception. |
| `type` | Type de message (voir plus bas). |
| `data` | Contenu propre au type. |
| `sig`, `kid` | **Réservés** pour la phase fermée : signature du message et identifiant de la clé. Absents en v1. |

## Topics MQTT

Tous les topics commencent par `safehome/{site}/`.

| Topic | Contenu | Retenu |
|---|---|---|
| `.../device/{device_id}/event` | Événements d'un appareil (porte ouverte, mouvement, panique) | non |
| `.../device/{device_id}/state/{capacité}` | Dernier état connu d'une capacité de l'appareil (un sujet par capacité) | oui |
| `.../device/{device_id}/cmd` | Commande vers l'appareil | non |
| `.../device/{device_id}/ack` | Réponse à une commande | non |
| `.../alarm/event` | Événements du cycle d'alarme (armée, déclenchée…) | non |
| `.../alarm/state` | État courant du moteur | oui |
| `.../alarm/cmd` | Ordre au moteur (armer, désarmer…), depuis le clavier, l'application ou le planning | non |
| `.../alarm/ack` | Réponse du moteur à un ordre | non |
| `.../alert/request` | Demande d'envoi d'alerte (SMS, appel) du moteur vers les canaux | non |
| `.../alert/result` | Résultat d'un envoi (envoyé, échoué, acquitté) | non |
| `.../alert/ack` | Acquittement d'un contact (SMS ou application) | non |
| `.../registry/cmd` | Appairage et retrait d'appareils, **local seulement** | non |
| `.../registry/event` | Appareil ajouté, refusé ou retiré | non |
| `.../registry/devices` | Liste des appareils connus | oui |
| `.../config/set` | Nouvelle configuration du site, numérotée par version (cloud vers site) | non |
| `.../config/applied` | Version de configuration appliquée par le site, ou refusée | oui |
| `.../system/heartbeat` | Signe de vie périodique | non |
| `.../system/health` | Santé de la centrale (secteur, batterie, liaisons) | oui |
| `.../gsm/status` | Lignes de la passerelle GSM (modem, réseau, crédit, test) | oui |
| `.../zigbee/status` | État du réseau Zigbee (Zigbee2MQTT, appareils muets, appairage) | oui |
| `.../agent/status` | Liaison avec le cloud, publiée par l'agent de liaison | oui |
| `.../video/cmd` | Ouvrir ou arrêter une session vidéo en direct (cloud vers site) | non |
| `.../video/session` | Étapes d'une session vidéo | non |
| `.../video/status` | Caméras, sessions en cours, données du mois | oui |
| `.../photo/taken` | Photo prise à l'alarme, annoncée par le service vidéo | non |
| `.../photo/cmd` | Où envoyer une photo (cloud vers l'agent de liaison seulement) | non |
| `.../photo/sent` | Résultat de l'envoi d'une photo, par l'agent | non |
| `.../maint/cmd` | Ouvrir ou fermer un tunnel de maintenance (cloud vers centrale) | non |
| `.../maint/session` | Étapes d'une session de maintenance, vues par le propriétaire | non |
| `.../install/mode` | Mode installation ouvert ou fermé, publié par la centrale | oui |
| `.../install/cmd` | Fin du mode installation, depuis la console locale, **local seulement** | non |

## Appareils et capacités

Le moteur raisonne sur des capacités, pas sur des modèles. Chaque appareil
déclare un `device_id` à nous. L'adresse du matériel (par exemple l'adresse
Zigbee) reste dans l'adaptateur.

| Capacité | Valeurs |
|---|---|
| `contact` | `open`, `closed` |
| `motion` | `detected`, `clear` |
| `smoke` | `alarm`, `clear` |
| `vibration` | `detected`, `clear` |
| `glass_break` | `detected`, `clear` |
| `panic` | `pressed` |
| `tamper` | `triggered`, `clear` |
| `keypad` | `arm`, `arm_now`, `disarm`, `silence`, `postpone` (`disarm` et `silence` portent un champ `code`) |
| `siren` | `on`, `off` |
| `mains` | `present`, `lost` (émis par le module batterie de la centrale, pas par un appareil Zigbee) |
| `battery` | pourcentage, 0 à 100 |
| `link` | qualité du lien radio, 0 à 100 |
| `availability` | `online`, `offline` (présence de l'appareil, émis par l'adaptateur) |

Description d'un appareil dans le registre (tenu par l'adaptateur) :

```json
{
  "device_id": "dev_k7p2",
  "model": "aqara.contact.mccgq11lm",
  "capabilities": ["contact", "tamper", "battery", "link"],
  "adapter": "adapter.zigbee"
}
```

Le registre dit **ce qu'est** l'appareil. Son nom (« Porte d'entrée ») et sa zone
(`entree`, `instant`…) appartiennent à la configuration du site, lue par le moteur
(voir `engine.md` et `adapter-zigbee.md`).

## Types de messages

### `device.event`

```json
{
  "v": 1,
  "id": "01JB8ZK3M2Q7T4X9H6N5R0WYCE",
  "site": "tg-lom-0001",
  "src": "adapter.zigbee",
  "seq": 48213,
  "ts": "2026-10-17T21:04:05.312Z",
  "type": "device.event",
  "data": {
    "device_id": "dev_k7p2",
    "capability": "contact",
    "value": "open"
  }
}
```

`battery` et `link` sont des événements comme les autres, publiés séparément et
seulement quand la valeur change de façon utile (par exemple 5 points de
batterie), pour économiser les données.

#### Le clavier est un appareil

Les touches du clavier arrivent comme des `device.event` de capacité `keypad`, pas
comme des `alarm.command` : la valeur est `arm`, `arm_now`, `disarm` ou `silence`, et
le champ `code` accompagne `disarm` et `silence`. La centrale traduit en ordre au moteur
(origine `keypad`). Le code n'est jamais recopié dans un message publié, ni dans les
journaux : il est masqué à l'affichage et vérifié sur un fil à part. L'application et le
cloud, eux, écrivent directement sur `alarm/cmd`.

### `device.state`

Même contenu que `device.event`, plus `since` (heure du dernier changement).
Publié en retenu : un composant qui démarre lit l'état courant sans attendre un
nouvel événement.

Un sujet par capacité (`device/{id}/state/contact`, `device/{id}/state/battery`…) :
le broker ne garde que le dernier message retenu d'un sujet, et une lecture de pile
ne doit pas effacer l'état de la porte. Un état ne porte jamais de code. Quand un
appareil est retiré, l'adaptateur efface ses états retenus (message vide).

### `command` et `command.ack`

```json
{
  "type": "command",
  "data": {
    "device_id": "dev_s3x9",
    "command": "siren.on",
    "args": { "duration_s": 60 },
    "exp": "2026-10-17T21:04:35.000Z"
  }
}
```

```json
{
  "type": "command.ack",
  "data": {
    "ref": "01JB8ZK3M2Q7T4X9H6N5R0WYCE",
    "status": "done",
    "reason": null
  }
}
```

- `ref` est l'`id` de la commande.
- `status` : `accepted`, `done`, `failed`, `expired`.
- Une commande périmée (`exp` dépassée) n'est pas exécutée.
- Les commandes sont idempotentes : recevoir deux fois `siren.on` ne change rien.

Commandes de la v1 : `siren.on`, `siren.off`, `device.identify` (faire clignoter
ou biper un appareil pour le repérer).

### Ordres au moteur (`alarm.command` et `alarm.ack`)

Les ordres arrivent sur `alarm/cmd`. Ils viennent du clavier (via l'adaptateur),
de l'application (via le cloud) ou du planning (interne).

```json
{
  "type": "alarm.command",
  "data": {
    "command": "alarm.disarm",
    "args": { "code": "<code>" },
    "origin": "app",
    "user": "u_42",
    "exp": "2026-10-17T21:05:05.000Z"
  }
}
```

| Commande | Effet | Arguments |
|---|---|---|
| `alarm.arm` | Arme, avec délai de sortie | `force` (booléen, optionnel) |
| `alarm.arm_now` | Termine le délai de sortie et arme tout de suite | |
| `alarm.disarm` | Désarme | `code` |
| `alarm.silence` | Coupe la sirène sans désarmer | `code` |
| `alarm.bypass` | Contourne des zones pour la période d'armement | `devices` (liste) |
| `alarm.postpone` | Reporte l'armement automatique | |

- `origin` : `keypad`, `app`, `schedule`. Il n'y a pas de SMS dans la v1.
- `user` : identifiant de la personne, quand il est connu (application).
- Le moteur répond sur `alarm/ack`.

```json
{
  "type": "alarm.ack",
  "data": {
    "ref": "01JB8ZK3M2Q7T4X9H6N5R0WYCE",
    "status": "refused",
    "reason": "zone_open",
    "detail": { "devices": ["dev_k7p2"] }
  }
}
```

- `status` : `done`, `refused`, `expired`, `failed`.
- `reason` si refus : `zone_open`, `bad_code`, `locked_out`, `wrong_state`,
  `postpone_limit`. Une commande inconnue répond `failed` avec la raison
  `unknown_command`.
- Les accusés ne contiennent jamais le code.
- Un armement accepté alors que des appareils sont muets ou ont une pile faible répond
  `done` avec `detail.offline` et `detail.low_battery` (listes d'appareils) : le clavier
  peut avertir. Ces défauts ne bloquent jamais l'armement.

### Événements d'alarme (`alarm.*`)

| Type | Quand | Contenu |
|---|---|---|
| `alarm.arming` | Le délai de sortie démarre | `origin`, `ends_at`, `forced`, `bypassed` (appareils contournés) ; `offline`, `low_battery` si des appareils sont muets ou ont une pile faible (avertissement) |
| `alarm.armed` | Le délai est terminé, ou `arm_now` | `origin`, `user` ; `mode` optionnel, absent en v1 |
| `alarm.disarmed` | Le système est désarmé | `origin`, `user` |
| `alarm.entry_delay` | Un détecteur d'entrée s'est ouvert | `ends_at`, `device_id` |
| `alarm.triggered` | L'alarme se déclenche | `incident`, cause : `device_id`, `capability`, `zone` |
| `alarm.siren_cycle` | Un cycle de sirène démarre | `incident`, `cycle`, `of` |
| `alarm.cleared` | L'alarme est arrêtée | `incident`, `origin` (`user` ou `timeout`), `user`, `duration_s` |
| `alarm.arming_scheduled` | Annonce de l'armement automatique | `scheduled_for` |
| `alarm.postponed` | L'armement automatique est reporté | `scheduled_for`, `count`, `max` |
| `alarm.auth_failed` | Code faux | `origin`, `attempts`, `locked_until` (si verrouillé) |

`alarm.state` (retenu) contient l'état courant : `disarmed`, `arming`, `armed`,
`entry_delay`, `triggered`. Il porte aussi `since`, et selon l'état :
`ends_at` (arming, entry_delay), `cycle` et `of` (triggered), `locked_until`
(si le désarmement est verrouillé), `next_scheduled` (prochain armement
automatique).

### Alertes (`alert.*`)

- `alert.request` : l'escalade demande à un canal d'envoyer un message à un contact
  (une demande par contact et par canal, voir `escalation.md`). Champs : `incident`,
  `kind` (`intrusion`, `fire`, `panic`, `tamper`, `auth_failed`,
  `arming_bypassed`, `battery_low_hub`, `mains_restored`, `device_offline`, `battery_low`, `gsm_degraded`, `zigbee_down`), `urgency` (`critical`, `high`, `low`),
  `wave`, `contact`, `channel` (`app`, `whatsapp`, `sms`, `call`) et `text`.
  `devices` (liste d'appareils, facultatif) est rempli quand l'alerte en concerne
  plusieurs, par exemple `arming_bypassed`, et `percent` (0 à 100) le niveau de
  batterie pour les alertes de batterie.
- `alert.result` : le canal répond. Champs : `ref` (l'`id` du `alert.request`),
  `status` (`queued`, `sent`, `delivered`, `failed`) et `reason` en cas d'échec
  (`no_network`, `no_credit`, `modem_error`, `timeout`, `busy`, `unreachable`,
  `no_uplink`). Pour `app` et `whatsapp`, `sent` veut dire que le cloud a pris le
  message et `delivered` qu'il est arrivé sur le téléphone. `no_uplink` vient de
  l'agent de liaison : internet est coupé, ou le cloud n'a pas confirmé en 10
  secondes. Le moteur s'en sert pour décider d'escalader.
- `alert.ack` : un contact acquitte. Champs : `incident`, `contact`, `via` (`sms`
  ou `app`). Il arrête l'escalade vers les autres contacts.

Le détail de l'escalade est dans `escalation.md`.

### Appareils : appairage et registre (`registry.*`)

Les ordres arrivent sur `registry/cmd` et ne viennent jamais du cloud.

| Type | Rôle | Contenu |
|---|---|---|
| `registry.pair` | Ouvre le réseau à de nouveaux appareils | `duration_s` |
| `registry.remove` | Retire un appareil | `device_id` |
| `registry.snapshot` | Liste des appareils (retenu sur `registry/devices`) | liste d'appareils |
| `registry.device_added` | Appareil accepté | `device_id`, `model`, `capabilities` |
| `registry.device_rejected` | Modèle sans profil, refusé | `model` |
| `registry.device_removed` | Appareil retiré | `device_id` |

- `registry.pair` : `duration_s` à 0 referme le réseau. L'adaptateur borne la durée
  (10 minutes par défaut).
- `registry.device_added`, `registry.device_rejected` et `registry.device_removed`
  portent `ref`, l'`id` de l'ordre qui les a causés (absent pour un appareil refusé
  hors appairage).
- `registry.device_rejected` : `reason` vaut `unknown_model` (pas de profil),
  `not_pairing` (entré hors d'une fenêtre d'appairage) ou `interview_failed` (modèle
  illisible) ; `model` est le modèle du matériel, vide s'il n'a pas pu être lu.
- Dans le registre, `model` est le nom de notre profil (`aqara.contact.mccgq11lm`) et
  `adapter` vaut `adapter.zigbee`. L'adresse Zigbee n'en sort jamais.

### `zigbee.status`

Publié retenu par l'adaptateur Zigbee sur `zigbee/status`, à chaque changement et de
nouveau chaque minute (comme `gsm.status`, la centrale sait ainsi que l'adaptateur
vit) :

```json
{
  "type": "zigbee.status",
  "data": { "bridge": "online", "devices": 6, "offline": ["dev_k7p2"], "pairing": false }
}
```

- `bridge` : `online` quand Zigbee2MQTT et sa clé tournent, `offline` sinon.
- `devices` : appareils enregistrés ; `offline` : ceux qui ne répondent plus ;
  `pairing` : le réseau est ouvert aux nouveaux appareils.
- La centrale en déduit le lien `zigbee` de `system.health` : `ok`, `down`, ou
  `silent` si l'adaptateur ne donne plus de nouvelles. Après 3 minutes de panne, le
  propriétaire reçoit l'alerte `zigbee_down` : l'alarme n'entend plus ses capteurs.

### Vidéo en direct (`video.*`)

Le détail et les raisons des choix sont dans `video.md`.

| Type | Topic | Contenu |
|---|---|---|
| `video.request` | `video/cmd` | `session`, `camera`, `reason` (`alarm`, `demand`), `user`, `incident`, `max_s` (absent ou 0 : sans limite), `push_url` (rtmps), `token`, `exp` |
| `video.stop` | `video/cmd` | `session` |
| `video.session` | `video/session` | `session`, `camera`, `status` (`starting`, `live`, `ended`, `failed`), `reason`, `duration_s`, `bytes` |
| `video.status` | `video/status` (retenu) | `cameras` (`id`, `name`, `reachable`), `active`, `uplink` (`fixed`, `mobile`), `used_mb_month`, `quota_mb`, `max_s` |

```json
{
  "type": "video.request",
  "data": {
    "session": "01JB8ZQ7V2T4X9H6N5R0WYCE00", "camera": "cam_caisse", "reason": "alarm",
    "user": "u_owner", "incident": "01JB8ZK3M2Q7T4X9H6N5R0WYCE", "max_s": 120,
    "push_url": "rtmps://relay.safehome.example/live/01JB8ZQ7V2T4X9H6N5R0WYCE00",
    "token": "<jeton>", "exp": "2026-10-17T21:05:35.000Z"
  }
}
```

- `token` n'est valable que pour cette session et n'apparaît jamais dans un journal. Le
  site l'ajoute à l'adresse en `?token=` au moment d'envoyer ; `push_url` est donc une
  adresse `rtmps://` sans requête ni identifiants.
- En liaison fixe (`uplink` = `fixed`), ni plafond ni durée limite : `max_s` est absent.
  En données mobiles, le client règle `quota_mb` et `max_s`, que le cloud reprend dans
  chaque demande.
- Une demande vue après `exp` répond `failed` avec la raison `expired`.
- Raisons de fin ou d'échec : `stopped`, `timeout`, `quota`, `busy`, `unknown_camera`,
  `camera_unreachable`, `relay_unreachable`, `expired`.

### Photos à l'alarme (`photo.*`)

Le détail est dans `video.md` (prise) et `agent.md` (envoi). Les images ne passent
jamais par MQTT.

| Type | Topic | Contenu |
|---|---|---|
| `photo.taken` | `photo/taken` | `photo` (ULID), `incident`, `camera`, `bytes`, `width` ; le `ts` de l'enveloppe est l'heure de la prise |
| `photo.upload` | `photo/cmd` | `photo`, `url` (https, sans requête ni identifiants), `token`, `exp` |
| `photo.sent` | `photo/sent` | `photo`, `status` (`sent`, `failed`), `reason` (`url_expired`, `missing`, `too_old`, `refused`) |

- Le cloud répond à chaque `photo.taken` par un `photo.upload`. L'agent envoie le
  fichier en `PUT` (`Content-Type: image/jpeg`) avec `Authorization: Bearer <token>`.
  Le jeton ne vaut que pour cette photo et n'apparaît jamais dans un journal.
- `photo.upload` est pour l'agent seul : il n'est jamais republié sur la centrale.
- `url_expired` : l'adresse est arrivée trop tard, a expiré pendant une coupure, ou a
  été refusée (401, 403, 404, 410). Le cloud peut envoyer un nouveau `photo.upload`.
- Une photo attend 24 h au plus sur le site (`too_old` ensuite). `missing` : le
  fichier a disparu (dossier plein, les plus anciennes partent d'abord).
- `refused` : le fichier lui-même est refusé (400, 413, 415, 422). Toute autre erreur
  (réseau, 5xx) est réessayée : 30 s, puis deux fois plus à chaque fois, 10 min au plus.

### Maintenance à distance (`maint.*`)

Le détail est dans `maintenance.md`.

| Type | Topic | Contenu |
|---|---|---|
| `maint.open` | `maint/cmd` | `session`, `user` (l'administrateur), `reason` (200 caractères au plus), `duration_s` (60 à 3600), `exp` |
| `maint.close` | `maint/cmd` | `session` |
| `maint.session` | `maint/session` | `session`, `user`, `status` (`opening`, `open`, `closed`, `failed`), `reason` (`stopped`, `timeout`, `busy`, `expired`, `disabled`, `unreachable`), `until`, `duration_s` |

- La centrale ouvre un tunnel SSH **sortant** vers notre bastion, une heure au plus,
  une session à la fois. Le cloud ne peut que le demander : entrer demande la clé d'un
  administrateur, que le cloud n'a pas.
- `maint.session` part au cloud et le propriétaire le voit dans l'application.

### Mode installation (`install.*`)

Le détail est dans `security-install.md` (console locale).

| Type | Topic | Contenu |
|---|---|---|
| `install.mode` | `install/mode` (retenu) | `status` (`open`, `closed`) ; ouvert : `until` ; fermé : `reason` (`timeout`, `armed`, `stopped`, `restart`) |
| `install.close` | `install/cmd` | rien (`{}`) |

- La centrale ouvre le mode quand le code installateur est tapé au clavier puis
  « désarmer », alarme désarmée, pour 30 minutes. La console locale allume alors son
  réseau Wi-Fi et sa page.
- Il se ferme quand le temps est écoulé (`timeout`), quand l'alarme quitte l'état
  désarmé (`armed`), sur `install.close` de la console (`stopped`), ou au démarrage de
  la centrale (`restart`), qui publie toujours un mode fermé.
- `install.mode` part au cloud : le propriétaire voit qu'un mode installation a été
  ouvert. `install/cmd` ne vient jamais du cloud.

### Configuration venue du cloud (`config.*`)

Le client règle son site dans l'application : contacts, horaires de fermeture, délais,
et en données mobiles son forfait vidéo. Le cloud envoie le tout à chaque changement.

| Type | Topic | Contenu |
|---|---|---|
| `config.set` | `config/set` | `version` (≥ 1, croît à chaque changement), et seulement ce que le cloud règle : `site_name`, `contacts` (`id`, `name`, `role`, `channels`, `phone`), `closing` (jour ou `daily` → `HH:MM` ou `off`), `exit_delay_s`, `entry_delay_s`, `video` (`quota_mb`, `max_s`) |
| `config.applied` | `config/applied` (retenu) | `version`, `status` (`applied`, `rejected`), `reason` |

```json
{
  "type": "config.set",
  "data": {
    "version": 4, "site_name": "Boutique Be",
    "contacts": [
      {"id": "u_owner", "name": "Ama", "role": "owner", "channels": ["app", "sms"], "phone": "+22890000001"},
      {"id": "u_night", "name": "Kofi", "role": "guard", "channels": ["sms", "call"], "phone": "+22870000002"}
    ],
    "closing": {"daily": "19:30", "sun": "off"},
    "exit_delay_s": 60, "entry_delay_s": 30
  }
}
```

- Le message porte **toute** la configuration réglée par le cloud, pas une
  différence : un champ absent garde la valeur des fichiers du site. Une liste
  envoyée (`contacts`, `closing`) remplace celle des fichiers.
- Limites : délais de 10 à 300 s (sortie) et de 10 à 180 s (entrée), 5 contacts au
  plus, un propriétaire (`owner`) au moins. Rôles : `owner`, `deputy`, `guard`,
  `manager`, `other`. `sms` et `call` demandent un numéro au format international
  (`+228...`). `max_s` ne dépasse pas la plus longue session vidéo.
- `quota_mb` et `max_s` ne valent qu'en données mobiles ; en liaison fixe, le site les
  ignore.
- La centrale vérifie la configuration avec ses fichiers, la garde, puis redémarre
  pour l'appliquer, **jamais pendant un délai ni une alarme** : elle attend que
  l'alarme soit désarmée ou armée. Elle répond `applied` une fois qu'elle tourne avec,
  ou `rejected` tout de suite, avec la raison (version plus ancienne que celle en
  service, valeur hors limites, pas de propriétaire…).
- Le cloud peut renvoyer la même version : la centrale répond de nouveau `applied` si
  c'est celle en service, et ne fait rien si elle attend déjà le moment calme.
- Les numéros de téléphone n'apparaissent jamais dans un journal ni dans
  `config.applied`.

### `system.heartbeat`

```json
{
  "type": "system.heartbeat",
  "data": {
    "boot_id": "01JB8Z0000000000000000",
    "uptime_s": 86412,
    "engine": "armed",
    "queue_depth": 0,
    "fw": { "engine": "0.1.0", "adapter": "0.1.0" },
    "clock_ok": true
  }
}
```

Publié toutes les minutes par défaut. Le cloud considère un site muet après trois
heartbeats manquants. L'intervalle est réglable par site. En mode économe (`economy`
dans `agent.status`), l'agent n'en fait suivre qu'un toutes les 5 minutes : le plus
récent.

`clock_ok` dit si l'horloge de la centrale est synchronisée (NTP). Après une coupure de
courant sans internet, elle est fausse jusqu'à la synchronisation : à `false`, le cloud
se fie à sa propre heure de réception plutôt qu'à `ts`. Absent quand la centrale ne
sait pas le dire.

### `system.health`

Retenu, publié à chaque changement :

```json
{
  "type": "system.health",
  "data": {
    "mains": "lost",
    "hub_battery": 82,
    "links": { "zigbee": "ok", "gsm": "ok", "internet": "down" },
    "gsm": {
      "lines": [
        { "id": "line1", "operator": "togocom", "responding": true, "registered": true,
          "signal": 71, "credit": "ok", "last_test": "2026-10-17T08:00:00Z",
          "last_test_ok": true }
      ]
    }
  }
}
```

Le lien `gsm` et les lignes viennent du message `gsm.status` de la passerelle. Le lien
`gsm` vaut `ok`, `down` (la passerelle dit qu'elle ne peut pas envoyer) ou `silent`
(elle ne donne plus de nouvelles depuis `gsm_silent_after`, 3 minutes par défaut). Il
est absent tant que la passerelle n'a jamais parlé (site sans GSM).

Le lien `internet` vient de la même façon du message `agent.status` de l'agent de
liaison : `ok`, `down` (l'agent n'atteint pas le cloud) ou `silent` (l'agent ne donne
plus de nouvelles depuis `internet_silent_after`, 3 minutes par défaut). Personne n'est
prévenu d'une coupure d'internet : l'escalade passe simplement aux canaux GSM sans
attendre le cloud.

### `agent.status`

Publié retenu par l'agent de liaison sur `agent/status`, sur le broker local et vers
le cloud, à chaque changement et de nouveau chaque minute même sans changement.

```json
{
  "type": "agent.status",
  "data": { "uplink": "up", "transport": "mqtts", "queued": 0, "oldest_s": 0, "dropped": 0 }
}
```

- `uplink` : `up` (connecté au cloud) ou `down` ; `transport` : `mqtts` (MQTT sur TLS,
  port 8883) ou `wss` (MQTT dans WebSocket, port 443), absent quand la liaison est
  coupée.
- `queued` : messages qui attendent le cloud ; `oldest_s` : âge du plus ancien, en
  secondes ; `dropped` : messages supprimés depuis le démarrage parce que la file était
  pleine.
- `economy` : présent et à `true` quand le client paie les données mobiles (liaison 4G).
  Le cloud ne reçoit alors le heartbeat et cet état que toutes les 5 minutes, au lieu
  de chaque minute. Il ne déclare donc le site muet qu'après 15 minutes sans nouvelles,
  au lieu de 3. Un changement de liaison (`uplink` ou `transport`) part tout de suite.
  Sur la centrale, rien ne change : l'état y est publié chaque minute.
- Le même message, avec `uplink` à `down` et `seq` à 0, est le « dernier message »
  (Last Will) que le broker du cloud publie si la connexion se coupe sans prévenir.
  L'agent le publie lui-même quand il s'arrête proprement.

### `gsm.status`

Publié retenu par la passerelle GSM sur `gsm/status`, à chaque changement et de
nouveau chaque minute même sans changement : c'est ainsi que la centrale sait que la
passerelle vit. Son `ts` est l'heure à laquelle la passerelle l'a écrit.

```json
{
  "type": "gsm.status",
  "data": {
    "lines": [
      { "id": "line1", "operator": "TOGOCOM", "responding": true, "registered": true,
        "signal": 70, "credit": "ok", "restarts": 0 }
    ],
    "queued": 0
  }
}
```

- `responding` : le modem répond ; `registered` : il est sur le réseau ; `signal` : 0 à
  100, arrondi à la dizaine ; `credit` : `ok`, `empty` (un envoi a été refusé faute de
  crédit) ou absent si inconnu ; `restarts` : redémarrages du modem.
- `queued` : SMS et appels en attente.

## Fiabilité

- **Dédoublonnage** : tout récepteur garde les `id` récents et ignore les
  doublons.
- **Ordre et trous** : `seq` croît de façon continue par site. Si le cloud voit un
  trou après une reconnexion, il sait qu'il manque des messages et les redemande.
- **File d'attente** : l'agent de liaison conserve les messages sur disque quand
  internet est coupé et les renvoie dans l'ordre à la reconnexion.
- **Les alertes locales passent d'abord** : le moteur, la sirène et la passerelle
  GSM ne dépendent jamais du cloud. Ce qui part vers le cloud est de la
  supervision.

## Évolution du contrat

- Ajouter un champ ou un type de message : pas de changement de version.
- Changer le sens ou supprimer un champ : nouvelle valeur de `v`. Pendant une
  migration, le cloud accepte les deux versions.
- Un composant qui reçoit un `v` plus grand que le sien l'ignore et le signale
  dans les journaux.

## Prévu pour la phase fermée

À réserver dès maintenant, à implémenter plus tard :

- `sig` et `kid` dans l'enveloppe, pour signer les messages.
- Un identifiant matériel unique (`did`) pour chaque appareil fabriqué par nous.
- Une règle de sérialisation canonique du JSON avant signature.

## Ce qui part vers le cloud

L'agent de liaison transmet seulement :

- les événements `alarm.*` et `alert.*`,
- `alarm.state` et `system.health`,
- le `system.heartbeat`,
- les `device.event` de type `smoke`, `panic`, `tamper`, `mains` et les
  changements utiles de `battery`,
- son propre `agent.status`,
- `video.session` et `video.status` (voir plus haut). Le flux vidéo lui-même ne passe
  jamais par MQTT : il va directement au relais du cloud ;
- `config.applied`, la réponse à une configuration ;
- `maint.session`, les étapes d'une maintenance à distance ;
- `install.mode`, l'ouverture et la fermeture du mode installation ;
- `photo.taken` et `photo.sent`, après les messages d'alarme. Les images elles-mêmes
  partent en HTTPS, une à la fois, quand plus rien d'une alarme n'attend.

Dans l'autre sens, le cloud peut publier seulement :

- sur `alarm/cmd`, les ordres de l'application (`origin` à `app`) ;
- sur `video/cmd`, les sessions vidéo ;
- sur `alert/result` et `alert/ack`, les résultats et les acquittements des canaux du
  cloud (application, WhatsApp), pour une alerte que l'agent lui a transmise ;
- sur `maint/cmd`, l'ouverture et la fermeture d'un tunnel de maintenance (durée
  bornée à une heure, ordre périmé refusé) ;
- sur `photo/cmd`, l'adresse d'envoi d'une photo que l'agent a annoncée (gardée par
  l'agent, jamais publiée sur la centrale) ;
- sur `config/set`, la configuration du site. L'agent la vérifie avant de la
  transmettre (version, limites, numéros) : une configuration invalide n'atteint pas
  la centrale.

L'agent refuse tout le reste. Les commandes d'appareils (`siren.on`…) ne sont pas
accessibles au cloud : seul le moteur les envoie.

Les événements `keypad` ne partent jamais vers le cloud, car ils portent le code.

Les événements bruts des capteurs (`contact`, `motion`, `vibration`,
`glass_break`, `link`) restent sur le site. Ils ne partent que dans le contexte
d'une alarme (cause du déclenchement). Cela évite d'envoyer des données mobiles
pour des événements sans valeur de supervision.

## Décisions

1. **Format** : JSON pour la v1, avec JSON Schema.
2. **Cloud** : il reçoit les événements d'alarme et d'alerte, l'état, la santé
   et le heartbeat, pas le flux brut des capteurs.
3. **Heartbeat** : une minute par défaut, réglable par site.
4. **Armement** : un seul mode (tout armé) pour la v1. D'autres modes pourront
   s'ajouter sans changer de version, via un champ `mode` optionnel.

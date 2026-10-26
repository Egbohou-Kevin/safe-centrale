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
  `id`. Une exception, les messages qui portent le code d'accès, pour qu'il n'attende
  jamais dans un broker : sur le broker du cloud, l'agent de liaison lit `alarm/cmd` en
  QoS 0 (voir « Qualité de service sur le broker du cloud ») ; sur celui de la
  centrale, l'agent republie ces ordres en QoS 0, et l'adaptateur Zigbee publie en
  QoS 0 un `device.event` de clavier qui porte un code (voir `systeme.md`, étape 6).
- **Tolérant aux champs inconnus** : un récepteur ignore ce qu'il ne connaît pas.
  Ajouter un champ ne casse rien.
- **Pas de secret ni de donnée personnelle** dans les événements. Seule exception :
  le code du site, présent dans les événements `keypad` et dans les ordres
  `alarm.disarm`, `alarm.silence` et `alarm.end`. Il ne circule que sur le broker local et sur
  la liaison TLS vers le site, n'attend jamais sur le broker du cloud (QoS 0, voir
  plus bas) ni, sauf un tronçon à corriger (`systeme.md`, « Limites connues »), sur
  celui de la centrale, n'est jamais écrit dans les journaux, n'est jamais renvoyé dans une
  réponse et n'est jamais transmis au cloud par l'agent de liaison dans un événement.

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
| `src` | Composant émetteur (`adapter.zigbee`, `engine`, `health`, `escalation`, `install`, `config`, `gsm`, `video`, `maint`, `console`, `agent.link`, `tool.install`). |
| `seq` | Sur le site, compteur croissant de chaque programme, conservé sur disque par blocs de 1000 (pour ménager la carte SD) : il peut sauter après un redémarrage, jamais reculer. La console et l'outil d'installation numérotent leurs ordres avec l'heure, en millisecondes. Vers le cloud, l'agent renumérote : continu par site, dans l'ordre d'envoi, pour repérer les trous (rejeu : `agent.replay`, voir `agent.md`). 0 : non numéroté (dernier message). |
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
| `.../alarm/cmd` | Ordre au moteur (armer, désarmer…), depuis l'application (seul le cloud y publie) | non |
| `.../alarm/ack` | Réponse du moteur à un ordre | non |
| `.../alert/request` | Demande d'envoi d'alerte (SMS, appel) de l'escalade vers les canaux | non |
| `.../alert/result` | Résultat d'un envoi (envoyé, échoué, acquitté) | non |
| `.../alert/ack` | Acquittement d'un contact (SMS ou application) | non |
| `.../alert/seen` | « Vu » d'un contact dans l'application, qui n'arrête rien (cloud vers site) | non |
| `.../registry/cmd` | Appairage et retrait d'appareils, **local seulement** | non |
| `.../registry/event` | Appareil ajouté, refusé ou retiré (la centrale oublie un appareil retiré) | non |
| `.../registry/devices` | Liste des appareils connus | oui |
| `.../config/set` | Nouvelle configuration du site, numérotée par version (cloud vers site) | non |
| `.../config/applied` | Version de configuration appliquée par le site, ou refusée | oui |
| `.../system/heartbeat` | Signe de vie périodique | non |
| `.../system/health` | Santé de la centrale (secteur, batterie, liaisons) | oui |
| `.../system/devices` | Santé de chaque appareil (en ligne, pile), pour la fiche Capteurs | oui |
| `.../system/contacts` | Qui une intrusion prévient, et quand, pour la liste des personnes prévenues | oui |
| `.../gsm/status` | Lignes de la passerelle GSM (modem, réseau, crédit, test) | oui |
| `.../zigbee/status` | État du réseau Zigbee (Zigbee2MQTT, appareils muets, appairage) | oui |
| `.../agent/status` | Liaison avec le cloud, publiée par l'agent de liaison | oui |
| `.../agent/cmd` | Ordre du cloud pour l'agent lui-même (`agent.replay`, rejeu de messages manqués) | non |
| `.../agent/replayed` | Résultat d'un rejeu, par l'agent (`agent.replayed`) | non |
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

### Qualité de service sur le broker du cloud

L'agent de liaison garde sa session sur le broker du cloud pendant un jour (session
MQTT 5 persistante) : pour un abonnement en QoS 1, le broker garde ce qui arrive pendant
que l'agent est absent et le lui remet à son retour. Mosquitto le garde en mémoire et
dans son fichier (`mosquitto.db`), même après l'expiration du message (la durée de vie
MQTT 5 limite la remise, pas la garde). Pour un abonnement en QoS 0, il ne garde rien.

| Topic (cloud vers site) | QoS de l'abonnement | Pourquoi |
|---|---|---|
| `alarm/cmd` | 0 | Porte le code d'accès (`alarm.disarm`, `alarm.silence`, `alarm.end`), que le cloud ne garde jamais : le broker ne doit pas le garder non plus. Un ordre ne vaut qu'une minute (`exp`) |
| `video/cmd` | 1 | Le jeton est celui du cloud, pour une session ; un `video.stop` vaut encore après une coupure, une demande périmée est refusée par l'agent |
| `photo/cmd` | 1 | Le jeton est celui du cloud, pour une photo ; l'adresse vaut jusqu'à son `exp` et la photo est une preuve de l'alarme |
| `config/set`, `maint/cmd`, `agent/cmd`, `alert/result`, `alert/ack`, `alert/seen` | 1 | Pas de secret ; ils valent encore au retour de l'agent (un `maint.open` périmé est refusé) |

Le cloud publie toujours en QoS 1 : le broker lui accuse réception, puis remet
l'ordre à l'agent en QoS 0 (la plus petite des deux). Le prix, accepté : un ordre donné
pendant que l'agent est absent, même brièvement (coupure d'internet, ou les quelque
45 s où le broker ne le sait pas encore parti), n'est jamais remis ni exécuté au retour.
Personne ne répond : le téléphone le voit `expired` (au bout de 70 s dans le cloud).
Redonner l'ordre une fois le site revenu est un geste de la personne.

**Exigence pour le broker du cloud** : `queue_qos0_messages` doit rester à `false`, la
valeur par défaut de Mosquitto. À `true`, Mosquitto garderait aussi pour une session
absente les messages remis en QoS 0, code compris, et tout ce qui précède tomberait. Le
cloud écrit ce réglage dans son `mosquitto.conf` plutôt que de compter sur la valeur
par défaut : le test de bout en bout de l'agent tourne sur un Mosquitto aux réglages
par défaut et ne prouve rien de la configuration du cloud. Côté centrale, un test
(`internal/agent`) vérifie que les topics lus en QoS 0 sont exactement ceux dont les
schémas ont un champ `code` (aujourd'hui `alarm/cmd`).

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
| `mains` | `present`, `lost` (de l'appareil `hub_device_id` de la centrale, publiée par `safehome-power` depuis le module batterie, jamais d'un profil Zigbee : voir `alimentation.md`) |
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
(voir `engine.md` et `adapter-zigbee.md`). Le nom part dans les événements d'alarme qui
nomment l'appareil (`device_name`, voir plus bas).

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
comme des `alarm.command` : la valeur est `arm`, `arm_now`, `disarm`, `silence` ou
`postpone`, et le champ `code` accompagne `disarm` et `silence`. La centrale traduit en
ordre au moteur (origine `keypad`). Le code n'est jamais recopié dans un message publié, ni dans les
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

L'état `availability` d'un appareil hors ligne porte aussi `heard` : l'heure de son
dernier message, à la minute, telle que Zigbee2MQTT l'a entendu (`last_seen`, jamais
plus tard que l'arrivée du message : un message gardé par le broker pendant un arrêt
de l'adaptateur n'est pas une nouvelle plus récente), absente s'il n'a rien dit depuis
le redémarrage de l'adaptateur (son appairage compte comme un message). Jamais pour un
appareil en ligne, ni pour une autre capacité (le schéma le refuse, la centrale ignore
un tel état) : l'heure du dernier message d'un appareil qui répond bouge avec chaque
porte et chaque passage. Figée tant que l'appareil est hors ligne, elle dit depuis
quand sa porte n'est plus surveillée. La centrale s'en sert pour `system.devices`.

```json
{
  "type": "device.state",
  "data": {
    "device_id": "dev_back", "capability": "availability", "value": "offline",
    "since": "2026-10-24T18:47:00.000Z", "heard": "2026-10-24T14:47:00.000Z"
  }
}
```

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
    "status": "done"
  }
}
```

- `ref` est l'`id` de la commande.
- `status` : `accepted`, `done`, `failed`, `expired`.
- `reason` est absent quand il est vide, jamais `null`.
- Une commande périmée (`exp` dépassée) n'est pas exécutée.
- Les commandes sont idempotentes : recevoir deux fois `siren.on` ne change rien.
- Un `siren.on` répondu `failed` ou `expired` pendant que l'alarme sonne est renvoyé
  par la centrale ; si la sirène refuse encore, les contacts reçoivent l'alerte
  `siren_failed`, sous un incident à elle qui nomme l'alarme (`alarm_incident`), et la
  centrale publie `alarm.siren_failed` pour l'historique (voir « Événements d'alarme »,
  « Alertes » et `adapter-zigbee.md`). Un `done` à un `siren.on` d'un cycle suivant la
  fait revenir : un feu levé ensuite ne la dit plus muette. Ce `done` dit que la sirène
  a rapporté l'état demandé pour un profil avec `confirm` ; sans `confirm` (la seule
  sirène livrée), il dit seulement que l'ordre est parti.

Commandes de la v1 : `siren.on`, `siren.off`. `device.identify` (faire clignoter
ou biper un appareil pour le repérer) est réservée : rien ne l'envoie, aucun profil ne
la prend en charge.

### Ordres au moteur (`alarm.command` et `alarm.ack`)

Les ordres arrivent sur `alarm/cmd`, publiés seulement par le cloud (l'application).
Une touche du clavier devient un ordre dans la centrale, sans passer par ce topic ;
le planning arme dans le moteur.

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
| `alarm.end` | Termine l'alarme sans désarmer : celle qui sonne, et celles de l'armement dont la sirène s'est arrêtée seule (« Terminer l'alarme ») | `code` |
| `alarm.bypass` | Contourne des zones pour la période d'armement | `devices` (liste) |
| `alarm.postpone` | Reporte l'armement automatique | |

L'agent ne laisse passer du cloud que `alarm.arm`, `alarm.arm_now`, `alarm.disarm`,
`alarm.silence` et `alarm.end` : `alarm.bypass` et `alarm.postpone`, sans code, sont
pour la centrale elle-même, jamais de loin. Le moteur ne contourne que pendant le délai
de sortie, une porte ou une fenêtre (zone `entry` ou `instant`) ouverte à ce moment ;
sinon l'ordre est refusé entier (`wrong_state`, `detail.devices` les appareils en
cause). Chaque contournement lève une alerte `arming_bypassed` qui liste les appareils.

- `code` : 4 à 8 chiffres ASCII, rien d'autre (`pattern` `^[0-9]{4,8}$` du schéma),
  la forme des codes de la centrale (`security-install.md`). Un code d'une autre forme
  n'est celui de personne : la centrale le compterait comme faux. Un test de la centrale
  lie le schéma à sa règle (`access.Config.WellFormed`).
- `origin` : `keypad`, `app`, `schedule`. Il n'y a pas de SMS dans la v1.
- `user` : identifiant de la personne, quand il est connu (application).
- Le moteur répond sur `alarm/ack`.
- Un ordre sans `exp` vaut une minute après son `ts` (un `ts` en avance sur l'horloge
  de la centrale compte pour maintenant). Les touches du clavier aussi : une touche
  gardée par le broker pendant une coupure répond `expired`. Un ordre périmé n'est pas
  vérifié : son code ne compte pas comme faux.
- L'âge d'un ordre se mesure à l'horloge de la centrale, celle qui date les messages
  du site : après un recul de l'horloge, un ordre récent reste valable, qu'il porte son
  `exp` (l'application) ou non (le clavier). L'heure d'une
  touche du clavier (le `ts` de son `device.event`) est celle où Zigbee2MQTT l'a entendue (`last_seen`) : une touche
  gardée par le broker pendant une coupure de l'adaptateur expire aussi.

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
  `postpone_limit`, `rate_limited`. Une commande inconnue répond `failed` avec la raison
  `unknown_command`.
- L'agent de liaison répond lui-même, tout de suite, à un ordre de l'application qu'il
  ne transmet pas à la centrale, pour que le cloud n'attende pas une réponse qui ne
  viendrait pas : `expired` (sans raison, comme le moteur) pour un ordre arrivé après son
  `exp`, `refused` avec `rate_limited` pour un ordre au-delà de sa limite de messages du
  cloud (voir `agent.md`). `ref` est l'`id` de l'ordre, `src` est `agent.link`. Il ne
  répond qu'à un ordre qui a passé ses contrôles (topic et enveloppe du site, `origin` à
  `app`) ; les autres restent refusés sans réponse. Cette réponse part au cloud
  seulement : la centrale n'a jamais eu l'ordre.
- Les accusés ne contiennent jamais le code.
- L'agent lit `alarm/cmd` en QoS 0 sur le broker du cloud : un ordre donné pendant son
  absence n'est pas gardé pour lui et ne s'exécute pas à son retour ; il reste sans
  réponse, `expired` pour le cloud (voir « Qualité de service sur le broker du cloud »).
- Un armement accepté alors que des appareils sont muets ou ont une pile faible répond
  `done` avec `detail.offline` et `detail.low_battery` (listes d'appareils) : le clavier
  peut avertir. Ces défauts ne bloquent jamais l'armement.
- Le code installateur suivi de « désarmer » au clavier répond `done` avec
  `detail.install` à `open` quand il ouvre le mode installation (voir
  `security-install.md`).

### Événements d'alarme (`alarm.*`)

| Type | Quand | Contenu |
|---|---|---|
| `alarm.arming` | Le délai de sortie démarre | `origin`, `user`, `ends_at`, `forced`, `bypassed` (appareils contournés) ; `offline`, `low_battery` si des appareils sont muets ou ont une pile faible (avertissement) |
| `alarm.armed` | Le délai est terminé, ou `arm_now` | `origin`, `user` ; `mode` optionnel, absent en v1 |
| `alarm.disarmed` | Le système est désarmé | `origin`, `user`, `incidents` (les incidents que ce désarmement termine, liste vide s'il n'en termine aucun) |
| `alarm.entry_delay` | Un détecteur d'entrée s'est ouvert | `ends_at`, `device_id`, `device_name` |
| `alarm.triggered` | L'alarme se déclenche | `incident`, cause : `device_id`, `device_name`, `capability`, `zone` |
| `alarm.siren_cycle` | Un cycle de sirène démarre ; jamais sur un site sans sirène | `incident`, `cycle`, `of` |
| `alarm.siren_failed` | Une sirène refuse de sonner pendant une alarme, renvois compris (une fois par sirène et par alarme) | `incident` (l'alarme), `device_id` (la sirène) |
| `alarm.cleared` | L'alarme est arrêtée | `incident`, `origin` (`user` ou `timeout`), `user`, `duration_s` ; `ended` (vrai) quand une personne l'a terminée avec `alarm.end` |
| `alarm.arming_scheduled` | Annonce de l'armement automatique | `scheduled_for` |
| `alarm.postponed` | L'armement automatique est reporté | `scheduled_for`, `count`, `max` |
| `alarm.auth_failed` | Code faux | `origin`, `attempts` (codes faux des 10 dernières minutes, celui-ci compris, clavier et application ensemble), `locked_until` (si verrouillé) |

`device_name` (facultatif) est le nom que la configuration du site donne à l'appareil
(« Porte d'entrée »), 32 caractères au plus (un nom plus long est coupé). Il est absent
quand l'appareil n'a pas de nom (pas de zone : le sabotage d'un clavier ou d'une sirène ;
le nom d'une sirène, `siren_names`, ne sert qu'aux textes des alertes).
La configuration nomme un appareil par sa zone : dans `alarm.triggered`, `zone` porte
aujourd'hui le même nom, mais c'est `device_name` qui dit l'appareil. Les listes
d'appareils (`bypassed`, `offline`, `low_battery` de `alarm.arming`) restent des
`device_id`.

`alarm.siren_failed` va à l'historique : l'alarme `incident` sonne sans cette sirène,
le magasin est peut-être silencieux. Il ne termine aucun incident et ne fait sonner
aucun téléphone : l'alerte aux contacts est l'`alert.request` `siren_failed`, sous un
incident à elle, qui nomme l'alarme dans `alarm_incident` (voir « Alertes »). Il ne
porte pas de `device_name` : le nom d'une sirène dans la configuration d'installation
(`siren_names`) ne sert aujourd'hui qu'au texte des alertes. Après un redémarrage de la
centrale en pleine alarme, l'événement peut repartir une fois : un lecteur range les
pannes d'une alarme par sirène.

`incidents` d'`alarm.disarmed` (toujours présent) nomme les incidents que le
désarmement termine : l'alarme qui sonne (dont l'`alarm.cleared` d'origine `user` part
juste avant), et les intrusions et sabotages de l'armement dont la sirène s'est déjà
arrêtée seule (leur `alarm.cleared` disait `timeout`, leur escalade continuait jusqu'à
ce désarmement, voir `engine.md`). Un feu ou une panique dont la sirène s'est arrêtée
seule n'y est pas : leur escalade va jusqu'à l'acquittement. Ceux qui portent les
messages de l'alarme (la passerelle GSM) apprennent ainsi qu'ils n'ont plus de raison
de partir.

`alarm.state` (retenu) contient l'état courant : `disarmed`, `arming`, `armed`,
`entry_delay`, `triggered`. Il porte aussi `since`, et selon l'état :
`ends_at` (arming, entry_delay, triggered), `cycle` et `of` (triggered),
`incident` et `siren` (triggered), `locked_until` (si le désarmement est
verrouillé), `next_scheduled` (prochain armement automatique).

`since` est l'heure d'entrée dans l'état, à une exception près : en `armed`, c'est
l'heure de l'armement (la fin du délai de sortie), même après un délai d'entrée ou une
alarme qui ont laissé le local armé. Une alarme finie seule (son programme épuisé) ou
coupée au code republie `armed` avec le `since` de l'armement, pas l'heure de sa fin.
Corrigé le 26 octobre 2026 après l'essai sur la vraie centrale (« armée depuis 10:33 »
pour un armement à 10:30) ; aucun champ ne change.

En `triggered`, l'état dit où en est l'alarme qui sonne : `incident` (l'incident qui
sonne), `siren` (`on` pendant un cycle de sirène, `paused` pendant la pause entre deux
cycles) et `ends_at` (la fin du cycle en cours si `siren` est `on`, la fin de la pause
si `paused`). `incident` est celui d'`alarm.triggered`, celui des `alert.request` de
cette alarme, et l'`alarm_incident` d'une `siren_failed` levée pendant elle (qui a son
propre `incident`). Un feu ou une panique levé pendant l'alarme a aussi son propre
incident, sans programme de sirène à lui tant qu'elle sonne : l'état garde celui de
l'alarme qui sonne. Quand elle finit, le feu sonne à son tour, sous son incident
(`alarm.triggered`, puis son état), si son détecteur n'est pas revenu au repos (voir
`engine.md`, « Une alarme pendant une autre »).

`incident` et `siren` ne viennent qu'en `triggered`. `ends_at` vient aussi en
`arming` et `entry_delay` (la fin du délai). En `triggered`, les trois vont ensemble
ou aucun ne vient (`allOf` dans le schéma) ; une centrale antérieure au 25 octobre
2026 n'en envoie aucun. Chaque étape du programme (fin d'un cycle, fin d'une pause)
republie `alarm.state` : l'application dit « La sirène sonne (2/3) » ou « En pause,
reprend à 16:20 » sans autre événement. `siren` est le programme du moteur, pas la
sirène elle-même : une sirène muette se dit par `alarm.siren_failed`, et `siren` reste
`on`. Sur un site sans sirène (`sirens` à 0 dans `system.devices`), le programme mesure
toujours la durée de l'alarme (sa fin, `alarm.cleared` d'origine `timeout`), mais rien
ne sonne : aucun `alarm.siren_cycle` ne part, et un lecteur ne dit jamais que la sirène
sonne. Les heures sont celles du programme : un redémarrage de la centrale en pleine
alarme ne les décale pas (l'échéance est sauvegardée avec l'état du moteur, voir
`engine.md`).

Un `ends_at` déjà passé, quel que soit l'état, veut dire que l'état retenu n'est plus à
jour. Le plus souvent, la centrale s'est tue depuis (coupure de courant, liaison
perdue) et c'est le dernier état qu'elle a dit. Pendant sa fenêtre de démarrage (2 s),
un événement d'appareil peut aussi republier l'état sauvegardé avant que le moteur
rejoue les étapes manquées pendant l'arrêt ; l'état à jour suit alors aussitôt. Un
lecteur ne le montre pas comme un compte à rebours en cours (« reprend à 16:20 » à
16:40) : il s'appuie sur la présence en ligne de la centrale et attend l'état suivant.

### Alertes (`alert.*`)

- `alert.request` : l'escalade demande à un canal d'envoyer un message à un contact
  (une demande par contact et par canal, voir `escalation.md`). Champs : `incident`,
  `kind` (`intrusion`, `fire`, `panic`, `tamper`, `auth_failed`,
  `arming_bypassed`, `battery_low_hub`, `mains_restored`, `device_offline`, `battery_low`, `gsm_degraded`, `zigbee_down`, `siren_failed`), `urgency` (`critical`, `high`, `low`),
  `incident_at`, `zone`, `wave`, `contact`, `channel` (`app`, `whatsapp`, `sms`, `call`) et `text`, écrit
  pour son canal : seul le texte d'un SMS dit comment acquitter (`Repondre 1 si vous
  gerez.`), l'application et WhatsApp ont leur propre bouton. Le texte d'un `fire` levé
  pendant une alarme dont une sirène est connue muette finit par une phrase comme
  `La sirene ne repond pas.` (avant la manière d'acquitter), sans autre champ (voir
  `escalation.md`, « Un feu dit la sirène muette »). `devices` (liste d'appareils, facultatif) est rempli quand l'alerte en concerne
  plusieurs, par exemple `arming_bypassed`, et `percent` (0 à 100) le niveau de
  batterie pour les alertes de batterie.
  `alarm_incident` (facultatif) nomme l'alarme en cours dont l'alerte fait partie,
  jamais l'incident de l'alerte elle-même : aujourd'hui, seule une `siren_failed` levée
  pendant une alarme le porte. Elle garde son propre `incident` (son acquittement, ses
  messages en attente à la passerelle GSM et son `incident_at` ne sont qu'à elle), et
  nomme l'alarme pour qu'un téléphone qui sonne déjà pour celle-ci l'y ajoute au lieu de
  sonner une deuxième fois. `alarm_handled` (facultatif, présent seulement à `true` et
  seulement avec `alarm_incident`) dit que cette alarme est déjà prise en charge : un
  contact l'a acquittée, ou une personne l'a arrêtée avec un code (désarmement, sirène
  coupée). L'alerte se dit alors sans faire sonner : jamais d'appel, pas de sonnerie
  dans l'application, pas de son sur un iPhone. Son `urgency` vaut alors `low`, ce qui
  le décrit sans rien décider : seul `alarm_handled` décide du bruit. Tant qu'un feu ou
  une panique du site attend encore une réponse, la centrale ne le met pas, même si
  l'alarme est prise en charge : l'alerte reste bruyante (voir `escalation.md`, « Sirène
  muette pendant une alarme »).
  La fin de l'alarme par une personne termine aussi, pour les téléphones, les alertes
  qui la nomment dans `alarm_incident` (décidé le 26 octobre 2026) : son `alarm.cleared`
  d'origine `user` (sirène coupée, ou `ended` avec `alarm.end`), ou l'`alarm.disarmed`
  qui la compte dans `incidents`. Le cloud dit leur fin aux téléphones qu'il a fait
  sonner pour elles, avec le même mot que pour l'alarme (désarmée, sirène coupée,
  terminée). La centrale, elle, ne les coupe pas : elle les rend discrètes, et leur
  escalade finit `informed` (voir `escalation.md`). Un acquittement de l'alarme ne les
  termine pas.
  `incident_at` (obligatoire) est l'heure où l'incident a été levé, en UTC à la
  milliseconde comme le `ts` de l'enveloppe : la même à chaque vague et à chaque
  relance de l'incident, alors que `ts` est l'heure de la demande (une deuxième vague
  part 2 minutes après l'intrusion). `zone` (obligatoire) est le fuseau horaire IANA du
  site, celui de la configuration de la centrale (`timezone`, `Africa/Lome`), `UTC`
  quand elle n'en donne pas. Avec les deux, un téléphone dans un autre fuseau peut dire
  l'heure de l'incident à l'heure du site. `wave` vaut 1 pour la première vague, puis
  croît (vagues suivantes, puis relances).
  `zone_name` (facultatif) est le nom de la zone de la cause tel que la configuration
  du site le donne (« Entrée », accents gardés), coupé à 32 caractères comme
  `device_name` : un téléphone ouvert depuis la notification dit la zone, même
  verrouillé. Il vient avec une intrusion, un incendie, une panique ou un sabotage
  levés par un seul appareil d'une zone ; il est absent sinon (plusieurs appareils, un
  appareil hors zone comme une sirène ou un clavier, une panne de la centrale, du GSM
  ou des piles). À ne pas confondre avec `zone`, le fuseau. Sans lui, l'alerte se dit
  comme avant. Le schéma ne le borne pas : un nom vide ou trop long ne doit jamais
  faire refuser l'alerte, celui qui le lit le coupe ou l'ignore.
- `alert.result` : le canal répond. Champs : `ref` (l'`id` du `alert.request`),
  `status` (`queued`, `sent`, `delivered`, `failed`) et `reason` en cas d'échec
  (`no_network`, `no_credit`, `modem_error`, `timeout`, `busy`, `unreachable`,
  `no_uplink`, `dropped`). `dropped` vient de la passerelle GSM : l'incident a pris fin (une
  personne a arrêté l'alarme, un contact la gère) avant que le SMS ou l'appel parte, il n'est
  donc pas parti ; l'escalade, qui a clos l'incident, n'en fait rien. Pour `app` et `whatsapp`, `sent` veut dire que le cloud a pris le
  message et `delivered` qu'il est arrivé sur le téléphone. `no_uplink` vient de
  l'agent de liaison : internet est coupé, ou le cloud n'a pas confirmé en 10
  secondes. Le moteur s'en sert pour décider d'escalader. Pour `app` et `whatsapp`,
  le cloud a ses propres raisons : `no_contact`, `no_app`, `no_phone`,
  `not_on_whatsapp`, `whatsapp_off`, `whatsapp_refused`, `undelivered`,
  `too_late`, `cloud_busy` (trop d'alertes en attente) et `cloud_error` (une panne
  du cloud pendant l'envoi). Le moteur ne lit pas la raison : quand tous les
  messages internet d'un tour ont échoué, le GSM part sans attendre les 10
  secondes.
- `alert.ack` : un contact acquitte. Champs : `incident`, `contact`, `via` (`sms`
  ou `app`). Il arrête l'escalade vers les autres contacts.
- `alert.seen` : un contact a touché « Vu » sur l'écran de l'alarme, dans
  l'application. Publié par le cloud seulement, sur `alert/seen`. Champs : `incident`,
  `contact` et `at` (quand la personne l'a dit, à l'heure du cloud, UTC à la
  milliseconde : le message peut attendre le retour de la liaison), tous obligatoires.
  Il **n'arrête rien** : l'escalade continue pour les autres contacts. Elle n'envoie
  plus rien à ce contact pour cet incident (ni le doublon GSM, ni les vagues suivantes,
  ni les relances), et une vague qui ne compte plus que des contacts qui ont vu est
  sautée sans attendre (voir `escalation.md`, « Vu »). Pour arrêter l'escalade, le
  téléphone envoie ensuite `alert.ack` (« Je m'en occupe »). La centrale ne le redit
  pas au cloud : le cloud le tient de l'application et le montre lui-même aux autres
  contacts. L'agent ne le laisse passer que pour un incident qu'il a vu monter.
- La passerelle GSM (`safehome-gsm`) lit aussi `alert/ack` et `alarm/event` (les
  `alarm.cleared` d'origine `user`, et les `incidents` d'`alarm.disarmed`) : elle
  abandonne alors les messages de ces incidents encore en attente (voir
  `gsm-gateway.md`).

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
| `registry.pair_failed` | Le réseau n'a pas pu être ouvert, la fenêtre est finie | `ref`, `reason` |

- `registry.pair` : `duration_s` à 0 referme le réseau. L'adaptateur borne la durée
  (10 minutes par défaut) et l'obtient de Zigbee2MQTT par morceaux de 254 secondes au
  plus.
- `registry.pair` et `registry.remove` écrits il y a plus d'une minute (`ts`) sont
  ignorés : l'outil d'installation qui les a envoyés a abandonné.
- `registry.pair_failed` : Zigbee2MQTT a refusé d'ouvrir le réseau (ou de le garder
  ouvert) pour l'ordre `ref`, n'a pas répondu à la demande, ou est passé hors ligne
  pendant la fenêtre ; `reason` reprend ses mots, ou dit ce qui s'est passé. La fenêtre est finie
  (`zigbee.status` repasse `pairing` à faux) : l'outil d'installation s'arrête en
  erreur.
- `registry.device_added`, `registry.device_rejected` et `registry.device_removed`
  portent `ref`, l'`id` de l'ordre qui les a causés (absent pour un appareil refusé
  hors appairage).
- `registry.device_rejected` : `reason` vaut `unknown_model` (pas de profil),
  `not_pairing` (entré hors d'une fenêtre d'appairage, ou appareil hors du registre
  qui parle dans le réseau, sans `ref` ni modèle) ou `interview_failed` (modèle
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

- `bridge` : `online` quand Zigbee2MQTT et sa clé tournent, `offline` sinon, et au
  démarrage de l'adaptateur tant que Zigbee2MQTT n'a pas prouvé qu'il tourne (voir
  `adapter-zigbee.md`, « État de la liaison »).
- `devices` : appareils enregistrés ; `offline` : ceux qui ne répondent plus ;
  `pairing` : le réseau est ouvert aux nouveaux appareils, Zigbee2MQTT l'a confirmé,
  ou un appareil entré pendant la fenêtre est encore interrogé (3 minutes au plus).
- La centrale en déduit le lien `zigbee` de `system.health` : `ok`, `down`, ou
  `silent` si l'adaptateur ne donne plus de nouvelles. Après 3 minutes de panne, le
  responsable (sinon le propriétaire) reçoit l'alerte `zigbee_down` : l'alarme
  n'entend plus ses capteurs.

### Vidéo en direct (`video.*`)

Le détail et les raisons des choix sont dans `video.md`.

| Type | Topic | Contenu |
|---|---|---|
| `video.request` | `video/cmd` | `session`, `camera`, `reason` (`alarm`, `demand`), `user`, `incident`, `max_s` (absent ou 0 : sans limite), `push_url` (rtmps), `token`, `exp` |
| `video.stop` | `video/cmd` | `session` |
| `video.session` | `video/session` | `session`, `camera`, `status` (`starting`, `live`, `ended`, `failed`), `reason`, `duration_s`, `bytes` |
| `video.status` | `video/status` (retenu, au démarrage et à chaque changement) | `cameras` (`id`, `name`, `reachable`), `active`, `uplink` (`fixed`, `mobile`), `used_mb_month`, `quota_mb`, `max_s` |

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
  adresse `rtmps://` sans requête ni identifiants, dont l'hôte est un nom, jamais une
  adresse IP (go2rtc ne vérifierait pas le certificat du relais, voir `video.md`).
- En liaison fixe (`uplink` = `fixed`), ni plafond ni durée limite : `max_s` est absent.
  En données mobiles, le client règle `quota_mb` et `max_s`, que le cloud reprend dans
  chaque demande.
- Une demande déjà périmée à son arrivée est refusée par l'agent, sans réponse ; si
  elle expire avant d'atteindre le service vidéo, il répond `failed`/`expired`.
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
- `url_expired` : l'adresse a expiré avant l'envoi ou a été refusée (401, 403, 404,
  410). Un `photo.upload` déjà périmé à son arrivée est refusé par l'agent, sans
  réponse. Le cloud peut envoyer un nouveau `photo.upload`.
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
- Dans la centrale, `configapp` suit aussi `install/mode` : la centrale ne redémarre
  pas pour une configuration tant que le mode est ouvert (voir `operations.md`).

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
  envoyée (`contacts`, `closing`) remplace celle des fichiers : `"closing": {}`
  retire toute mise en service automatique, y compris après un redémarrage.
  `"contacts": []` est refusé (`config.applied` `rejected`) : personne ne serait plus
  prévenu d'une alarme.
- Limites : délais de 10 à 300 s (sortie) et de 10 à 180 s (entrée), 5 contacts au
  plus et 1 au moins, dont un propriétaire (`owner`). Seul le fichier du site peut
  n'avoir aucun contact (personne n'est alors prévenu d'une alarme, `check` le signale). Rôles : `owner`, `deputy`, `guard`,
  `manager`, `other`. `sms` et `call` demandent un numéro au format international
  (`+228...`). `quota_mb` de 0 à 8 388 608. `max_s` ne dépasse pas la plus longue
  session vidéo.
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

Retenu, publié à chaque changement (les statuts répétés des services ne le republient
pas) :

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
          "signal": 70, "credit": "ok", "last_test": "2026-10-17T08:00:00Z",
          "last_test_ok": true }
      ]
    }
  }
}
```

Le lien `gsm` et les lignes viennent du message `gsm.status` de la passerelle. Le lien
`gsm` vaut `ok`, `down` (la passerelle dit qu'elle ne peut pas envoyer) ou `silent`
(elle ne donne plus de nouvelles depuis `gsm_silent_after`, 3 minutes par défaut). Il
vaut `none` sur une centrale **sans ligne SMS par choix** (`without_gsm` dans
`hub.json`, voir `operations.md`) : elle ne surveille aucune passerelle et ne demande
jamais de SMS ni d'appel, et `gsm` (les lignes) est absent. Sinon, il est absent tant
que la passerelle n'a jamais parlé : la ligne est installée, mais la centrale n'en a
pas encore de nouvelles (elle démarre, ou la passerelle n'a jamais démarré). Un lecteur
distingue donc `none` (pas de SMS, c'est voulu) de `down` et `silent` (la ligne est en
panne). Avant le 25 octobre 2026, une centrale sans GSM n'envoyait aucun lien `gsm`,
comme une passerelle pas encore entendue.

Le lien `internet` vient de la même façon du message `agent.status` de l'agent de
liaison : `ok`, `down` (l'agent n'atteint pas le cloud) ou `silent` (l'agent ne donne
plus de nouvelles depuis `internet_silent_after`, 3 minutes par défaut). Personne n'est
prévenu d'une coupure d'internet : l'escalade passe simplement aux canaux GSM sans
attendre le cloud.

### `system.contacts`

Publié par la centrale (`src` : `escalation`), retenu sur `system/contacts`, à son démarrage :
les contacts du site dans l'ordre où le plan d'escalade d'une intrusion les prévient, le plan
que la centrale suit vraiment (celui de `hub.json`, ou par défaut), pour la liste des
personnes prévenues de l'application. Jamais un numéro de téléphone.

```json
{
  "type": "system.contacts",
  "data": {
    "contacts": [
      { "id": "u_ama", "name": "Ama", "role": "deputy", "channels": ["app", "sms"], "told_after_s": 0 },
      { "id": "u_kofi", "name": "Kofi", "role": "guard", "channels": ["sms", "call"], "told_after_s": 0 },
      { "id": "u_kevin", "name": "Kevin", "role": "owner", "channels": ["app"], "told_after_s": 120 },
      { "id": "u_yao", "role": "manager", "channels": ["whatsapp"], "told_after_s": 300 }
    ]
  }
}
```

- `told_after_s` : les secondes après le déclenchement où une intrusion le prévient pour la
  première fois (une vague suivante qui le prévient encore est une relance) ; absent quand une
  intrusion ne le prévient jamais : il vient alors en dernier. À égalité, l'ordre de la
  configuration.
- `name` : absent quand la configuration n'en donne pas. `role` et `channels` : les mots de
  `config.set`. Sur une centrale sans ligne SMS (`without_gsm`), `channels` ne garde que
  `app` et `whatsapp`, les seuls qui partent : un contact qui n'acceptait que SMS et
  appel a une liste vide et pas de `told_after_s`, personne ne le prévient.
- Les contacts et les plans ne changent qu'avec la configuration du site, qui redémarre la
  centrale ; l'agent de liaison le redit au cloud à chaque connexion.

### `system.devices`

Publié par la centrale (`src` : `health`), retenu sur `system/devices` : la santé de
chaque appareil du registre, pour la fiche Capteurs de l'application.

```json
{
  "type": "system.devices",
  "data": {
    "devices": [
      { "id": "dev_back", "name": "Arrière-cour", "zone": "instant", "type": "contact",
        "online": false, "since": "2026-10-24T18:47:00.000Z",
        "heard": "2026-10-24T14:47:00.000Z", "battery": 60 },
      { "id": "dev_smoke", "name": "Réserve", "zone": "24h_fire", "type": "smoke",
        "online": true, "battery": 10, "battery_low": true },
      { "id": "dev_door", "name": "Entrée", "zone": "entry", "type": "contact",
        "online": true, "battery": 80 },
      { "id": "dev_siren1", "type": "siren", "online": true }
    ],
    "sirens": 1
  }
}
```

- `id` : l'identifiant du registre. Seuls les appareils du registre (`registry/devices`)
  sont dans la liste, rangés par `id` ; un site sans appareil envoie une liste vide.
- `name`, `zone` : le nom et le type de la zone de l'appareil dans la configuration du
  site (`zones` de `hub.json`), absents pour un appareil hors zone (une sirène, un
  clavier). `name` est coupé à 32 caractères, comme `device_name`. `zone` vaut `entry`,
  `instant`, `interior`, `24h_fire`, `24h_panic` ou `24h_tamper` : l'application les dit
  en mots simples, jamais tels quels.
- `type` : la première capacité de l'appareil (commandes comprises : une sirène a
  `siren`) dans cet ordre : `siren`, `keypad`, `smoke`, `contact`, `motion`,
  `glass_break`, `vibration`, `panic`. `tamper`, `battery` et `link` ne décident
  jamais. Un appareil dont aucune capacité ne décide (aucun profil aujourd'hui) n'est
  pas dans la liste : ce n'est ni un capteur, ni un clavier, ni une sirène.
- `online` : faux dès que l'adaptateur compte l'appareil hors ligne (un silence de
  `offline_after`, 4 h par défaut et 2 h pour la sirène, ou un appareil sorti du
  réseau). Seulement quand il est faux : `since`, quand il est devenu injoignable, et
  `heard`, sa dernière nouvelle, absente s'il n'a rien dit depuis le redémarrage de
  l'adaptateur. Les deux viennent de l'état `availability` retenu (voir
  « `device.state` ») et restent figés tant que l'appareil est injoignable. Ils partent
  à la minute parce que l'application n'en montre pas plus (« aucune nouvelle depuis
  14:47 ») : ce n'est pas une protection, voir plus bas ce que dit le `ts`.
- Pour l'application, aucune heure ne se déduit de l'autre : elle montre `heard`, ou
  `since` quand `heard` manque, et ne calcule jamais l'une à partir de l'autre et
  d'`offline_after`. Après un redémarrage de l'adaptateur, le silence compte depuis le
  redémarrage, et `heard` est l'heure de Zigbee2MQTT, pas celle de l'arrivée.
- Jamais d'heure pour un appareil en ligne (le schéma refuse `since` et `heard` avec
  `online` à `true`) : l'heure de son dernier message bouge avec chaque porte et chaque
  passage, et les Aqara ne se signalent d'elles-mêmes qu'environ toutes les 50 minutes.
- Ce que la liste dit de l'activité de la boutique : aucune heure pour un appareil qui
  répond, et aucun envoi pour une porte ou un passage en soi, donc rien qui suive
  l'activité au jour le jour. Mais le `ts` de l'enveloppe date à la milliseconde
  chaque liste partie sur un changement, et l'agent le garde quand il la redit. Celle
  qui annonce un appareil resté muet part `offline_after` après l'arrivée de son
  dernier message (ou le redémarrage de l'adaptateur) : son `ts` redonne cette heure à
  la seconde près, là où `heard` la dit à la minute. Celle qui annonce son retour part
  à son premier message, souvent une porte ; celle d'une pile qui change de dizaine
  part avec un message de l'appareil, qui peut en être une aussi. Quelques moments par
  appareil et par an, pas le fil de l'activité. Choix fait le 25 octobre 2026 : la
  liste part tout de suite, pour que l'application montre sans attendre un appareil
  revenu. La retenir ne cacherait rien : un événement `battery` ou `tamper` porté par
  le même message part au cloud tout de suite, et `alarm.armed` et `alarm.disarmed`
  disent déjà quand la boutique ferme et ouvre.
- `sirens` (depuis le 26 octobre 2026, toujours envoyé par la centrale, absent d'une
  plus ancienne) : le nombre de sirènes que la configuration du site nomme
  (`siren_ids`), appairées ou non. Une sirène nommée mais pas appairée n'est pas dans
  `devices` (elle n'est pas au registre) et compte ici. `0` : le site n'a pas de sirène,
  par choix (`"siren_ids": []` dans `hub.json`) ; ses alarmes ne font que prévenir, la
  centrale ne dit aucun `alarm.siren_cycle` ni aucune `siren_failed`, et le cloud et
  l'application ne disent jamais qu'une sirène sonne, ni qu'elle n'a pas sonné.
- `battery` : la dernière lecture en %, arrondie à la dizaine la plus proche (comme le
  signal GSM), absente sans pile suivie ou jamais lue. `battery_low` : présent, à
  `true`, seulement quand la pile est faible, avec la règle de la supervision (faible à
  15 % ou moins, bonne de nouveau au-dessus de 30 %).
- Jamais : l'état d'une porte ou d'un mouvement, l'état armé d'une zone, l'adresse
  Zigbee, la qualité de liaison (utile à l'installateur, qui la lit sur place).
- Taille : environ 100 octets par appareil, 600 à 900 octets pour un kit.
- Envoi : seulement sur un vrai changement (en ligne ou non, pile arrondie, pile
  faible, appareil ajouté ou retiré ; le nom et la zone changent avec la configuration,
  qui redémarre la centrale), une fois à chaque démarrage de la centrale, une fois les
  états retenus revenus (comme `system.health`), et de nouveau à chaque connexion de
  l'agent au cloud. Pas de renvoi périodique.
- La centrale suit la présence de chaque appareil par son état `availability` retenu,
  au démarrage et ensuite : l'événement ne dit ni `since` ni `heard`.

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
  secondes ; `dropped` : messages supprimés sans atteindre le cloud (file pleine,
  message trop vieux, refus du broker du cloud). Le compte survit aux redémarrages.
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
  crédit) ou absent si inconnu ; `restarts` : redémarrages du modem ; `last_test` et
  `last_test_ok` : heure et résultat du dernier SMS de test, absents avant le premier.
- `queued` : SMS et appels en attente.

## Fiabilité

- **Dédoublonnage** : tout récepteur garde les `id` récents et ignore les
  doublons.
- **Ordre et trous** : vers le cloud, `seq` croît de façon continue par site (l'agent
  renumérote). Si le cloud voit un trou après une reconnexion, il sait qu'il manque des
  messages et les redemande.
- **File d'attente** : l'agent de liaison conserve les messages sur disque quand
  internet est coupé et les renvoie à la reconnexion : alertes et alarmes d'abord, les
  états en dernier (seul le plus récent de chaque état est gardé). À priorité égale,
  l'ordre d'arrivée.
- **Les alertes locales passent d'abord** : le moteur, la sirène et la passerelle
  GSM ne dépendent jamais du cloud. Ce qui part vers le cloud est de la
  supervision.

## Évolution du contrat

- Ajouter un champ ou un type de message : pas de changement de version. Un nouveau
  type sur le chemin du cloud demande pourtant le cloud à jour avant la centrale : un
  cloud qui ne le connaît pas refuse chaque copie, et chaque copie refusée laisse en
  plus un trou dans les `seq` (voir « Fiabilité ») : il redemande ce numéro quelques
  fois, en vain, puis le compte perdu, à chaque copie.
- Changer le sens ou supprimer un champ : nouvelle valeur de `v`. Pendant une
  migration, le cloud accepte les deux versions.
- Un composant qui reçoit un `v` plus grand que le sien l'ignore et le signale
  dans les journaux.

Changements depuis la v1 validée, sans changement de `v` :

- **26 octobre 2026, la fin d'une alarme et le site sans sirène** (deuxième essai sur la
  vraie centrale, une sirène configurée jamais appairée) : la fin de l'alarme par une
  personne termine aussi, pour les téléphones, les alertes qui la nomment
  (`alarm_incident`, la sirène muette) : rien ne change dans les messages, le cloud la
  déduit de l'`alarm.cleared` ou de l'`alarm.disarmed` de l'alarme (voir « Alertes »).
  Un site sans sirène est valide (`"siren_ids": []`) : `system.devices` porte `sirens`
  (facultatif, le nombre de sirènes que la configuration nomme, appairées ou non ; `0`
  pour un site sans sirène), et un tel site ne publie aucun `alarm.siren_cycle`. Un
  cloud qui ne connaît pas `sirens` l'ignore.

- **25 octobre 2026, « Vu » et la centrale sans SMS** : nouveau message `alert.seen`
  (`incident`, `contact`, `at`), du cloud vers la centrale sur `alert/seen` : un contact
  a vu l'alarme dans l'application sans la prendre en charge (voir « Alertes »). Il
  descend du cloud seulement, et la centrale ne le redit pas : le cloud l'envoie
  quand il veut, une centrale qui ne le connaît pas encore le refuse sans dommage (son
  agent n'a pas la route : l'escalade continue comme avant, le contact reçoit encore le
  doublon). Le lien `gsm` de `system.health` vaut `none` sur une centrale sans ligne
  SMS par choix (`without_gsm`), qui ne demande plus aucun SMS ni appel (voir
  « `system.health` ») ; aucun schéma ne change pour cela (`links` est une table de
  mots). Un cloud qui ne connaît pas `none` le laisse de côté, comme avant le lien
  absent.

- **25 octobre 2026, les messages abandonnés** : la passerelle GSM publie un `alert.result`
  `failed`, raison `dropped`, pour chaque SMS ou appel qu'elle abandonne parce que son incident a
  pris fin : celui qui lit l'escalade ne le prend jamais pour envoyé (voir « Alertes »).
- **25 octobre 2026, les personnes prévenues** : nouveau message `system.contacts`, retenu sur
  `system/contacts` : qui une intrusion prévient, et quand, du plan que la centrale suit (voir
  « `system.contacts` »). Un cloud qui ne le connaît pas encore refuse chaque copie : il est mis
  à jour avant la centrale.
- **25 octobre 2026, « Terminer l'alarme »** : nouvel ordre `alarm.end` (avec `code`). Il termine
  l'alarme sans désarmer : celle qui sonne, comme `alarm.silence`, et celles de l'armement dont la
  sirène s'est arrêtée seule, que seul un désarmement terminait (voir « Ordres »). Chacune est dite
  par un `alarm.cleared` d'origine `user` qui porte `ended` (facultatif, vrai) ; pour une alarme
  arrêtée seule avant, `duration_s` vaut 0. Un cloud qui ne connaît pas `ended` l'ignore et dit la
  sirène coupée ; un cloud qui ne connaît pas `alarm.end` ne l'envoie pas.
- **25 octobre 2026, la zone dans l'alerte** : `alert.request` porte `zone_name`
  (facultatif), le nom de la zone de la cause (« Entrée »), pour que l'écran de
  l'alarme le dise même ouvert depuis la notification (voir « Alertes »). `zone` reste
  le fuseau. Un cloud qui ne le connaît pas encore l'ignore.
- **25 octobre 2026, la fiche Capteurs** : nouveau message `system.devices`, publié
  par la centrale (`src` `health`) et retenu sur `system/devices` : la santé de chaque
  appareil du registre (nom et type de sa zone, type d'appareil, en ligne ou non, pile
  arrondie à la dizaine, pile faible), pour la fiche Capteurs de l'application (voir
  « `system.devices` »). L'agent le relaie au cloud et le redit à chaque connexion, comme
  `system.health`. Jamais l'état d'une porte ni un mouvement, jamais une heure pour un
  appareil en ligne. L'état `availability` retenu d'un appareil hors ligne porte
  `heard`, l'heure de son dernier message à la minute (voir « `device.state` ») ; un
  `heard` sur un autre état est refusé. Un cloud qui ne connaît pas encore
  `system.devices` refuse chaque copie et y voit un trou dans les numéros (voir
  plus haut) : il est mis à jour avant la centrale.
- **25 octobre 2026, la sirène dans l'état** : `alarm.state` en `triggered` porte
  trois champs facultatifs, tous ou aucun : `incident` (l'incident qui sonne), `siren`
  (`on` pendant un cycle, `paused` pendant la pause) et `ends_at` (la fin du cycle ou
  de la pause en cours). `incident` et `siren` ne viennent jamais dans un autre état ;
  `ends_at` reste aussi la fin du délai en `arming` et `entry_delay`, comme avant
  (`allOf` dans le schéma, voir « Événements d'alarme »). Pour la carte d'alarme de
  l'application : « La sirène sonne (2/3) », « En pause, reprend à 16:20 ». Aucun
  nouvel événement : chaque étape du programme de la sirène republiait déjà
  `alarm.state`. Un `ends_at` déjà passé veut dire que l'état n'est plus à jour. Un
  cloud qui ne connaît pas encore ces champs les accepte (un champ inconnu est permis),
  et un cloud à jour accepte l'état d'une centrale antérieure, qui n'en porte aucun :
  l'ordre de mise à jour est libre, à condition que le cloud recopie `alarm-state.json`
  à l'octet et ne rende aucun de ces champs obligatoire.
- **24 octobre 2026, la sirène muette** : `alert.request` porte deux champs
  facultatifs, `alarm_incident` (l'alarme en cours dont l'alerte fait partie) et
  `alarm_handled` (cette alarme est déjà prise en charge : l'alerte se dit sans faire
  sonner), le second seulement avec le premier (`dependentRequired` dans le schéma, voir
  « Alertes »). Nouvel événement `alarm.siren_failed` (`incident`, `device_id`) sur
  `alarm/event`, relayé au cloud par l'agent, pour l'historique (voir « Événements
  d'alarme »). À l'essai, une sirène muette après l'acquittement d'une intrusion faisait
  sonner les téléphones une deuxième fois. Un cloud qui ne connaît pas encore
  `alarm.siren_failed` le refuse : il est mis à jour avant la centrale.
- **24 octobre 2026** : `alert.request` porte `incident_at` (l'heure de l'incident,
  la même à chaque vague) et `zone` (le fuseau IANA du site), tous deux obligatoires
  (voir « Alertes »). À l'essai, un téléphone à Paris montrait pour la deuxième vague
  d'une intrusion de 16:15 à Lomé « 18:17 » : l'heure de la demande, à l'heure du
  téléphone. `alarm.disarmed` porte `incidents`, obligatoire, les incidents que le
  désarmement termine (voir « Événements d'alarme ») : la passerelle GSM abandonne
  aussi les messages d'une intrusion dont la sirène s'était arrêtée seule avant le
  désarmement (avant, ils partaient encore). La centrale refuse `timezone` à `Local`,
  qui n'est pas un nom IANA.
- **22 octobre 2026** : l'agent de liaison redit au cloud le dernier `alarm.state` et le
  dernier `system.health` à chaque connexion, avec un nouvel `id` (voir « Ce qui part
  vers le cloud »). `alarm.entry_delay` et `alarm.triggered` portent `device_name`,
  le nom de l'appareil, facultatif (voir « Événements d'alarme »). Le schéma
  d'`alarm.command` donne la forme du code : `pattern` `^[0-9]{4,8}$` sur `args.code`
  (voir « Ordres au moteur »). L'agent répond tout de suite par `alarm.ack` à un ordre
  de l'application qu'il ne transmet pas : `expired` pour un ordre périmé, `refused`
  avec la nouvelle raison `rate_limited` pour un ordre au-delà de sa limite de débit
  (avant, il les jetait sans répondre). Le verrouillage ne compte plus que les codes faux des
  10 dernières minutes, et `attempts` d'`alarm.auth_failed` les compte de même (avant :
  les codes faux de suite, sans limite de temps, voir `engine.md`). Une alerte
  `auth_failed` (`alert.request`) part aussi sans verrouillage, tous les 3 codes faux
  depuis le dernier bon code ; l'`alarm.auth_failed` du même code n'a alors pas de
  `locked_until` (aucun champ ni schéma ne change). L'agent lit
  `alarm/cmd` en QoS 0 sur le broker du cloud, qui ne garde donc plus pour un agent
  absent un ordre portant le code ; un ordre donné pendant une coupure n'est plus
  exécuté au retour, le téléphone le voit `expired` (voir « Qualité de service sur le
  broker du cloud »). Les autres topics descendants restent en QoS 1. Sur le broker de
  la centrale aussi, un ordre de l'application et un `device.event` de clavier qui porte
  un code passent en QoS 0 (voir « Principes »).

## Prévu pour la phase fermée

À réserver dès maintenant, à implémenter plus tard :

- `sig` et `kid` dans l'enveloppe, pour signer les messages.
- Un identifiant matériel unique (`did`) pour chaque appareil fabriqué par nous.
- Une règle de sérialisation canonique du JSON avant signature.

## Ce qui part vers le cloud

L'agent de liaison transmet seulement :

- les événements `alarm.*` et `alert.*`, et les réponses aux ordres (`alarm.ack`),
- `alarm.state`, `system.health` et `system.devices`, à chaque changement, et de
  nouveau à chaque connexion de l'agent au cloud (une fois par minute au plus) : le
  dernier de chacun, même contenu et même `ts`, avec un nouvel `id` (voir `agent.md`).
  Le cloud garde le plus récent par son `seq` ;
- le `system.heartbeat`,
- les `device.event` de type `smoke`, `panic`, `tamper`, `mains` et les
  changements utiles de `battery`,
- son propre `agent.status`, et `agent.replayed` après un rejeu demandé par le cloud,
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
- sur `agent/cmd`, les demandes de rejeu (`agent.replay`) pour l'agent lui-même,
  jamais publiées sur la centrale ;
- sur `alert/result` et `alert/ack`, les résultats et les acquittements des canaux du
  cloud (application, WhatsApp), pour une alerte que l'agent lui a transmise ;
- sur `alert/seen`, le « Vu » d'un contact dans l'application, pour un incident que
  l'agent lui a transmis ;
- sur `maint/cmd`, l'ouverture et la fermeture d'un tunnel de maintenance (durée
  bornée à une heure, ordre périmé refusé) ;
- sur `photo/cmd`, l'adresse d'envoi d'une photo que l'agent a annoncée (gardée par
  l'agent, jamais publiée sur la centrale) ;
- sur `config/set`, la configuration du site. L'agent n'en vérifie que la forme
  (version, limites, numéros) avant de la transmettre ; la centrale refuse une version
  plus ancienne, une liste sans propriétaire et des heures de fermeture fausses.

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

# Adaptateur Zigbee

Statut : v1 codée et testée sans matériel (voir « Ce qui est codé » à la fin), puis
essayée sur le vrai matériel le 24 octobre 2026 avec la clé et le détecteur d'ouverture
(voir « Constaté sur le matériel »). Les autres profils restent à vérifier sur les vrais
appareils.

L'adaptateur Zigbee est le programme en Go qui traduit entre Zigbee2MQTT et notre
contrat. C'est la seule partie de la centrale qui connaît le matériel du commerce
(Aqara, Heiman, Tuya). Le moteur d'alarme, la passerelle GSM et l'agent de liaison
ne voient jamais un message de Zigbee2MQTT. Le jour où nous fabriquons nos propres
capteurs, on écrit un autre adaptateur, ou un nouveau jeu de profils, et le reste ne
bouge pas (voir `architecture.md`).

Les noms de sujets et de champs de Zigbee2MQTT cités ici viennent de ma
connaissance de cet outil. Ils sont à vérifier sur la version installée, avec du
vrai matériel, avant d'être figés.

## Responsabilités

- Lire les messages de Zigbee2MQTT et publier des événements du contrat.
- Traduire les commandes du contrat (`siren.on`…) en commandes Zigbee2MQTT.
- Tenir le **registre des appareils** : identité, modèle, capacités.
- Conduire l'**appairage** et le retrait d'un appareil.
- Surveiller la présence de chaque appareil (hors ligne ou non).
- Signaler l'état de la liaison Zigbee dans `system.health`.

Il ne fait pas : décider d'une alarme (moteur), connaître les zones (configuration
du site, lue par le moteur), parler au cloud (agent).

## Entrées et sorties

| Entrée | Source |
|---|---|
| Messages d'appareils, liste des appareils, état du pont | Zigbee2MQTT (broker local) |
| `device/{id}/cmd` | moteur d'alarme |
| `registry/cmd` (appairer, retirer) | outil d'installation, local seulement |

| Sortie | Destination |
|---|---|
| `device/{id}/event` et `device/{id}/state/{capacité}` | moteur, agent |
| `device/{id}/ack` | moteur |
| `registry/devices` (retenu) et `registry/event` | moteur, outil d'installation |
| `zigbee/status` (retenu), d'où la centrale tire le lien « zigbee » de `system/health` | centrale, agent |

## Profils d'appareil

Chaque modèle d'appareil a un **profil** : un petit fichier de données qui dit
comment traduire. Ajouter un modèle, c'est ajouter un profil, sans toucher au code.
Les profils sont des fichiers JSON (pas de YAML : aucune bibliothèque à ajouter),
un par fichier, dans le dossier `profiles_dir` (exemples dans `config/profiles/`).

```json
{
  "name": "aqara.contact.mccgq11lm",
  "models": ["MCCGQ11LM"],
  "capabilities": {
    "contact": { "from": "contact", "map": { "true": "closed", "false": "open" } },
    "battery": { "from": "battery" },
    "link":    { "from": "linkquality", "max": 255, "step": 20 }
  },
  "offline_after": "4h"
}
```

```json
{
  "name": "heiman.siren.hs2wd",
  "models": ["HS2WD-E"],
  "capabilities": { "link": { "from": "linkquality", "max": 255, "step": 20 } },
  "commands": {
    "siren.on":  { "payload": { "warning": { "mode": "burglar", "level": "very_high", "duration": "{duration_s}" } } },
    "siren.off": { "payload": { "warning": { "mode": "stop" } } }
  }
}
```

- `name` : notre nom du profil (le `model` du registre) ; `models` : les modèles de
  Zigbee2MQTT (`definition.model`) auxquels il s'applique.
- Une capacité lit un champ (`from`). Avec `map`, la valeur brute écrite en texte
  (`"true"`, `"false"`, `"disarm"`) devient la nôtre ; sans `map`, c'est un nombre de 0
  à 100, ramené à cette échelle par `max` (255 pour `linkquality`) et annoncé quand il
  bouge d'au moins `step` (5 par défaut).
- `momentary` : une action (bouton panique, touche de clavier), annoncée à chaque
  message, jamais gardée comme état. `code_from` : le champ du code tapé (clavier).
  Seuls `disarm` et `silence` portent le code ; un code tapé avec une autre touche est
  écarté, la touche reste (un clavier peut envoyer le code tapé avant « armer »).
- Une commande envoie `payload` ; la chaîne `"{duration_s}"` y est remplacée par la
  durée. Avec `confirm` (`capability`, `value`), la commande n'est `done` que quand
  l'appareil rapporte cet état ; sans, elle l'est dès l'envoi.
- `offline_after` : silence avant de déclarer l'appareil hors ligne (4 h par défaut).

Chaque profil est vérifié contre le contrat au démarrage : une valeur que le contrat
refuse, une capacité inconnue, un clavier qui ne serait pas une action, une commande
qui n'existe pas, une valeur de `confirm` que la règle de sa capacité ne peut jamais
produire (par exemple `"ON"` brut au lieu de `"on"`) arrêtent l'adaptateur avec un message clair (`safehome-zigbee check`).

Les champs de Zigbee2MQTT les plus courants pour nos capteurs : `contact`
(vrai = fermé), `occupancy` (mouvement), `smoke`, `vibration`, `tamper`,
`battery`, `linkquality` (0 à 255), `action` (boutons et claviers). Les claviers et
les boutons panique varient beaucoup d'un modèle à l'autre : chacun a son profil.

## Registre des appareils

Pour chaque appareil, l'adaptateur garde sur disque :

| Champ | Rôle |
|---|---|
| `device_id` | Identifiant à nous (par exemple `dev_k7p2`), créé à l'appairage, stable, jamais réutilisé |
| `ieee` | Adresse Zigbee, **connue de l'adaptateur seulement** |
| `model` et `profile` | Modèle détecté et profil appliqué |
| `capabilities` | Capacités déclarées par le profil |
| `adapter` | `adapter.zigbee` |

À l'appairage, l'adaptateur renomme l'appareil dans Zigbee2MQTT avec son `device_id`.
Les sujets deviennent `zigbee2mqtt/dev_k7p2` : plus de nom de fabricant dans les
journaux et les échanges.

Le **nom** (« Porte d'entrée ») et la **zone** (`entree`, `instant`…) ne sont pas
dans le registre de l'adaptateur : ils appartiennent à la configuration du site, que
lit le moteur. Le registre dit ce qu'est l'appareil, la configuration dit à quoi il
sert.

Le registre est publié en retenu sur `registry/devices`, pour que le moteur et
l'outil d'installation le lisent au démarrage.

## Appairage

1. L'installateur démarre le mode appairage (`registry.pair`, durée par défaut 120
   secondes, 10 minutes au plus). L'adaptateur ouvre le réseau Zigbee à de nouveaux
   appareils. Zigbee n'ouvre pas le réseau plus de 254 secondes d'une seule demande
   (Zigbee2MQTT refuse au-delà) : une fenêtre plus longue est ouverte par morceaux de
   254 secondes au plus, chacun demandé 10 secondes avant la fin du précédent, pour
   que le réseau ne se referme jamais entre deux. La fenêtre ne compte comme ouverte
   (`zigbee.status`, `pairing` à vrai) qu'une fois que Zigbee2MQTT a répondu « ok » à
   la première demande (voir plus bas).
2. L'installateur active l'appareil (appui long sur un bouton, retrait de la
   languette de pile…). Un appareil sur pile qui s'endort vite doit être tenu éveillé
   pendant l'interrogation : l'Aqara MCCGQ11LM demande, après l'appui long de 5 s, un
   appui bref toutes les 2 à 3 secondes pendant environ 30 s (voir « Constaté sur le
   matériel »).
3. Zigbee2MQTT signale l'arrivée et termine l'interrogation de l'appareil.
4. L'adaptateur cherche un profil pour ce modèle :
   - **profil trouvé** : il crée le `device_id`, renomme l'appareil, enregistre
     l'appareil et publie `registry.device_added` ;
   - **modèle inconnu** : il refuse l'appareil, le retire du réseau et publie
     `registry.device_rejected` avec le modèle. On évite ainsi d'accepter un
     appareil que nous ne savons pas surveiller.
5. L'installateur donne le nom et la zone (configuration), puis fait un **test** :
   déclencher l'appareil et vérifier que son événement arrive.
6. À la fin du délai, ou sur ordre, l'adaptateur referme le réseau.

**Chaque demande attend sa réponse.** Chaque demande d'ouverture (`permit_join`) porte
une `transaction` à elle (l'`id` de l'ordre, un point et le numéro de la demande dans
la fenêtre), que Zigbee2MQTT 2.x recopie dans sa réponse sur
`bridge/response/permit_join`, qu'elle soit « ok » ou une erreur (`getResponse` dans
`lib/util/utils.ts`). L'adaptateur ne lit que la réponse à la demande qu'il attend :
la réponse à une demande de fermeture (qui ne porte pas de transaction), à une fenêtre
remplacée depuis ou à une demande déjà répondue est journalisée et laissée de côté.
Sans cela, le refus tardif d'une fermeture arrivé juste après l'ouverture d'une
nouvelle fenêtre aurait mis fin à cette nouvelle fenêtre.

- **Réponse « ok »** : la fenêtre est ouverte, `zigbee.status` passe `pairing` à vrai.
  Pour les morceaux suivants, la fenêtre reste ouverte pendant l'attente (le morceau
  précédent court encore).
- **Refus** : l'adaptateur journalise le refus, met fin à la fenêtre (et redemande la
  fermeture, au cas où un morceau précédent serait encore ouvert) et publie
  `registry.pair_failed` avec l'`id` de l'ordre et la raison donnée par Zigbee2MQTT.
  L'outil d'installation l'affiche et s'arrête en erreur, au lieu d'attendre devant un
  réseau fermé (trouvé à l'essai sur le matériel du 24 octobre 2026 : `-for 5m`
  demandait 300 secondes d'un coup, Zigbee2MQTT 2.14.2 répondait « Cannot permit join
  for more than 254 seconds. » et l'outil attendait comme si le réseau était ouvert).
- **Pas de réponse** (Zigbee2MQTT arrêté ou figé) : comme pour une commande, la
  demande est renvoyée, avec la même transaction (une réponse tardive à la première
  compte encore), toutes les `confirm_wait` (3 s), `attempts` (3) fois en tout. Sans
  réponse au bout du compte (9 s), la fenêtre échoue de même : `registry.pair_failed`
  avec la raison « Zigbee2MQTT did not answer the request to open the network: is it
  running? ». L'outil d'installation attend 15 s la confirmation de l'ouverture, plus
  que l'adaptateur, pour entendre cette raison.

Quand l'adaptateur s'arrête pendant une fenêtre (même pas encore confirmée : la
réponse peut n'être qu'en retard), il referme le réseau : un appareil qui entrerait
sans lui resterait dans le réseau de Zigbee2MQTT, hors du registre, sans surveillance.
Au redémarrage, si la fenêtre court encore, il le rouvre pour ce qui en reste, et
attend la réponse comme pour un morceau suivant.

Le **retrait** d'un appareil (`registry.remove`) l'enlève de Zigbee2MQTT et du
registre. Son `device_id` est retiré définitivement.

L'appairage ne peut être lancé que **localement**, jamais depuis le cloud (le cloud
ne peut publier ni sur `registry/cmd` ni sur `device/{id}/cmd`, voir `contract.md`) :
on ne doit pas pouvoir ajouter un appareil à un site à distance. Comment l'installateur envoie cet ordre est
décrit dans le document sur l'installation d'un site.

Pour la phase fermée, Zigbee permet de n'accepter que des appareils dont on connaît
un code d'installation. Ce sera la base de « un appareil ne rejoint un site que s'il
est enregistré chez nous » (voir `architecture.md`).

## De Zigbee2MQTT vers des événements

- **Seuls les changements produisent un événement.** Zigbee2MQTT republie tout
  l'état d'un appareil à chaque message. L'adaptateur compare avec la dernière valeur
  connue et n'émet que ce qui change. Exceptions : les actions ponctuelles comme
  `panic` produisent toujours un événement.
- **Événement et état.** Chaque changement produit un `device.event` et met à jour le
  `device.state` retenu.
- **État au démarrage.** Zigbee2MQTT tourne sans messages retenus (`retain: false`) :
  l'état vient de la copie que l'adaptateur garde lui-même dans son dossier d'état.
  Un message retenu qui arriverait quand même met à jour l'état mais ne produit
  **jamais** d'événement : un vieux « porte ouverte » rejoué au redémarrage ne doit
  pas déclencher l'alarme.
- **`battery` et `link`** : publiés seulement quand la valeur change de façon utile
  (voir `contract.md`).
- **Présence.** L'adaptateur note l'heure du dernier message de chaque appareil. Sans
  message pendant la durée `offline_after` du profil (4 heures par défaut, voir
  `engine.md`), il émet `availability` = `offline`. Le premier message suivant
  émet `online`. L'état retenu d'un appareil hors ligne porte aussi `heard`, l'heure
  de son dernier message à la minute, telle que Zigbee2MQTT l'a entendu (`last_seen`,
  voir « QoS » plus bas ; son appairage compte), absente s'il n'a rien dit depuis le
  redémarrage de l'adaptateur ; jamais pour un appareil en ligne, dont le dernier
  message bouge avec chaque porte et chaque passage. La centrale en tire
  `system.devices` (voir `contract.md`).

## Commandes vers les appareils

Le moteur publie `device/{id}/cmd`. L'adaptateur :

1. répond `accepted` quand la commande est valide et publiée vers Zigbee2MQTT ;
2. attend la confirmation prévue par le profil (changement d'état de la sirène) ;
3. répond `done` si elle arrive ;
4. sinon renvoie la commande : 3 envois en tout, à 3 s d'intervalle, puis répond
   `failed`.

Zigbee est une radio : un message peut se perdre. La sirène est le cas critique, donc
la confirmation et les nouvelles tentatives comptent. Un détecteur de fumée avec sa
sirène intégrée sonne de toute façon localement, sans dépendre de nous.

La centrale lit `device/{sirène}/ack` : un `siren.on` répondu `expired` ou `failed`
alors que l'alarme sonne encore est renvoyé pour le temps qui reste du cycle, 3 fois au
plus par sirène et par ordre. Si le dernier envoi échoue aussi, la centrale journalise
une erreur et lève l'alerte `siren_failed` (urgence `high`, l'appareil dans `devices`) :
le magasin est silencieux pendant l'alarme. Elle part une seule fois par sirène et par
alarme, même si la sirène refuse encore aux cycles suivants, et même après un
redémarrage de la centrale. C'est un incident à part, comme un sabotage, qui nomme
l'alarme (`alarm_incident`) : acquitter l'intrusion ne l'arrête pas, et le désarmement
non plus (la sirène reste à réparer). Une fois l'alarme prise en charge (acquittée, ou
arrêtée avec un code), l'alerte part discrète, sans appel ni sonnerie (voir
`escalation.md`, « Sirène muette pendant une alarme »). La centrale publie aussi
`alarm.siren_failed` (l'alarme et la sirène) pour l'historique (voir `contract.md`).
Un échec qui arrive dans la dernière seconde du cycle n'est ni renvoyé ni compté : le
cycle suivant renvoie l'ordre, avec ses propres tentatives. Les textes nomment la
sirène par son nom d'installation (`siren_names`), ou par défaut « Sirène »,
« Sirène 2 » (voir `escalation.md`, « Le nom d'une sirène »).

Un `siren.off` répondu `expired` ou `failed` est renvoyé lui aussi, 3 fois au plus par
sirène et par ordre, puis journalisé en erreur, sans alerte. Sans cela, l'arrêt d'un
désarmement parti pendant que l'adaptateur redémarrait laissait la sirène sonner le
reste de son cycle (5 minutes au plus), sans trace, alors que l'alarme disait désarmé et
que plus rien ne pouvait la faire taire (corrigé le 25 octobre 2026 après une revue). Le
`expired` arrive justement quand l'adaptateur répond de nouveau.

Une sirène signalée muette le reste, pour la centrale, jusqu'à la fin de l'alarme ou
jusqu'à ce qu'un `siren.on` d'un cycle suivant lui soit répondu `done`. Pendant ce
temps, un feu levé dit dans son texte que la sirène ne répond pas (voir
`escalation.md`, « Un feu dit la sirène muette »). Un `done` ne fait rien d'autre : ni
nouvel événement, ni message aux contacts. Il ne dit que ce que le profil sait : avec
`confirm`, la sirène a rapporté l'état demandé ; sans, l'ordre est seulement parti
(voir plus haut). La Heiman HS2WD, seule sirène livrée, n'a pas de `confirm` : pour
elle, `done` ne prouve pas qu'elle sonne, et une sirène signalée muette parce que
l'adaptateur était bloqué (`expired`) revient au premier ordre envoyé après sa reprise.

## État de la liaison

L'adaptateur surveille l'état du pont Zigbee2MQTT et de la clé :

- pont en ligne et clé présente : `links.zigbee` = `ok` ;
- pont arrêté, clé débranchée ou erreur : `links.zigbee` = `down`.

Le moteur déclare alors un défaut (voir `engine.md`). Un redémarrage automatique du
pont est assuré par le système (service ou conteneur avec redémarrage).

## Sauvegarde du réseau Zigbee

Le réseau Zigbee repose sur une clé et un identifiant de réseau gardés par
Zigbee2MQTT, avec une sauvegarde de la clé. **Si la carte mémoire est perdue sans
sauvegarde, tous les appareils doivent être réappairés sur place.** Il faut donc
sauvegarder cette configuration, avec le registre, hors de la centrale. La méthode
(chiffrée, vers le cloud) est décidée dans le document sur l'installation et la
sécurité.

## Ajouts reportés dans `contract.md`

Capacité `availability`, topics `registry/*`, types `registry.*` avec leurs schémas,
type `zigbee.status` et topic `zigbee/status`, alerte `zigbee_down`, un sujet d'état
retenu par capacité.

## Tests

- **Faux Zigbee2MQTT** : les tests donnent à l'adaptateur des messages de
  Zigbee2MQTT écrits à la main. Tout le comportement de l'adaptateur se teste sans
  radio. Les fichiers de référence enregistrés sur du vrai matériel (un par modèle)
  n'existent pas encore.
- Scénarios : changement d'état, message répété sans changement, état conservé au
  démarrage (aucun événement), appareil muet puis de retour, commande de sirène
  confirmée, non confirmée avec nouvelles tentatives, appairage réussi, appairage
  d'un modèle inconnu, ouverture refusée, réponse d'une autre demande ignorée,
  ouverture jamais confirmée, clavier passé en QoS 0, retrait, pont arrêté.
- Ensuite, tests avec les vrais appareils du kit, pour valider chaque profil.

## Décidé

- Seuls les modèles pour lesquels nous avons un profil sont acceptés. Les autres sont
  refusés à l'appairage.
- Un seul modèle par rôle en v1, celui du kit.
- Les ajouts de la section précédente sont reportés dans `contract.md`.

## Ce qui est codé

| Paquet | Rôle |
|---|---|
| `internal/zigbee` | Le domaine : profils, changements seulement, états conservés sans événement, actions, présence, commandes confirmées avec nouvelles tentatives, appairage, retrait, registre. Pur, sans horloge |
| `internal/zigbee/z2m` | La seule traduction depuis et vers Zigbee2MQTT (sujets, champs, requêtes du pont) |
| `internal/zigbee/wire` | La traduction vers le contrat |
| `internal/zigbeeapp` | La couche application : messages MQTT vers le domaine, publication, état répété chaque minute |
| `internal/zigbeeconfig` | Lecture de la configuration et des profils, vérifiés contre le contrat |
| `cmd/safehome-zigbee` | Le programme |

Choix faits au codage :

- **Premier état** : la première valeur connue d'une capacité (appareil neuf, état
  perdu) est enregistrée **et annoncée** : un message en direct dit le présent, et
  une porte neuve dont le premier mot est « ouverte » est une ouverture que l'alarme
  doit entendre (trou trouvé avec le kit virtuel). Le prix, dans le cas rare d'un état
  perdu, est un événement de plus pour une valeur que la centrale connaissait déjà.
- **Messages retenus** : ils mettent à jour l'état sans événement, ne ramènent pas un
  appareil hors ligne, ne confirment aucune commande, et une action retenue est
  ignorée.
- **QoS** : Zigbee2MQTT publie les messages des appareils en QoS 1, non retenus
  (`deploy/zigbee2mqtt/configuration.yaml`). L'adaptateur garde une session durable :
  le broker garde ces messages pendant un redémarrage de l'adaptateur (une porte
  ouverte entre-temps n'est pas perdue). Zigbee2MQTT ajoute à chaque message l'heure
  où il a entendu l'appareil (`last_seen`, en ISO 8601, réglage `advanced.last_seen`
  du même fichier). Les actions ponctuelles (clavier, panique) sont datées de ce
  moment, jamais plus tard que maintenant, et l'événement publié porte cette heure.
  Une touche gardée par le broker pendant une coupure de l'adaptateur arrive donc
  avec son âge réel, et la centrale la laisse expirer. Sans `last_seen`, c'est l'heure
  d'arrivée qui compte. Les claviers font exception : Zigbee2MQTT publie leurs
  messages en QoS 0 (voir « Clavier : QoS 0 »), et le broker n'en garde aucun.
  L'adaptateur, lui, publie l'événement du clavier qui porte un code en QoS 0 : le
  broker ne le garde ni pour la centrale ni pour l'agent absents.
- **Clavier : QoS 0** : les séquences d'un clavier portent le code d'accès en clair
  (`action_code`). En QoS 1, Mosquitto garderait pour l'adaptateur absent (un
  redémarrage) le message de Zigbee2MQTT, code compris, et pourrait l'écrire dans
  `mosquitto.db`. Zigbee2MQTT publie donc les messages des claviers en QoS 0 : à
  l'appairage d'un appareil dont le profil lit un code (une capacité avec `code_from`),
  l'adaptateur lui pose l'option d'appareil `qos: 0` par
  `bridge/request/device/options` (`{"id": "<adresse>", "options": {"qos": 0}}`).
  Zigbee2MQTT la garde dans sa configuration (section `devices:`), elle passe devant
  `device_options` (`qos: 1`) et vaut dès le message suivant, sans redémarrage.
  L'adaptateur la redemande à chaque fois que Zigbee2MQTT passe en ligne, donc aussi à chaque
  démarrage : un clavier appairé avant, ou une configuration de Zigbee2MQTT restaurée
  d'une sauvegarde, la retrouve. Un refus est journalisé (« Zigbee2MQTT refused a
  request », `device/options`).
  Pourquoi ainsi : une section `devices:` écrite à la main dans
  `deploy/zigbee2mqtt/configuration.yaml` demanderait l'adresse du clavier avant son
  appairage, et un oubli ne se verrait pas ; `qos: 0` pour tous les appareils perdrait
  une porte ouverte pendant un redémarrage de l'adaptateur. Le profil le dit par
  `code_from`, déjà là, plutôt que par un drapeau à part qui pourrait le contredire.
  Le prix : tout message du clavier est en QoS 0, pas seulement les codes. Une touche,
  un sabotage ou un appui sur « urgence » du clavier pendant les quelques secondes où
  l'adaptateur redémarre est perdu (comme un code envoyé à la centrale qui redémarre,
  `systeme.md`) ; l'état du sabotage revient au message suivant du clavier.
- **Présence** : l'heure du dernier message n'est pas sauvegardée (elle change à
  chaque message et userait la carte SD). Après un redémarrage, le silence de chaque
  appareil compte à partir de ce moment, et un appareil qui n'a rien dit depuis passe
  hors ligne sans `heard` : le redémarrage n'est pas une nouvelle de lui. Un appareil
  déjà hors ligne le reste, et son état retenu garde son `heard`. Un message gardé par
  le broker pendant un arrêt de l'adaptateur arrive en retard : le silence de
  l'appareil compte depuis son arrivée, mais `heard` est l'heure où Zigbee2MQTT l'a
  entendu (`last_seen`, jamais plus tard que l'arrivée ; l'arrivée sans `last_seen`), et
  ne recule jamais. Daté de l'arrivée, `heard` dirait au propriétaire que sa porte était
  surveillée plus longtemps qu'elle ne l'a été, de toute la durée de l'arrêt.
- **Commandes** : une à la fois par appareil ; une nouvelle remplace celle qui attend
  (réponse `failed`, raison `superseded`). Sans confirmation au bout de 3 s, elle est
  renvoyée, 3 fois en tout, puis `failed` (`no_confirmation`). Une commande périmée
  répond `expired`. Les commandes visent l'adresse Zigbee, qui ne change pas, même si
  le renommage a échoué.
- **Appairage** : borné à 10 minutes, ouvert par morceaux de 254 secondes au plus
  (voir « Appairage »). Un appareil entré hors d'une fenêtre
  d'appairage, illisible ou sans profil est retiré du réseau et signalé. Un identifiant
  n'est jamais redonné, même après un retrait.
- **Abonnements** : l'adaptateur ne s'abonne qu'à ce qu'il lit de Zigbee2MQTT : les
  messages des appareils (un niveau sous le sujet de base, `zigbee2mqtt/+`),
  `bridge/state`, `bridge/event` et les réponses à ses demandes (`bridge/response/#`).
  Le reste de `bridge/` reste sur le broker : `bridge/definitions` (tous les modèles
  que Zigbee2MQTT connaît, retenu, 228 ko avec la version 2.14.2, au-delà de la limite
  de 64 ko de l'adaptateur, qui le jetait avec un avertissement à chaque démarrage) et
  `bridge/devices` (qui grossit avec le réseau) ne seraient lus que pour être jetés.
- **Retrait** : les états retenus de l'appareil sont effacés, pour qu'une porte
  retirée laissée « ouverte » ne bloque pas l'armement.
- **États retenus** : un sujet par capacité (`device/{id}/state/{capacité}`), correction
  du contrat trouvée en codant : avec un seul sujet par appareil, la pile effaçait
  l'état de la porte.
- **Santé** : l'adaptateur publie `zigbee.status` (retenu, répété chaque minute). La
  centrale passe le lien `zigbee` à `down` quand le pont est arrêté, à `silent` quand
  l'adaptateur se tait depuis 3 minutes, et prévient le propriétaire (`zigbee_down`)
  3 minutes après le début de la panne : 3 minutes pour un pont arrêté, 6 pour un
  adaptateur muet.

Constaté sur le matériel (essai du 24 octobre 2026, Zigbee2MQTT 2.14.2 sur Node.js 24) :

- **Node.js** : 22 ou 24 seulement. Avec Node.js 26.5 (hors de ce que Zigbee2MQTT 2.14.2
  déclare), le port série s'ouvrait mais aucune écriture n'atteignait la clé : elle
  répondait à la réinitialisation (`RSTACK`), puis chaque trame EZSP attendait jusqu'à
  l'échec. Avec 24.22.1, tout a marché aussitôt. L'installation fixe la ligne 24 et
  `deploy/install.sh` refuse une autre version (`systeme.md`, étape 8).
- **Clé Sonoff ZBDongle-E** : USB `10c4:ea60`, « Sonoff Zigbee 3.0 USB Dongle Plus V2 » ;
  micrologiciel EmberZNet 7.4.4 (EZSP 13) ; adaptateur `ember` à 115200 bauds, sans
  contrôle de flux matériel (`rtscts: false`), comme dans
  `deploy/zigbee2mqtt/configuration.yaml`.
- **Aqara MCCGQ11LM** : il ne s'appaire que si, après l'appui long de 5 s sur son
  bouton, on appuie brièvement dessus toutes les 2 à 3 secondes pendant l'interrogation
  (environ 30 s) : sinon il s'endort avant la fin et l'interrogation échoue. Il rapporte
  `contact` à `true` (fermé) et `false` (ouvert), comme le dit son profil.
- **`permit_join`** : Zigbee2MQTT refuse plus de 254 secondes d'une seule demande
  (voir « Appairage »).

À vérifier sur le matériel :

- Les noms des sujets et des champs de la version de Zigbee2MQTT installée, en
  particulier le format de `bridge/state`. `permit_join` (l'adaptateur envoie `value`
  et `time` pour couvrir les versions 1 et 2) est vérifié avec la version 2.14.2 : elle
  lit `time` et refuse plus de 254 secondes. La `transaction` recopiée dans la réponse
  (ok ou erreur) et l'option d'appareil `qos` (lue à chaque message, sans redémarrage)
  sont lues dans le code de Zigbee2MQTT 2.x (`lib/util/utils.ts`,
  `lib/extension/bridge.ts`, `lib/controller.ts`), pas encore vues sur le kit : à
  vérifier avec un vrai clavier (après l'appairage, la section `devices:` de la
  configuration de Zigbee2MQTT porte `qos: 0`, et
  `mosquitto_sub -d -q 1 -t 'zigbee2mqtt/<id du clavier>'` montre ses messages en `q0`).
- Chaque profil de `config/profiles/` hors MCCGQ11LM : modèles, champs, valeurs
  (notamment les touches du clavier et la commande de la sirène, qui ne rapporte
  peut-être pas son état).
- La durée réelle entre deux messages de chaque capteur, pour régler `offline_after`.

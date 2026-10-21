# Adaptateur Zigbee

Statut : v1 codée et testée sans matériel (voir « Ce qui est codé » à la fin). Les noms
de Zigbee2MQTT et les profils restent à vérifier sur les vrais appareils.

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
   secondes). L'adaptateur ouvre le réseau Zigbee à de nouveaux appareils.
2. L'installateur active l'appareil (appui long sur un bouton, retrait de la
   languette de pile…).
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
  émet `online`.

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
alarme, même si la sirène refuse encore aux cycles suivants. C'est un incident à part,
comme un sabotage : acquitter l'intrusion ne l'arrête pas, et le désarmement non plus
(la sirène reste à réparer). Un échec qui arrive dans la dernière seconde du cycle
n'est ni renvoyé ni compté : le cycle suivant renvoie l'ordre, avec ses propres
tentatives.

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
  d'un modèle inconnu, retrait, pont arrêté.
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
  d'arrivée qui compte.
- **Présence** : l'heure du dernier message n'est pas sauvegardée (elle change à
  chaque message et userait la carte SD). Après un redémarrage, le silence de chaque
  appareil compte à partir de ce moment.
- **Commandes** : une à la fois par appareil ; une nouvelle remplace celle qui attend
  (réponse `failed`, raison `superseded`). Sans confirmation au bout de 3 s, elle est
  renvoyée, 3 fois en tout, puis `failed` (`no_confirmation`). Une commande périmée
  répond `expired`. Les commandes visent l'adresse Zigbee, qui ne change pas, même si
  le renommage a échoué.
- **Appairage** : borné à 10 minutes. Un appareil entré hors d'une fenêtre
  d'appairage, illisible ou sans profil est retiré du réseau et signalé. Un identifiant
  n'est jamais redonné, même après un retrait.
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

À vérifier sur le matériel :

- Les noms des sujets et des champs de la version de Zigbee2MQTT installée, en
  particulier `permit_join` (l'adaptateur envoie `value` et `time` pour couvrir les
  versions 1 et 2) et le format de `bridge/state`.
- Chaque profil de `config/profiles/` : modèles, champs, valeurs (notamment les touches
  du clavier et la commande de la sirène, qui ne rapporte peut-être pas son état).
- La durée réelle entre deux messages de chaque capteur, pour régler `offline_after`.

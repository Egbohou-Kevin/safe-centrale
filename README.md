# safeHome

Système de télésurveillance pour les commerces. La conception est dans `docs/`, le
matériel de départ dans `kit.md`.

## Conventions

Documents en français. Code, commentaires et messages d'erreur en anglais.

## Où en est le code

### Le contrat

| Dossier | Contenu |
|---|---|
| `internal/contract` | Le contrat d'événements : enveloppe, types de messages, identifiants (ULID), topics MQTT |
| `schemas/` | Les schémas JSON du contrat, pour le cloud et le firmware |

### Les domaines, avec leur traduction vers le contrat

| Dossier | Contenu |
|---|---|
| `internal/engine` | Le moteur d'alarme (le domaine) : machine à états déterministe, sans horloge ni entrées-sorties, sans connaître le contrat. Il tient aussi la table des capteurs acceptés par chaque type de zone (`engine.Accepts`), reprise par l'installateur |
| `internal/engine/wire` | La traduction entre le domaine et le contrat |
| `internal/health` | La supervision (second domaine) : batterie de la centrale, piles, appareils muets, résumé quotidien. Mêmes règles que le moteur |
| `internal/health/wire` | La traduction entre la supervision et le contrat |
| `internal/access` | Les codes d'accès (troisième domaine) : un code par personne, 5 au maximum, stockés hachés avec un algorithme lent, vérification qui dit à qui appartient le code |
| `internal/escalation` | L'escalade (quatrième domaine) : qui est prévenu, par quel canal, dans quel ordre et après combien de temps ; acquittements, relances, limite par heure |
| `internal/escalation/wire` | La traduction entre l'escalade et le contrat |
| `internal/gsm` | La passerelle GSM (cinquième domaine) : file à priorités, lignes, reprises, réponses « 1 », surveillance du modem |
| `internal/gsm/wire` | La traduction entre la passerelle et le contrat |
| `internal/zigbee` | L'adaptateur Zigbee (sixième domaine) : profils, changements, présence, commandes confirmées, appairage et registre |
| `internal/zigbee/z2m` | La seule traduction depuis et vers Zigbee2MQTT |
| `internal/zigbee/wire` | La traduction entre l'adaptateur et le contrat |
| `internal/video` | La vidéo en direct (septième domaine) : sessions, refus, durée maximale, plafond du mois en données mobiles, test des caméras |
| `internal/video/wire` | La traduction entre la vidéo et le contrat |
| `internal/maint` | La maintenance à distance (huitième domaine) : une session à la fois, une heure au plus, reprise après une coupure du tunnel |
| `internal/maint/wire` | La traduction entre la maintenance et le contrat |
| `internal/power` | Le module batterie de la centrale (neuvième domaine) : coupure du secteur confirmée après quelques secondes, charge annoncée par paliers, arrêt propre avant la batterie vide |
| `internal/power/wire` | La traduction entre le module batterie et le contrat (secteur et batterie de l'appareil `hub_device_id`) |
| `internal/agent` | Le cœur de l'agent de liaison : file pour le cloud à priorités et bornée, état de l'alarme et santé redits à chaque connexion au cloud, contrôle de ce qui descend du cloud, délai de prise en charge des alertes. Sans entrées-sorties ni horloge, il ne connaît que le contrat |

### Les couches application

| Dossier | Contenu |
|---|---|
| `internal/hub` | La couche application de la centrale : relie MQTT aux domaines (moteur, supervision, codes, escalade), file d'envoi avec reprise, clavier, battement de cœur. Elle publie par `outbox.Publisher` et ne connaît aucun adaptateur. `SiteCodes` donne les codes aux outils de l'installateur : un code n'ouvre qu'une chose |
| `internal/gateway` | La couche application de la passerelle : un fil par modem, publication des résultats et de l'état des lignes |
| `internal/zigbeeapp` | La couche application de l'adaptateur Zigbee |
| `internal/agentapp` | La couche application de l'agent : envoi au cloud avec reprise, republication sur la centrale, `agent.status` |
| `internal/videoapp` | La couche application de la vidéo : démarre et arrête les flux par le port `Streamer`, compte les octets, publie `video.session` et `video.status` |
| `internal/maintapp` | La couche application de la maintenance : ouvre et ferme le tunnel par le port `Tunneler`, publie `maint.session` |
| `internal/powerapp` | La couche application du module batterie : lit le module par le port `Board` chaque seconde, publie le secteur et la batterie de la centrale, demande l'arrêt |
| `internal/install` | La couche application de l'installateur : appairage, retrait, zones et sirènes (seul à modifier le site : `Editor`, port `Sites`), vérification de la configuration contre les appareils appairés, tests en direct. L'outil en ligne de commande et la console locale s'en servent |
| `internal/consoleapp` | La couche application de la console locale : suit le mode installation, allume le Wi-Fi de la centrale et sert la page pendant qu'il dure, fait l'appairage, les zones et les sirènes (par `internal/install`), les codes des contacts (et retire ceux d'anciens contacts) et les tests. Elle déclare ses ports (`Hotspot`, `Sites`, `Codes`, `Serve`) |
| `internal/consoleapp/web` | La page de la console : formulaires HTML en français, lisibles sur un téléphone, rien de chargé d'ailleurs, seulement à l'adresse de la centrale sur son réseau |
| `internal/configapp` | La couche application qui reçoit `config.set` dans la centrale : vérification, garde, redémarrage à un moment calme (jamais pendant le mode installation, et aussi quand les zones ou les sirènes ont changé), réponse `config.applied`. Elle ne connaît ni fichier ni processus (ports) |

### La plomberie commune

| Dossier | Contenu |
|---|---|
| `internal/inbox` | La porte d'entrée partagée par toutes les couches application : le message reçu (`Message`), les abonnements (`Subscription`), la vérification d'un message (`Open`, `Envelope`) et les derniers identifiants vus (`RecentIDs`). Il ne connaît que le contrat |
| `internal/outbox` | La file d'envoi MQTT partagée par toutes les couches application des programmes de la centrale : ordre, reprises, numéro de message qui survit aux redémarrages. Mêmes réglages partout (`Settings`), ouverture en un appel (`Open`), un message encodé et mis en file en un appel (`Send`, ou `SendWithCode` en QoS 0 pour un message qui porte le code d'accès) |
| `internal/driver` | La boucle qui fait tourner un domaine en temps réel : horloge, file d'entrées, sauvegarde, envoi des actions. Générique, elle ne connaît aucun domaine. `Every` répète un travail sur l'horloge. |
| `internal/service` | Ce que les programmes ont en commun : ligne de commande (`run`, `check`, `version`), verrou du dossier d'état, client MQTT, dialogue avec systemd, arrêt propre, redémarrage pour charger une nouvelle configuration du cloud (`Restart.Watch`), demande de relecture à un programme qui tourne (`Reload`). Seuls les programmes de `cmd/` s'en servent |
| `internal/configfile` | Ce que les lectures de configuration ont en commun : l'en-tête des fichiers (`ProgramFile`, qui donne un `Program`), JSON strict, niveau de journal, durées, secrets, écriture atomique d'un fichier |

### La lecture des configurations

| Dossier | Contenu |
|---|---|
| `internal/hubconfig` | Lecture et validation du fichier de configuration de la centrale (JSON strict), nom de son fichier de pid (`PidFile`) ; les zones et les sirènes réglées sur place, gardées dans le dossier d'état (`site-devices.json`, adaptateur `SiteFiles` du port `install.Sites`) |
| `internal/siteconfig` | La configuration réglée depuis l'application (`config.set`), gardée dans le dossier d'état ; la centrale, la passerelle GSM et la vidéo la lisent et la posent sur leur propre fichier au démarrage |
| `internal/gsmconfig` | Lecture du fichier de configuration de la passerelle, avec les contacts réglés depuis le cloud |
| `internal/zigbeeconfig` | Lecture de la configuration et des profils d'appareils (`config/profiles/`) |
| `internal/agentconfig` | Lecture de la configuration de l'agent et du secret du cloud |
| `internal/videoconfig` | Lecture de la configuration de la vidéo (caméras, liaison, plafond) |
| `internal/maintconfig` | Lecture de la configuration de la maintenance |
| `internal/powerconfig` | Lecture de la configuration du module batterie |
| `internal/consoleconfig` | Lecture de la configuration de la console et de la phrase de passe de son Wi-Fi |

### Les adaptateurs

| Dossier | Contenu |
|---|---|
| `internal/adapter/mqtt` | L'adaptateur MQTT 5 (client de Mosquitto) : seul paquet qui connaît la bibliothèque MQTT |
| `internal/adapter/filestore` | Écriture atomique de l'état sur disque (survit à une coupure de courant) ; un fichier écrit prend le propriétaire du dossier |
| `internal/adapter/atmodem` | L'adaptateur du modem : commandes AT sur le port série (bibliothèque standard seulement) ; `attest` est un faux modem pour les tests |
| `internal/adapter/queuedir` | La file de l'agent pour le cloud sur disque : un petit fichier par message, écrit d'un coup, qui survit à une coupure de courant |
| `internal/adapter/photodir` | Le dossier des photos prises à l'alarme : le service vidéo les y écrit, l'agent les lit et les efface ; borné en nombre et en taille |
| `internal/adapter/upload` | L'envoi d'un fichier au cloud en HTTPS (PUT, jeton en `Authorization`), sans jamais écrire le jeton (bibliothèque standard seulement) |
| `internal/adapter/sdnotify` | Le dialogue avec systemd : « démarré », puis « fonctionne encore » tant que les boucles répondent (chien de garde, bibliothèque standard seulement) |
| `internal/adapter/sysclock` | Dit si l'horloge du système est synchronisée, pour `clock_ok` du heartbeat (noyau Linux, bibliothèque standard seulement) |
| `internal/adapter/pidfile` | Empêche un programme de tourner deux fois sur le même état |
| `internal/adapter/go2rtc` | L'adaptateur go2rtc : un processus par session, piloté sur un socket Unix (bibliothèque standard seulement) ; `go2rtctest` est un faux go2rtc pour les tests |
| `internal/adapter/sshtunnel` | Le tunnel : le client OpenSSH vers le bastion (`ssh -R`), bastion épinglé, aucune configuration lue |
| `internal/adapter/x728` | Le module batterie Geekworm X728 : ligne du secteur et bouton d'alimentation par l'interface GPIO de Linux, jauge sur le bus I2C, bibliothèque standard seule |
| `internal/adapter/hotspot` | Le réseau Wi-Fi propre à la centrale, créé et supprimé par `nmcli` (NetworkManager), jamais démarré au boot (bibliothèque standard seulement) |

### Les programmes

| Dossier | Contenu |
|---|---|
| `cmd/safehome-hub` | Le programme de la centrale : sa racine de composition, qui branche le vrai client MQTT, le vrai disque et la vraie horloge sur la centrale et sur `configapp` |
| `cmd/safehome-gsm` | Le programme de la passerelle GSM |
| `cmd/safehome-zigbee` | Le programme de l'adaptateur Zigbee |
| `cmd/safehome-agent` | Le programme de l'agent de liaison |
| `cmd/safehome-video` | Le programme de la vidéo en direct |
| `cmd/safehome-maint` | Le programme de la maintenance à distance |
| `cmd/safehome-power` | Le programme du module batterie |
| `cmd/safehome-console` | Le programme de la console locale |
| `cmd/safehome-install` | L'outil de l'installateur, en ligne de commande sur la centrale |
| `deploy/` | Les fichiers à poser sur la centrale (services systemd, Mosquitto, Zigbee2MQTT, journaux, chien de garde). Aussi `bundle.sh`, qui fabrique le paquet d'un site, et `install.sh`, qui l'installe ou le met à jour sur la centrale. Des tests vérifient que ces fichiers sont d'accord avec les programmes et font tourner le script sur un faux système |

### Les tests

| Dossier | Contenu |
|---|---|
| `internal/archtest` | Les tests d'architecture : ils échouent si les règles de dépendance sont cassées |
| `internal/qualitytest` | Les garde-fous de qualité : tailles, gofmt, ASCII, copier-coller, code mort, faux de test redéclarés, règles de cohérence (voir « Lancer les tests ») |
| `internal/schematest` | Validation des messages contre les schémas JSON (tests seulement) |
| `internal/driver/drivertest` | Pour les tests : une fausse horloge (ses minuteries suivent une horloge monotone, comme la vraie), un faux disque, un journal partagé et de quoi attendre |
| `internal/outbox/outboxtest` | Pour les tests : un faux broker qui garde ce qu'on lui publie, et qu'on peut rendre absent, faire refuser ou faire attendre |
| `internal/adapter/mqtt/mqtttest` | Pour les tests : un vrai Mosquitto privé, lancé sur un port libre (TLS et WebSocket compris) |
| `internal/adapter/atmodem/attest` | Pour les tests : un faux modem qui répond aux commandes AT |
| `internal/adapter/go2rtc/go2rtctest` | Pour les tests : un faux go2rtc |

Des tests vérifient que les schémas restent alignés avec les structures Go, que tout ce
que le moteur publie est accepté par son schéma (enveloppe comprise), et que les schémas
refusent bien les messages invalides.

## Lancer les tests

```bash
go test ./...          # tout
go test -short ./...   # sans les scénarios de bout en bout ni le script d'installation
scripts/check.sh       # la vérification complète : gofmt, vet, règles, go test -race, build pour la centrale
```

Go 1.27 ou plus récent.

Des garde-fous tournent avec les tests : les règles d'architecture
(`internal/archtest`) et de qualité (`internal/qualitytest` : tailles, gofmt, ASCII,
copier-coller, code mort, faux de test redéclarés, cohérence). Un échec dit
`fichier:ligne: [règle] problème. Fix: quoi faire`. Les seuils et la façon d'ajouter
une exception (dans `exceptions_test.go`, toujours avec sa raison) sont dans
`docs/architecture.md`, « Les garde-fous de qualité ».

Avant chaque commit, un hook lance `scripts/check.sh --quick` (gofmt, vet,
`go test -short ./...`). À activer une fois par clone :

```bash
git config core.hooksPath .githooks
```

L'intégration continue (`.github/workflows/ci.yml`) lance le même script, en entier, à
chaque push et pull request : le hook, le script et la CI ne listent pas chacun leurs
commandes. Elle s'active en poussant le dépôt sur GitHub ; une fois le dépôt distant
créé, y rendre la CI obligatoire avant merge (protection de branche). La CI installe
Mosquitto, donc les tests de bout en bout y tournent ; go2rtc n'est pas empaqueté : les
tests vidéo qui en ont besoin y sont ignorés et ne tournent qu'en local.

Avant le vrai matériel, le **kit virtuel** (`vkit/`, `docs/kit-virtuel.md`) installe la
centrale dans une VM Debian ARM sur le Mac, avec un faux modem, un faux Zigbee2MQTT et
un faux cloud, et rejoue la vie d'un site (`vkit/up.sh`, `vkit/deploy.sh`,
`vkit/test.sh`).

Les domaines, le contrat et la traduction n'ont aucune dépendance externe. Chaque
bibliothèque externe est enfermée dans le seul paquet qui l'enveloppe, et un test
d'architecture refuse tout autre import :

| Bibliothèque | Où | Rôle |
|---|---|---|
| `github.com/eclipse/paho.golang` (MQTT 5, `autopaho`) | `internal/adapter/mqtt` seulement | Parler au broker, se reconnecter |
| `github.com/gorilla/websocket` (déjà tirée par paho) | `internal/adapter/mqtt` seulement | MQTT dans WebSocket : un paquet par message, ce qu'exige Mosquitto 2.1 |
| `github.com/santhosh-tekuri/jsonschema/v6` | tests seulement (`internal/schematest`) | Valider les messages contre les schémas |

Toute nouvelle dépendance est décidée explicitement (demandée à Kevin avant
téléchargement).

Un programme externe, go2rtc (licence MIT, accepté par Kevin), sert à la vidéo : il
n'est pas une bibliothèque, le service le lance (`internal/adapter/go2rtc`). Ses
tests tournent contre un faux go2rtc ; ceux contre le vrai (caméra et relais simulés
par ffmpeg) sont ignorés si go2rtc ou ffmpeg ne sont pas installés.

Les photos à l'alarme demandent aussi ffmpeg sur la centrale (paquet Debian, validé
par Kevin le 20 octobre 2026) : go2rtc s'en sert pour décoder une image de la caméra.

La console locale crée son réseau Wi-Fi avec NetworkManager (`nmcli`), déjà dans
Raspberry Pi OS. Rien à télécharger, mais c'est **à vérifier sur le kit** (mode
« partagé », règle polkit). Ses tests tournent contre un faux `nmcli`.

Les tests de l'adaptateur MQTT et du programme de la centrale tournent contre un vrai
broker Mosquitto privé, lancé par `internal/adapter/mqtt/mqtttest` sur un port libre
(`brew install mosquitto`). Si Mosquitto n'est pas installé, ces tests sont ignorés.

Pour la centrale elle-même (compilation pour le Raspberry Pi comprise) :
`docs/operations.md`.

## Documents de conception

| Document | Sujet |
|---|---|
| `docs/architecture.md` | Principe, composants, architecture hexagonale, décisions |
| `docs/glossaire.md` | Français ↔ anglais : le langage commun |
| `docs/contract.md` | Contrat d'événements |
| `docs/engine.md` | Moteur d'alarme |
| `docs/escalation.md` | Escalade des alertes |
| `docs/gsm-gateway.md` | Passerelle GSM |
| `docs/agent.md` | Agent de liaison avec le cloud |
| `docs/adapter-zigbee.md` | Adaptateur Zigbee |
| `docs/video.md` | Vidéo en direct vers l'application |
| `docs/maintenance.md` | Maintenance à distance |
| `docs/security-install.md` | Sécurité et installation d'un site |
| `docs/systeme.md` | Système de la centrale : installation pas à pas, lecture seule, services, recette |
| `docs/operations.md` | Exploitation de la centrale : configuration, commandes, démarrage, arrêt, codes |
| `docs/kit-virtuel.md` | Kit virtuel : la centrale dans une VM avec de faux équipements, et son scénario |

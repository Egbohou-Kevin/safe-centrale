# safeHome

Système de télésurveillance pour les commerces. La conception est dans `docs/`, le
matériel de départ dans `kit.md`.

## Conventions

Documents en français. Code, commentaires et messages d'erreur en anglais.

## Où en est le code

| Dossier | Contenu |
|---|---|
| `internal/contract` | Le contrat d'événements : enveloppe, types de messages, identifiants (ULID), topics MQTT |
| `internal/engine` | Le moteur d'alarme (le domaine) : machine à états déterministe, sans horloge ni entrées-sorties, sans connaître le contrat |
| `internal/engine/wire` | La traduction entre le domaine et le contrat |
| `internal/health` | La supervision (second domaine) : batterie de la centrale, piles, appareils muets, résumé quotidien. Même règles que le moteur |
| `internal/access` | Les codes d'accès (troisième domaine) : un code par personne, 5 au maximum, stockés hachés avec un algorithme lent, vérification qui dit à qui appartient le code |
| `internal/escalation` | L'escalade (quatrième domaine) : qui est prévenu, par quel canal, dans quel ordre et après combien de temps ; acquittements, relances, limite par heure |
| `internal/escalation/wire` | La traduction entre l'escalade et le contrat |
| `internal/health/wire` | La traduction entre la supervision et le contrat |
| `internal/adapter/mqtt` | L'adaptateur MQTT 5 (client de Mosquitto) : seul paquet qui connaît la bibliothèque MQTT |
| `internal/adapter/filestore` | Écriture atomique de l'état sur disque (survit à une coupure de courant) |
| `internal/hub` | La couche application de la centrale : relie MQTT aux domaines (moteur, supervision, codes), file d'envoi avec reprise, clavier, battement de cœur. Elle déclare ses propres ports |
| `internal/gsm` | La passerelle GSM (cinquième domaine) : file à priorités, lignes, reprises, réponses « 1 », surveillance du modem |
| `internal/gsm/wire` | La traduction entre la passerelle et le contrat |
| `internal/gateway` | La couche application de la passerelle : un fil par modem, publication des résultats et de l'état des lignes |
| `internal/outbox` | La file d'envoi MQTT partagée par toutes les couches application des programmes de la centrale : ordre, reprises, numéro de message qui survit aux redémarrages |
| `internal/adapter/atmodem` | L'adaptateur du modem : commandes AT sur le port série (bibliothèque standard seulement) ; `attest` est un faux modem pour les tests |
| `internal/adapter/queuedir` | La file de l'agent pour le cloud sur disque : un petit fichier par message, écrit d'un coup, qui survit à une coupure de courant |
| `internal/adapter/photodir` | Le dossier des photos prises à l'alarme : le service vidéo les y écrit, l'agent les lit et les efface ; borné en nombre et en taille |
| `internal/adapter/upload` | L'envoi d'un fichier au cloud en HTTPS (PUT, jeton en `Authorization`), sans jamais écrire le jeton (bibliothèque standard seulement) |
| `internal/adapter/sdnotify` | Le dialogue avec systemd : « démarré », puis « fonctionne encore » tant que les boucles répondent (chien de garde, bibliothèque standard seulement) |
| `internal/adapter/sysclock` | Dit si l'horloge du système est synchronisée, pour `clock_ok` du heartbeat (noyau Linux, bibliothèque standard seulement) |
| `internal/adapter/pidfile` | Empêche un programme de tourner deux fois sur le même état |
| `internal/gsmconfig` | Lecture du fichier de configuration de la passerelle |
| `cmd/safehome-gsm` | Le programme de la passerelle GSM |
| `internal/zigbee` | L'adaptateur Zigbee (sixième domaine) : profils, changements, présence, commandes confirmées, appairage et registre |
| `internal/zigbee/z2m` | La seule traduction depuis et vers Zigbee2MQTT |
| `internal/zigbee/wire` | La traduction entre l'adaptateur et le contrat |
| `internal/zigbeeapp` | La couche application de l'adaptateur Zigbee |
| `internal/zigbeeconfig` | Lecture de la configuration et des profils d'appareils (`config/profiles/`) |
| `cmd/safehome-zigbee` | Le programme de l'adaptateur Zigbee |
| `internal/agent` | Le cœur de l'agent de liaison : file pour le cloud à priorités et bornée, contrôle de ce qui descend du cloud, délai de prise en charge des alertes. Sans entrées-sorties ni horloge, il ne connaît que le contrat |
| `internal/agentapp` | La couche application de l'agent : envoi au cloud avec reprise, republication sur la centrale, `agent.status` |
| `internal/agentconfig` | Lecture de la configuration de l'agent et du secret du cloud |
| `cmd/safehome-agent` | Le programme de l'agent de liaison |
| `internal/video` | La vidéo en direct (septième domaine) : sessions, refus, durée maximale, plafond du mois en données mobiles, test des caméras |
| `internal/video/wire` | La traduction entre la vidéo et le contrat |
| `internal/videoapp` | La couche application de la vidéo : démarre et arrête les flux par le port `Streamer`, compte les octets, publie `video.session` et `video.status` |
| `internal/adapter/go2rtc` | L'adaptateur go2rtc : un processus par session, piloté sur un socket Unix (bibliothèque standard seulement) ; `go2rtctest` est un faux go2rtc pour les tests |
| `internal/videoconfig` | Lecture de la configuration de la vidéo (caméras, liaison, plafond) |
| `cmd/safehome-video` | Le programme de la vidéo en direct |
| `internal/maint` | La maintenance à distance (huitième domaine) : une session à la fois, une heure au plus, reprise après une coupure du tunnel |
| `internal/maint/wire` | La traduction entre la maintenance et le contrat |
| `internal/maintapp` | La couche application de la maintenance : ouvre et ferme le tunnel par le port `Tunneler`, publie `maint.session` |
| `internal/adapter/sshtunnel` | Le tunnel : le client OpenSSH vers le bastion (`ssh -R`), bastion épinglé, aucune configuration lue |
| `internal/maintconfig` | Lecture de la configuration de la maintenance |
| `cmd/safehome-maint` | Le programme de la maintenance à distance |
| `internal/install` | La couche application de l'installateur : appairage, retrait, zones et sirènes, vérification de la configuration contre les appareils appairés, tests en direct. L'outil en ligne de commande et la console locale s'en servent |
| `internal/consoleapp` | La couche application de la console locale : suit le mode installation, allume le Wi-Fi de la centrale et sert la page pendant qu'il dure, fait l'appairage, les zones, les sirènes, les codes des contacts et les tests. Elle déclare ses ports (`Hotspot`, `Sites`, `Codes`, `Serve`) |
| `internal/consoleapp/web` | La page de la console : formulaires HTML en français, lisibles sur un téléphone, rien de chargé d'ailleurs, seulement à l'adresse de la centrale sur son réseau |
| `internal/adapter/hotspot` | Le réseau Wi-Fi propre à la centrale, créé et supprimé par `nmcli` (NetworkManager), jamais démarré au boot (bibliothèque standard seulement) |
| `internal/consoleconfig` | Lecture de la configuration de la console et de la phrase de passe de son Wi-Fi |
| `cmd/safehome-console` | Le programme de la console locale |
| `cmd/safehome-install` | L'outil de l'installateur, en ligne de commande sur la centrale |
| `internal/siteconfig` | La configuration réglée depuis l'application (`config.set`), gardée dans le dossier d'état ; la centrale, la passerelle GSM et la vidéo la lisent et la posent sur leur propre fichier au démarrage |
| `internal/configapp` | La couche application qui reçoit `config.set` dans la centrale : vérification, garde, redémarrage à un moment calme, réponse `config.applied`. Elle ne connaît ni fichier ni processus (ports) |
| `internal/hubconfig` | Lecture et validation du fichier de configuration de la centrale (JSON strict) ; les zones et les sirènes réglées sur place, gardées dans le dossier d'état (`site-devices.json`) |
| `cmd/safehome-hub` | Le programme de la centrale : sa racine de composition, qui branche le vrai client MQTT, le vrai disque et la vraie horloge sur la centrale et sur `configapp` |
| `internal/service` | Ce que les programmes ont en commun : ligne de commande (`run`, `check`, `version`), verrou du dossier d'état, client MQTT, dialogue avec systemd, arrêt propre. Seuls les programmes de `cmd/` s'en servent |
| `internal/configfile` | Ce que les lectures de configuration ont en commun : JSON strict, niveau de journal, durées, secrets, écriture atomique d'un fichier |
| `internal/driver` | La boucle qui fait tourner un domaine en temps réel : horloge, file d'entrées, sauvegarde, envoi des actions. Générique, elle ne connaît aucun domaine |
| `internal/archtest` | Les tests d'architecture : ils échouent si les règles de dépendance sont cassées |
| `internal/schematest` | Validation des messages contre les schémas JSON (tests seulement) |
| `schemas/` | Les schémas JSON du contrat, pour le cloud et le firmware |
| `deploy/` | Les fichiers à poser sur la centrale (services systemd, Mosquitto, Zigbee2MQTT, journaux, chien de garde). Aussi `bundle.sh`, qui fabrique le paquet d'un site, et `install.sh`, qui l'installe ou le met à jour sur la centrale. Des tests vérifient que ces fichiers sont d'accord avec les programmes et font tourner le script sur un faux système |

Des tests vérifient que les schémas restent alignés avec les structures Go, que tout ce
que le moteur publie est accepté par son schéma (enveloppe comprise), et que les schémas
refusent bien les messages invalides.

## Lancer les tests

```bash
go test ./...
```

Go 1.27 ou plus récent.

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

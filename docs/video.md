# Vidéo en direct

Statut : service vidéo de la centrale codé (`internal/video`, `internal/videoapp`,
`internal/adapter/go2rtc`, `cmd/safehome-video`), testé contre un faux go2rtc et le
vrai. Il prend aussi les **photos à l'alarme** (voir « Photos à l'alarme »). Le relais
du cloud et l'application ne sont pas codés. Voir « Ce qui est codé » à la fin.

## But

Permettre au propriétaire (et aux contacts qu'il autorise) de regarder une caméra du
site en direct depuis l'application :

- **à l'alarme** : la notification d'alarme propose « Voir en direct » ;
- **à la demande** : depuis l'écran du site, à tout moment.

La vidéo est un confort et une aide à la décision (« est-ce un voleur ou l'employé
qui a oublié son code ? »). Elle ne fait **jamais** partie de la chaîne d'alarme : si
elle tombe, la sirène, les SMS et l'escalade continuent.

## Principe

```
Application ──(1) demande──► Cloud ──(2) video.request──► Agent ──► Service vidéo (centrale)
     ▲                         │                                         │
     │                         │                                   (3) lit la caméra (RTSP)
     │                         ▼                                         │
     └──(5) regarde (WebRTC)── Relais vidéo du cloud ◄──(4) envoie le flux┘
```

1. L'application demande une session (ou la propose depuis la notification d'alarme).
2. Le cloud vérifie les droits de la personne sur ce site, choisit l'adresse du relais
   et un jeton valable pour cette seule session, et publie `video.request`.
3. Le service vidéo de la centrale lit le flux de la caméra sur le réseau local.
4. Il l'envoie au relais du cloud en **RTMPS** (RTMP chiffré par TLS), **en ouvrant
   lui-même la connexion vers l'extérieur** : rien à ouvrir sur la box du client.
   C'est indispensable en 4G, où l'opérateur bloque presque toujours les connexions
   entrantes.
5. L'application regarde le flux sur le relais, en WebRTC (chiffré, environ une
   demi-seconde de retard).

Le site n'envoie qu'**un seul flux** par caméra, même si plusieurs personnes regardent :
le relais le distribue. Le flux vidéo ne passe jamais par MQTT ; MQTT ne porte que
les ordres et les comptes rendus.

## Décisions

- **Pas de réencodage sur la centrale.** Le service recopie la vidéo telle que la
  caméra la produit. Un Raspberry Pi 4 de 1 Go suffit (de l'ordre de 20 à 50 Mo de
  mémoire). Le Pi 5 n'a d'ailleurs plus d'encodeur vidéo matériel.
- **Flux secondaire de la caméra** (environ 640×360, 300 à 500 kbit/s) : de l'ordre de
  **150 à 250 Mo par heure** de direct. Le flux principal (haute définition) reste
  sur le site.
- **H.264 obligatoire.** Le H.265 est mal lu par les téléphones et les navigateurs. La
  Tapo C200 envoie du H.264 ; une Reolink doit être réglée en H.264.
- **Plafond et durée selon la liaison du site** (décidé par Kevin) :
  - **liaison fixe** (la box du commerce, en Wi-Fi ou par câble) : regarder ne coûte
    rien de plus au client, donc **ni plafond ni durée limite** ;
  - **données mobiles** (routeur 4G) : les données sont à la charge du client, donc
    **c'est lui qui règle** le plafond mensuel (`quota_mb`) et la durée maximale d'une
    session (`max_s`) dans l'application. Valeurs proposées à l'installation, que le
    client peut changer : 1 Go par mois et 5 minutes par session. Quand le plafond est
    atteint, le service refuse la session (`quota`).

  L'installateur indique le type de liaison à l'installation (`uplink` : `fixed` ou
  `mobile`). L'installateur le change dans `video.json` et `agent.json` si le client
  change d'abonnement. Le service publie le
  type, les réglages et la consommation du mois dans `video.status`.
- **Pas de flux sans spectateur.** Même sans durée limite, une session s'arrête quand
  plus personne ne regarde (application fermée, téléphone en veille) : le relais le
  voit et le cloud envoie `video.stop`. Ce n'est pas une limite pour le client, c'est
  pour ne pas faire tourner une caméra et le relais pour rien.
- **Sessions simultanées bornées** (par défaut 2 par site), pour ne pas saturer la
  liaison montante. Au-delà : `busy`.
- **À l'alarme, jamais de démarrage automatique** (décidé par Kevin) : à la réception de
  l'alerte, l'application propose « Voir en direct » ; la session ne démarre que si la
  personne appuie.
- **Pas de vidéo à la place de l'alarme** : une alarme part toujours, avec ou sans
  vidéo, et l'acquittement reste « 1 » ou le bouton de l'application.

## Photos à l'alarme

Quand l'alarme se déclenche (`alarm.triggered`), le service vidéo prend quelques
images des caméras, sans attendre personne :

- **liaison fixe** : 3 images par caméra, à 2 s d'intervalle, 640 px de large ;
- **données mobiles** (mode économe, données payées par le client) : 1 image par
  caméra, 480 px de large ;
- 4 caméras au plus, une seule série par alarme, 6 alarmes photographiées par heure
  au plus (un capteur qui se déclenche sans cesse ne doit pas remplir la liaison) ;
- `"photos": false` dans `video.json` les coupe.

Les images sont prises même au-delà du plafond du mois : quelques centaines de
kilo-octets qui peuvent montrer qui est entré. Elles comptent dans la consommation du
mois.

Une image se prend par go2rtc : un go2rtc ouvert pour la série lit toutes les
caméras, et rend sur demande une image JPEG réduite (`/api/frame.jpeg`), que
**ffmpeg** décode depuis l'image clé suivante de la caméra (une ou deux secondes au
plus). C'est le seul usage de ffmpeg sur la centrale : il faut donc l'installer
(`apt install ffmpeg`, voir `systeme.md`). go2rtc ne réencode jamais un flux en
direct.

Chaque image est gardée dans `/var/lib/safehome/photos` (60 au plus, 8 Mo au plus ;
les plus anciennes partent d'abord), puis annoncée par `photo.taken`. L'agent de
liaison l'envoie au cloud et l'efface (voir `agent.md`). Une caméra qui ne rend pas
d'image est sautée pour le reste de la série, avec la cause dans le journal.

## Messages (réservés dans `contract.md`)

| Type | Topic | Sens | Rôle |
|---|---|---|---|
| `video.request` | `video/cmd` | cloud → site | Ouvrir une session : `session`, `camera`, `reason` (`alarm`, `demand`), `user`, `incident`, `max_s` (absent ou 0 : sans limite), `push_url`, `token`, `exp` |
| `video.stop` | `video/cmd` | cloud → site | Arrêter une session : `session` |
| `video.session` | `video/session` | site → cloud | Étapes : `starting`, `live`, `ended`, `failed`, avec `reason`, `duration_s`, `bytes` |
| `video.status` | `video/status` (retenu) | site → cloud | Caméras joignables, sessions en cours, type de liaison (`uplink`), données du mois, plafond et durée maximale réglés par le client. Publié au démarrage et à chaque changement, jamais répété à l'identique : il partirait au cloud, peut-être en 4G |

Raisons de fin ou d'échec : `stopped` (arrêt demandé), `timeout` (`max_s` atteint),
`quota`, `busy`, `unknown_camera`, `camera_unreachable`, `relay_unreachable`,
`expired` (demande arrivée après `exp`).

Le cloud peut aussi publier sur `video/cmd`. Ce que le cloud peut publier est listé
dans `contract.md`. Il ne peut toujours pas commander les appareils ni l'appairage.

## Sécurité et vie privée

- **Caméras jamais exposées sur internet** : elles sont sur le réseau local (ou celui
  du routeur 4G), mots de passe d'usine changés, accès à leur cloud de marque coupé
  si possible. L'adresse et le mot de passe RTSP de chaque caméra restent dans la
  configuration du site ; le contrat ne connaît que l'identifiant (`cam_caisse`).
- **Jeton par session** : `token` ne vaut que pour une session et un relais, et expire
  avec elle. Comme un code d'accès, il n'apparaît jamais dans un journal (le contrat
  le masque à l'affichage, un test le vérifie).
- **`push_url` en rtmps seulement**, sans identifiants ni requête dans l'adresse : le
  site y ajoute le jeton en `?token=` au moment d'envoyer. Ni l'adresse complète ni le
  jeton n'apparaissent dans les journaux, même quand go2rtc les écrit dans les siens.
- **Adresses des caméras** (avec leur mot de passe) seulement dans
  `/etc/safehome/video.json`, lisible seulement par le compte `safehome` (celui de
  tous nos programmes), et dans le dossier privé
  de chaque go2rtc pendant une session.
- **Demande périmée refusée** (`exp`) : un ordre rejoué plus tard ne rallume pas une
  caméra.
- **Journal des visionnages** dans le cloud : qui a regardé quelle caméra, quand,
  combien de temps.
- **Cadre légal** : filmer des employés et des clients relève de la loi togolaise sur
  la protection des données personnelles (loi de 2019, à vérifier avec un juriste).
  Probablement : panneau « site sous vidéosurveillance », information des employés,
  pas de caméra dans les lieux privés, durée de conservation limitée des images.
  Le direct sans enregistrement réduit le problème sans le supprimer.

## Composants à construire

| Où | Quoi | Remarque |
|---|---|---|
| Centrale | Service vidéo : reçoit `video.request`/`video.stop`, lit la caméra, pousse vers le relais, compte les données, publie `video.session` et `video.status` | Codé : `safehome-video` pilote go2rtc (voir plus bas) |
| Cloud | Relais vidéo (par exemple MediaMTX) et serveur TURN si besoin | Reçoit le flux en RTMPS, vérifie le jeton (`?token=`), le distribue aux applications en WebRTC |
| Cloud | Autorisation des sessions, jetons, journal des visionnages | Dans le cloud Java |
| Agent de liaison | Transmettre `video/cmd` vers le site, `video/session` et `video/status` vers le cloud | Mêmes règles que les autres messages |
| Application | Bouton « Voir en direct », lecture WebRTC, « continuer » | |

## Ce qui est codé

### Un go2rtc par session

go2rtc (version 1.9, licence MIT) lit le flux de la caméra et le pousse au relais sans
le réencoder. Deux limites de go2rtc, lues dans son code source, ont fixé la façon de
s'en servir :

- **Il ne sait pousser qu'en RTMP ou RTMPS**, pas en WebRTC (WHIP). Le site pousse
  donc en RTMPS ; le relais du cloud (MediaMTX le sait) reçoit du RTMPS et sert du
  WebRTC aux applications. Le contrat demande en conséquence un `push_url` en
  `rtmps://`.
- **Son interface ne permet pas d'arrêter un envoi** : il réessaie sans fin toutes les
  5 secondes. Le service lance donc **un go2rtc par session** et arrête la session en
  arrêtant ce go2rtc. Un go2rtc qui s'arrête tout seul termine la session.

Chaque go2rtc n'écoute que sur un socket Unix, dans un dossier privé où est écrite sa
configuration (l'adresse de la caméra) : pas de serveur RTSP, pas de WebRTC, pas de
page web, aucun port ouvert. Si le service meurt, systemd arrête ses go2rtc avec lui
(et, sous Linux, le noyau les arrête aussi), pour qu'aucun flux ne continue à
consommer les données mobiles du client.

### Le service

- **Le domaine** (`internal/video`) : refus (`expired`, `unknown_camera`, `busy`,
  `quota`), passage à `live` dès que les premiers octets partent, fin à `max_s` (le plus
  court entre la demande du cloud et le réglage du client en données mobiles), à
  l'épuisement du plafond, quand rien ne part depuis 30 s (`relay_unreachable`), ou si
  le relais n'a rien reçu 20 s après la demande. La consommation du mois est gardée sur
  disque et repart de zéro le 1er de chaque mois (heure UTC, celle du Togo). Les
  sessions, elles, ne sont pas gardées : après un redémarrage du service, leurs flux
  sont arrêtés et le relais le voit.
- **Le comptage** : les octets de vidéo et de son que go2rtc envoie, lus toutes les
  2 secondes. L'enveloppe RTMP et TLS n'est pas comptée (quelques pour cent en plus,
  à mesurer sur le kit).
- **Les caméras** sont testées toutes les 5 minutes (connexion à leur port RTSP) ;
  `video.status` dit lesquelles répondent.
- **Une caméra en H.265** est refusée comme injoignable (`camera_unreachable`), avec
  la cause dans le journal : go2rtc ne pousse que du H.264 en RTMP.

### Tests

- Contre un faux go2rtc (le programme de test joue go2rtc) : tout le service, sans
  rien installer.
- Contre le vrai go2rtc 1.9.14, quand go2rtc et ffmpeg sont installés
  (`brew install ffmpeg`) : une caméra simulée (un autre go2rtc nourri par ffmpeg
  d'une mire en H.264) pousse vers un relais simulé (ffmpeg en attente d'un flux
  RTMP). Le test vérifie que le relais reçoit bien du H.264, que les octets sont
  comptés, que l'envoi s'arrête avec la session et que ni le jeton ni le mot de passe
  n'apparaissent. Une caméra en H.265 est refusée. Ces essais ont trouvé deux erreurs
  que le faux go2rtc cachait (le nom que go2rtc donne à l'envoi, et un masquage des
  secrets qui gênait la lecture de ses erreurs).
- Lancé avec notre configuration, go2rtc n'ouvre aucun port réseau (vérifié avec
  `lsof`) et sa page web est désactivée.

- Photos, contre le vrai go2rtc et ffmpeg : une image de la caméra simulée à 640 et
  à 320 px (de l'ordre de 0,1 s chacune), une caméra morte refusée. go2rtc 1.9.14
  répond « 200 » sans rien quand la caméra ne répond pas : le service le reconnaît.

Le client règle le plafond et la durée depuis l'application : ils arrivent par
`config.set` (`video.quota_mb`, `video.max_s`), remplacent ceux de `video.json` en
données mobiles, et le service redémarre pour les prendre (une session en cours
s'arrête). En liaison fixe, ils sont ignorés et `safehome-video check` le signale.

Pas encore fait : le relais du cloud et l'application.

## À mesurer sur le kit d'essai

- Débit réel du flux secondaire de chaque modèle de caméra, et donc la consommation
  par heure.
- Retard entre la scène et l'écran, en 4G et sur une box.
- Mémoire et processeur du service vidéo sur le Pi 4 de 1 Go, avec deux sessions,
  et pendant une série de photos (ffmpeg compris).
- Poids réel d'une photo à 640 et 480 px selon la caméra et la scène.
- Comportement quand la liaison montante est faible : le relais doit dégrader sans
  bloquer les messages d'alarme (la vidéo passe après).

## Questions ouvertes

- Le relais du cloud fait sortir chaque flux vers chaque spectateur : en liaison fixe,
  sans limite, ce trafic est **à notre charge**. À chiffrer avec l'hébergeur avant
  d'annoncer le direct illimité, et à surveiller par site.
- Enregistrer quelques secondes autour d'une alarme (preuve pour la police) : utile,
  mais c'est de la conservation d'images, donc plus de règles.

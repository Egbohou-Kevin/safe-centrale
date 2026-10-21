# Agent de liaison

Statut : v1 codée (`internal/agent`, `internal/agentapp`, `cmd/safehome-agent`), sauf
les photos, le rejeu d'une plage de `seq` et la configuration venue du cloud. Voir
« Ce qui est codé » à la fin.

L'agent de liaison est le programme en Go qui relie la centrale au cloud. Il
transmet au cloud ce qui doit être supervisé, reçoit les ordres et la
configuration venus du cloud, et garde les messages en attente quand internet est
coupé. Il ne prend aucune décision d'alarme : si l'agent s'arrête, le moteur, la
sirène et la passerelle GSM continuent.

## Responsabilités

- Ouvrir et maintenir une connexion **sortante** vers le cloud.
- Envoyer au cloud les messages de la liste « Ce qui part vers le cloud » de
  `contract.md`, dont le heartbeat de la centrale.
- Garder les messages sur disque quand la liaison est coupée, puis les renvoyer
  dans l'ordre.
- Recevoir les ordres de l'application (`alarm/cmd`) et la configuration, les
  contrôler, puis les publier sur le broker local.
- Transporter les alertes pour les canaux du cloud (application, WhatsApp) et
  leurs résultats.
- Envoyer les photos prises à l'alarme.

## Entrées et sorties

| Direction | Contenu | Détail |
|---|---|---|
| Local vers cloud | `alarm.*`, `alert.*`, `alarm/state`, `system/health`, événements `smoke`, `panic`, `tamper`, `mains`, `battery` | Liste exacte dans `contract.md` |
| Local vers cloud | Heartbeat | Publié par la centrale chaque minute, relayé par l'agent |
| Local et cloud | `agent.status` | Créé par l'agent : à chaque changement et chaque minute |
| Local vers cloud | Photos | Envoi à part (voir « Photos ») |
| Cloud vers local | `alarm/cmd` | Ordres de l'application |
| Cloud vers local | `video/cmd` | Sessions vidéo en direct |
| Cloud vers local | Configuration | Contacts, planning, délais, politique de canaux (pas encore : refusée) |
| Cloud vers local | Résultats et acquittements des canaux du cloud | Republiés en `alert.result` et `alert.ack` |

Les événements `keypad` ne partent jamais, car ils portent le code. Les
commandes d'appareils (`siren.on`…) ne descendent jamais du cloud.

## Liaison avec le cloud

**MQTT sur TLS**, connexion sortante, avec une session persistante :

- Aucun port n'est ouvert sur la box du client, et pas besoin d'adresse IP fixe.
- L'ordre de l'application arrive tout de suite par la même connexion, sans que
  l'agent ait à interroger le cloud.
- Le protocole est conçu pour les liaisons instables et économes.

Si le port MQTT sur TLS (8883) est bloqué par la box ou le réseau, l'agent bascule
sur **MQTT dans WebSocket, port 443**, qui passe presque partout.

La reconnexion se fait avec un délai croissant (1 s, 2 s, 4 s… plafonné à 30 s), dont
l'agent n'attend qu'une part tirée au hasard, la première tentative comprise (jusqu'à
2 s) : des centaines de sites ne se reconnectent pas tous au même instant après une
panne du cloud.

L'agent déclare un **dernier message** (« Last Will ») au broker : si la connexion
se coupe sans prévenir, le broker publie « site hors ligne » (`agent.status` avec
`uplink` à `down`). Le cloud le sait immédiatement, sans attendre trois heartbeats
manquants. Comme une déconnexion propre ne déclenche pas ce message, l'agent le publie
lui-même quand il s'arrête.

## Messages en attente

Quand internet est coupé :

- Les événements d'alarme et d'alerte, l'état et la santé sont écrits dans une file
  sur disque, avec leur `seq`. Un message ne quitte la file que quand le broker du
  cloud en a accusé réception : après une coupure de courant, ce qui n'était pas
  confirmé repart (le cloud ignore les doublons grâce à `id`).
- **Seul le dernier heartbeat est gardé** : un signe de vie vieux de deux heures n'a
  aucun intérêt. De même, pour un message retenu (`alarm.state`, `system.health`,
  `agent.status`, `video.status`), seul le dernier compte.
- À la reconnexion, la file part par priorité : d'abord l'alarme et les alertes,
  puis les événements, puis les états, et dans l'ordre d'arrivée à priorité égale.
- La file est bornée : 20 000 messages, 10 Mo ou 7 jours par défaut. Si elle est
  pleine, on supprime d'abord les messages les moins importants : les événements
  d'alarme et d'alerte ne sont jamais supprimés avant les autres. Les suppressions
  sont comptées dans `agent.status` (`dropped`).
- **Sur la carte SD**, chaque message de la file est un petit fichier, dans le
  dossier `agent_queue` du répertoire d'état. Ajouter, numéroter ou envoyer un
  message n'écrit que son fichier, jamais toute la file. Vider 20 000 messages après
  une coupure écrit ainsi environ 15 Mo, au lieu de plus de 100 Go. Le reste de
  l'état de l'agent est petit et tient dans un seul fichier. Après une coupure de
  courant, l'agent remet les deux d'accord au démarrage. Une file gardée par une
  version précédente est reprise sans perte.
- **Numérotation pour le cloud** : chaque programme numérote ses propres messages, et
  beaucoup restent sur le site. L'agent numérote donc de nouveau ce qui part au cloud,
  à la première tentative d'envoi : là-bas, `seq` est continu par site, et un trou
  veut dire un message manqué. Un état ou un heartbeat remplacé avant confirmation
  garde le numéro de celui qu'il remplace.
- **Rejeu** : le cloud demande `agent.replay` (`from`, `to`, 1000 au plus, topic
  `agent/cmd`). L'agent renvoie ce qu'il a encore, après le reste, sans `retain`, avec
  le même `id` et le même `seq`, et répond `agent.replayed` (`resent`, `missing`,
  `last`). Ce qui a été envoyé est gardé **en mémoire seulement** (5000 messages,
  2 Mo) pour ne pas user la carte SD : après un redémarrage de l'agent, les anciens
  numéros sont dans `missing`.

## Alertes pour les canaux du cloud

Quand le moteur demande une alerte par application ou WhatsApp :

1. L'agent l'envoie au cloud en priorité, sans la mettre derrière une longue file. Si
   internet est déjà coupé, il ne la met pas en file : il répond tout de suite
   `failed` avec `no_uplink`.
2. Le cloud confirme qu'il la prend en charge. Si l'agent n'a pas cette
   confirmation en **10 secondes**, il publie `alert.result` avec `failed` et la
   raison `no_uplink`. Le moteur passe alors au GSM, comme prévu dans
   `escalation.md`.
3. Le cloud renvoie ensuite `delivered` quand la notification arrive, puis
   l'acquittement quand un contact répond depuis l'application. L'agent les
   republie en `alert.result` et `alert.ack` sur le broker local.

## Ordres et configuration reçus

Tout ce qui descend du cloud est contrôlé avant d'être publié localement :

- **Seuls sont acceptés** : un ordre de l'application sur `alarm/cmd` (`origin` à
  `app`), une session vidéo sur `video/cmd` (un `video.stop` sans session est refusé), la configuration du site sur
  `config/set` (vérifiée : version, limites, numéros), un ordre de maintenance sur
  `maint/cmd` (une heure au plus, voir `maintenance.md`), et sur `alert/result` et
  `alert/ack` les réponses à une alerte que l'agent a transmise (un `alert.ack` doit
  venir de l'app et porter sur un incident que l'agent a vu passer). Le reste est
  refusé et enregistré dans le journal.
- L'enveloppe doit être valide, avec le bon `site`.
- Un message déjà vu (même `id`) est ignoré. Un message périmé (`exp` dépassée) est
  refusé.
- Il y a une limite de débit (20 d'un coup, 60 par minute par défaut), pour qu'un
  cloud défaillant ou piraté ne puisse pas noyer la centrale d'ordres.

La **configuration** (contacts, horaires, délais, forfait vidéo) est numérotée par
version. L'agent la transmet à la centrale, qui la vérifie, la garde sur disque et
redémarre à un moment calme pour l'appliquer, puis confirme la version en service
par `config.applied` (voir `contract.md`). En cas de coupure d'internet, la centrale
travaille avec la dernière configuration reçue.

Un ordre de désarmement venu du cloud ne suffit pas à désarmer : le moteur vérifie
toujours le code (voir `engine.md`). La façon dont le code arrive sur le site, au
moment où le propriétaire le choisit ou le change, est décidée dans le document
sur la sécurité.

## Photos

Les photos ne passent pas par MQTT, qui n'est pas fait pour des fichiers.

- Le service vidéo prend les images à l'alarme (voir `video.md`), les garde dans
  `/var/lib/safehome/photos` et annonce chacune (`photo.taken`).
- L'agent fait monter l'annonce comme un autre message. Le cloud répond avec une
  adresse d'envoi temporaire et un jeton (`photo.upload`), que l'agent garde pour
  lui : ils ne sont jamais republiés sur la centrale, ni écrits dans un journal.
- L'agent envoie le fichier par HTTPS (`PUT`, jeton en `Authorization: Bearer`), une
  photo à la fois, et seulement quand plus aucun message d'alarme n'attend dans sa
  file : les photos suivent les événements d'alarme, jamais devant. Il dit ensuite
  ce qu'il en est (`photo.sent`) et efface le fichier.
- Les images sont réduites (640 px de large en liaison fixe) pour tenir sur une
  liaison 4G. Sur une liaison 4G payée par le client, le **mode économe** s'applique :
  une seule image par caméra, 480 px.
- L'événement d'alarme part tout de suite sans attendre l'image. L'image est
  rattachée à l'incident quand elle arrive.
- Échecs : réseau ou cloud en panne, l'agent réessaie (30 s, puis deux fois plus,
  10 min au plus) tant que l'adresse vaut ; adresse expirée ou refusée, il le dit
  (`url_expired`) et le cloud en donne une autre ; une photo de plus de 24 h est
  abandonnée (`too_old`). Les photos suivies et leurs adresses survivent à un
  redémarrage de l'agent ; un envoi interrompu reprend au démarrage.
- L'agent HTTPS fait confiance à la même autorité que pour le broker du cloud
  (`ca_file`), ou à celles du système. Il ne suit pas les redirections (le jeton
  partirait avec).

## Données mobiles

Le heartbeat, avec la connexion maintenue, représente de l'ordre de 15 à 20 Mo par
mois. C'est une estimation à vérifier en test. Le reste du trafic est faible, hors
photos. Le mode économe protège un client dont le forfait est à sa charge.

**Mode économe** : l'installateur l'active par `"uplink": "mobile"` dans `agent.json`,
la même valeur que dans `video.json`.

- **Ce qui est espacé.** Le heartbeat et `agent.status` partent au cloud toutes les
  5 minutes au lieu de chaque minute, ce qui divise leur part par cinq.
- **Ce qui part comme avant.** Tout le reste : alarmes, alertes, états, santé.
- **Ce qui part tout de suite quand même.** Un changement de liaison (connexion
  rétablie, repli en WebSocket). Le cloud efface ainsi au plus vite le « hors ligne »
  laissé par le broker.
- **Après une coupure.** Le cloud reçoit le heartbeat le plus récent, pas les
  anciens.
- **Ce que voit le cloud.** `agent.status` porte `economy`, et le cloud sait alors
  qu'il doit attendre 15 minutes avant de déclarer le site muet.
- **Sur la centrale, rien ne change.** Le heartbeat et l'état de l'agent y restent
  publiés chaque minute, et la centrale voit toujours en 3 minutes que l'agent s'est
  tu.

Le maintien de la connexion (le « ping » MQTT toutes les 30 s quand rien ne passe)
coûte aussi des données. Il reste tel quel en attendant une mesure sur le kit :
l'espacer retarderait le moment où le cloud voit une centrale disparaître.

## Horloge

Un Raspberry Pi 4 n'a pas d'horloge qui survit à une coupure de courant : après une
panne, l'heure est fausse jusqu'à la synchronisation par internet. La centrale indique
dans le heartbeat si l'horloge est synchronisée (`clock_ok`, lu dans le noyau par
`internal/adapter/sysclock`). Le noyau cesse de la dire synchronisée après quelques
heures sans correction : après une longue coupure d'internet, `false` veut dire « pas
vérifiée depuis longtemps ». Le cloud ajoute sa propre
heure de réception, comme prévu dans `contract.md`. Un module d'horloge à pile
(quelques euros) règle le problème : à décider pour le kit.

## Authentification

- **Chaque centrale a ses propres identifiants**, fournis à l'installation. Elle
  ne peut publier que sous `safehome/{son site}/...` et lire que ses ordres et sa
  configuration. Le cloud applique ces droits.
- La connexion utilise TLS et vérifie le certificat du cloud.
- Pour la phase fermée, ces identifiants deviennent une clé propre à chaque
  centrale, avec signature des messages (champs `sig` et `kid` réservés dans le
  contrat).

Les détails (création des identifiants, révocation si une centrale est volée) sont
dans le document sur la sécurité et l'installation d'un site.

## Surveillance de l'agent

L'agent publie son état dans `agent.status` (voir `contract.md`) : liaison établie ou
non, protocole utilisé, taille de la file, ancienneté du plus vieux message en attente,
messages supprimés. La centrale en tire le lien `internet` de `system.health`, et
compte internet comme coupé si l'agent se tait plus de 3 minutes. L'escalade passe
alors directement aux canaux GSM.

## Tests

- **Faux cloud** (un broker MQTT local) pour tout tester sans serveur réel.
- Scénarios : coupure puis retour d'internet avec une file pleine, redémarrage de
  l'agent avec une file non vide, message en double, trou de `seq` et rejeu, ordre
  refusé (mauvais site, périmé, déjà vu), alerte sans confirmation en 10 s, port
  8883 bloqué avec repli sur 443, file pleine.

## Ce qui est codé

- Le cœur (`internal/agent`) : la file à priorités, ses bornes et sa sauvegarde, les
  contrôles de ce qui descend du cloud, le délai de confirmation des alertes. Il ne
  fait aucune entrée-sortie et ne lit pas l'horloge ; il ne connaît que le contrat.
- La couche application (`internal/agentapp`) : l'envoi au cloud un message à la fois
  avec reprise, la republication sur la centrale, `agent.status` chaque minute.
- Le programme (`cmd/safehome-agent`) et son service systemd. Le test de bout en bout
  fait tourner l'agent entre deux vrais brokers Mosquitto, dont un en TLS : alarme qui
  monte, ordre qui descend, configuration transmise, configuration sans version
  refusée, coupure d'internet avec alarme gardée puis envoyée au retour, numéros sans
  trou côté cloud et rejeu d'un message, annonce « hors ligne » à l'arrêt.
- Le repli WebSocket est testé contre Mosquitto 2.1. Celui-ci coupe la connexion quand
  un paquet MQTT arrive en plusieurs messages WebSocket ; l'adaptateur MQTT envoie
  donc chaque paquet dans un seul message (bibliothèque `gorilla/websocket`, déjà
  utilisée par la bibliothèque MQTT).

Le mode économe est codé dans le cœur (`internal/agent/economy.go`). Le test de bout en
bout couvre aussi une photo : annoncée,
adresse reçue, envoyée en HTTPS à un faux point d'arrivée du cloud, effacée.

## Décidé

- Liaison : MQTT sur TLS, avec repli en WebSocket sur le port 443.
- Photos : 3 images réduites par alarme, 1 seule en mode économe (4G payée par le
  client).
- Module d'horloge à pile dans le kit.

# Agent de liaison

Statut : v1 codée (`internal/agent`, `internal/agentapp`, `cmd/safehome-agent`), avec
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
| Local vers cloud | `alarm.*`, `alert.*`, `alarm/state`, `system/health`, `system/devices`, événements `smoke`, `panic`, `tamper`, `mains`, `battery` | Liste exacte dans `contract.md` |
| Local vers cloud | Heartbeat | Publié par la centrale chaque minute, relayé par l'agent |
| Local et cloud | `agent.status` | Créé par l'agent : à chaque changement et chaque minute |
| Local vers cloud | Photos | Envoi à part (voir « Photos ») |
| Cloud vers local | `alarm/cmd` | Ordres de l'application |
| Cloud vers local | `video/cmd` | Sessions vidéo en direct |
| Cloud vers local | Configuration (`config/set`) | Contacts, planning, délais, forfait vidéo : contrôlée, puis publiée sur la centrale |
| Cloud vers local | Résultats et acquittements des canaux du cloud | Republiés en `alert.result` et `alert.ack` |
| Cloud vers local | « Vu » dans l'application | Republié en `alert.seen` |

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

La session est persistante (un jour) : ce que le cloud publie pendant une absence de
l'agent l'attend sur le broker du cloud, sauf les ordres de l'application. L'agent lit
`alarm/cmd` en QoS 0, et le broker ne garde rien pour lui sur ce topic (voir « Ordres
et configuration reçus »).

Sur les deux brokers, l'agent n'accuse réception d'un message (QoS 1) qu'une fois que
sa boucle l'a pris, et elle fait tout ce qu'elle a pris, même quand elle s'arrête. Un
message qui arrive pendant l'arrêt (entre la fin de la boucle et la déconnexion) n'est
donc pas acquitté : le broker le garde pour la session suivante, au lieu de l'oublier
alors que personne ne l'a traité. Les accusés partent dans l'ordre des messages
(MQTT l'exige) : un message non pris retient ceux qui le suivent, que l'agent ne
prend pas non plus. Reste perdu ce que la boucle avait pris sans l'avoir fait quand le
programme meurt d'un coup (plantage, chien de garde, coupure), comme le reste de ce
qu'elle garde en mémoire.

L'agent ne se dit relié au cloud qu'une fois abonné à ses ordres. Si le broker ne
répond pas à l'abonnement (10 s), l'agent le redemande, avec le même délai croissant,
tant que la connexion tient.

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
  `system.devices`, `agent.status`, `video.status`, `config.applied`, `install.mode`),
  seul le dernier compte.
- À la reconnexion, la file part par priorité : d'abord l'alarme et les alertes,
  puis les événements, puis les états, et dans l'ordre d'arrivée à priorité égale.
- La file est bornée : 20 000 messages, 10 Mo ou 7 jours par défaut. Si elle est
  pleine, on supprime d'abord les messages les moins importants : les événements
  d'alarme et d'alerte ne sont jamais supprimés avant les autres. Les suppressions
  (file pleine, message trop vieux, refus du broker) sont comptées dans
  `agent.status` (`dropped`). Ce compte survit aux redémarrages.
- **Refus du broker du cloud** : un message refusé pour de bon (accusé de réception
  avec un code d'échec : non autorisé, invalide…) est supprimé. Renvoyé, il bloquerait
  toute la file derrière lui. Le journal donne son type, jamais son contenu. Un refus
  passager (quota dépassé) est réessayé, après 1 s, puis deux fois plus à chaque
  refus (30 s au plus) ; un changement de la liaison abrège l'attente, mais seulement
  s'il est venu après le choix du message refusé (corrigé le 26 octobre 2026 : un
  changement déjà vu abrégeait l'attente, et la suivante doublait pour rien). Une
  alerte pour l'application refusée ainsi échoue tout de suite : la centrale passe au
  GSM.
- **Sur la carte SD**, chaque message de la file est un petit fichier, dans le
  dossier `agent_queue` du répertoire d'état. Ajouter, numéroter ou envoyer un
  message n'écrit que son fichier, jamais toute la file. Le dernier numéro donné au
  cloud a son propre fichier de quelques octets (`agent_uplink.json`). Le reste de
  l'état (`agent.json`, jusqu'à 60 Ko avec les `id` déjà reçus du cloud) n'est pas
  réécrit pour chaque message. Un message parti laisse à la place de son fichier une
  trace de quelques octets, effacée 10 minutes plus tard (voir « Messages redonnés
  après leur envoi »). Vider 20 000 messages après une coupure écrit ainsi environ
  10 Mo de données, au lieu de 1,2 Go (mesuré par
  `TestDrainingTheQueueWritesItemsNotTheQueue` : 500 Ko pour 1000 messages, traces
  comprises). Après une coupure de courant, l'agent remet les fichiers d'accord au
  démarrage.
- **Numérotation pour le cloud** : chaque programme numérote ses propres messages, et
  beaucoup restent sur le site. L'agent numérote donc de nouveau ce qui part au cloud,
  à la première tentative d'envoi : là-bas, `seq` est continu par site, et un trou
  veut dire un message manqué. Un état ou un heartbeat remplacé avant confirmation
  garde le numéro de celui qu'il remplace. Le numéro est écrit sur disque avant
  l'envoi : il n'est jamais redonné, même après une coupure de courant.
- **États redonnés par le broker de la centrale** : à chaque connexion de l'agent, le
  broker local redonne les messages retenus. Un état déjà parti (même `id`) ne repart
  pas : il prendrait un nouveau numéro, que le cloud jetterait comme doublon, et le
  trou laissé ne se refermerait jamais. L'agent garde donc, par topic retenu, l'`id`
  du dernier état parti. Un état changé pendant un arrêt de l'agent a un autre `id` et
  part. Un état redit (voir ci-dessous) porte un `id` de l'agent, pas celui de la
  centrale : encore en file à un redémarrage, il ne remplace pas l'`id` gardé, sinon
  l'état de la centrale redonné par le broker repartirait sous un nouveau numéro. L'agent ne lit pas ses propres topics (`agent/status`, `agent/replayed`, `agent/moved`) sur
  la centrale.
- **Messages redonnés après leur envoi** : le broker de la centrale peut aussi redonner
  un événement, une alerte ou une annonce de photo déjà partis au cloud. Mosquitto ne
  sauvegarde ses sessions que toutes les 5 minutes (`autosave_interval`) : après une
  coupure de courant, il rend ce qu'il gardait pour l'agent à sa dernière sauvegarde,
  dont ce que l'agent avait pris et envoyé depuis ; et les accusés de réception vers
  le broker local partent par lots, perdus si la connexion se coupe. Remis en file, un
  tel message prendrait un nouveau numéro, que le cloud jetterait comme doublon : un
  trou qui ne se refermerait jamais. Une photo annoncée de nouveau serait suivie de
  nouveau, une alerte attendrait une confirmation qui ne peut pas venir. L'agent garde
  donc l'`id` de ce qui est parti pendant 10 minutes, sur disque : le fichier du
  message est remplacé à l'envoi par une trace de quelques octets (au lieu d'être
  effacé), effacée ensuite. Un message redonné dans ce délai, même après un
  redémarrage de l'agent, est écarté. Les états retenus sont reconnus autrement (voir
  ci-dessus).
- **L'état du moment à chaque connexion au cloud** : un cloud qui a perdu l'état de
  l'alarme, la santé ou la liste des appareils (base remise à zéro, ou cloud qui ne
  les gardait pas encore) n'aurait rien jusqu'au prochain changement. À chaque
  nouvelle connexion au cloud, l'agent redit donc le dernier `alarm.state`, le dernier
  `system.health` et le dernier `system.devices` de la centrale, comme des messages
  neufs : même contenu, même `ts`, mais un nouvel `id` (le cloud jette un `id` connu
  comme doublon). Ils passent par la file comme les autres : numérotés, confirmés,
  gardés pour un rejeu. Pas plus d'une fois par connexion, ni plus d'une fois par
  minute : sur une liaison qui saute (ou deux agents sur le même identifiant, une
  carte SD clonée, qui se chassent l'un l'autre), chaque connexion ajouterait sinon
  trois messages, trois fichiers de file et des octets payés en mode économe. Un état
  redit il y a moins d'une minute est arrivé au cloud ou attend encore dans la file.
  Un état qui attend déjà dans la file part tel quel. L'agent garde ces trois états en
  mémoire seulement : après un redémarrage, il les apprend quand le broker de la
  centrale les redonne, et les redit alors si la connexion au cloud est déjà là. Le
  cloud garde l'état le plus récent par son numéro : le même état redit n'y change
  rien. Ils ne sont pas republiés sur la centrale.
- **Rejeu** : le cloud demande `agent.replay` (`from`, `to`, 1000 au plus, topic
  `agent/cmd`). L'agent renvoie ce qu'il a encore, après le reste, sans `retain`, avec
  le même `id` et le même `seq`, et répond `agent.replayed` (`resent`, `missing`,
  `last`). Ce qui a été envoyé est gardé **en mémoire seulement** (5000 messages,
  2 Mo) pour ne pas user la carte SD : après un redémarrage de l'agent, les anciens
  numéros sont dans `missing`.

## Déménager vers une autre cellule

Le cloud de la cellule A peut ordonner à l'agent de passer à la cellule B
(`agent.move` sur `agent/cmd`, AD-20 du cloud) sans déplacer l'installateur :

1. **Gel** : dès l'ordre, plus rien ne part vers A. Ce qui monte reste dans la file
   sur disque, dans l'ordre, sans numéro neuf ni renumérotation. `agent.status` dit la
   liaison coupée : une alerte pour l'application ou WhatsApp échoue tout de suite
   (`no_uplink`), celles qui attendaient la confirmation du cloud aussi, et la centrale
   passe au GSM comme lors d'une coupure d'internet. Aucune photo ne part.
2. L'agent écrit la configuration neuve à côté de l'ancienne, sans la toucher :
   `cloud.next.json` (adresses et secret, 0600) dans son dossier
   `/var/lib/safehome/agent`. Le fichier `/etc/safehome/agent.json` est en lecture
   seule pour l'agent (`ProtectSystem=strict`) : il ne change jamais ; un cloud de
   déménagement le remplace. Chaque écriture passe par un fichier temporaire, vidé sur
   la carte, renommé, puis le dossier vidé : un arrêt entre deux écritures laisse
   l'ancienne configuration intacte.
3. L'agent redémarre (systemd le relance) et se connecte à B avec le même
   identifiant et le secret neuf, en réessayant jusqu'au délai (`give_up_after`, 600 s
   par défaut, 3600 au plus) compté depuis l'ordre : B refuse tant que A copie le site.
4. **B accepte** : `cloud.next.json` devient `cloud.json` (renommage), qui remplace le
   cloud de `agent.json` ; un `cloud.json` d'un déménagement précédent est gardé une
   fois (`cloud.previous.json`). La file part vers B, dans l'ordre, ses numéros à la
   suite de ceux que A a reçus ; les photos vont désormais aux hôtes de B. L'agent
   répond `agent.moved` (`to` : `b`) sur `agent/moved`.
5. **B injoignable au-delà du délai** : `cloud.next.json` est effacé, l'agent
   redémarre sur A, la file part vers A et `agent.moved` dit `back` avec
   `unreachable`. Une configuration neuve impossible à écrire renvoie de même vers A
   tout de suite (`not_saved`).

Le déménagement en cours est dans l'état sauvegardé de l'agent (sans le secret) : un
redémarrage au milieu reprend au même point, et le délai reste compté depuis l'ordre.
Après un arrêt entre l'écriture de la configuration neuve et la sauvegarde de l'état,
une configuration neuve sans déménagement connu est effacée (retour à A) ; un
déménagement connu sans sa configuration revient à A tout de suite (`not_saved`).
Un ordre périmé (`until` passé), mal formé, ou un second ordre pendant un
déménagement est refusé et journalisé : rien ne bouge. Le secret n'est jamais
journalisé. Pourquoi l'ordre porte un secret, et ce qui le borne : `contract.md`,
« Qualité de service sur le broker du cloud ».

## Alertes pour les canaux du cloud

Quand le moteur demande une alerte par application ou WhatsApp :

1. L'agent l'envoie au cloud avant tout le reste de la file, même devant une longue
   file d'événements d'alarme. Si internet est déjà coupé, il ne la met pas en file :
   il répond tout de suite `failed` avec `no_uplink`.
2. Le cloud confirme qu'il la prend en charge. Si l'agent n'a pas cette
   confirmation en **10 secondes**, il publie `alert.result` avec `failed` et la
   raison `no_uplink`. Le moteur passe alors au GSM, comme prévu dans
   `escalation.md`. De même si internet se coupe avant la confirmation. Le
   cloud peut pourtant avoir l'alerte (son broker la garde pendant qu'il
   redémarre) : l'agent se souvient une heure de celles qu'il a abandonnées, et
   les résultats qui arrivent ensuite pour elles ne sont pas republiés (un `sent`
   ou un `delivered` tardif dérangerait l'escalade, déjà passée au GSM). Le
   journal le dit en simple information, pas comme un refus.
3. Le cloud renvoie ensuite `delivered` quand la notification arrive, puis
   l'acquittement quand un contact répond depuis l'application. L'agent les
   republie en `alert.result` et `alert.ack` sur le broker local.

## Ordres et configuration reçus

Tout ce qui descend du cloud est contrôlé avant d'être publié localement :

- **Seuls sont acceptés** : un ordre de l'application sur `alarm/cmd` (`origin` à
  `app`, et seulement `alarm.arm`, `alarm.arm_now`, `alarm.disarm`, `alarm.silence` ou
  `alarm.end` : contourner des zones ou reporter l'armement automatique se fait sur
  place, jamais de loin), une session vidéo sur `video/cmd` (un `video.stop` sans session est refusé), la configuration du site sur
  `config/set` (vérifiée : version, limites, numéros), un ordre de maintenance sur
  `maint/cmd` (une heure au plus, voir `maintenance.md`), et sur `alert/result` et
  `alert/ack` les réponses à une alerte que l'agent a transmise (un `alert.ack` doit
  venir de l'app et porter sur un incident que l'agent a vu passer), et sur `alert/seen`
  le « Vu » de l'application (`alert.seen`, un incident que l'agent a vu passer, un
  contact et une heure) ; et pour l'agent
  lui-même `photo.upload` sur `photo/cmd`, `agent.replay` et `agent.move` sur
  `agent/cmd`, jamais republiés sur la centrale. Le reste est refusé et enregistré dans le journal.
- **Aucun ordre avec un code n'attend sur le broker du cloud.** L'agent s'abonne à
  `alarm/cmd` en QoS 0, à tous les autres topics du cloud en QoS 1. Avec une session
  gardée un jour et un abonnement en QoS 1, Mosquitto garderait pour un agent absent
  un `alarm.disarm` ou un `alarm.silence`, code compris, en mémoire et dans son fichier,
  même périmé (la durée de vie MQTT 5 limite la remise, pas la garde : vérifié par le
  cloud sur Mosquitto 2.0.22). En QoS 0, il ne garde rien. Un ordre ne vaut qu'une
  minute : celui donné pendant une coupure, même courte, n'est pas exécuté au retour,
  et le téléphone le voit `expired`. Les jetons de `video/cmd` et de `photo/cmd` sont
  ceux du cloud, pour une session ou une photo : ils restent en QoS 1, pour qu'un
  `video.stop` ou une adresse d'envoi arrivent encore après une coupure. Le tableau est
  dans `contract.md` (« Qualité de service sur le broker du cloud »). Cela suppose que
  le broker du cloud garde `queue_qos0_messages false` (la valeur par défaut de
  Mosquitto, à écrire dans sa configuration) : à `true`, il garderait aussi ces ordres.
- Un ordre de l'application passe sur la centrale en QoS 0 lui aussi : le broker de
  la centrale ne le garde pas pour une centrale qui redémarre (voir `systeme.md`,
  étape 6). Les autres messages du cloud y passent en QoS 1.
- L'enveloppe doit être valide, avec le bon `site`.
- Un message déjà vu (même `id`) est ignoré. Un message périmé (`exp` dépassée) est
  refusé.
- Il y a une limite de débit (20 d'un coup, 60 par minute par défaut), pour qu'un
  cloud défaillant ou piraté ne puisse pas noyer la centrale d'ordres. Un message
  refusé par cette limite ne change rien à ce que l'agent sauvegarde : un flot ne
  devient pas un flot d'écritures sur la carte SD. Les réponses à ce que l'agent a
  envoyé (`alert.result`, `alert.ack`, `alert.seen`, `photo.upload`), vérifiées contre ce qu'il a
  envoyé, n'y comptent pas : leur nombre croît avec les contacts et les canaux d'une
  grande alarme (trois résultats par alerte), et une réponse refusée serait perdue
  pour de bon (un acquittement, une photo jamais envoyée). Elles ont leur propre
  limite, cinq fois plus large (100 d'un coup, 300 par minute par défaut) : chacune
  passée s'écrit sur la carte SD et part vers la centrale, et un cloud défaillant qui
  en enverrait des milliers, chacune avec un nouvel `id`, ne doit pas en faire un flot.
  Corrigé le 25 octobre 2026 après une revue : elles n'avaient aucune limite. Une
  limite sauvegardée avec
  une heure à venir (l'horloge avançait, puis a reculé, voir « Horloge ») est rendue
  entière, une fois, à partir de maintenant : attendre que l'horloge la rattrape
  refuserait chaque ordre de l'application pendant tout ce temps.
- **Un ordre de l'application qui ne va pas à la centrale reçoit une réponse tout de
  suite** : sans elle, le cloud attendrait plus d'une minute pour conclure `expired`.
  L'agent publie lui-même `alarm.ack` vers le cloud, `ref` à l'`id` de l'ordre :
  - ordre périmé (`exp` dépassée à l'arrivée) : `expired`, sans raison, comme le moteur ;
  - ordre au-delà de la limite de débit : `refused` avec `rate_limited`. Pas
    `locked_out`, qui dit au téléphone que trois codes faux ont verrouillé le
    désarmement pour dix minutes, alors que rien n'a été vérifié et qu'on peut
    réessayer dans quelques secondes.

  Seul un ordre qui a passé les contrôles reçoit cette réponse : topic et enveloppe du
  site, type attendu, `origin` à `app`, ordre permis à l'application. Les autres sont refusés sans réponse, comme
  avant. La réponse ne reprend rien de l'ordre sauf son `id` (jamais le code). L'ordre
  répondu est gardé parmi les `id` déjà vus : redonné plus tard par le broker, il ne
  s'exécute pas après un `refused`. La réponse part au cloud seulement, sans passer par
  la centrale, qui n'a jamais eu l'ordre ; elle suit la file comme les autres messages
  d'alarme.
- Les réponses aux ordres au-delà de la limite ont leur propre limite, de la même
  taille (20 d'un coup, 60 par minute) : un flot d'ordres n'obtient pas plus de réponses
  que d'ordres transmis (chaque réponse s'écrit sur la carte SD), le reste est refusé
  sans réponse, et ces réponses ne prennent rien sur la limite des ordres.

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
- L'adresse n'est acceptée que si son hôte est celui du cloud (`cloud.urls`), sur le
  port https ou celui d'une de ces adresses : la photo et son jeton ne partent jamais
  vers un autre hôte, ni vers le réseau du magasin (règle dans `contract.md`, photos).
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
  5 minutes au lieu de chaque minute, ce qui divise leur part par cinq. Les 5 minutes
  comptent depuis l'entrée du message dans la file, pas depuis la réponse du cloud.
- **Ce qui part comme avant.** Tout le reste : alarmes, alertes, états, santé.
- **Ce qui part tout de suite quand même.** Un changement de liaison (connexion
  rétablie, repli en WebSocket). Le cloud efface ainsi au plus vite le « hors ligne »
  laissé par le broker. L'état de l'alarme, la santé et la liste des appareils redits
  à chaque connexion partent aussi (environ un kilo-octet par connexion).
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
heure de réception, comme prévu dans `contract.md`. Il faut donc une horloge à pile
(`fake-hwclock` retiré, voir `systeme.md`, étape 4) : celle du module batterie X728 si
sa carte en porte bien une (ses fiches l'annoncent, à confirmer à la réception), sinon
un module DS3231 (voir `kit.md`). L'heure est alors juste dès le démarrage, mais
`clock_ok` reste à `false` jusqu'au retour d'internet, le noyau ne connaissant que la
synchronisation par NTP. À valider avec le module choisi. Si l'horloge se trompe
malgré tout puis recule, le moteur ne prolonge ni un verrouillage ni le compte des
codes faux, ni ses délais, après un redémarrage comme pendant que la centrale tourne
(voir `engine.md`). L'agent rend entière une limite de débit datée dans le futur
(voir plus haut) ; mais pendant qu'il tourne, son temps s'arrête jusqu'à ce que
l'horloge le rattrape (voir `architecture.md`) : seul ce qui restait de ses limites
passe pendant ce temps. L'échéance (`exp`) d'un message du cloud est jugée, elle, sur
l'horloge telle qu'elle est : le cloud la date sur son heure, juste, et le temps
arrêté de l'agent la croirait dépassée pour tout ce temps (corrigé le 25 octobre 2026
après une revue : après un recul de l'horloge, chaque ordre de l'application était
refusé `expired`).

Une horloge qui saute en avant (NTP qui corrige au démarrage le Pi sans module
d'horloge, parti de sa dernière heure connue) ne fait rien expirer : la boucle le voit
sur l'horloge monotone (voir `architecture.md`) et l'agent avance d'autant ce qu'il
chronomètre (`Relay.Stepped`) : l'attente de la confirmation d'une alerte par le cloud
(sinon elle échouait aussitôt, et la centrale passait au GSM pour rien), ce dont il se
souvient un moment (alertes, incidents, messages arrivés au cloud, photos, états redits,
messages répétés), ses limites de débit, et l'âge des messages en file, en mémoire
seulement : leur copie sur disque n'est pas réécrite, la file les garde une semaine.
L'échéance d'une adresse de photo, donnée par le cloud, ne bouge pas (26 octobre 2026).

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
  l'agent avec une file non vide, état du moment redit à chaque connexion, message en
  double, trou de `seq` et rejeu, ordre
  refusé (mauvais site, déjà vu), ordre donné pendant une coupure jamais remis, ordre périmé ou au-delà de la limite répondu tout de
  suite, alerte sans confirmation en 10 s, port 8883 bloqué avec repli sur 443, file
  pleine.

## Ce qui est codé

- Le cœur (`internal/agent`) : la file à priorités, ses bornes et sa sauvegarde, la
  numérotation et le rejeu, l'état du moment redit à chaque connexion
  (`internal/agent/restate.go`), les photos, les contrôles de ce qui descend du cloud et
  la réponse aux ordres de l'application qui ne vont pas à la centrale
  (`internal/agent/down.go`), le délai de confirmation des alertes. Il ne
  fait aucune entrée-sortie et ne lit pas l'horloge ; il ne connaît que le contrat.
- La couche application (`internal/agentapp`) : l'envoi au cloud un message à la fois
  avec reprise, la republication sur la centrale, `agent.status` chaque minute.
- Le programme (`cmd/safehome-agent`) et son service systemd. Le test de bout en bout
  fait tourner l'agent entre deux vrais brokers Mosquitto, dont un en TLS : alarme qui
  monte, ordre qui descend, configuration transmise, configuration sans version
  refusée, coupure d'internet avec alarme gardée puis envoyée au retour, ordre avec
  code donné pendant la coupure jamais remis (et configuration donnée en même temps
  remise au retour), numéros sans
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
- Une horloge à pile dans le kit : celle du X728 si sa carte en porte une (à
  confirmer), sinon un module DS3231.

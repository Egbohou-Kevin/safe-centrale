# Architecture safeHome

Statut : en vigueur, vérifiée par `internal/archtest`.

## Objectif

Système de télésurveillance pour les commerces au Togo (Lomé d'abord), avec un
abonnement autour de 20 000 à 25 000 FCFA par mois. On teste d'abord sur un cas
d'usage réel avec du matériel du commerce. À terme, le système devient fermé et
utilise nos propres produits.

Contraintes de la région : coupures de courant fréquentes, internet fixe peu
fiable, données mobiles chères, SMS et WhatsApp très utilisés, paiement par
mobile money.

## Principe : le site se suffit à lui-même

Tout ce qui protège le commerce tourne sur place. Le cloud supervise et notifie.
Une coupure d'internet ou du serveur ne doit jamais empêcher la sirène ni les
SMS.

```
 SUR PLACE (commerce)                                   CLOUD
┌─────────────────────────────────────────┐       ┌──────────────────────────┐
│ Capteurs Zigbee ─┐                      │       │ Ingestion (API)          │
│ Sirène Zigbee ◄──┤ Zigbee               │       │ Base de données          │
│ Clavier / SOS ───┘                      │       │ Alertes et escalade      │
│        │                                │       │ Surveillance des         │
│  Zigbee2MQTT ◄── clé Zigbee             │       │   heartbeats             │
│        │ MQTT                           │       │ App client + console     │
│  Broker MQTT (Mosquitto)                │       │ Facturation (mobile      │
│        │ MQTT                           │       │   money)                 │
│  Moteur d'alarme ──► Passerelle GSM ──► SMS/appel        ▲                │
│        │                  (modem 4G)    │       └────────┼─────────────────┘
│        ├──► Service caméras (RTSP)      │                │ HTTPS / MQTT TLS
│        └──► Agent de liaison ───────────┼────────────────┘ (sortant seulement)
└─────────────────────────────────────────┘
```

Pour les alertes, **internet est le canal préférentiel** (application, WhatsApp),
et le GSM (SMS, appel) est le canal de secours, qui double l'urgence quand
internet est coupé ou qu'on n'obtient pas de réponse. Cela limite le coût du GSM
et des données mobiles. Détail dans `escalation.md`.

Liaison internet de la centrale : la plupart des commerces ont une box Wi-Fi, qui
est la liaison principale. La 4G sert de secours quand la box tombe. Un client
sans box peut utiliser la 4G comme liaison principale, avec un forfait de données
à sa charge. Dans ce cas, seul le coût des SMS est économisé.

Les SIM de la centrale et leur crédit sont à la charge du client. Nous assurons
que le système fonctionne : surveillance de l'état des SIM, test périodique et
avertissement du client quand le canal de secours est dégradé.

Les canaux d'alerte sont interchangeables : chaque canal est un module et la
politique de canaux (quel canal, dans quel ordre, après quel délai) est une
configuration. Détail dans `escalation.md`.

## Architecture hexagonale

Le code suit une architecture hexagonale, adaptée à Go plutôt que recopiée de Java.

```
          adaptateurs (entrées et sorties)
   MQTT · disque · horloge · Zigbee · modem GSM · cloud
                       │
              engine/wire  (traduction)
                       │
        ┌──────────────┴──────────────┐
        │   domaine : engine           │   ne connaît ni le format des messages,
        │   (événements du domaine)    │   ni le réseau, ni le disque, ni l'horloge
        └──────────────────────────────┘
```

- **Les domaines** (`internal/engine` pour l'alarme, `internal/health` pour la
  supervision de la centrale et des appareils, `internal/access` pour les codes
  d'accès, `internal/escalation` pour l'escalade des alertes, `internal/gsm` pour la
  passerelle GSM, `internal/zigbee` pour l'adaptateur Zigbee, `internal/video` pour la
  vidéo en direct, `internal/maint` pour la maintenance à distance) sont purs : ils
  dépendent seulement de la bibliothèque standard, n'ont aucune entrée-sortie et ne
  lisent jamais l'horloge. Ils reçoivent des entrées qui portent leur heure et
  renvoient des actions et des événements du domaine.
- **Le contrat** (`internal/contract`) est le langage publié. Il ne dépend de rien.
- **La traduction** (un paquet `wire` par domaine : `engine`, `health`, `escalation`,
  `gsm`, `zigbee`, `video`, `maint`) est le seul endroit qui traduit entre un domaine et
  le contrat. Si le contrat change de version, seules ces parties changent. Les
  domaines ne se connaissent pas : la couche application (`internal/hub`) les relie.
  Le pilote ne connaît aucun domaine.
- **Les adaptateurs et le pilote** viennent autour : MQTT, disque, horloge, vérification des
  codes. Leurs interfaces se déclarent côté consommateur et restent petites. Une
  bibliothèque externe n'est importée que par le paquet d'adaptateur qui l'enveloppe
  (par exemple `internal/adapter/mqtt` pour le client MQTT) : le reste du code ne la
  voit pas, et la changer ne touche qu'un paquet.
- **Le pilote** (`internal/driver`) fait tourner un domaine en temps réel. Une seule
  goroutine possède le domaine (il n'a donc besoin d'aucun verrou) : tout ce qui veut
  l'atteindre, message de capteur, ordre de l'application ou échéance, devient une tâche
  exécutée à son tour. Après chaque tâche, la boucle envoie les actions au monde
  extérieur, puis sauvegarde l'état (les effets d'abord : mieux vaut une sirène
  déclenchée et un état en retard qu'un état sauvegardé pour des actions que personne
  n'a faites). La boucle est générique : elle ne connaît ni le moteur ni la supervision,
  seulement des fonctions (`Core`). Le temps, le stockage et la sortie sont des ports :
  les tests la font tourner avec une horloge simulée. Quand elle s'arrête, elle ne
  prend plus de tâche et fait celles qu'elle a déjà prises : un programme peut dire au
  broker qu'il a pris un message dès que la boucle l'a accepté (option `Take` de
  l'adaptateur MQTT, utilisée par l'adaptateur Zigbee et l'agent). Le temps ne recule jamais pour le
  domaine, même si l'horloge système est corrigée en arrière : la boucle lui donne
  l'heure murale, sans la lecture monotone que Go ajoute à `time.Now` (deux heures qui
  en portent une se comparent par elle seule, ce qui cacherait le recul). Ce garde a un
  prix : après un recul de X, le temps du domaine s'arrête jusqu'à ce que l'horloge le
  rattrape, et chaque échéance posée ou tenue pendant ce temps arrive X plus tard (une
  étape d'escalade, une ligne GSM vérifiée). Le moteur d'alarme y échappe : sa boucle
  suit l'horloge telle qu'elle est, en arrière aussi (option `FollowsClock`), et le
  moteur ramène lui-même ce qu'une horloge en avance a daté dans le futur (voir
  `engine.md`, « une horloge qui a avancé puis reculé »). L'adaptateur Zigbee aussi :
  le `last_seen` de Zigbee2MQTT recule avec l'horloge, et un temps arrêté aurait pris
  chaque nouveau message pour un ancien (voir `adapter-zigbee.md`, « Cache rejoué »).
  Les autres domaines gardent le garde : il leur faudrait chacun la même reprise. Le
  garde ne vaut que pendant une exécution : un nouveau processus part de l'horloge
  telle qu'elle est. Un saut de l'horloge en avant (NTP qui corrige au démarrage un
  Pi sans module d'horloge, parti de sa dernière heure connue) se voit sur l'horloge
  monotone (`driver.Monotonic`) : la boucle le dit au cœur qui le demande
  (`Core.Stepped`), qui avance d'autant ce qu'il chronomètre. Chaque cœur qui mesure
  une durée sur l'heure lue le demande : l'adaptateur Zigbee (le silence des appareils,
  26 octobre 2026), puis le moteur (délais, sirène, verrouillage), la supervision (le
  silence des services), l'escalade (vagues et relances), la passerelle GSM (l'âge des
  demandes), l'agent (l'attente de confirmation du cloud), l'alimentation (la coupure
  sur batterie vide) et la vidéo (les sessions). Seules les durées avancent : une heure
  du jour (l'armement automatique, le résumé quotidien) et les heures publiées (`at`,
  `since`, `incident_at`) restent ce qu'elles sont, et ce qui est publié après le saut
  est daté sur l'heure juste. La maintenance ne le demande pas : sa fin borne un accès
  à distance, qu'un saut raccourcit (le côté sûr) sans jamais le prolonger, et l'ouvrir
  demande le cloud, donc internet, donc une heure déjà juste. `driver.Every` répète un
  travail (un statut, le battement de cœur) sur la même horloge. La fausse horloge des
  tests (`drivertest`) fait comme la vraie : ses minuteries suivent une horloge
  monotone, que `Set` ne bouge pas.
- **La couche application** (`internal/hub`) relie tout cela pour la centrale : elle
  reçoit les messages MQTT, les traduit (`wire`), les remet aux domaines par leurs
  boucles, et publie ce qui en sort. Elle publie par le port de la file d'envoi
  (`outbox.Publisher`) et réutilise ceux du pilote (horloge, stockage). La fenêtre de
  démarrage attend sur l'horloge du pilote. Elle ne connaît aucun adaptateur : les
  tests la font tourner avec un faux broker et une horloge simulée. Aucun domaine ne
  l'importe. Les outils de l'installateur changent les codes par `hub.SiteCodes` : les
  codes des personnes et celui de l'installateur, pour les contacts du site, et un code
  n'ouvre qu'une chose.
- **La passerelle GSM** a la même forme : son domaine (`internal/gsm`), sa couche
  application (`internal/gateway`, qui déclare le port `Modem`), son adaptateur
  (`internal/adapter/atmodem`). La file d'envoi MQTT (`internal/outbox`) est partagée
  par toutes les couches application des programmes de la centrale ; elle connaît le
  contrat et le pilote, et aucun domaine. `Send` encode un message tout de suite et le
  met en file, avec un identifiant tiré de la fonction donnée à `Open` ; un message
  qu'on ne peut pas encoder est écrit au journal et laissé de côté. La file est en
  mémoire et bornée (`Settings.Size` : 2000 messages pour le hub, 1000 pour l'agent et
  Zigbee, 500 pour le GSM, 200 pour la vidéo, 50 pour les autres) :
  un redémarrage perd ce qu'elle tenait. Pleine, elle jette le plus ancien message de
  la plus basse priorité, déduite du topic (`priorityOf`, même règle que la file de
  l'agent vers le cloud) ; les autres partent dans leur ordre. Haute : `alarm/*`,
  `alert/*`, `device/+/cmd` et `device/+/ack` (la sirène et sa réponse), et tout topic
  hors contrat (une demande à Zigbee2MQTT : une sirène à faire sonner). Basse : les
  états périodiques (`device/+/state`, `system/heartbeat`, `system/health`,
  `system/devices`, `registry/devices`, `gsm/status`, `zigbee/status`, `video/status`,
  `agent/status`). Normale : le reste (événements des appareils, sessions, autres
  ordres, `system/contacts`, `config/applied`, `install/mode`). Un message retenu vide,
  qui efface l'état gardé par le broker (celui d'un appareil retiré : une porte restée
  « ouverte » bloquerait toute mise en marche), est au moins normal, même sur
  `device/+/state`. Si elle ne tient que
  des messages de priorité haute, elle jette le plus ancien et le dit au journal
  (« only alarm messages »). Les tests des
  couches application partagent son faux broker (`outboxtest.Broker`). La porte
  d'entrée (`internal/inbox`) aussi :
  le message reçu, les abonnements, la vérification d'un message contre le contrat
  (`Open`), les identifiants déjà vus. Elle ne connaît que le contrat.
- **L'adaptateur Zigbee** aussi : son domaine (`internal/zigbee`), sa couche
  application (`internal/zigbeeapp`). Zigbee2MQTT parle déjà MQTT : il n'y a pas
  d'adaptateur technique de plus, mais un paquet de traduction (`internal/zigbee/z2m`),
  seul à connaître les messages de Zigbee2MQTT, comme `wire` est seul à connaître le
  contrat.
- **L'agent de liaison** : un cœur (`internal/agent`) qui n'est pas un domaine au sens
  du DDD (il ne connaît que le contrat, puisque son métier est de le transporter), mais
  qui suit les mêmes règles : ni entrées-sorties ni horloge, tournant dans la boucle du
  pilote. Sa couche application (`internal/agentapp`) déclare deux ports de
  publication, la centrale et le cloud, sur lesquels la racine de composition branche
  deux clients MQTT. Elle déclare aussi le port `QueueStore` : la file pour le cloud,
  un petit fichier par message (`internal/adapter/queuedir`).
- **La vidéo** a la forme de la passerelle : son domaine (`internal/video`, sessions et
  plafond), sa traduction (`internal/video/wire`), sa couche application
  (`internal/videoapp`, qui déclare le port `Streamer`) et son adaptateur
  (`internal/adapter/go2rtc`, qui lance un go2rtc par session).
- **L'outil de l'installateur** : une couche application (`internal/install`) sans
  domaine propre, avec ses ports (`Publisher`, messages reçus, `Sites`) et sans
  horloge ; l'outil en ligne de commande (`cmd/safehome-install`) et la console locale
  s'appuient dessus. Les règles de zones viennent du moteur : la table des capteurs
  acceptés par chaque type de zone est dans `internal/engine` (`engine.Accepts`).
  `internal/install` est seul à modifier le site (zones et sirènes) : son `Editor`
  lit, change et écrit par le port `Sites`, pour la console comme pour l'outil.
  L'adaptateur de ce port est `hubconfig.SiteFiles`, qui garde les zones et les
  sirènes dans le dossier d'état.
- **La console locale** : sa couche application (`internal/consoleapp`) suit le mode
  installation et déclare ses ports (`Hotspot`, `Sites`, `Codes`, `Serve`). `Sites` est
  `install.Sites` ; `Codes` a `People` (les contacts, et à part les codes d'anciens
  contacts), `Set` (qui refuse qui n'est pas un contact) et `Remove`. Sa page
  (`internal/consoleapp/web`) ne connaît qu'elle. Le Wi-Fi est l'adaptateur
  `internal/adapter/hotspot` (`nmcli`).
- **La maintenance à distance** a la forme de la vidéo : son domaine (`internal/maint`,
  une session à la fois, une heure au plus), sa traduction (`internal/maint/wire`), sa
  couche application (`internal/maintapp`, qui déclare le port `Tunneler`, sans
  horloge) et son adaptateur (`internal/adapter/sshtunnel`, le client OpenSSH vers le
  bastion). Détail dans `maintenance.md`.
- **La configuration réglée depuis le cloud** (`config.set`) : sa couche application
  (`internal/configapp`) tourne dans le programme de la centrale. Elle ne connaît ni
  fichier ni processus : vérifier, garder, redémarrer et savoir si le moment est calme
  sont des ports (`Check`, `Save`, `Restart`, `Quiet`), et elle ne lit pas l'horloge.
  Elle redémarre aussi la centrale, à un moment calme, quand `site-devices.json` a
  changé (port `DevicesChanged`) : elle le vérifie à chaque état de l'alarme, mode
  installation ou message de configuration. `internal/hub` ne fait que répondre à
  `Quiet` (`Hub.Quiet`) : le moteur dit son propre état, après les entrées déjà en
  file, car l'état publié sur `alarm/state` revient par le broker en retard sur lui.
  La règle de ce qui est calme (`configapp.QuietState` : désarmé ou armé ; pour des
  zones changées, désarmé ou le délai de sortie) reste dans `configapp`, qui la passe
  au port : les deux couches application ne s'importent pas.
  Son travail (vérifier et écrire une configuration sur la carte SD, interroger le
  moteur) se fait sur sa propre goroutine, jamais sur celle du client MQTT, qui porte
  tous les messages de la centrale. Un arrêt normal finit ce travail ; un plantage
  peut perdre une configuration reçue à l'instant, sans `config.applied` pour elle.
  Le fichier qui garde cette
  configuration dans le dossier d'état est `internal/siteconfig`, qui ne connaît que le
  contrat ; la centrale, la passerelle GSM et la vidéo le lisent au démarrage.
- **L'arrêt** a la même forme dans toutes les couches application qui ont une boucle
  (centrale, passerelle, Zigbee, agent, vidéo, maintenance). Tout tourne sur un
  contexte `life`, annulé dès que le contexte de `Run` se termine. Puis `Run` attend
  la fin de ses goroutines et fait un dernier envoi de la file (`Flush`, 3 s au plus).
  Un message qui arrive pendant ce temps est abandonné sans erreur au journal ;
  l'adaptateur Zigbee et l'agent ne l'acquittent pas, et le broker le leur redonne au
  démarrage suivant. Les autres programmes l'acquittent encore, et il est perdu (un
  état retenu revient à la connexion suivante, un événement non). Dans la centrale, la
  boucle de l'escalade a son propre contexte et s'arrête après celles du moteur et de
  la supervision : ce qu'elles font encore pendant l'arrêt (une tâche déjà prise, une
  échéance qui tombe à ce moment) est sauvegardé comme fait, et l'alerte d'une alarme
  levée alors, ou la fin d'une escalade, doit encore arriver à l'escalade. Avant le
  25 octobre 2026, elle pouvait être perdue sans trace : la sirène reprenait au
  redémarrage, mais personne n'était jamais prévenu.
- **Les lectures de configuration** (`internal/hubconfig`, `internal/gsmconfig`, etc.)
  partagent `internal/configfile` : JSON strict, niveau de journal, durées, secrets,
  écriture atomique. L'en-tête commun des fichiers (`site`, `broker_url`, `client_id`,
  `state_dir`, `log_level`) est `configfile.ProgramFile` ; sa méthode `Build` en fait un
  `configfile.Program`, valeurs par défaut comprises. Il ne connaît rien du module, et seuls les paquets `*config`
  l'importent : aucun domaine ni aucune couche application ne lit de fichier. Depuis le
  29 octobre 2026, `Program` porte aussi le nom du programme sur le broker local et son mot de
  passe, lu dans le dossier que systemd lui donne (`CREDENTIALS_DIRECTORY`, `LoadCredential=`) ;
  `AsOperator` lit celui de l'opérateur (`/etc/safehome/mqtt/operator.secret`) pour un outil
  lancé sous sudo. Une erreur nomme le fichier, jamais ce qu'il contient.
- **`internal/service`** est la part de la racine de composition commune à tous les
  programmes : ligne de commande, verrou du dossier d'état, clients MQTT, dialogue
  avec systemd, arrêt propre. `service.Reload` envoie SIGHUP au programme d'un fichier
  de pid : la centrale relit alors ses codes. `service.Local` remplit l'adresse, l'identifiant,
  le nom et le mot de passe d'un client du broker local à partir de la configuration
  (`service.LocalBroker`) : chaque programme passe par lui, pour n'en oublier aucun (29 octobre
  2026). Il ne connaît que quelques adaptateurs, la porte
  d'entrée et la file d'envoi, rien des domaines ni des couches application, et seul `cmd/` l'importe.
- **Les racines de composition** (`cmd/safehome-hub`, `cmd/safehome-gsm`,
  `cmd/safehome-zigbee`, `cmd/safehome-agent`, `cmd/safehome-video`, `cmd/safehome-maint`,
  `cmd/safehome-console`, `cmd/safehome-install`) sont les seuls endroits autorisés à
  importer les adaptateurs, avec `internal/service` (l'assemblage commun que seul
  `cmd/` importe) : elles construisent le vrai client MQTT, le vrai disque et la vraie
  horloge et les passent à la couche application.
- **DDD léger** : on le réserve aux contextes qui ont des règles (alarme, escalade). L'agent
  et la passerelle sont surtout de la plomberie et n'en ont pas besoin.

Ces règles sont vérifiées par des tests (`internal/archtest`). Ils échouent :

- si un domaine importe un paquet du module (le contrat compris), une bibliothèque
  tierce ou un paquet d'entrées-sorties, ou s'il lit l'horloge ;
- si le contrat importe un paquet du module ;
- si un paquet `wire` connaît autre chose que son domaine et le contrat, ou fait des
  entrées-sorties ; si `internal/zigbee/z2m` connaît autre chose que le domaine Zigbee ;
- si le pilote ou un adaptateur importe un paquet du module ; si le pilote lit
  l'horloge ailleurs que dans `SystemClock` ;
- si la file d'envoi connaît autre chose que le contrat et le pilote, ou son faux
  broker autre chose qu'elle et le contrat ; si la porte
  d'entrée connaît autre chose que le contrat, fait des entrées-sorties ou lit
  l'horloge ;
- si le cœur de l'agent connaît autre chose que le contrat ou lit l'horloge ; si
  `agentapp`, `videoapp`, `maintapp`, `configapp`, `install` ou `consoleapp` (et sa
  page) dépassent la liste de paquets qui leur est permise (aucun adaptateur), ou,
  pour certains, lisent l'horloge ou font des entrées-sorties ;
- si `internal/siteconfig` connaît autre chose que le contrat et `internal/configfile` ;
  si `internal/configfile` importe un paquet du module, lit l'horloge, ou est importé
  hors des paquets `*config` ;
- si un adaptateur est importé ailleurs que dans `cmd/` et `internal/service` ; si
  `internal/service` connaît autre chose que ses adaptateurs, la porte d'entrée et la
  file d'envoi, ou est
  importé hors de `cmd/` ;
- si une couche application (`hub`, `gateway` et tout paquet `*app`, y compris ceux
  ajoutés plus tard) lit l'horloge ou attend sans passer par le port `driver.Clock` ;
- si un domaine, le contrat ou le pilote importe la couche application de la centrale ;
- si une bibliothèque externe est importée hors de son adaptateur, ou si du code livré
  importe un outil de test (`schematest`, `mqtttest`, `attest`, `go2rtctest`, `drivertest`,
  `outboxtest`) ;
- si du code livré lance un programme (`os/exec`) hors d'un adaptateur ou d'un programme
  de `cmd/` (29 octobre 2026, étude de sécurité de la centrale) ;
- si un programme de `cmd/` bâtit un `mqtt.Options` du broker local sans `service.Local`
  (un `mqtt.Options` écrit à la main doit nommer `Username` et `Password`, comme le client du
  cloud de l'agent), ou ne passe jamais par `service.Local` (29 octobre 2026, le broker local
  exige un mot de passe par programme).

### Les garde-fous de qualité

`internal/qualitytest` empêche le code de revenir en arrière sur la lisibilité. Ses
tests tournent avec `go test ./...` (environ une seconde) :

| Règle | Seuil |
|---|---|
| `size` : longueur d'une fonction, d'un fichier | 50 et 400 lignes ; 160 et 750 lignes pour les tests |
| `gofmt` : chaque fichier `.go` est formaté | – |
| `ascii` : fichiers `.go` en ASCII seulement (le français va dans les gabarits et la doc) | – |
| `duplication`, `test-duplication` : copier-coller, même avec des variables renommées | plus de 130 lexèmes (12 à 15 lignes) ; 200 pour les tests |
| `fakes` : pas de copie des faux partagés (`drivertest`, `outboxtest`) ni des aides fusionnées (`decodeOrder`, `statusRepeater`…) ; un test peut les embarquer | – |
| `dead` : un identifiant exporté de `internal/` que rien n'utilise hors des tests de son paquet (sauf méthodes d'interface, paquets de test, `cmd/`) | – |
| `slog` : l'erreur d'un journal sous la clé `err`, jamais `error` | – |
| `sleep` : pas de `time.Sleep` dans `internal/` hors adaptateurs et paquets de test | – |
| `marker` : la méthode qui scelle une interface porte son nom (`isAction` pour `Action`) | – |
| `packagedoc` : chaque paquet a un commentaire « Package nom … » (« Command nom … ») | – |
| `tls-verify` : aucun fichier, tests compris, ne coupe la vérification du certificat TLS (`InsecureSkipVerify`) ; un test passe par le client de son serveur `httptest` (29 octobre 2026) | – |
| `readme` : chaque paquet de `internal/` et `cmd/` a sa ligne dans le README, et aucune ligne ne nomme un paquet disparu | – |

Lire un échec : `fichier:ligne: [règle] ce qui ne va pas. Fix: quoi faire`. On corrige
le code. Si la règle a vraiment tort à cet endroit, l'exception va dans `allowed`
(`internal/qualitytest/exceptions_test.go`) avec sa raison ; une exception qui ne sert
plus fait échouer le test. Les deux exceptions actuelles : le câblage du démon commun
à `safehome-gsm` et `safehome-video`, et l'aide `publishApplied` de leurs tests (deux
paquets `main` ne partagent pas de code de test).

Le vocabulaire commun entre les documents (français) et le code (anglais) est dans
`glossaire.md`. Convention : documents en français, code et commentaires en anglais.

## Composants sur place

1. **Capteurs et sirène (Zigbee)** : ouverture, mouvement, fumée, vibration,
   panique, clavier.
2. **Clé Zigbee** : radio reliée au Raspberry Pi par USB. Elle reçoit et émet.
3. **Zigbee2MQTT** : logiciel qui lit le flux brut de la clé et le publie en
   MQTT sous une forme lisible.
4. **Broker MQTT (Mosquitto)** : bus de messages local. Tout passe par lui.
5. **Moteur d'alarme** : cœur du produit. Machine à états (désarmé, armé, délai
   d'entrée, alerte), règles et horaires.
6. **Passerelle GSM** : pilote le modem 4G pour envoyer SMS et appels, sans
   dépendre d'internet.
7. **Service caméras** : photos à l'alerte, mises en attente si le réseau est
   coupé, et vidéo en direct vers l'application, à l'alarme ou à la demande, par un
   relais du cloud (voir `video.md`). Jamais dans la chaîne d'alarme.
8. **Agent de liaison** : envoie les événements au cloud avec un heartbeat, et
   garde les messages en file d'attente quand internet tombe.
9. **Surveillance de la centrale** : coupure secteur, batterie faible, redémarrage.
   La coupure et la batterie viennent du module batterie de la centrale (Geekworm
   X728), lu par `safehome-power`, qui éteint aussi la centrale proprement avant la
   batterie vide (voir `alimentation.md`).

## Composants cloud

- **Ingestion** : reçoit les événements de chaque site.
- **Base de données** : sites, contacts, événements.
- **Alertes et escalade** : WhatsApp, notification push, puis appel.
- **Surveillance des heartbeats** : alerte quand une centrale ne donne plus de
  nouvelles.
- **App client et console opérateur.**
- **Facturation** par mobile money.

## Communications

| Liaison | Protocole |
|---|---|
| Capteurs ↔ clé Zigbee | Zigbee (radio 2,4 GHz) |
| Clé ↔ Zigbee2MQTT | USB |
| Composants du site ↔ broker | MQTT local |
| Passerelle GSM ↔ modem | Commandes AT (série ou USB) |
| Site → cloud | Connexion sortante TLS (MQTT ou HTTPS), aucun port ouvert |
| Cloud → client | Notification push, WhatsApp, SMS |

## Règle d'architecture : le cœur ne connaît pas le matériel

À terme, le système devient fermé et utilise nos propres produits. Pour que
cette transition ne demande pas de refonte, le moteur d'alarme, la passerelle
GSM, l'agent de liaison et le cloud ne parlent que dans **notre propre langage**.
Zigbee2MQTT, Aqara ou Heiman sont des détails de la couche basse.

```
Matériel (Zigbee aujourd'hui, nos produits demain)
        │
   ADAPTATEUR   ← seule pièce qui change quand le matériel change
        │
   Événements canoniques (notre contrat)
        │
   Moteur d'alarme, passerelle GSM, agent de liaison, cloud
```

À fixer dès le départ :

1. **Un format d'événement à nous, versionné.** Par exemple : `site_id`,
   `device_id`, `capability`, `valeur`, `horodatage`, `batterie`, `signal`. Le
   moteur ne voit jamais un message propre à un fabricant.
2. **Des capacités plutôt que des modèles.** Le moteur raisonne sur `contact`,
   `motion`, `smoke`, `siren`, `panic`, `tamper`, pas sur un modèle précis. Un
   autre capteur de porte produit le même événement.
3. **Un identifiant d'appareil à nous**, distinct de l'adresse Zigbee. L'adaptateur
   tient la table de correspondance.
4. **Des commandes canoniques** dans l'autre sens (`siren.on`, `siren.off`).
   L'adaptateur les traduit pour le matériel.
5. **Des topics MQTT à nous**, par exemple `safehome/{site_id}/events/...` et
   `safehome/{site_id}/commands/...`. Le même contrat sert entre le site et le
   cloud.

### Ce que « fermé » demande

À prévoir dans le format des messages, à implémenter plus tard :

- **Identité de chaque appareil** (numéro unique et clé), pour que seuls nos
  produits se connectent.
- **Appairage maîtrisé** : un appareil ne rejoint un site que s'il est
  enregistré chez nous.
- **Messages signés**, pour empêcher la fabrication de fausses alertes.
- **Mise à jour du firmware à distance**, avec signature.

Fermer le produit ne veut pas forcément dire inventer un protocole radio. Un
protocole propriétaire coûte cher à développer et à homologuer dans chaque pays.
Option plus réaliste : même radio standard (Zigbee) avec notre marque, notre
chiffrement, notre appairage et notre firmware signé. La décision d'aller plus
loin reste ouverte.

### Chemin de transition

- **Phase 1** : matériel du commerce, adaptateur Zigbee2MQTT.
- **Phase 2** : nos propres capteurs, qui parlent le même contrat. On écrit un
  deuxième adaptateur, le reste ne bouge pas.
- **Phase 3** : notre propre centrale (carte dédiée) qui exécute le même moteur.

## Décisions ouvertes

1. **Moteur d'alarme maison ou Home Assistant.** Décidé : service maison, petit,
   testable et prévisible, car c'est le cœur du produit.
2. **Cloud dès le départ ou plus tard.** Décidé : cloud minimal pour le pilote
   (heartbeat et WhatsApp), console opérateur et facturation ensuite.
3. **Plusieurs clients dès la conception.** Décidé : un `site_id` dans chaque
   message dès le début.
4. **Langages.** Décidé : Go pour le moteur et les services sur place (léger, un
   seul exécutable, compile pour le Pi et pour une future carte Linux dédiée),
   Java pour le cloud. Le firmware des capteurs (phase 2) sera choisi plus tard,
   Rust à évaluer à ce moment. Le contrat d'événements relie les trois mondes.
5. **Microcontrôleur de secours pour la sirène et les SMS.** Décidé : pas au
   premier prototype. À la place : watchdog matériel du Pi, chien de garde de
   systemd sur nos programmes (une boucle bloquée les fait redémarrer), système de fichiers
   en lecture seule, alerte de heartbeat dans le cloud. À reconsidérer pour la
   centrale de la phase 3.

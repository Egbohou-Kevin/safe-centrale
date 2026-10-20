# Architecture safeHome

Statut : brouillon de travail, avant le premier code.

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
  passerelle GSM, `internal/zigbee` pour l'adaptateur Zigbee) sont purs : il dépend seulement de la bibliothèque
  standard, n'a aucune entrée-sortie et ne lit jamais l'horloge. Il reçoit des entrées
  qui portent leur heure et renvoie des actions et des événements du domaine.
- **Le contrat** (`internal/contract`) est le langage publié. Il ne dépend de rien.
- **La traduction** (`internal/engine/wire`, `internal/health/wire`) est le seul endroit
  qui connaît un domaine et le contrat. Si le contrat change de version, seules ces
  parties changent. Les deux domaines ne se connaissent pas : le pilote les relie.
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
  les tests la font tourner avec une horloge simulée. Le temps ne recule jamais pour le
  domaine, même si l'horloge système est corrigée en arrière.
- **La couche application** (`internal/hub`) relie tout cela pour la centrale : elle
  reçoit les messages MQTT, les traduit (`wire`), les remet aux domaines par leurs
  boucles, et publie ce qui en sort. Elle déclare ses propres ports (`Publisher`) et
  réutilise ceux du pilote (horloge, stockage). Elle ne connaît aucun adaptateur : les
  tests la font tourner avec un faux broker et une horloge simulée. Aucun domaine ne
  l'importe.
- **La passerelle GSM** a la même forme : son domaine (`internal/gsm`), sa couche
  application (`internal/gateway`, qui déclare le port `Modem`), son adaptateur
  (`internal/adapter/atmodem`). La file d'envoi MQTT (`internal/outbox`) sert aux deux
  applications ; elle connaît le contrat et aucun domaine.
- **L'adaptateur Zigbee** aussi : son domaine (`internal/zigbee`), sa couche
  application (`internal/zigbeeapp`). Zigbee2MQTT parle déjà MQTT : il n'y a pas
  d'adaptateur technique de plus, mais un paquet de traduction (`internal/zigbee/z2m`),
  seul à connaître les messages de Zigbee2MQTT, comme `wire` est seul à connaître le
  contrat.
- **L'agent de liaison** : un cœur (`internal/agent`) qui n'est pas un domaine au sens
  du DDD (il ne connaît que le contrat, puisque son métier est de le transporter), mais
  qui suit les mêmes règles : ni entrées-sorties ni horloge, tournant dans la boucle du
  pilote. Sa couche application (`internal/agentapp`) déclare deux ports de
  publication, la centrale et le cloud ; la racine de composition y branche deux
  clients MQTT.
- **La vidéo** a la forme de la passerelle : son domaine (`internal/video`, sessions et
  plafond), sa traduction (`internal/video/wire`), sa couche application
  (`internal/videoapp`, qui déclare le port `Streamer`) et son adaptateur
  (`internal/adapter/go2rtc`, qui lance un go2rtc par session).
- **L'outil de l'installateur** : une couche application (`internal/install`) sans
  domaine propre (les règles de zones viennent du moteur), avec ses ports (`Publisher`,
  messages reçus) et sans horloge ; l'outil en ligne de commande
  (`cmd/safehome-install`) et la console locale s'appuient dessus. Les zones et les
  sirènes sont gardées par `internal/hubconfig`, dans le dossier d'état.
- **La console locale** : sa couche application (`internal/consoleapp`) suit le mode
  installation et déclare ses ports (`Hotspot`, `Sites`, `Serve`). Sa page
  (`internal/consoleapp/web`) ne connaît qu'elle. Le Wi-Fi est l'adaptateur
  `internal/adapter/hotspot` (`nmcli`).
- **Les racines de composition** (`cmd/safehome-hub`, `cmd/safehome-gsm`,
  `cmd/safehome-zigbee`, `cmd/safehome-agent`, `cmd/safehome-video`, `cmd/safehome-maint`,
  `cmd/safehome-console`, `cmd/safehome-install`) sont les seuls endroits autorisés à
  importer les adaptateurs : elles construisent le vrai client MQTT, le vrai disque et
  la vraie horloge et les passent à la couche application.
- **DDD léger** : on le réserve aux contextes qui ont des règles (alarme, escalade). L'agent
  et la passerelle sont surtout de la plomberie et n'en ont pas besoin.

Ces règles sont vérifiées par des tests (`internal/archtest`), qui échouent si le domaine
importe le contrat, une bibliothèque tierce ou un paquet d'entrées-sorties, ou s'il lit
l'horloge ; si le pilote ou un adaptateur importe un domaine, le contrat ou un autre
paquet du module ; si le pilote lit l'horloge ailleurs que dans `SystemClock` ; si une
bibliothèque externe est importée hors de son adaptateur ; si un adaptateur est importé
ailleurs que dans la racine de composition ; ou si un domaine importe la couche
application.

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
9. **Surveillance de la centrale** : coupure secteur (via la prise connectée),
   batterie faible, redémarrage.

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

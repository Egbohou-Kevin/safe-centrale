<img src="marque/seq-icone.svg" width="56" alt="SeQ">

# Sécurité et installation d'un site

Statut : v1, codée ; essayée sur le kit virtuel (`kit-virtuel.md`), pas encore sur un
vrai Raspberry Pi.

Ce document décrit comment une centrale reçoit son identité, comment elle est
installée chez un client, comment le code d'accès arrive sur le site, et ce qui se
passe si une centrale est volée, détruite ou remplacée.

## Ce qu'il faut protéger

| Menace | Réponse |
|---|---|
| La centrale est volée ou détruite pendant un cambriolage | Le site devient muet : le cloud alerte le propriétaire (voir `agent.md`). Les identifiants de la centrale sont révoqués. |
| Quelqu'un coupe le courant ou internet | La centrale continue sur batterie et par GSM (voir `escalation.md`). |
| Un voleur a le téléphone du propriétaire | Le désarmement exige le code du site. Le cloud borne un téléphone à 2 codes faux en 10 minutes, et la centrale verrouille l'application après 3 codes faux de l'application en 10 minutes : un téléphone seul n'atteint donc jamais le verrouillage. Depuis le 29 octobre 2026, le verrouillage de l'application ne bloque pas le clavier (ni l'inverse) : un cloud compromis qui enverrait des codes faux n'empêche pas les employés de désarmer sur place. Ce qui arrête un voleur patient, c'est l'alerte : tous les 3 codes faux depuis le dernier bon code, les contacts sont prévenus (une alerte toutes les 15 minutes à son rythme), et ils retirent l'accès du téléphone ou changent les codes (voir `engine.md`). |
| Un appareil pirate est ajouté au site | L'appairage se fait seulement en local, et seuls les modèles connus sont acceptés (voir `adapter-zigbee.md`). |
| Le cloud est compromis | Le cloud ne peut envoyer que des ordres d'alarme, de vidéo, de maintenance et de configuration, et les résultats des alertes. Il ne commande ni la sirène ni les appareils. |
| Faux SMS d'acquittement | Il ne peut qu'arrêter l'escalade, et l'incident reste visible (voir `gsm-gateway.md`). |
| Un processus de la centrale publie un faux ordre sur le broker local | Depuis le 29 octobre 2026, le broker de la centrale exige un nom et un mot de passe par programme (plus Zigbee2MQTT et l'opérateur), faits par `install.sh` et donnés à chaque service par systemd, lisibles de lui seul, et chaque nom ne lit et n'écrit que ses topics (`deploy/mosquitto/safehome.acl`) : un programme compromis (la batterie, la vidéo) ne publie ni `alarm/cmd`, ni `maint/cmd`, ni `config/set` (voir `systeme.md`, « Mosquitto »). |
| Brouillage radio du Zigbee | Limite connue en v1. Une disparition simultanée de tous les capteurs est un signal à surveiller plus tard. |

## Identités

- **Une centrale** a un identifiant propre (`hub_xxxx`), distinct du site. Une
  centrale peut être remplacée sans changer de site.
- **Un site** a un `site_id` (par exemple `tg-lom-0001`), créé à l'activation.
- **Une personne** (propriétaire, gérant, gardien) a un identifiant (`u_xx`), lié à
  son numéro de téléphone.

## Identifiants de la centrale

En v1, chaque centrale a un **secret propre** (jeton aléatoire long), enregistré
dans le cloud. Elle se connecte avec son identifiant et ce jeton, sur une liaison
TLS dont elle vérifie le certificat. Le cloud lui donne le droit de publier seulement
sous son propre site, et de lire seulement ses ordres et sa configuration.

Pourquoi un jeton et pas un certificat par centrale : les certificats ajoutent une
gestion (délivrance, expiration, horloge juste) lourde pour un pilote. Pour la phase
fermée, le jeton est remplacé par une clé propre à chaque centrale, avec signature des
messages (champs `sig` et `kid` du contrat).

Le jeton se **révoque** depuis le cloud : le broker refuse alors la centrale. Il peut
être renouvelé sans visite sur place.

## Code d'accès du site

Le code est le point faible d'une alarme : il doit être choisi sur place et rester
difficile à deviner.

- **Longueur** : 6 chiffres recommandés, 4 au minimum.
- **Stockage** : sur la centrale seulement, sous forme hachée avec un algorithme lent.
  Il n'est jamais dans le cloud, ni sur son broker : l'agent de liaison lit les ordres
  de l'application (`alarm/cmd`) en QoS 0, et le broker n'en garde aucun pour une
  centrale absente (voir `agent.md`).
- **Vérification** : toujours par le moteur, sur le site. Trois codes faux d'une même
  origine en 10 minutes verrouillent le désarmement depuis cette origine pendant
  10 minutes : le clavier et l'application comptent chacun les leurs (29 octobre 2026).
  Le verrouillage de l'application laisse le clavier libre, et inversement : un cloud
  compromis ou défaillant, qui sait sceller n'importe quel code, ne peut pas empêcher
  les employés de désarmer au clavier pendant une intrusion. Un code faux de plus de 10 minutes ne
  compte plus : des erreurs éparses dans la journée ne verrouillent pas, et un
  téléphone que le cloud borne à 2 codes faux en 10 minutes ne peut pas, seul, tenir
  le site verrouillé. En revanche, les codes faux depuis le dernier bon code se
  comptent sans limite de temps et pour tout le site, clavier et application
  ensemble : au 3e, au 6e, au 9e..., les contacts reçoivent l'alerte `auth_failed`,
  sans verrouillage (deux essais à l'application et un au clavier suffisent). Un voleur patient, à 2 essais toutes les
  10 minutes (288 par jour), mettrait en moyenne près de 6 jours à tomber sur l'un de
  5 codes de 4 chiffres ; les contacts le savent dès son 3e essai.
- **Limite connue** : avec 6 chiffres, un code se retrouve vite par essais si le fichier
  haché est volé avec la centrale. Le verrouillage ne protège que contre des essais en
  ligne. La parade est la révocation et le changement du code après un vol.

**Réglage sur la centrale.** Les codes se posent avec la ligne de commande de la
centrale (`safehome-hub codes ... set UTILISATEUR`, le code est lu sur l'entrée
standard : il ne reste ni dans l'historique du shell ni dans la liste des processus).
Un code faible (chiffres répétés, suite) ou déjà pris est refusé, 5 personnes au
maximum. Un code refusé parce qu'il est déjà pris dit qu'il ouvre le site : après 3
refus de ce genre en 10 minutes, la ligne de commande et la console refusent tout
nouveau code pendant 10 minutes, pour qu'elles ne servent pas à essayer les codes l'un
après l'autre (les refus sont gardés dans le dossier d'état, `codes/codes-taken.json`). Si la centrale tourne, elle recharge les codes toute seule (signal SIGHUP).
La centrale ignore le code d'une personne qui n'est plus contact du site (elle le
journalise, sans le code). Elle prend les contacts tels qu'ils sont enregistrés au
moment où elle relit les codes (fichier de la centrale et configuration venue du
cloud). Elle relit aussi les codes dès qu'une configuration venue du cloud est
enregistrée : un contact ajouté pendant le mode installation peut recevoir un code
tout de suite, un contact retiré ne désarme plus. `safehome-hub codes set` refuse un utilisateur qui n'est pas
un contact ; `codes list` signale ces codes restants. La console les liste (« Codes
d'anciens contacts ») et permet de les retirer. Les fichiers d'état écrits par un outil
lancé avec `sudo` prennent le propriétaire du dossier, le verrou des codes
(`codes.lock`) aussi ; sinon l'outil échoue en le disant. Le verrou n'est jamais pris à
travers un lien : un `codes.lock` qui est un lien symbolique, ou un autre nom d'un autre
fichier, est refusé et laissé tel quel, pour qu'un outil lancé en root ne donne jamais un
autre fichier au propriétaire du dossier.
Le nombre de tours du hachage (`access.iterations`) doit être réglé sur le
Raspberry Pi réel : à mesurer à l'installation, il n'a pas encore été chronométré.

Pour un **désarmement à distance**, le code part de l'application vers le cloud, puis
vers le site, sur des liaisons chiffrées. Le cloud ne le garde pas et ne l'écrit dans
aucun journal. Le champ du code est prévu pour pouvoir être chiffré de bout en bout
plus tard, sans changer le contrat.

## Installation d'un site

### 1. Préparation en atelier

Avant la visite, nous préparons la centrale :

- installation du système et des programmes,
- création de l'identifiant `hub_xxxx` et de son jeton, enregistrés dans le cloud,
- test du modem, de l'horloge et de la batterie,
- étiquette avec l'identifiant et un code d'activation (code imprimé ou QR).

### 2. Visite de repérage

L'installateur relève : portes et fenêtres à protéger, emplacement de la centrale,
prise de courant, présence d'une box internet, et **qualité du signal GSM** à
l'endroit prévu (la centrale peut la mesurer). Il vérifie aussi que le client a une
SIM avec du crédit (voir `kit.md`).

### 3. Pose

1. Fixer la centrale, la brancher, insérer la SIM du client.
2. Connecter la centrale à la box (câble ou Wi-Fi).
3. **Activation** : la centrale est liée au client avec le code d'activation. Le cloud
   crée le site et envoie la configuration de départ.
4. Le propriétaire est enregistré comme premier contact, avec son numéro.

### 4. Appareils

Avec la console locale (voir plus bas), l'installateur :

- appaire chaque appareil (`registry.pair`),
- donne à chacun un nom et une zone,
- teste chaque appareil en le déclenchant et en vérifiant l'événement.

### 5. Choix du code

Le code est choisi sur place par le propriétaire. Il ne passe jamais par le cloud.
Il le tape lui-même sur la console locale, pendant le mode installation (voir plus
bas). Il se règle aussi en SSH (`safehome-hub codes`), seul moyen de retirer le code
d'un contact (la console ne retire que les codes d'anciens contacts).

### 6. Contacts et configuration

Le propriétaire (ou l'installateur avec lui) saisit les contacts (responsable, gardien,
gérant…), l'heure de fermeture, les délais. La configuration est envoyée à la centrale,
qui la garde sur disque.

### 7. Recette

Une liste de contrôle, enregistrée dans le cloud comme preuve :

- armer, ouvrir une porte, vérifier la sirène, le SMS et la notification ;
- couper le courant : la centrale continue, l'alerte de batterie part au bon seuil ;
- éteindre la box : l'alerte passe par GSM ;
- chaque contact reçoit une alerte de test et l'acquitte ;
- une photo arrive dans l'application ;
- le désarmement à distance, avec le code, fonctionne.

### 8. Remise au client

Démonstration du clavier, du code, de l'application, et de la conduite à tenir en cas
d'alarme.

## Console locale

Statut : codée (`internal/consoleapp`, sa page `internal/consoleapp/web`, le Wi-Fi
`internal/adapter/hotspot`, `cmd/safehome-console`). Testée avec un faux `nmcli` et
une vraie page HTTP, puis sur le kit virtuel : un « téléphone » rejoint le Wi-Fi de la
console et ouvre sa page (`kit-virtuel.md`, étape 9). Décidée avec Kevin le
20 octobre 2026.

L'installateur appaire, nomme les zones, choisit les sirènes et teste les appareils
depuis son téléphone, sans ordinateur. C'est une page web servie par la centrale
pendant le **mode installation** seulement.

### Ouvrir le mode installation

1. L'alarme est désarmée.
2. L'installateur tape le **code installateur** au clavier, puis « désarmer ».
3. La centrale ouvre le mode pour **30 minutes** et le dit (`install.mode`, retenu).
   Le clavier ne reçoit rien ; seul un `alarm.ack` `done` avec `detail.install` à
   `open` part sur `alarm/ack`.

Le code installateur :

- est à part des codes des personnes. Il ne prend la place d'aucune d'elles et ne
  désarme jamais ;
- ne peut pas être le code d'une personne, ni l'inverse ;
- se règle par `sudo -u safehome safehome-hub codes -config /etc/safehome/hub.json set
  installer` (voir `operations.md`). Il est gardé haché, comme les autres.

Tapé alarme armée, ou pendant le blocage après trop d'erreurs, il compte comme un
mauvais code : il ne peut donc pas servir à deviner. Une commande de l'application ne
peut pas ouvrir le mode, même avec ce code.

### Le réseau de la console

En mode installation, la centrale allume **son propre réseau Wi-Fi**. Par défaut il
s'appelle `SeQ-tg-lom-0001` et la page est à l'adresse `http://10.42.0.1/`.

- **Rien n'écoute sur le réseau du commerce.** La page n'est servie que sur ce réseau à
  part, et seulement pendant le mode. Elle ne répond qu'aux appareils de ce réseau
  (`10.42.0.0/24`) : Linux remet un paquet adressé à `10.42.0.1` quelle que soit
  l'interface par laquelle il arrive, et un appareil du réseau du commerce qui ferait
  passer `10.42.0.1` par la centrale atteindrait la page sans la phrase de passe.
  Une demande venue d'une autre adresse est refusée.
- **Le réseau ne mène nulle part ailleurs.** NetworkManager donne les adresses aux
  téléphones (son mode « partagé »), mais ce mode allume aussi le routage et fait
  sortir les téléphones par les autres connexions de la centrale : un téléphone entré
  avec la phrase de l'étiquette atteindrait le réseau du commerce (les flux des
  caméras, la page d'administration de la box) et internet. Un script de
  NetworkManager (`/etc/NetworkManager/dispatcher.d/50-safehome-console`, posé par le
  script d'installation) coupe le routage de ce réseau dès qu'il est levé. La console
  attend qu'il soit coupé, et retire le réseau sinon (10 s au plus) : le journal
  donne la raison, et `safehome-console check` signale un script absent. Seule la
  centrale elle-même reste joignable depuis ce réseau.
- **Le réseau est protégé (WPA2).** Sa phrase de passe est sur l'étiquette de la
  centrale. Le script d'installation la tire au sort une fois et l'affiche pour
  l'étiquette (`/etc/safehome/console.secret`).
- **Pour l'utiliser, il faut deux choses** : la phrase de passe de l'étiquette, et le
  code installateur tapé sur place.
- **La page refuse une adresse inconnue.** Elle ne répond qu'à `10.42.0.1` (protection
  contre le « DNS rebinding »). Elle ne charge rien d'ailleurs et ne s'affiche pas
  dans une autre page.
- **La page refuse les ordres venus d'un autre site.** Un téléphone connecté en même
  temps aux données mobiles ne peut pas se faire envoyer, par une page piégée, un
  ordre vers `10.42.0.1` (retrait, fin, code) : la page refuse tout envoi d'une autre
  origine (réponse 403).
- **Le réseau ne survit pas au mode.** Il est créé à l'ouverture, jamais démarré au
  boot, et supprimé à la fin. Il est aussi supprimé quand le service s'arrête, et
  quand son démarrage a échoué (coupé par son délai, NetworkManager a pu le lever
  quand même) : la fin du mode le retire alors aussi. Un service mort pendant un mode
  (plantage, chien de garde) n'a pas pu le supprimer : le service le supprime à son
  démarrage, avant tout nouveau mode, même si le mode est fini entre-temps.
- **Un réseau qui n'a pas démarré est retenté.** La puce Wi-Fi occupée par le réseau
  de la box, par exemple : l'installateur ne voit pas le réseau, et retape le code
  installateur. La centrale ouvre une nouvelle demi-heure, et la console relance le
  réseau (au lieu de seulement prolonger un mode sans page).
- NetworkManager crée le réseau (`nmcli`). Le service tourne sans droits
  d'administration. Une règle polkit lui permet de créer, lever, baisser et supprimer
  des connexions (jamais d'éteindre la radio Wi-Fi), et une capacité systemd seulement
  d'écouter sur le port 80.

**Limite de la règle polkit.** polkit connaît l'utilisateur, pas le programme : tous
nos services tournent sous `safehome`, et chacun a donc ces droits. Un service
compromis pourrait lire le mot de passe du Wi-Fi de la box, changer ou supprimer
cette connexion, ou la baisser. Un réseau réservé à `safehome` seul
(`settings.modify.own`) demanderait une session que NetworkManager voie, ce qu'un
service n'a pas ; c'est à essayer sur la centrale avant de le changer.

**Limite du Wi-Fi.** Si la centrale va sur internet par le Wi-Fi de la box, la même
puce Wi-Fi ne peut pas tenir les deux réseaux. Pendant le mode installation, la
centrale perd alors la box : l'agent garde ses messages et le GSM reste là. Le câble
est conseillé.

### Ce que fait la page

- **Appairer** : elle ouvre le réseau Zigbee 2 minutes. Les appareils ajoutés ou
  refusés s'affichent au fur et à mesure. La page continue pendant ce temps : un
  appareil ajouté reçoit sa zone tout de suite, et un capteur déclenché aussitôt
  apparaît dans les tests.
- **Zones** : un nom (32 caractères au plus, sans caractère de contrôle ni caractère
  invisible comme un saut de ligne ou une marque de sens d'écriture), un type, et une
  pièce facultative (même règle que le nom), que la liste des appareils montre au
  propriétaire. La page ne propose que les types que l'appareil peut servir. La zone
  part en entier : le champ pièce est prérempli de la pièce actuelle, et vidé, la zone
  n'en a plus (depuis le 28 octobre 2026). La centrale applique la
  même règle aux noms d'un `hub.json` modifié à la main : elle refuse de démarrer
  avec un nom fautif, et l'erreur nomme la zone.
- **Sirènes** : ajouter ou retirer une sirène, et lui donner son nom dans les textes des
  alertes (même règle qu'un nom de zone ; sans nom, « Sirène », ou « Sirène 2 » parmi
  plusieurs, ce que la page signale). La dernière ne peut pas partir.
- **Retirer un appareil** du réseau Zigbee. Un retrait que la centrale refuserait
  (la dernière sirène) est refusé avant que rien ne parte. Pendant le retrait,
  l'appareil est tenu pour parti : un autre onglet ne peut pas retirer l'autre sirène
  entre-temps. Un second retrait du même appareil (un double appui, un autre onglet)
  est refusé : la page dit que le retrait est déjà en cours. Un ordre parti sans
  réponse de l'adaptateur peut encore s'exécuter :
  l'appareil quitte la configuration quand même, et la page le dit : sans zone ni rôle
  de sirène, et peut-être encore appairé, il n'est plus surveillé (corrigé le 25
  octobre 2026 après une revue : la page disait seulement que l'adaptateur n'avait pas
  répondu).
- **Vérifier** : la page montre ce qui manque, comme `safehome-install check`. Elle
  ne dit « Tout est en ordre » que si la configuration des zones se lit : refusée, la
  centrale surveille d'autres zones que celles de l'installateur (corrigé le 25
  octobre 2026 après une revue). Par
  exemple un appareil sans usage, ou une zone qui ne pourra jamais se déclencher.
- **Tests** : les événements des appareils depuis l'ouverture du mode, avec le nom de
  leur zone. La page se met à jour seule, et le code d'un clavier n'y apparaît jamais.
- **Codes d'accès** : chaque contact de la centrale (propriétaire, gérant…) tape
  lui-même son code, deux fois, sur le téléphone. Le code n'est ni affiché, ni dans
  l'adresse, ni dans les journaux. Il est refusé s'il est trop facile, déjà pris, ou
  si c'est le code installateur. La centrale le prend aussitôt (`SIGHUP`, comme après
  `safehome-hub codes set`). Deux codes posés au même moment (deux téléphones, ou la
  page et `codes set`) sont gardés tous les deux : chaque changement relit la liste,
  calcule quelques secondes et la récrit en entier, sous un verrou
  (`codes.lock` dans le dossier d'état) que la page et l'outil prennent l'un après
  l'autre. Ce verrou prend le propriétaire du dossier comme les fichiers d'état : créé
  par `sudo safehome-hub codes set` et laissé à root seul, la console ne pourrait plus
  jamais l'ouvrir (corrigé le 25 octobre 2026 après une revue). La page ne retire que les codes d'anciens contacts
  (« Codes d'anciens contacts ») ; retirer le code d'un contact reste en SSH. Le code
  installateur lui-même ne se règle ni ne se retire depuis la page, même par un ordre
  forgé : celui qui atteindrait la page s'ouvrirait sinon le mode installation. Il se
  règle sur la centrale seulement (`codes set installer`).
- **Terminer** : elle demande à la centrale de fermer le mode (`install.close`).

**Sabotage.** Pendant le mode, la centrale ne déclenche pas d'alarme de sabotage :
l'installateur ouvre les capteurs (pile, fixation). Elle note ces sabotages sans les
donner au moteur, qui ne les tient donc pas pour signalés. Le feu et la panique ne sont
jamais coupés. Dès la fermeture du mode, le sabotage alerte de nouveau, et un sabotage
noté pendant le mode et toujours actif (un couvercle laissé ouvert) est signalé à la
fermeture. Avant le 25 octobre 2026, le moteur le tenait pour signalé : il ne l'était
jamais, même après un redémarrage. Un appareil retiré pendant le mode n'a plus de
sabotage noté : remplacer un capteur ouvert, le retirer à la console, puis armer le soir
ne fait pas sonner la sirène pour un appareil qui n'existe plus (corrigé le 25 octobre
2026 après une revue).

### Après le mode

Les zones et les sirènes ne sont plus dans `/etc/safehome/hub.json`, qui est en
lecture seule. Elles sont gardées dans le dossier d'état
(`/var/lib/safehome/site-devices.json`, droits 0600), écrites par la console ou par
l'outil en ligne de commande, et vérifiées comme la centrale les vérifierait.

À la fermeture du mode (« Terminer », 30 minutes écoulées, ou alarme qui quitte l'état
désarmé), la centrale redémarre si les zones ou les sirènes ont changé. Elle attend un
moment calme pour cela, comme pour une configuration du cloud : jamais pendant un
délai d'entrée ni une alarme. Pour des zones ou des sirènes changées, le moment calme
est la centrale désarmée ou le délai de sortie, pas armée : un armement fait sur les
anciennes zones n'a jamais vu les nouveaux appareils, et au redémarrage une fenêtre
d'une nouvelle zone restée ouverte serait prise pour une fenêtre ouverte pendant
l'arrêt, une sirène dès l'armement. Redémarrée pendant le délai de sortie, la centrale
le finit sur les nouvelles zones : la fenêtre est contournée et signalée. Les zones
changées trouvées armées attendent le prochain désarmement (corrigé le 25 octobre 2026
après une revue).

La partie de la centrale qui applique la configuration (`configapp`) suit
`install/mode` : aucun redémarrage tant que le mode installation est ouvert ; une
configuration reçue pendant le mode est gardée et appliquée à sa fermeture (au premier
moment calme). Une modification faite avec `safehome-install` hors mode installation
est appliquée d'elle-même au prochain moment calme.

Un fichier abîmé n'empêche jamais la centrale de démarrer. Elle reprend alors les
zones de `hub.json` et le dit dans son journal.

### Le cloud

- Il voit le mode (`install.mode` remonte par l'agent) : le propriétaire peut savoir
  qu'un mode installation a été ouvert.
- Il ne peut ni l'ouvrir, ni le fermer, ni appairer : `install/cmd` et
  `registry/cmd` ne descendent jamais du cloud.

### Pas encore fait

- Le kit virtuel tourne sur Debian 13, où NetworkManager et `dnsmasq-base` (le mode
  « partagé »), la règle polkit et le réseau de la console marchent. Reste à le
  vérifier sur Raspberry Pi OS Lite, avec la vraie puce Wi-Fi et sa portée.
- Le script qui coupe le routage du réseau de la console n'a pas encore tourné dans
  le kit virtuel (il le vérifie maintenant : la passerelle de la centrale ne doit pas
  répondre au téléphone), ni sur un vrai Pi.

## Sauvegarde et remplacement

Sont sauvegardés vers le cloud, par la liaison chiffrée et sous une clé gérée par nous :
la configuration du site (contacts, planning, délais), le registre des appareils, la clé
et l'identifiant du réseau Zigbee. **Le code haché n'est pas sauvegardé** : s'il est
perdu, il est refait sur place.

Si une centrale est détruite ou volée :

1. révocation de son jeton dans le cloud ;
2. préparation d'une nouvelle centrale en atelier ;
3. activation sur le même site : la configuration et le registre sont restaurés ;
4. le réseau Zigbee est restauré avec la clé sauvegardée, pour que les appareils se
   reconnectent sans réappairage (à vérifier avec Zigbee2MQTT et la clé neuve) ;
5. nouveau code choisi sur place, en particulier après un vol.

## Accès et traçabilité

- **Application** : connexion avec le numéro de téléphone et un code reçu par message.
  Chaque personne a son compte. Le détail du cloud est décrit plus tard.
- **Installateur et support** : comptes séparés, avec accès limité aux sites qui leur
  sont confiés, et chaque action enregistrée.
- **Journal** : armement, désarmement (avec la personne et l'origine), changement de
  configuration, appairage et retrait, mode installation, révocation. Il est tenu sur
  la centrale et envoyé au cloud.
- **Maintenance à distance** : pas de connexion entrante vers la centrale. Un tunnel
  SSH sortant vers notre bastion, sur demande, une heure au plus, visible du
  propriétaire ; les clés des administrateurs ne sont jamais dans le cloud
  (`maintenance.md`).
- **Cloisonnement des services de la centrale** (28 octobre 2026) : Zigbee2MQTT (Node) et
  go2rtc, du code tiers, ne lisent pas le secret du cloud ni la clé de maintenance
  (`/etc/safehome`) ; Zigbee2MQTT ne lit pas non plus la clé des codes ni les codes d'accès
  (`/var/lib/safehome`), la vidéo encore (elle y garde ses photos : lot à part). Les services
  qui ne parlent qu'au broker local (centrale, Zigbee, batterie) n'ont aucune adresse IP hors de
  la centrale. Détail, réglage par réglage et vérifications sur le Pi : `systeme.md`.

## Contrat

Reporté dans `contract.md` :

- Topics `safehome/{site}/config/set` (cloud vers site) et `config/applied` (retenu).
- Le cloud peut publier sur `alarm/cmd`, `video/cmd`, `alert/result`, `alert/ack`,
  `config/set` et `maint/cmd`, et pour l'agent lui-même sur `photo/cmd` et `agent/cmd`.
  Rien d'autre.
- Configuration numérotée par version, confirmée par le site (`config.applied`).

Reste à ajouter : les événements de journal `audit.config_applied`,
`audit.install_mode` et `audit.hub_activated`.

## Décidé

- Un code par personne (5 au maximum, comme les contacts), lié à l'identifiant de la
  personne. Les événements d'armement et de désarmement portent la personne.
- La console locale (page web de la centrale, active seulement en mode installation)
  est le moyen de l'installateur en v1. Elle est codée (voir plus haut). L'outil en
  ligne de commande `safehome-install` (par SSH, voir `operations.md`) fait le même
  travail et s'appuie sur la même logique (`internal/install`).
- Choix faits au codage (`internal/access`) :
  - 4 à 8 chiffres, uniquement des chiffres (c'est ce que saisit un clavier). Le schéma
    des ordres (`schemas/alarm-command.json`, `args.code`) dit la même forme au cloud,
    et un test de la centrale empêche les deux de s'écarter.
  - Refusés d'office : un chiffre répété (0000, 777777) et les suites (1234, 987654,
    7890). C'est un ajout, pour tenir « difficile à deviner » ; il ne remplace pas le
    conseil de 6 chiffres.
  - Un code identifie une personne : deux personnes ne peuvent pas avoir le même.
  - Haché avec PBKDF2-HMAC-SHA256 (bibliothèque standard de Go, sans téléchargement),
    sel aléatoire de 16 octets par code. Le coût est enregistré avec chaque code, donc
    on peut l'augmenter plus tard sans invalider les codes existants.
  - Vérifier un code compare à **tous** les codes enregistrés, sans s'arrêter au
    premier trouvé, et en temps constant pour la comparaison.
  - Un changement n'est pris en compte qu'après sa sauvegarde sur disque : un code
    qui n'a pas pu être enregistré n'est jamais accepté « en attendant », pour ne pas
    disparaître au prochain redémarrage.
  - Le code n'apparaît dans aucune erreur, aucune sauvegarde, aucun affichage.
  - Le comptage des codes faux (ceux des 10 dernières minutes, et ceux depuis le
    dernier bon code) et le verrouillage restent dans le moteur d'alarme.
- Le coût du hachage est à régler sur la vraie centrale : vérifier 5 codes prend
  environ 90 ms sur un Mac récent, et sera plusieurs fois plus long sur un Raspberry Pi.
  Mesure à faire sur le matériel : `go test -run xxx -bench Verify ./internal/access`.
- Le broker local exige un mot de passe par programme (29 octobre 2026) : aucun client
  anonyme, un nom par programme (`safehome-hub`…), plus `zigbee2mqtt` et `operator`. Le mot
  de passe d'un service lui vient de systemd (`LoadCredential`), jamais d'un fichier lisible
  des autres unités, et n'apparaît dans aucun journal, aucune erreur, aucune ligne de
  commande. Un utilisateur Unix par groupe de services vient dans un lot suivant.
- Le broker local donne à chaque nom ses seuls topics (29 octobre 2026, décision de Kevin
  sur la remarque 4 de l'étude de sécurité de la centrale, désormais close) : une ACL
  (`deploy/mosquitto/safehome.acl`) tirée du code, programme par programme ; les ordres
  (`alarm/cmd`, `maint/cmd`, `config/set`, `install/cmd`, `video/cmd`, `alert/*`,
  `registry/cmd`, `device/+/cmd`) n'ont que leurs vrais producteurs. Le risque inverse, un
  droit oublié qui rendrait un programme sourd (Mosquitto accepte l'abonnement et ne livre
  rien), est tenu par les tests : chaque programme y tourne contre l'ACL livrée, sous son nom.
- Chaque service ne voit de `/etc/safehome` que ses fichiers (29 octobre 2026, remarque 3 de
  l'étude de sécurité de la centrale, **en partie close** : première phase, et deuxième phase pour la clé de l'agent et les codes) : la console,
  exposée au Wi-Fi du magasin, ne lit plus `cloud.secret`, `maint.key`, `video.json` ni
  `gsm.json`, et aucun service SeQ ne lit plus `/var/lib/zigbee2mqtt` (la clé du réseau
  Zigbee). Même utilisateur `safehome` pour tous, la séparation est faite par systemd (option
  b de l'étude `fichiers-par-service`, choisie sans Kevin sur sa recommandation, à relire) ;
  le détail, et comment ajouter un fichier de réglage, dans `systeme.md` (« Ce qui tourne »).
  - **Deuxième phase close pour la clé de l'agent et les codes** (29 octobre 2026) : `code-key`
    est dans `/var/lib/safehome/agent/`, vu de l'agent seul ; les codes (`access.json`,
    `installer.json`, `codes-taken.json`, leurs `.prev`, `codes.lock`) dans
    `/var/lib/safehome/codes/`, vus de la centrale et de la console seules
    (`InaccessiblePaths=` dans les autres unités). `install.sh` les déplace (copie, puis un lien
    relatif à l'ancienne place, pour un retour arrière) ; le détail, la règle du déplacement et
    la liste à vérifier sur le Pi dans `systeme.md` (« Ce qui tourne », les dossiers d'état).
    **À relire** : quand le dossier gagne, un fichier à plat sans équivalent dans le dossier
    (un vieux `.prev`) est supprimé (choix du constructeur, pas de Kevin).

  Reste ouvert :
  - **le reste de l'état** est à plat : lisible et inscriptible par toutes les unités SeQ
    sauf la vidéo. `engine.json` (lot 6) est reporté ;
  - **la centrale et la console partagent `codes/`** : une console compromise lit les
    empreintes des codes (cassables hors ligne) et peut les réécrire ;
  - **option a** (un utilisateur Unix par groupe de services) : écartée pour l'instant, elle
    casse le SIGHUP de la console à la centrale (même uid exigé), le dossier d'état partagé
    des codes et les photos de la vidéo à l'agent, et oblige à migrer la partition d'une
    alarme en service. Même uid, un service compromis qui sortirait de son espace de montage
    garderait les droits de `safehome` sur tout : à reprendre, pour la seule console d'abord.
- Le code ne se change que sur place (clavier ou console locale), jamais à distance
  depuis l'application en v1. À reconsidérer quand le chiffrement de bout en bout
  existera.

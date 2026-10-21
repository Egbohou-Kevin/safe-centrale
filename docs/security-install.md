# Sécurité et installation d'un site

Statut : brouillon v1, à valider avant le premier code.

Ce document décrit comment une centrale reçoit son identité, comment elle est
installée chez un client, comment le code d'accès arrive sur le site, et ce qui se
passe si une centrale est volée, détruite ou remplacée.

## Ce qu'il faut protéger

| Menace | Réponse |
|---|---|
| La centrale est volée ou détruite pendant un cambriolage | Le site devient muet : le cloud alerte le propriétaire (voir `agent.md`). Les identifiants de la centrale sont révoqués. |
| Quelqu'un coupe le courant ou internet | La centrale continue sur batterie et par GSM (voir `escalation.md`). |
| Un voleur a le téléphone du propriétaire | Le désarmement exige le code du site, et l'accès se verrouille après 3 codes faux (voir `engine.md`). |
| Un appareil pirate est ajouté au site | L'appairage se fait seulement en local, et seuls les modèles connus sont acceptés (voir `adapter-zigbee.md`). |
| Le cloud est compromis | Le cloud ne peut envoyer que des ordres d'alarme et de configuration. Il ne peut pas commander la sirène ni ajouter d'appareil. |
| Faux SMS d'acquittement | Il ne peut qu'arrêter l'escalade, et l'incident reste visible (voir `gsm-gateway.md`). |
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
  Il n'est jamais dans le cloud.
- **Vérification** : toujours par le moteur, sur le site. Trois codes faux verrouillent
  le désarmement pendant 10 minutes.
- **Limite connue** : avec 6 chiffres, un code se retrouve vite par essais si le fichier
  haché est volé avec la centrale. Le verrouillage ne protège que contre des essais en
  ligne. La parade est la révocation et le changement du code après un vol.

**Réglage sur la centrale.** Les codes se posent avec la ligne de commande de la
centrale (`safehome-hub codes ... set UTILISATEUR`, le code est lu sur l'entrée
standard : il ne reste ni dans l'historique du shell ni dans la liste des processus).
Un code faible (chiffres répétés, suite) ou déjà pris est refusé, 5 personnes au
maximum. Si la centrale tourne, elle recharge les codes toute seule (signal SIGHUP).
La centrale ignore le code d'une personne qui n'est plus contact du site (elle le
journalise, sans le code). `safehome-hub codes set` refuse un utilisateur qui n'est pas
un contact ; `codes list` signale ces codes restants. La console les liste (« Codes
d'anciens contacts ») et permet de les retirer. Les fichiers d'état écrits par un outil
lancé avec `sudo` prennent le propriétaire du dossier ; sinon l'outil échoue en le
disant.
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
une vraie page HTTP. **Pas encore essayée sur le kit**. Décidée avec Kevin le
20 octobre 2026.

L'installateur appaire, nomme les zones, choisit les sirènes et teste les appareils
depuis son téléphone, sans ordinateur. C'est une page web servie par la centrale
pendant le **mode installation** seulement.

### Ouvrir le mode installation

1. L'alarme est désarmée.
2. L'installateur tape le **code installateur** au clavier, puis « désarmer ».
3. La centrale ouvre le mode pour **30 minutes** et le dit (`install.mode`, retenu). Le
   clavier reçoit une réponse « fait ».

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
s'appelle `safehome-tg-lom-0001` et la page est à l'adresse `http://10.42.0.1/`.

- **Rien n'écoute sur le réseau du commerce.** La page n'est servie que sur ce réseau à
  part, et seulement pendant le mode.
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
  boot, et supprimé à la fin. Il est aussi supprimé quand le service s'arrête.
- NetworkManager crée le réseau (`nmcli`). Le service tourne sans droits
  d'administration. Une règle polkit lui permet seulement de gérer les réseaux, et
  une capacité systemd seulement d'écouter sur le port 80.

**Limite.** Si la centrale va sur internet par le Wi-Fi de la box, la même puce Wi-Fi
ne peut pas tenir les deux réseaux. Pendant le mode installation, la centrale perd
alors la box : l'agent garde ses messages et le GSM reste là. Le câble est conseillé.

### Ce que fait la page

- **Appairer** : elle ouvre le réseau Zigbee 2 minutes. Les appareils ajoutés ou
  refusés s'affichent au fur et à mesure.
- **Zones** : un nom (32 caractères au plus) et un type. La page ne propose que les
  types que l'appareil peut servir.
- **Sirènes** : ajouter ou retirer une sirène. La dernière ne peut pas partir.
- **Retirer un appareil** du réseau Zigbee. Un retrait que la centrale refuserait
  (la dernière sirène) est refusé avant que rien ne parte.
- **Vérifier** : la page montre ce qui manque, comme `safehome-install check`. Par
  exemple un appareil sans usage, ou une zone qui ne pourra jamais se déclencher.
- **Tests** : les événements des appareils depuis l'ouverture du mode, avec le nom de
  leur zone. La page se met à jour seule, et le code d'un clavier n'y apparaît jamais.
- **Codes d'accès** : chaque contact de la centrale (propriétaire, gérant…) tape
  lui-même son code, deux fois, sur le téléphone. Le code n'est ni affiché, ni dans
  l'adresse, ni dans les journaux. Il est refusé s'il est trop facile, déjà pris, ou
  si c'est le code installateur. La centrale le prend aussitôt (`SIGHUP`, comme après
  `safehome-hub codes set`). La page ne retire que les codes d'anciens contacts
  (« Codes d'anciens contacts ») ; retirer le code d'un contact reste en SSH.
- **Terminer** : elle demande à la centrale de fermer le mode (`install.close`).

**Sabotage.** Pendant le mode, la centrale ne déclenche pas d'alarme de sabotage :
l'installateur ouvre les capteurs (pile, fixation). Elle voit toujours les signaux. Le
feu et la panique ne sont jamais coupés. Dès la fermeture du mode, le sabotage alerte
de nouveau.

### Après le mode

Les zones et les sirènes ne sont plus dans `/etc/safehome/hub.json`, qui est en
lecture seule. Elles sont gardées dans le dossier d'état
(`/var/lib/safehome/site-devices.json`, droits 0600), écrites par la console ou par
l'outil en ligne de commande, et vérifiées comme la centrale les vérifierait.

À la fermeture du mode (« Terminer », 30 minutes écoulées, ou alarme qui quitte l'état
désarmé), la centrale redémarre si les zones ou les sirènes ont changé. Elle attend un
moment calme pour cela, comme pour une configuration du cloud : jamais pendant un
délai ni une alarme.

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

- Vérifier sur le kit que NetworkManager et `dnsmasq-base` (le mode « partagé »)
  sont bien là sur Raspberry Pi OS Lite. Vérifier aussi la règle polkit et la portée
  du réseau.

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

## Contrat

Reporté dans `contract.md` :

- Topics `safehome/{site}/config/set` (cloud vers site) et `config/applied` (retenu).
- Le cloud peut publier sur `alarm/cmd`, `video/cmd`, `alert/result`, `alert/ack` et
  `config/set`, rien d'autre.
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
  - 4 à 8 chiffres, uniquement des chiffres (c'est ce que saisit un clavier).
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
  - Le comptage des codes faux et le verrouillage restent dans le moteur d'alarme.
- Le coût du hachage est à régler sur la vraie centrale : vérifier 5 codes prend
  environ 90 ms sur un Mac récent, et sera plusieurs fois plus long sur un Raspberry Pi.
  Mesure à faire sur le matériel : `go test -run xxx -bench Verify ./internal/access`.
- Le code ne se change que sur place (clavier ou console locale), jamais à distance
  depuis l'application en v1. À reconsidérer quand le chiffrement de bout en bout
  existera.

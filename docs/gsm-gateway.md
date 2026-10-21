# Passerelle GSM

Statut : v1 codée et testée sans matériel (faux modem). À valider sur le vrai modem avec
de vraies SIM Togocom et Moov. Les choix faits au codage sont à la fin du document.

La passerelle GSM est le programme en Go qui possède le modem 4G de la centrale.
Elle envoie les SMS et lance les appels demandés par le moteur d'alarme, reçoit
les réponses des contacts et surveille l'état des SIM. C'est un canal d'alerte
comme les autres (voir `escalation.md`) : elle reçoit des `alert.request` et
répond par des `alert.result`, sans connaître le moteur.

## Responsabilités

- Envoyer les SMS et lancer les appels (canaux `sms` et `call`).
- Garder une file d'envoi sur disque, avec priorités et reprises.
- Recevoir les SMS entrants et transformer l'acquittement `1` en `alert.ack`.
- Surveiller le modem et la SIM : réseau, signal, crédit si possible, résultat des
  envois.
- Envoyer le SMS de test périodique.
- Redémarrer le modem quand il se bloque.

Elle ne décide pas qui prévenir ni quand : c'est le moteur. Elle n'exécute aucun
ordre reçu par SMS dans la v1.

## Entrées et sorties

| Entrée | Source |
|---|---|
| `alert.request` avec `channel` = `sms` ou `call` | moteur d'alarme |
| `alarm.cleared` et `alert.ack` (fin d'un incident) | moteur d'alarme, passerelle elle-même, application |
| SMS entrants, état du réseau | modem |

| Sortie | Destination |
|---|---|
| `alert.result` | moteur d'alarme (statuts `queued`, `sent`, `delivered`, `failed`) |
| `alert.ack` | moteur d'alarme |
| État des lignes dans `system.health` | surveillance, agent de liaison |

## Lignes : une abstraction pour 1 ou 2 modems

Un modem ne contient en général qu'**une** SIM. Avoir deux SIM d'opérateurs
différents (Togocom et Moov) demande donc deux modems, ou un module à double SIM
avec bascule. La passerelle raisonne en **lignes** : une ligne est un modem avec sa
SIM.

- La v1 peut tourner avec une seule ligne.
- Ajouter une deuxième ligne ne change que la configuration.
- Si une ligne échoue, la passerelle essaie l'autre. La ligne qui a réussi en
  dernier est essayée en premier.

Chaque ligne est décrite en configuration : chemin du port du modem, opérateur,
activée ou non.

## Envoi d'un SMS

- Le moteur écrit des messages courts, sans accents spéciaux, pour tenir dans un
  seul SMS de 160 caractères (voir `escalation.md`).
- La passerelle demande un **accusé de livraison** à l'opérateur. Elle publie
  `sent` quand le modem a accepté le message, puis `delivered` quand l'accusé
  arrive. Tous les opérateurs ne les renvoient pas : sans accusé, le statut reste
  `sent`.
- Un échec (pas de réseau, SIM sans crédit, erreur du modem) produit `failed` avec
  une raison (`no_network`, `no_credit`, `modem_error`, `timeout`).

## Appel

La passerelle compose le numéro, laisse sonner une durée fixée (20 secondes par
défaut), puis raccroche. Le contact voit un appel manqué avec le numéro de la
centrale. Il n'y a pas de message parlé (voir `escalation.md`).

- Statut `sent` : l'appel a sonné jusqu'au bout ou le contact a décroché.
- Statut `failed` avec raison `busy`, `unreachable` ou `no_network`.

À vérifier avec l'opérateur : si un appel qui n'est pas décroché est facturé au
client.

## File d'envoi

- Chaque demande est écrite sur disque avant d'être traitée. Un redémarrage ne perd
  rien.
- Priorités : `fire` et `panic` d'abord, puis `intrusion`, puis le reste. Une
  demande urgente passe devant les autres, sans interrompre une commande déjà en
  cours au modem.
- Reprise avec délai croissant en cas d'échec, plafonnée. Une demande trop vieille
  (par exemple plus de 10 minutes pour une alerte d'intrusion) est abandonnée et
  signalée en échec : la vague suivante de l'escalade a déjà pris le relais.
- **Pas de doublon** : la passerelle retient l'`id` de chaque demande traitée. Une
  demande reçue deux fois n'envoie qu'un SMS.

## Accès au modem

Un seul traitement parle au modem à la fois, par ligne. Les commandes (AT) sont
envoyées l'une après l'autre, chacune avec un délai maximum. Les événements
spontanés du modem (SMS reçu, appel entrant, perte de réseau) sont lus par le même
traitement et distribués ensuite.

## Surveillance de la ligne

Toutes les 30 secondes, la passerelle vérifie :

- que le modem répond,
- qu'il est enregistré sur le réseau,
- le niveau de signal.

Elle vérifie aussi :

- **Crédit ou forfait** : la lecture passe par un code propre à chaque opérateur,
  donc configurable et facultative. Quand elle n'est pas disponible, les erreurs
  d'envoi (`no_credit`) servent de signal.
- **SMS de test** : un SMS envoyé chaque semaine pour détecter une SIM suspendue
  avant qu'une alerte en dépende.

L'état de chaque ligne va dans `system.health` :

```json
"gsm": {
  "lines": [
    { "id": "line1", "operator": "togocom", "registered": true, "signal": 71,
      "credit": "ok", "last_test": "2026-10-17T08:00:00Z", "last_test_ok": true }
  ]
}
```

Quand le GSM reste inutilisable (ou la passerelle muette) 10 minutes, la supervision
envoie au propriétaire une alerte « protection dégradée » par internet, avec ce qu'il faut faire (recharger, vérifier
la SIM).

## Modem bloqué

Si le modem ne répond plus à trois vérifications de suite, la passerelle le
redémarre : commande logicielle d'abord, puis coupure d'alimentation du port USB
ou de la carte si le matériel le permet. Les demandes en attente restent dans la
file. Un modem qui se bloque souvent est signalé dans `system.health`.

## SMS entrants

- La passerelle ne garde que les SMS dont le numéro est celui d'un contact du site.
  Elle ignore et enregistre les autres.
- Un SMS `1` d'un contact devient un `alert.ack` pour l'incident ouvert pour ce
  contact. Il arrête l'escalade vers les autres. Il ne désarme rien.
- Aucune autre commande n'est comprise dans la v1.
- Les numéros sont normalisés au format international (+228 pour le Togo).
- Un SMS peut être envoyé sous un faux numéro : le risque est limité, car ce SMS ne
  fait qu'arrêter l'escalade, mais il justifie que l'incident reste visible dans
  l'application.

## Configuration

| Paramètre | Défaut |
|---|---|
| Lignes (port, opérateur, activée) | 1 ligne |
| Durée de sonnerie d'un appel | 20 s |
| Intervalle de vérification | 30 s |
| Fréquence du SMS de test | 1 par semaine |
| Âge maximum d'une demande d'intrusion | 10 min |

## Interchangeabilité

Le modem est derrière une petite interface (envoyer un SMS, appeler, lire les SMS,
donner l'état). Remplacer le modèle de modem, ou ajouter plus tard un autre moyen
d'envoi, ne touche que cette partie. La passerelle de test (voir ci-dessous) utilise
la même interface.

## Tests

- **Faux modem** qui répond comme un vrai (OK, erreurs, perte de réseau, SMS
  entrant) : tous les scénarios de la passerelle se testent sans matériel.
- Scénarios : envoi réussi, SIM sans crédit, réseau perdu, modem bloqué qui
  redémarre, redémarrage avec une file pleine, doublon, SMS entrant d'un contact et
  d'un inconnu, bascule d'une ligne à l'autre.
- Ensuite, tests sur le vrai modem avec de vraies SIM de Togocom et de Moov.

## Choix faits au codage

Découpage, selon l'architecture hexagonale :

| Paquet | Rôle |
|---|---|
| `internal/gsm` | Le domaine : file à priorités, choix de la ligne, reprises, abandon des demandes trop vieilles, doublons, surveillance du modem, réponses « 1 », SMS de test. Pur, sans horloge |
| `internal/gsm/wire` | Traduction entre le domaine et le contrat |
| `internal/gateway` | La couche application : un fil par ligne (une commande à la fois par modem), publication des résultats, de l'acquittement et de l'état des lignes |
| `internal/adapter/atmodem` | L'adaptateur du modem : commandes AT sur le port série, bibliothèque standard seulement |
| `internal/adapter/atmodem/attest` | Un faux modem qui répond comme un vrai, pour les tests |
| `internal/gsmconfig`, `cmd/safehome-gsm` | Le fichier de configuration et le programme |

Là où le document laissait un doute :

- **Un programme à part** (`safehome-gsm`), à côté de `safehome-hub`, comme prévu : ils ne
  se parlent que par MQTT. La file d'envoi MQTT est partagée (`internal/outbox`).
- **Échec signalé tout de suite, reprises ensuite.** Quand toutes les lignes ont échoué
  une fois, la passerelle publie `failed` (l'escalade passe à la vague suivante sans
  attendre), puis continue d'essayer avec un délai qui double (5 s, 10 s, … 2 min au
  plus) jusqu'à l'âge maximum. Un envoi réussi plus tard publie `sent`.
- **Âge maximum** : 10 minutes pour fire, panic et intrusion, une heure pour le reste
  (réglable). Une demande abandonnée publie `failed` avec la raison `timeout`.
  L'âge d'une demande compte depuis son `ts` (l'heure où l'escalade l'a écrite), pas
  depuis sa réception : une demande gardée par le broker pendant que la passerelle
  était arrêtée part en échec `timeout` dès son arrivée si elle est trop vieille, sans
  être envoyée. Un `ts` dans le futur compte depuis la réception.
- **Fin d'incident** : la passerelle écoute `alarm.cleared` (origine `user` seulement :
  une alarme dont les cycles de sirène se sont épuisés continue son escalade) et
  `alert.ack` (y compris un « 1 » reçu par elle-même, pris en compte tout de suite).
  Les messages de l'incident encore en attente sont abandonnés sans résultat publié
  (l'escalade ne les attend plus), un message en cours au modem n'est pas réessayé,
  et une demande de cet incident arrivée plus tard est ignorée.
- **Une ligne en échec** : on essaie l'autre ligne avant de signaler l'échec. Pas de
  crédit (`no_credit`) marque la ligne `credit: "empty"` jusqu'au prochain envoi réussi ;
  pas de réseau la marque non enregistrée jusqu'à la vérification suivante.
- **Les alertes passent avant les vérifications** : une ligne libre envoie d'abord ce
  qui attend, puis fait sa vérification de 30 s.
- **Ligne qui ne répond pas** à une commande dans son délai (90 s pour un SMS, plus la
  sonnerie pour un appel) : comptée en échec. Une réponse tardive d'une ligne n'envoie
  pas le message une deuxième fois ailleurs.
- **Modem bloqué** : trois vérifications sans réponse → redémarrage (`AT+CFUN=1,1`),
  puis réouverture du port (un modem USB revient comme un nouveau périphérique). La
  coupure d'alimentation du port USB n'est pas faite.
- **Redémarrage de la passerelle** : la file est sur disque. Un message qu'une ligne était
  en train d'envoyer repart : mieux vaut un SMS en double qu'aucun. Ce que fait une
  ligne (envoi, vérification, redémarrage du modem) et l'heure de sa prochaine
  vérification ne sont pas sauvegardés : chaque ligne est vérifiée de nouveau au
  démarrage. Le signal est sauvegardé arrondi à la dizaine, pour ne pas réécrire
  l'état à chaque vérification.
- **« 1 »** : il acquitte le dernier incident envoyé à ce contact, pendant 24 heures. Un
  « 1 » d'un numéro inconnu, un autre texte, ou rien à acquitter : ignoré et journalisé,
  sans le numéro ni le texte.
- **SMS de test** : envoyé chaque semaine au numéro de contrôle, il ne publie rien vers
  l'escalade ; son résultat va dans l'état de la ligne (`last_test`, `last_test_ok`). Il
  n'est pas réessayé : son rôle est de dire.
- **État des lignes** : publié retenu dans un nouveau message `gsm.status` (voir
  `contract.md`), à chaque changement et de nouveau chaque minute (`status_every`)
  même sans changement. La centrale le recopie dans `system.health` et en déduit le
  lien `gsm` : `ok` s'il reste une ligne qui répond, est sur le réseau et a du crédit,
  `down` sinon. Le signal est arrondi à la dizaine pour ne pas publier un changement
  à chaque vérification.
- **Passerelle muette** : une passerelle arrêtée ou bloquée ne peut pas le dire
  elle-même. Sans nouvel état depuis 3 minutes (`gsm_silent_after`, côté centrale), la
  centrale passe le lien `gsm` à `silent` et compte une panne. La répétition passe par
  la boucle du domaine : une boucle bloquée se voit aussi, pas seulement un processus
  mort. L'heure prise en compte est celle du message (`ts`) : un état retenu laissé par
  une passerelle morte depuis longtemps est muet dès le démarrage de la centrale.
  Un message de dernière volonté MQTT n'a pas été retenu : il aurait signalé un
  processus mort plus vite, mais pas une boucle bloquée, et l'alerte attend de toute
  façon 10 minutes.
- **Protection dégradée** : la supervision de la centrale prévient le propriétaire (par
  internet) quand le GSM reste inutilisable ou muet 10 minutes, une fois par panne.
- **Port série** : le programme règle le port avec `stty` (vitesse, mode brut) puis
  l'ouvre comme un fichier. Pas de bibliothèque série à télécharger.
- **Modem absent au démarrage** : la passerelle démarre quand même et réessaie à chaque
  vérification.

À vérifier sur le vrai modem :

- Les codes d'erreur des opérateurs pour « pas de crédit » (`+CMS ERROR: 21` est
  supposé) et le format exact des accusés de livraison (`+CDS`).
- Le comportement de l'appel (certains modems demandent `AT+CLCC` pour suivre l'appel).
- Le port à utiliser pour les commandes (souvent `/dev/ttyUSB2` sur un modem USB).
- Un SMS reçu dont le texte est exactement « OK » perturberait la lecture : cas rare,
  noté.

## Décidé

- Une seule ligne en v1 (un modem, une SIM). La deuxième ligne viendra plus tard,
  avec un deuxième modem, sans changer le code de la passerelle.
- Le SMS de test hebdomadaire est envoyé à un numéro de contrôle à nous.

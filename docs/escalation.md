# Escalade des alertes

Statut : v1 codée (`internal/escalation`), à valider sur le terrain. Les choix faits au codage sont à la fin du document.

L'escalade décide **qui est prévenu, par quel moyen, dans quel ordre et après
combien de temps** quand le système détecte un problème. Elle doit fonctionner
sans internet : la sirène locale et les SMS partent même si le cloud est
injoignable.

## Qui fait quoi

- **L'escalade** (`internal/escalation`, dans la centrale) décide : elle garde le
  plan d'escalade de chaque incident, mesure les délais et choisit la vague suivante.
  Elle publie des `alert.request` (voir `contract.md`). Le moteur d'alarme et la
  supervision lui donnent leurs alertes.
- **Les canaux** exécutent : la passerelle GSM envoie les SMS et lance les appels,
  l'agent de liaison envoie vers les canaux du cloud (WhatsApp, notification
  push). Chacun répond par un `alert.result`.
- **Le cloud** a sa propre escalade pour un seul cas : le site qui ne répond plus
  (voir « Côté cloud »).

## Contacts

Chaque site a une liste d'au plus 5 contacts : propriétaire, gérant, gardien,
responsable (délégué du propriétaire, facultatif) et autres. Chaque contact a :

- un nom et un rôle,
- un numéro de téléphone,
- les canaux qu'il accepte (SMS, appel, WhatsApp, application),
- un rôle (`owner` propriétaire, `deputy` responsable, `guard` gardien, `manager`
  gérant, `other` autre).

La liste est modifiée depuis le cloud, puis **copiée et conservée sur la
centrale**. Une panne d'internet ne retire donc aucun contact.

La v1 prévoit des personnes seulement. Pas d'appel automatique à la police ni à
une société de gardiennage.

## Canaux

| Canal | Passe par | Rôle | Marche sans internet |
|---|---|---|---|
| Notification application | internet et cloud | **Préférentiel** | non |
| WhatsApp | internet et cloud | **Préférentiel** (photos, détails) | non |
| SMS | modem de la centrale | **Secours** | oui |
| Appel | modem de la centrale | **Secours** | oui |

L'internet est le canal préférentiel : il coûte peu et porte les photos. Le GSM
(SMS et appel) sert de canal secondaire, pour deux raisons : prendre le relais
quand internet ne marche pas, et **doubler l'urgence** quand personne ne réagit.
Cela limite aussi le coût des SMS et de la 4G.

L'appel depuis le modem fait seulement sonner le téléphone : le contact voit le
numéro de la centrale et comprend qu'il doit lire le SMS. Un message parlé
demanderait un service d'appel dans le cloud, avec un coût et une dépendance à
internet. Ce n'est pas prévu en v1.

## Ordre des canaux pour chaque contact prévenu

1. **Internet d'abord.** L'escalade demande une notification application ou
   WhatsApp.
2. **GSM tout de suite** si internet est inutilisable : la centrale n'a plus de
   liaison avec le cloud, ou le cloud n'a pas confirmé l'envoi dans les 10
   secondes.
3. **GSM en doublon** si le contact n'a pas acquitté 60 secondes après la
   notification : SMS et appel, pour insister.

Exception : pour `fire` et `panic`, le SMS part en même temps que la notification,
sans attendre, car le délai coûte trop cher. Décidé.

Les alertes non urgentes (résumé quotidien, pile faible, appareil muet) passent
par internet seulement. Si internet est coupé, elles attendent son retour au lieu
de partir par SMS.

## Acquittement

Un contact acquitte pour dire « j'ai vu, je m'en occupe ». L'acquittement arrête
l'escalade vers les autres contacts. Il ne désarme pas l'alarme et ne coupe pas la
sirène.

Moyens d'acquitter :

- **Répondre par SMS** (par exemple `1`) à l'alerte. Le numéro de l'expéditeur doit
  être celui d'un contact.
- **Bouton dans l'application** ou la notification.

Un acquittement est enregistré dans un événement `alert.ack` avec le contact et le
moyen utilisé.

## Plans par type d'alerte

| Type | Déclencheur | Plan |
|---|---|---|
| `intrusion` | alarme déclenchée | Vagues successives (voir ci-dessous) |
| `fire` | fumée | Tous les contacts tout de suite |
| `panic` | bouton panique | Tous les contacts tout de suite, sans sirène |
| `tamper` | sabotage | Premier contact (responsable ou propriétaire) tout de suite |
| `auth_failed` | codes faux répétés | Premier contact (responsable ou propriétaire) tout de suite |
| `arming_bypassed` | armement automatique avec une zone ouverte, ou fin d'un délai de sortie (ou `arm_now`) avec une zone `instant` restée ouverte (zone contournée) | Premier contact (responsable ou propriétaire) tout de suite |
| `mains_lost` | coupure du secteur | Aucune alerte en soi, voir « Coupures de courant » |
| `battery_low_hub` | batterie de la centrale à 40 % (urgence `high`), puis à 10 % (`critical`), seulement pendant une coupure | Premier contact (responsable ou propriétaire), une alerte à chaque seuil (voir « Coupures de courant ») |
| `mains_restored` | retour du courant, seulement si une alerte de batterie est partie (champ `percent` : niveau actuel) | Internet de préférence, urgence basse |
| `device_offline`, `battery_low` | appareil muet, pile faible | Résumé quotidien au propriétaire, tous les jours tant que le défaut dure (liste dans `devices`) |
| `gsm_degraded` | secours SMS inutilisable depuis 10 minutes ; une passerelle GSM muette compte comme en panne après 3 minutes, l'alerte part donc 13 minutes après son dernier message | Propriétaire, par internet seulement |
| `zigbee_down` | capteurs injoignables depuis 3 minutes (Zigbee2MQTT ou sa clé arrêtés) ; un adaptateur muet compte comme en panne après 3 minutes, l'alerte part donc après 6 minutes : l'alarme est aveugle | Premier contact (responsable ou propriétaire) tout de suite, doublé par SMS et appel ; « 1 » pour acquitter |
| `siren_failed` | pendant une alarme, une sirène refuse l'ordre de sonner, renvois compris (voir `adapter-zigbee.md`) ; une fois par sirène et par alarme, urgence `high`, la sirène dans `devices` | Premier contact (responsable ou propriétaire) **et** gardien tout de suite, doublé par SMS et appel, sans relance : le plan de l'alarme insiste déjà ; « 1 » pour acquitter |

### Intrusion : vagues

Chaque contact a un rôle : `deputy` (responsable, délégué du propriétaire,
facultatif), `guard` (gardien, facultatif), `owner` (propriétaire), `manager` (gérant)
ou `other` (autre). Les rôles absents sont sautés.

| Moment | Qui | Comment |
|---|---|---|
| 0 | Le responsable s'il existe, sinon le propriétaire ; **et** le gardien s'il existe | Internet d'abord, GSM à +1 min sans acquittement (ou tout de suite si internet est coupé) |
| +2 min sans acquittement | Le propriétaire (si le responsable a été prévenu en premier) ; relance du gardien et de la première personne | Internet, GSM 1 min après |
| +5 min sans acquittement | Le gérant et tous les autres contacts | Internet, GSM 1 min après |
| Ensuite, toutes les 10 min | Tous les contacts qui n'ont pas acquitté | Internet et SMS, jusqu'à 3 relances |

Sans responsable, la vague de +2 min relance quand même le propriétaire (prévenu à 0)
et le gardien. Une vague sans personne à prévenir est sautée sans attendre son délai.

Au bout des relances, l'escalade arrête l'escalade et note l'incident comme « sans
réponse ». La sirène suit son propre calendrier (voir `engine.md`).

Tous les délais sont réglables par site.

## Coupures de courant

La centrale a sa propre batterie et sait quand elle passe dessus : elle détecte
ainsi la coupure du secteur et mesure le niveau de charge. Tant que la surveillance
fonctionne, une coupure n'est pas une alerte : les coupures sont fréquentes et
alerter à chaque fois épuiserait les contacts.

- **Coupure du secteur** : enregistrée et visible dans l'application, sans alerte.
- **Batterie à 40 %** : une alerte au propriétaire (premier contact).
- **Batterie à 10 %** : une deuxième alerte, urgente.
- Ces deux seuils sont réglables, et chaque seuil n'envoie qu'une alerte par
  coupure. Ils se réarment quand la batterie remonte (par exemple au-dessus de
  60 %).
- **Retour du courant** : un message court, seulement si une alerte de batterie
  est partie, par **internet de préférence**. Il n'est pas urgent : le GSM n'est
  utilisé que si internet reste indisponible longtemps (la box met du temps à
  revenir).

Pendant une coupure, la box du client est probablement éteinte (elle existe avant
nous et nous ne la gérons pas). Les alertes de batterie suivent la règle générale :
internet d'abord, GSM quand internet est inutilisable. En pratique, elles partent
donc par SMS.

**Source.** Le module batterie de la centrale (Geekworm X728) : `safehome-power`
publie le secteur et la charge au nom de l'appareil `hub_device_id`, et éteint la
centrale proprement avant que la batterie soit vide (`alimentation.md`).
`internal/zigbeeconfig` interdit la capacité `mains` dans les profils d'appareil :
seule la centrale la donne.

## Canaux interchangeables

Les canaux doivent pouvoir changer sans toucher au moteur d'alarme :

- Chaque canal (application, WhatsApp, SMS, appel, et d'autres plus tard) est un
  module qui reçoit un `alert.request` et répond par un `alert.result`. Ajouter ou
  remplacer un canal ne change que ce module.
- La **politique de canaux est une donnée de configuration**, pas du code : pour
  chaque type d'alerte, une liste ordonnée d'étapes (canal, délai, condition de
  passage à l'étape suivante). Par exemple : application, puis SMS si pas
  d'acquittement après 60 secondes.
- La politique se règle par site et par type d'alerte. Les règles décrites dans ce
  document sont les valeurs par défaut.

## Messages

Un SMS est court et lisible seul, sans accents spéciaux pour tenir en un seul
message de 160 caractères :

```
ALARME Boutique Lome: intrusion (Entree) a 21:04. Repondre 1 si vous gerez.
```

Il contient : le site, ce qui s'est passé et où (zone), l'heure, comment acquitter. Les messages WhatsApp et de l'application peuvent ajouter les photos et
les détails.

## Échecs et repli

- **Internet inutilisable** : l'escalade envoie directement par GSM, sans attendre
  le délai de 60 secondes.
- **SMS non envoyé** (pas de réseau, SIM sans crédit) : la passerelle essaie ses
  autres lignes, puis publie un échec. Si tous les messages de la vague échouent pour
  tous ses contacts, l'escalade passe à la vague suivante sans attendre. Sans aucune
  ligne utilisable, la demande attend dans la file de la passerelle, puis échoue à son
  âge maximum (10 minutes pour une intrusion).
- **Aucun canal ne marche** : la sirène continue et l'incident reste en attente.
  Dès que la liaison revient, les alertes en attente partent.
- **SIM et crédit** : les SIM de la centrale (SMS, appels, et données pour un
  client sans box) sont fournies et rechargées par le client. Nous ne payons pas
  leur crédit, mais nous répondons du fonctionnement du système. La passerelle
  surveille donc l'état de chaque SIM et le signale dans `system.health` :
  réseau, crédit ou forfait quand l'opérateur permet de le lire, et résultat des
  envois.
- **Test périodique** : une ligne prépayée peu utilisée peut être suspendue par
  l'opérateur. La passerelle envoie donc un SMS de test à intervalles réguliers
  (par exemple chaque semaine) vers un numéro de contrôle, pour détecter une SIM
  morte avant le jour où l'alerte en dépend.
- **Prévenir le client** : une SIM sans crédit, suspendue ou sans réseau rend le
  canal de secours inutilisable. Le propriétaire est averti par internet, avec ce
  qu'il doit faire (recharger), et l'état « protection dégradée » apparaît dans
  l'application tant que le problème dure.

## Regroupement et limites

- Plusieurs détecteurs qui s'ouvrent pendant le même incident ne produisent pas
  chacun leur SMS. En pratique, une fois l'alarme déclenchée, le moteur n'émet plus
  rien pour les détecteurs suivants : l'incident ne connaît que le premier. Un feu ou
  une panique pendant une intrusion part quand même, comme une alerte à part.
- Un maximum de messages par contact et par heure limite le coût et le bruit. Les
  alertes de type `fire` et `panic` ne sont jamais bridées.
- Un incident a un identifiant, repris dans tous ses messages et événements.

## Côté cloud

Le cloud surveille le heartbeat de chaque site (voir `contract.md`). Si trois
heartbeats manquent, le cloud avertit le propriétaire par WhatsApp ou SMS, depuis
ses propres canaux. Il ne peut pas compter sur la centrale pour le faire, puisque
c'est la centrale qui est muette.

## Contrat

Ajouts à reporter dans `contract.md` :

- `alert.request` : `incident`, `kind`, `urgency`, `wave`, `contact`, `channel`
  (`app`, `whatsapp`, `sms`, `call`), `text`.
- `alert.result` : `ref` (le `alert.request`), `status` (`queued`, `sent`,
  `delivered`, `failed`), `reason` si échec. Pour `app` et `whatsapp`, `sent` veut
  dire que le cloud a pris le message, `delivered` qu'il est arrivé sur le
  téléphone.
- `alert.ack` : `incident`, `contact`, `via` (`sms`, `app`).
- Configuration des contacts : par `config.set` (voir `contract.md`). La liste du
  cloud remplace celle du fichier de la centrale ; un propriétaire au moins. Les
  numéros ne vont jamais dans `hub.json` ni dans un journal ; la centrale les garde
  dans `site-config.json` (dossier d'état, 0600), où la passerelle les lit.

## Décidé

- Contacts : propriétaire, gérant, gardien, responsable délégué et autres.
- Vagues d'intrusion : 0, +2 min, +5 min, relances toutes les 10 min.
- Le gardien est prévenu dès la première minute, avec le responsable ou le
  propriétaire.
- L'appel depuis la centrale fait seulement sonner le téléphone, sans message
  parlé (le SMS donne l'information). Le message parlé pourra venir plus tard.
- Acquittement : réponse `1` par SMS depuis le numéro d'un contact, ou bouton dans
  l'application.

## Décidé (suite)

- Coupures de courant : pas d'alerte pour la coupure elle-même. Alertes de batterie
  à 40 % puis à 10 %, par internet d'abord et GSM sinon. Message de retour du
  courant seulement si une alerte est partie, par internet de préférence.
- Canaux interchangeables : la politique de canaux est une configuration, chaque
  canal est un module.

## Choix faits au codage

L'escalade est un quatrième domaine pur (`internal/escalation`), comme le moteur, la
supervision et les codes : pas d'horloge, pas d'entrée-sortie, les entrées portent leur
heure. La couche application (`internal/hub`) lui donne les alertes du moteur et de la
supervision, et publie ce qu'il décide. Là où le document laissait un doute :

- **Une demande par contact et par canal.** Les alertes brutes du moteur ne sont plus
  publiées telles quelles : l'escalade les transforme en `alert.request` précis (`contact`,
  `channel`, `wave`, `text`). Un canal n'a qu'à envoyer ce qu'on lui demande.
- **Plans en données.** Chaque type d'alerte a un plan (vagues, relances, internet
  seulement ou non) modifiable ; les valeurs par défaut sont celles de ce document.
  Les contacts, les réglages et les plans sont dans le fichier de la centrale
  (`escalation.plans`, détail dans `operations.md`) ; un plan écrit remplace celui
  par défaut pour son type, un type absent garde le sien.
- **Internet d'abord, GSM ensuite.** Si le cloud n'a pas confirmé en 10 s, ou si le
  message est en échec, ou si le lien internet est coupé, le SMS et l'appel partent. Le
  doublon à 60 s envoie seulement les canaux GSM qui n'ont pas encore servi à ce tour.
- **Fire et panic.** SMS en même temps que la notification, jamais bridés. Ajout de
  l'implémentation : ils ont aussi les relances toutes les 10 minutes (3 fois), sinon
  personne ne relance un incendie que personne n'a vu.
- **Relances** (toutes les 10 min, 3 fois) : internet et SMS à tous, sans appel (mais si
  internet ne confirme pas, le GSM reprend la main comme pour une vague).
- **Vague vide.** Une vague sans personne à prévenir est sautée tout de suite. Une
  vague dont tous les contacts sont injoignables (messages en échec) fait avancer la
  suivante sans attendre son délai.
- **Acquittement.** Un acquittement d'un contact connu arrête l'escalade de l'incident
  entier (toutes ses alertes). Un acquittement d'un inconnu est ignoré.
- **Fin de l'incident.** Un désarmement par une personne, ou l'arrêt de la sirène avec
  un code, arrête l'escalade (`Close`) : il serait absurde de relancer les contacts
  après coup. L'arrêt de la sirène par expiration ne l'arrête pas.
- **Regroupement.** Un même incident et un même type d'alerte levés plusieurs fois se
  fusionnent : les appareils s'ajoutent, aucun message de plus ne part. Le moteur ne
  lève jamais deux fois la même intrusion (il se tait une fois déclenché) : c'est une
  sécurité, qui ne sert pas aujourd'hui pour les intrusions.
- **Limite par heure.** 20 messages par contact et par heure (réglable), sauf fire et panic.
  Un message retenu est journalisé.
- **Alertes non urgentes** (résumés, retour du courant, protection dégradée) : internet
  seulement. Si internet est coupé elles attendent son retour. Le retour du courant peut
  partir par GSM après une heure sans internet.
- **Textes.** En français, en ASCII simple, 160 caractères au plus (le nom du site et des
  zones sont nettoyés de leurs accents) ; la manière de répondre (`Repondre 1 si vous
  gerez.`) est gardée quand le texte est coupé.
- **Reprise.** L'état est sauvegardé : après un redémarrage, les vagues et les délais
  en retard sont rejoués dans l'ordre, à leur heure.

La passerelle GSM (`safehome-gsm`) exécute les demandes `sms` et `call`, connaît les
numéros des contacts et publie `alert.ack` pour une réponse « 1 ». Les canaux
application et WhatsApp partent par l'agent de liaison vers le cloud (dépôt
`safeHome-cloud`), qui envoie WhatsApp ; l'application n'existe pas encore. Pas encore
fait : le changement des plans depuis le cloud (les contacts, eux, arrivent par
`config.set`, que le cloud envoie dès qu'ils changent).

## Questions ouvertes

Aucune question ouverte pour le moment.

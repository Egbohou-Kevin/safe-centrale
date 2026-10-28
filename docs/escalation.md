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

Un acquittement donné dans l'application passe par le cloud et arrête les SMS et les
appels de l'escalade : la passerelle GSM le confirme donc au contact par un SMS, une fois
par incident (« l'alarme est prise en charge en votre nom dans l'application »), pour
qu'un acquittement qu'il n'a pas donné ne lui reste pas inconnu. Un contact sans numéro
ne peut pas en être averti (voir `gsm-gateway.md`).

Un acquittement est enregistré dans un événement `alert.ack` avec le contact et le
moyen utilisé. Il rend aussi discrète l'alerte d'une sirène muette pendant cette
alarme (voir « Sirène muette pendant une alarme »).

## « Vu »

Décidé par Kevin le 25 octobre 2026 (maquettes AlarmeVu et AlarmeVuChoix), fait le même
jour. Sur l'écran de l'alarme, le bouton « Je gère » devient « Vu ». Toucher « Vu »
fait taire ce téléphone et dit aux autres que ce contact a vu, **sans arrêter
l'escalade** : le cloud publie `alert.seen` (`incident`, `contact`, `at`, voir
`contract.md`). Un volet demande ensuite « Et les autres contacts ? » :

- « Je m'en occupe » : l'acquittement de toujours (`alert.ack`, `via` à `app`),
  l'escalade s'arrête ;
- « Prévenir les autres » : rien de plus, l'escalade continue et les autres voient
  « Kevin a vu à 13:43 » dans l'application ;
- sans réponse : l'escalade continue.

Ce que fait l'escalade d'un « Vu » d'un contact connu, pour l'incident en cours (toutes
ses alertes), choix du 25 octobre 2026 :

- **Elle ne s'arrête pas.** Les autres contacts sont prévenus comme prévu, vague après
  vague, relances comprises, et l'incident finit « sans réponse » si personne ne le
  prend en charge.
- **Elle n'envoie plus rien à ce contact pour cet incident** : son téléphone s'est tu,
  et ne doit pas sonner de nouveau. Le reste de son tour tombe (le relais GSM si le
  cloud n'a pas confirmé, le doublon SMS et appel à +1 min, un message internet
  retenu), et les vagues suivantes et les relances le sautent. Un SMS ou un appel déjà
  remis à la passerelle GSM part encore : ce qui est parti n'est pas retiré.
- **Une vague qui ne compte plus que des contacts qui ont vu est sautée sans attendre**,
  comme une vague vide : les suivants sont prévenus plus tôt. C'est ce que veut
  « Prévenir les autres ».
- Le « Vu » est gardé avec l'état de l'escalade, après un redémarrage aussi. Il ne rend
  pas l'alarme « prise en charge » : l'alerte d'une sirène muette reste bruyante (voir
  « Sirène muette pendant une alarme »). Un « Vu » d'un inconnu, ou pour un incident
  dont l'escalade est finie, ne change rien.

La centrale ne republie pas le « Vu » : le cloud le tient de l'application, le garde dans
le fil de l'incident et le montre aux autres contacts.

## Une centrale sans ligne SMS

Décidé par Kevin le 25 octobre 2026 (maquette SMSAbsent) : une centrale peut n'avoir
**aucune ligne SMS, par choix** (pas de modem, pas de SIM). L'installateur l'écrit dans
`hub.json` (`"without_gsm": true`, voir `operations.md`) ; elle n'a pas de `gsm.json` et
ne lance pas la passerelle GSM. Alors :

- l'escalade ne demande **jamais** de SMS ni d'appel, quels que soient les canaux d'un
  contact : aucune étape « SMS » n'est créée, donc aucune ne reste sans réponse ni ne
  finit en échec. Le relais GSM d'internet, le doublon et les SMS immédiats du feu et de
  la panique n'existent pas ; sans internet, les messages attendent son retour ;
- un contact qui n'accepte que SMS et appel n'est jamais prévenu : une vague qui ne
  compte que lui est sautée, `safehome-hub check` le signale, et `system.contacts` le
  montre sans canal ni `told_after_s` ;
- la surveillance ne suit aucune passerelle et ne lève jamais `gsm_degraded` ; le lien
  `gsm` de `system.health` vaut `none`. L'application montre une tuile grise « Sans
  SMS », au lieu de la tuile rouge d'une ligne installée mais en panne (`down`,
  `silent`).

Choix du 25 octobre 2026 : ne pas créer les étapes plutôt que les créer en échec
(« pas de SMS sur ce site ») : plus simple, et cohérent avec « une vague sans personne à
prévenir est sautée ». Par défaut, sans la clé, la ligne SMS est attendue : une
passerelle muette reste une panne.

## Plans par type d'alerte

| Type | Déclencheur | Plan |
|---|---|---|
| `intrusion` | alarme déclenchée | Vagues successives (voir ci-dessous) |
| `fire` | fumée | Tous les contacts tout de suite |
| `panic` | bouton panique | Tous les contacts tout de suite, sans sirène |
| `tamper` | sabotage | Premier contact (responsable ou propriétaire) tout de suite |
| `auth_failed` | codes faux répétés : 3 en 10 minutes (le désarmement se verrouille), ou tous les 3 depuis le dernier bon code, sans verrouillage (voir `engine.md`) | Premier contact (responsable ou propriétaire) tout de suite |
| `arming_bypassed` | armement automatique avec une zone ouverte, ou fin d'un délai de sortie (ou `arm_now`) avec une zone `instant` restée ouverte (zone contournée) | Premier contact (responsable ou propriétaire) tout de suite |
| `mains_lost` | coupure du secteur | Aucune alerte en soi, voir « Coupures de courant » |
| `battery_low_hub` | batterie de la centrale à 40 % (urgence `high`), puis à 10 % (`critical`), seulement pendant une coupure | Premier contact (responsable ou propriétaire), une alerte à chaque seuil (voir « Coupures de courant ») |
| `mains_restored` | retour du courant, seulement si une alerte de batterie est partie (champ `percent` : niveau actuel) | Internet de préférence, urgence basse |
| `device_offline`, `battery_low` | appareil muet, pile faible | Résumé quotidien au propriétaire, tous les jours tant que le défaut dure (liste dans `devices`) |
| `gsm_degraded` | secours SMS inutilisable depuis 10 minutes ; une passerelle GSM muette compte comme en panne après 3 minutes, l'alerte part donc 13 minutes après son dernier message | Propriétaire, par internet seulement |
| `zigbee_down` | capteurs injoignables depuis 3 minutes (Zigbee2MQTT ou sa clé arrêtés) ; un adaptateur muet compte comme en panne après 3 minutes, l'alerte part donc après 6 minutes : l'alarme est aveugle | Premier contact (responsable ou propriétaire) tout de suite, doublé par SMS et appel ; « 1 » pour acquitter |
| `siren_failed` | pendant une alarme, une sirène refuse l'ordre de sonner, renvois compris (voir `adapter-zigbee.md`) ; une fois par sirène et par alarme, urgence `high`, la sirène dans `devices`, sous un incident à elle qui nomme l'alarme (`alarm_incident`) | Premier contact (responsable ou propriétaire) **et** gardien tout de suite, doublé par SMS et appel, sans relance : le plan de l'alarme insiste déjà ; « 1 » pour acquitter. Une fois l'alarme prise en charge, discrète : sans appel ni sonnerie (voir « Sirène muette pendant une alarme ») |

### Intrusion : vagues

Chaque contact a un rôle : `deputy` (responsable, délégué du propriétaire,
facultatif), `guard` (gardien, facultatif), `owner` (propriétaire), `manager` (gérant)
ou `other` (autre). Les rôles absents sont sautés. Un rôle peut être tenu par plusieurs
contacts (deux propriétaires, souvent un couple ; deux responsables) : chacun reçoit ce
que reçoit son rôle, ici comme pour le « premier contact » des autres alertes (les
responsables, sinon les propriétaires) et le résumé du propriétaire. Avant le 25
octobre 2026, seul le premier de la liste l'était : le second propriétaire n'apprenait
une intrusion qu'à la première relance, à +15 min, et jamais un sabotage.

| Moment | Qui | Comment |
|---|---|---|
| 0 | Les responsables s'il y en a, sinon les propriétaires ; **et** les gardiens | Internet d'abord, GSM à +1 min sans acquittement (ou tout de suite si internet est coupé) |
| +2 min sans acquittement | Les propriétaires (si les responsables ont été prévenus en premier) ; relance des gardiens et des premiers prévenus | Internet, GSM 1 min après |
| +5 min sans acquittement | Le gérant et tous les autres contacts | Internet, GSM 1 min après |
| Ensuite, toutes les 10 min | Tous les contacts qui n'ont pas acquitté | Internet et SMS, jusqu'à 3 relances |

Sans responsable, la vague de +2 min relance quand même le propriétaire (prévenu à 0)
et le gardien. Une vague sans personne à prévenir est sautée sans attendre son délai.

Au bout des relances, l'escalade arrête l'escalade et note l'incident comme « sans
réponse ». La sirène suit son propre calendrier (voir `engine.md`).

Tous les délais sont réglables par site.

## Sirène muette pendant une alarme

Décidé le 24 octobre 2026, après l'essai du même jour : le propriétaire avait appuyé
sur « Je gère », puis la sirène a refusé de sonner, et l'alerte `siren_failed` a fait
sonner les téléphones comme une deuxième alarme.

Une sirène qui refuse l'ordre de sonner, renvois compris, lève `siren_failed` (voir
`adapter-zigbee.md`). Les alarmes qui font sonner la sirène sont l'intrusion,
l'incendie, le sabotage pendant l'armement, et la panique seulement si `panic_siren`
est vrai. Pendant une de ces alarmes, l'alerte de la sirène :

- **garde son propre incident**. Ainsi, l'acquittement ou l'arrêt de l'alarme ne la
  termine pas, la passerelle GSM ne jette pas ses SMS à la fin de l'alarme, son
  `incident_at` est le sien, et le cloud ne la prend pas pour un renvoi de l'alarme ;
- **nomme l'alarme** dans `alarm_incident`. Un téléphone qui sonne déjà pour l'alarme
  y ajoute l'alerte de la sirène au lieu de sonner une deuxième fois ;
- **part une seule fois par sirène et par alarme**, même après un redémarrage de la
  centrale : la garde est dans l'état sauvegardé de l'escalade ;
- est **bruyante ou discrète**, selon ce que décide l'escalade. C'est elle qui voit
  toutes les réponses (application, « 1 » par SMS, WhatsApp, désarmement), et son état
  est sauvegardé.

### Quand l'alerte est-elle discrète ?

Quand deux conditions sont réunies :

1. **L'alarme est prise en charge.** Un contact connu l'a acquittée (« Je gère »,
   « 1 », bouton WhatsApp), ou une personne l'a arrêtée avec un code (désarmement, ou
   sirène coupée). La fin de la sirène au bout de ses cycles ne compte pas : personne
   n'a rien fait.
2. **Aucun feu ni aucune panique du site n'attend de réponse.** Un feu ou une panique
   attend une réponse tant que son escalade est en cours. Un feu levé pendant une
   intrusion n'a pas de sirène à lui, et ses occupants comptent peut-être sur la sirène
   muette.

Le résultat ne dépend que des réponses des personnes, jamais de l'horloge. L'escalade
garde 24 heures les alarmes prises en charge, même une fois leur escalade finie : un
« 1 » qui arrive tard compte aussi.

| | Bruyante : personne n'a pris l'alarme en charge | Discrète : l'alarme est prise en charge |
|---|---|---|
| Qui | Le premier contact et le gardien | Les mêmes |
| Urgence | `high` | `low`, et `alarm_handled` |
| Texte | `ALERTE Boutique Lome: la sirene (Reserve) ne repond pas a 21:04, l'alarme est silencieuse.`, et la manière de répondre par SMS | `Boutique Lome: la sirene (Reserve) n'a pas sonne a 21:04. L'alarme est deja prise en charge.` (`L'alarme a ete arretee.` après un code), sans manière de répondre |
| Internet | D'abord | D'abord |
| SMS | Tout de suite sans internet ou liaison coupée ; si le cloud ne confirme pas en 10 s ou en cas d'échec ; en doublon à +1 min | Tout de suite sans internet ou liaison coupée ; si le cloud ne confirme pas en 10 s ou en cas d'échec ; à +1 min si le téléphone du contact n'a pas dit l'avoir reçue (`delivered`, pas seulement `sent`) |
| Appel | Tout de suite sans internet ou liaison coupée ; si le cloud ne confirme pas en 10 s ou en cas d'échec ; en doublon à +1 min | Jamais. L'appel refusé est journalisé |
| Limite par heure | Exemptée | Exemptée |
| Fin de l'escalade | `unanswered` (sans réponse) | `informed` (informé) |

Le SMS à +1 min de l'alerte discrète remplace l'appel du doublon : `sent` veut seulement
dire que le cloud a pris le message. Un téléphone Tecno ou Infinix qui dort, ou dont
l'application a été arrêtée, ne doit pas perdre le fait.

**Avant toute prise en charge, rien ne change.** L'alerte part comme avant, et
l'escalade de l'alarme continue sans être touchée. Par GSM, le premier contact et le
gardien peuvent donc recevoir deux appels à +1 min, celui de l'alarme et celui de la
sirène, et un gardien sans internet deux appels tout de suite (décidé le 24 octobre
2026).

**Une prise en charge qui arrive après la panne** rend discrète l'alerte déjà partie :
plus d'étape, le doublon devient un SMS seul et seulement si le téléphone n'a pas dit
l'avoir reçue, l'appel est refusé. Le secours SMS d'un message internet que le cloud
n'a pas confirmé reste dû. Ce qui est déjà parti n'est pas retiré (un appel déjà dans
la file de la passerelle part encore), et aucun autre incident n'est terminé. Si un feu
ou une panique attend encore une réponse, l'alerte reste bruyante, puis devient
discrète quand il est pris en charge à son tour.

**Incendie et panique.** Si l'alarme est elle-même un incendie (ou une panique avec
`panic_siren`), la même règle s'applique : une fois l'incendie pris en charge, la
sirène ne fait plus appeler le gardien, elle passe par l'application ou par SMS
(décidé le 24 octobre 2026). Un feu levé pendant une intrusion ne change pas l'alarme
de la sirène : c'est l'intrusion. Il garde l'alerte bruyante tant qu'il n'est pas pris
en charge, et son escalade n'est jamais touchée. Si une sirène est connue muette, son
texte le dit (voir « Un feu dit la sirène muette » plus bas).

**Sabotage.** Son escalade (le premier contact, une vague) finit d'habitude « sans
réponse » : personne ne l'a pris en charge, l'alerte de la sirène reste bruyante.

**Ce qui ne change pas.**

- Acquitter l'alerte de la sirène (« 1 » ou « Je gère » sur elle) ne termine qu'elle, pas
  l'alarme, qui sonne peut-être encore chez d'autres (décidé le 24 octobre 2026). Un
  « 1 » par SMS ne dit pas à quelle alerte il répond : reçu quand l'alarme et la sirène
  muette ont été envoyées au contact à quelques minutes d'écart, il acquitte d'abord la
  sirène muette, et un deuxième « 1 » l'alarme (voir `gsm-gateway.md`, « 1 »).
- Un désarmement ne coupe pas l'escalade de `siren_failed` : il la rend discrète, et elle
  finit `informed`. La sirène reste à réparer, et le fait arrive quand même. La fin de
  l'alarme sans désarmer (`alarm.end`, « Couper l'alarme ») fait exactement de même, comme
  la sirène coupée avec un code. Pour les téléphones, chacune de ces fins termine aussi
  l'alerte de la sirène : le cloud la déduit de l'`alarm_incident` de l'alerte et leur dit
  la fin avec le même mot que pour l'alarme (décidé le 26 octobre 2026, après le
  deuxième essai : « Couper l'alarme » laissait la carte de la sirène muette dans
  l'application, que seul « Désarmer » faisait partir). Un acquittement de l'alarme ne la
  termine pas.
- Un contact qui n'accepte que l'appel ne reçoit pas l'alerte discrète. C'est journalisé,
  et l'escalade de l'alarme l'a déjà appelé.
- Hors alarme (un futur test de sirène, ou le bip d'armement), `siren_failed` reste une
  alerte à part et bruyante, sans `alarm_incident` (décidé le 24 octobre 2026).

La centrale publie aussi `alarm.siren_failed` (l'alarme et la sirène) pour l'historique,
une fois par sirène et par alarme. Après un redémarrage en pleine alarme, cet événement
peut repartir une fois, mais pas l'alerte (voir `contract.md`).

### Le nom d'une sirène

Décidé le 24 octobre 2026, fait le 25 octobre : les textes nomment une sirène par le nom
que l'installateur lui donne (`siren_names` dans la configuration d'installation, voir
`operations.md`), plus par son identifiant (`dev_siren1`). Les choix par défaut, pris
le 25 octobre 2026 :

- Sans nom, une sirène s'appelle « Sirène » si c'est la seule du site sans zone, et
  « Sirène 1 », « Sirène 2 »… (dans l'ordre où elles ont été ajoutées) s'il y en a
  plusieurs. `safehome-install check` et la console le signalent quand il y en a
  plusieurs : personne ne sait laquelle est la « 2 ».
- Un nom qui dit déjà « sirène » (les noms par défaut, ou « Sirène extérieure ») sert
  de sujet : `la sirene ne repond pas`, `la sirene 2 ne repond pas`,
  `la sirene exterieure ne repond pas`. Un autre nom, celui d'un lieu, va entre
  parenthèses : `la sirene (Reserve) ne repond pas`.
- Un appareil qui a aussi une zone (un détecteur de fumée qui sonne) garde le nom de sa
  zone, et n'en prend pas d'autre : la centrale refuse un `siren_names` pour lui,
  l'outil et la console refusent de le nommer, et lui donner une zone remplace son nom
  de sirène. Il ne compte pas dans les rangs des noms par défaut : avec une seule autre
  sirène, celle-ci s'appelle « Sirène », jamais « Sirène 2 » sans « Sirène 1 ». La
  console, `safehome-install devices` et `check` le nomment comme les alertes. Choix du
  25 octobre 2026, après une revue : ces outils ne regardaient pas la zone, montraient
  un nom que les alertes ne disaient pas et acceptaient un nom de sirène sans effet.
- Le nom suit la règle d'un nom de zone : 32 caractères au plus, sans caractère de
  contrôle ni caractère invisible. Les accents sont retirés dans les textes, comme pour
  les zones.

Les mêmes noms servent aux autres alertes qui listent des appareils : au résumé
quotidien, une sirène hors ligne ou à pile faible se lit « Sirène » et plus
`dev_siren1`. Les événements d'alarme (`alarm.siren_failed`) ne portent toujours que
l'identifiant de la sirène.

### Un feu dit la sirène muette

Décidé le 24 octobre 2026, fait le 25 octobre : un feu levé pendant une alarme dont une
sirène est connue muette le dit à la fin de son texte, avant la manière de répondre :
`ALARME INCENDIE Boutique Lome: fumee detectee (Cuisine) a 21:09. La sirene ne repond
pas. Repondre 1 si vous gerez.` Les personnes sur place ne sont pas prévenues par
cette sirène. La phrase n'est jamais coupée. Elle nomme les sirènes muettes
(`La sirene (Reserve) ne repond pas.`, `Les sirenes (Sirene 1, Reserve) ne repondent
pas.`), sauf si leurs noms prennent trop de place : plus de 64 caractères, ou assez pour
que le message ne tienne plus en entier. Elle dit alors seulement `La sirene ne repond
pas.` ou `Les sirenes ne repondent pas.` Choix du 25 octobre 2026, après une revue :
l'endroit et l'heure d'un feu passent avant le nom des sirènes. Avec le nom d'une vraie
boutique (`Quincaillerie Esperance Be-Kpota`) et d'une de ses zones
(`Arriere-boutique cote cour`), nommer deux sirènes faisait couper la zone et l'heure du
SMS, qui tient en entier avec la phrase courte. Si même la phrase courte ne laisse pas
la place, c'est le message qui est coupé, comme pour un nom de site trop long : la
phrase sur la sirène et la manière de répondre restent entières.

Une sirène est connue muette, choix du 25 octobre 2026 :

- quand elle a été signalée muette pendant l'alarme qui sonne encore ;
- et qu'aucun `siren.on` d'un cycle suivant ne lui a été répondu `done` depuis. Un
  `done` à un `siren.off`, à un ordre plus ancien ou à une autre commande ne compte pas.
  Elle redevient muette si elle refuse de nouveau, sans nouvelle alerte.

C'est l'alarme qui compte, pas l'incident `siren_failed` : acquitter cette alerte la
termine mais laisse la sirène connue muette, et une sirène revenue n'est plus connue
muette même si sa `siren_failed` continue son escalade. Un `done` ne prouve pas que la
sirène sonne : avec un profil qui a un `confirm`, l'appareil a rapporté l'état demandé ;
sans, l'ordre est seulement parti vers Zigbee2MQTT. C'est le cas de la seule sirène
livrée, la Heiman HS2WD, qui ne rapporte rien quand elle sonne : une sirène signalée
muette parce que l'adaptateur était bloqué (`expired`) revient au premier ordre envoyé
après sa reprise (voir `adapter-zigbee.md`).

Une fois cette alarme finie (désarmement, sirène coupée, fin des cycles), rien n'est
dit : l'alarme suivante redonne l'ordre à chaque sirène, et une sirène qui refuse encore
lève sa propre `siren_failed`, rattachée à cette alarme. Un feu levé désarmé est sa
propre alarme : si la sirène refuse, c'est cette `siren_failed`, bruyante tant que le feu
attend une réponse, qui le dit. Le savoir est en mémoire : après un redémarrage de la
centrale, une sirène redevient connue muette à son prochain refus (au cycle suivant).
La phrase est gardée avec l'incident du feu, donc dans ses relances, après un
redémarrage aussi. Une panique ne la porte pas : elle ne fait pas sonner la sirène par
défaut.

## Coupures de courant

La centrale a sa propre batterie et sait quand elle passe dessus : elle détecte
ainsi la coupure du secteur et mesure le niveau de charge. Tant que la surveillance
fonctionne, une coupure n'est pas une alerte : les coupures sont fréquentes et
alerter à chaque fois épuiserait les contacts.

- **Coupure du secteur** : enregistrée et visible dans l'application, sans alerte.
- **Batterie à 40 %** : une alerte au propriétaire (premier contact).
- **Batterie à 10 %** : une deuxième alerte, urgente.
- Ces deux seuils sont réglables. La première alerte ne part qu'une fois, jusqu'à ce
  que la batterie remonte (par exemple au-dessus de 60 %). L'urgente part une fois
  par coupure, même si la batterie n'a pas remonté depuis la précédente : la
  centrale peut s'éteindre pendant celle-ci, et le retour du courant est alors
  annoncé de nouveau.
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

Le texte est écrit pour son canal. « Répondre 1 » ne vaut que pour un SMS : la
notification de l'application a son bouton « Je gère », le message WhatsApp son bouton
« Je m'en occupe », et un appel ne montre aucun texte. Pour ces canaux, le même texte
sans la phrase de réponse :

```
ALARME Boutique Lome: intrusion (Entree) a 21:04.
```

Trouvé à l'essai sur le matériel du 24 octobre 2026 : l'application affichait la
consigne du SMS à côté de son propre bouton.

## Échecs et repli

- **Internet inutilisable** : l'escalade envoie directement par GSM, sans attendre
  le délai de 60 secondes.
- **SMS non envoyé** (pas de réseau, SIM sans crédit) : la passerelle essaie ses
  autres lignes, puis publie un échec. Si tous les messages de la vague échouent pour
  tous ses contacts, l'escalade passe à la vague suivante sans attendre, que ces
  contacts aient l'application ou non : un relais par GSM qui n'a plus rien à envoyer
  (tous les canaux GSM du contact sont déjà partis, après l'échec d'internet) n'est plus
  attendu. Avant le 25 octobre 2026, ce relais prévu à 60 secondes retenait la vague
  suivante jusqu'à son délai pour tout contact qui avait l'application. De même sans
  liaison internet : le message internet retenu jusqu'à son retour ne retient pas la
  vague suivante quand tous les messages GSM de la vague ont échoué. Il reste dû et part
  au retour de la liaison (corrigé le 25 octobre 2026 après une revue : il retenait la
  vague suivante jusqu'à son délai, au moment où le GSM venait d'échouer). Sans aucune
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
  alertes moins graves qu'une intrusion (sabotage, codes faux, zone contournée…) n'en
  prennent que la moitié : un flot de codes faux tapés à l'application ne doit pas
  laisser une intrusion sans message. Les alertes de type `fire`, `panic` et
  `siren_failed` ne sont jamais bridées. Pour la
  sirène, c'est le seul message qui dit que le magasin est silencieux : il ne doit pas
  être retenu après les vagues et les relances de l'alarme. Son volume est borné
  autrement : une vague sans relance, une fois par sirène et par alarme.
- Un incident a un identifiant, repris dans tous ses messages et événements.

## Côté cloud

Le cloud surveille le heartbeat de chaque site (voir `contract.md`). Si trois
heartbeats manquent, le cloud avertit le propriétaire par WhatsApp ou SMS, depuis
ses propres canaux. Il ne peut pas compter sur la centrale pour le faire, puisque
c'est la centrale qui est muette.

## Contrat

Ajouts à reporter dans `contract.md` :

- `alert.request` : `incident`, `incident_at` (l'heure où l'incident a été levé, la
  même à chaque vague et relance), `zone` (le fuseau IANA du site), `kind`, `urgency`,
  `wave`, `contact`, `channel` (`app`, `whatsapp`, `sms`, `call`), `text`, et pour
  l'alerte d'une sirène muette pendant une alarme `alarm_incident` (l'alarme) et
  `alarm_handled` (l'alarme est déjà prise en charge : l'alerte est discrète) ;
  `zone_name`, le nom de la zone de la cause (« Entrée »), quand un seul appareil d'une
  zone a levé une intrusion, un incendie, une panique ou un sabotage.
- `alert.result` : `ref` (le `alert.request`), `status` (`queued`, `sent`,
  `delivered`, `failed`), `reason` si échec. Pour `app` et `whatsapp`, `sent` veut
  dire que le cloud a pris le message, `delivered` qu'il est arrivé sur le
  téléphone.
- `alert.ack` : `incident`, `contact`, `via` (`sms`, `app`).
- `alert.seen` : `incident`, `contact`, `at`. Le « Vu » de l'application, qui n'arrête
  rien (voir « Vu »).
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

- **Horloge avancée au démarrage.** Le Pi 4 sans module d'horloge repart après une
  coupure de sa dernière heure connue, puis NTP l'avance d'un coup de toute la durée de
  la coupure. La boucle le voit sur l'horloge monotone (voir `architecture.md`) : la
  prochaine vague ou relance de chaque incident, l'attente de la confirmation du cloud
  et le relais par le GSM avancent d'autant, comme la limite horaire et la mémoire d'un
  jour (`Escalation.Stepped`) ; sans cela, toutes les vagues et relances partaient
  d'un coup. `incident_at` reste l'heure de la levée : les étapes comptent d'elle plus
  le saut (`moved` dans l'état sauvegardé). De même, la supervision avance le dernier
  message de la passerelle GSM, de l'adaptateur Zigbee et de l'agent, et le début d'une
  panne pas encore dite (`Monitor.Stepped`) : pas de `zigbee_down` ni de
  `gsm_degraded` pour un service entendu juste avant le saut (26 octobre 2026).

- **Une demande par contact et par canal.** Les alertes brutes du moteur ne sont plus
  publiées telles quelles : l'escalade les transforme en `alert.request` précis (`contact`,
  `channel`, `wave`, `text`). Un canal n'a qu'à envoyer ce qu'on lui demande. Chaque
  demande porte aussi l'heure de l'incident (`incident_at`, celle de sa levée, gardée
  dans l'état sauvegardé : la même à chaque vague et relance, même après un
  redémarrage) et le fuseau du site (`zone`) : l'application dit ainsi « intrusion à
  16:15 (heure de la boutique) » pour une deuxième vague partie à 16:17, même sur un
  téléphone dans un autre fuseau.
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
  entier (toutes ses alertes). Un acquittement d'un inconnu est ignoré. L'escalade
  retient 24 heures que l'incident est pris en charge, même si l'acquittement arrive
  après la fin de son escalade : l'alerte d'une sirène muette qui le nomme est alors
  discrète (voir « Sirène muette pendant une alarme »).
- **Fin de l'incident.** Un désarmement par une personne, ou l'arrêt de la sirène avec
  un code, arrête l'escalade (`Close`) : il serait absurde de relancer les contacts
  après coup. L'arrêt de la sirène par expiration ne l'arrête pas ; mais le
  désarmement qui suit, par une personne, arrête alors celle des intrusions et des
  sabotages de cet armement, quoi qu'ait fait la sirène entre-temps (le moteur les
  retient, voir `engine.md`, « Reprise de la sirène »). Un feu ou une panique dont la
  sirène s'est déjà arrêtée vont, eux, jusqu'à l'acquittement : le désarmement ne dit
  pas que quelqu'un a vu le feu. Pendant que la sirène sonne, le désarmement ou l'arrêt
  avec un code arrête l'escalade de l'alarme qui sonne, quel que soit son type, et
  compte comme sa prise en charge, retenue 24 heures. Un désarmement ne termine pas
  l'alerte d'une sirène en panne (`siren_failed`) : la sirène reste à réparer. Il la
  rend discrète (voir « Sirène muette pendant une alarme »).
- **Regroupement.** Un même incident et un même type d'alerte levés plusieurs fois se
  fusionnent : les appareils s'ajoutent, aucun message de plus ne part. Le moteur ne
  lève jamais deux fois la même intrusion (il se tait une fois déclenché) : c'est une
  sécurité, qui ne sert pas aujourd'hui pour les intrusions.
- **Limite par heure.** 20 messages par contact et par heure (réglable), sauf fire, panic
  et siren_failed. Les intrusions peuvent tous les prendre ; les autres alertes bridées
  la moitié au plus (10), comptée à part, pour qu'il en reste toujours aux intrusions.
  Un message retenu est journalisé. Corrigé le 25 octobre 2026 après une revue : un
  flot d'alertes moins graves (codes faux, sabotages) pouvait user la limite avant
  l'intrusion.
- **Alertes non urgentes** (résumés, retour du courant, protection dégradée) : internet
  seulement. Si internet est coupé elles attendent son retour. Le retour du courant peut
  partir par GSM après une heure sans internet.
- **Textes.** En français, en ASCII simple, 160 caractères au plus (le nom du site et des
  zones sont nettoyés de leurs accents) ; un texte par canal : seul le SMS porte la
  manière de répondre (`Repondre 1 si vous gerez.`), gardée quand le texte est coupé.
- **Reprise.** L'état est sauvegardé : après un redémarrage, les vagues et les délais
  en retard sont rejoués dans l'ordre, à leur heure. Les alarmes prises en charge et
  les sirènes déjà signalées y sont aussi, 24 heures (un état écrit avant elles se
  relit). Les plans, eux, sont ceux de la configuration du nouveau démarrage : un
  incident gardé à une étape que son plan n'a plus (des rappels retirés dans
  `escalation.plans`, ou par une mise à jour) se termine sans réponse, sans autre
  message. Avant le 25 octobre 2026, cette étape revenait sans fin : la boucle de
  l'escalade ne répondait plus, le chien de garde de systemd tuait la centrale, qui
  repartait sur le même état, sans fin, moteur d'alarme compris.

La passerelle GSM (`safehome-gsm`) exécute les demandes `sms` et `call`, connaît les
numéros des contacts et publie `alert.ack` pour une réponse « 1 ». Les canaux
application et WhatsApp partent par l'agent de liaison vers le cloud (dépôt
`safeHome-cloud`), qui envoie WhatsApp et les notifications de l'application (Android, en essai depuis le 28 octobre 2026). Pas encore
fait : le changement des plans depuis le cloud (les contacts, eux, arrivent par
`config.set`, que le cloud envoie dès qu'ils changent).

## Questions ouvertes

Aucune question ouverte pour le moment.

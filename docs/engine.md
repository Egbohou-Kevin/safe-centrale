# Moteur d'alarme

Statut : spécifié pour la v1. Le cœur est codé dans `internal/engine` (voir « Avancement »).

Le moteur d'alarme est le programme en Go qui tourne sur la centrale (le
Raspberry Pi, plus tard notre propre carte). Il décide ce que fait le système à
partir des événements reçus. Il ne parle ni Zigbee ni au modem : il lit et publie
des messages du contrat (`contract.md`) sur le broker MQTT local.

## Responsabilités

Le moteur :

- tient l'état de l'alarme et ses transitions,
- applique les règles de zones et les délais,
- commande la sirène,
- lève des alertes ; l'escalade de la centrale en fait les `alert.request` et suit
  leur résultat,
- publie l'état et les événements d'alarme.

Il ne fait pas : le pilotage radio (adaptateur), l'envoi de SMS (passerelle GSM),
la prise de photos (service caméras), la liaison cloud (agent).

## Entrées et sorties

| Entrées | Source |
|---|---|
| `device.event` et `device.state` | adaptateur |
| Ordres d'armement et de désarmement | clavier, application, planning |
| Tic d'horloge | interne |

| Sorties | Destination |
|---|---|
| `alarm.*` et `alarm/state` | broker, agent de liaison |
| `command` (par exemple `siren.on`) | adaptateur |
| Alertes | escalade de la centrale, qui en fait les `alert.request` |

## Principe de conception

Le cœur du moteur est une fonction pure :

```
(état, événement, heure) → (nouvel état, actions)
```

Les délais sont aussi des événements (« le délai d'entrée a expiré »). Il n'y a pas
d'appel direct à l'horloge système dans la logique. Conséquences :

- on teste chaque scénario avec une horloge simulée, sans attendre ;
- on rejoue une séquence réelle d'événements pour comprendre un incident ;
- le comportement est prévisible, ce qu'on attend du cœur d'une alarme.

Les entrées et sorties (MQTT, disque, horloge) sont à l'extérieur de cette
fonction.

## États

| État | Sens |
|---|---|
| `disarmed` | Désarmé. Seules les zones permanentes surveillent. |
| `arming` | Délai de sortie : on a armé, personne ne doit encore déclencher. |
| `armed` | Armé. |
| `entry_delay` | Une porte d'entrée s'est ouverte, le délai pour désarmer court. |
| `triggered` | Alarme déclenchée : sirène et alertes. |

```
disarmed ──arm──► arming ──délai de sortie écoulé──► armed
                                                      │
                          porte d'entrée ouverte ─────┤
                                                      ▼
                                                 entry_delay
                                                      │
        zone instantanée / interior (depuis armed)    │ délai d'entrée écoulé
                          │                           │
                          ▼                           ▼
                       triggered ◄────────────────────┘
                          │
                          └─ cycles de sirène épuisés ──► état d'avant l'alarme

disarm : depuis arming, armed, entry_delay ou triggered, retour à disarmed.
```

Après les cycles de sirène (voir « Reprise de la sirène »), le système repasse dans
son état d'avant l'alarme (`armed` ; `disarmed` pour un feu déclenché désarmé ;
`arming` pour une alarme pendant le délai de sortie, qui repart alors en entier, voir
« Armement et délai de sortie »), avec la mémoire de ce qui s'est déclenché. Il ne
reste pas bloqué en `triggered`.

## Zones

Chaque appareil appartient à une zone, qui détermine son comportement.

| Type de zone | Capteurs | Quand le système est armé | Quand il est désarmé |
|---|---|---|---|
| `entry` | contact, mouvement, vibration, bris de vitre | `entry_delay` | rien |
| `instant` | contact, mouvement, vibration, bris de vitre | déclenchement immédiat | rien |
| `interior` | mouvement, contact | déclenchement, sauf pendant un délai d'entrée | rien |
| `24h_fire` | fumée | déclenchement | déclenchement |
| `24h_panic` | panique | alerte silencieuse (sans sirène par défaut) | alerte silencieuse |
| `24h_tamper` | sabotage | déclenchement | alerte sans sirène |

Les zones permanentes (`24h_*`) surveillent même désarmé.

Le capteur de sabotage d'un appareil compte toujours comme un sabotage (règle
`24h_tamper`), quelle que soit sa zone, même sans zone (clavier, sirène). Ouvrir un
détecteur de fumée n'est donc pas un feu. Les autres capteurs ne comptent que si le
type de zone les accepte (`engine.Accepts`, dans `internal/engine/zones.go`). Les
outils d'installation lisent la même table : un appareil n'est jamais mis dans une
zone qu'il ne peut pas déclencher.

## Transitions détaillées

| De | Événement | Vers | Actions |
|---|---|---|---|
| `disarmed` | ordre d'armement, zones fermées | `arming` | publie `alarm.arming`, démarre le délai de sortie |
| `disarmed` | ordre d'armement, zone ouverte | `disarmed` | refus, publie la liste des zones ouvertes (voir « Armement avec une zone ouverte ») |
| `arming` | délai de sortie écoulé | `armed` | publie `alarm.armed` |
| `arming` | ordre de désarmement | `disarmed` | publie `alarm.disarmed` |
| `armed` | zone `entry` ouverte | `entry_delay` | publie `alarm.entry_delay`, démarre le délai d'entrée |
| `armed` | zone `instant` ou `interior` | `triggered` | `siren.on`, alerte, publie `alarm.triggered` |
| `entry_delay` | ordre de désarmement valide | `disarmed` | arrête les délais, publie `alarm.disarmed` |
| `entry_delay` | délai d'entrée écoulé | `triggered` | idem déclenchement |
| `entry_delay` | zone `instant` | `triggered` | idem déclenchement |
| `entry_delay` | sabotage, fumée, panique qui sonne | `triggered` | idem déclenchement ; le délai d'entrée continue derrière l'alarme et devient une intrusion s'il n'est pas arrêté (voir « Une alarme pendant une autre ») |
| `armed` | ordre de désarmement | `disarmed` | publie `alarm.disarmed` ; termine l'escalade des intrusions et sabotages de cet armement dont la sirène s'est arrêtée seule, que `alarm.disarmed` nomme dans `incidents` |
| `triggered` | ordre de désarmement | `disarmed` | `siren.off`, publie `alarm.cleared` et `alarm.disarmed` ; termine l'escalade de l'alarme en cours (et de celles de l'armement arrêtées seules), toutes nommées dans `incidents` d'`alarm.disarmed` |
| `triggered` | `alarm.silence`, code valide | état d'avant l'alarme | `siren.off`, `alarm.cleared` ; termine l'escalade de l'alarme en cours ; un délai de sortie repart en entier |
| `triggered` | `alarm.end`, code valide | état d'avant l'alarme | comme `alarm.silence`, et termine aussi les alarmes de l'armement dont la sirène s'est arrêtée seule ; chaque `alarm.cleared` porte `ended` |
| `armed` | `alarm.end`, code valide | `armed` | termine l'escalade des intrusions et sabotages de cet armement dont la sirène s'est arrêtée seule, sans désarmer : un `alarm.cleared` (`user`, `ended`, `duration_s` 0) pour chacun ; rien en cours, l'ordre est fait et ne change rien |
| `arming` | ordre `arm_now` | `armed` | termine le délai de sortie, publie `alarm.armed` |
| `triggered` | cycle de sirène écoulé, cycles restants | `triggered` | `siren.off`, pause, puis `siren.on` au cycle suivant ; un acquittement (« Je gère », SMS « 1 ») n'y change rien, seuls le code et le désarmement arrêtent la sirène |
| `triggered` | cycles de sirène épuisés | état d'avant l'alarme (`armed`, `disarmed` pour un feu déclenché désarmé, `arming` pendant le délai de sortie) | `siren.off`, publie `alarm.cleared` ; un délai de sortie repart en entier |
| tout état sauf `triggered` | fumée | `triggered` (cause fumée) | sirène et alerte |
| tout état | panique | inchangé | alerte urgente, sans sirène par défaut (réglable par site) |
| `triggered` | fumée, panique qui sonne, sabotage, zone `instant` (site armé sous l'alarme), d'un genre autre que l'alarme qui sonne, ou feu pendant un feu dont tous les détecteurs sont revenus au repos | `triggered` | alerte sous son propre incident, une fois par genre ; un feu sonne à son tour quand l'alarme finit (voir « Une alarme pendant une autre ») |

## Délais et durées (valeurs par défaut)

| Réglage | Défaut | Remarque |
|---|---|---|
| Délai de sortie | 2 min | Le client peut armer tout de suite sans attendre (voir « Armement et délai de sortie ») |
| Délai d'entrée | 45 s | Le temps de désarmer au clavier ; un code tapé avant la fin est attendu pendant sa vérification, 10 s au plus (voir « Désarmement à distance avec un code ») |
| Durée d'un cycle de sirène | 5 min | Voir « Reprise de la sirène » |
| Pause entre deux cycles | 2 min | |
| Nombre de cycles | 3 | À confirmer : plafond pour limiter le bruit et la batterie |
| Délai d'oubli d'appareil | 4 h | Un capteur muet plus longtemps est déclaré hors ligne |

Tous sont réglables par site.

## Armement et délai de sortie

Quand le client arme, le système démarre le délai de sortie de 2 minutes et
l'annonce : message sur l'application (événement `alarm.arming`). Le message donne
l'heure de fin (« armement automatique dans 2 min »). Les bips du clavier ne sont pas
faits : le contrat n'a pas de commande vers le clavier (seulement `siren.on`,
`siren.off` et `device.identify`). Le client peut aussi
**armer tout de suite**, sans attendre la fin, avec une touche du clavier ou
l'ordre `alarm.arm_now` de l'application.

Armer tout de suite termine le délai restant : le système passe en `armed`
immédiatement. Si quelqu'un est encore à l'intérieur, un mouvement déclenche
l'alarme, donc cette action est réservée à celui qui sait être sorti.

Le désarmement pendant le délai de sortie annule l'armement.

Une alarme pendant le délai de sortie (un feu, un sabotage, une panique qui sonne) ne
l'arme pas à sa fin : le système revient en `arming`, avec un délai de sortie entier
(`alarm.arming` de nouveau, au nom de la même personne). Celui qui coupe au clavier la
sirène d'une fausse alerte de fumée est encore à l'intérieur : armé tout de suite, son
pas suivant déclencherait une intrusion. Si personne n'arrête la sirène, la fin de ce
délai fait ce que fait toute fin de délai de sortie (`alarm.armed`, zones restées
ouvertes contournées et signalées). Corrigé le 25 octobre 2026 après une revue :
l'alarme finie, le système passait en `armed` sans délai ni contournement.

À la fin du délai de sortie (ou à `arm_now`), une porte `entry` restée ouverte
démarre le délai d'entrée, comme si elle venait de s'ouvrir. Une zone `instant`
restée ouverte est contournée, et une alerte `arming_bypassed` liste les appareils
(`devices`) : personne n'est plus au clavier pour l'apprendre.

L'ordre `alarm.bypass` ne contourne que pendant le délai de sortie, et seulement une
porte ou une fenêtre (zone `entry` ou `instant`) ouverte à ce moment : jamais une zone
`interior`, une porte fermée ou un appareil inconnu, ni à l'avance, désarmé. Sinon il
est refusé entier (`wrong_state`, `detail.devices`). Chaque contournement lève une
alerte `arming_bypassed` qui liste les appareils : rien d'autre ne le montrerait. L'agent
ne le laisse jamais venir du cloud. Un contact ne signale
que ses changements : une fenêtre ouverte pendant le délai et restée ouverte ne
dirait plus rien ensuite.

## Reprise de la sirène

Quand l'alarme se déclenche, la sirène sonne 5 minutes. Si personne n'a rien fait
(ni désarmement ni coupure de la sirène par un code valide), elle s'arrête deux
minutes, puis reprend pour un nouveau cycle, jusqu'à 3 cycles. À la fin des
cycles, le système repasse dans son état d'avant l'alarme (`armed`, `disarmed`
pour un feu déclenché désarmé, `arming` pendant le délai de sortie), avec la mémoire
de ce qui s'est déclenché.
Les alertes continuent de suivre leur escalade pendant ce temps.

Un désarmement ou une coupure de sirène avec un code valide arrête tout
immédiatement.

L'état publié (`alarm.state`) suit ce programme : en `triggered`, il nomme l'incident
qui sonne, dit si la sirène est dans un cycle (`siren` à `on`) ou dans la pause
(`paused`), et jusqu'à quand (`ends_at`, voir `contract.md`). Ce sont les heures du
programme : un tic en retard ne les décale pas, et un redémarrage non plus (l'échéance
est dans l'état sauvegardé, et `Boot` rejoue à leur heure les étapes manquées).

Quand la sirène s'arrête seule, l'alarme n'est pas finie pour les contacts : son
escalade continue. Le moteur retient les intrusions et les sabotages de l'armement
en cours dont la sirène s'est arrêtée seule (dans son état sauvegardé, donc après un
redémarrage aussi) ; le désarmement par une personne qui termine cet armement
termine aussi leur escalade, quoi qu'ait fait la sirène entre-temps. Un feu ou une
panique ne s'arrêtent pas ainsi : ils sont surveillés jour et nuit, et désarmer la
boutique plus tard ne dit pas que quelqu'un les a vus ; leur escalade va jusqu'à
l'acquittement d'un contact (ou au bout de ses relances). Constaté à l'essai sur le
matériel du 24 octobre 2026 : un désarmement depuis l'application une minute après
l'arrêt de la sirène laissait partir la vague suivante de l'intrusion.

La centrale vérifie que chaque sirène prend l'ordre : un `siren.on` refusé est renvoyé
quelques fois, puis les contacts reçoivent l'alerte `siren_failed` (voir
`adapter-zigbee.md`). Elle nomme l'alarme qui sonne, et part discrète si cette alarme est
déjà prise en charge (voir `escalation.md`, « Sirène muette pendant une alarme »). Le
moteur n'en sait rien : il continue ses cycles. Les textes nomment la sirène par son
nom dans la configuration d'installation (`siren_names`), ou par défaut « Sirène »,
« Sirène 2 ». Jusqu'à la fin de cette alarme, ou jusqu'à ce qu'elle prenne de nouveau
un ordre de sonner, la sirène est connue muette : un feu levé entre-temps le dit dans
son texte, puisqu'il n'a pas de programme de sirène à lui tant qu'elle sonne et que
ses occupants ne l'entendent pas (voir `escalation.md`, « Un feu dit la sirène
muette »).

### Un site sans sirène

Décidé le 26 octobre 2026, au deuxième essai sur la vraie centrale : le site déclarait
une sirène jamais appairée, et chaque intrusion levait une `siren_failed`. Un site sans
sirène est une configuration valide et calme : `"siren_ids": []` dans `hub.json` (la
ligne absente reste refusée : un oubli, pas un choix). `safehome-hub check` l'accepte et
le dit (« no siren, by choice »), le script d'installation aussi.

- Le moteur ne change pas : son programme mesure toujours la durée de l'alarme (cycles et
  pauses, puis la fin d'origine `timeout`), et l'escalade suit le sien.
- La centrale n'envoie aucun ordre de sirène, ne publie aucun `alarm.siren_cycle`, et ne
  lève jamais `siren_failed` : rien ne peut refuser de sonner.
- `system.devices` dit `sirens` à `0` (le nombre de sirènes que la configuration nomme,
  appairées ou non) : le cloud et l'application ne disent jamais « La sirène sonne »
  (`alarm.state` garde le mot `siren` du programme, voir `contract.md`).
- Les outils d'installation ne retirent jamais la dernière sirène d'un site : un site sans
  sirène est un choix écrit dans `hub.json`, pas l'effet d'un retrait.

## Une alarme pendant une autre

Rien de ce qui arrive pendant une alarme n'est perdu. Une autre alarme d'un genre
différent de celle qui sonne lève son alerte, sous son propre incident, une fois par
genre et par alarme qui sonne (un sabotage une fois par appareil). Une alarme du genre
de celle qui sonne n'ajoute rien : elle est déjà dite. Sauf un feu pendant un feu dont
tous les détecteurs sont revenus au repos : c'est un nouveau feu (voir plus bas). Corrigé le 25 octobre 2026 après
une revue : jusque-là, seuls un feu ou une panique alertaient, et un feu n'avait jamais
de sirène à lui.

- **Feu** (ou panique qui fait sonner la sirène) : pas de programme de sirène à lui tant
  que l'autre alarme sonne, puisque la sirène sonne déjà. Il attend sa fin, quelle qu'elle
  soit (code, désarmement, cycles épuisés), puis sonne à son tour, sous l'incident de son
  alerte, avec ses propres cycles, sans alerter de nouveau. La personne qui arrête une
  intrusion au clavier entend donc la sirène repartir pour le feu. Le moteur retient
  chaque détecteur en alarme : un deuxième détecteur qui se déclenche pendant que le
  premier attend rejoint son incident, sans nouvelle alerte, et le feu sonne tant que
  l'un d'eux est encore en alarme. Un détecteur revenu au repos avant la fin de l'autre
  alarme ne sonne plus. Quand tous sont revenus au repos, un détecteur qui se déclenche
  ensuite (le même ou un autre) est un nouveau feu : il alerte de nouveau, sous un
  nouvel incident (la première alerte a pu être acquittée « ce n'était que de la
  vapeur »), et attend la sirène à son tour. Corrigé le 25 octobre 2026 après une
  revue : ce second feu était perdu, sans alerte ni sirène.
- **Feu pendant un feu qui sonne** : la même règle. Tant qu'un détecteur du feu qui
  sonne est encore en alarme, un autre détecteur le rejoint, sans nouvelle alerte. Une
  fois qu'ils sont tous revenus au repos, un détecteur qui se déclenche est un nouveau
  feu : il alerte sous son propre incident et sonne quand le premier finit, quelle
  que soit cette fin. Corrigé le 25 octobre 2026 après une revue : il était tenu pour
  le feu déjà dit, et le code qui coupait la sirène du premier (« ce n'était que de la
  vapeur ») le laissait sans sirène ni alerte.
- **Intrusion** (une zone `instant`) quand le site est armé sous l'alarme (un feu ou un
  sabotage alors qu'il était armé ou pendant le délai de sortie) : une alerte, sans
  sirène à elle. Comme pendant un
  délai d'entrée, la porte d'entrée et l'intérieur ne disent rien : celui qui entre
  répond le plus souvent à l'alarme. Le désarmement qui termine l'armement termine son
  escalade, comme celle d'une intrusion dont la sirène s'est arrêtée seule.
- **Sabotage** : une alerte, sans sirène à lui. Le moteur ne retient un sabotage comme
  signalé (et ne le redit pas après un redémarrage) que s'il l'a vraiment signalé.

L'état publié (`alarm.state`) reste celui de l'alarme qui sonne.

Une alarme d'un autre genre qu'une intrusion pendant un délai d'entrée (sabotage, feu,
panique qui sonne) sonne aussitôt, mais ne prend pas la place de l'intrusion : le délai
d'entrée continue derrière elle, et il est gardé dans la sauvegarde du moteur
(`entry_ends`). Si aucun code ne l'arrête avant sa fin, celui qui est entré est un
intrus : l'intrusion lève son alerte sous son propre incident, avec l'escalade d'une
intrusion (vagues, gardien, rappels), comme une intrusion pendant une autre alarme ;
le désarmement qui termine l'armement termine son escalade. Sinon, quelqu'un qui arrache
le capot du clavier ou de la sirène pendant les 45 secondes ramènerait l'escalade
complète d'une intrusion au seul message d'un sabotage. Un désarmement avant la fin
termine tout, sans intrusion. Un `alarm.silence` (qui ne désarme pas) avant la fin rend
le reste du délai d'entrée : l'état revient à `entry_delay`, avec sa fin d'origine.
Corrigé le 25 octobre 2026 après une revue.

## Sources d'armement et de désarmement

- **Clavier** (code ou badge) : le cas principal.
- **Application ou cloud** : commande `alarm.arm` ou `alarm.disarm` envoyée au
  site.
- **Planning** : armement automatique à l'heure de fermeture.
- **SMS** depuis un numéro autorisé : hors v1 (l'authentification par SMS reste à
  sécuriser).

Toute commande est datée, a un émetteur identifié et est enregistrée dans un
événement `alarm.armed` ou `alarm.disarmed` avec son origine.

### Désarmement à distance avec un code

Le désarmement à distance est autorisé, mais il exige **le code du site**, en plus
de la connexion à l'application. Un téléphone volé ne suffit donc pas : le voleur
doit aussi connaître le code.

- Le code est **vérifié sur le site**, par la centrale (`internal/access`) ; le
  moteur reçoit le résultat et applique le verrouillage. Le code est stocké haché sur
  la centrale. Le cloud le transmet sans le garder ni l'écrire dans ses journaux.
- Après 3 codes faux en 10 minutes, le moteur refuse tout désarmement pendant 10
  minutes et publie un événement `alarm.auth_failed` avec une alerte aux contacts.
- Les 10 minutes glissent : seuls comptent les codes faux des 10 dernières minutes
  (`engine.LockoutWindow`). Deux codes faux, puis un troisième 11 minutes après le
  premier, ne verrouillent pas ; trois codes faux espacés de quelques heures non plus.
  Un bon code remet le compte à zéro, comme le verrouillage.
- Un second compte ignore le temps : les codes faux depuis le dernier bon code
  (`wrong_codes` dans la sauvegarde du moteur, clavier et application ensemble). Au
  3e, puis au 6e, au 9e..., les contacts reçoivent l'alerte `auth_failed`, **sans
  verrouillage** : l'`alarm.auth_failed` de ce code n'a pas de `locked_until`. Sans
  lui, quelqu'un de patient (un voleur avec le téléphone du propriétaire, que le cloud
  laisse essayer 2 codes toutes les 10 minutes, ou quelqu'un au clavier qui espace ses
  essais) resterait sous la fenêtre : jamais verrouillé, et personne ne le saurait,
  les `alarm.auth_failed` n'allant qu'à l'historique, que le voleur lit. Seul un bon
  code remet ce compte à zéro ; un verrouillage, non.
- Une horloge qui a avancé puis reculé (module d'horloge défaillant, heure fausse au
  démarrage corrigée ensuite par internet) ne prolonge rien : un code faux daté du
  futur compte comme entré maintenant, et un verrouillage finit au plus tard 10 minutes
  (`lockout_duration`) après maintenant. De même pour les délais et la sirène : un délai
  de sortie ou d'entrée, un cycle de sirène ou une pause finit au plus tard sa propre
  durée après maintenant (sinon, un délai d'entrée d'une heure où une vraie intrusion
  ne sonne pas, ou une sirène qui sonne une heure). L'armement automatique est
  recalculé : un plan fait sur une horloge en avance a pu sauter la fermeture du jour
  (voir « Armement automatique et report »). Cela vaut après un redémarrage comme
  pendant que la centrale tourne : la boucle du moteur lui donne l'heure telle qu'elle
  est, en arrière aussi (`driver.Options.FollowsClock`), et le moteur ramène ce qu'il
  avait daté dans le futur à chaque entrée. Le cas courant est d'ailleurs le second :
  la centrale démarre avant la synchronisation par internet, qui recule l'horloge
  pendant qu'elle tourne. Un ordre de l'application garde le temps qu'il lui reste à
  l'horloge de la centrale, qui l'a vérifié. Corrigé le 25 octobre 2026 après une
  revue : pendant une exécution, le temps du moteur s'arrêtait jusqu'à ce que
  l'horloge le rattrape, et tout ce qu'il posait durait le recul de plus.
- La vérification d'un code est lente exprès (environ une seconde par code enregistré
  sur la centrale). Un bon code tapé dans la dernière seconde du délai d'entrée
  arriverait au moteur après sa fin : la sirène et la première vague d'alertes
  partiraient pour rien. La centrale dit donc au moteur, dès qu'un code arrive, qu'il
  est en cours de vérification (`engine.CodeEntered`) ; la fin du délai d'entrée
  attend l'ordre d'un code tapé dans ses 10 dernières secondes, 10 secondes au plus
  (`engine.CodeCheckHold`, pas un réglage : il borne aussi ce qu'une suite de codes
  faux peut ajouter au délai). Un code faux ne retient plus rien une fois vérifié :
  l'alarme part aussitôt. Un code tapé après la fin ne retient rien. Corrigé le 25
  octobre 2026 après une revue.
- Les ordres arrivent au moteur dans l'ordre où ils arrivent à la centrale : un ordre
  sans code (armer, reporter) tapé pendant qu'un code est vérifié attend derrière lui.
  Un « armer » tapé juste après un « désarmer » avec son code n'est donc plus fait en
  premier.
- Une alerte tous les 3 codes faux, pas à chaque code faux : c'est le rythme
  d'avant la fenêtre (le 3e code faux de suite verrouillait et alertait), et la limite
  par contact et par heure de l'escalade (20 messages, `escalation.md`) est partagée
  avec les intrusions. Un flot d'alertes `auth_failed` ne l'étouffe pas : les alertes
  moins graves qu'une intrusion n'en prennent que la moitié, l'autre reste aux
  intrusions (corrigé le 25 octobre 2026 après une revue). Un
  téléphone au rythme du cloud déclenche une alerte toutes les 15 minutes, 4 par
  heure. Un code faux ne lève jamais deux alertes : quand le 3e code faux verrouille
  aussi, une seule part. Au pire (un verrouillage qui ne tombe pas sur un multiple de
  3), deux alertes pour trois codes faux.
- Le même contrôle s'applique au clavier, avec le même compte : codes faux du clavier
  et de l'application s'ajoutent, et le verrouillage vaut pour les deux.
- La fenêtre n'est pas un réglage : le cloud borne chaque téléphone à 2 codes faux en
  10 minutes, ce qui ne garde un téléphone sous le verrouillage du site que si les deux
  fenêtres sont les mêmes. Le nombre de codes (`lockout_attempts`, 3) et la durée du
  verrouillage (`lockout_duration`, 10 minutes) restent réglables dans `hub.json`.

Idée pour plus tard, hors v1 : un code de contrainte, qui désarme normalement
mais envoie une alerte silencieuse, utile en cas de braquage.

### Un code par personne

Chaque personne autorisée (5 au maximum) a son propre code, lié à son identifiant
(`user`). Le moteur sait donc **qui** arme, désarme ou coupe la sirène, et l'inscrit
dans les événements. Retirer l'accès d'une personne revient à supprimer son code, sans
changer celui des autres. Le verrouillage après 3 codes faux en 10 minutes s'applique au site entier.

### Armement automatique et report

Le client définit une heure de fermeture, éventuellement différente chaque jour
de la semaine, dans le fuseau du site (Lomé : UTC, sans changement d'heure).

1. **Avant l'heure** : 10 minutes avant, le système prévient (message sur
   l'application ; bips du clavier pas faits, voir plus haut) que l'armement est
   prévu à l'heure indiquée.
2. **Report** : le client peut repousser la fermeture, avec une touche du clavier
   ou depuis l'application. Chaque report ajoute 30 minutes. Il y a un plafond de
   3 reports et une heure limite après laquelle le système arme quand même.
3. **À l'heure** : le système passe en `arming` et déclenche le délai de sortie de
   2 minutes, avec le même message. Quelqu'un encore à l'intérieur peut désarmer,
   ou repousser si le plafond n'est pas atteint.
4. **Zone ouverte à l'heure prévue** : l'armement automatique ne peut pas être
   refusé en silence. Il tente d'armer, contourne les zones ouvertes et le signale
   aux contacts, car oublier d'armer est le risque que cette fonction supprime.

Les valeurs (10 min, 30 min, 3 reports) sont des défauts réglables par site.

Choix faits au codage :

- **Heure limite** : c'est un délai après l'heure de fermeture prévue
  (`AutoArmLimit`, 2 h par défaut), pas une heure du jour : cela évite les cas
  limites autour de minuit. Un report n'ajoute jamais plus que ce qui reste avant
  cette limite ; quand il n'en reste plus, le report est refusé (`postpone_limit`).
- **Le plan** n'existe que lorsque le système est désarmé (et pendant le délai de
  sortie qu'il a lancé, pour qu'on puisse encore le repousser). Il est recalculé à
  chaque désarmement : on désarme à 21 h 20, il réarmera donc à 22 h ; on désarme à
  22 h 01 pendant le délai de sortie, il n'armera plus avant le lendemain. Une alarme
  pendant que le plan existe (un feu désarmé, pendant le délai de sortie) le garde : à
  sa fin, par les cycles épuisés ou par un code qui coupe la sirène, le plan reprend,
  et si l'heure de fermeture est passée entre-temps, la boutique arme alors, avec un
  délai de sortie entier, comme après un armement manqué (sous la limite du plan). Un
  désarmement est un choix : il refait le plan depuis son heure. Corrigé le 25 octobre
  2026 après une revue : un feu fini après l'heure de fermeture effaçait l'armement du
  soir.
- **Report pendant le délai de sortie** : il annule ce délai (événement
  `alarm.disarmed`, contournements effacés), puis le plan repart de l'heure prévue
  plus 30 minutes. Chaque report fait de nouveau l'objet d'un préavis.
- **Pas de code** pour reporter : c'est une facilité, pas un droit d'accès. Elle est
  donc pour la centrale elle-même : l'agent ne laisse pas venir `alarm.postpone` du cloud.
- **Zone ouverte** : l'armement automatique contourne la zone et envoie une alerte
  `arming_bypassed` qui liste les appareils (`devices`).
- **Fuseau** : `Config.Location` (UTC par défaut, donc Lomé). Les heures de
  fermeture sont lues dans ce fuseau, une par jour de la semaine ; un jour sans
  heure n'arme pas.
- **Nouvelle configuration** : elle s'applique par un redémarrage, que la centrale
  fait désarmée. Au redémarrage, le plan sauvegardé est comparé aux heures de
  fermeture : une fermeture retirée ou déplacée refait le plan depuis maintenant (la
  première fermeture après le redémarrage), une fermeture inchangée garde ses reports,
  sous la limite et le préavis de la nouvelle configuration. Sans cela, le plan fait
  avec les anciennes heures armait quand même : à 22 h une boutique restée ouverte
  tard, ou trop tard une boutique dont la fermeture avait avancé (corrigé le 25 octobre
  2026 après une revue).
- **Armement manqué** (centrale éteinte ou arrêtée à l'heure prévue, une échéance en
  retard de plus d'une minute) : revenue avant la limite du plan (2 h après la
  fermeture), elle arme quand même, mais le délai de sortie part maintenant, pour que
  ceux qui sont dedans aient tout le délai pour reporter ou sortir ; revenue après, elle
  ne rejoue pas l'armement de la veille sur une boutique rouverte et prévoit la
  fermeture suivante. Avant le 25 octobre 2026, l'armement était rejoué à son heure : le
  délai de sortie était fini depuis longtemps, et le premier mouvement des employés le
  lendemain matin déclenchait l'alarme.

## Armement avec une zone ouverte

Par défaut, l'armement est refusé si une zone `entry` ou `instant` est ouverte. Le
moteur publie les zones concernées (dans la réponse à l'ordre). Le clavier ne signale
pas encore l'erreur : le contrat n'a pas de commande vers le clavier. Un armement
forcé (`force: true`) met ces zones en contournement pour cette période d'armement
et le signale dans l'événement.

## Défauts et supervision

- **Pile faible** : avertissement, ne bloque pas l'armement.
- **Appareil hors ligne** (muet plus de 4 h) : défaut signalé, armement possible
  avec avertissement.
- **Coupure secteur** : événement enregistré, sans alerte. Le système continue sur
  batterie.
- **Batterie de la centrale basse** : alertes à 40 % puis à 10 %, car le site
  risque de s'éteindre.
- La source de ces deux points est le module batterie de la centrale (Geekworm X728,
  `safehome-power`, voir `alimentation.md`), qui publie la capacité `mains` et la charge.
- **Modem ou adaptateur en panne** : défaut signalé dans `system.health`. Le
  moteur continue.

## Persistance et redémarrage

L'état courant est écrit sur disque (écriture atomique) à chaque transition. Au
redémarrage :

1. le moteur lit son dernier état ;
2. il lit l'état retenu des appareils sur le broker, chacun daté de son début
   (`since`), et note quelles portes et fenêtres sont ouvertes, sans encore agir. Un
   état plus ancien que ce que le moteur sait est écarté (voir plus bas) ;
3. il rejoue à leur heure les échéances passées pendant l'arrêt, avec les portes et
   fenêtres telles qu'elles sont maintenant : une porte refermée pendant l'arrêt ne
   lance pas le délai d'entrée à la fin du délai de sortie, une fenêtre refermée n'est
   pas contournée ; ce qui est encore ouvert est traité comme à la fin de tout délai de
   sortie (voir « Armement et délai de sortie »). Puis il reprend l'état obtenu : armé
   reste armé ; déclenché reste `triggered`, et la sirène reprend pour ce qui reste du
   cycle en cours (rien pendant une pause ; si les cycles se sont épuisés pendant
   l'arrêt, il est revenu à son état d'avant l'alarme, en général `armed`). Désarmé, il
   vérifie d'abord le plan d'armement automatique contre la configuration, et un
   armement manqué pendant l'arrêt suit la règle de « Armement automatique et report » ;
4. si une zone est encore en alarme, il se déclenche. Ce qu'il savait déjà avant
   l'arrêt n'est pas nouveau : un sabotage déjà signalé et toujours actif n'est pas
   signalé de nouveau, une porte ou une fenêtre déjà connue ouverte et toujours ouverte
   ne déclenche rien (une porte intérieure laissée ouverte à l'armement, ou la porte
   d'entrée restée ouverte après une sirène coupée au code). Un contact ne dit que ses
   changements : sans le redémarrage, rien n'aurait sonné. Un sabotage apparu pendant
   l'arrêt, ou une porte ouverte pendant l'arrêt, l'est. Un état plus ancien que ce
   que le moteur sait ne déclenche rien non plus (voir plus bas) ;
5. il publie son état retenu ; le redémarrage se voit au `boot_id` du heartbeat.

L'heure de l'armement est gardée dans l'état (`armed_since`) : `alarm.state` en
`armed` la donne pour `since`, même après un délai d'entrée ou une alarme qui ont
laissé le local armé, et après un redémarrage. L'heure d'entrée dans le mode
(`since` de l'état sauvegardé) reste celle du retour à `armed`, que l'étape 2 utilise
pour les capteurs momentanés : un mouvement commencé pendant l'alarme reste ancien.
Corrigé le 26 octobre 2026 après l'essai sur la vraie centrale : une alarme finie
seule republiait « armée depuis » l'heure de sa fin.

Les états retenus du broker peuvent être vieux de quelques minutes : Mosquitto ne les
écrit sur disque que toutes les 5 minutes (`autosave_interval`, pour la carte SD), et
après un arrêt brutal (une coupure sans la carte X728, un redémarrage par le chien de
garde) il rend chacun tel qu'il était à sa dernière écriture. Une porte refermée juste
avant la coupure reviendrait ouverte, et serait prise pour une porte ouverte pendant
l'arrêt : une sirène au retour du courant. De même un détecteur de fumée revenu au
repos, ou un mouvement dont le retour au repos n'était pas encore écrit. Le moteur, lui,
écrit son état à chaque changement. Il garde donc, dans son état, l'heure du dernier
état qu'il a pris de chaque porte, fenêtre, détecteur de fumée et sabotage (`heard`),
et écarte au démarrage l'état retenu qui n'est pas plus récent. Un capteur
momentané (mouvement, vibration, bris de vitre) n'a pas cette heure, que chaque passant
réécrirait : son état est écarté s'il a commencé avant le mode où est le moteur (ou
avant la fin du délai de sortie en cours). Vivant, il a été entendu alors et a fait ce
qu'il fait dans ce mode ; actif depuis, ce n'est pas un nouveau mouvement. Un état qui
ne dit pas quand il a commencé, ou daté après maintenant (une horloge partie en avant),
est pris tel quel. Un état changé pendant la fenêtre de démarrage est une nouvelle de
ce démarrage, jamais écartée. Corrigé le 25 octobre 2026 après une revue : une fenêtre
fermée deux minutes avant une coupure, la boutique armée, déclenchait l'intrusion au
retour du courant.

Après une restauration, le moteur ne rejoue aucune échéance avant `Boot` : ni tic, ni
rattrapage au passage d'un ordre ou d'un signal reçu pendant la fenêtre de démarrage.
Cet ordre ou ce signal s'applique à l'état sauvegardé. C'est `Boot` qui rejoue les
échéances manquées, une fois les états connus. Un signal reçu pendant la fenêtre est
une nouvelle de ce démarrage : appliqué à l'état sauvegardé, il a pu y être écarté
(l'alarme qui sonnait à l'arrêt, finie depuis), et `Boot` le reprend avec les états
retenus. Une porte ouverte pendant la fenêtre n'est donc pas « déjà connue ouverte »
(corrigé le 25 octobre 2026 après une revue : l'intrusion entendue dans la fenêtre
était perdue). Un cycle de sirène rejoué en retard ne
sonne que ce qui en reste, une seule fois (un seul `siren.on`, pas un par cycle
rejoué).

Un appareil retiré du réseau est oublié : la centrale envoie `Forget` au moteur et à
la supervision à chaque `registry.device_removed`. Il ne tient plus une porte ouverte,
un contournement ni un sabotage déjà vu, pas même un sabotage que le mode installation
gardait pour sa fermeture. Ce que les incidents en ont retenu reste.

## Contrat

Les ordres reçus (`alarm/cmd`), les accusés (`alarm/ack`) et les événements
publiés par le moteur sont définis dans `contract.md`. Le moteur publie
`alarm.arming` au début du délai de sortie (avec l'heure de fin pour le
compte à rebours) et `alarm.armed` quand le système est réellement armé.

## Alertes et escalade

Le moteur lève des alertes d'un genre (`intrusion`, `fire`, `panic`, `tamper`,
`auth_failed`, `arming_bypassed`) ; la supervision lève les siennes (appareil muet,
pile faible, passerelles) ; la centrale lève `siren_failed` pour une sirène qui ne
prend pas l'ordre de sonner, rattachée à l'alarme qui sonne, et donne à un feu levé
pendant cette alarme les sirènes connues muettes, que son texte dit. L'escalade, dans la
centrale, en fait les `alert.request` : qui est prévenu, dans quel ordre et après
combien de temps sans réponse est décrit dans le document d'escalade. Cette logique
reste sur le site pour fonctionner sans internet.

## Tests

Tous les scénarios s'écrivent en tableau avec horloge simulée :

- armement, désarmement dans le délai de sortie,
- entrée normale avec désarmement dans le délai,
- entrée sans désarmement,
- intrusion par fenêtre, par mouvement,
- fumée désarmé et armé,
- panique,
- redémarrage pendant l'armement et pendant le déclenchement,
- porte ouverte à l'armement, avec et sans forçage,
- appareil muet, pile faible, coupure secteur.

## Questions ouvertes

Décidé :

- Délai de sortie 2 min, avec armement immédiat possible. Délai d'entrée 45 s.
- Sirène : cycles de 5 min, pause de 2 min, reprise tant que rien n'est fait.
- Panique silencieuse par défaut.
- Désarmement à distance autorisé, avec le code du site.
- Armement automatique à l'heure de fermeture, avec possibilité de report.

- Sirène : 3 cycles au maximum.
- Verrouillage après 3 codes faux en 10 minutes (fenêtre glissante), pendant 10
  minutes. Alerte aux contacts, sans verrouillage, tous les 3 codes faux depuis le
  dernier bon code.
- Armement automatique : préavis de 10 min, report de 30 min, 3 reports au
  maximum.
- Pas d'armement ni de désarmement par SMS dans la v1.

Le moteur est entièrement spécifié pour la v1. Les ajouts sont reportés dans
`contract.md` et les JSON Schema sont écrits.

## Avancement

Codé et testé dans `internal/engine` (plus de 60 scénarios, horloge simulée) :

- États, zones, délais, armement forcé et contournement, `arm_now`.
- Cycles de sirène, silence, désarmement, fumée, panique, sabotage.
- Codes faux et verrouillage, expiration des ordres, accusés.
- Sauvegarde de l'état (`Snapshot`) et reprise après redémarrage (`Boot`).
- Armement automatique à l'heure de fermeture : préavis, report (`alarm.postpone`),
  limite, zone ouverte contournée avec alerte (plus de 20 scénarios dans
  `schedule_test.go`).

Codé et testé dans `internal/health` (supervision, domaine séparé du moteur, même
règles : pur, sans horloge) :

- Coupure de secteur sans alerte, alertes de batterie de la centrale à 40 % puis à
  10 % (la première une fois par recharge, réarmée au-dessus de 60 % ; l'urgente à
  chaque coupure, la centrale pouvant s'éteindre pendant celle-ci), message de retour
  du courant seulement si une alerte de batterie est partie.
- Piles faibles des capteurs (15 %, réarmées au-dessus de 30 %) et appareils muets,
  regroupés dans un résumé quotidien à 9 h. L'heure du prochain résumé est sauvegardée ;
  au redémarrage, une heure plus tardive que le prochain 9 h vient d'une horloge partie
  en avance puis reculée : elle est ramenée au prochain 9 h, sinon le résumé se tairait
  sur les appareils muets et les piles mortes jusqu'à cette heure-là (corrigé le 25
  octobre 2026 après une revue).
- Services suivis par leur état répété (GSM, Zigbee, agent) : l'heure du dernier état
  de chaque service n'est pas sauvegardée (elle change chaque minute et userait la
  carte SD). Après un redémarrage, le silence compte depuis le redémarrage, même si
  un état retenu plus ancien arrive : ce qu'il dit vaut, pas son âge. Après une
  coupure, les services se sont arrêtés avec la centrale et reviennent avec elle :
  dater leur silence d'avant le redémarrage lèverait une alerte urgente à chaque
  démarrage. Un service qui ne revient pas est dit muet trois minutes après le
  redémarrage, puis signalé dans les délais habituels. Un service déjà muet le reste.
  Une panne encore ouverte à l'arrêt et pas encore signalée (une ligne GSM qui a perdu
  le réseau pendant la coupure, un pont Zigbee arrêté avec la centrale) repart elle
  aussi du redémarrage : signalée dans les délais habituels si elle dure, jamais dès le
  démarrage. Une panne déjà signalée ne l'est pas de nouveau.
- La liste des appareils (`Monitor.Devices`, d'où la centrale tire `system.devices`,
  voir `contract.md`) : les appareils du registre (entrée `Registry`) avec leur zone
  (`Config.Zones`, reprises de `hub.json`), en ligne ou non, et pour un appareil hors
  ligne depuis quand et sa dernière nouvelle (données par l'adaptateur dans
  `Availability`), la pile arrondie à la dizaine et la pile faible (même règle que le
  résumé). Rien dans la liste ne bouge avec l'activité de la boutique : la supervision
  ne reçoit ni porte ni mouvement, et un appareil qui répond n'y a pas d'heure (ce que
  dit encore le `ts` d'une liste envoyée : voir `contract.md`, « `system.devices` »).
  La liste fait partie de l'état sauvegardé : après un redémarrage, elle est entière
  avant le retour des états retenus. La pile y est gardée arrondie, pour ne pas
  réécrire l'état à chaque point perdu. Un état sauvegardé avant la liste se relit :
  elle se remplit avec les messages retenus.

Codé et testé côté pilote (`internal/driver`, `internal/adapter/filestore`) :

- Boucle d'horloge générique : échéance du domaine devenue un tic à la bonne heure,
  entrées traitées dans l'ordre, temps qui ne recule jamais (sauf pour un domaine qui
  suit l'horloge, comme le moteur et l'adaptateur Zigbee).
- Sauvegarde de l'état après chaque changement, sans réécrire un état identique
  (la carte SD s'use), avec écriture atomique, copie de la version précédente et
  reprise sur cette copie si le fichier est abîmé. Un état abîmé des deux côtés
  empêche le démarrage : c'est à l'application de décider, pas un défaut silencieux.
- Une erreur d'envoi ou de sauvegarde n'arrête pas la boucle.

Codé et testé dans `internal/access` (domaine des codes d'accès, voir
`security-install.md`) : un code par personne (5 au maximum), vérification qui retrouve
la personne, refus des codes faibles ou déjà pris, sauvegarde avant prise en compte.
Il fournit au moteur l'`Auth` (`Checked`, `OK`, `User`) ; la centrale fait le
branchement (ci-dessous).

Codé et testé dans `internal/hub` et `cmd/safehome-hub` (le branchement de bout en bout,
voir `operations.md`) : messages MQTT vers les domaines, enveloppes et topics en sortie,
vérification des codes sur un fil à part, commande de la sirène, clavier, démarrage avec
les états retenus, battement de cœur. Un test lance la centrale entière contre un vrai
broker : armement au clavier, porte forcée, sirène, mauvais code puis bon code,
redémarrage qui garde l'état armé, rechargement des codes sans redémarrage.

Codé dans `internal/hub` : l'avertissement à l'armement. Le moteur ne connaît pas la
santé des appareils ; la centrale garde le dernier résumé de la supervision et ajoute
les appareils muets et les piles faibles à l'événement `alarm.arming` et à la réponse
`done` de l'ordre d'armement (voir `contract.md`).

Codé dans `internal/hub` : la liste des appareils, `system.devices` (voir
`contract.md`), décidée le 25 octobre 2026 pour la fiche Capteurs de l'application.
Choix faits, à connaître :

- La centrale lit le registre retenu de l'adaptateur (`registry/devices`) et donne les
  zones de `hub.json` à la supervision, qui fait la liste.
- Elle suit la présence de chaque appareil par son état `availability` retenu, au
  démarrage **et ensuite** : lui seul dit depuis quand un appareil est hors ligne et sa
  dernière nouvelle (`heard`). L'événement `availability`, qui ne dit ni l'un ni
  l'autre, ne compte plus pour la supervision ; l'adaptateur publie toujours les deux
  ensemble. Les autres états retenus ne comptent toujours qu'au démarrage.
- Elle publie la liste à la fin du démarrage, une fois les états retenus revenus (sinon
  chaque état revenu ferait une liste à lui), puis seulement quand ce qu'elle dit
  change, comparée octet par octet à la dernière publiée, comme `system.health`. Une
  liste part donc à chaque démarrage de la centrale, et l'agent la redit au cloud à
  chaque connexion. Pas de renvoi périodique.
- Une liste due à un changement part tout de suite, même celle d'un appareil revenu,
  dont le `ts` est l'heure de son premier message, souvent une porte : l'application
  le montre revenu sans attendre. La retenir ne cacherait rien (un événement `battery`
  ou `tamper` du même message part au cloud tout de suite). Le `ts` de celle d'un
  appareil passé hors ligne redonne aussi sa dernière nouvelle à la seconde près.
  Quelques moments par appareil et par an, jamais le fil de l'activité (voir
  `contract.md`, « `system.devices` »).
- La pile est arrondie à la dizaine la plus proche, comme le signal GSM : 15 % se lit
  20 %, avec `battery_low`. `since` et `heard` partent à la minute, ce que montre
  l'application ; la minute ne cache rien, vu le `ts`. Un appareil dont aucune
  capacité ne donne un type n'est pas dans la liste (aucun profil aujourd'hui) : le
  schéma exige un type que l'application sait dire.

Pas encore codé :

- Le canal application côté cloud : l'application n'existe pas encore. Le canal
  WhatsApp, lui, est codé dans le cloud (dépôt `safeHome-cloud`), éteint tant que le
  compte Meta n'existe pas : le cloud répond alors `failed` et l'escalade passe au GSM.

Les liens de `system.health` viennent chacun de leur programme : le GSM et les lignes
de la passerelle (`gsm.status`), le Zigbee de l'adaptateur (`zigbee.status`, voir
`adapter-zigbee.md`), internet de l'agent de liaison (`agent.status`).

Choix faits au codage, à connaître :

- `Engine` est un objet déterministe plutôt qu'une fonction pure : même principe
  (pas d'horloge, pas d'entrée-sortie, tout vient des entrées), plus simple à écrire.
- Le moteur ne connaît pas le contrat : il émet des événements du domaine (`ArmingStarted`,
  `Triggered`…) et reçoit des signaux de capteurs. `internal/engine/wire` les traduit en
  messages du contrat, ce qui isole le moteur des changements de version.
- Les échéances dépassées sont rejouées à leur heure, même si le tic arrive en
  retard (centrale suspendue, coupure) : le calendrier de la sirène reste exact.
- Un désarmement sans code vérifié compte comme un code faux.
- Un état « detected » encore retenu au redémarrage déclenche l'alarme s'il a commencé
  après le début du mode en cours (voir « Persistance et redémarrage ») : les
  adaptateurs doivent publier le retour au repos (`clear`) des capteurs de mouvement.
- Les événements d'alarme portent un `incident` (identifiant), repris par les
  alertes, pour relier la sirène, les SMS et les acquittements.

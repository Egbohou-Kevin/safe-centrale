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
son état d'avant l'alarme (`armed`, ou `disarmed` pour un feu déclenché désarmé),
avec la mémoire de ce qui s'est déclenché. Il ne reste pas bloqué en
`triggered`.

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
| `armed` | ordre de désarmement | `disarmed` | publie `alarm.disarmed` |
| `triggered` | ordre de désarmement | `disarmed` | `siren.off`, publie `alarm.cleared` et `alarm.disarmed` |
| `triggered` | `alarm.silence`, code valide | état d'avant l'alarme | `siren.off`, `alarm.cleared` |
| `arming` | ordre `arm_now` | `armed` | termine le délai de sortie, publie `alarm.armed` |
| `triggered` | cycle de sirène écoulé, rien acquitté, cycles restants | `triggered` | `siren.off`, pause, puis `siren.on` au cycle suivant |
| `triggered` | cycles de sirène épuisés | état d'avant l'alarme (`armed`, ou `disarmed` pour un feu déclenché désarmé) | `siren.off`, publie `alarm.cleared` |
| tout état | fumée | `triggered` (cause fumée) | sirène et alerte |
| tout état | panique | inchangé | alerte urgente, sans sirène par défaut (réglable par site) |

## Délais et durées (valeurs par défaut)

| Réglage | Défaut | Remarque |
|---|---|---|
| Délai de sortie | 2 min | Le client peut armer tout de suite sans attendre (voir « Armement et délai de sortie ») |
| Délai d'entrée | 45 s | Le temps de désarmer au clavier |
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

À la fin du délai de sortie (ou à `arm_now`), une porte `entry` restée ouverte
démarre le délai d'entrée, comme si elle venait de s'ouvrir. Une zone `instant`
restée ouverte est contournée, et une alerte `arming_bypassed` liste les appareils
(`devices`) : personne n'est plus au clavier pour l'apprendre. Un contact ne signale
que ses changements : une fenêtre ouverte pendant le délai et restée ouverte ne
dirait plus rien ensuite.

## Reprise de la sirène

Quand l'alarme se déclenche, la sirène sonne 5 minutes. Si personne n'a rien fait
(ni désarmement ni coupure de la sirène par un code valide), elle s'arrête deux
minutes, puis reprend pour un nouveau cycle, jusqu'à 3 cycles. À la fin des
cycles, le système repasse dans son état d'avant l'alarme (`armed`, ou `disarmed`
pour un feu déclenché désarmé), avec la mémoire de ce qui s'est déclenché.
Les alertes continuent de suivre leur escalade pendant ce temps.

Un désarmement ou une coupure de sirène avec un code valide arrête tout
immédiatement.

La centrale vérifie que chaque sirène prend l'ordre : un `siren.on` refusé est renvoyé
quelques fois, puis les contacts reçoivent l'alerte `siren_failed` (voir
`adapter-zigbee.md`).

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
- Après 3 codes faux de suite, le moteur refuse tout désarmement pendant 10
  minutes et publie un événement `alarm.auth_failed` avec une alerte aux contacts.
- Le même contrôle s'applique au clavier : même verrouillage après plusieurs
  erreurs.

Idée pour plus tard, hors v1 : un code de contrainte, qui désarme normalement
mais envoie une alerte silencieuse, utile en cas de braquage.

### Un code par personne

Chaque personne autorisée (5 au maximum) a son propre code, lié à son identifiant
(`user`). Le moteur sait donc **qui** arme, désarme ou coupe la sirène, et l'inscrit
dans les événements. Retirer l'accès d'une personne revient à supprimer son code, sans
changer celui des autres. Le verrouillage après 3 codes faux s'applique au site entier.

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
  22 h 01 pendant le délai de sortie, il n'armera plus avant le lendemain.
- **Report pendant le délai de sortie** : il annule ce délai (événement
  `alarm.disarmed`, contournements effacés), puis le plan repart de l'heure prévue
  plus 30 minutes. Chaque report fait de nouveau l'objet d'un préavis.
- **Pas de code** pour reporter : c'est une facilité, pas un droit d'accès.
- **Zone ouverte** : l'armement automatique contourne la zone et envoie une alerte
  `arming_bypassed` qui liste les appareils (`devices`).
- **Fuseau** : `Config.Location` (UTC par défaut, donc Lomé). Les heures de
  fermeture sont lues dans ce fuseau, une par jour de la semaine ; un jour sans
  heure n'arme pas.

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
2. il lit l'état retenu des appareils sur le broker et note quelles portes et
   fenêtres sont ouvertes, sans encore agir ;
3. il rejoue à leur heure les échéances passées pendant l'arrêt, avec les portes et
   fenêtres telles qu'elles sont maintenant : une porte refermée pendant l'arrêt ne
   lance pas le délai d'entrée à la fin du délai de sortie, une fenêtre refermée n'est
   pas contournée ; ce qui est encore ouvert est traité comme à la fin de tout délai de
   sortie (voir « Armement et délai de sortie »). Puis il reprend l'état obtenu : armé
   reste armé ; déclenché reste `triggered`, et la sirène reprend pour ce qui reste du
   cycle en cours (rien pendant une pause ; si les cycles se sont épuisés pendant
   l'arrêt, il est revenu à son état d'avant l'alarme, en général `armed`) ;
4. si une zone est encore en alarme, il se déclenche. Un sabotage déjà vu avant
   l'arrêt et toujours actif n'est pas signalé de nouveau ; un sabotage apparu pendant
   l'arrêt l'est ;
5. il publie son état retenu ; le redémarrage se voit au `boot_id` du heartbeat.

Après une restauration, le moteur ne rejoue aucune échéance avant `Boot` : ni tic, ni
rattrapage au passage d'un ordre ou d'un signal reçu pendant la fenêtre de démarrage.
Cet ordre ou ce signal s'applique à l'état sauvegardé. C'est `Boot` qui rejoue les
échéances manquées, une fois les états connus. Un cycle de sirène rejoué en retard ne
sonne que ce qui en reste, une seule fois (un seul `siren.on`, pas un par cycle
rejoué).

Un appareil retiré du réseau est oublié : la centrale envoie `Forget` au moteur et à
la supervision à chaque `registry.device_removed`. Il ne tient plus une porte ouverte,
un contournement ni un sabotage déjà vu. Ce que les incidents en ont retenu reste.

## Contrat

Les ordres reçus (`alarm/cmd`), les accusés (`alarm/ack`) et les événements
publiés par le moteur sont définis dans `contract.md`. Le moteur publie
`alarm.arming` au début du délai de sortie (avec l'heure de fin pour le
compte à rebours) et `alarm.armed` quand le système est réellement armé.

## Alertes et escalade

Le moteur lève des alertes d'un genre (`intrusion`, `fire`, `panic`, `tamper`,
`auth_failed`, `arming_bypassed`) ; la supervision lève les siennes (appareil muet,
pile faible, passerelles) ; la centrale lève `siren_failed` pour une sirène qui ne
prend pas l'ordre de sonner. L'escalade, dans la centrale, en fait les `alert.request` :
qui est prévenu, dans quel ordre et après combien de temps sans réponse est décrit
dans le document d'escalade. Cette logique reste sur le site pour fonctionner sans
internet.

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
- Verrouillage après 3 codes faux, pendant 10 minutes.
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
  10 % (une par seuil et par coupure, réarmées au-dessus de 60 %), message de retour
  du courant seulement si une alerte de batterie est partie.
- Piles faibles des capteurs (15 %, réarmées au-dessus de 30 %) et appareils muets,
  regroupés dans un résumé quotidien à 9 h.
- Services suivis par leur état répété (GSM, Zigbee, agent) : l'heure du dernier état
  de chaque service n'est pas sauvegardée (elle change chaque minute et userait la
  carte SD). Après un redémarrage, le silence compte depuis le redémarrage, sauf si
  un état retenu plus ancien arrive : il fait foi. Un service déjà muet le reste.

Codé et testé côté pilote (`internal/driver`, `internal/adapter/filestore`) :

- Boucle d'horloge générique : échéance du domaine devenue un tic à la bonne heure,
  entrées traitées dans l'ordre, temps qui ne recule jamais.
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
- Un état « detected » périmé au redémarrage déclenche l'alarme : les adaptateurs
  doivent publier le retour au repos (`clear`) des capteurs de mouvement.
- Les événements d'alarme portent un `incident` (identifiant), repris par les
  alertes, pour relier la sirène, les SMS et les acquittements.

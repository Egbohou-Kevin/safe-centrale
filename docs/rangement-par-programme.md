<img src="marque/seq-icone.svg" width="56" alt="SeQ">

# Ranger `internal` par programme

Proposition du 11/11/2026 (demande de Kevin, 23:28). Kevin, 23:35 : « qu'elle enchaîne sans m'attendre » :
le renommage suit cette table sans attendre son oui ; les choix marqués « décidé seule, à relire »
sont les options recommandées, réversibles, prises sans lui.
Lue sur `main` à `be47049` : 69 paquets sous `internal`, 9 programmes sous `cmd`.

```
internal/
├── hub/       app/{hub,configapp}  core/{engine,health,escalation,access}(+wire)  adapter/machine  config/hubconfig
├── zigbee/    app/zigbeeapp        core/zigbee(+wire,z2m)                                          config/zigbeeconfig
├── gsm/       app/gateway          core/gsm(+wire)                  adapter/atmodem               config/gsmconfig
├── agent/     app/agentapp         core/{agent,sealcode}            adapter/{queuedir,upload}     config/agentconfig
├── video/     app/videoapp         core/video(+wire)                adapter/go2rtc                config/videoconfig
├── power/     app/powerapp         core/power(+wire)                adapter/x728                  config/powerconfig
├── maint/     app/maintapp         core/maint(+wire)                adapter/sshtunnel             config/maintconfig
├── console/   app/consoleapp(+web)                                  adapter/hotspot               config/consoleconfig
├── install/   app/install
├── shared/    contract(+schematest) driver inbox outbox service
│              adapter/{mqtt,filestore,filelock,pidfile,sdnotify,sysclock,photodir}  config/{configfile,siteconfig}
└── checks/    archtest qualitytest

dépendances permises :  cmd/safehome-X ─► internal/X ─► internal/shared
                        dans X : app ─► core ;  core ─► shared/contract seulement ;  adapter ─► rien du module
exceptions nommées :    console ─► hub (codes, réglages), install ;  install ─► hub/config ;  hub/config ─► install
```

## Règles de nommage

1. **Un dossier par programme**, au nom du programme sans `safehome-` : `internal/hub` sert
   `cmd/safehome-hub`. Dedans, quatre dossiers de rôle au plus : `app`, `core`, `adapter`,
   `config`.
2. **Le dernier élément du chemin garde le nom du paquet d'aujourd'hui** :
   `internal/hub/core/engine` reste le paquet `engine`, `internal/gsm/core/gsm` reste `gsm`.
   Seules les lignes d'`import` changent ; aucun identifiant, aucun alias. Le prix : un nom répété
   (`hub/app/hub`, `gsm/core/gsm`). Renommer les paquets en `app`, `core`… obligerait à des alias
   partout où un programme en importe deux (la console importe l'application du hub et la sienne).
3. **Le commun s'appelle `shared`** : ce qui sert à deux programmes ou plus. Ses outils
   (`contract`, `driver`, `inbox`, `outbox`, `service`) sont directement dessous ; ses adaptateurs
   sous `shared/adapter`, ses réglages sous `shared/config`. Rien d'autre.
4. **`…/wire` et `zigbee/z2m` restent sous leur cœur** (`hub/core/engine/wire`) : ils traduisent ce
   cœur et lui seul, la règle `TestWireOnlyBridgesItsDomainAndTheContract` le tient déjà.
5. **Un paquet de test reste sous le paquet qu'il simule** : `mqtttest` sous
   `shared/adapter/mqtt`, `drivertest` sous `shared/driver`, `outboxtest` sous `shared/outbox`,
   `attest` sous `gsm/adapter/atmodem`, `go2rtctest` sous `video/adapter/go2rtc`.
   `schematest` (lit les schémas du contrat) va sous `shared/contract`.
6. **Les garde-fous vont sous `checks`** (`checks/archtest`, `checks/qualitytest`) : ils ne servent
   aucun programme, ils lisent tout le dépôt.

## La table, dossier par dossier

Rangée par nouveau chemin. « Programmes » : les `cmd/safehome-…` qui l'embarquent (dépendances
transitives, `go list -deps`), « les neuf » quand tous le font.

| Dossier actuel (`internal/…`) | Nouveau chemin (`internal/…`) | Rôle | Programmes |
|---|---|---|---|
| `adapter/queuedir` | `agent/adapter/queuedir` | adaptateur | agent |
| `adapter/upload` | `agent/adapter/upload` | adaptateur | agent |
| `agentapp` | `agent/app/agentapp` | application | agent |
| `agentconfig` | `agent/config/agentconfig` | réglages | agent |
| `agent` | `agent/core/agent` | cœur | agent |
| `sealcode` | `agent/core/sealcode` | cœur | agent |
| `archtest` | `checks/archtest` | garde-fou | — (tests du dépôt) |
| `qualitytest` | `checks/qualitytest` | garde-fou | — (tests du dépôt) |
| `adapter/hotspot` | `console/adapter/hotspot` | adaptateur | console |
| `consoleapp/web` | `console/app/consoleapp/web` | application | console |
| `consoleapp` | `console/app/consoleapp` | application | console |
| `consoleconfig` | `console/config/consoleconfig` | réglages | console |
| `adapter/atmodem/attest` | `gsm/adapter/atmodem/attest` | outil de test | — (tests seulement) |
| `adapter/atmodem` | `gsm/adapter/atmodem` | adaptateur | gsm |
| `gateway` | `gsm/app/gateway` | application | gsm |
| `gsmconfig` | `gsm/config/gsmconfig` | réglages | gsm |
| `gsm/wire` | `gsm/core/gsm/wire` | traduction | gsm |
| `gsm` | `gsm/core/gsm` | cœur | gsm |
| `adapter/machine` | `hub/adapter/machine` | adaptateur | hub |
| `configapp` | `hub/app/configapp` | application | hub |
| `hub` | `hub/app/hub` | application | console, hub, install |
| `hubconfig` | `hub/config/hubconfig` | réglages | console, hub, install |
| `access` | `hub/core/access` | cœur | console, hub, install |
| `engine/wire` | `hub/core/engine/wire` | traduction | console, hub, install |
| `engine` | `hub/core/engine` | cœur | console, hub, install |
| `escalation/wire` | `hub/core/escalation/wire` | traduction | console, hub, install |
| `escalation` | `hub/core/escalation` | cœur | console, hub, install |
| `health/wire` | `hub/core/health/wire` | traduction | console, hub, install |
| `health` | `hub/core/health` | cœur | console, hub, install |
| `install` | `install/app/install` | application | console, hub, install |
| `adapter/sshtunnel` | `maint/adapter/sshtunnel` | adaptateur | maint |
| `maintapp` | `maint/app/maintapp` | application | maint |
| `maintconfig` | `maint/config/maintconfig` | réglages | maint |
| `maint/wire` | `maint/core/maint/wire` | traduction | maint |
| `maint` | `maint/core/maint` | cœur | maint |
| `adapter/x728` | `power/adapter/x728` | adaptateur | power |
| `powerapp` | `power/app/powerapp` | application | power |
| `powerconfig` | `power/config/powerconfig` | réglages | power |
| `power/wire` | `power/core/power/wire` | traduction | power |
| `power` | `power/core/power` | cœur | power |
| `adapter/filelock` | `shared/adapter/filelock` | adaptateur | console, hub |
| `adapter/filestore` | `shared/adapter/filestore` | adaptateur | les, neuf |
| `adapter/mqtt/mqtttest` | `shared/adapter/mqtt/mqtttest` | outil de test | — (tests seulement) |
| `adapter/mqtt` | `shared/adapter/mqtt` | adaptateur | les, neuf |
| `adapter/photodir` | `shared/adapter/photodir` | adaptateur | agent, video |
| `adapter/pidfile` | `shared/adapter/pidfile` | adaptateur | les, neuf |
| `adapter/sdnotify` | `shared/adapter/sdnotify` | adaptateur | les, neuf |
| `adapter/sysclock` | `shared/adapter/sysclock` | adaptateur | hub, video |
| `configfile` | `shared/config/configfile` | réglages | les, neuf |
| `siteconfig` | `shared/config/siteconfig` | réglages | console, gsm, hub, install, video |
| `schematest` | `shared/contract/schematest` | outil de test | — (tests seulement) |
| `contract` | `shared/contract` | contrat | les, neuf |
| `driver/drivertest` | `shared/driver/drivertest` | outil de test | — (tests seulement) |
| `driver` | `shared/driver` | outil commun | les, neuf |
| `inbox` | `shared/inbox` | outil commun | les, neuf |
| `outbox/outboxtest` | `shared/outbox/outboxtest` | outil de test | — (tests seulement) |
| `outbox` | `shared/outbox` | outil commun | les, neuf |
| `service` | `shared/service` | outil commun | les, neuf |
| `adapter/go2rtc/go2rtctest` | `video/adapter/go2rtc/go2rtctest` | outil de test | — (tests seulement) |
| `adapter/go2rtc` | `video/adapter/go2rtc` | adaptateur | video |
| `videoapp` | `video/app/videoapp` | application | video |
| `videoconfig` | `video/config/videoconfig` | réglages | video |
| `video/wire` | `video/core/video/wire` | traduction | video |
| `video` | `video/core/video` | cœur | video |
| `zigbeeapp` | `zigbee/app/zigbeeapp` | application | zigbee |
| `zigbeeconfig` | `zigbee/config/zigbeeconfig` | réglages | zigbee |
| `zigbee/wire` | `zigbee/core/zigbee/wire` | traduction | zigbee |
| `zigbee/z2m` | `zigbee/core/zigbee/z2m` | traduction | zigbee |
| `zigbee` | `zigbee/core/zigbee` | cœur | zigbee |

Soit 69 paquets : hub 11, zigbee 5, gsm 6, agent 6, video 6, power 5, maint 5, console 4,
install 1, shared 18, checks 2. Les 437 fichiers suivis sous `cmd` et `internal` (434 Go, 3 gabarits HTML) restent ; ceux
d'`internal` changent de dossier (`git mv`), ceux de `cmd`, `deploy` et `vkit` ne changent que
d'imports.

## Les cas qui ne se rangent pas simplement

1. **La console et l'installateur travaillent sur le site du hub.** `cmd/safehome-console` ouvre
   les codes du hub (`hub.OpenSiteCodes`, `access`, `hubconfig.CodesLock`) et lit `hub.json` ;
   `cmd/safehome-install` lit `hub.json` ; `consoleapp` est assise sur `install` ; `hubconfig`
   lit et écrit le site au format `install.Site`.
   - **Décidé seule, à relire (11/11/2026). (recommandé) Ranger tel quel, exceptions nommées dans `archtest`** : console → `hub/app/hub`,
     `hub/core/access`, `hub/config/hubconfig`, `install/app/install` ; install →
     `hub/config/hubconfig` ; `hub/config/hubconfig` → `install/app/install`. Vue au premier
     passage de la règle (12/11/2026), une cinquième, décidée seule de même : install →
     `hub/core/engine` (l'installateur vérifie types de zone et capteurs par la règle du moteur). Chaque exception a sa
     raison écrite, toute autre flèche entre programmes rougit. Renommage pur, rien ne change.
   - Sinon : sortir `install.Site` et les codes du site dans `shared/site`. Plus propre à terme,
     mais c'est un changement de code, pas un renommage : un lot à part, après celui-ci.
2. **Décidé seule, à relire (11/11/2026).** **`access`** sert le hub et la console. La table le met sous `hub/core/access` (son
   propriétaire est le hub, la console y passe par l'exception 1) plutôt que sous `shared`, comme
   l'esquisse le proposait : le commun garde seulement ce que plusieurs programmes possèdent.
3. **`configapp`** (la configuration reçue du cloud) n'est embarqué que par le hub : seconde
   application du hub, `hub/app/configapp`.
4. **Décidé seule, à relire (11/11/2026).** **Un test qui croise deux programmes** : `power/wire` vérifie en test que le hub (`health`,
   `health/wire`) lit bien ce que l'alimentation publie. (recommandé) Le laisser : `archtest` ne
   juge que le code de production, comme aujourd'hui ; ce test est un contrat entre deux
   programmes.
5. **Des chemins relatifs dépendent de la profondeur** : 17 fichiers de test lisent `config/`, `schemas/` ou
   `deploy/` par `../..`, et `archtest`, `qualitytest`, `schematest` retrouvent la racine par
   `runtime.Caller`. Chacun prend un ou deux `..` de plus, corrigé dans le lot qui le déplace.
6. **`photodir`, `sysclock`, `filelock`** ne servent que deux programmes chacun (agent et vidéo,
   hub et vidéo, hub et console) : `shared/adapter`, par la règle 3.

## Les règles d'`archtest` à réécrire

| Règle | Aujourd'hui | Après |
|---|---|---|
| `TestDomainCoresArePure` | liste de 9 dossiers | même liste, nouveaux chemins |
| `TestDriverAndAdaptersKnowNoDomain` | lit `internal/adapter/*` | lit `internal/*/adapter/*` |
| `TestOnlyTheCompositionRootImportsAdapters`, `allowedExec`, `TestTheVirtualKitStaysOutside` | préfixe `internal/adapter/` | tout chemin `internal/<x>/adapter/` |
| `TestApplicationLayersUseOnlyTheClockPort` | `hub`, `gateway` et les `*app` | tout `internal/*/app/*` |
| `TestConfigFileServesOnlyTheConfigurationPackages` | `internal/*config` | tout `internal/*/config/*` |
| `thirdPartyAllowed`, `TestOnlyTestsImportTestSupport`, `openBrokers` | chemins `adapter/mqtt`, `schematest`… | nouveaux chemins |
| les règles par paquet (`outbox`, `inbox`, `agent`, `videoapp`, `maintapp`, `powerapp`, `configapp`, `install`, `consoleapp`, `service`, `wire`, `z2m`, `NoDomainImportsTheHub`) | `internal("engine")`… | mêmes listes, nouveaux chemins |
| **nouvelle** `TestEveryPackageHasARole` | — | tout paquet est sous `<programme>/{app,core,adapter,config}/…`, `shared/…` ou `checks/…` : rien à plat |
| **nouvelle** `TestAProgramImportsNoOtherProgram` | — | `cmd/safehome-X` et `internal/X` n'importent que `internal/X`, `internal/shared` et les exceptions du cas 1 |
| **nouvelle** `TestACoreImportsNoAdapterNorApplication` | — | un paquet sous `*/core/` n'importe ni adaptateur (le sien, un partagé, `shared/service`) ni application (les mots de Kevin) |

Chaque nouvelle règle a son témoin sur un cas positif (un import interdit écrit exprès, vu rouge).
`qualitytest` suit aussi (préfixe `internal/adapter/` dans `consistency_test.go`), et
`scripts/check.sh` (`rule_packages`).

## L'ordre des lots

Le commun d'abord (`shared`, puis `checks`) : tout le monde l'importe, le déplacer en premier
fait d'un coup les imports partagés, puis chaque programme ne touche plus que les siens. Ensuite
un programme par commit, des feuilles vers le hub : power, maint, video, agent, gsm, zigbee,
install, console, hub. Chaque commit passe le crochet (`scripts/check.sh --quick`) ; le
`scripts/check.sh` complet une fois à la fin, puis la note datée dans `architecture.md`
(« Décisions ouvertes ») et les docs qui citent les anciens chemins (`README.md`, `docs/*.md`).

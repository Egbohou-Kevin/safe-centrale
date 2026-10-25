# Glossaire

Les documents sont en français, le code et ses commentaires en anglais. Ce glossaire
relie les deux : c'est le langage commun du projet.

## Alarme

| Français | Code (anglais) | Sens |
|---|---|---|
| moteur d'alarme | `engine` | Le cœur de la centrale, qui décide |
| état / mode | `Mode` | `disarmed`, `arming`, `armed`, `entry_delay`, `triggered` |
| désarmé | `ModeDisarmed` | Seules les zones permanentes surveillent |
| armement en cours, délai de sortie | `ModeArming`, `ExitDelay` | Le temps de sortir après avoir armé |
| armé | `ModeArmed` | Le système surveille |
| délai d'entrée | `ModeEntryDelay`, `EntryDelay` | Le temps de désarmer après avoir ouvert la porte |
| déclenché | `ModeTriggered` | L'alarme sonne |
| zone | `Zone`, `ZoneKind` | Le rôle d'un appareil : `entry`, `instant`, `interior`, `24h_fire`, `24h_panic`, `24h_tamper` |
| armement automatique | `Schedule`, `Closing` | Armement à l'heure de fermeture, sans action du client |
| préavis | `ArmingScheduled`, `AutoArmNotice` | Annonce de l'armement automatique, 10 minutes avant |
| report | `Postponed`, `CmdPostpone` | Repousser l'armement automatique de 30 minutes |
| heure limite | `AutoArmLimit` | Au-delà, on ne peut plus repousser |
| supervision | `health` (`Monitor`) | Surveille la centrale et les appareils, hors alarme |
| résumé quotidien | `SummaryAt` | Une alerte par jour qui liste appareils muets et piles faibles |
| seuil de batterie | `HubBatteryWarn`, `HubBatteryCritical` | 40 % puis 10 % |
| réarmement d'un seuil | `HubBatteryRearm` | Au-dessus de 60 %, le seuil peut alerter de nouveau |
| pilote | `driver` (`Loop`) | Fait tourner un domaine en temps réel |
| tâche | `task` | Un travail exécuté à son tour par la boucle |
| magasin d'état | `Store` (`filestore`) | Sauvegarde l'état sur disque, de façon atomique |
| code d'accès | code (`access.Registry`) | Un par personne, haché, vérifié sur le site |
| code faible | `ErrWeakCode` | Chiffre répété ou suite (0000, 1234) : refusé |
| coût du hachage | `Iterations` | Nombre de tours de PBKDF2, à régler sur la centrale |
| contourner une zone | bypass | Ignorer une zone jusqu'au désarmement |
| armement forcé | `Force` | Armer en contournant les zones ouvertes |
| armer tout de suite | `arm_now` | Terminer le délai de sortie |
| couper la sirène | `silence` | Arrêter la sirène sans désarmer |
| cycle de sirène | `SirenCycle` | Une période de sirène, suivie d'une pause |
| incident | `Incident` | Un déclenchement, de la première sirène à son arrêt |
| cause | `Cause` | L'appareil et la zone qui ont déclenché |
| verrouillage | lockout (`LockedUntil`) | Désarmement refusé après trop de codes faux |
| escalade | `escalation` | Décide qui est prévenu, par quel canal, quand |
| vague | `Wave` | Une étape du plan : à tel délai, prévenir telles personnes |
| relance | `Repeat` | Prévenir de nouveau tout le monde, toutes les 10 minutes |
| plan | `Plan` | Les vagues d'un type d'alerte, une donnée de configuration |
| acquittement | `Ack` | « Je m'en occupe » : arrête l'escalade de l'incident |
| contact | `Contact` | Une personne à prévenir : rôle et canaux acceptés |
| passerelle GSM | `gsm` (`Gateway`), `gateway` | Envoie les SMS et les appels, lit les réponses |
| ligne | `Line` | Un modem et sa SIM |
| accusé de livraison | `DeliveryReport` | L'opérateur confirme que le SMS est arrivé |
| SMS de test | `TestNumber`, `TestEvery` | SMS hebdomadaire vers un numéro de contrôle |
| protection dégradée | `gsm_degraded` | Le secours GSM ne marche plus : le propriétaire est prévenu |
| capteurs injoignables | `zigbee_down` | Le réseau Zigbee ne marche plus : l'alarme est aveugle |
| sirène muette | `siren_failed` | Une sirène refuse de sonner pendant une alarme |
| sirène connue muette | `silentSirens`, `Raise.Silent` | Signalée muette pendant l'alarme qui sonne, et pas entendue sonner depuis : un feu levé alors le dit |
| nom d'une sirène | `siren_names`, `SirenNames`, `SirenName` | Son nom dans les textes des alertes, réglé à l'installation ; par défaut « Sirène », ou « Sirène 2 » parmi plusieurs |
| alarme prise en charge | `Handled` | Un contact l'a acquittée, ou une personne l'a arrêtée avec un code ; retenu 24 heures |
| alerte discrète | `Quiet`, `alarm_handled` | L'alerte d'une alarme déjà prise en charge : dite sans faire sonner, jamais d'appel |
| profil d'appareil | `Profile` | Comment lire et commander un modèle d'appareil (fichier JSON) |
| appairage | `pairing`, `registry.pair` | Ouvrir le réseau Zigbee à un nouvel appareil |
| registre | `registry` | Liste des appareils connus de l'adaptateur |
| pont (Zigbee2MQTT) | `bridge` | Le logiciel qui relie la clé Zigbee au broker |
| session vidéo | `video session` | Un direct d'une caméra, borné dans le temps |
| relais vidéo | `relay` | Serveur du cloud qui reçoit le flux du site et le distribue aux applications |
| jeton (vidéo) | `token` | Autorisation valable pour une seule session, jamais journalisée |
| plafond de données | `quota` | Données vidéo autorisées par mois pour un site |
| horloge synchronisée | `clock_ok` | L'heure de la centrale a été vérifiée par internet (NTP) ; dit dans le heartbeat |
| file d'envoi (partagée) | `outbox` | Messages MQTT gardés quand le broker est absent, renvoyés dans l'ordre ; partagée par toutes les couches application |
| porte d'entrée (partagée) | `inbox` | Message reçu, abonnements, vérification d'un message contre le contrat, identifiants déjà vus ; partagée par toutes les couches application |
| application (couche) | `hub`, `gateway`, `zigbeeapp`, `agentapp`, `videoapp`, `maintapp`, `configapp`, `consoleapp`, `install` | Relie MQTT aux domaines et à leurs boucles, par des ports qu'elle déclare |
| racine de composition | les programmes de `cmd/` (avec `internal/service`) | Assemblent les vrais adaptateurs, seuls endroits à les connaître |
| fenêtre de démarrage | `BootWindow` | Écoute des états retenus au lancement, après la connexion au broker |
| fichier de pid | `hub.pid` | Empêche deux centrales sur le même état, indique qui prévenir pour les codes |
| signal | `Signal` | Un capteur devient actif ou inactif |
| capteur | `SensorKind` | Ce qu'un appareil détecte |

## Architecture

| Français | Code | Sens |
|---|---|---|
| contrat | `contract` | Le langage commun des messages (format publié) |
| enveloppe | `Envelope` | Les champs communs à tous les messages |
| événement du domaine | `engine.Event` | Un fait survenu dans l'alarme, indépendant du format des messages |
| traduction du contrat | `engine/wire` | Fait le lien entre le domaine et le contrat |
| action | `engine.Action` | Ce que le moteur demande au monde extérieur |
| instantané | `Snapshot` | L'état complet du moteur, écrit sur disque |
| pilote | driver | La boucle qui fait tourner un domaine en temps réel |
| centrale | hub | Le boîtier installé chez le client |
| agent de liaison | `agent`, `agentapp`, `Relay` | Le programme qui relie la centrale au cloud |
| liaison montante | uplink | La connexion de l'agent au cloud (`up` ou `down`) |
| dernier message | Last Will | Ce que le broker du cloud publie si l'agent disparaît sans prévenir |
| session vidéo | `video.Request`, `session` | Un direct d'une caméra vers le relais du cloud |
| relais vidéo | relay, `push_url` | Le serveur du cloud qui reçoit le flux du site et le distribue aux applications |
| plafond du mois | `Quota`, `quota_mb` | Données mobiles que la vidéo peut consommer par mois, réglé par le client |
| liaison du site | `Mobile`, `uplink` (`fixed`, `mobile`) | Box du commerce ou routeur 4G payé par le client |
| site | `site` | Le lieu surveillé, identifié par `site_id` |
| configuration du cloud | `config.set`, `siteconfig`, `site-config.json` | Ce que le client règle dans l'application (contacts, horaires, délais, forfait vidéo), numéroté par version et posé sur les fichiers du site |
| chien de garde (systemd) | watchdog, `WatchdogSec`, `sdnotify`, `Alive` | systemd redémarre un programme qui ne dit plus qu'il fonctionne ; le programme ne le dit que si ses boucles répondent |
| photo d'alarme | `photo.taken`, `TakePhotos`, `photodir` | Image réduite d'une caméra prise au déclenchement, gardée sur le site jusqu'à son envoi par l'agent |
| mode économe | `EconomyPhotos`, `Economy`, `uplink: mobile` | En données mobiles payées par le client : une seule photo par caméra, plus petite ; heartbeat et état de l'agent au cloud toutes les 5 minutes |
| mode installation | `install.mode`, `installMode` | 30 minutes ouvertes au clavier avec le code installateur, alarme désarmée : la console locale est disponible |
| code installateur | `InstallerUser`, `installer` | Code à part de ceux des personnes ; ouvre le mode installation, ne désarme jamais |
| console locale | `safehome-console`, `consoleapp` | Page web de la centrale pour l'installateur, sur le Wi-Fi propre à la centrale, pendant le mode installation |
| zones réglées sur place | `site-devices.json`, `DevicesFile` | Zones et sirènes écrites par la console ou l'outil, dans le dossier d'état ; remplacent celles de `hub.json` |
| maintenance à distance | `maint`, `maint.open`, `safehome-maint` | Tunnel SSH sortant de la centrale vers notre bastion, sur demande, une heure au plus |
| bastion | `bastion` | Notre serveur où arrivent les tunnels ; les administrateurs passent par lui |
| moment calme | `restartIfQuiet` | Alarme désarmée ou armée, mode installation fermé : la centrale peut redémarrer pour appliquer une configuration ou de nouvelles zones |

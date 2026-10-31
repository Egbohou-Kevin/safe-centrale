<img src="marque/seq-icone.svg" width="56" alt="SeQ">

# Kit virtuel

Statut : en place depuis le 21 octobre 2026. Une centrale complète tourne dans une
machine virtuelle sur le Mac, installée par le vrai `deploy/install.sh`, avec de faux
équipements à la place du matériel. Un scénario rejoue la vie d'un site et vérifie
chaque étape.

Il ne remplace pas le kit d'essai : il attrape avant lui les erreurs de déploiement,
d'intégration entre programmes et de comportement face aux pannes.

## Ce qui est simulé, et comment

| Vrai matériel | Dans le kit virtuel |
|---|---|
| Raspberry Pi 4 (1 Go), Raspberry Pi OS Lite 64 bits | VM Debian 13 ARM 64 bits (même base), 1 Go, avec Lima |
| Carte SD : partition de données | Second disque de la VM, monté comme à l'étape 2 de `systeme.md` |
| Chien de garde matériel | `softdog` |
| Modem GSM USB | `vkit-fakemodem` : le simulateur AT des tests derrière un pseudo-terminal `/dev/ttyVMODEM` (groupe `dialout`, comme un vrai modem) |
| Clé Zigbee et Zigbee2MQTT | `vkit-fakez2m` : parle sur le broker comme Zigbee2MQTT, avec son nom et son mot de passe (`LoadCredential=`, 29 octobre 2026) |
| Cloud | Un second Mosquitto en TLS sur `127.0.0.1:8883`, avec sa propre autorité |
| Caméra du commerce | `vkit-camera` : un go2rtc qui diffuse une mire de test en RTSP (H.264) sur `127.0.0.1:8554` |
| Relais vidéo du cloud | `vkit-relay` (ffmpeg qui reçoit le flux RTMP) derrière `vkit-relay-tls` (rtmps sur `127.0.0.1:1936`, certificat du faux cloud, reconnu par le système comme le serait le vrai) |
| Stockage des photos du cloud | `vkit-fakeupload` : reçoit les `PUT` HTTPS de l'agent (certificat du faux cloud), vérifie qu'il s'agit d'un JPEG ; un jeton qui ne commence pas par `vkit-ok` est refusé (401) |
| Bastion de maintenance | Le serveur SSH de la VM : un compte `tunnel` sans shell, qui ne peut qu'écouter (`AllowTcpForwarding remote` sous `Match User tunnel`), la clé de la centrale limitée à son port (`permitlisten`), comme dans `maintenance.md` |
| Module batterie (Geekworm X728) | `vkit-power` : les simulateurs du noyau `gpio-sim` (ses broches, sur une puce nommée comme celle du Pi 4) et `i2c-stub` (sa jauge à `0x36`), compilés par `up.sh` ; `safehome-power` les lit par les mêmes interfaces que sur le Pi |
| Wi-Fi de la console, téléphone de l'installateur | Deux radios simulées par le noyau (`mac80211_hwsim`) : `wlan0` pour la centrale, `wlan1` pour le téléphone |

Nos programmes tournent **sans aucune modification** : seule la configuration du site
(`vkit/site/`) diffère, avec des délais courts (sortie 15 s, entrée 10 s) et le port
du faux modem.

## Ce que le kit virtuel ne remplace pas

- La radio Zigbee réelle : portée, appairage, noms exacts des champs des profils.
- Le vrai modem et les opérateurs togolais : codes d'erreur, accusés de livraison.
- La vraie coupure de courant, l'usure de la carte SD, la racine en lecture seule de
  Raspberry Pi OS (`overlayroot`), l'horloge sans pile.
- La vitesse du Pi (le hachage des codes d'accès se règle sur le vrai Pi).

## S'en servir

Sur un Mac Apple silicon, avec Lima (`brew install lima`). `up.sh` télécharge les sources
de deux simulateurs du noyau (version et empreintes épinglées). `deploy.sh` télécharge
une fois go2rtc pour la centrale (version et empreinte épinglées, gardé dans
`~/.cache/safehome-vkit`) :

```bash
vkit/up.sh       # crée et prépare la VM (une fois, quelques minutes ; à relancer si prepare-pi.sh change)
vkit/deploy.sh   # construit le paquet de l'arbre courant et l'installe, comme une mise à jour
vkit/test.sh     # rejoue tout le scénario depuis un site vierge (une dizaine de minutes)
vkit/test.sh arm intrusion   # ou seulement certaines étapes
```

Pour agir à la main sur les faux équipements, dans la VM (`limactl shell safehome-pi`) :

```bash
echo 'sms +22870000003 1' | sudo tee /run/vkit/modem        # un SMS arrive
echo 'unplug' | sudo tee /run/vkit/modem                    # le modem est débranché
sudo vkit-power mains lost                                  # coupure du secteur
sudo vkit-power battery 3650 38                             # l'accu à 3,65 V, 38 %
echo 'network off' | sudo tee /run/vkit/modem               # plus de réseau GSM
echo 'send 0x00158d0000000001 {"contact":false}' | sudo tee /run/vkit/zigbee  # la porte s'ouvre
echo 'answers off' | sudo tee /run/vkit/zigbee              # Zigbee2MQTT ne répond plus aux demandes
sudo systemctl stop vkit-cloud                              # coupure d'internet (le cloud tombe)
journalctl -u vkit-modem -f                                 # les SMS et appels partis
```

Les ordres reconnus sont décrits en tête de `vkit/fakemodem/control.go`,
`vkit/fakez2m/devices.go` et `vkit/power.sh`.

## Le scénario

`vkit/scenario/steps.sh`, une fonction par étape :

1. **Site vierge**, puis **installation** : codes, appairage de cinq appareils (et
   refus d'un modèle inconnu), zones, sirène, `check` ; le clavier réglé en QoS 0 dans
   Zigbee2MQTT ; un appairage que Zigbee2MQTT ne confirme pas finit en erreur.
2. **Armement** depuis l'application, par le cloud et l'agent.
3. **Intrusion** : délai d'entrée, déclenchement, sirène, alerte au cloud, SMS et appel
   au gardien ; il répond « 1 » ; le propriétaire désarme. Le code n'apparaît dans
   aucun journal ni sur le disque.
4. **Trois codes faux** : alerte, verrouillage.
5. **Fumée** sans armement : alarme incendie, SMS immédiat.
6. **Coupure d'internet** pendant l'armement : le cloud reçoit tout au retour. Le
   scénario coupe l'agent seul au pare-feu ; arrêter le broker du cloud ferait aussi
   perdre son abonnement à l'écoute du scénario, juste quand l'agent renvoie sa file.
7. **Modem débranché** puis rebranché, **perte du réseau GSM**.
8. **Plantage** de la centrale armée (elle reste armée), **gel** (le chien de garde de
   systemd la redémarre).
9. **Mode installation** au clavier, réseau Wi-Fi de la console, un « téléphone » s'y
   connecte et ouvre la page, sans pouvoir aller plus loin (la passerelle de la
   centrale ne lui répond pas).
10. **Maintenance à distance** demandée par le cloud : le tunnel s'ouvre, le port du
    bastion mène au SSH de la centrale ; la clé de la centrale n'ouvre ni shell sur
    le bastion, ni l'écoute sur le port d'un autre site, ni une connexion depuis le
    bastion vers le port d'un site (`-W`) ; le tunnel se referme.
11. **Vidéo en direct** demandée depuis l'application : la caméra est joignable, le
    flux arrive au relais en H.264 sans réencodage, s'arrête sur demande, et son jeton
    n'apparaît dans aucun journal.
12. **Photos à l'alarme** : la centrale prend une photo au déclenchement, le cloud
    l'apprend ; une adresse refusée est signalée (`url_expired`), puis la photo
    arrive au stockage en JPEG et l'agent confirme l'envoi ; le jeton n'apparaît
    dans aucun journal.
13. **Coupure de courant** : la coupure seule n'alerte personne ; la batterie qui
    baisse alerte à 40 %, puis en urgence à 10 % ; presque vide, la centrale tient le
    bouton du module pour s'éteindre proprement ; le retour du secteur est annoncé, et
    la centrale le montre encore après son redémarrage.

## Ce qu'il a déjà trouvé

Quatre défauts que les tests unitaires ne voyaient pas, corrigés avec un test chacun :

- **La sirène du catalogue n'était pas une sirène** : le profil Heiman ne rapporte
  rien quand elle sonne, donc le registre ne lui donnait pas la capacité `siren`, et
  l'installateur ne pouvait la déclarer comme sirène. Une commande `siren.on`/`off`
  fait désormais d'un appareil une sirène, et les capacités sont recalculées au
  démarrage pour les appareils déjà appairés.
- **Une première ouverture était ignorée** : la première valeur d'un capteur était
  enregistrée sans événement. Une porte appairée fermée mais muette jusqu'à
  l'intrusion n'aurait pas déclenché l'alarme.
- **Un modem débranché restait perdu** pendant une alarme : la passerelle gardait le
  port mort, et ne le redémarrait que sur une ligne au repos. Le port est maintenant
  rouvert à la demande suivante, et de nouveau à chaque demande tant qu'il ne s'ouvre
  pas : un modem USB met quelques secondes à revenir, et un seul essai, fait trop tôt,
  laissait le port fermé jusqu'au redémarrage du modem (corrigé le 25 octobre 2026
  après une revue ; le kit rebranche le modem tout de suite, il ne le voyait pas).
- **Le mode installation se refermait tout seul** quand l'installateur tapait son code
  juste après un redémarrage de la centrale (coupure de courant, chien de garde) : la
  fin du démarrage annonçait « fermé, redémarrage » par-dessus, et la console coupait
  son Wi-Fi alors que la centrale tenait le mode ouvert. Trouvé seulement en
  enchaînant les étapes du scénario.

Et deux détails : `pair` proposait `zone` comme étape suivante pour une sirène, et
la centrale redémarrée ne montrait plus l'état du secteur tant qu'il ne changeait pas.

Il a aussi conduit à l'alerte `siren_failed` : une sirène qui refuse de sonner
pendant une alarme n'était qu'une ligne de journal.

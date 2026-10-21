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
| Clé Zigbee et Zigbee2MQTT | `vkit-fakez2m` : parle sur le broker comme Zigbee2MQTT |
| Cloud | Un second Mosquitto en TLS sur `127.0.0.1:8883`, avec sa propre autorité |
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

Sur un Mac Apple silicon, avec Lima (`brew install lima`) :

```bash
vkit/up.sh       # crée et prépare la VM (une fois, quelques minutes)
vkit/deploy.sh   # construit le paquet de l'arbre courant et l'installe, comme une mise à jour
vkit/test.sh     # rejoue tout le scénario depuis un site vierge (une dizaine de minutes)
vkit/test.sh arm intrusion   # ou seulement certaines étapes
```

Pour agir à la main sur les faux équipements, dans la VM (`limactl shell safehome-pi`) :

```bash
echo 'sms +22870000003 1' | sudo tee /run/vkit/modem        # un SMS arrive
echo 'unplug' | sudo tee /run/vkit/modem                    # le modem est débranché
echo 'network off' | sudo tee /run/vkit/modem               # plus de réseau GSM
echo 'send 0x00158d0000000001 {"contact":false}' | sudo tee /run/vkit/zigbee  # la porte s'ouvre
sudo systemctl stop vkit-cloud                              # coupure d'internet
journalctl -u vkit-modem -f                                 # les SMS et appels partis
```

Les ordres reconnus sont décrits en tête de `vkit/fakemodem/control.go` et
`vkit/fakez2m/devices.go`.

## Le scénario

`vkit/scenario/steps.sh`, une fonction par étape :

1. **Site vierge**, puis **installation** : codes, appairage de cinq appareils (et
   refus d'un modèle inconnu), zones, sirène, `check`.
2. **Armement** depuis l'application, par le cloud et l'agent.
3. **Intrusion** : délai d'entrée, déclenchement, sirène, alerte au cloud, SMS et appel
   au gardien ; il répond « 1 » ; le propriétaire désarme. Le code n'apparaît dans
   aucun journal ni sur le disque.
4. **Trois codes faux** : alerte, verrouillage.
5. **Fumée** sans armement : alarme incendie, SMS immédiat.
6. **Coupure d'internet** pendant l'armement : le cloud reçoit tout au retour.
7. **Modem débranché** puis rebranché, **perte du réseau GSM**.
8. **Plantage** de la centrale armée (elle reste armée), **gel** (le chien de garde de
   systemd la redémarre).
9. **Mode installation** au clavier, réseau Wi-Fi de la console, un « téléphone » s'y
   connecte et ouvre la page.

## Ce qu'il a déjà trouvé

Trois défauts que les tests unitaires ne voyaient pas, corrigés avec un test chacun :

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
  rouvert à la demande suivante.

Et un détail : `pair` proposait `zone` comme étape suivante pour une sirène.

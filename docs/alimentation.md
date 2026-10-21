# Alimentation de la centrale

Statut : choix du module fait le 21 octobre 2026 (Geekworm X728). Le programme est
écrit et tourne dans le kit virtuel. Il n'a **pas encore tourné sur la vraie carte** :
les points marqués « à valider » se vérifient sur le kit d'essai.

## Pourquoi

Les coupures de courant sont fréquentes à Lomé. La centrale doit :

- continuer à tourner sur batterie, l'alarme comprise ;
- savoir qu'elle est passée sur batterie, et combien de charge il lui reste, pour
  prévenir le propriétaire avant de s'éteindre (règles dans `escalation.md`) ;
- s'éteindre proprement quand la batterie est presque vide, plutôt que de tomber
  d'un coup, puis repartir seule au retour du courant.

## Le module : Geekworm X728

Une carte qui se pose sur le connecteur du Pi, avec des accus 18650, qui alimente le
Pi et le recharge sur secteur. Ce que la centrale en utilise :

| Fonction | Broche du Pi (numérotation BCM) | Sens |
|---|---|---|
| Coupure du secteur | GPIO6 | entrée : haute quand le module tourne sur batterie |
| Jauge de batterie | bus I2C 1, adresse `0x36` | tension de l'accu, charge restante |
| Bouton d'alimentation | GPIO26 (GPIO13 sur les cartes avant la 2.1) | sortie : tenue haute 3 s, le module éteint le Pi et coupe le courant |
| Demande d'arrêt | GPIO5 | du module vers le Pi : impulsion haute (bouton de la carte, ou demande ci-dessus) |
| Pi en marche | GPIO12 | du Pi vers le module : haute tant que le Pi tourne |

Ces broches viennent des scripts officiels du fabricant (dépôt `geekworm-com/x728-script`).
Le wiki du fabricant, qui décrit les cavaliers et l'horloge éventuelle de la carte,
n'a pas pu être lu : **à valider sur la carte reçue**, en particulier sa révision
(inscrite sur la carte) pour la broche du bouton.

## Ce que fait le programme

`safehome-power` (`cmd/safehome-power`, configuration `power.json`) lit le module
chaque seconde et publie, au nom de l'appareil `hub_device_id` de la centrale :

- `mains` : `present` ou `lost`. Un changement n'est retenu qu'après **3 secondes**
  stables : un vacillement du secteur n'est pas une coupure ;
- `battery` : la charge en pour cent, annoncée quand elle a bougé de **2 points**
  (et toujours à 0 et 100 %).

Comme pour un capteur, chaque valeur part en événement et en état retenu. La
supervision de la centrale (`internal/health`) en tire les alertes :
`battery_low_hub` pendant une coupure (40 %, puis 10 %), `mains_restored` au retour
si une alerte de batterie est partie. Une coupure seule n'est pas une alerte.

**Batterie presque vide** : sur batterie, si l'accu reste sous **3,0 V** pendant
**30 secondes** (le seuil du script du fabricant), le programme tient le bouton
d'alimentation 3 secondes. Le module demande alors l'arrêt au Pi, qui s'éteint
proprement, puis il coupe le courant. Au retour du secteur, le module rallume le Pi
(réglage « AUTO ON » de la carte, **à valider**), et la centrale repart dans l'état
où elle était (armée reste armée).

Un module qui ne répond plus est écrit une fois dans le journal, puis une fois
quand il répond à nouveau. Un module absent au démarrage arrête le programme, que
systemd relance toutes les 10 secondes.

## Réglages du système

Dans `/boot/firmware/config.txt`, puis redémarrer :

```
# Le bus I2C (jauge du module, et horloge s'il y en a une)
dtparam=i2c_arm=on
# Le module demande l'arrêt par une impulsion haute sur GPIO5 : le noyau éteint le Pi
dtoverlay=gpio-shutdown,gpio_pin=5,active_low=0,gpio_pull=down
# Le Pi dit au module qu'il tourne : GPIO12 haute pendant la marche, basse une fois éteint
dtoverlay=gpio-poweroff,gpiopin=12,active_low=1
```

Les deux dernières lignes remplacent le script `x728-pwr` du fabricant : rien à
installer, le noyau s'en charge. **À valider** sur la carte : qu'un appui sur son
bouton éteint le Pi proprement, et qu'elle coupe le courant une fois le Pi éteint.
Si ce n'est pas le cas, revenir au script du fabricant pour ces deux broches.

Le programme tourne sous l'utilisateur `safehome`, avec les groupes `gpio` et `i2c`
de Raspberry Pi OS (unité `deploy/systemd/safehome-power.service`).

## Configuration

`config/power.example.json`. `device_id` doit être le `hub_device_id` de `hub.json` :
`install.sh` refuse sinon, car la centrale ne verrait jamais les coupures.

| Clé | Défaut | Rôle |
|---|---|---|
| `device_id` | (obligatoire) | l'appareil de la centrale |
| `gpio_chip` | `pinctrl-bcm2711` | le nom de la puce GPIO du Pi 4 |
| `mains_line` | 6 | la broche de la coupure du secteur |
| `cut_line` | 26 | la broche du bouton (13 sur les cartes avant la 2.1) |
| `i2c_bus` | `/dev/i2c-1` | le bus de la jauge |
| `cut_millivolts` | 3000 | le seuil d'arrêt, de 2800 à 3600 |

`safehome-power check -config /etc/safehome/power.json` vérifie la configuration et lit
le module une fois. Un module injoignable est signalé sans faire échouer
l'installation, qui peut précéder le redémarrage qui allume le bus I2C.

## Dans le kit virtuel

La VM n'a ni GPIO ni I2C : `vkit/up.sh` y compile les simulateurs du noyau
(`gpio-sim` et `i2c-stub`), qui offrent au programme les mêmes interfaces que le Pi.
Le scénario coupe le secteur, vide la batterie et vérifie les alertes et la demande
d'arrêt (`kit-virtuel.md`).

## À valider sur le kit d'essai

- La révision de la carte et la broche du bouton (26 ou 13).
- L'arrêt propre et la coupure du courant par les réglages ci-dessus.
- Le rallumage au retour du secteur (cavalier « AUTO ON »).
- La tension d'alimentation d'entrée de la carte et le bloc secteur à acheter.
- La présence d'une horloge sur la carte : elle dispenserait du module DS3231
  (`systeme.md`, étape 4).
- L'autonomie réelle avec deux accus, le Pi, le modem et la clé Zigbee branchés.

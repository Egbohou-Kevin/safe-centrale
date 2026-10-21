# Maintenance à distance

Statut : codé côté centrale (`internal/maint`, `internal/maintapp`,
`internal/adapter/sshtunnel`, `cmd/safehome-maint`), testé avec un faux `ssh` et
contre le vrai client OpenSSH (lecture de nos options). Le bastion et l'écran du cloud
ne sont pas faits. Décidé avec Kevin le 20 octobre 2026.

## But

Pouvoir intervenir sur une centrale (un modem qui ne répond plus, un journal à lire,
une mise à jour qui coince) **sans se déplacer et sans rien ouvrir sur la box du
client**. En 4G, l'opérateur bloque de toute façon les connexions entrantes.

## Principe

```
Admin ──ssh──► Bastion ◄──tunnel ssh sortant── Centrale (safehome-maint)
                 │                                   ▲
                 └── localhost:22001 ═══════════════►│ SSH de la centrale (port 22)

Cloud ──maint.open──► Agent ──► safehome-maint ──maint.session──► Agent ──► Cloud (propriétaire)
```

1. Un administrateur demande la maintenance d'un site dans le cloud, avec une raison.
2. Le cloud publie `maint.open` (session, administrateur, raison, durée d'une heure au
   plus). L'agent de liaison le vérifie et le passe à la centrale.
3. `safehome-maint` lance le client OpenSSH : la centrale se connecte **vers** notre
   bastion et y offre son propre serveur SSH, sur un port de la boucle locale du
   bastion réservé à ce site (`ssh -R localhost:22001:localhost:22`).
4. L'administrateur se connecte au bastion, puis à la centrale par ce port, avec
   **sa clé personnelle**.
5. Le tunnel se ferme de lui-même au bout de la durée demandée (une heure au plus), ou
   sur `maint.close`.
6. Chaque étape part au cloud (`maint.session`) : le **propriétaire la voit** dans
   l'application (« Maintenance en cours par kevin jusqu'à 15 h 30 : modem qui ne
   répond plus »). La centrale l'écrit aussi dans son journal.

## Sécurité

- **Rien n'écoute sur le réseau du client.** Le tunnel est une connexion sortante ; le
  port n'existe que sur la boucle locale du bastion.
- **Le cloud ne peut pas entrer.** Il peut seulement demander l'ouverture du tunnel.
  Se connecter à la centrale demande la clé d'un administrateur, que seul le fichier
  `authorized_keys` de la centrale connaît. Ces clés ne sont **jamais dans le cloud** :
  un cloud piraté ouvrirait au pire un tunnel vers notre bastion, sans pouvoir s'en
  servir.
- **Le bastion est épinglé** : sa clé d'hôte est dans
  `/etc/safehome/maint_known_hosts`, vérifiée strictement. Un faux bastion est refusé.
- **La clé de la centrale ne sert qu'au tunnel.** Sur le bastion, sa ligne
  `authorized_keys` interdit tout sauf écouter sur le port du site :

  ```text
  restrict,port-forwarding,permitlisten="localhost:22001" ssh-ed25519 AAAA... tg-lom-0001
  ```

  Une centrale volée ne permet donc ni d'ouvrir un shell sur le bastion, ni d'écouter
  sur le port d'un autre site. On retire sa ligne, et c'est fini.
- **Une session à la fois, une heure au plus**, refusée si l'ordre arrive après son
  `exp`. Le tunnel meurt avec le service (systemd et le noyau tuent `ssh`).
- **Coupure possible par l'installateur** : `"enabled": false` dans `maint.json` ; la
  centrale répond alors `failed` / `disabled`.
- Le client `ssh` ne lit aucune configuration de l'utilisateur ni du système
  (`-F /dev/null`), ne pose jamais de question (`BatchMode`), et s'arrête si le port
  est refusé (`ExitOnForwardFailure`).

## Messages

| Type | Topic | Sens | Contenu |
|---|---|---|---|
| `maint.open` | `maint/cmd` | cloud → centrale | `session`, `user`, `reason` (200 caractères au plus), `duration_s` (60 à 3600), `exp` |
| `maint.close` | `maint/cmd` | cloud → centrale | `session` |
| `maint.session` | `maint/session` | centrale → cloud | `session`, `user`, `status` (`opening`, `open`, `closed`, `failed`), `reason`, `until`, `duration_s` |

Après un redémarrage de la centrale, une session trouvée finie annonce une durée
(`duration_s`) bornée à sa fin prévue : jamais plus que la durée accordée.

Raisons : `stopped` (fermé par le cloud), `timeout` (durée écoulée), `busy` (une
session déjà ouverte), `expired`, `disabled`, `unreachable` (le bastion n'a pas laissé
entrer la centrale).

## Comportement

- Le tunnel est `open` quand le bastion a accordé le port (ligne `remote forward
  success` d'OpenSSH).
- S'il ne s'ouvre jamais : `failed` / `unreachable`, avec la cause dans le journal
  (refus de connexion, clé refusée, clé d'hôte inconnue…).
- S'il tombe une fois ouvert (internet coupé) : il est rouvert 10 s plus tard, tant
  que la session dure, sans nouveau message.
- Si le service redémarre pendant une session, elle reprend jusqu'à son heure de fin
  (elle est gardée dans le dossier d'état, fichier `maint`).

## Installation

Sur la centrale, une fois par site :

```bash
# La clé de la centrale (sans phrase de passe : le service s'en sert seul)
sudo ssh-keygen -t ed25519 -N "" -C tg-lom-0001 -f /etc/safehome/maint.key
# Comme cloud.secret : root:safehome, 0640. Le service la lit sans pouvoir la changer ;
# OpenSSH n'exige 0600 que d'une clé qui appartient à l'utilisateur lui-même.
sudo chown root:safehome /etc/safehome/maint.key
sudo chmod 640 /etc/safehome/maint.key
cat /etc/safehome/maint.key.pub      # à poser sur le bastion, avec permitlisten (voir plus haut)

# La clé d'hôte du bastion, relevée sur le bastion lui-même (pas par ssh-keyscan sur le réseau)
echo "bastion.safehome.example ssh-ed25519 AAAA..." | sudo tee /etc/safehome/maint_known_hosts

# Les clés des administrateurs, pour entrer par le tunnel
sudo install -d -m 700 -o admin -g admin /home/admin/.ssh
# y mettre leurs clés publiques dans /home/admin/.ssh/authorized_keys

safehome-maint check -config /etc/safehome/maint.json
sudo systemctl enable --now safehome-maint
```

`maint.json` (exemple : `config/maint.example.json`) : `bastion.host`, `bastion.port`,
`bastion.user` (le compte des tunnels sur le bastion), `bastion.remote_port` (**un port
par site**, à noter dans la fiche du site), `key_file`, `known_hosts`, `local_port`
(22), `max_length` (1 h au plus), `enabled`.

Sur le bastion (à faire) : un serveur Debian minimal, un compte `tunnel` sans shell
dont `authorized_keys` liste une ligne par site, `AllowTcpForwarding remote`,
`GatewayPorts no` (les ports restent sur la boucle locale), et les comptes des
administrateurs. Son adresse et sa clé d'hôte sont les mêmes pour toutes les
centrales.

## Se connecter

```bash
ssh -J kevin@bastion.safehome.example -p 22001 admin@localhost
```

## À faire

- Le bastion lui-même, et l'écran du cloud (demander, voir, fermer).
- Montrer la session au propriétaire dans l'application, et lui permettre de la
  fermer (le cloud enverra `maint.close`).
- Vérifier sur le kit que `permitlisten="localhost:22001"` accepte bien la demande
  `-R localhost:22001:localhost:22`.

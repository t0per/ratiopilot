# RatioPilot

Simulateur léger : simule un client BitTorrent qui seed, avec une interface web en temps réel.

- **Binaire Go unique**, aucune dépendance. ~5 Mo de RAM mesurés avec 6 torrents actifs. Image Docker ~11 Mo.
- **95 profils `.client` fournis** (qBittorrent jusqu'à 5.2.4, µTorrent, Transmission, Deluge, rTorrent…). Même format de requête, mêmes en-têtes, même génération de `peer_id` et de `key`.
- **Trackers HTTP, HTTPS et UDP** (BEP 15).
- **Interface web** : débit en direct, graphique sur 1 h, liste des torrents (pause, reprise, annonce forcée, suppression), glisser-déposer de `.torrent`, réglages, journal, thème clair/sombre, mobile.
- **Sécurité** : mot de passe, cookie signé, protection CSRF et anti-force brute. La passkey des trackers privés n'apparaît jamais dans les messages d'erreur.

## Avertissement

Annoncer un upload fictif est contraire au règlement de la plupart des trackers privés, qui détectent les débits incohérents et bannissent. Utilise l'environnement de test fourni pour essayer sans risque.

## Test rapide (environnement isolé)

```bash
docker compose -f docker-compose.test.yml up -d --build
```

| Service | Adresse |
|---|---|
| RatioPilot (mot de passe `test`) | http://ratiopilot.localhost:8088 |
| Tracker de test (annonces reçues, débit déduit) | http://tracker.localhost:8088 |
| Tableau de bord Traefik | http://localhost:8089 |
| RatioPilot depuis un autre poste | http://IP-du-serveur:8090 |
| Tracker de test depuis un autre poste | http://IP-du-serveur:8091 |

Le tracker factice dépose 6 torrents de test au premier lancement (dont 2 en UDP). Clique sur **Démarrer le seed**, puis compare l'interface avec la page du tracker.

Tout effacer : `docker compose -f docker-compose.test.yml down && sudo rm -rf data-test`

## Déploiement derrière ton Traefik

```bash
cp .env.example .env        # RP_PASSWORD, RP_DOMAIN, réseau/entrypoint/certresolver Traefik
mkdir -p data && sudo chown 1000:1000 data
docker compose up -d --build
```

Le compose suppose un Traefik existant sur le réseau Docker externe `proxy` (à adapter dans `.env`).

## Données (`./data`)

| Chemin | Rôle |
|---|---|
| `torrents/` | fichiers `.torrent` surveillés (ajout/suppression détectés en 5 s) |
| `clients/` | `.client` personnalisés (prioritaires sur ceux intégrés) |
| `config.json` | réglages (modifiables depuis l'interface) |
| `state.json` | cumul envoyé, pauses, torrents terminés, état marche/arrêt |

## Variables d'environnement

| Variable | Défaut | Rôle |
|---|---|---|
| `RP_PASSWORD` | généré | mot de passe de l'interface (sinon créé dans `data/admin-password.txt`) |
| `RP_AUTH=off` | — | désactive le login (si Traefik gère déjà l'authentification) |
| `RP_DATA` | `/data` | dossier de données |
| `RP_LISTEN` | `:8080` | adresse d'écoute |
| `HTTP_PROXY` / `HTTPS_PROXY` | — | proxy pour les annonces (ex : Gluetun) |

## Fonctionnement

1. Un débit global est tiré au hasard entre min et max, puis renouvelé toutes les N minutes (20 par défaut).
2. Il est réparti entre les torrents actifs selon leurs leechers : `poids = (L/(S+L))² × 100 × L`. Sans leecher, rien n'est envoyé.
3. Chaque torrent annonce `started`, puis à l'intervalle demandé par le tracker, puis `stopped` à l'arrêt.
4. En cas d'échec : tracker suivant, nouvel essai (30 s, 60 s, 2 min…), puis mise en erreur après N échecs.

## Développement

```bash
go test ./...
RP_DATA=./data RP_PASSWORD=dev go run .
go run ./cmd/testtracker          # tracker factice sur :6969
```

Structure : `internal/emu` (émulation des clients), `internal/tracker` (HTTP/UDP), `internal/engine` (planification, débit), `internal/web` (API + flux SSE), `web/` (interface Alpine.js + uPlot, sans build).

## Licences

Code RatioPilot : MIT. Fichiers `clients/*.client` : Apache-2.0, © Anthony Raymond et contributeurs (`clients/LICENSE-APACHE-2.0`). Alpine.js et uPlot : MIT. Police Barlow : SIL OFL.

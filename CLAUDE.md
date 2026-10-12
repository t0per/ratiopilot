# CLAUDE.md — RatioPilot

Contexte pour Claude Code. Lis ce fichier avant toute modification.

## Le projet

RatioPilot est un simulateur en Go : il simule un client BitTorrent qui seed (annonces aux trackers avec un upload fictif) et expose une interface web temps réel.

Le propriétaire n'est pas développeur : explique les changements simplement, en français, et privilégie les solutions clés en main sous Docker.

## Commandes

```bash
go build ./... && go vet ./... && go test ./...          # à lancer après chaque modification Go
RP_DATA=./data RP_PASSWORD=dev go run .                  # appli sur :8080
go run ./cmd/testtracker                                 # tracker factice sur :6969 (HTTP + UDP)
docker compose -f docker-compose.test.yml up -d --build  # environnement de test complet
```

## Architecture

| Dossier | Rôle |
|---|---|
| `main.go` | point d'entrée, variables d'environnement, embarque `web/` et `clients/` |
| `internal/bencode` | décodage/encodage bencode |
| `internal/torrent` | lecture des `.torrent` (info_hash, taille, trackers) |
| `internal/emu` | émulation client : lit les profils `.client`, génère peer_id/key, construit la query |
| `internal/tracker` | annonces HTTP(S) et UDP (BEP 15) |
| `internal/engine` | planification, rotation, répartition du débit, état persistant, journal |
| `internal/web` | API REST, flux SSE `/api/stream`, auth (cookie HMAC), fichiers statiques |
| `web/` | interface Alpine.js + uPlot, sans étape de build (librairies dans `web/vendor`) |
| `clients/` | 95 profils `.client` (Apache-2.0, voir `clients/LICENSE-APACHE-2.0`) |
| `cmd/testtracker` | tracker factice pour les tests, génère des `.torrent` de test |

## Règles

- **Go : bibliothèque standard uniquement.** Pas de dépendance externe sans demander.
- **Frontend : pas de bundler, pas de framework lourd.** Alpine.js + CSS maison. Couleurs via les variables CSS de `web/style.css` (thèmes clair et sombre).
- **Interface, logs et messages d'erreur en français.**
- **Format `.client`** : ne pas le casser ; `go test ./internal/emu` vérifie que les 95 profils se chargent.
- **Ne jamais afficher une URL de tracker complète dans un message** (elle contient la passkey des trackers privés).
- Le port annoncé est réellement ouvert (accepte et ferme les connexions).
- Unités : 1 ko = 1000 octets (convention usuelle), virgule décimale dans l'UI.

## Comportement de référence

- Débit global aléatoire entre min et max (ko/s), renouvelé toutes les `speedRefreshMinutes`.
- Répartition : `poids = (L/(S+L))² × 100 × L` ; 0 si aucun leecher ou aucun seeder.
- `started` → annonces régulières à l'intervalle du tracker → `stopped` à l'arrêt.
- Échec : tracker suivant + attente progressive (30 s, 60 s, 2 min…), torrent en erreur après `maxFailures`.

## Déploiement

- Machine cible : Docker perso du propriétaire (192.168.1.1), derrière Traefik.
- `docker-compose.yml` : production, Traefik existant sur le réseau externe `proxy`, variables dans `.env` (voir `.env.example`).
- `docker-compose.test.yml` : environnement isolé (Traefik + RatioPilot + tracker factice), aucun vrai tracker contacté.
- Image finale `FROM scratch`, utilisateur 1000:1000, données dans `/data`.

## Pistes d'évolution

- Persister l'historique du graphique entre redémarrages.
- Notifications (Discord/ntfy) sur erreur ou ratio atteint.
- Limites de débit par torrent ou par tracker.
- Script de mise à jour des `.client` depuis leur source d'origine.

package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Config is the user-editable configuration (data/config.json).
type Config struct {
	MinUploadRate       int     `json:"minUploadRate"`    // kB/s (1 kB = 1000 octets, comme la plupart des clients)
	MaxUploadRate       int     `json:"maxUploadRate"`    // kB/s
	SimultaneousSeed    int     `json:"simultaneousSeed"` // torrents annoncés en même temps
	Client              string  `json:"client"`           // fichier .client émulé
	KeepZeroLeechers    bool    `json:"keepTorrentWithZeroLeechers"`
	UploadRatioTarget   float64 `json:"uploadRatioTarget"`   // -1 = jamais
	Port                int     `json:"port"`                // port annoncé aux trackers
	SpeedRefreshMinutes int     `json:"speedRefreshMinutes"` // nouveau débit global aléatoire toutes les N minutes
	MaxFailures         int     `json:"maxFailures"`         // échecs d'annonce consécutifs avant mise en erreur
	AutoStart           bool    `json:"autoStart"`           // démarrer le seed au lancement
}

// DefaultConfig returns safe defaults with a recent client.
func DefaultConfig() Config {
	return Config{
		MinUploadRate:       30,
		MaxUploadRate:       160,
		SimultaneousSeed:    20,
		Client:              "qbittorrent-5.2.4.client",
		KeepZeroLeechers:    true,
		UploadRatioTarget:   -1,
		Port:                0,
		SpeedRefreshMinutes: 20,
		MaxFailures:         5,
		AutoStart:           false,
	}
}

// Validate checks values and returns a French error message.
func (c Config) Validate() error {
	switch {
	case c.MinUploadRate < 0:
		return errors.New("le débit minimum doit être positif")
	case c.MaxUploadRate < c.MinUploadRate:
		return errors.New("le débit maximum doit être ≥ au minimum")
	case c.MaxUploadRate > 1_000_000:
		return errors.New("débit maximum trop élevé (1 000 000 kB/s max)")
	case c.SimultaneousSeed < 1 || c.SimultaneousSeed > 1000:
		return errors.New("torrents simultanés : entre 1 et 1000")
	case c.Client == "":
		return errors.New("aucun client sélectionné")
	case c.UploadRatioTarget != -1 && c.UploadRatioTarget <= 0:
		return errors.New("ratio cible : -1 (désactivé) ou une valeur > 0")
	case c.Port < 0 || c.Port > 65535:
		return errors.New("port invalide")
	case c.SpeedRefreshMinutes < 1 || c.SpeedRefreshMinutes > 1440:
		return errors.New("rafraîchissement du débit : entre 1 et 1440 minutes")
	case c.MaxFailures < 1 || c.MaxFailures > 100:
		return errors.New("échecs max : entre 1 et 100")
	}
	return nil
}

func loadConfig(path string) (Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s illisible: %w", path, err)
	}
	return cfg, nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

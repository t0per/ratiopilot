// RatioPilot — simulateur de seed (réécriture légère de JOAL) avec interface web.
package main

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	_ "time/tzdata" // fuseaux horaires embarqués (image scratch)

	"ratiopilot/internal/engine"
	"ratiopilot/internal/web"
)

var version = "dev"

//go:embed web
var webFS embed.FS

//go:embed clients/*.client
var clientsFS embed.FS

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func main() {
	log.SetFlags(log.LstdFlags)
	dataDir := env("RP_DATA", "./data")
	listen := env("RP_LISTEN", ":8080")

	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		resp, err := http.Get("http://127.0.0.1" + listen[strings.LastIndex(listen, ":"):] + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		os.Exit(0)
	}

	password, err := resolvePassword(dataDir)
	if err != nil {
		log.Fatal(err)
	}

	clients, _ := fs.Sub(clientsFS, "clients")
	eng, err := engine.New(dataDir, clients)
	if err != nil {
		log.Fatalf("démarrage impossible: %v", err)
	}
	static, _ := fs.Sub(webFS, "web")
	srv, err := web.New(eng, static, dataDir, password, version)
	if err != nil {
		log.Fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	engineDone := make(chan struct{})
	go func() { eng.Run(ctx); close(engineDone) }()

	httpSrv := &http.Server{Addr: listen, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("RatioPilot %s — interface sur %s (données : %s)", version, listen, dataDir)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	log.Printf("arrêt en cours (envoi des annonces \"stopped\")…")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = httpSrv.Shutdown(sctx)
	cancel()
	<-engineDone
	log.Printf("arrêté")
}

// resolvePassword: RP_PASSWORD, sinon RP_AUTH=off (auth déléguée à Traefik),
// sinon mot de passe généré et stocké dans data/admin-password.txt.
func resolvePassword(dataDir string) (string, error) {
	if p := os.Getenv("RP_PASSWORD"); p != "" {
		return p, nil
	}
	if strings.EqualFold(os.Getenv("RP_AUTH"), "off") {
		log.Printf("ATTENTION : authentification désactivée (RP_AUTH=off)")
		return "", nil
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return "", err
	}
	p := filepath.Join(dataDir, "admin-password.txt")
	if b, err := os.ReadFile(p); err == nil && strings.TrimSpace(string(b)) != "" {
		return strings.TrimSpace(string(b)), nil
	}
	pw := web.RandomPassword()
	if err := os.WriteFile(p, []byte(pw+"\n"), 0o600); err != nil {
		return "", err
	}
	log.Printf("Mot de passe généré : %s (stocké dans %s — définis RP_PASSWORD pour le changer)", pw, p)
	return pw, nil
}

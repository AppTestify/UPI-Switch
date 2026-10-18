// Package app assembles the switch from its parts; cmd/switch and the end-to-end tests share it.
package app

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/AppTestify/UPI-Switch/internal/api"
	"github.com/AppTestify/UPI-Switch/internal/config"
	"github.com/AppTestify/UPI-Switch/internal/core"
	"github.com/AppTestify/UPI-Switch/internal/db"
	"github.com/AppTestify/UPI-Switch/internal/metrics"
	"github.com/AppTestify/UPI-Switch/internal/ratelimit"
	"github.com/AppTestify/UPI-Switch/internal/recon"
	"github.com/AppTestify/UPI-Switch/internal/risk"
	"github.com/AppTestify/UPI-Switch/internal/router"
	"github.com/AppTestify/UPI-Switch/internal/security"
	"github.com/AppTestify/UPI-Switch/internal/store"
	"github.com/AppTestify/UPI-Switch/internal/webhook"
)

// App is a running switch instance.
type App struct {
	Cfg        config.Switch
	Pool       *pgxpool.Pool
	Store      *store.Store
	Service    *core.Service
	Recon      *recon.Engine
	Dispatcher *webhook.Dispatcher
	Server     *api.Server
	Metrics    *metrics.Registry
	Log        *slog.Logger
}

// Options override parts of the assembly (tests inject keys and HTTP clients).
type Options struct {
	Signer     security.Signer
	BankHTTP   *http.Client
	Logger     *slog.Logger
	FileSource recon.FileSource
}

// ParseAdminTokens parses "name:role:token,..." into admin tokens.
func ParseAdminTokens(s string) ([]api.AdminToken, error) {
	var out []api.AdminToken
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.SplitN(part, ":", 3)
		if len(f) != 3 || (f[1] != api.RoleAdmin && f[1] != api.RoleOps && f[1] != api.RoleViewer) || len(f[2]) < 16 {
			return nil, fmt.Errorf("ADMIN_TOKENS entry %q must be name:role:token with role admin|ops|viewer and a token of 16+ chars", f[0])
		}
		out = append(out, api.AdminToken{Name: f[0], Role: f[1], Token: f[2]})
	}
	return out, nil
}

// ParseConsoleUsers parses "email:role:password,..." into console sign-ins.
func ParseConsoleUsers(s string) ([]api.ConsoleUser, error) {
	var out []api.ConsoleUser
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.SplitN(part, ":", 3)
		if len(f) != 3 || !strings.Contains(f[0], "@") || (f[1] != api.RoleAdmin && f[1] != api.RoleOps && f[1] != api.RoleViewer) || len(f[2]) < 8 {
			return nil, fmt.Errorf("CONSOLE_USERS entry %q must be email:role:password with role admin|ops|viewer and a password of 8+ chars", f[0])
		}
		name, _, _ := strings.Cut(f[0], "@")
		out = append(out, api.ConsoleUser{Email: strings.ToLower(f[0]), Name: name, Role: f[1], Password: f[2]})
	}
	return out, nil
}

// New connects to the database, migrates, and wires every component.
func New(ctx context.Context, cfg config.Switch, opt Options) (*App, error) {
	log := opt.Logger
	if log == nil {
		log = slog.Default()
	}
	if cfg.DataKey == "" {
		return nil, errors.New("DATA_KEY is required (vault key for VPA and secret encryption)")
	}
	signer := opt.Signer
	if signer == nil {
		k, err := security.LoadPrivateKey(cfg.SigningKeyPath)
		if err != nil {
			return nil, fmt.Errorf("signing key: %w (run `go run ./cmd/keygen` first)", err)
		}
		signer = security.NewSoftHSM(k, cfg.SigningKeyID)
	}
	tokens, err := ParseAdminTokens(cfg.AdminTokens)
	if err != nil {
		return nil, err
	}
	loc, err := time.LoadLocation(cfg.ReconTZ)
	if err != nil {
		return nil, fmt.Errorf("RECON_TZ: %w", err)
	}
	pool, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, err
	}
	if err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	bankHTTP := opt.BankHTTP
	if bankHTTP == nil {
		if bankHTTP, err = bankClient(cfg); err != nil {
			pool.Close()
			return nil, err
		}
	}
	vault := security.NewVault(cfg.DataKey)
	st := store.New(pool, vault)
	m := metrics.New()
	rt := router.New(nil)
	svc := core.New(core.Config{
		OrgID: cfg.OrgID, IntentExpiry: cfg.IntentExpiry, FirstCheckAfter: cfg.FirstCheckAfter, MaxCheckAttempts: cfg.MaxCheckAttempts,
	}, core.Deps{
		Store: st, Signer: signer, Vault: vault, Router: rt, HTTP: bankHTTP, Logger: log, Metrics: m,
		Risk: &risk.Engine{Stats: st, GlobalMCC: cfg.BlockedMCC, Location: loc},
	})
	if cfg.PSPSeed != "" {
		if err := seedPSPs(ctx, st, cfg.PSPSeed); err != nil {
			pool.Close()
			return nil, err
		}
	}
	if err := svc.ReloadPSPs(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	src := opt.FileSource
	if src == nil {
		src = recon.HTTPSource{Client: bankHTTP}
	}
	rc := &recon.Engine{Store: st, Resolver: svc, Source: src, Location: loc, Log: log}
	var lim ratelimit.Limiter = ratelimit.NewLocal()
	if cfg.RedisURL != "" {
		o, err := redis.ParseURL(cfg.RedisURL)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("REDIS_URL: %w", err)
		}
		lim = ratelimit.Redis{Client: redis.NewClient(o)}
	}
	users, err := ParseConsoleUsers(cfg.ConsoleUsers)
	if err != nil {
		pool.Close()
		return nil, err
	}
	sessionKey := sha256.Sum256([]byte("console-session:" + cfg.DataKey))
	srv := api.New(api.Config{AdminTokens: tokens, TrustProxy: cfg.TrustProxy, BankIPAllowlist: cfg.BankIPAllowlist,
		ConsoleUsers: users, SessionKey: sessionKey[:], SessionTTL: cfg.SessionTTL, Location: loc}, svc, st, rc, lim, log, m)
	return &App{
		Cfg: cfg, Pool: pool, Store: st, Service: svc, Recon: rc, Server: srv, Metrics: m, Log: log,
		Dispatcher: &webhook.Dispatcher{Store: st, Log: log, Metrics: m},
	}, nil
}

// Start runs the background workers: status poller, webhook dispatcher, PSP heartbeat and config reload.
func (a *App) Start(ctx context.Context) {
	go a.Service.RunPoller(ctx, a.Cfg.PollEvery)
	go a.Dispatcher.Run(ctx, a.Cfg.WebhookEvery)
	go a.Service.RunHeartbeat(ctx, a.Cfg.HeartbeatEvery)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := a.Service.ReloadPSPs(ctx); err != nil {
					a.Log.Error("psp reload", "err", err)
				}
			}
		}
	}()
}

// Close releases resources.
func (a *App) Close() { a.Pool.Close() }

type pspSeed struct {
	store.PSPRecord
	PublicKeyFile string `json:"public_key_file"`
}

func seedPSPs(ctx context.Context, st *store.Store, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("PSP_SEED: %w", err)
	}
	var seeds []pspSeed
	if err := json.Unmarshal(b, &seeds); err != nil {
		return fmt.Errorf("PSP_SEED: %w", err)
	}
	for _, sd := range seeds {
		if sd.PublicKeyFile != "" {
			k, err := os.ReadFile(sd.PublicKeyFile)
			if err != nil {
				return fmt.Errorf("PSP_SEED %s: %w", sd.ID, err)
			}
			sd.PublicKeyPEM = string(k)
		}
		if err := st.UpsertPSP(ctx, sd.PSPRecord); err != nil {
			return err
		}
	}
	return nil
}

// bankClient builds the HTTP client for PSP links, with mTLS when certificates are configured.
func bankClient(cfg config.Switch) (*http.Client, error) {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 256
	if cfg.BankTLSCert != "" || cfg.BankTLSCA != "" {
		tc := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.BankTLSCert != "" {
			cert, err := tls.LoadX509KeyPair(cfg.BankTLSCert, cfg.BankTLSKey)
			if err != nil {
				return nil, fmt.Errorf("bank mTLS cert: %w", err)
			}
			tc.Certificates = []tls.Certificate{cert}
		}
		if cfg.BankTLSCA != "" {
			pem, err := os.ReadFile(cfg.BankTLSCA)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(pem)
			tc.RootCAs = pool
		}
		tr.TLSClientConfig = tc
	}
	return &http.Client{Timeout: 10 * time.Second, Transport: tr}, nil
}

// ServerTLS builds the listener TLS config; with TLS_CLIENT_CA set, client certificates are verified (mTLS).
func ServerTLS(cfg config.Switch) (*tls.Config, error) {
	if cfg.TLSCert == "" {
		return nil, nil
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSClientCA != "" {
		pem, err := os.ReadFile(cfg.TLSClientCA)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(pem)
		tc.ClientCAs = pool
		tc.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return tc, nil
}

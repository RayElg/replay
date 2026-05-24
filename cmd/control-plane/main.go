package main

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pressly/goose/v3"
	"golang.org/x/term"
)

//go:embed migrations/*.sql
var migrations embed.FS

func initS3(storeURL string) (*minio.Client, string, error) {
	u, err := url.Parse(storeURL)
	if err != nil {
		return nil, "", err
	}
	endpoint := u.Host
	bucket := strings.TrimPrefix(u.Path, "/")

	accessKey := os.Getenv("MINIO_ROOT_USER")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey := os.Getenv("MINIO_ROOT_PASSWORD")
	if secretKey == "" {
		secretKey = "minioadmin"
	}

	// Secure drives the scheme of presigned URLs (https vs http). Derive it
	// from the configured URL so PUBLIC_OBJECT_STORE=https://… actually emits
	// https presigns; anything else (http, s3, schemeless) stays plaintext.
	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: u.Scheme == "https",
		Region: "us-east-1",
	})
	return client, bucket, err
}

// openDB and runMigrations are factored so CLI subcommands can re-use them
// without spinning up the HTTP server.
func openDB() *sql.DB {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		dbURL = "postgres://replay:replay@localhost:5432/postgres?sslmode=disable"
	}
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		slog.Error("failed to open database", "error", err)
		os.Exit(1)
	}
	if err := db.Ping(); err != nil {
		slog.Error("failed to ping database", "error", err)
		os.Exit(1)
	}
	return db
}

func runMigrations(db *sql.DB) {
	goose.SetBaseFS(migrations)
	if err := goose.SetDialect("postgres"); err != nil {
		slog.Error("failed to set goose dialect", "error", err)
		os.Exit(1)
	}
	if err := goose.Up(db, "migrations"); err != nil {
		slog.Error("failed to run migrations", "error", err)
		os.Exit(1)
	}
}

// missingProdSecrets returns the names of env vars that look unsafe for a real
// deployment. Only consulted when REPLAY_COOKIE_SECURE=true; in dev mode the
// loose defaults stay convenient. Order matters for the error message — list
// secrets first, then unsafe defaults.
func missingProdSecrets() []string {
	if strings.ToLower(os.Getenv("REPLAY_COOKIE_SECURE")) != "true" {
		return nil
	}
	var missing []string
	required := []string{
		"REPLAY_ENCRYPT_KEY",
		"REPLAY_WEBHOOK_TOKEN",
		"REPLAY_BROKER_JWT_SECRET",
		"REPLAY_RUNNER_MQTT_PASSWORD",
		"REPLAY_EXTERNAL_URL",
	}
	for _, name := range required {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if os.Getenv("MINIO_ROOT_PASSWORD") == "minioadmin" || os.Getenv("MINIO_ROOT_PASSWORD") == "" {
		missing = append(missing, "MINIO_ROOT_PASSWORD (must not be empty or 'minioadmin')")
	}
	if os.Getenv("POSTGRES_PASSWORD") == "replay" || os.Getenv("POSTGRES_PASSWORD") == "" {
		missing = append(missing, "POSTGRES_PASSWORD (must not be empty or 'replay')")
	}
	return missing
}

func main() {
	level := slog.LevelInfo
	if os.Getenv("REPLAY_LOG_DEBUG") == "true" {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)

	// CLI subcommands — operator-facing utilities that don't start the server.
	// Dispatched before the flag parser so they don't conflict with --migrate-only.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "bootstrap-key":
			runBootstrapKeyCLI(os.Args[2:])
			return
		case "create-user":
			runCreateUserCLI(os.Args[2:])
			return
		case "reset-password":
			runResetPasswordCLI(os.Args[2:])
			return
		case "invite":
			runInviteCLI(os.Args[2:])
			return
		case "reset-link":
			runResetLinkCLI(os.Args[2:])
			return
		case "gen-broker-jwt-key":
			runGenBrokerJWTKeyCLI(os.Args[2:])
			return
		case "rewrap-secrets":
			runRewrapSecretsCLI(os.Args[2:])
			return
		case "seed-example":
			runSeedExampleCLI(os.Args[2:])
			return
		}
	}

	migrateOnly := flag.Bool("migrate-only", false, "run migrations and exit")
	flag.Parse()

	// Operator signalled "this is production" by setting REPLAY_COOKIE_SECURE=true.
	// Refuse to start with dev defaults or empty secrets — better to fail loudly
	// at boot than to silently serve with a known-weak setup.
	if !*migrateOnly {
		if missing := missingProdSecrets(); len(missing) > 0 {
			slog.Error("production deployment missing required env vars",
				"missing", missing,
				"hint", "REPLAY_COOKIE_SECURE=true signals a real deployment — set the listed vars or unset REPLAY_COOKIE_SECURE for dev")
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db := openDB()
	defer db.Close()
	slog.Info("database connected")
	runMigrations(db)
	slog.Info("migrations complete")

	if *migrateOnly {
		return
	}

	publicStoreURL := os.Getenv("PUBLIC_OBJECT_STORE")
	if publicStoreURL == "" {
		publicStoreURL = "http://localhost:9000/replay-artifacts"
	}
	s3, bucket, err := initS3(publicStoreURL)
	if err != nil {
		slog.Error("failed to init S3", "error", err)
		os.Exit(1)
	}

	internalStoreURL := os.Getenv("INTERNAL_OBJECT_STORE")
	if internalStoreURL == "" {
		internalStoreURL = publicStoreURL
	}
	internalS3, _, err := initS3(internalStoreURL)
	if err != nil {
		slog.Error("failed to init internal S3", "error", err)
		os.Exit(1)
	}

	// Env-var bootstrap (idempotent — only fires on first boot with empty tables).
	bootstrapCtx, bootstrapCancel := context.WithTimeout(ctx, 10*time.Second)
	bootstrapAPIKeyFromEnv(bootstrapCtx, db)
	bootstrapUserFromEnv(bootstrapCtx, db)
	configurePgmqttAuth(bootstrapCtx, db)
	if err := configurePgmqttJWT(bootstrapCtx, db); err != nil {
		// JWT prerequisites are operator-controllable (license, env vars). If we
		// can't satisfy them, refusing to boot is the only safe option — otherwise
		// browsers fall through to anonymous broker connections.
		slog.Error("pgmqtt jwt autoconfig failed", "error", err)
		bootstrapCancel()
		os.Exit(1)
	}
	bootstrapCancel()

	if raw := os.Getenv("REPLAY_ALLOWED_REPO_PATHS"); raw != "" {
		for _, p := range strings.Split(raw, ":") {
			if p = strings.TrimSpace(p); p != "" {
				allowedRepoPaths = append(allowedRepoPaths, p)
			}
		}
		slog.Info("local repo access enabled", "paths", allowedRepoPaths)
	}

	var anthClient *anthropic.Client
	agentModel := os.Getenv("AGENT_MODEL")
	if agentModel == "" {
		agentModel = "claude-sonnet-4-6"
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		c := anthropic.NewClient(option.WithAPIKey(key))
		anthClient = &c
		slog.Info("agent enabled", "model", agentModel)
	} else {
		slog.Warn("ANTHROPIC_API_KEY not set — agent chat will return 503")
	}

	go startAutoTriageLoop(ctx, db, internalS3, bucket, anthClient, agentModel)

	// Long-running background jobs.
	startAuditSink(ctx, db)
	go gcSessions(ctx, db)
	go gcAuditEvents(ctx, db)
	go gcUserInvitesAndResets(ctx, db)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(authMiddleware(db))
	r.Use(auditMiddleware())

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		w.Header().Set("Content-Type", "application/json")
		if err := db.PingContext(ctx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{
				"status": "unhealthy",
				"db":     err.Error(),
			})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"status": "ok", "db": "ok"})
	})

	registerAuthConfigRoute(r, db)
	registerPasswordAuthRoutes(r, db)
	registerAPIKeyRoutes(r, db)
	registerUserManagementRoutes(r, db)
	registerAuditRoutes(r, db)
	registerBrokerTokenRoute(r, db)
	registerBrokerCredentialsRoute(r, db)

	registerWorkspaceRoutes(r, db)
	registerRunRoutes(r, db, s3, bucket)
	registerScriptRoutes(r, db)
	registerEnvironmentRoutes(r, db)
	registerIntegrationRoutes(r, db)
	registerGithubScriptRoutes(r, db)
	registerWebhookRoutes(r, db)
	registerGithubWebhookRoute(r, db)
	registerAgentRoutes(r, db, internalS3, bucket, anthClient, agentModel)
	registerScriptPatchRoutes(r, db)

	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	server := &http.Server{
		Addr:        addr,
		Handler:     r,
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	go func() {
		slog.Info("control-plane starting", "addr", addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("server failed", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "error", err)
	}
}

// ─── CLI subcommands ────────────────────────────────────────────────────

func runBootstrapKeyCLI(args []string) {
	fs := flag.NewFlagSet("bootstrap-key", flag.ExitOnError)
	name := fs.String("name", "bootstrap", "human label for the key")
	_ = fs.Parse(args)

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	key, err := createBootstrapAPIKey(ctx, db, *name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap-key failed:", err)
		os.Exit(1)
	}
	fmt.Println(key)
	fmt.Fprintln(os.Stderr, "store this key now — it will not be shown again")
}

func runCreateUserCLI(args []string) {
	fs := flag.NewFlagSet("create-user", flag.ExitOnError)
	email := fs.String("email", "", "user email")
	name := fs.String("name", "", "display name (defaults to email)")
	password := fs.String("password", "", "password (prompted if blank)")
	_ = fs.Parse(args)

	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email required")
		os.Exit(2)
	}
	if *password == "" {
		*password = promptPassword("password: ")
	}

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := createUserCLI(ctx, db, *email, *name, *password); err != nil {
		fmt.Fprintln(os.Stderr, "create-user failed:", err)
		os.Exit(1)
	}
	fmt.Println("ok")
}

func runResetPasswordCLI(args []string) {
	fs := flag.NewFlagSet("reset-password", flag.ExitOnError)
	email := fs.String("email", "", "user email")
	password := fs.String("password", "", "new password (prompted if blank)")
	_ = fs.Parse(args)

	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email required")
		os.Exit(2)
	}
	if *password == "" {
		*password = promptPassword("new password: ")
	}

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := createUserCLI(ctx, db, *email, "", *password); err != nil {
		fmt.Fprintln(os.Stderr, "reset-password failed:", err)
		os.Exit(1)
	}
	// Revoke existing sessions so old logins can't continue with the rotated password.
	if _, err := db.ExecContext(ctx, `DELETE FROM user_sessions
		WHERE user_id IN (SELECT id FROM users WHERE lower(email) = lower($1))`, *email); err != nil {
		fmt.Fprintln(os.Stderr, "warn: failed to revoke sessions:", err)
	}
	fmt.Println("ok")
}

// runInviteCLI: `control-plane invite --email foo@bar.com [--workspace slug]`
//
// Self-hosted invites are operator-driven: we mint the token + URL here and
// the operator hands it to the user out-of-band. No SMTP, no Resend, no email
// config to keep alive.
func runInviteCLI(args []string) {
	fs := flag.NewFlagSet("invite", flag.ExitOnError)
	email := fs.String("email", "", "user email")
	workspaceSlug := fs.String("workspace", "", "workspace slug (defaults to the seeded workspace)")
	_ = fs.Parse(args)

	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email required")
		os.Exit(2)
	}
	base, err := externalURLBase()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	workspaceID := defaultWorkspaceID
	if *workspaceSlug != "" {
		if err := db.QueryRowContext(ctx,
			`SELECT id FROM workspaces WHERE slug = $1`, *workspaceSlug,
		).Scan(&workspaceID); err != nil {
			fmt.Fprintln(os.Stderr, "invite: workspace lookup failed:", err)
			os.Exit(1)
		}
	}

	token, expires, err := mintInvite(ctx, db, workspaceID, *email, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "invite:", err)
		os.Exit(1)
	}
	fmt.Println(base + "/invite/" + token)
	fmt.Fprintln(os.Stderr, "expires:", expires.Format(time.RFC3339))
}

// runResetLinkCLI: `control-plane reset-link --email foo@bar.com`
//
// Issues a one-shot password-reset link. Unlike the (deleted) HTTP reset
// request flow, this *does* fail if the email isn't a known user — operators
// benefit from the explicit error, presence-leak concerns don't apply on a
// CLI they alone run.
func runResetLinkCLI(args []string) {
	fs := flag.NewFlagSet("reset-link", flag.ExitOnError)
	email := fs.String("email", "", "user email")
	_ = fs.Parse(args)

	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email required")
		os.Exit(2)
	}
	base, err := externalURLBase()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	db := openDB()
	defer db.Close()
	runMigrations(db)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	token, expires, err := mintPasswordReset(ctx, db, *email)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reset-link:", err)
		os.Exit(1)
	}
	fmt.Println(base + "/reset/" + token)
	fmt.Fprintln(os.Stderr, "expires:", expires.Format(time.RFC3339))
}

func promptPassword(prompt string) string {
	fmt.Fprint(os.Stderr, prompt)
	b, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Fprintln(os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "password read failed:", err)
		os.Exit(1)
	}
	return string(b)
}

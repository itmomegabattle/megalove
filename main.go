package main

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrations embed.FS

//go:embed demo.sql
var demoSQL string

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	command := "serve"
	if len(os.Args) == 2 {
		command = os.Args[1]
	}
	if len(os.Args) == 2 && command == "admin-hash" {
		return printAdminHash(os.Stdin, os.Stdout)
	}
	if len(os.Args) > 2 || (command != "serve" && command != "migrate" && command != "demo") {
		return errors.New("использование: megalove [serve|migrate|demo|admin-hash]")
	}

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return errors.New("задай DATABASE_URL для подключения к PostgreSQL")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return errors.New("не удалось прочитать настройки DATABASE_URL")
	}
	defer db.Close()

	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return errors.New("не удалось подключиться к PostgreSQL, проверь адрес и доступ")
	}

	switch command {
	case "migrate":
		if err := migrate(ctx, db); err != nil {
			return err
		}
		log.Print("Миграции применены")
		return nil
	case "demo":
		if _, err := db.ExecContext(ctx, demoSQL); err != nil {
			return fmt.Errorf("демоданные: %w", err)
		}
		log.Print("Демоданные готовы")
		return nil
	}

	if _, err := db.ExecContext(ctx, "SELECT 1 FROM events LIMIT 0"); err != nil {
		return errors.New("схема PostgreSQL недоступна, сначала выполни migrate")
	}
	address := os.Getenv("MEGALOVE_ADDR")
	if address == "" {
		address = "127.0.0.1:8081"
	}
	mux := http.NewServeMux()
	eventSlug := os.Getenv("MEGALOVE_EVENT_SLUG")
	if eventSlug == "" {
		eventSlug = "megabattle-2026"
	}
	localVoteHTTP := os.Getenv("MEGALOVE_VOTE_LOCAL_HTTP") == "1"
	if localVoteHTTP && !loopbackAddress(address) {
		return errors.New("MEGALOVE_VOTE_LOCAL_HTTP требует локальный IP в MEGALOVE_ADDR")
	}
	voting := votingEvent{db: db, slug: eventSlug, localHTTP: localVoteHTTP}
	if err := mountAdmin(mux, db, voting, os.Getenv("MEGALOVE_ADMIN_HASH"), address, os.Getenv("MEGALOVE_ADMIN_LOCAL_HTTP") == "1"); err != nil {
		return err
	}
	testVoting := voting
	testVoting.testRun = true
	mux.HandleFunc("GET /test/{$}", testVoting.home)
	mux.HandleFunc("GET /api/voting/test/status", testVoting.status)
	mux.HandleFunc("GET /api/voting/test/catalog", testVoting.catalog)
	mux.HandleFunc("GET /api/voting/test/photos/{person}", testVoting.publicPhoto)
	mux.Handle("POST /api/voting/test/ballot", http.NewCrossOriginProtection().Handler(http.HandlerFunc(testVoting.submitBallot)))
	mux.HandleFunc("GET /{$}", voting.home)
	mux.HandleFunc("GET /people", voting.people)
	mux.HandleFunc("GET /translation", translationPage)
	mux.HandleFunc("GET /singer", facultyChartPage("vocalist", "Вокалист"))
	mux.HandleFunc("GET /actor", facultyChartPage("actor", "Актёр"))
	mux.HandleFunc("GET /dancer", facultyChartPage("dancer", "Танцор"))
	mux.HandleFunc("GET /danser", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dancer", http.StatusPermanentRedirect)
	})
	mux.HandleFunc("GET /api/translation", voting.translationRanks)
	mux.HandleFunc("GET /api/translation/{nomination}", voting.translationTopCandidates)
	mux.HandleFunc("GET /people/photos/{person}", voting.peoplePhoto)
	registration := participantAdmin{db}
	mux.HandleFunc("GET /register/complete", func(w http.ResponseWriter, r *http.Request) {
		writeParticipantPage(w, "registered", http.StatusOK, participantView{})
	})
	mux.HandleFunc("GET /register/{token}", registration.register)
	mux.Handle("POST /register/{token}", http.NewCrossOriginProtection().Handler(http.HandlerFunc(registration.register)))
	mux.HandleFunc("GET /api/voting/status", voting.status)
	mux.HandleFunc("GET /api/voting/catalog", voting.catalog)
	mux.HandleFunc("GET /api/voting/photos/{person}", voting.publicPhoto)
	mux.HandleFunc("GET /api/backup/portraits/{person}", voting.backupPortrait)
	mux.Handle("POST /api/voting/ballot", http.NewCrossOriginProtection().Handler(http.HandlerFunc(voting.submitBallot)))
	mux.Handle("GET /assets/", http.FileServerFS(viewerAssets))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			http.Error(w, "База недоступна", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	server := &http.Server{
		Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
	}
	log.Printf("MegaLove: http://%s", address)
	go voting.syncBackupLoop()
	return server.ListenAndServe()
}

func loopbackAddress(address string) bool {
	host, _, err := net.SplitHostPort(address)
	return err == nil && net.ParseIP(host).IsLoopback()
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Serialize migrations when multiple operators launch them together.
	if _, err := tx.ExecContext(ctx, "SELECT pg_advisory_xact_lock(814214, 4)"); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			name text PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)
	`); err != nil {
		return err
	}
	files, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	for _, name := range files {
		var applied bool
		err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)", name).Scan(&applied)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (name) VALUES ($1)", name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

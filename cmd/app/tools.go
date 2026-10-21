package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"railway/internal/allocator"
	"railway/internal/auth"
	"railway/internal/platform/pg"
	"railway/internal/platform/redisx"
	"railway/migrations"
)

// app migrate up [--seed=dev] [--open-in=2m]
// app migrate down | status
func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	seed := fs.String("seed", "", "load seed data: dev")
	openIn := fs.Duration("open-in", 0, "dev only: move every booking window to now+duration")
	action := "up"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		action, args = args[0], args[1:]
	}
	_ = fs.Parse(args)
	url := os.Getenv("PG_URL")
	if url == "" {
		return errors.New("PG_URL is required")
	}
	db, err := sql.Open("pgx", url)
	if err != nil {
		return err
	}
	defer db.Close()
	for i := 0; ; i++ { // wait for Postgres to come up
		if err = db.Ping(); err == nil || i > 30 {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return err
	}
	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		return err
	}
	switch action {
	case "up":
		if err := goose.Up(db, "."); err != nil {
			return err
		}
	case "down":
		return goose.Down(db, ".")
	case "status":
		return goose.Status(db, ".")
	default:
		return fmt.Errorf("unknown migrate action %q", action)
	}
	if *seed == "dev" {
		if err := seedDev(url, *openIn); err != nil {
			return fmt.Errorf("seed: %w", err)
		}
	}
	return nil
}

// testUsers are extra dev accounts (password "dev-password-123") with saved
// passengers; test/seeddata books tickets for them through the API.
var testUsers = []struct {
	phone, email, name string
	passengers         [][3]string // name, gender, type
}{
	{"+8801711111111", "ayesha@example.com", "Ayesha Rahman", [][3]string{{"Rafiq Rahman", "MALE", "ADULT"}, {"Nabila Rahman", "FEMALE", "CHILD"}}},
	{"+8801811111112", "karim@example.com", "Karim Uddin", [][3]string{{"Salma Begum", "FEMALE", "ADULT"}}},
	{"+8801911111113", "nusrat@example.com", "Nusrat Jahan", [][3]string{{"Imran Hossain", "MALE", "ADULT"}, {"Mitu Jahan", "FEMALE", "ADULT"}}},
	{"+8801611111114", "tanvir@example.com", "Tanvir Hasan", nil},
	{"+8801511111115", "farhana@example.com", "Farhana Akter", [][3]string{{"Sabbir Ahmed", "MALE", "ADULT"}}},
}

func testUserPhones() []string {
	out := make([]string, len(testUsers))
	for i, u := range testUsers {
		out[i] = u.phone
	}
	return out
}

// seedTestUsers adds the test users that do not exist yet, each with their saved passengers.
func seedTestUsers(ctx context.Context, pool *pgxpool.Pool) error {
	hasher := auth.NewHasher(65536, 1, 2)
	for _, u := range testUsers {
		var exists bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_user WHERE phone = $1)`, u.phone).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		hash, err := hasher.Hash("dev-password-123")
		if err != nil {
			return err
		}
		var id int64
		if err := pool.QueryRow(ctx, `INSERT INTO app_user (phone, email, name, password_hash, verified)
		        VALUES ($1, $2, $3, $4, true) RETURNING user_id`, u.phone, u.email, u.name, hash).Scan(&id); err != nil {
			return err
		}
		for _, p := range u.passengers {
			if _, err := pool.Exec(ctx, `INSERT INTO saved_passenger (user_id, name, gender, passenger_type) VALUES ($1, $2, $3, $4)`,
				id, p[0], p[1], p[2]); err != nil {
				return err
			}
		}
	}
	return nil
}

// seedDev loads seed/bd.sql (Bangladesh Railway stations, coaches, trains and
// journeys for the next 12 days; re-running adds new dates) and a dev user
// "+8801700000001" / "dev-password-123". --open-in moves every booking window
// that has not opened yet to now+duration.
func seedDev(url string, openIn time.Duration) error {
	ctx := context.Background()
	pool, err := pg.Connect(ctx, url)
	if err != nil {
		return err
	}
	defer pool.Close()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	_, err = conn.Conn().PgConn().Exec(ctx, migrations.BDSeed).ReadAll()
	conn.Release()
	if err != nil {
		return fmt.Errorf("bangladesh seed: %w", err)
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM app_user WHERE phone = '+8801700000001'`).Scan(&users); err != nil {
		return err
	}
	if users == 0 {
		hash, err := auth.NewHasher(65536, 1, 2).Hash("dev-password-123")
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `INSERT INTO app_user (phone, email, name, password_hash, verified)
		        VALUES ('+8801700000001', 'dev@example.com', 'Dev User', $1, true)`, hash); err != nil {
			return err
		}
	}
	if err := seedTestUsers(ctx, pool); err != nil {
		return err
	}
	var journeys int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM journey WHERE run_date >= current_date`).Scan(&journeys)
	fmt.Printf("Bangladesh seed loaded: %d upcoming journeys; dev user +8801700000001 / dev-password-123\n", journeys)
	fmt.Printf("test users (password dev-password-123): %s\n", strings.Join(testUserPhones(), ", "))

	if openIn > 0 {
		at := time.Now().UTC().Add(openIn)
		tag, err := pool.Exec(ctx, `UPDATE journey_pool SET opens_at = $1 WHERE opens_at > $1`, at)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, `UPDATE scheduler_job SET due_at = $1 - interval '1 minute' * (kind = 'WARM')::int
		        WHERE kind IN ('WARM','OPEN') AND sent_at IS NULL AND due_at > $1`, at); err != nil {
			return err
		}
		fmt.Printf("%d booking windows now open at %s\n", tag.RowsAffected(), at.Format(time.RFC3339))
	}
	return nil
}

// app keygen --dir ./keys: dev ES256 keys for JWTs, local admission tokens and ticket QR codes.
func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ExitOnError)
	dir := fs.String("dir", "keys", "output directory")
	_ = fs.Parse(args)
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{"dev-es256.pem", "admission-es256.pem", "ticket-es256.pem"} {
		p := filepath.Join(*dir, name)
		made, err := auth.GenerateKeyFile(p)
		if err != nil {
			return err
		}
		if made {
			fmt.Println("wrote", p)
		}
	}
	return nil
}

// app invariants --journey 902341: replay the log and run the §24 checks.
func runInvariants(args []string) error {
	fs := flag.NewFlagSet("invariants", flag.ExitOnError)
	id := fs.Int64("journey", 0, "journey id")
	_ = fs.Parse(args)
	ctx := context.Background()
	rt, err := setup(ctx)
	if err != nil {
		return err
	}
	cfg := rt.cfg
	loader := allocator.NewLoader(cfg, rt.cat, rt.db)
	rdb := redisx.New(cfg.RedisAddr)
	res, err := allocator.CheckJourney(ctx, loader, rt.db, rdb, *id)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(b))
	if ok, _ := res["ok"].(bool); !ok {
		return errors.New("invariant violations")
	}
	return nil
}

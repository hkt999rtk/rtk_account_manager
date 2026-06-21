package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/usercache"

	"github.com/jackc/pgx/v5"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: user-cache <rebuild|delete|inspect> [--user-id id|--email email]")
	}
	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()
	cache := usercache.NewRedisCache(usercache.Config{
		Addr:   cfg.UserCacheAddr,
		Prefix: cfg.UserCachePrefix,
	})
	switch args[0] {
	case "rebuild":
		return rebuild(ctx, db, cache)
	case "delete":
		userID, email, err := parseUserSelector(args[1:])
		if err != nil {
			return err
		}
		return deleteUser(ctx, cache, userID, email)
	case "inspect":
		userID, email, err := parseUserSelector(args[1:])
		if err != nil {
			return err
		}
		return inspect(ctx, db, cache, userID, email)
	default:
		return fmt.Errorf("unknown user-cache command %q", args[0])
	}
}

type dbQuerier interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func rebuild(ctx context.Context, db dbQuerier, cache *usercache.RedisCache) error {
	rows, err := db.Query(ctx, `
		SELECT id::text, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, developer_cloud_limit, created_at, updated_at, disabled_at
		FROM users
		WHERE disabled_at IS NULL
	`)
	if err != nil {
		return err
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		user, hash, err := scanPlatformAuth(rows)
		if err != nil {
			return err
		}
		if err := cache.PutPlatformAuth(ctx, user, hash); err != nil {
			return err
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	fmt.Printf("rebuilt %d platform user cache records\n", count)
	return nil
}

func deleteUser(ctx context.Context, cache *usercache.RedisCache, userID, email string) error {
	if strings.TrimSpace(userID) == "" {
		cachedID, ok, err := cache.GetPlatformUserIDByEmail(ctx, email)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Println("cache record not found")
			return nil
		}
		userID = cachedID
	}
	if err := cache.DeletePlatformUser(ctx, userID); err != nil {
		return err
	}
	fmt.Printf("deleted platform user cache %s\n", userID)
	return nil
}

func inspect(ctx context.Context, db dbQuerier, cache *usercache.RedisCache, userID, email string) error {
	if strings.TrimSpace(userID) == "" {
		if err := db.QueryRow(ctx, `
			SELECT id::text FROM users WHERE email = $1 AND disabled_at IS NULL
		`, normalizeEmail(email)).Scan(&userID); errors.Is(err, pgx.ErrNoRows) {
			return errors.New("postgres user not found")
		} else if err != nil {
			return err
		}
	}
	pgUser, pgHash, err := getPlatformAuth(ctx, db, userID)
	if err != nil {
		return err
	}
	cacheUser, cacheHash, ok, err := cache.GetPlatformAuth(ctx, userID)
	if err != nil {
		return err
	}
	fmt.Printf("postgres user_id=%s email=%s updated_at=%s\n", pgUser.ID, pgUser.Email, pgUser.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"))
	if !ok {
		fmt.Println("redis missing")
		return nil
	}
	match := pgUser.Email == cacheUser.Email && pgHash == cacheHash && pgUser.UpdatedAt.Equal(cacheUser.UpdatedAt)
	fmt.Printf("redis user_id=%s email=%s updated_at=%s match=%t\n", cacheUser.ID, cacheUser.Email, cacheUser.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"), match)
	return nil
}

func parseUserSelector(args []string) (string, string, error) {
	fs := flag.NewFlagSet("selector", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	userID := fs.String("user-id", "", "platform user id")
	email := fs.String("email", "", "platform user email")
	if err := fs.Parse(args); err != nil {
		return "", "", err
	}
	if strings.TrimSpace(*userID) == "" && strings.TrimSpace(*email) == "" {
		return "", "", errors.New("either --user-id or --email is required")
	}
	return strings.TrimSpace(*userID), normalizeEmail(*email), nil
}

func getPlatformAuth(ctx context.Context, db dbQuerier, userID string) (model.User, string, error) {
	return scanPlatformAuth(db.QueryRow(ctx, `
		SELECT id::text, email, password_hash, display_name, email_verified, email_verified_at, signup_pending_verification, developer_cloud_limit, created_at, updated_at, disabled_at
		FROM users
		WHERE id = $1 AND disabled_at IS NULL
	`, userID))
}

type authScanner interface {
	Scan(dest ...any) error
}

func scanPlatformAuth(row authScanner) (model.User, string, error) {
	var user model.User
	var hash string
	err := row.Scan(&user.ID, &user.Email, &hash, &user.DisplayName, &user.EmailVerified, &user.EmailVerifiedAt, &user.SignupPendingVerification, &user.DeveloperCloudLimit, &user.CreatedAt, &user.UpdatedAt, &user.DisabledAt)
	return user, hash, err
}

func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

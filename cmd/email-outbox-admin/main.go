package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: email-outbox-admin list [status] [limit] | requeue <id>")
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
	repository := store.New(db)
	switch args[0] {
	case "list":
		status := model.EmailOutboxStatus("")
		if len(args) > 1 {
			status = model.EmailOutboxStatus(strings.TrimSpace(args[1]))
		}
		limit := 50
		if len(args) > 2 {
			limit, err = strconv.Atoi(args[2])
			if err != nil {
				return fmt.Errorf("invalid limit: %w", err)
			}
		}
		items, err := repository.ListEmailOutbox(ctx, status, limit)
		if err != nil {
			return err
		}
		type safeItem struct {
			ID             string                  `json:"id"`
			IdempotencyKey string                  `json:"idempotency_key"`
			MessageType    string                  `json:"message_type"`
			Status         model.EmailOutboxStatus `json:"status"`
			AttemptCount   int                     `json:"attempt_count"`
			AvailableAt    time.Time               `json:"available_at"`
			LeaseUntil     *time.Time              `json:"lease_until,omitempty"`
			LastError      *string                 `json:"last_error,omitempty"`
			ExpiresAt      *time.Time              `json:"expires_at,omitempty"`
			SentAt         *time.Time              `json:"sent_at,omitempty"`
			CreatedAt      time.Time               `json:"created_at"`
		}
		safeItems := make([]safeItem, 0, len(items))
		for _, item := range items {
			safeItems = append(safeItems, safeItem{
				ID: item.ID, IdempotencyKey: item.IdempotencyKey, MessageType: item.MessageType,
				Status: item.Status, AttemptCount: item.AttemptCount, AvailableAt: item.AvailableAt,
				LeaseUntil: item.LeaseUntil, LastError: item.LastError, ExpiresAt: item.ExpiresAt,
				SentAt: item.SentAt, CreatedAt: item.CreatedAt,
			})
		}
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		return encoder.Encode(safeItems)
	case "requeue":
		if len(args) != 2 {
			return fmt.Errorf("usage: email-outbox-admin requeue <id>")
		}
		applied, err := repository.RequeueEmailOutbox(ctx, args[1], time.Now().UTC())
		if err != nil {
			return err
		}
		if !applied {
			return fmt.Errorf("email is not eligible for requeue")
		}
		fmt.Fprintln(os.Stdout, "requeued")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

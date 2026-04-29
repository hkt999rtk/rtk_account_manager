package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"rtk_account_manager/internal/config"
	"rtk_account_manager/internal/database"
	"rtk_account_manager/internal/model"
	"rtk_account_manager/internal/store"
)

type usageError struct {
	message string
}

func (e usageError) Error() string {
	return e.message
}

type outboxListOutput struct {
	Statuses []string                    `json:"statuses"`
	Limit    int                         `json:"limit"`
	Offset   int                         `json:"offset"`
	Messages []model.DeviceMessageOutbox `json:"messages"`
}

type inboxListOutput struct {
	Statuses []string                   `json:"statuses"`
	Limit    int                        `json:"limit"`
	Offset   int                        `json:"offset"`
	Messages []model.DeviceMessageInbox `json:"messages"`
}

type outboxRequeueOutput struct {
	Changed   bool                      `json:"changed"`
	Message   model.DeviceMessageOutbox `json:"message"`
	Operation *model.DeviceOperation    `json:"operation,omitempty"`
}

type inboxRequeueOutput struct {
	Changed   bool                     `json:"changed"`
	Message   model.DeviceMessageInbox `json:"message"`
	Operation *model.DeviceOperation   `json:"operation,omitempty"`
	Note      string                   `json:"note,omitempty"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if _, ok := err.(usageError); ok {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return usageError{message: usageText()}
	}

	cfg, err := config.LoadWorker()
	if err != nil {
		return err
	}

	ctx := context.Background()
	db, err := database.Connect(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer db.Close()

	messageStore := store.New(db)

	scope := args[0]
	command := args[1]
	rest := args[2:]

	switch scope {
	case "outbox":
		return runOutboxCommand(ctx, messageStore, command, rest, stdout, stderr)
	case "inbox":
		return runInboxCommand(ctx, messageStore, command, rest, stdout, stderr)
	default:
		return usageError{message: fmt.Sprintf("unknown scope %q\n\n%s", scope, usageText())}
	}
}

func runOutboxCommand(ctx context.Context, messageStore *store.Store, command string, args []string, stdout, stderr io.Writer) error {
	switch command {
	case "list":
		flags := flag.NewFlagSet("outbox list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statusesArg := flags.String("status", "retrying,dead_lettered", "comma-separated outbox statuses")
		limit := flags.Int("limit", 20, "max rows to return")
		offset := flags.Int("offset", 0, "rows to skip before listing")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if *limit < 0 || *offset < 0 {
			return usageError{message: "outbox list requires non-negative -limit and -offset"}
		}

		statuses, err := parseOutboxStatuses(*statusesArg)
		if err != nil {
			return usageError{message: err.Error()}
		}

		messages, err := messageStore.ListOutboxMessagesByStatus(ctx, statuses, *limit, *offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, outboxListOutput{
			Statuses: outboxStatusStrings(statuses),
			Limit:    *limit,
			Offset:   *offset,
			Messages: messages,
		})
	case "show":
		flags := flag.NewFlagSet("outbox show", flag.ContinueOnError)
		flags.SetOutput(stderr)
		messageID := flags.String("message-id", "", "outbox message id")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if strings.TrimSpace(*messageID) == "" {
			return usageError{message: "outbox show requires -message-id"}
		}

		detail, err := messageStore.GetOutboxMessageDetail(ctx, *messageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("outbox message %q not found", *messageID)
			}
			return err
		}
		return writeJSON(stdout, detail)
	case "requeue":
		flags := flag.NewFlagSet("outbox requeue", flag.ContinueOnError)
		flags.SetOutput(stderr)
		messageID := flags.String("message-id", "", "outbox message id")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if strings.TrimSpace(*messageID) == "" {
			return usageError{message: "outbox requeue requires -message-id"}
		}

		message, operation, changed, err := messageStore.RequeueOutboxMessage(ctx, *messageID, time.Now().UTC())
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("outbox message %q not found", *messageID)
			}
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("outbox message %q is not eligible for requeue or its related lifecycle operation already completed", *messageID)
			}
			return err
		}
		return writeJSON(stdout, outboxRequeueOutput{
			Changed:   changed,
			Message:   message,
			Operation: operation,
		})
	default:
		return usageError{message: fmt.Sprintf("unknown outbox command %q\n\n%s", command, usageText())}
	}
}

func runInboxCommand(ctx context.Context, messageStore *store.Store, command string, args []string, stdout, stderr io.Writer) error {
	switch command {
	case "list":
		flags := flag.NewFlagSet("inbox list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		statusesArg := flags.String("status", "retrying,dead_lettered", "comma-separated inbox statuses")
		limit := flags.Int("limit", 20, "max rows to return")
		offset := flags.Int("offset", 0, "rows to skip before listing")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if *limit < 0 || *offset < 0 {
			return usageError{message: "inbox list requires non-negative -limit and -offset"}
		}

		statuses, err := parseInboxStatuses(*statusesArg)
		if err != nil {
			return usageError{message: err.Error()}
		}

		messages, err := messageStore.ListInboxMessagesByStatus(ctx, statuses, *limit, *offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, inboxListOutput{
			Statuses: inboxStatusStrings(statuses),
			Limit:    *limit,
			Offset:   *offset,
			Messages: messages,
		})
	case "show":
		flags := flag.NewFlagSet("inbox show", flag.ContinueOnError)
		flags.SetOutput(stderr)
		messageID := flags.String("message-id", "", "inbox message id")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if strings.TrimSpace(*messageID) == "" {
			return usageError{message: "inbox show requires -message-id"}
		}

		detail, err := messageStore.GetInboxMessageDetail(ctx, *messageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("inbox message %q not found", *messageID)
			}
			return err
		}
		return writeJSON(stdout, detail)
	case "requeue":
		flags := flag.NewFlagSet("inbox requeue", flag.ContinueOnError)
		flags.SetOutput(stderr)
		messageID := flags.String("message-id", "", "inbox message id")
		if err := flags.Parse(args); err != nil {
			return usageError{message: err.Error()}
		}
		if strings.TrimSpace(*messageID) == "" {
			return usageError{message: "inbox requeue requires -message-id"}
		}

		message, operation, changed, err := messageStore.RequeueInboxMessage(ctx, *messageID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("inbox message %q not found", *messageID)
			}
			if errors.Is(err, store.ErrConflict) {
				return fmt.Errorf("inbox message %q is not eligible for requeue; only retrying or dead-lettered rows can be reopened", *messageID)
			}
			return err
		}
		return writeJSON(stdout, inboxRequeueOutput{
			Changed:   changed,
			Message:   message,
			Operation: operation,
			Note:      "inbox requeue reopens the persisted dedupe row; acknowledged broker messages still require replay or redelivery to be processed again",
		})
	default:
		return usageError{message: fmt.Sprintf("unknown inbox command %q\n\n%s", command, usageText())}
	}
}

func parseOutboxStatuses(raw string) ([]model.DeviceMessageOutboxStatus, error) {
	parts := splitCSV(raw)
	if len(parts) == 0 {
		return nil, errors.New("at least one outbox status is required")
	}

	statuses := make([]model.DeviceMessageOutboxStatus, 0, len(parts))
	for _, part := range parts {
		status := model.DeviceMessageOutboxStatus(part)
		switch status {
		case model.DeviceMessageOutboxStatusPending,
			model.DeviceMessageOutboxStatusPublished,
			model.DeviceMessageOutboxStatusRetrying,
			model.DeviceMessageOutboxStatusDeadLettered:
			statuses = append(statuses, status)
		default:
			return nil, fmt.Errorf("unsupported outbox status %q", part)
		}
	}
	return statuses, nil
}

func parseInboxStatuses(raw string) ([]model.DeviceMessageInboxStatus, error) {
	parts := splitCSV(raw)
	if len(parts) == 0 {
		return nil, errors.New("at least one inbox status is required")
	}

	statuses := make([]model.DeviceMessageInboxStatus, 0, len(parts))
	for _, part := range parts {
		status := model.DeviceMessageInboxStatus(part)
		switch status {
		case model.DeviceMessageInboxStatusProcessed,
			model.DeviceMessageInboxStatusFailed,
			model.DeviceMessageInboxStatusRetrying,
			model.DeviceMessageInboxStatusDeadLettered:
			statuses = append(statuses, status)
		default:
			return nil, fmt.Errorf("unsupported inbox status %q", part)
		}
	}
	return statuses, nil
}

func splitCSV(raw string) []string {
	fields := strings.Split(raw, ",")
	parts := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		parts = append(parts, field)
	}
	return parts
}

func outboxStatusStrings(statuses []model.DeviceMessageOutboxStatus) []string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	return values
}

func inboxStatusStrings(statuses []model.DeviceMessageInboxStatus) []string {
	values := make([]string, 0, len(statuses))
	for _, status := range statuses {
		values = append(values, string(status))
	}
	return values
}

func writeJSON(w io.Writer, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	_, err = w.Write(encoded)
	return err
}

func usageText() string {
	return `usage:
  lifecycle-admin outbox list [-status retrying,dead_lettered] [-limit 20] [-offset 0]
  lifecycle-admin outbox show -message-id <id>
  lifecycle-admin outbox requeue -message-id <id>
  lifecycle-admin inbox list [-status retrying,dead_lettered] [-limit 20] [-offset 0]
  lifecycle-admin inbox show -message-id <id>
  lifecycle-admin inbox requeue -message-id <id>`
}

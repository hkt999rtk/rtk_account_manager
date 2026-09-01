package billingbootstrap

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
	"rtk_account_manager/internal/store"
)

type eventStore interface {
	ClaimBillingCloudCreations(context.Context) ([]store.BillingCloudCreationJob, error)
	FinishBillingCloudCreation(context.Context, store.BillingCloudCreationJob, *store.BillingCloudCreationReceipt) (bool, error)
}
type Receiver interface {
	Bootstrap(context.Context, store.BillingCloudCreation) (store.BillingCloudCreationReceipt, error)
}
type Worker struct {
	store    eventStore
	receiver Receiver
	logger   *zap.Logger
}

func NewWorker(s eventStore, r Receiver, l *zap.Logger) (*Worker, error) {
	if s == nil || r == nil {
		return nil, ErrInvalid
	}
	if l == nil {
		l = zap.NewNop()
	}
	return &Worker{s, r, l}, nil
}
func (w *Worker) RunOnce(ctx context.Context) (int, error) {
	jobs, err := w.store.ClaimBillingCloudCreations(ctx)
	if err != nil {
		return 0, err
	}
	delivered := 0
	for _, job := range jobs {
		if err := ctx.Err(); err != nil {
			return delivered, err
		}
		receipt, err := w.receiver.Bootstrap(ctx, job.BillingCloudCreation)
		var proof *store.BillingCloudCreationReceipt
		if err == nil {
			proof = &receipt
		}
		applied, err := w.store.FinishBillingCloudCreation(ctx, job, proof)
		if err != nil {
			return delivered, err
		}
		if applied && proof != nil {
			delivered++
		}
	}
	return delivered, nil
}
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		batch, cancel := context.WithTimeout(ctx, time.Minute)
		_, err := w.RunOnce(batch)
		cancel()
		if err != nil && !errors.Is(err, context.Canceled) {
			w.logger.Warn("Billing cloud creation batch incomplete; durable events retained")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

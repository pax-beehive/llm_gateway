// Package providerattempt coordinates the terminal financial state of one
// bounded Provider execution without widening the capability-specific Provider
// interfaces.
package providerattempt

import (
	"context"
	"errors"
	"sync"

	"github.com/toddzheng/llm-gateway/internal/quota"
)

var ErrAlreadySettled = errors.New("Provider Attempt quota reservation is already settled")

type Attempt struct {
	controller        quota.Controller
	reservationID     string
	mu                sync.Mutex
	settled           bool
	sideEffectStarted bool
	usagePersisted    bool
}

func New(controller quota.Controller, reservation *quota.Reservation) *Attempt {
	if controller == nil || reservation == nil {
		return nil
	}
	return &Attempt{controller: controller, reservationID: reservation.ID}
}

func (attempt *Attempt) ReservationID() string {
	if attempt == nil {
		return ""
	}
	return attempt.reservationID
}

func (attempt *Attempt) Settled() bool {
	if attempt == nil {
		return true
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.settled
}

func (attempt *Attempt) MarkSideEffectStarted() {
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	attempt.sideEffectStarted = true
	attempt.mu.Unlock()
}

func (attempt *Attempt) SideEffectStarted() bool {
	if attempt == nil {
		return false
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.sideEffectStarted
}

func (attempt *Attempt) MarkUsagePersisted() {
	if attempt == nil {
		return
	}
	attempt.mu.Lock()
	attempt.usagePersisted = true
	attempt.mu.Unlock()
}

func (attempt *Attempt) UsagePersisted() bool {
	if attempt == nil {
		return false
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.usagePersisted
}

func (attempt *Attempt) Commit(ctx context.Context, actual quota.ActualUsage) error {
	return attempt.settle(func() error { return attempt.controller.Commit(ctx, attempt.reservationID, actual) })
}

func (attempt *Attempt) Release(ctx context.Context) error {
	return attempt.settle(func() error { return attempt.controller.Release(ctx, attempt.reservationID) })
}

func (attempt *Attempt) Uncertain(ctx context.Context) error {
	return attempt.settle(func() error { return attempt.controller.Uncertain(ctx, attempt.reservationID) })
}

func (attempt *Attempt) settle(settle func() error) error {
	if attempt == nil {
		return nil
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.settled {
		return ErrAlreadySettled
	}
	if err := settle(); err != nil {
		return err
	}
	attempt.settled = true
	return nil
}

package providerattempt_test

import (
	"context"
	"errors"
	"testing"

	"github.com/toddzheng/llm-gateway/internal/providerattempt"
	"github.com/toddzheng/llm-gateway/internal/quota"
)

type controller struct{ committed, released, uncertain int }

func (c *controller) Reserve(context.Context, quota.ReservationRequest) (quota.Reservation, error) {
	return quota.Reservation{}, nil
}
func (c *controller) ReserveRefresh(context.Context, quota.RefreshReservationRequest) (quota.Reservation, error) {
	return quota.Reservation{}, nil
}
func (c *controller) Commit(context.Context, string, quota.ActualUsage) error {
	c.committed++
	return nil
}
func (c *controller) Release(context.Context, string) error       { c.released++; return nil }
func (c *controller) Uncertain(context.Context, string) error     { c.uncertain++; return nil }
func (c *controller) Reconcile(context.Context, int) (int, error) { return 0, nil }

func TestAttemptSettlesExactlyOnce(t *testing.T) {
	c := &controller{}
	attempt := providerattempt.New(c, &quota.Reservation{ID: "quota-1"})
	attempt.MarkSideEffectStarted()
	attempt.MarkUsagePersisted()
	if err := attempt.Commit(context.Background(), quota.ActualUsage{Requests: 1}); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Release(context.Background()); !errors.Is(err, providerattempt.ErrAlreadySettled) {
		t.Fatalf("second settlement = %v", err)
	}
	if !attempt.Settled() || !attempt.SideEffectStarted() || !attempt.UsagePersisted() || attempt.ReservationID() != "quota-1" || c.committed != 1 || c.released != 0 {
		t.Fatalf("attempt state is inconsistent: %#v", c)
	}
}

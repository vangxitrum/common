package apierr_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"gitlab.internal/tuan.quang.tran/aioz-template/internal/utils/apierr"
)

// The recipes in the README's "Errors" section, verbatim, so documentation
// that stops compiling fails the build instead of quietly misleading whoever
// copies it. It also puts the documented slugs in front of the codebase-wide
// reason policy test, so the examples cannot teach a rule they break.

var errStoreNotFound = errors.New("not found")

type contractStore interface {
	GetByID(context.Context, string) (any, error)
}

type payments interface {
	Charge(context.Context, int) error
}

type doc struct {
	contracts     contractStore
	payments      payments
	maxNameLength int
}

// fakeStore returns whatever error it is given.
type fakeStore struct{ err error }

func (f fakeStore) GetByID(context.Context, string) (any, error) { return nil, f.err }

type fakePayments struct{ err error }

func (f fakePayments) Charge(context.Context, int) error { return f.err }

func (s doc) validation(name string) error {
	if len(name) > s.maxNameLength {
		return apierr.BadRequest("name-too-long",
			"name must be at most %d bytes", s.maxNameLength).
			With("limit", s.maxNameLength, "got", len(name))
	}
	return nil
}

func (s doc) missingRecord(ctx context.Context, id string) error {
	_, err := s.contracts.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, errStoreNotFound) {
			return apierr.NotFound("contract-not-found", "no such contract").
				With("resource", "contract", "id", id)
		}
		return apierr.Internal("contract-load-failed", err, "id", id)
	}
	return nil
}

func (s doc) dependency(ctx context.Context, amount int, accountID string) error {
	if err := s.payments.Charge(ctx, amount); err != nil {
		return apierr.Wrapf("charge-failed", http.StatusBadGateway, err,
			"charging %s", accountID)
	}
	return nil
}

func (s doc) nilSafe(err error) error {
	return apierr.Wrap("contract-save-failed", http.StatusInternalServerError, err)
}

func TestREADMERecipesCompileAndBehave(t *testing.T) {
	t.Parallel()

	s := doc{maxNameLength: 64}

	err := s.validation("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if apierr.Status(err) != http.StatusBadRequest ||
		apierr.Reason(err) != "name-too-long" ||
		len(apierr.Fields(err)) != 4 {
		t.Fatalf("validation recipe: %v", err)
	}

	if got := s.nilSafe(nil); got != nil {
		t.Fatalf("Wrap must pass nil through, got %v", got)
	}

	// The reading-an-error list.
	_ = apierr.Status(err)
	_ = apierr.Reason(err)
	_ = apierr.Fields(err)
	_ = apierr.Tags(err)
	_ = apierr.Trace(err)

	ctx := context.Background()

	// The missing-record recipe: the two branches must produce different
	// reasons, which is the whole point of the example.
	missing := doc{contracts: fakeStore{err: errStoreNotFound}}
	err = missing.missingRecord(ctx, "c-42")
	if apierr.Status(err) != http.StatusNotFound ||
		apierr.Reason(err) != "contract-not-found" {
		t.Fatalf("missing-record recipe: %v", err)
	}

	broken := doc{contracts: fakeStore{err: errors.New("connection refused")}}
	err = broken.missingRecord(ctx, "c-42")
	if apierr.Status(err) != http.StatusInternalServerError ||
		apierr.Reason(err) != "contract-load-failed" {
		t.Fatalf("missing-record fault branch: %v", err)
	}

	// The dependency recipe: Wrap must keep the cause reachable.
	cause := errors.New("gateway timeout")
	failing := doc{payments: fakePayments{err: cause}}
	err = failing.dependency(ctx, 100, "acct-1")
	if !errors.Is(err, cause) {
		t.Fatalf("Wrapf must keep errors.Is working: %v", err)
	}
	if apierr.Status(err) != http.StatusBadGateway {
		t.Fatalf("dependency recipe status: %d", apierr.Status(err))
	}

	// And the happy path returns nil.
	ok := doc{contracts: fakeStore{}, payments: fakePayments{}}
	if err := ok.missingRecord(ctx, "c-1"); err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if err := ok.dependency(ctx, 1, "acct-1"); err != nil {
		t.Fatalf("happy path: %v", err)
	}
}

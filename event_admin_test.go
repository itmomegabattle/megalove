package main

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventSelectionKeepsBallotsSeparate(t *testing.T) {
	db := ballotTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	v := votingEvent{db: db, slug: "demo"}
	var second int64
	if err := db.QueryRowContext(ctx, "INSERT INTO events (slug, name) VALUES ('second', 'Second event') RETURNING id").Scan(&second); err != nil {
		t.Fatal(err)
	}
	change := func(from, to string) {
		t.Helper()
		if err := v.transition(ctx, from, to); err != nil {
			t.Fatal(err)
		}
	}
	choose := func(eventID int64) {
		t.Helper()
		if err := v.activateEvent(ctx, eventID); err != nil {
			t.Fatal(err)
		}
	}
	const browser = "11111111-1111-4111-8111-111111111111"
	change("preparing", "open")
	if err := v.acceptBallotForEvent(ctx, "demo/1", browser, ballotChoices{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if err := v.activateEvent(ctx, second); !errors.Is(err, errActiveEventOpen) {
		t.Fatalf("switching during open vote: %v", err)
	}
	if err := v.removeEvent(ctx, 1, "Мегабаттл — демонстрация"); !errors.Is(err, errActiveEventOpen) {
		t.Fatalf("deleting an open event: %v", err)
	}
	change("open", "closed")
	choose(second)
	page := httptest.NewRecorder()
	v.dashboard(page, httptest.NewRequest("GET", "/admin", nil))
	for _, want := range []string{"admin-event-list", "data-current", "event-start-1", "event-delete-1", "event-create", "Точно удалить?", "Second event"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), want) {
			t.Fatalf("dashboard missing %q (HTTP %d)", want, page.Code)
		}
	}
	view, err := v.calculateResults(ctx)
	if err != nil || view.Event.ID != second || view.Ballots != 0 || view.Participants != 0 {
		t.Fatalf("new event must start empty: %+v, %v", view, err)
	}
	if err := v.transitionForEvent(ctx, 1, "preparing", "open"); !errors.Is(err, errVotingTransition) {
		t.Fatalf("stale admin form: %v", err)
	}
	if err := v.acceptBallotForEvent(ctx, "demo/1", browser, ballotChoices{1, 2, 3}); !errors.Is(err, errBallotEventChanged) {
		t.Fatalf("stale browser ballot: %v", err)
	}
	choose(1)
	view, err = v.calculateResults(ctx)
	if err != nil || view.Event.State != "closed" || view.Ballots != 1 || !view.Verified {
		t.Fatalf("old event's closed results lost: %+v, %v", view, err)
	}
	if err := v.removeEvent(ctx, 1, "wrong"); !errors.Is(err, errDeleteConfirmation) {
		t.Fatalf("bad confirmation: %v", err)
	}
	if err := v.removeEvent(ctx, 1, view.Event.Name); err != nil {
		t.Fatal(err)
	}
	events, err := v.eventList(ctx)
	if err != nil || len(events) != 1 || events[0].ID != second {
		t.Fatalf("other event was not preserved: %+v, %v", events, err)
	}
	choose(second)
}

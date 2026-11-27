package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

type photoTransport func(*http.Request) (*http.Response, error)

func (f photoTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRemoveParticipantBeforeOpening(t *testing.T) {
	db := ballotTestDB(t)
	admin := participantAdmin{db}
	remove := func(id string) int {
		r := httptest.NewRequest(http.MethodPost, "/", nil)
		r.SetPathValue("event", "1")
		r.SetPathValue("person", id)
		w := httptest.NewRecorder()
		admin.remove(w, r)
		return w.Code
	}
	if got := remove("1"); got != http.StatusSeeOther {
		t.Fatalf("remove: %d", got)
	}
	if got := remove("1"); got != http.StatusNotFound {
		t.Fatalf("remove missing: %d", got)
	}
	if _, err := db.Exec("UPDATE events SET state='open' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if got := remove("2"); got != http.StatusConflict {
		t.Fatalf("remove during voting: %d", got)
	}
}

func TestEditParticipantAfterVotingPreservesBallots(t *testing.T) {
	db := ballotTestDB(t)
	ctx := context.Background()
	v := votingEvent{db: db, slug: "demo"}
	a := participantAdmin{db}
	p := participant{ID: 1, Name: "Исправленное имя", Faculty: "tint", Dancer: true, Actor: true, Character: "Герой"}
	if err := v.transition(ctx, "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	if err := a.write(ctx, 1, &p, nil); !errors.Is(err, errParticipantLocked) {
		t.Fatalf("open event allowed edit: %v", err)
	}
	if err := v.acceptBallot(ctx, "11111111-1111-4111-8111-111111111111", ballotChoices{1, 2, 3}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE events SET voting_ends_at=clock_timestamp()-interval '1 second' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err := a.write(ctx, 1, &p, nil); err != nil {
		t.Fatal(err)
	}
	p.Dancer = false
	p.Name = "Имя после прогона"
	if err := a.write(ctx, 1, &p, nil); err != nil {
		t.Fatalf("voted nomination removal: %v", err)
	}
	result, err := v.calculateResults(ctx)
	if err != nil || !result.Verified || result.Ballots != 1 || result.Nominations[0].Candidates[0].Name != "Анна Волкова" {
		t.Fatalf("result after edit: %+v, %v", result, err)
	}
}

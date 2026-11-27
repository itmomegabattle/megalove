package main

import (
	"context"
	"errors"
	"testing"
)

func TestRehearsalsAndConcertUseIndependentRostersAndBallots(t *testing.T) {
	db := ballotTestDB(t)
	ctx := context.Background()
	concert := votingEvent{db: db, slug: "demo"}
	rehearsal := concert
	rehearsal.testRun = true
	const browser = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	vote := ballotChoices{1, 2, 3}
	step := func(from, to, mode string) {
		t.Helper()
		if err := concert.transitionRun(ctx, 1, from, to, 6, mode); err != nil {
			t.Fatal(err)
		}
	}
	step("preparing", "open", "new-test")
	if err := concert.acceptBallot(ctx, browser, vote); !errors.Is(err, errVotingClosed) {
		t.Fatalf("concert before start: %v", err)
	}
	if err := rehearsal.acceptBallotForEvent(ctx, "demo/1", browser, vote); err != nil {
		t.Fatal(err)
	}
	step("open", "closed", "test")
	step("closed", "open", "new-test")
	if err := rehearsal.acceptBallotForEvent(ctx, "demo/1", browser, vote); !errors.Is(err, errBallotEventChanged) {
		t.Fatalf("stale test: %v", err)
	}
	step("open", "closed", "test")
	admin := participantAdmin{db}
	candidate := participant{ID: 1, Name: "Новое имя", Faculty: "ktu", Dancer: true, Actor: true}
	if err := admin.write(ctx, 1, &candidate, nil); err != nil {
		t.Fatal(err)
	}
	step("closed", "open", "main")
	if err := concert.acceptBallotForEvent(ctx, "demo/3", browser, vote); err != nil {
		t.Fatal(err)
	}
	step("open", "closed", "main")
	step("closed", "open", "main")
	if err := concert.acceptBallotForEvent(ctx, "demo/3", browser, vote); err != nil {
		t.Fatal(err)
	}
	step("open", "closed", "main")
	for i, want := range []int64{1, 0, 1} {
		r, err := concert.resultsForRun(ctx, int64(i+1))
		if err != nil || !r.Verified || r.Ballots != want {
			t.Fatalf("run %d count=%d verified=%t err=%v", i+1, r.Ballots, r.Verified, err)
		}
		if i != 1 && (r.Nominations[0].Candidates[0].Name == candidate.Name) != (i == 2) {
			t.Fatal("historical roster changed")
		}
	}
}

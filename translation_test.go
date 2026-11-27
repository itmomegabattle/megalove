package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"testing"
)

func TestTranslationRanksBestCandidatePerFaculty(t *testing.T) {
	db := ballotTestDB(t)
	v := votingEvent{db: db, slug: "demo"}
	ctx := context.Background()
	if _, err := db.Exec(`
		WITH added AS (
			INSERT INTO participants (event_id,faculty_code,display_name)
			SELECT 1,'tint','Танцор ' || i FROM generate_series(1,5) AS i RETURNING id
		)
		INSERT INTO candidacies (event_id,participant_id,nomination)
		SELECT 1,id,'dancer' FROM added`); err != nil {
		t.Fatal(err)
	}
	ranking := func(mode string) map[string][]translationPlace {
		t.Helper()
		w := httptest.NewRecorder()
		v.translationRanks(w, httptest.NewRequest("GET", "/api/translation?mode="+mode, nil))
		var data map[string][]translationPlace
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &data) != nil || len(data) != 3 || len(data["dancer"]) != 5 {
			t.Fatalf("translation ranks: %d %s", w.Code, w.Body.String())
		}
		return data
	}

	if err := v.transitionRun(ctx, 1, "preparing", "open", 6, "new-test"); err != nil {
		t.Fatal(err)
	}
	rehearsal := v
	rehearsal.testRun = true
	if err := rehearsal.acceptBallot(ctx, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", ballotChoices{6, 4, 5}); err != nil {
		t.Fatal(err)
	}
	if ranking("main")["dancer"][0].Place != 0 || ranking("main")["dancer"][0].TotalVotes != 0 {
		t.Fatal("test ballots appeared in the main ranking")
	}
	if err := v.transitionRun(ctx, 1, "open", "closed", 6, "test"); err != nil {
		t.Fatal(err)
	}
	if err := v.transitionRun(ctx, 1, "closed", "open", 6, "main"); err != nil {
		t.Fatal(err)
	}
	// КТУ has three votes for one dancer. ТИНТ has four split between two dancers.
	for index, dancer := range []int64{1, 1, 1, 6, 6, 10, 10} {
		id := fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", index)
		if err := v.acceptBallot(ctx, id, ballotChoices{dancer, 2, 3}); err != nil {
			t.Fatal(err)
		}
	}
	if ranks := ranking("main")["dancer"]; ranks[0] != (translationPlace{"КТУ", 1, 3}) || ranks[1] != (translationPlace{"ТИНТ", 2, 4}) {
		t.Fatalf("unexpected faculty order: %+v", ranks)
	}
	topFor := func(nomination, mode string) []translationCandidate {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/api/translation/"+nomination+"?mode="+mode, nil)
		r.SetPathValue("nomination", nomination)
		v.translationTopCandidates(w, r)
		var top []translationCandidate
		if err := json.Unmarshal(w.Body.Bytes(), &top); w.Code != 200 || err != nil || len(top) == 0 {
			t.Fatalf("candidate ranking %s %s: %d %s", nomination, mode, w.Code, w.Body)
		}
		return top
	}
	top := topFor("dancer", "main")
	if len(top) != 5 || top[0] != (translationCandidate{1, "Анна Волкова", 3}) ||
		top[1] != (translationCandidate{6, "Танцор 1", 2}) ||
		top[2] != (translationCandidate{10, "Танцор 5", 2}) {
		t.Fatalf("unexpected candidate order: %+v", top)
	}
	for _, tc := range []struct {
		nomination, faculty string
		test, main          translationCandidate
	}{
		{"dancer", "ТИНТ", translationCandidate{6, "Танцор 1", 1}, top[0]},
		{"vocalist", "ФТМФ", translationCandidate{4, "Илья Котов", 1}, translationCandidate{2, "Полина Егорова", 7}},
		{"actor", "ФТМИ", translationCandidate{5, "Мария Белова", 1}, translationCandidate{3, "Дарья Лебедева", 7}},
	} {
		if topFor(tc.nomination, "test")[0] != tc.test || topFor(tc.nomination, "main")[0] != tc.main {
			t.Fatalf("mixed vote modes in %s", tc.nomination)
		}
		if got := ranking("test")[tc.nomination][0]; got.Faculty != tc.faculty || got.TotalVotes != 1 {
			t.Fatalf("wrong test faculty ranking for %s: %+v", tc.nomination, got)
		}
	}
}

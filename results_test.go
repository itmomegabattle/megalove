package main

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestResultsTiesAndExport(t *testing.T) {
	db := ballotTestDB(t)
	v := votingEvent{db: db, slug: "demo"}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	ranking := func() resultsView {
		t.Helper()
		view, err := v.calculateResults(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return view
	}
	get := func(handler http.HandlerFunc) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(ctx, "GET", "https://vote.example/admin/results", nil)
		w := httptest.NewRecorder()
		handler(w, r)
		return w
	}
	view := ranking()
	if view.Ballots != 0 || view.Verified {
		t.Fatalf("unexpected pre-vote result: %+v", view)
	}
	if w := get(v.resultsPage); w.Code != 200 || strings.Count(w.Body.String(), "Нет голосов") != 3 ||
		strings.Contains(w.Body.String(), "Лидеры:") {
		t.Fatal("no-vote results must not show leaders")
	}

	exec(`INSERT INTO candidacies(event_id, participant_id, nomination)
		SELECT event_id, id, 'dancer' FROM participants WHERE id IN (4, 5)`)
	if err := v.transition(ctx, "preparing", "open"); err != nil {
		t.Fatal(err)
	}
	for i, choices := range []ballotChoices{{1, 2, 3}, {4, 2, 5}, {1, 2, 5}, {4, 4, 3}} {
		browser := fmt.Sprintf("11111111-1111-4111-8111-%012d", i+1)
		if err := v.acceptBallot(ctx, browser, choices); err != nil {
			t.Fatal(err)
		}
	}
	if w := get(v.exportResults); w.Code != http.StatusConflict {
		t.Fatalf("live export returned %d", w.Code)
	}
	if err := v.transition(ctx, "open", "closed"); err != nil {
		t.Fatal(err)
	}
	view = ranking()
	if view.Ballots != 4 || !view.Verified {
		t.Fatalf("closed recount failed: %+v", view)
	}
	want := []map[int64]int64{{1: 2, 4: 2, 5: 0}, {2: 3, 4: 1}, {3: 2, 5: 2}}
	for i, group := range view.Nominations {
		if group.Votes != view.Ballots || len(group.Candidates) != len(want[i]) {
			t.Fatalf("wrong nomination total: %+v", group)
		}
		for _, candidate := range group.Candidates {
			votes, exists := want[i][candidate.ID]
			if !exists || votes != candidate.Votes || candidate.Leader != (votes > 0 && votes == group.Candidates[0].Votes) {
				t.Fatalf("wrong candidate ranking: %+v", candidate)
			}
			if (candidate.Leader && candidate.Place != 1) || (votes == 0 && candidate.Place != 0) {
				t.Fatalf("wrong place: %+v", candidate)
			}
		}
	}
	if w := get(v.resultsPage); w.Code != 200 || !strings.Contains(w.Body.String(), "Контрольный пересчёт совпал.") {
		t.Fatal("verified results not visible")
	}
	export := get(v.exportResults)
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(export.Body.String(), "\ufeff")))
	reader.Comma = ';'
	rows, err := reader.ReadAll()
	if export.Code != 200 || err != nil || len(rows) != 8 || rows[0][0] != "Номинация" {
		t.Fatalf("invalid export: %d, %v, %v", export.Code, rows, err)
	}
	leaders := 0
	for _, row := range rows[1:] {
		if row[6] == "Да" {
			leaders++
		}
		if row[7] != "4" {
			t.Fatalf("wrong export total: %v", row)
		}
	}
	if leaders != 5 {
		t.Fatalf("expected five joint and single leaders, got %d", leaders)
	}
}

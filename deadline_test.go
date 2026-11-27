package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"html"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeadlineAndVoteJournal(t *testing.T) {
	db := ballotTestDB(t)
	ctx := context.Background()
	v := votingEvent{db: db, slug: "demo"}
	if err := v.transitionTimed(ctx, 1, "preparing", "open", 6); err != nil {
		t.Fatal(err)
	}
	var seconds float64
	if err := db.QueryRow("SELECT extract(epoch FROM voting_ends_at-clock_timestamp()) FROM events WHERE id=1").Scan(&seconds); err != nil || seconds < 350 || seconds > 360 {
		t.Fatalf("deadline: %v, %v", seconds, err)
	}
	const browser = "11111111-1111-4111-8111-111111111111"
	vote := ballotChoices{1, 2, 3}
	if err := v.acceptBallot(ctx, browser, vote); err != nil {
		t.Fatal(err)
	}
	var acceptedAt time.Time
	if err := db.QueryRow("SELECT accepted_at FROM ballots WHERE event_id=1").Scan(&acceptedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE events SET voting_ends_at=clock_timestamp()-interval '1 second' WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	v = votingEvent{db: db, slug: "demo"}
	if err := v.acceptBallot(ctx, "22222222-2222-4222-8222-222222222222", vote); !errors.Is(err, errVotingClosed) {
		t.Fatalf("after deadline: %v", err)
	}
	if err := v.acceptBallot(ctx, browser, vote); err != nil {
		t.Fatal(err)
	}
	for _, endpoint := range []struct {
		name    string
		handler func(*httptest.ResponseRecorder)
		want    string
	}{
		{"journal", func(w *httptest.ResponseRecorder) { v.ballotJournal(w, httptest.NewRequest("GET", "/", nil)) }, "ballot_id,accepted_at"},
		{"chart", func(w *httptest.ResponseRecorder) { v.ballotTimeline(w, httptest.NewRequest("GET", "/", nil)) }, "timeline-chart"},
		{"backup", func(w *httptest.ResponseRecorder) { v.backupCandidates(w, httptest.NewRequest("GET", "/", nil)) }, "nomination,id,name,faculty"},
	} {
		w := httptest.NewRecorder()
		endpoint.handler(w)
		if w.Code != 200 || !strings.Contains(w.Body.String(), endpoint.want) {
			t.Fatalf("%s: %d %s", endpoint.name, w.Code, w.Body)
		}
		switch endpoint.name {
		case "journal":
			rows, err := csv.NewReader(strings.NewReader(w.Body.String())).ReadAll()
			if err != nil || len(rows) != 2 || len(rows[1]) != 5 || strings.Join(rows[1][2:], ",") != "1,2,3" {
				t.Fatalf("journal ballots: %v, %v", rows, err)
			}
			at, err := time.Parse(time.RFC3339Nano, rows[1][1])
			if err != nil || !at.Equal(acceptedAt) {
				t.Fatalf("journal timestamp: %v, %v", at, err)
			}
		case "chart":
			_, attr, _ := strings.Cut(w.Body.String(), `data-chart="`)
			raw, _, _ := strings.Cut(attr, `"`)
			var chart struct{ Counts []map[int64]map[int64]int }
			if err := json.Unmarshal([]byte(html.UnescapeString(raw)), &chart); err != nil || len(chart.Counts) != 3 {
				t.Fatalf("chart data: %+v, %v", chart, err)
			}
			for i, counts := range chart.Counts {
				seconds := counts[int64(i+1)]
				if len(counts) != 1 || len(seconds) != 1 || seconds[acceptedAt.Unix()] != 1 {
					t.Fatalf("chart counts for nomination %d: %v", i, counts)
				}
			}
		}
	}
	if err := v.transition(ctx, "closed", "open"); err != nil {
		t.Fatal(err)
	}
	if err := v.transition(ctx, "open", "closed"); err != nil {
		t.Fatal(err)
	}
}

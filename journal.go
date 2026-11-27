package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

type journalBallot struct {
	ID, Dancer, Vocalist, Actor int64
	At                          time.Time
}

func readJournal(ctx context.Context, tx *sql.Tx, id int64, limit int) ([]journalBallot, error) {
	var count any
	if limit > 0 {
		count = limit
	}
	rows, err := tx.QueryContext(ctx, `SELECT journal_id, dancer_id, vocalist_id, actor_id, accepted_at
 FROM ballots WHERE run_id=$1 ORDER BY accepted_at DESC,journal_id DESC LIMIT $2`, id, count)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var journal []journalBallot
	for rows.Next() {
		var b journalBallot
		if err := rows.Scan(&b.ID, &b.Dancer, &b.Vocalist, &b.Actor, &b.At); err != nil {
			return nil, err
		}
		b.At = b.At.In(time.FixedZone("МСК", 10800))
		journal = append(journal, b)
	}
	return journal, rows.Err()
}

func (v votingEvent) closedJournal(w http.ResponseWriter, r *http.Request) (resultsView, []journalBallot, bool) {
	view, err := v.resultsForRun(r.Context(), requestedRun(r))
	if err != nil {
		participantError(w, err)
		return view, nil, false
	}
	if view.Event.State != "closed" || !view.Verified {
		http.Error(w, "Доступно после закрытия и сверки голосования.", 409)
		return view, nil, false
	}
	tx, err := v.db.BeginTx(r.Context(), &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		participantError(w, err)
		return view, nil, false
	}
	defer tx.Rollback()
	ballots, err := readJournal(r.Context(), tx, view.RunID, 0)
	if err != nil {
		participantError(w, err)
		return view, nil, false
	}
	if int64(len(ballots)) != view.Ballots {
		http.Error(w, "Итоги изменились. Обнови страницу.", 409)
		return view, nil, false
	}
	return view, ballots, true
}

func (v votingEvent) ballotJournal(w http.ResponseWriter, r *http.Request) {
	_, ballots, ok := v.closedJournal(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="ballot-journal.csv"`)
	c := csv.NewWriter(w)
	c.Write([]string{"ballot_id", "accepted_at", "dancer_id", "vocalist_id", "actor_id"})
	for _, b := range ballots {
		c.Write([]string{strconv.FormatInt(b.ID, 10), b.At.Format(time.RFC3339Nano), strconv.FormatInt(b.Dancer, 10), strconv.FormatInt(b.Vocalist, 10), strconv.FormatInt(b.Actor, 10)})
	}
	c.Flush()
}

func (v votingEvent) ballotTimeline(w http.ResponseWriter, r *http.Request) {
	view, ballots, ok := v.closedJournal(w, r)
	if !ok {
		return
	}
	series := make([]map[int64]map[int64]int, 3)
	for i := range series {
		series[i] = make(map[int64]map[int64]int)
	}
	for _, b := range ballots {
		for i, id := range [3]int64{b.Dancer, b.Vocalist, b.Actor} {
			if series[i][id] == nil {
				series[i][id] = make(map[int64]int)
			}
			series[i][id][b.At.Unix()]++
		}
	}
	data, err := json.Marshal(struct {
		Results resultsView
		Counts  []map[int64]map[int64]int
	}{view, series})
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := resultsPages.ExecuteTemplate(w, "timeline", string(data)); err != nil {
		participantError(w, err)
	}
}

func (v votingEvent) backupCandidates(w http.ResponseWriter, r *http.Request) {
	view, err := v.calculateResults(r.Context())
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="backup-candidates.csv"`)
	c := csv.NewWriter(w)
	c.Write([]string{"nomination", "id", "name", "faculty"})
	for _, nom := range view.Nominations {
		for _, p := range nom.Candidates {
			c.Write([]string{nom.Code, strconv.FormatInt(p.ID, 10), p.Name, p.Faculty})
		}
	}
	c.Flush()
}

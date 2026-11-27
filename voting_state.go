package main

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"time"
)

//go:embed voting_state.html
var votingStateHTML string

var votingPages = template.Must(template.Must(participantPages.Clone()).
	Funcs(template.FuncMap{"stateLabel": stateLabel}).Parse(votingStateHTML))

type votingEvent struct {
	db        *sql.DB
	slug      string
	localHTTP bool
	testRun   bool
}

type votingChange struct {
	From, To, Time string
}

type votingView struct {
	Test    bool
	Event   eventInfo
	Changes []votingChange
	Error   string
}

func stateLabel(state string) string {
	switch state {
	case "open":
		return "Голосование открыто"
	case "closed":
		return "Голосование закрыто"
	default:
		return "Голосование ещё не открыто"
	}
}

func (v votingEvent) current(ctx context.Context) (eventInfo, error) {
	var event eventInfo
	err := v.db.QueryRowContext(ctx, "SELECT e.id, e.name, "+effectiveVotingState+" FROM events e WHERE "+activeEventFilter, v.slug).
		Scan(&event.ID, &event.Name, &event.State)
	return event, err
}

func (v votingEvent) home(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	event, err := v.current(r.Context())
	if err != nil {
		http.Error(w, "Голосование недоступно", http.StatusServiceUnavailable)
		return
	}
	err = v.db.QueryRowContext(r.Context(), "SELECT "+publicRunState+" FROM events e "+latestRunJoin+" WHERE e.id=$1", event.ID, v.kind()).Scan(&event.State)
	if err != nil {
		participantError(w, err)
		return
	}
	writeVotingPage(w, "voting-home", http.StatusOK, votingView{Event: event, Test: v.testRun})
}

func writeVotingPage(w http.ResponseWriter, page string, status int, view votingView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := votingPages.ExecuteTemplate(w, page, view); err != nil {
		log.Printf("voting template: %v", err)
	}
}

func (v votingEvent) adminPage(w http.ResponseWriter, r *http.Request) {
	v.showAdmin(w, r, http.StatusOK, "")
}

func (v votingEvent) showAdmin(w http.ResponseWriter, r *http.Request, status int, message string) {
	event, err := v.current(r.Context())
	if err != nil {
		participantError(w, err)
		return
	}
	rows, err := v.db.QueryContext(r.Context(), `
		SELECT from_state, to_state, changed_at FROM event_state_changes
		WHERE event_id = $1 ORDER BY id DESC LIMIT 30
	`, event.ID)
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	view := votingView{Event: event, Error: message}
	for rows.Next() {
		var change votingChange
		var at time.Time
		if err := rows.Scan(&change.From, &change.To, &at); err != nil {
			participantError(w, err)
			return
		}
		change.Time = at.In(time.FixedZone("МСК", 3*60*60)).Format("02.01.2006 15:04:05 MST")
		view.Changes = append(view.Changes, change)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	writeVotingPage(w, "voting-admin", status, view)
}

var errVotingTransition = errors.New("invalid voting transition")
var errVotingRoster = errors.New("empty nomination")

const effectiveVotingState = `CASE WHEN e.state = 'open' AND e.voting_ends_at <= clock_timestamp()
 THEN 'closed' ELSE e.state END`

func (v votingEvent) changeState(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать команду", http.StatusBadRequest)
		return
	}
	from, to := r.PostForm.Get("from"), r.PostForm.Get("to")
	var eventID int64
	if raw := r.PostForm.Get("event_id"); raw != "" {
		var err error
		eventID, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || eventID < 1 {
			http.Error(w, "Неверное мероприятие", http.StatusBadRequest)
			return
		}
	}
	minutes := 6
	if raw := r.PostForm.Get("minutes"); to == "open" && raw != "" {
		var err error
		minutes, err = strconv.Atoi(raw)
		if err != nil || minutes < 1 || minutes > 120 {
			http.Error(w, "Укажи длительность от 1 до 120 минут", http.StatusBadRequest)
			return
		}
	}
	revision := int64(-1)
	if raw := r.PostForm.Get("revision"); raw != "" {
		var err error
		if revision, err = strconv.ParseInt(raw, 10, 64); err != nil || revision < 0 {
			http.Error(w, "Обнови панель.", 400)
			return
		}
	}
	switch err := v.transitionAtRevision(r.Context(), eventID, from, to, minutes, r.PostForm.Get("mode"), revision); {
	case errors.Is(err, errVotingTransition):
		v.showDashboard(w, r, http.StatusConflict, "Состояние изменилось или переход недоступен. Проверь текущий статус.")
	case errors.Is(err, errVotingRoster):
		v.showDashboard(w, r, http.StatusConflict, "Добавь хотя бы одного участника в каждую номинацию перед открытием.")
	case err != nil:
		participantError(w, err)
	default:
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
	}
}

func (v votingEvent) transition(ctx context.Context, expected, target string) error {
	return v.transitionForEvent(ctx, 0, expected, target)
}

func (v votingEvent) transitionForEvent(ctx context.Context, eventID int64, expected, target string) error {
	return v.transitionTimed(ctx, eventID, expected, target, 6)
}

func (v votingEvent) transitionTimed(ctx context.Context, eventID int64, expected, target string, minutes int) error {
	return v.transitionRun(ctx, eventID, expected, target, minutes, "main")
}

func (v votingEvent) transitionRun(ctx context.Context, eventID int64, expected, target string, minutes int, mode string) error {
	return v.transitionAtRevision(ctx, eventID, expected, target, minutes, mode, -1)
}
func (v votingEvent) transitionAtRevision(ctx context.Context, eventID int64, expected, target string, minutes int, mode string, revision int64) error {
	if mode == "" {
		mode = "main"
	}
	if mode != "main" && mode != "test" && mode != "new-test" {
		return errVotingTransition
	}
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM active_voting_event WHERE singleton FOR SHARE"); err != nil {
		return err
	}
	var event eventInfo
	var backupActive bool
	err = tx.QueryRowContext(ctx, "SELECT e.id FROM events e WHERE "+activeEventFilter+" FOR UPDATE OF e", v.slug).
		Scan(&event.ID)
	if err != nil {
		return err
	}
	err = tx.QueryRowContext(ctx, "SELECT "+effectiveVotingState+", backup_activated_at IS NOT NULL FROM events e WHERE id=$1", event.ID).
		Scan(&event.State, &backupActive)
	if err != nil {
		return err
	}
	if revision >= 0 {
		var latest int64
		if err := tx.QueryRowContext(ctx, `SELECT coalesce(max(id),0) FROM event_state_changes WHERE event_id=$1`, event.ID).Scan(&latest); err != nil {
			return err
		}
		if latest != revision {
			return errVotingTransition
		}
	}
	if backupActive || (eventID != 0 && event.ID != eventID) || event.State != expected ||
		!((target == "open" && (expected == "preparing" || expected == "closed")) ||
			(target == "closed" && expected == "open")) {
		return errVotingTransition
	}
	if err := changeRun(ctx, tx, event.ID, mode, target, minutes); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET state = $2, registration_token = NULL,
		voting_ends_at = CASE WHEN $2 = 'open' THEN (SELECT deadline FROM voting_runs WHERE event_id=$1 AND closed_at IS NULL) ELSE NULL END
		WHERE id = $1`, event.ID, target); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO event_state_changes (event_id, from_state, to_state, changed_at)
		VALUES ($1, $2, $3, clock_timestamp())
	`, event.ID, expected, target); err != nil {
		return err
	}
	return tx.Commit()
}

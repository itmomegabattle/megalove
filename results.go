package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/csv"
	"errors"
	"html/template"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed results.html
var resultsHTML string

var resultsPages = template.Must(template.Must(votingPages.Clone()).Parse(resultsHTML))

type rankedCandidate struct {
	Recent        int64
	ID            int64
	Name, Faculty string
	Votes         int64
	Place         int
	Leader        bool
}

type nominationResult struct {
	Code, Title string
	Candidates  []rankedCandidate
	Votes       int64
}

type runOption struct {
	ID    int64
	Label string
}
type resultsView struct {
	Revision      int64
	EndsUnix      int64
	HasMain       bool
	RunID         int64
	Kind          string
	Runs          []runOption
	Deadline      string
	RecentBallots []journalBallot
	Event         eventInfo
	Ballots       int64
	Participants  int64
	Nominations   []nominationResult
	Updated       string
	Verified      bool
}

type dashboardView struct {
	LastBackup       string
	RosterChanges    int64
	HasTest          bool
	RegistrationKey  string
	RegistrationOpen bool
	Results          resultsView
	Error            string
	Events           []eventInfo
	NewEventID       int64
	EventNotice      string
	BackupActive     bool
	BackupURL        string
}

func (v votingEvent) dashboard(w http.ResponseWriter, r *http.Request) {
	v.showDashboard(w, r, http.StatusOK, "")
}

func (v votingEvent) showDashboard(w http.ResponseWriter, r *http.Request, status int, message string) {
	results, err := v.calculateResults(r.Context())
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		participantError(w, err)
		return
	}
	view := dashboardView{Results: results, Error: message}
	if r.URL.Query().Get("event_updated") == "1" {
		view.EventNotice = "Название сохранено."
	}
	if r.URL.Query().Get("event_deleted") == "1" {
		view.EventNotice = "Мероприятие удалено."
	}
	if results.Event.ID != 0 {
		var slug string
		err = v.db.QueryRowContext(r.Context(), "SELECT registration_key,registration_token IS NOT NULL,slug,backup_activated_at IS NOT NULL FROM events WHERE id=$1", results.Event.ID).Scan(&view.RegistrationKey, &view.RegistrationOpen, &slug, &view.BackupActive)
		if err != nil {
			participantError(w, err)
			return
		}
		err = v.db.QueryRowContext(r.Context(), `SELECT coalesce((SELECT to_char(snapshot_at AT TIME ZONE 'Europe/Moscow','DD.MM HH24:MI') FROM backup_receipts WHERE singleton),'')`).Scan(&view.LastBackup)
		if err != nil {
			participantError(w, err)
			return
		}
		err = v.db.QueryRowContext(r.Context(), `WITH last_test AS (SELECT id FROM voting_runs WHERE event_id=$1 AND kind='test' ORDER BY id DESC LIMIT 1),
        old AS (SELECT participant_id,display_name,faculty_code,md5(photo_jpeg) AS photo,
            (SELECT jsonb_object_agg(nomination,character_name) FROM run_candidacies c WHERE c.run_id=p.run_id AND c.participant_id=p.participant_id) roles
            FROM run_participants p WHERE run_id=(SELECT id FROM last_test)),
        current AS (SELECT id,display_name,faculty_code,md5(photo_jpeg) AS photo,
            (SELECT jsonb_object_agg(nomination,character_name) FROM candidacies c WHERE c.event_id=p.event_id AND c.participant_id=p.id) roles
            FROM participants p WHERE event_id=$1)
        SELECT EXISTS(SELECT 1 FROM last_test),count(*) FROM old FULL JOIN current ON old.participant_id=current.id
        WHERE ROW(old.display_name,old.faculty_code,old.photo,old.roles) IS DISTINCT FROM ROW(current.display_name,current.faculty_code,current.photo,current.roles)`, results.Event.ID).Scan(&view.HasTest, &view.RosterChanges)
		if err != nil {
			participantError(w, err)
			return
		}
		settings, err := eventBackupSettings(r.Context(), v.db, results.Event.ID)
		if err != nil {
			participantError(w, err)
			return
		}
		if slug == settings.event && responderURL(settings.link) {
			view.BackupURL = settings.link
		}
	}
	view.Events, err = v.eventList(r.Context())
	if err != nil {
		participantError(w, err)
		return
	}
	created, _ := strconv.ParseInt(r.URL.Query().Get("created"), 10, 64)
	for _, event := range view.Events {
		if created == event.ID {
			view.NewEventID = event.ID
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := resultsPages.ExecuteTemplate(w, "dashboard", view); err != nil {
		log.Printf("admin dashboard template: %v", err)
	}
}

func (v votingEvent) calculateResults(ctx context.Context) (resultsView, error) {
	return v.resultsForRun(ctx, 0)
}
func requestedRun(r *http.Request) int64 {
	raw := r.URL.Query().Get("run")
	if raw == "" {
		return 0
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id < 1 {
		return -1
	}
	return id
}
func (v votingEvent) resultsForRun(ctx context.Context, selected int64) (resultsView, error) {
	view := resultsView{Nominations: []nominationResult{
		{Code: "dancer", Title: "Самый яркий танцор"},
		{Code: "vocalist", Title: "Самый яркий вокалист"},
		{Code: "actor", Title: "Самый яркий актёр"},
	}}
	tx, err := v.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return view, err
	}
	defer tx.Rollback()

	var updated time.Time
	var deadline sql.NullTime
	err = tx.QueryRowContext(ctx, `
		SELECT e.id,e.name, `+publicRunState+`, (SELECT count(*) FROM ballots b WHERE b.run_id=vr.id),
		       (SELECT count(*) FROM participants p WHERE p.event_id = e.id), statement_timestamp(), vr.deadline,coalesce(vr.id,0),coalesce(vr.kind,''),exists(select 1 from voting_runs where event_id=e.id and kind='main'), (SELECT coalesce(max(id),0) FROM event_state_changes WHERE event_id=e.id)
		FROM events e LEFT JOIN LATERAL (SELECT * FROM voting_runs WHERE event_id=e.id AND ($2::bigint=0 OR id=$2)
        ORDER BY (closed_at IS NULL AND (deadline IS NULL OR deadline>statement_timestamp())) DESC,(kind='main') DESC,id DESC LIMIT 1) vr ON true
        WHERE `+activeEventFilter, v.slug, selected).Scan(&view.Event.ID, &view.Event.Name, &view.Event.State, &view.Ballots, &view.Participants, &updated, &deadline, &view.RunID, &view.Kind, &view.HasMain, &view.Revision)
	if err != nil {
		return view, err
	}
	if selected != 0 && view.RunID != selected {
		return view, sql.ErrNoRows
	}
	history, err := tx.QueryContext(ctx, `SELECT id, (CASE kind WHEN 'test' THEN 'Тестовый прогон' ELSE 'Основное голосование' END) || ' · ' || to_char(started_at AT TIME ZONE 'Europe/Moscow','DD.MM HH24:MI') FROM voting_runs WHERE event_id=$1 ORDER BY id DESC`, view.Event.ID)
	if err != nil {
		return view, err
	}
	for history.Next() {
		var item runOption
		if err := history.Scan(&item.ID, &item.Label); err != nil {
			history.Close()
			return view, err
		}
		view.Runs = append(view.Runs, item)
	}
	history.Close()
	if err = history.Err(); err != nil {
		return view, err
	}
	view.Updated = updated.In(time.FixedZone("МСК", 3*60*60)).Format("02.01.2006 15:04:05 MST")
	if deadline.Valid {
		view.EndsUnix = deadline.Time.Unix()
		view.Deadline = deadline.Time.In(time.FixedZone("МСК", 10800)).Format("15:04:05 MST")
	}

	rows, err := tx.QueryContext(ctx, `
		WITH tally AS (
			SELECT choice.nomination, choice.participant_id, count(*) AS votes,
			       count(*) FILTER (WHERE b.accepted_at > statement_timestamp() - interval '10 seconds') AS recent
			FROM ballots b
			CROSS JOIN LATERAL (VALUES
				('dancer', b.dancer_id), ('vocalist', b.vocalist_id), ('actor', b.actor_id)
			) AS choice(nomination, participant_id)
			WHERE b.run_id = $1
			GROUP BY choice.nomination, choice.participant_id
		)
		SELECT c.nomination, p.id, p.display_name, f.short_name, coalesce(t.votes, 0) AS votes, coalesce(t.recent, 0)
		FROM (
            SELECT participant_id,nomination FROM run_candidacies WHERE run_id=$1
            UNION ALL SELECT participant_id,nomination FROM candidacies WHERE $1=0 AND event_id=$2
        ) c JOIN (
            SELECT participant_id AS id,display_name,faculty_code FROM run_participants WHERE run_id=$1
            UNION ALL SELECT id,display_name,faculty_code FROM participants WHERE $1=0 AND event_id=$2
        ) p ON p.id=c.participant_id
		JOIN faculties f ON f.code = p.faculty_code
		LEFT JOIN tally t ON t.nomination = c.nomination AND t.participant_id = c.participant_id
		
		ORDER BY c.nomination, votes DESC, p.display_name, p.id
	`, view.RunID, view.Event.ID)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	byCode := make(map[string]*nominationResult, len(view.Nominations))
	for i := range view.Nominations {
		group := &view.Nominations[i]
		byCode[group.Code] = group
	}
	for rows.Next() {
		var code string
		var candidate rankedCandidate
		if err := rows.Scan(&code, &candidate.ID, &candidate.Name, &candidate.Faculty, &candidate.Votes, &candidate.Recent); err != nil {
			return view, err
		}
		group := byCode[code]
		if candidate.Votes > 0 {
			candidate.Place = len(group.Candidates) + 1
			if last := len(group.Candidates) - 1; last >= 0 && candidate.Votes == group.Candidates[last].Votes {
				candidate.Place = group.Candidates[last].Place
			}
			candidate.Leader = candidate.Place == 1
		}
		group.Votes += candidate.Votes
		group.Candidates = append(group.Candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		return view, err
	}
	rows.Close()
	if view.Event.State == "open" {
		view.RecentBallots, err = readJournal(ctx, tx, view.RunID, 10)
		if err != nil {
			return view, err
		}
	}
	if view.Event.State == "closed" {
		view.Verified, err = verifyResults(ctx, tx, view)
		if err != nil {
			return view, err
		}
	}
	return view, tx.Commit()
}

func verifyResults(ctx context.Context, tx *sql.Tx, view resultsView) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT dancer_id, vocalist_id, actor_id FROM ballots WHERE run_id = $1", view.RunID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var ballotCount int64
	counts := [3]map[int64]int64{{}, {}, {}}
	for rows.Next() {
		var ids [3]int64
		if err := rows.Scan(&ids[0], &ids[1], &ids[2]); err != nil {
			return false, err
		}
		ballotCount++
		for i, id := range ids {
			counts[i][id]++
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if ballotCount != view.Ballots {
		return false, nil
	}
	for i, group := range view.Nominations {
		if group.Votes != ballotCount {
			return false, nil
		}
		for _, candidate := range group.Candidates {
			if counts[i][candidate.ID] != candidate.Votes {
				return false, nil
			}
			delete(counts[i], candidate.ID)
		}
		if len(counts[i]) != 0 {
			return false, nil
		}
	}
	return true, nil
}

func (v votingEvent) resultsPage(w http.ResponseWriter, r *http.Request) {
	view, err := v.resultsForRun(r.Context(), requestedRun(r))
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := resultsPages.ExecuteTemplate(w, "results", view); err != nil {
		log.Printf("results template: %v", err)
	}
}

func (v votingEvent) exportResults(w http.ResponseWriter, r *http.Request) {
	view, err := v.resultsForRun(r.Context(), requestedRun(r))
	if err != nil {
		participantError(w, err)
		return
	}
	if view.Event.State != "closed" || !view.Verified {
		http.Error(w, "Выгрузка доступна после закрытия и успешной сверки результатов", http.StatusConflict)
		return
	}
	var data bytes.Buffer
	data.WriteString("\ufeff")
	writer := csv.NewWriter(&data)
	writer.Comma = ';'
	writer.UseCRLF = true
	writer.Write([]string{"Номинация", "ID", "Участник", "Факультет", "Голоса", "Место", "Лидер", "Всего бюллетеней"})
	for _, group := range view.Nominations {
		for _, candidate := range group.Candidates {
			place, leader := "", ""
			if candidate.Place > 0 {
				place = strconv.Itoa(candidate.Place)
			}
			if candidate.Leader {
				leader = "Да"
			}
			writer.Write([]string{group.Title, strconv.FormatInt(candidate.ID, 10), csvText(candidate.Name),
				csvText(candidate.Faculty), strconv.FormatInt(candidate.Votes, 10), place, leader,
				strconv.FormatInt(view.Ballots, 10)})
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="megabattle-results.csv"`)
	w.Write(data.Bytes())
}

func csvText(value string) string {
	if trimmed := strings.TrimLeft(value, " \t\r\n"); trimmed != "" && strings.ContainsRune("=+-@", rune(trimmed[0])) {
		return "'" + value
	}
	return value
}

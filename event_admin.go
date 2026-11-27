package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Until an administrator makes an explicit selection, preserve the deployment default.
const activeEventFilter = `e.id = coalesce(
	(SELECT event_id FROM active_voting_event WHERE singleton),
	(SELECT id FROM events WHERE slug = $1)
)`

func (v votingEvent) eventList(ctx context.Context) ([]eventInfo, error) {
	rows, err := v.db.QueryContext(ctx, "SELECT e.id, e.name, "+effectiveVotingState+", (SELECT count(*) FROM participants WHERE event_id=e.id),(SELECT count(*) FROM ballots WHERE event_id=e.id) FROM events e ORDER BY e.id DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []eventInfo
	for rows.Next() {
		var event eventInfo
		if err := rows.Scan(&event.ID, &event.Name, &event.State, &event.PeopleCount, &event.BallotCount); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (v votingEvent) createEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if !validEventName(name) {
		v.showDashboard(w, r, http.StatusBadRequest, "Введи название мероприятия длиной до 120 символов.")
		return
	}
	var key [12]byte
	rand.Read(key[:])
	var id int64
	if err := v.db.QueryRowContext(r.Context(), "INSERT INTO events (slug, name, backup_requested) VALUES ($1, $2, $3) RETURNING id",
		"event-"+hex.EncodeToString(key[:]), name, driveFileID.MatchString(os.Getenv("MEGALOVE_BACKUP_FOLDER_ID"))).Scan(&id); err != nil {
		participantError(w, err)
		return
	}
	http.Redirect(w, r, "/admin?created="+strconv.FormatInt(id, 10)+"#event-management", http.StatusSeeOther)
}

func validEventName(name string) bool {
	return name != "" && utf8.ValidString(name) && !strings.ContainsRune(name, 0) && utf8.RuneCountInString(name) <= 120
}

func (v votingEvent) editEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if !validEventName(name) {
		v.showDashboard(w, r, http.StatusBadRequest, "Введи название мероприятия длиной до 120 символов.")
		return
	}
	var id int64
	err := v.db.QueryRowContext(r.Context(), "UPDATE events SET name=$2 WHERE id=$1 RETURNING id", pathID(r, "event"), name).Scan(&id)
	if err != nil {
		participantError(w, err)
		return
	}
	http.Redirect(w, r, "/admin?event_updated=1#event-management", http.StatusSeeOther)
}

var errDeleteConfirmation = errors.New("event name did not match")
var errDeleteBackup = errors.New("backup form deletion failed")
var errBackupBusy = errors.New("backup form is busy")

func (v votingEvent) deleteEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	switch err := v.removeEvent(r.Context(), pathID(r, "event"), strings.TrimSpace(r.PostForm.Get("confirm"))); {
	case errors.Is(err, errActiveEventOpen):
		v.showDashboard(w, r, http.StatusConflict, "Сначала закрой голосование этого мероприятия.")
	case errors.Is(err, errDeleteConfirmation):
		v.showDashboard(w, r, http.StatusBadRequest, "Для удаления введи точное название мероприятия.")
	case errors.Is(err, errBackupBusy):
		v.showDashboard(w, r, http.StatusConflict, "Резервная форма обновляется. Повтори удаление через несколько секунд.")
	case errors.Is(err, errDeleteBackup):
		log.Printf("delete event backup: %v", err)
		v.showDashboard(w, r, http.StatusBadGateway, "Не удалось переместить резервную форму в корзину Google Drive. Мероприятие сохранено. Повтори удаление позже.")
	case err != nil:
		participantError(w, err)
	default:
		http.Redirect(w, r, "/admin?event_deleted=1#event-management", http.StatusSeeOther)
	}
}

func (v votingEvent) removeEvent(ctx context.Context, id int64, confirmation string) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM active_voting_event WHERE singleton FOR UPDATE"); err != nil {
		return err
	}
	var name, state, formID string
	if err := tx.QueryRowContext(ctx, "SELECT name,state,coalesce(backup_form_id,'') FROM events WHERE id=$1 FOR UPDATE", id).Scan(&name, &state, &formID); err != nil {
		return err
	}
	if state == "open" {
		if err := tx.QueryRowContext(ctx, "SELECT "+effectiveVotingState+" FROM events e WHERE id=$1", id).Scan(&state); err != nil {
			return err
		}
		if state == "open" {
			return errActiveEventOpen
		}
	}
	if confirmation != name {
		return errDeleteConfirmation
	}
	locked, err := tryBackupLock(ctx, tx, id)
	if err != nil {
		return err
	}
	if !locked {
		return errBackupBusy
	}
	for _, query := range []string{
		"UPDATE active_voting_event SET event_id=NULL WHERE event_id=$1",
		"DELETE FROM ballots WHERE event_id=$1",
		"DELETE FROM candidacies WHERE event_id=$1",
		"DELETE FROM participants WHERE event_id=$1",
		"DELETE FROM event_state_changes WHERE event_id=$1",
		"DELETE FROM events WHERE id=$1",
	} {
		if _, err := tx.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}
	if formID != "" {
		client, err := managedBackupSettings(currentBackupSettings()).client(ctx)
		if err == nil {
			err = trashBackupForm(ctx, client, formID)
		}
		if err != nil {
			return fmt.Errorf("%w: %v", errDeleteBackup, err)
		}
	}
	return tx.Commit()
}

var errActiveEventOpen = errors.New("active event is open")

func (v votingEvent) selectEvent(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать форму", http.StatusBadRequest)
		return
	}
	id, err := strconv.ParseInt(r.PostForm.Get("event_id"), 10, 64)
	if err != nil || id < 1 {
		v.showDashboard(w, r, http.StatusBadRequest, "Выбери мероприятие из списка.")
		return
	}
	switch err := v.activateEvent(r.Context(), id); {
	case errors.Is(err, errActiveEventOpen):
		v.showDashboard(w, r, http.StatusConflict, "Сначала закрой текущее голосование.")
	case err != nil:
		participantError(w, err)
	default:
		http.Redirect(w, r, "/admin#event-management", http.StatusSeeOther)
	}
}

func (v votingEvent) activateEvent(ctx context.Context, target int64) error {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM active_voting_event WHERE singleton FOR UPDATE"); err != nil {
		return err
	}
	var currentID int64
	var state string
	err = tx.QueryRowContext(ctx, "SELECT e.id, "+effectiveVotingState+" FROM events e WHERE "+activeEventFilter, v.slug).
		Scan(&currentID, &state)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if currentID == target {
		return nil
	}
	if state == "open" {
		return errActiveEventOpen
	}
	var selected int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM events WHERE id = $1", target).Scan(&selected); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE active_voting_event SET event_id = $1 WHERE singleton", selected); err != nil {
		return err
	}
	return tx.Commit()
}

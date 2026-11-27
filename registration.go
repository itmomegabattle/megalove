package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strconv"

	qrcode "github.com/skip2/go-qrcode"
)

var registrationKey = regexp.MustCompile(`^[a-f0-9]{48}$`)

func newRegistrationKey() string {
	var raw [24]byte
	rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

func (a participantAdmin) register(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' blob:; frame-ancestors 'none'; form-action 'self'")
	token := r.PathValue("token")
	var id int64
	err := a.db.QueryRowContext(r.Context(), "SELECT id FROM events e WHERE registration_token=$1 AND ("+effectiveVotingState+")<>'open'", token).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Регистрация закрыта или ссылка устарела.", http.StatusNotFound)
		return
	}
	if err != nil {
		participantError(w, err)
		return
	}
	r.SetPathValue("event", strconv.FormatInt(id, 10))
	if r.Method == http.MethodPost {
		if r.URL.Query().Get("preview") == "1" {
			previewPortrait(w, r)
			return
		}
		a.saveForm(w, r, token)
		return
	}
	view, err := a.formView(r)
	if err != nil {
		participantError(w, err)
		return
	}
	view.Public = true
	view.Submission = newRegistrationKey()
	writeParticipantPage(w, "form", http.StatusOK, view)
}

func (v votingEvent) toggleRegistration(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Неверная форма", 400)
		return
	}
	id := r.PostForm.Get("event_id")
	event, err := v.current(r.Context())
	if err != nil {
		participantError(w, err)
		return
	}
	if id != strconv.FormatInt(event.ID, 10) {
		http.Error(w, "Мероприятие изменилось", 409)
		return
	}
	var opening bool
	switch r.PostForm.Get("action") {
	case "open":
		opening = true
	case "close":
	default:
		http.Error(w, "Неверное действие", 400)
		return
	}
	result, err := v.db.ExecContext(r.Context(), `UPDATE events e SET registration_token=CASE WHEN $2 THEN registration_key ELSE NULL END
 WHERE id=$3 AND (`+effectiveVotingState+`) <> 'open' AND `+`id=(SELECT e.id FROM events e WHERE `+activeEventFilter+`) AND
 ($2 <> (registration_token IS NOT NULL))`, v.slug, opening, event.ID)
	if err != nil {
		participantError(w, err)
		return
	}
	n, err := result.RowsAffected()
	if err != nil {
		participantError(w, err)
		return
	}
	if n != 1 {
		http.Error(w, "Состояние регистрации изменилось. Обнови панель.", 409)
		return
	}
	http.Redirect(w, r, "/admin#participants", http.StatusSeeOther)
}

func (a participantAdmin) writeRegistration(ctx context.Context, id int64, token, submission string, p *participant, photo []byte) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPreparingEvent(ctx, tx, id); err != nil {
		return err
	}
	var valid bool
	if err := tx.QueryRowContext(ctx, "SELECT registration_token IS NOT DISTINCT FROM $2::text FROM events WHERE id=$1", id, token).Scan(&valid); err != nil {
		return err
	}
	if !valid {
		return errParticipantLocked
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS (SELECT 1 FROM participants WHERE event_id=$1 AND registration_submission=$2)", id, submission).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	if err := saveParticipant(ctx, tx, id, p, photo); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE participants SET registration_submission=$2 WHERE id=$1", p.ID, submission); err != nil {
		return err
	}
	return tx.Commit()
}

func (v votingEvent) registrationQR(w http.ResponseWriter, r *http.Request) {
	var key string
	if err := v.db.QueryRowContext(r.Context(), "SELECT registration_key FROM events WHERE id=$1", pathID(r, "event")).Scan(&key); err != nil {
		participantError(w, err)
		return
	}
	png, err := qrcode.Encode("https://vote.megabattle.site/register/"+key, qrcode.Medium, 768)
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="megabattle-registration.png"`)
	w.Write(png)
}

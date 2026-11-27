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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"
)

//go:embed participants.html
var participantPagesHTML string

var participantPages = template.Must(template.New("participants").Parse(participantPagesHTML))

type participantAdmin struct{ db *sql.DB }

type eventInfo struct {
	PeopleCount, BallotCount int64
	ID                       int64
	Name, State              string
}

type facultyOption struct {
	Code, ShortName string
}

type participant struct {
	ID                                int64
	Name, Faculty, FacultyShort       string
	Character                         string
	Dancer, Vocalist, Actor, HasPhoto bool
}

type participantView struct {
	Public     bool
	Submission string
	Event      eventInfo
	Events     []eventInfo
	Groups     []participantGroup
	Person     participant
	Faculties  []facultyOption
	Error      string
	ReadOnly   bool
}

func (v participantView) CanEdit() bool {
	return !v.ReadOnly && (v.Event.State == "preparing" || v.Event.State == "closed")
}

type participantGroup struct {
	Faculty string
	People  []participant
}

func mountParticipants(mux *http.ServeMux, db *sql.DB) {
	admin := participantAdmin{db}
	mux.HandleFunc("GET /admin/events", admin.events)
	mux.HandleFunc("GET /admin/events/{event}/participants", admin.list)
	mux.HandleFunc("GET /admin/events/{event}/participants/groups", admin.directoryGroups)
	mux.HandleFunc("POST /admin/events/{event}/participants/groups", admin.assignDirectoryGroups)
	mux.HandleFunc("GET /admin/events/{event}/participants/new", admin.form)
	mux.HandleFunc("POST /admin/events/{event}/participants/new", admin.save)
	mux.HandleFunc("GET /admin/events/{event}/participants/{person}", admin.form)
	mux.HandleFunc("POST /admin/events/{event}/participants/{person}", admin.save)
	mux.HandleFunc("POST /admin/events/{event}/participants/{person}/delete", admin.remove)
	mux.HandleFunc("GET /admin/events/{event}/participants/{person}/photo", admin.photo)
}

func (a participantAdmin) remove(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	eventID, personID := pathID(r, "event"), pathID(r, "person")
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		participantError(w, err)
		return
	}
	defer tx.Rollback()
	if err := lockPreparingEvent(ctx, tx, eventID); err != nil {
		if errors.Is(err, errParticipantLocked) {
			http.Error(w, "Удаление доступно до открытия голосования.", http.StatusConflict)
		} else {
			participantError(w, err)
		}
		return
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM candidacies WHERE event_id=$1 AND participant_id=$2", eventID, personID); err != nil {
		participantError(w, err)
		return
	}
	result, err := tx.ExecContext(ctx, "DELETE FROM participants WHERE event_id=$1 AND id=$2", eventID, personID)
	if err == nil {
		var count int64
		count, err = result.RowsAffected()
		if err == nil && count == 0 {
			http.NotFound(w, r)
			return
		}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		participantError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/events/"+strconv.FormatInt(eventID, 10)+"/participants", http.StatusSeeOther)
}

func writeParticipantPage(w http.ResponseWriter, name string, status int, view participantView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := participantPages.ExecuteTemplate(w, name, view); err != nil {
		log.Printf("participants template: %v", err)
	}
}

func participantError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		http.Error(w, "Запись не найдена", http.StatusNotFound)
		return
	}
	log.Printf("participants database: %T", err)
	http.Error(w, "Не удалось выполнить операцию", http.StatusInternalServerError)
}

func pathID(r *http.Request, name string) int64 {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id < 1 {
		return 0
	}
	return id
}

func (a participantAdmin) event(ctx context.Context, id int64) (eventInfo, error) {
	var event eventInfo
	err := a.db.QueryRowContext(ctx, "SELECT e.id, e.name, "+effectiveVotingState+" FROM events e WHERE e.id = $1", id).
		Scan(&event.ID, &event.Name, &event.State)
	return event, err
}

func (a participantAdmin) events(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.QueryContext(r.Context(), "SELECT id, name, state FROM events ORDER BY id DESC")
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	var view participantView
	for rows.Next() {
		var event eventInfo
		if err := rows.Scan(&event.ID, &event.Name, &event.State); err != nil {
			participantError(w, err)
			return
		}
		view.Events = append(view.Events, event)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	writeParticipantPage(w, "events", http.StatusOK, view)
}

const participantsSelect = `
	SELECT p.id, p.display_name, p.faculty_code, f.short_name, p.photo_jpeg IS NOT NULL,
	       coalesce(bool_or(c.nomination = 'dancer'), false),
	       coalesce(bool_or(c.nomination = 'vocalist'), false),
	       coalesce(bool_or(c.nomination = 'actor'), false),
	       coalesce(max(c.character_name), '')
	FROM participants p JOIN faculties f ON f.code = p.faculty_code
	LEFT JOIN candidacies c ON c.event_id = p.event_id AND c.participant_id = p.id
	WHERE p.event_id = $1
`

func scanPerson(row interface{ Scan(...any) error }, p *participant) error {
	return row.Scan(&p.ID, &p.Name, &p.Faculty, &p.FacultyShort, &p.HasPhoto,
		&p.Dancer, &p.Vocalist, &p.Actor, &p.Character)
}

func (a participantAdmin) list(w http.ResponseWriter, r *http.Request) {
	event, err := a.event(r.Context(), pathID(r, "event"))
	if err != nil {
		participantError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), participantsSelect+
		" GROUP BY p.id, f.short_name ORDER BY f.short_name, CASE WHEN bool_or(c.nomination='dancer') THEN 1 WHEN bool_or(c.nomination='vocalist') THEN 2 ELSE 3 END, p.display_name, p.id", event.ID)
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	view := participantView{Event: event}
	for rows.Next() {
		var p participant
		if err := scanPerson(rows, &p); err != nil {
			participantError(w, err)
			return
		}
		if len(view.Groups) == 0 || view.Groups[len(view.Groups)-1].Faculty != p.FacultyShort {
			view.Groups = append(view.Groups, participantGroup{Faculty: p.FacultyShort})
		}
		group := &view.Groups[len(view.Groups)-1]
		group.People = append(group.People, p)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	writeParticipantPage(w, "list", http.StatusOK, view)
}

func (a participantAdmin) formView(r *http.Request) (participantView, error) {
	event, err := a.event(r.Context(), pathID(r, "event"))
	view := participantView{Event: event}
	if err != nil {
		return view, err
	}
	if r.PathValue("person") != "" {
		row := a.db.QueryRowContext(r.Context(), participantsSelect+
			" AND p.id = $2 GROUP BY p.id, f.short_name", event.ID, pathID(r, "person"))
		if err := scanPerson(row, &view.Person); err != nil {
			return view, err
		}
	}
	rows, err := a.db.QueryContext(r.Context(), "SELECT code, short_name FROM faculties ORDER BY short_name")
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var faculty facultyOption
		if err := rows.Scan(&faculty.Code, &faculty.ShortName); err != nil {
			return view, err
		}
		view.Faculties = append(view.Faculties, faculty)
	}
	return view, rows.Err()
}

func (a participantAdmin) form(w http.ResponseWriter, r *http.Request) {
	view, err := a.formView(r)
	if err != nil {
		participantError(w, err)
		return
	}
	view.ReadOnly = r.URL.Query().Get("view") == "1"
	writeParticipantPage(w, "form", http.StatusOK, view)
}

func (a participantAdmin) save(w http.ResponseWriter, r *http.Request) {
	a.saveForm(w, r, "")
}

func (a participantAdmin) saveForm(w http.ResponseWriter, r *http.Request, registration string) {
	view, err := a.formView(r)
	if err != nil {
		participantError(w, err)
		return
	}
	view.Public = registration != ""
	if view.Public {
		view.Submission = newRegistrationKey()
	}
	reject := func(status int, message string) {
		view.Error = message
		writeParticipantPage(w, "form", status, view)
	}
	if !view.CanEdit() {
		reject(http.StatusConflict, "Анкеты доступны только для просмотра.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPhotoBytes+(64<<10))
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		reject(http.StatusBadRequest, "Не удалось прочитать форму. Дождись предпросмотра сжатого фото и попробуй снова.")
		return
	}
	defer r.MultipartForm.RemoveAll()
	if view.Public {
		if key := r.PostForm.Get("submission"); registrationKey.MatchString(key) {
			view.Submission = key
		} else {
			reject(http.StatusBadRequest, "Обнови страницу и отправь анкету снова.")
			return
		}
	}

	p := &view.Person
	p.Name = strings.TrimSpace(r.PostForm.Get("name"))
	p.Faculty = r.PostForm.Get("faculty")
	p.Dancer = r.PostForm.Get("dancer") == "on"
	p.Vocalist = r.PostForm.Get("vocalist") == "on"
	p.Actor = r.PostForm.Get("actor") == "on"
	p.Character = ""
	if p.Actor {
		p.Character = strings.TrimSpace(r.PostForm.Get("character"))
	}
	if message := validateParticipant(view); message != "" {
		reject(http.StatusBadRequest, message)
		return
	}
	photo, err := uploadedPhoto(r)
	if err != nil {
		reject(http.StatusBadRequest, err.Error())
		return
	}
	if view.Public {
		if len(photo) == 0 {
			reject(http.StatusBadRequest, "Добавь фотографию.")
			return
		}
		// Once the complete form has been read, a disconnected browser
		// must not roll back the registration.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 12*time.Second)
		defer cancel()
		err = a.writeRegistration(ctx, view.Event.ID, registration, view.Submission, p, photo)
	} else {
		err = a.write(r.Context(), view.Event.ID, p, photo)
	}
	if errors.Is(err, errParticipantLocked) {
		view.Event.State = "locked"
		reject(http.StatusConflict, "Анкеты доступны только для просмотра.")
		return
	}
	if errors.Is(err, errNominationHasVotes) {
		reject(http.StatusConflict, "Нельзя убрать номинацию, в которой участник уже получил голоса. Оставь её отмеченной и сохрани остальные правки.")
		return
	}
	if err != nil {
		participantError(w, err)
		return
	}
	if view.Public {
		http.Redirect(w, r, "/register/complete", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/events/"+strconv.FormatInt(view.Event.ID, 10)+"/participants", http.StatusSeeOther)
}

func validateParticipant(view participantView) string {
	p := view.Person
	return participantValidation(p, view.Faculties)
}

func participantValidation(p participant, faculties []facultyOption) string {
	if !utf8.ValidString(p.Name) || strings.ContainsRune(p.Name, 0) || p.Name == "" || utf8.RuneCountInString(p.Name) > 160 {
		return "Введи имя и фамилию, до 160 символов."
	}
	knownFaculty := false
	for _, f := range faculties {
		knownFaculty = knownFaculty || p.Faculty == f.Code
	}
	if !knownFaculty {
		return "Выбери факультет."
	}
	if !p.Dancer && !p.Vocalist && !p.Actor {
		return "Выбери хотя бы одну роль."
	}
	if !utf8.ValidString(p.Character) || strings.ContainsRune(p.Character, 0) || utf8.RuneCountInString(p.Character) > 160 {
		return "Имя персонажа должно содержать до 160 символов."
	}
	return ""
}

var errParticipantLocked = errors.New("event roster locked")
var errNominationHasVotes = errors.New("nomination has votes")

func (a participantAdmin) write(ctx context.Context, eventID int64, p *participant, photo []byte) error {
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := lockPreparingEvent(ctx, tx, eventID); err != nil {
		return err
	}
	if err := saveParticipant(ctx, tx, eventID, p, photo); err != nil {
		return err
	}
	return tx.Commit()
}

func lockPreparingEvent(ctx context.Context, tx *sql.Tx, eventID int64) error {
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT "+effectiveVotingState+" FROM events e WHERE id = $1 FOR UPDATE", eventID).Scan(&state); err != nil {
		return err
	}
	if state == "open" {
		return errParticipantLocked
	}
	return nil
}

func saveParticipant(ctx context.Context, tx *sql.Tx, eventID int64, p *participant, photo []byte) error {
	var err error
	if p.ID == 0 {
		err = tx.QueryRowContext(ctx, `
			INSERT INTO participants (event_id, faculty_code, display_name, photo_jpeg)
			VALUES ($1, $2, $3, $4) RETURNING id
		`, eventID, p.Faculty, p.Name, photo).Scan(&p.ID)
	} else {
		err = tx.QueryRowContext(ctx, `
			UPDATE participants SET faculty_code = $3, display_name = $4, photo_jpeg = coalesce($5, photo_jpeg)
			WHERE event_id = $1 AND id = $2 RETURNING id
		`, eventID, p.ID, p.Faculty, p.Name, photo).Scan(&p.ID)
	}
	if err != nil {
		return err
	}
	for _, nomination := range []struct {
		code      string
		selected  bool
		character string
	}{
		{"dancer", p.Dancer, ""},
		{"vocalist", p.Vocalist, ""},
		{"actor", p.Actor, p.Character},
	} {
		if !nomination.selected {
			if _, err := tx.ExecContext(ctx, "DELETE FROM candidacies WHERE event_id = $1 AND participant_id = $2 AND nomination = $3",
				eventID, p.ID, nomination.code); err != nil {
				var pgErr *pgconn.PgError
				if errors.As(err, &pgErr) && pgErr.Code == "23503" {
					return errNominationHasVotes
				}
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO candidacies (event_id, participant_id, nomination, character_name)
			VALUES ($1, $2, $3, nullif($4, ''))
			ON CONFLICT (event_id, participant_id, nomination) DO UPDATE SET character_name = excluded.character_name
		`, eventID, p.ID, nomination.code, nomination.character); err != nil {
			return err
		}
	}
	return nil
}

func (a participantAdmin) photo(w http.ResponseWriter, r *http.Request) {
	var data []byte
	err := a.db.QueryRowContext(r.Context(), `
		SELECT photo_jpeg FROM participants WHERE event_id = $1 AND id = $2 AND photo_jpeg IS NOT NULL
	`, pathID(r, "event"), pathID(r, "person")).Scan(&data)
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Write(data)
}

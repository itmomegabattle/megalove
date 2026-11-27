package main

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"html/template"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

//go:embed backup_vote.html
var backupHTML string

var backupPages = template.Must(template.Must(participantPages.Clone()).Parse(backupHTML))
var driveFileID = regexp.MustCompile(`^[A-Za-z0-9_-]{10,160}$`)

type backupCheck struct {
	Label string
	OK    bool
}

type backupView struct {
	EventID       int64
	URL           string
	Editor        string
	Checks        []backupCheck
	Ready, Active bool
	CanReview     bool
	Error         string
}

type googlePublishState struct {
	Published bool `json:"isPublished"`
	Accepting bool `json:"isAcceptingResponses"`
}

type googleBackupForm struct {
	WriteControl struct {
		RequiredRevisionID string
	}
	FormID          string
	ResponderURI    string
	RevisionID      string
	PublishSettings struct {
		State *googlePublishState `json:"publishState"`
	}
	Items []struct {
		Title        string
		QuestionItem struct {
			Question struct {
				QuestionID     string
				Required       bool
				ChoiceQuestion struct {
					Type    string
					Options []struct {
						Value   string
						IsOther bool
						Image   *struct {
							ContentURI string
							AltText    string
						}
					}
				}
			}
		}
	}
}

type backupSettings struct {
	event, id, link, credentials string
	expected                     int
	rehearsed                    bool
	managed                      bool
}

func currentBackupSettings() backupSettings {
	expected, _ := strconv.Atoi(os.Getenv("MEGALOVE_BACKUP_EXPECTED_PARTICIPANTS"))
	return backupSettings{
		event: os.Getenv("MEGALOVE_BACKUP_EVENT"), id: os.Getenv("MEGALOVE_BACKUP_FORM_ID"),
		link: os.Getenv("MEGALOVE_BACKUP_RESPONDER_URL"), credentials: os.Getenv("MEGALOVE_BACKUP_CREDENTIALS"),
		expected: expected, rehearsed: os.Getenv("MEGALOVE_BACKUP_REHEARSED") == "1",
	}
}

func responderURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "docs.google.com" &&
		strings.HasPrefix(u.Path, "/forms/d/e/") && strings.HasSuffix(u.Path, "/viewform") &&
		u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (s backupSettings) endpoint() string {
	return "https://forms.googleapis.com/v1/forms/" + url.PathEscape(s.id)
}

func (s backupSettings) client(ctx context.Context) (*http.Client, error) {
	key, err := os.ReadFile(s.credentials)
	if err != nil {
		return nil, err
	}
	config, err := google.CredentialsFromJSON(ctx, key, "https://www.googleapis.com/auth/forms.body")
	if err != nil {
		return nil, err
	}
	client := oauth2.NewClient(ctx, config.TokenSource)
	client.Timeout = 8 * time.Second
	return client, nil
}

func requestGoogleForm(ctx context.Context, client *http.Client, method, endpoint, body string) (googleBackupForm, error) {
	var form googleBackupForm
	req, err := http.NewRequestWithContext(ctx, method, endpoint, strings.NewReader(body))
	if err != nil {
		return form, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return form, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		var details struct{ Error struct{ Message string } }
		if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&details) == nil && details.Error.Message != "" {
			message := regexp.MustCompile(`https?://[^\s"'<>]+`).ReplaceAllString(details.Error.Message, "[URL]")
			return form, fmt.Errorf("Google Forms HTTP %d: %s", resp.StatusCode, message)
		}
		return form, fmt.Errorf("Google Forms HTTP %d", resp.StatusCode)
	}
	err = json.NewDecoder(resp.Body).Decode(&form)
	return form, err
}

var backupTitles = map[string]string{"dancer": "Самый яркий танцор", "vocalist": "Самый яркий вокалист", "actor": "Самый яркий актёр"}

func matchingBackup(form googleBackupForm, roster map[string][]string) bool {
	if len(form.Items) != len(backupTitles) {
		return false
	}
	seen := map[string]bool{}
	for _, item := range form.Items {
		for code, title := range backupTitles {
			if item.Title != title {
				continue
			}
			question := item.QuestionItem.Question
			kind := question.ChoiceQuestion.Type
			if seen[code] || !question.Required || (kind != "DROP_DOWN" && kind != "RADIO") {
				return false
			}
			var choices []string
			for _, option := range question.ChoiceQuestion.Options {
				if option.IsOther {
					return false
				}
				choices = append(choices, option.Value)
			}
			slices.Sort(choices)
			slices.Sort(roster[code])
			if len(choices) == 0 || !slices.Equal(choices, roster[code]) {
				return false
			}
			seen[code] = true
		}
	}
	return len(seen) == len(backupTitles)
}

func (v votingEvent) inspectBackup(ctx context.Context, tx *sql.Tx, eventID int64, settings backupSettings, client *http.Client) (backupView, error) {
	view := backupView{EventID: eventID}
	var slug string
	var synced, syncedRoster string
	var registration bool
	if err := tx.QueryRowContext(ctx, "SELECT slug,registration_token IS NOT NULL,backup_activated_at IS NOT NULL,coalesce(backup_synced_revision,''),coalesce(backup_synced_roster,'') FROM events WHERE id=$1", eventID).
		Scan(&slug, &registration, &view.Active, &synced, &syncedRoster); err != nil {
		return view, err
	}
	configured := settings.event == slug && driveFileID.MatchString(settings.id) && responderURL(settings.link)
	if configured {
		view.URL = settings.link
		view.Editor = "https://docs.google.com/forms/d/" + settings.id + "/edit"
	}
	rows, err := tx.QueryContext(ctx, `SELECT p.id,p.display_name,f.short_name,p.photo_jpeg,
		coalesce(string_agg(c.nomination,','),'') FROM participants p JOIN faculties f ON f.code=p.faculty_code
		LEFT JOIN candidacies c ON c.event_id=p.event_id AND c.participant_id=p.id WHERE p.event_id=$1
		GROUP BY p.id,f.short_name`, eventID)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	roster := map[string][]string{}
	fingerprints := map[string]string{}
	count, bad := 0, 0
	for rows.Next() {
		var id int64
		var name, faculty, nominations string
		var photo []byte
		if err := rows.Scan(&id, &name, &faculty, &photo, &nominations); err != nil {
			return view, err
		}
		count++
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(photo))
		if err != nil || cfg.Width != cfg.Height || cfg.Width > 720 || nominations == "" {
			bad++
		} else if _, err := jpeg.Decode(bytes.NewReader(photo)); err != nil {
			bad++
		}
		for _, code := range strings.Split(nominations, ",") {
			label := fmt.Sprintf("%s · %s · №%d", name, faculty, id)
			roster[code] = append(roster[code], label)
			fingerprints[label] = label + " [" + photoFingerprint(photo) + "]"
		}
	}
	if err := rows.Err(); err != nil {
		return view, err
	}
	var googleOK, matching, matchingPhotos, accessOK, accepting bool
	if configured && client != nil {
		form, err := requestGoogleForm(ctx, client, http.MethodGet, settings.endpoint(), "")
		if err == nil {
			googleOK = form.PublishSettings.State != nil
			accepting = googleOK && form.PublishSettings.State.Accepting
			matching = matchingBackup(form, roster)
			revisionVerified := synced != "" && synced == form.RevisionID && syncedRoster == backupRosterDigest(roster, fingerprints)
			legacyPhotos := !settings.managed && synced == ""
			matchingPhotos = matching && (revisionVerified || legacyPhotos)
			for _, item := range form.Items {
				for _, option := range item.QuestionItem.Question.ChoiceQuestion.Options {
					matchingPhotos = matchingPhotos && option.Image != nil && option.Image.ContentURI != "" &&
						(revisionVerified || option.Image.AltText == fingerprints[option.Value])
				}
			}
			accessOK = googleOK && form.ResponderURI == view.URL && form.PublishSettings.State.Published &&
				(!(settings.managed || view.Active) || accepting)
		}
	}
	var testOpen, snapshotOK bool
	err = tx.QueryRowContext(ctx, `WITH main AS (SELECT id FROM voting_runs WHERE event_id=$1 AND kind='main'),
      actual AS (SELECT p.id,p.display_name,p.faculty_code,md5(p.photo_jpeg) photo,c.nomination FROM participants p JOIN candidacies c ON c.participant_id=p.id AND c.event_id=p.event_id WHERE p.event_id=$1),
      frozen AS (SELECT p.participant_id,p.display_name,p.faculty_code,md5(p.photo_jpeg) photo,c.nomination FROM run_participants p JOIN run_candidacies c USING(run_id,participant_id) WHERE p.run_id IN (SELECT id FROM main))
      SELECT EXISTS(SELECT 1 FROM voting_runs WHERE event_id=$1 AND kind='test' AND closed_at IS NULL AND deadline>clock_timestamp()),
      NOT EXISTS(SELECT 1 FROM main) OR NOT EXISTS((SELECT * FROM actual EXCEPT SELECT * FROM frozen) UNION ALL (SELECT * FROM frozen EXCEPT SELECT * FROM actual))`, eventID).Scan(&testOpen, &snapshotOK)
	if err != nil {
		return view, err
	}
	view.Checks = []backupCheck{
		{"Тестовое голосование завершено.", !testOpen},
		{"Состав соответствует зафиксированному концертному бюллетеню.", snapshotOK},
		{"Форма и постоянная ссылка привязаны к этому мероприятию.", configured},
		{"Регистрация на сайте закрыта.", !registration},
		{fmt.Sprintf("Количество совпадает с утверждённым составом (%d на сайте из %d ожидаемых).", count, settings.expected), configured && settings.expected > 0 && count == settings.expected},
		{fmt.Sprintf("У каждой анкеты есть номинация и читаемое квадратное фото. Ошибок: %d.", bad), count > 0 && bad == 0},
		{"Во всех трёх номинациях есть кандидаты.", len(roster["dancer"]) > 0 && len(roster["vocalist"]) > 0 && len(roster["actor"]) > 0},
		{"Сервер читает Google Форму и её настройки публикации.", googleOK},
		{"Варианты в обеих формах совпадают по имени, факультету и ID. В каждой номинации обязателен один выбор.", matching},
		{"Фотографии вариантов в Google Forms соответствуют текущим фото кандидатов.", matchingPhotos},
		{"Google подтверждает публикацию формы и адрес для зрителей.", accessOK},
		{"Организатор проверил QR, доступ зрителя, пробную отправку и выгрузку.", configured && settings.rehearsed},
	}
	view.Ready = true
	view.CanReview = !view.Active && configured && !registration && bad == 0 && matching && matchingPhotos && accessOK
	for _, check := range view.Checks {
		view.Ready = view.Ready && check.OK
	}
	return view, nil
}

func (v votingEvent) reviewBackup(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := pathID(r, "event")
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		participantError(w, err)
		return
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT "+effectiveVotingState+" FROM events e WHERE e.id=$1 AND backup_form_id IS NOT NULL FOR UPDATE", id).Scan(&state); err != nil {
		participantError(w, err)
		return
	}
	settings, err := eventBackupSettings(ctx, tx, id)
	if err != nil {
		participantError(w, err)
		return
	}
	client, _ := settings.client(ctx)
	view, err := v.inspectBackup(ctx, tx, id, settings, client)
	if err != nil {
		participantError(w, err)
		return
	}
	if !view.CanReview || state == "open" {
		view.Error = "Закрой регистрацию и основное голосование на сайте. Дождись синхронизации и повтори проверку."
		renderBackup(w, http.StatusConflict, view)
		return
	}
	if _, err := tx.ExecContext(ctx, `UPDATE events SET backup_reviewed_count=(SELECT count(*) FROM participants WHERE event_id=$1) WHERE id=$1`, id); err != nil {
		participantError(w, err)
		return
	}
	if err := tx.Commit(); err != nil {
		participantError(w, err)
		return
	}
	http.Redirect(w, r, "/admin/events/"+strconv.FormatInt(id, 10)+"/backup", http.StatusSeeOther)
}

func (v votingEvent) backupPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	settings, err := eventBackupSettings(ctx, v.db, pathID(r, "event"))
	if err != nil {
		participantError(w, err)
		return
	}
	client, _ := settings.client(ctx)
	tx, err := v.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelRepeatableRead})
	if err != nil {
		participantError(w, err)
		return
	}
	defer tx.Rollback()
	view, err := v.inspectBackup(ctx, tx, pathID(r, "event"), settings, client)
	if err != nil {
		participantError(w, err)
		return
	}
	if r.URL.Query().Get("status") == "1" {
		votingJSON(w, http.StatusOK, struct{ Ready, Active bool }{view.Ready, view.Active})
		return
	}
	renderBackup(w, http.StatusOK, view)
}

func renderBackup(w http.ResponseWriter, status int, view backupView) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := backupPages.ExecuteTemplate(w, "backup-checklist", view); err != nil {
		participantError(w, err)
	}
}

func (v votingEvent) backupQR(w http.ResponseWriter, r *http.Request) {
	s, err := eventBackupSettings(r.Context(), v.db, pathID(r, "event"))
	if err != nil {
		participantError(w, err)
		return
	}
	var slug string
	if err := v.db.QueryRowContext(r.Context(), "SELECT slug FROM events WHERE id=$1", pathID(r, "event")).Scan(&slug); err != nil {
		participantError(w, err)
		return
	}
	if s.event != slug || !responderURL(s.link) || !driveFileID.MatchString(s.id) {
		http.NotFound(w, r)
		return
	}
	png, err := whiteBackupQR(s.link)
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Content-Disposition", `attachment; filename="megabattle-plan-b.png"`)
	_, _ = w.Write(png)
}

func whiteBackupQR(link string) ([]byte, error) {
	code, err := qrcode.New(link, qrcode.Medium)
	if err != nil {
		return nil, err
	}
	code.ForegroundColor = color.White
	code.BackgroundColor = color.Transparent
	return code.PNG(768)
}

func (v votingEvent) activateBackup(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(22 * time.Second))
	settings, err := eventBackupSettings(ctx, v.db, pathID(r, "event"))
	if err != nil {
		participantError(w, err)
		return
	}
	client, _ := settings.client(ctx)
	v.activateBackupWith(ctx, w, pathID(r, "event"), settings, client)
}

func tryBackupLock(ctx context.Context, tx *sql.Tx, id int64) (bool, error) {
	var locked bool
	err := tx.QueryRowContext(ctx, "SELECT pg_try_advisory_xact_lock(-$1::bigint)", id).Scan(&locked)
	return locked, err
}

func (v votingEvent) activateBackupWith(ctx context.Context, w http.ResponseWriter, id int64, settings backupSettings, client *http.Client) {
	tx, err := v.db.BeginTx(ctx, nil)
	if err != nil {
		participantError(w, err)
		return
	}
	defer tx.Rollback()
	locked, err := tryBackupLock(ctx, tx, id)
	if err != nil {
		participantError(w, err)
		return
	}
	if !locked {
		renderBackup(w, http.StatusConflict, backupView{EventID: id, Error: "Список кандидатов обновляется в Google. Повтори запуск через несколько секунд."})
		return
	}
	if _, err := tx.ExecContext(ctx, "SELECT 1 FROM active_voting_event WHERE singleton FOR SHARE"); err != nil {
		participantError(w, err)
		return
	}
	var state string
	err = tx.QueryRowContext(ctx, "SELECT e.state FROM events e WHERE "+activeEventFilter+" AND e.id=$2 FOR UPDATE OF e", v.slug, id).Scan(&state)
	if err != nil {
		participantError(w, err)
		return
	}
	var testing bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM voting_runs WHERE event_id=$1 AND kind='test' AND closed_at IS NULL AND deadline>clock_timestamp())`, id).Scan(&testing); err != nil {
		participantError(w, err)
		return
	}
	if testing {
		http.Error(w, "Заверши тестовый прогон перед запуском Плана Б.", http.StatusConflict)
		return
	}
	view, err := v.inspectBackup(ctx, tx, id, settings, client)
	if err != nil {
		participantError(w, err)
		return
	}
	if view.Active {
		w.Header().Set("Location", "/admin#plan-b")
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	if !view.Ready {
		view.Error = "План Б пока не готов. Проверь отмеченные пункты."
		renderBackup(w, http.StatusConflict, view)
		return
	}
	if err := enableBackupResponses(ctx, client, settings); err != nil {
		view.Error = "Google не подтвердил приём ответов. Сайт продолжает работать в прежнем режиме. Проверь состояние формы и повтори запуск."
		renderBackup(w, http.StatusBadGateway, view)
		return
	}
	_, err = tx.ExecContext(ctx, `UPDATE events SET state='closed', voting_ends_at=NULL, registration_token=NULL,
		backup_activated_at=clock_timestamp() WHERE id=$1`, id)
	if err == nil {
		_, err = tx.ExecContext(ctx, `UPDATE voting_runs SET closed_at=least(clock_timestamp(),deadline) WHERE event_id=$1 AND closed_at IS NULL`, id)
	}
	if err == nil && state != "closed" {
		_, err = tx.ExecContext(ctx, `INSERT INTO event_state_changes(event_id,from_state,to_state,changed_at)
			VALUES ($1,$2,'closed',clock_timestamp())`, id, state)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		view.Error = "Google уже принимает ответы, но сайт не подтвердил закрытие. Повтори запуск Плана Б или закрой сайт вручную."
		renderBackup(w, http.StatusBadGateway, view)
		return
	}
	w.Header().Set("Location", "/admin#plan-b")
	w.WriteHeader(http.StatusSeeOther)
}

func enableBackupResponses(ctx context.Context, client *http.Client, settings backupSettings) error {
	form, err := requestGoogleForm(ctx, client, http.MethodPost, settings.endpoint()+":setPublishSettings",
		`{"publishSettings":{"publishState":{"isPublished":true,"isAcceptingResponses":true}},"updateMask":"publishState"}`)
	if err != nil {
		return err
	}
	state := form.PublishSettings.State
	if state == nil || !state.Published || !state.Accepting {
		return fmt.Errorf("Google did not confirm that the form accepts responses")
	}
	return nil
}

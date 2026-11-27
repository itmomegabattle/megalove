package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
)

type peopleChoice struct {
	Label string
	URL   string
}

type peopleView struct {
	Title      string
	Breadcrumb string
	Query      string
	Back       peopleChoice
	Choices    []peopleChoice
	ShowPeople bool
	People     []participant
}

var peopleRoles = []struct{ Code, Label string }{
	{"actor", "Актёры"}, {"dancer", "Танцоры"}, {"vocalist", "Певцы"},
}

func peopleURL(faculty, role, group string) string {
	values := url.Values{}
	if faculty != "" {
		values.Set("faculty", faculty)
	}
	if role != "" {
		values.Set("role", role)
	}
	if group != "" {
		values.Set("group", group)
	}
	if len(values) == 0 {
		return "/people"
	}
	return "/people?" + values.Encode()
}

func (v votingEvent) people(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'")
	faculty, role, group := r.URL.Query().Get("faculty"), r.URL.Query().Get("role"), r.URL.Query().Get("group")
	view := peopleView{Title: "Участники Мегабаттла"}
	view.Query = strings.Join(strings.Fields(r.URL.Query().Get("q")), " ")
	if len([]rune(view.Query)) > 160 {
		http.Error(w, "Слишком длинный поисковый запрос", http.StatusBadRequest)
		return
	}
	if view.Query != "" {
		view.Title = "Поиск участников"
		view.Back = peopleChoice{"К факультетам", "/people"}
		view.ShowPeople = true
		if err := v.searchPeople(r, &view); err != nil {
			participantError(w, err)
			return
		}
	} else if faculty == "" {
		rows, err := v.db.QueryContext(r.Context(), `
          SELECT code,short_name FROM faculties
          ORDER BY array_position(ARRAY['ktu','ftmi','ftmf','tint','nozh'],code), short_name`)
		if err != nil {
			participantError(w, err)
			return
		}
		defer rows.Close()
		for rows.Next() {
			var code, label string
			if err := rows.Scan(&code, &label); err != nil {
				participantError(w, err)
				return
			}
			view.Choices = append(view.Choices, peopleChoice{label, peopleURL(code, "", "")})
		}
		if err := rows.Err(); err != nil {
			participantError(w, err)
			return
		}
	} else if err := v.peopleSelection(r, &view, faculty, role, group); err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := votingPages.ExecuteTemplate(w, "people", view); err != nil {
		log.Printf("people template: %v", err)
	}
}

func (v votingEvent) searchPeople(r *http.Request, view *peopleView) error {
	query := strings.ReplaceAll(strings.ToLower(view.Query), "ё", "е")
	rows, err := v.db.QueryContext(r.Context(), `
		SELECT p.id,p.display_name,f.short_name,p.photo_jpeg IS NOT NULL
		FROM participants p JOIN events e ON e.id=p.event_id
		JOIN faculties f ON f.code=p.faculty_code
		WHERE `+activeEventFilter+` AND p.registration_submission IS NOT NULL
		  AND NOT EXISTS (
		      SELECT 1 FROM unnest(string_to_array($2,' ')) AS q(term)
		      WHERE strpos(replace(lower(p.display_name),'ё','е'),q.term)=0
		  )
		ORDER BY p.display_name,p.id`, v.slug, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var person participant
		if err := rows.Scan(&person.ID, &person.Name, &person.FacultyShort, &person.HasPhoto); err != nil {
			return err
		}
		view.People = append(view.People, person)
	}
	return rows.Err()
}

func (v votingEvent) peopleSelection(r *http.Request, view *peopleView, faculty, role, group string) error {
	var facultyName string
	if err := v.db.QueryRowContext(r.Context(), "SELECT short_name FROM faculties WHERE code=$1", faculty).Scan(&facultyName); err != nil {
		return err
	}
	view.Title = facultyName
	view.Back = peopleChoice{"К факультетам", "/people"}
	if role == "" {
		for _, item := range peopleRoles {
			view.Choices = append(view.Choices, peopleChoice{item.Label, peopleURL(faculty, item.Code, "")})
		}
		return nil
	}
	var roleName string
	for _, item := range peopleRoles {
		if item.Code == role {
			roleName = item.Label
			break
		}
	}
	if roleName == "" {
		return sql.ErrNoRows
	}
	view.Title, view.Breadcrumb = roleName, facultyName
	view.Back = peopleChoice{"К ролям", peopleURL(faculty, "", "")}
	const matchingRole = ` FROM participants p JOIN events e ON e.id=p.event_id
        JOIN candidacies c ON c.event_id=p.event_id AND c.participant_id=p.id
        WHERE ` + activeEventFilter + ` AND p.faculty_code=$2 AND c.nomination=$3
          AND p.registration_submission IS NOT NULL`
	if group == "" {
		view.Choices = []peopleChoice{
			{"Мальчики", peopleURL(faculty, role, "boy")},
			{"Девочки", peopleURL(faculty, role, "girl")},
		}
		var unassigned bool
		if err := v.db.QueryRowContext(r.Context(), "SELECT EXISTS(SELECT 1 "+matchingRole+" AND p.directory_group IS NULL)", v.slug, faculty, role).Scan(&unassigned); err != nil {
			return err
		}
		if unassigned {
			view.Choices = append(view.Choices, peopleChoice{"Без группы", peopleURL(faculty, role, "unassigned")})
		}
		return nil
	}
	groupLabel, ok := map[string]string{"boy": "Мальчики", "girl": "Девочки", "unassigned": "Без группы"}[group]
	if !ok {
		return sql.ErrNoRows
	}
	view.Title = groupLabel
	view.Breadcrumb = facultyName + " · " + roleName
	view.Back = peopleChoice{"К выбору группы", peopleURL(faculty, role, "")}
	view.ShowPeople = true
	rows, err := v.db.QueryContext(r.Context(), `SELECT p.id,p.display_name,p.photo_jpeg IS NOT NULL,coalesce(c.character_name,'')`+
		matchingRole+` AND p.directory_group IS NOT DISTINCT FROM nullif($4,'unassigned') ORDER BY p.display_name,p.id`, v.slug, faculty, role, group)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var person participant
		if err := rows.Scan(&person.ID, &person.Name, &person.HasPhoto, &person.Character); err != nil {
			return err
		}
		view.People = append(view.People, person)
	}
	return rows.Err()
}

func (v votingEvent) peoplePhoto(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex")
	var photo []byte
	err := v.db.QueryRowContext(r.Context(), `
 SELECT p.photo_jpeg FROM participants p JOIN events e ON e.id=p.event_id
 WHERE `+activeEventFilter+` AND p.id=$2 AND p.registration_submission IS NOT NULL AND p.photo_jpeg IS NOT NULL`, v.slug, pathID(r, "person")).Scan(&photo)
	if errors.Is(err, sql.ErrNoRows) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(photo)
}

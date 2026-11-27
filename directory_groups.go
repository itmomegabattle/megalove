package main

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

type directoryGroupRow struct {
	ID                   int64
	Name, Faculty, Group string
	HasPhoto             bool
}

func (a participantAdmin) directoryGroups(w http.ResponseWriter, r *http.Request) {
	event, err := a.event(r.Context(), pathID(r, "event"))
	if err != nil {
		participantError(w, err)
		return
	}
	rows, err := a.db.QueryContext(r.Context(), `
		SELECT p.id,p.display_name,f.short_name,coalesce(p.directory_group,''),p.photo_jpeg IS NOT NULL
		FROM participants p JOIN faculties f ON f.code=p.faculty_code
		WHERE p.event_id=$1 ORDER BY f.short_name,p.display_name,p.id`, event.ID)
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	view := struct {
		Event  eventInfo
		People []directoryGroupRow
		Saved  bool
	}{Event: event, Saved: r.URL.Query().Get("saved") == "1"}
	for rows.Next() {
		var row directoryGroupRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Faculty, &row.Group, &row.HasPhoto); err != nil {
			participantError(w, err)
			return
		}
		view.People = append(view.People, row)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := participantPages.ExecuteTemplate(w, "directory-groups", view); err != nil {
		log.Printf("directory groups template: %v", err)
	}
}

func (a participantAdmin) assignDirectoryGroups(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Не удалось прочитать назначения.", http.StatusBadRequest)
		return
	}
	type assignment struct {
		ID    int64  `json:"id"`
		Group string `json:"group_code"`
	}
	var updates []assignment
	for key, values := range r.PostForm {
		number, ok := strings.CutPrefix(key, "group-")
		id, err := strconv.ParseInt(number, 10, 64)
		if !ok || err != nil || id < 1 || len(values) != 1 || (values[0] != "boy" && values[0] != "girl") {
			http.Error(w, "Неверное назначение группы.", http.StatusBadRequest)
			return
		}
		updates = append(updates, assignment{id, values[0]})
	}
	eventID := pathID(r, "event")
	if len(updates) > 0 {
		data, err := json.Marshal(updates)
		if err != nil {
			participantError(w, err)
			return
		}
		if _, err := a.db.ExecContext(r.Context(), `
			UPDATE participants p SET directory_group = a.group_code
			FROM jsonb_to_recordset($2::jsonb) AS a(id bigint,group_code text)
			WHERE p.event_id=$1 AND p.id=a.id AND p.directory_group IS DISTINCT FROM a.group_code`, eventID, string(data)); err != nil {
			participantError(w, err)
			return
		}
	}
	http.Redirect(w, r, "/admin/events/"+strconv.FormatInt(eventID, 10)+"/participants/groups?saved=1", http.StatusSeeOther)
}

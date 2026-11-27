package main

import (
	_ "embed"
	"html/template"
	"net/http"
)

//go:embed translation.html
var translationHTML string

//go:embed faculty_chart.html
var facultyChartHTML string

var facultyChartTemplate = template.Must(template.New("faculty-chart").Parse(facultyChartHTML))

func translationPage(w http.ResponseWriter, r *http.Request) {
	translationHeaders(w)
	w.Write([]byte(translationHTML))
}

func facultyChartPage(nomination, title string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		translationHeaders(w)
		if err := facultyChartTemplate.Execute(w, struct{ Nomination, Title string }{nomination, title}); err != nil {
			panic(http.ErrAbortHandler)
		}
	}
}

func translationHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; base-uri 'none'; frame-ancestors 'none'")
}

func translationRunKind(r *http.Request) string {
	if r.URL.Query().Get("mode") == "test" {
		return "test"
	}
	return "main"
}

type translationPlace struct {
	Faculty    string `json:"faculty"`
	Place      int    `json:"place"`
	TotalVotes int64  `json:"totalVotes"`
}

func (v votingEvent) translationRanks(w http.ResponseWriter, r *http.Request) {
	rows, err := v.db.QueryContext(r.Context(), `
		WITH current_run AS (
			SELECT vr.id FROM events e `+latestRunJoin+` WHERE `+activeEventFilter+`
		),
		candidate_votes AS (
			SELECT choice.nomination, choice.participant_id, count(*) AS votes
			FROM ballots b JOIN current_run r ON r.id=b.run_id
			CROSS JOIN LATERAL (VALUES
				('dancer',b.dancer_id), ('vocalist',b.vocalist_id), ('actor',b.actor_id)
			) AS choice(nomination,participant_id)
			GROUP BY choice.nomination,choice.participant_id
		),
		faculty_scores AS (
			SELECT c.nomination,p.faculty_code,max(c.votes) AS votes,sum(c.votes)::bigint AS total_votes
			FROM candidate_votes c CROSS JOIN current_run r
			JOIN run_participants p ON p.run_id=r.id AND p.participant_id=c.participant_id
			GROUP BY c.nomination,p.faculty_code
		),
		ranked AS (
			SELECT n.code,f.short_name,coalesce(s.votes,0) AS votes,coalesce(s.total_votes,0) AS total_votes,
			       rank() OVER (PARTITION BY n.code ORDER BY coalesce(s.votes,0) DESC) AS place
			FROM (VALUES ('dancer'),('vocalist'),('actor')) AS n(code)
			CROSS JOIN faculties f
			LEFT JOIN faculty_scores s ON s.nomination=n.code AND s.faculty_code=f.code
		)
		SELECT code,short_name,CASE WHEN votes>0 THEN place ELSE 0 END,total_votes
		FROM ranked ORDER BY code,votes DESC,short_name`, v.slug, translationRunKind(r))
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	ranks := make(map[string][]translationPlace, 3)
	for rows.Next() {
		var code string
		var place translationPlace
		if err := rows.Scan(&code, &place.Faculty, &place.Place, &place.TotalVotes); err != nil {
			participantError(w, err)
			return
		}
		ranks[code] = append(ranks[code], place)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	votingJSON(w, http.StatusOK, ranks)
}

type translationCandidate struct {
	Number int    `json:"number"`
	Name   string `json:"name"`
	Votes  int64  `json:"votes"`
}

func (v votingEvent) translationTopCandidates(w http.ResponseWriter, r *http.Request) {
	nomination := r.PathValue("nomination")
	switch nomination {
	case "dancer", "vocalist", "actor":
	default:
		http.NotFound(w, r)
		return
	}
	rows, err := v.db.QueryContext(r.Context(), `
		WITH current_run AS (
			SELECT vr.id FROM events e `+latestRunJoin+` WHERE `+activeEventFilter+`
		),
		candidates AS (
			SELECT c.participant_id,p.display_name
			FROM run_candidacies c JOIN current_run r ON r.id=c.run_id
			JOIN run_participants p ON p.run_id=c.run_id AND p.participant_id=c.participant_id
			WHERE c.nomination=$3
		),
		tally AS (
			SELECT CASE $3 WHEN 'dancer' THEN b.dancer_id WHEN 'vocalist' THEN b.vocalist_id ELSE b.actor_id END AS participant_id,
			       count(*) AS votes
			FROM ballots b JOIN current_run r ON r.id=b.run_id
			GROUP BY 1
		)
		SELECT c.participant_id,c.display_name,coalesce(t.votes,0) AS votes
		FROM candidates c LEFT JOIN tally t USING (participant_id)
		ORDER BY votes DESC,c.participant_id
		LIMIT 5`, v.slug, translationRunKind(r), nomination)
	if err != nil {
		participantError(w, err)
		return
	}
	defer rows.Close()
	top := make([]translationCandidate, 0, 5)
	for rows.Next() {
		var candidate translationCandidate
		if err := rows.Scan(&candidate.Number, &candidate.Name, &candidate.Votes); err != nil {
			participantError(w, err)
			return
		}
		top = append(top, candidate)
	}
	if err := rows.Err(); err != nil {
		participantError(w, err)
		return
	}
	votingJSON(w, http.StatusOK, top)
}

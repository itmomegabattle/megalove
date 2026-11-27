CREATE TABLE voting_runs (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id bigint NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    kind text NOT NULL CHECK (kind IN ('test', 'main')),
    started_at timestamptz NOT NULL,
    deadline timestamptz,
    closed_at timestamptz,
    UNIQUE (event_id, id)
);

CREATE UNIQUE INDEX voting_runs_one_main ON voting_runs(event_id) WHERE kind = 'main';
CREATE UNIQUE INDEX voting_runs_one_open ON voting_runs(event_id) WHERE closed_at IS NULL;

-- The historical roster survives later edits to the registration records.
CREATE TABLE run_participants (
    run_id bigint NOT NULL REFERENCES voting_runs(id) ON DELETE CASCADE,
    participant_id bigint NOT NULL,
    display_name text NOT NULL,
    faculty_code text NOT NULL REFERENCES faculties(code),
    photo_jpeg bytea,
    PRIMARY KEY (run_id, participant_id)
);

CREATE TABLE run_candidacies (
    run_id bigint NOT NULL,
    participant_id bigint NOT NULL,
    nomination text NOT NULL CHECK (nomination IN ('dancer','vocalist','actor')),
    character_name text,
    PRIMARY KEY (run_id, participant_id, nomination),
    FOREIGN KEY (run_id, participant_id)
        REFERENCES run_participants(run_id, participant_id) ON DELETE CASCADE
);

INSERT INTO voting_runs(event_id, kind, started_at, deadline, closed_at)
SELECT e.id, 'main', coalesce(
    (SELECT min(changed_at) FROM event_state_changes WHERE event_id=e.id AND to_state='open'),
    (SELECT min(accepted_at) FROM ballots WHERE event_id=e.id), now()),
    e.voting_ends_at,
    CASE WHEN e.state='closed' THEN coalesce(
        (SELECT max(changed_at) FROM event_state_changes WHERE event_id=e.id AND to_state='closed'), now())
        WHEN e.voting_ends_at <= now() THEN e.voting_ends_at END
FROM events e
WHERE e.state <> 'preparing' OR EXISTS (SELECT 1 FROM ballots WHERE event_id=e.id);

INSERT INTO run_participants
SELECT r.id, p.id, p.display_name, p.faculty_code, p.photo_jpeg
FROM voting_runs r JOIN participants p ON p.event_id=r.event_id;
INSERT INTO run_candidacies
SELECT r.id, c.participant_id, c.nomination, c.character_name
FROM voting_runs r JOIN candidacies c ON c.event_id=r.event_id;

ALTER TABLE ballots ADD COLUMN run_id bigint;
UPDATE ballots b SET run_id=r.id FROM voting_runs r WHERE r.event_id=b.event_id;
ALTER TABLE ballots ALTER COLUMN run_id SET NOT NULL;
ALTER TABLE ballots DROP CONSTRAINT ballots_pkey,
    DROP CONSTRAINT ballots_event_id_dancer_id_dancer_nomination_fkey,
    DROP CONSTRAINT ballots_event_id_vocalist_id_vocalist_nomination_fkey,
    DROP CONSTRAINT ballots_event_id_actor_id_actor_nomination_fkey;
ALTER TABLE ballots ADD PRIMARY KEY (run_id, browser_id),
    ADD FOREIGN KEY (event_id, run_id) REFERENCES voting_runs(event_id, id),
    ADD FOREIGN KEY (run_id, dancer_id, dancer_nomination)
        REFERENCES run_candidacies(run_id, participant_id, nomination),
    ADD FOREIGN KEY (run_id, vocalist_id, vocalist_nomination)
        REFERENCES run_candidacies(run_id, participant_id, nomination),
    ADD FOREIGN KEY (run_id, actor_id, actor_nomination)
        REFERENCES run_candidacies(run_id, participant_id, nomination);
CREATE INDEX ballots_run_time ON ballots(run_id, accepted_at);

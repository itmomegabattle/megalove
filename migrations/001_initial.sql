CREATE TABLE events (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    slug text NOT NULL UNIQUE CHECK (btrim(slug) <> ''),
    name text NOT NULL CHECK (btrim(name) <> ''),
    state text NOT NULL DEFAULT 'preparing' CHECK (state IN ('preparing', 'open', 'closed'))
);

CREATE TABLE event_state_changes (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id bigint NOT NULL REFERENCES events (id),
    from_state text NOT NULL CHECK (from_state IN ('preparing', 'open', 'closed')),
    to_state text NOT NULL CHECK (to_state IN ('open', 'closed')),
    changed_at timestamptz NOT NULL DEFAULT now(),
    CHECK (from_state <> to_state)
);

CREATE TABLE faculties (
    code text PRIMARY KEY,
    short_name text NOT NULL UNIQUE,
    full_name text NOT NULL UNIQUE
);

INSERT INTO faculties (code, short_name, full_name) VALUES
    ('ktu', 'КТУ', 'Мегафакультет компьютерных технологий и управления'),
    ('tint', 'ТИНТ', 'Мегафакультет трансляционных и информационных технологий'),
    ('nozh', 'НОЖ', 'Мегафакультет наук о жизни'),
    ('ftmf', 'ФТМФ', 'Физико-технический мегафакультет'),
    ('ftmi', 'ФТМИ', 'Мегафакультет технологического менеджмента и инноваций');

CREATE TABLE participants (
    id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    event_id bigint NOT NULL REFERENCES events (id),
    faculty_code text NOT NULL REFERENCES faculties (code),
    display_name text NOT NULL CHECK (btrim(display_name) <> ''),
    photo_path text,
    UNIQUE (event_id, id)
);

CREATE TABLE candidacies (
    event_id bigint NOT NULL,
    participant_id bigint NOT NULL,
    nomination text NOT NULL CHECK (nomination IN ('dancer', 'vocalist', 'actor')),
    performance text GENERATED ALWAYS AS (
        CASE WHEN nomination = 'actor' THEN 'theater' ELSE 'showcase' END
    ) STORED,
    character_name text CHECK (
        character_name IS NULL OR (nomination = 'actor' AND btrim(character_name) <> '')
    ),
    PRIMARY KEY (event_id, participant_id, nomination),
    FOREIGN KEY (event_id, participant_id) REFERENCES participants (event_id, id)
);

CREATE TABLE ballots (
    event_id bigint NOT NULL REFERENCES events (id),
    browser_id uuid NOT NULL,
    accepted_at timestamptz NOT NULL DEFAULT now(),
    dancer_id bigint NOT NULL,
    vocalist_id bigint NOT NULL,
    actor_id bigint NOT NULL,
    dancer_nomination text GENERATED ALWAYS AS ('dancer'::text) STORED,
    vocalist_nomination text GENERATED ALWAYS AS ('vocalist'::text) STORED,
    actor_nomination text GENERATED ALWAYS AS ('actor'::text) STORED,
    PRIMARY KEY (event_id, browser_id),
    FOREIGN KEY (event_id, dancer_id, dancer_nomination)
        REFERENCES candidacies (event_id, participant_id, nomination),
    FOREIGN KEY (event_id, vocalist_id, vocalist_nomination)
        REFERENCES candidacies (event_id, participant_id, nomination),
    FOREIGN KEY (event_id, actor_id, actor_nomination)
        REFERENCES candidacies (event_id, participant_id, nomination)
);

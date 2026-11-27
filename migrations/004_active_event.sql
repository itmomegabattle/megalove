CREATE TABLE active_voting_event (
    singleton boolean PRIMARY KEY CHECK (singleton),
    event_id bigint REFERENCES events (id)
);

INSERT INTO active_voting_event (singleton) VALUES (true);

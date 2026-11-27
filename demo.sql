DO $$
DECLARE
    demo_event bigint;
    demo_person bigint;
    entry record;
BEGIN
    IF current_database() <> 'megalove_demo' THEN
        RAISE EXCEPTION 'Демоданные разрешены только в базе megalove_demo';
    END IF;

    INSERT INTO events (slug, name)
    VALUES ('demo', 'Мегабаттл — демонстрация')
    ON CONFLICT (slug) DO NOTHING
    RETURNING id INTO demo_event;

    IF demo_event IS NULL THEN
        RETURN;
    END IF;

    FOR entry IN SELECT * FROM (VALUES
        ('ktu', 'Анна Волкова', 'dancer', NULL),
        ('tint', 'Полина Егорова', 'vocalist', NULL),
        ('nozh', 'Дарья Лебедева', 'actor', 'Дед Савелий'),
        ('ftmf', 'Илья Котов', 'vocalist', NULL),
        ('ftmi', 'Мария Белова', 'actor', NULL)
    ) AS sample(faculty, display_name, nomination, character_name)
    LOOP
        INSERT INTO participants (event_id, faculty_code, display_name)
        VALUES (demo_event, entry.faculty, entry.display_name)
        RETURNING id INTO demo_person;

        INSERT INTO candidacies (event_id, participant_id, nomination, character_name)
        VALUES (demo_event, demo_person, entry.nomination, entry.character_name);
    END LOOP;
END $$;

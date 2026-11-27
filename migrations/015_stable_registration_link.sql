ALTER TABLE events ADD COLUMN registration_key text;
UPDATE events SET registration_key = coalesce(registration_token,
    replace(gen_random_uuid()::text, '-', '') || left(replace(gen_random_uuid()::text, '-', ''), 16));
ALTER TABLE events
    ALTER COLUMN registration_key SET DEFAULT
        (replace(gen_random_uuid()::text, '-', '') || left(replace(gen_random_uuid()::text, '-', ''), 16)),
    ALTER COLUMN registration_key SET NOT NULL,
    ADD UNIQUE (registration_key);

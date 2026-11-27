ALTER TABLE participants ADD COLUMN photo_jpeg bytea;

INSERT INTO events (slug, name)
SELECT 'megabattle-2026', 'Мегабаттл 2026'
WHERE current_database() <> 'megalove_demo'
ON CONFLICT (slug) DO NOTHING;

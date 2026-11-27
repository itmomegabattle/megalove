CREATE TABLE backup_receipts (
    singleton boolean PRIMARY KEY DEFAULT true CHECK(singleton),
    snapshot_at timestamptz NOT NULL
);

ALTER TABLE participants ADD COLUMN directory_group text
    CHECK (directory_group IN ('boy', 'girl'));

ALTER TABLE platform.outbox_events
    ADD COLUMN purpose text NOT NULL DEFAULT 'operational' CHECK (purpose IN ('operational', 'optional'));
UPDATE platform.schema_stamp SET version = 6 WHERE singleton;

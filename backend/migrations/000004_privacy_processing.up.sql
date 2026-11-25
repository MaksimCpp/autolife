ALTER TABLE platform.privacy_jobs
    ADD COLUMN delete_receipt_hash bytea CHECK (octet_length(delete_receipt_hash) = 32),
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now(),
    ADD CHECK (type = 'delete' OR delete_receipt_hash IS NULL);
CREATE UNIQUE INDEX privacy_delete_receipt ON platform.privacy_jobs (delete_receipt_hash)
    WHERE delete_receipt_hash IS NOT NULL;
CREATE INDEX privacy_pending_idx ON platform.privacy_jobs (next_attempt_at,id) WHERE state='requested';
CREATE INDEX outbox_owner_idx ON platform.outbox_events ((payload->>'owner_id'));
UPDATE platform.schema_stamp SET version = 4 WHERE singleton;

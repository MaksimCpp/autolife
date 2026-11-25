DROP INDEX platform.privacy_pending_idx, platform.privacy_delete_receipt, platform.outbox_owner_idx;
ALTER TABLE platform.privacy_jobs DROP COLUMN delete_receipt_hash, DROP COLUMN next_attempt_at;
UPDATE platform.schema_stamp SET version = 3 WHERE singleton;

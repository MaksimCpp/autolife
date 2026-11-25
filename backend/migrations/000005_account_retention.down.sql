DROP FUNCTION platform.prune_account_history();
UPDATE platform.schema_stamp SET version = 4 WHERE singleton;

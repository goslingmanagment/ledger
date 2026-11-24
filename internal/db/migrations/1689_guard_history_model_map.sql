-- Migration 1690 expands the optional model map while backfilling old bookings.
-- Give that one historical query an empty object for invalid JSON and restore the
-- original setting afterwards. Already-upgraded databases need no adjustment.
CREATE TABLE migration_pot_model_map(value TEXT NOT NULL);
INSERT INTO migration_pot_model_map
SELECT value FROM settings WHERE key='hub_model_slugs' AND NOT json_valid(value)
 AND NOT EXISTS(SELECT 1 FROM schema_migrations WHERE name='1690_hub_pot_distributions');
UPDATE settings SET value='{}' WHERE key='hub_model_slugs' AND EXISTS(SELECT 1 FROM migration_pot_model_map);

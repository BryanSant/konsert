-- 008: normalize every stored timestamp to the fixed-width UTC layout
-- 2006-01-02T15:04:05.000000000Z (always 30 characters), so text comparison and
-- ORDER BY agree with chronological order. Legacy rows were written with
-- time.RFC3339Nano, which strips trailing fractional zeros ("...05Z",
-- "...05.5Z"), making "...05.5Z" sort before "...05Z".
--
-- Only values that look like a legacy UTC RFC3339 timestamp are rewritten:
-- 'YYYY-MM-DDTHH:MM:SS' followed by 'Z' or by '.' + 1-8 digits + 'Z'.

UPDATE workers SET last_seen_at =
    substr(last_seen_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(last_seen_at, 20, 1) = '.' THEN substr(last_seen_at, 21, length(last_seen_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE last_seen_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(last_seen_at) BETWEEN 20 AND 29
  AND (length(last_seen_at) = 20 OR (substr(last_seen_at, 20, 1) = '.' AND substr(last_seen_at, 21, length(last_seen_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE workers SET facts_updated_at =
    substr(facts_updated_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(facts_updated_at, 20, 1) = '.' THEN substr(facts_updated_at, 21, length(facts_updated_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE facts_updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(facts_updated_at) BETWEEN 20 AND 29
  AND (length(facts_updated_at) = 20 OR (substr(facts_updated_at, 20, 1) = '.' AND substr(facts_updated_at, 21, length(facts_updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE tasks SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE tasks SET started_at =
    substr(started_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(started_at, 20, 1) = '.' THEN substr(started_at, 21, length(started_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE started_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(started_at) BETWEEN 20 AND 29
  AND (length(started_at) = 20 OR (substr(started_at, 20, 1) = '.' AND substr(started_at, 21, length(started_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE tasks SET completed_at =
    substr(completed_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(completed_at, 20, 1) = '.' THEN substr(completed_at, 21, length(completed_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE completed_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(completed_at) BETWEEN 20 AND 29
  AND (length(completed_at) = 20 OR (substr(completed_at, 20, 1) = '.' AND substr(completed_at, 21, length(completed_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE task_events SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE chat_sessions SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE chat_sessions SET updated_at =
    substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(updated_at) BETWEEN 20 AND 29
  AND (length(updated_at) = 20 OR (substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE chat_messages SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE usage_events SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE spaces SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE settings SET updated_at =
    substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(updated_at) BETWEEN 20 AND 29
  AND (length(updated_at) = 20 OR (substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE workspaces SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE projects SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE repos SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE worktrees SET created_at =
    substr(created_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(created_at, 20, 1) = '.' THEN substr(created_at, 21, length(created_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE created_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(created_at) BETWEEN 20 AND 29
  AND (length(created_at) = 20 OR (substr(created_at, 20, 1) = '.' AND substr(created_at, 21, length(created_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE orchestrator_memory SET updated_at =
    substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(updated_at) BETWEEN 20 AND 29
  AND (length(updated_at) = 20 OR (substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE subscription_quotas SET reset_at =
    substr(reset_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(reset_at, 20, 1) = '.' THEN substr(reset_at, 21, length(reset_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE reset_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(reset_at) BETWEEN 20 AND 29
  AND (length(reset_at) = 20 OR (substr(reset_at, 20, 1) = '.' AND substr(reset_at, 21, length(reset_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE subscription_quotas SET updated_at =
    substr(updated_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(updated_at, 20, 1) = '.' THEN substr(updated_at, 21, length(updated_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE updated_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(updated_at) BETWEEN 20 AND 29
  AND (length(updated_at) = 20 OR (substr(updated_at, 20, 1) = '.' AND substr(updated_at, 21, length(updated_at) - 21) NOT GLOB '*[^0-9]*'));

UPDATE schema_migrations SET applied_at =
    substr(applied_at, 1, 19) || '.' ||
    substr((CASE WHEN substr(applied_at, 20, 1) = '.' THEN substr(applied_at, 21, length(applied_at) - 21) ELSE '' END) || '000000000', 1, 9) || 'Z'
WHERE applied_at GLOB '[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9][0-9]:[0-9][0-9]:[0-9][0-9]*Z'
  AND length(applied_at) BETWEEN 20 AND 29
  AND (length(applied_at) = 20 OR (substr(applied_at, 20, 1) = '.' AND substr(applied_at, 21, length(applied_at) - 21) NOT GLOB '*[^0-9]*'));

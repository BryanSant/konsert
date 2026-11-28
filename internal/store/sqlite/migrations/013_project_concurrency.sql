-- Per-project concurrency cap (SYR-WZGZ6): at most this many of the project's tasks and
-- ticket runs at once across the fleet; 0 = no cap. Extra work waits in the queue.

ALTER TABLE projects ADD COLUMN max_concurrent INTEGER NOT NULL DEFAULT 0;

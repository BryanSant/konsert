-- Machine policy (SYR-BJDY6): a machine's standing rules for new work, as
-- protocol.MachinePolicy JSON ('' = none). Set by the user in the UI or CLI, audited.

ALTER TABLE workers ADD COLUMN policy_json TEXT NOT NULL DEFAULT '';

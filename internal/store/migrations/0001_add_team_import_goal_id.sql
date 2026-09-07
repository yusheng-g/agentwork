-- Links the team_import tracking row to the system-task goal that
-- drives the import. The goal provides goal-plane visibility (timeline,
-- feed, status) while team_import retains the git config storage.
ALTER TABLE team_import ADD COLUMN goal_id TEXT NOT NULL DEFAULT '';

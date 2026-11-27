-- migrate:up
-- MySQL only: these columns are TEXT here, with no width to widen. The
-- version exists in every dialect so the migration sets stay identical.
SELECT 1;

-- migrate:down
SELECT 1;

-- History must never be rewritten: reject any UPDATE or DELETE on the ledger.
-- Until now "append-only" was only a convention in the Go code; this makes
-- the database enforce it.
--
-- Limitation: row triggers don't fire on TRUNCATE (the test harness relies on
-- that to reset tables). Real protection against an operator would come from
-- database permissions, which this project doesn't set up.
CREATE FUNCTION reject_ledger_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not allowed', TG_OP;
END;
$$;

CREATE TRIGGER ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION reject_ledger_change();

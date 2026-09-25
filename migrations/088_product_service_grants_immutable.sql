-- A Platform OTA period seal relies on the complete historical Product grant
-- sequence. Backfill and normal Product edits insert new revisions; neither
-- operation needs to update or delete an existing grant.
CREATE OR REPLACE FUNCTION reject_product_service_grant_mutation()
RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'Product service grant history is immutable' USING ERRCODE = '23514';
END;
$$;

CREATE TRIGGER product_service_grants_immutable
BEFORE UPDATE OR DELETE ON product_service_grants
FOR EACH ROW EXECUTE FUNCTION reject_product_service_grant_mutation();

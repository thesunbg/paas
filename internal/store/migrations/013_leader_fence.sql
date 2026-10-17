-- Leader fencing. A new leader bumps leader.epoch (TakeLeadership); every write of the
-- previous leader runs paas_fence(its epoch) first and fails with PF001 once superseded.
-- The shared xact lock is held to commit and the bump takes it exclusively, so a fenced
-- tx that passed the check is finished before the new epoch becomes visible.
CREATE TABLE leader (id int PRIMARY KEY CHECK (id = 1), epoch bigint NOT NULL);
INSERT INTO leader VALUES (1, 0);

CREATE FUNCTION paas_fence(n bigint) RETURNS void LANGUAGE plpgsql VOLATILE AS $$
BEGIN
	PERFORM pg_advisory_xact_lock_shared(7313002);
	-- separate statement: fresh snapshot, taken after the lock wait
	IF (SELECT epoch FROM leader WHERE id = 1) <> n THEN
		RAISE EXCEPTION 'paas: fenced (epoch %)', n USING ERRCODE = 'PF001';
	END IF;
END $$;

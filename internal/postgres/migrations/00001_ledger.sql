-- Ledger schema. The database itself enforces the ledger invariants, so a bug
-- in the application (or a stray manual query) cannot silently corrupt money:
--
--   * entries and transfers are append-only (UPDATE, DELETE, TRUNCATE rejected)
--   * every transfer has exactly two entries, a debit and a credit, summing to 0
--   * each entry's balance_after continues the account's previous balance
--   * an account's cached balance equals its latest balance_after at commit
--   * non-system accounts can never go negative
--   * account, transfer and entry currencies always agree
--   * an idempotency key can create at most one transfer, ever
--
-- Names are unqualified so the schema can be installed into any search_path
-- (the test suite installs one copy per test).

-- +goose Up

CREATE TABLE accounts (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    currency    text        NOT NULL,
    is_system   boolean     NOT NULL DEFAULT false,
    balance     bigint      NOT NULL DEFAULT 0,
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT accounts_currency_format CHECK (currency ~ '^[A-Z]{3}$'),
    -- Second line of defence against overdrafts; the first is the balance check
    -- made while holding the row lock.
    CONSTRAINT accounts_balance_non_negative CHECK (is_system OR balance >= 0),
    -- Target of the composite foreign keys that keep currencies consistent.
    CONSTRAINT accounts_id_currency_key UNIQUE (id, currency)
);

-- Money enters and leaves the ledger only through one system account per
-- currency. Its balance is the negated sum of all user balances.
CREATE UNIQUE INDEX accounts_one_system_account_per_currency
    ON accounts (currency) WHERE is_system;

CREATE TABLE transfers (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind             text        NOT NULL,
    idempotency_key  text,
    request_hash     bytea,
    from_account_id  uuid        NOT NULL,
    to_account_id    uuid        NOT NULL,
    amount           bigint      NOT NULL,
    currency         text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT transfers_kind_valid CHECK (kind IN ('transfer', 'funding')),
    CONSTRAINT transfers_amount_positive CHECK (amount > 0),
    CONSTRAINT transfers_distinct_accounts CHECK (from_account_id <> to_account_id),
    -- Client transfers always carry their idempotency key and request hash.
    CONSTRAINT transfers_key_and_hash_together CHECK ((idempotency_key IS NULL) = (request_hash IS NULL)),
    CONSTRAINT transfers_client_transfer_has_key CHECK (kind <> 'transfer' OR idempotency_key IS NOT NULL),
    -- Permanent exactly-once guard: idempotency records expire (TTL), this does not.
    CONSTRAINT transfers_idempotency_key_key UNIQUE (idempotency_key),
    CONSTRAINT transfers_from_account_fk FOREIGN KEY (from_account_id, currency) REFERENCES accounts (id, currency),
    CONSTRAINT transfers_to_account_fk FOREIGN KEY (to_account_id, currency) REFERENCES accounts (id, currency)
);

-- Signed amounts: a debit (money leaving the account) is negative, a credit
-- is positive, so the two entries of a transfer sum to zero.
CREATE TABLE entries (
    id             bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    transfer_id    uuid        NOT NULL REFERENCES transfers (id),
    account_id     uuid        NOT NULL,
    currency       text        NOT NULL,
    amount         bigint      NOT NULL,
    balance_after  bigint      NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT entries_amount_non_zero CHECK (amount <> 0),
    CONSTRAINT entries_account_fk FOREIGN KEY (account_id, currency) REFERENCES accounts (id, currency),
    CONSTRAINT entries_one_per_account_per_transfer UNIQUE (transfer_id, account_id)
);

CREATE INDEX entries_account_id_id_idx ON entries (account_id, id);

-- Stored responses of idempotent requests. The key record is written in the
-- same transaction as the work it describes, so it exists if and only if the
-- work committed. Rows expire after a TTL and are deleted by a cleanup job.
CREATE TABLE idempotency_keys (
    key              text        PRIMARY KEY,
    request_method   text        NOT NULL,
    request_path     text        NOT NULL,
    request_hash     bytea       NOT NULL,
    response_status  integer     NOT NULL,
    response_body    bytea       NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL,
    CONSTRAINT idempotency_keys_key_length CHECK (char_length(key) BETWEEN 1 AND 255),
    CONSTRAINT idempotency_keys_hash_length CHECK (octet_length(request_hash) = 32),
    CONSTRAINT idempotency_keys_status_valid CHECK (response_status BETWEEN 100 AND 599),
    CONSTRAINT idempotency_keys_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);

-- Append-only enforcement -----------------------------------------------------

-- +goose StatementBegin
CREATE FUNCTION ledger_reject_mutation() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger table % is append-only: % is not allowed', TG_TABLE_NAME, TG_OP
        USING ERRCODE = 'integrity_constraint_violation';
END
$$;
-- +goose StatementEnd

CREATE TRIGGER entries_append_only
    BEFORE UPDATE OR DELETE ON entries
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER entries_no_truncate
    BEFORE TRUNCATE ON entries
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER transfers_append_only
    BEFORE UPDATE OR DELETE ON transfers
    FOR EACH ROW EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER transfers_no_truncate
    BEFORE TRUNCATE ON transfers
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();
CREATE TRIGGER accounts_no_truncate
    BEFORE TRUNCATE ON accounts
    FOR EACH STATEMENT EXECUTE FUNCTION ledger_reject_mutation();

-- Accounts: only the balance may change, and accounts are never deleted.
-- +goose StatementBegin
CREATE FUNCTION ledger_protect_account() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'accounts cannot be deleted (account %)', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.currency IS DISTINCT FROM OLD.currency
       OR NEW.is_system IS DISTINCT FROM OLD.is_system
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'only the balance of account % may change', OLD.id
            USING ERRCODE = 'integrity_constraint_violation';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER accounts_protect
    BEFORE UPDATE OR DELETE ON accounts
    FOR EACH ROW EXECUTE FUNCTION ledger_protect_account();

-- Double-entry enforcement ------------------------------------------------------

-- A transfer must consist of exactly one debit of -amount on the source account
-- and one credit of +amount on the destination account. Checked at commit
-- (deferred), after both entries exist, from both tables so that neither a
-- transfer without entries nor an extra entry for an old transfer gets in.
-- +goose StatementBegin
CREATE FUNCTION ledger_assert_transfer_balanced(p_transfer_id uuid) RETURNS void
LANGUAGE plpgsql AS $$
DECLARE
    t        transfers%ROWTYPE;
    n        bigint;
    total    numeric;
    debited  boolean;
    credited boolean;
BEGIN
    SELECT * INTO t FROM transfers WHERE id = p_transfer_id;
    SELECT count(*),
           coalesce(sum(e.amount), 0),
           coalesce(bool_or(e.account_id = t.from_account_id AND e.amount = -t.amount), false),
           coalesce(bool_or(e.account_id = t.to_account_id AND e.amount = t.amount), false)
      INTO n, total, debited, credited
      FROM entries e
     WHERE e.transfer_id = p_transfer_id;
    IF n <> 2 OR total <> 0 OR NOT debited OR NOT credited THEN
        RAISE EXCEPTION 'transfer % is not a balanced double entry (% entries, sum %)', p_transfer_id, n, total
            USING ERRCODE = 'check_violation';
    END IF;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger_check_transfer_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger_assert_transfer_balanced(NEW.id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_transfer_balanced() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    PERFORM ledger_assert_transfer_balanced(NEW.transfer_id);
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER transfers_balanced
    AFTER INSERT ON transfers
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_transfer_balanced();
CREATE CONSTRAINT TRIGGER entries_balanced
    AFTER INSERT ON entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_transfer_balanced();

-- Running-balance chain: an entry's balance_after must equal the account's
-- previous balance_after plus the entry amount. The account row is locked first
-- so concurrent writers to the same account are serialized even if they forgot
-- to lock it themselves.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_entry_chain() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    previous bigint;
BEGIN
    PERFORM 1 FROM accounts WHERE id = NEW.account_id FOR UPDATE;
    SELECT e.balance_after INTO previous
      FROM entries e
     WHERE e.account_id = NEW.account_id
     ORDER BY e.id DESC
     LIMIT 1;
    IF NEW.balance_after <> coalesce(previous, 0) + NEW.amount THEN
        RAISE EXCEPTION 'entry for account % has balance_after %, expected % + %',
            NEW.account_id, NEW.balance_after, coalesce(previous, 0), NEW.amount
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;
-- +goose StatementEnd

CREATE TRIGGER entries_chain
    BEFORE INSERT ON entries
    FOR EACH ROW EXECUTE FUNCTION ledger_check_entry_chain();

-- The cached balance must equal the latest balance_after (0 without entries),
-- so "balance is the sum of the account's entries" holds at every commit.
-- +goose StatementBegin
CREATE FUNCTION ledger_check_account_balance() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
    cached bigint;
    derived bigint;
BEGIN
    SELECT a.balance INTO cached FROM accounts a WHERE a.id = NEW.id;
    SELECT e.balance_after INTO derived
      FROM entries e
     WHERE e.account_id = NEW.id
     ORDER BY e.id DESC
     LIMIT 1;
    IF cached IS DISTINCT FROM coalesce(derived, 0) THEN
        RAISE EXCEPTION 'account % balance % does not match its entries (%)', NEW.id, cached, coalesce(derived, 0)
            USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END
$$;
-- +goose StatementEnd

CREATE CONSTRAINT TRIGGER accounts_balance_matches_entries
    AFTER INSERT OR UPDATE ON accounts
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_check_account_balance();

-- +goose Down

DROP TABLE idempotency_keys;
DROP TABLE entries;
DROP TABLE transfers;
DROP TABLE accounts;
DROP FUNCTION ledger_check_account_balance();
DROP FUNCTION ledger_check_entry_chain();
DROP FUNCTION ledger_check_entry_transfer_balanced();
DROP FUNCTION ledger_check_transfer_balanced();
DROP FUNCTION ledger_assert_transfer_balanced(uuid);
DROP FUNCTION ledger_protect_account();
DROP FUNCTION ledger_reject_mutation();

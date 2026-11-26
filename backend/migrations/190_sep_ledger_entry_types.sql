ALTER TABLE sep_ledger DROP CONSTRAINT sep_ledger_type_check;
ALTER TABLE sep_ledger ADD CONSTRAINT sep_ledger_type_check CHECK(type IN ('credit_compute','debit_compute','settlement','paid_consumption','free_record'));

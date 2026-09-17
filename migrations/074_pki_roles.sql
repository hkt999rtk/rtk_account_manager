INSERT INTO roles(name,scope_type,description,system_role)
VALUES ('pki_admin','platform','Approve and manage PKI lifecycle operations',true),
       ('security_custodian','platform','Approve offline CA custody ceremonies',true),
       ('pki_auditor','platform','Read PKI lifecycle evidence',true)
ON CONFLICT(name) DO NOTHING;

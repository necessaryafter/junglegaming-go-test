# ADR 0012 — Permissões do banco por responsabilidade

## Decisão

Separar três identidades no PostgreSQL:

- `jungle`: administrador local, usado somente pelo provisionamento e pelos testes.
- `jungle_migrator`: login sem superuser, CREATEDB, CREATEROLE, REPLICATION ou
  BYPASSRLS; proprietário do schema, tabelas e funções financeiras.
- `jungle_runtime`: login sem esses atributos, sem memberships e sem propriedade
  do banco ou dos objetos; usado pela API e por todos os workers.

O Compose executa `db-roles`, `migrate` e `db-grants` nessa ordem, antes de iniciar
a aplicação. As credenciais administrativas e de migrations não são passadas ao
container da aplicação. No host, `DATABASE_URL` configura o runtime e
`MIGRATION_DATABASE_URL` configura exclusivamente o comando de migrations.
Os SQLs de provisionamento e grants usam um advisory lock por banco para
serializar reaplicações concorrentes.

| Objeto | Runtime |
| --- | --- |
| Banco | CONNECT |
| Schema | USAGE |
| wallets, wager_transactions, inbox, outbox | SELECT, INSERT, UPDATE |
| wallet_ledger | SELECT, INSERT |
| schema_migrations | SELECT |
| Funções dos três triggers financeiros | EXECUTE |

Revogar permissões de `PUBLIC` no banco, schema, tabelas e funções. Revogar do
runtime explicitamente SET e ALTER SYSTEM sobre `session_replication_role`, além de
limpar configurações prévias do login de runtime. Sem TEMPORARY ou CREATE, o
runtime também não pode criar objetos temporários para sombrear as tabelas
consultadas pelos triggers. As funções continuam com SECURITY INVOKER.

Novos objetos não recebem privilégios de runtime por padrão. Depois de cada
migration, o serviço `db-grants` reaplica a lista explícita de acessos. Uma nova
tabela de negócio exige atualizar essa lista. Permissões globais de criação de
papéis e senhas ficam no provisionamento, fora das migrations do schema.

## Atualização e limites

`db-roles` funciona em banco vazio e em volumes existentes: transfere somente o
schema e os seis objetos tabulares e três funções conhecidos para o migrator.
Não executa REASSIGN OWNED sobre todos os objetos do administrador, não transfere
a propriedade do banco e não remove dados. Aplicações antigas devem ser paradas
durante a troca de propriedade e credenciais. O procedimento está no [guia de operação](../guias/operacao.md#permissões-e-atualização-de-volumes-existentes).

O banco deve ser dedicado ao serviço, pois o provisionamento revoga privilégios
de PUBLIC. Papéis com memberships inesperadas ou um runtime que já seja dono do
banco são recusados e exigem revisão. Privilégios perigosos sobre
`session_replication_role` herdados de PUBLIC também provocam erro; o provisionamento
não modifica essa ACL global do cluster para outros usuários. Em produção, o administrador da plataforma
deve executar o provisionamento equivalente com segredos próprios e auditar
privilégios adicionais do ambiente. O DBA e o proprietário das tabelas mantêm
poder de DDL; essa credencial não pertence ao processo financeiro.

Privilégios DML não substituem a autorização por provedor na aplicação nem as
invariantes dos triggers. Uma credencial de runtime comprometida ainda permite
DML nos objetos listados; esta decisão limita DDL e remoção das proteções do banco.

## Verificação

`TestDatabaseRoleSeparation` executa os mesmos SQLs de provisionamento e grants
usados pelo Compose, em bancos e papéis isolados. Verifica banco novo e migração
de ownership de banco existente com preservação do saldo inicial. Conecta com
logins reais e senhas distintas; o migrator aplica migrations sem ser superuser.

O runtime abre carteira, processa aposta e reversão, reconhece replay com inbox,
retoma referência pendente, reivindica e confirma outbox e reconcilia o ledger.
Tentativas de DDL, alteração de funções e triggers, criação de papéis, SET ROLE,
session_replication_role, alteração do ledger e histórico de migrations retornam
SQLSTATE 42501. Objetos futuros também recusam acesso. Uma alteração inconsistente
do saldo continua sendo bloqueada pelo trigger financeiro no commit (23514).
